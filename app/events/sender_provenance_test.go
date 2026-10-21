package events

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage/engine"
)

type senderProvenanceFixture struct {
	path    string
	db      *engine.SQL
	journal *moderation.Journal
	adapter *recovery
	deletes map[int]int
	bans    int
	cards   []string
}

func newSenderProvenanceFixture(t *testing.T) *senderProvenanceFixture {
	t.Helper()
	f := &senderProvenanceFixture{path: filepath.Join(t.TempDir(), "provenance.db"), deletes: map[int]int{}}
	f.adapter = &recovery{
		gate: func(a moderation.Action) bool {
			if a.Kind == "notice" || a.Kind == "card" {
				return a.ChatID == 456
			}
			return a.ChatID == 123 && a.IdentityID == 501
		},
		superName: func(string, int64) bool { return false },
		api: recoveryAPIFunc(func(_ context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
			switch q := c.(type) {
			case tbapi.GetChatMemberConfig:
				require.EqualValues(t, 123, q.ChatID)
				require.EqualValues(t, 501, q.UserID)
				return apiOK(tbapi.ChatMember{User: &tbapi.User{ID: 501}, Status: "member"}), nil
			case tbapi.BanChatMemberConfig:
				require.EqualValues(t, 123, q.ChatID)
				require.EqualValues(t, 501, q.UserID)
				f.bans++
				return apiOK(true), nil
			case tbapi.DeleteMessageConfig:
				require.EqualValues(t, 123, q.ChatID)
				require.Contains(t, []int{701, 702, 703}, q.MessageID)
				f.deletes[q.MessageID]++
				return apiOK(true), nil
			case tbapi.MessageConfig:
				require.EqualValues(t, 456, q.ChatID)
				f.cards = append(f.cards, q.Text)
				return apiOK(tbapi.Message{MessageID: 600}), nil
			case tbapi.EditMessageTextConfig:
				require.EqualValues(t, 456, q.ChatID)
				f.cards = append(f.cards, q.Text)
				return apiOK(tbapi.Message{MessageID: 600}), nil
			default:
				t.Fatalf("unexpected Telegram request %T", c)
				return nil, nil
			}
		}),
	}
	f.reopen(t)
	t.Cleanup(func() { require.NoError(t, f.db.Close()) })
	return f
}

func (f *senderProvenanceFixture) reopen(t *testing.T) {
	t.Helper()
	if f.db != nil {
		require.NoError(t, f.db.Close())
	}
	var err error
	f.db, err = engine.NewSqlite(f.path, "fixture")
	require.NoError(t, err)
	f.journal, err = moderation.NewJournal(context.Background(), f.db, moderation.DefaultLimits())
	require.NoError(t, err)
	f.adapter.journal = f.journal
}

func senderProvenancePlan(now time.Time, origin string) moderation.Plan {
	p := moderation.Plan{Key: "source:123:701:user:501", ChatID: 123, MessageID: 701, IdentityID: 501, IdentityKind: "user", Destination: 456,
		Origin: origin, Card: true, SourceText: "original source text"}
	for _, mid := range []int{701, 702, 703} {
		kind := "cleanup_delete"
		if mid == 701 {
			kind = "original_delete"
		}
		p.Actions = append(p.Actions, moderation.Action{Kind: kind, ChatID: 123, MessageID: mid, IdentityID: 501, IdentityKind: "user", MessageDate: now.Unix()})
	}
	if origin == "report:sender:-777" {
		p.RequesterID = -777
		p.DeliveryChat = 123
		p.DeliveryMessage = 900
		for i := range p.Actions {
			p.Actions[i].State = moderation.Manual
			p.Actions[i].Code = "sender_command_pending_confirmation"
		}
	} else {
		p.Actions = append(p.Actions, moderation.Action{Kind: "ban", ChatID: 123, IdentityID: 501, IdentityKind: "user"})
	}
	return p
}

func (f *senderProvenanceFixture) accept(t *testing.T, p moderation.Plan, now time.Time) {
	t.Helper()
	_, _, _, err := f.journal.Accept(context.Background(), p, now)
	require.NoError(t, err)
}

func (f *senderProvenanceFixture) view(t *testing.T) moderation.Card {
	t.Helper()
	c, err := f.journal.CardView(context.Background(), "source:123:701:user:501")
	require.NoError(t, err)
	return c
}

func (f *senderProvenanceFixture) queue(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.GetContext(context.Background(), &n, `SELECT count(*) FROM moderation_actions WHERE kind NOT IN ('notice','card') AND state IN ('pending','running')`))
	return n
}

func (f *senderProvenanceFixture) drain(t *testing.T, now time.Time) {
	t.Helper()
	worker := moderation.Worker{Journal: f.journal, Executor: f.adapter, Now: func() time.Time { return now }}
	for range 2 {
		drained := false
		for range 50 {
			worked, err := worker.Step(context.Background())
			require.NoError(t, err)
			if !worked {
				drained = true
				break
			}
		}
		require.True(t, drained, "worker must drain the due queue")
		now = now.Add(time.Minute)
	}

}

func (f *senderProvenanceFixture) assertConfirmedOnce(t *testing.T) {
	t.Helper()
	require.Zero(t, f.queue(t))
	require.Equal(t, 1, f.bans)
	require.Equal(t, map[int]int{701: 1, 702: 1, 703: 1}, f.deletes)
	for _, op := range currentCardActions(f.view(t)) {
		require.Equal(t, moderation.Confirmed, op.State)
	}
	card := compactCardText(f.view(t), "fixture")
	require.Contains(t, card, "Сообщение удалено")
	require.Contains(t, card, "Дополнительно удалено: 2")
	require.NotEmpty(t, f.cards)
	require.Contains(t, f.cards[len(f.cards)-1], "Сообщение удалено")
}

func TestSenderRequestIndependentPlanWorker(t *testing.T) {
	for _, origin := range []string{"detector", "forward", "forward:456:1001"} {
		for _, decision := range []string{"pending", "reject", "expired"} {
			t.Run(origin+"/"+decision, func(t *testing.T) {
				f := newSenderProvenanceFixture(t)
				now := time.Now()
				accepted := now.Add(-time.Second)
				if decision == "expired" {
					accepted = now.Add(-16 * time.Minute)
				}
				request := senderProvenancePlan(accepted, "report:sender:-777")
				independent := senderProvenancePlan(accepted, origin)
				f.accept(t, request, accepted)
				require.Zero(t, f.queue(t))
				request.DeliveryMessage++
				f.accept(t, request, accepted.Add(time.Millisecond))
				require.Zero(t, f.queue(t))
				f.reopen(t)
				f.drain(t, now)
				require.Empty(t, f.deletes)
				require.Zero(t, f.bans)
				f.accept(t, independent, accepted.Add(time.Second))
				require.Equal(t, 4, f.queue(t), "independent authority must queue the original, both cleanup IDs and ban")
				before := compactCardText(f.view(t), "fixture")
				require.Contains(t, before, "Удаление ожидает выполнения")
				if decision == "reject" {
					fresh, err := f.journal.Decide(context.Background(), "reject sender request", f.view(t), "reject_report", 11, now)
					require.NoError(t, err)
					require.True(t, fresh)
					require.NoError(t, f.journal.EndDecision(context.Background(), "reject sender request"))
				}
				if decision == "expired" {
					fresh, err := f.journal.Decide(context.Background(), "late approval", f.view(t), "approve_report", 11, now)
					require.ErrorContains(t, err, "expired")
					require.False(t, fresh)
					require.NotContains(t, compactCardText(f.view(t), "fixture"), "действия не разрешены")
				}
				require.Equal(t, 4, f.queue(t))
				request.DeliveryMessage++
				f.accept(t, request, now)
				f.accept(t, independent, now)
				require.Equal(t, 4, f.queue(t))
				f.reopen(t)
				f.drain(t, now.Add(2*time.Minute))
				f.assertConfirmedOnce(t)
				state := f.view(t).RequestState
				switch decision {
				case "pending":
					require.Equal(t, "pending", state)
				case "reject":
					require.Equal(t, "reject_report", state)
				case "expired":
					require.Equal(t, "expired", state)
				}
				var count int
				require.NoError(t, f.db.GetContext(context.Background(), &count, `SELECT count(*) FROM moderation_actions WHERE kind IN ('original_delete','cleanup_delete')`))
				require.Equal(t, 6, count, "three request placeholders and three independent deletions")
				f.reopen(t)
				f.accept(t, independent, now.Add(3*time.Minute))
				request.DeliveryMessage++
				f.accept(t, request, now.Add(3*time.Minute))
				f.drain(t, now.Add(4*time.Minute))
				f.assertConfirmedOnce(t)
				require.NoError(t, f.db.GetContext(context.Background(), &count, `SELECT count(*) FROM moderation_actions WHERE kind IN ('original_delete','cleanup_delete')`))
				require.Equal(t, 6, count)
			})
		}
	}
}

func TestSenderRequestAfterIndependentPlanWorker(t *testing.T) {
	for _, origin := range []string{"detector", "forward"} {
		for _, completed := range []bool{false, true} {
			t.Run(origin+"/"+map[bool]string{false: "queued", true: "confirmed"}[completed], func(t *testing.T) {
				f := newSenderProvenanceFixture(t)
				now := time.Now()
				independent := senderProvenancePlan(now, origin)
				f.accept(t, independent, now)
				require.Equal(t, 4, f.queue(t))
				if completed {
					f.drain(t, now.Add(time.Minute))
					f.assertConfirmedOnce(t)
				}
				request := senderProvenancePlan(now, "report:sender:-777")
				f.accept(t, request, now.Add(time.Second))
				f.accept(t, request, now.Add(2*time.Second))
				f.reopen(t)
				require.Empty(t, f.view(t).RequestState, "a request cannot replace existing independent authority")
				f.drain(t, now.Add(2*time.Minute))
				f.assertConfirmedOnce(t)
				var count int
				require.NoError(t, f.db.GetContext(context.Background(), &count, `SELECT count(*) FROM moderation_actions WHERE kind IN ('original_delete','cleanup_delete')`))
				require.Equal(t, 3, count)
			})
		}
	}
}
