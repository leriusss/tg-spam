package events

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/go-pkgz/testutils/containers"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage/engine"
)

func instructionFleet(t *testing.T, counts []int, open func(string) *engine.SQL) *sharedFleet {
	t.Helper()
	if open == nil && os.Getenv("FORWARD_INSTRUCTION_DB_COPIES") != "" {
		base := os.Getenv("FORWARD_INSTRUCTION_DB_COPIES")
		copies := t.TempDir()
		open = func(gid string) *engine.SQL {
			path := filepath.Join(copies, gid+".db")
			if _, err := os.Stat(path); os.IsNotExist(err) {
				raw, readErr := os.ReadFile(filepath.Join(base, gid+".db"))
				require.NoError(t, readErr)
				require.NoError(t, os.WriteFile(path, raw, 0o600))
			}
			db, err := engine.NewSqlite(path, gid)
			require.NoError(t, err)
			return db
		}
	}
	f := newSharedFleet(t, counts, open)
	for i, fixture := range f.fixtures {
		fixture.listener.SuperUsers = nil // a current chat admin needs no SUPER_USER entry
		config := f.bridges[i].config
		config.Policy = moderation.ForwardAllPolicy
		config.Open = func(context.Context) (*moderation.SharedStore, func(), error) {
			return f.store, func() {}, nil
		}
		fixture.listener.SharedForward = &config
		name := []string{"Балтия", "Европа", "Неопорт", "ЛЕС", "Multiverse"}[i]
		fixture.api.GetChatFunc = func(c tbapi.ChatInfoConfig) (tbapi.ChatFullInfo, error) {
			return tbapi.ChatFullInfo{Chat: tbapi.Chat{ID: c.ChatID, Title: name}}, nil
		}
	}
	return f
}

// Exercise actual listener routing, role lookup and intake, not a direct journal
// enqueue or callback. Do waits for the coordination goroutine on channel close.
func instructionForward(t *testing.T, f *sharedFleet, delivery int, indices ...int) {
	t.Helper()
	if len(indices) == 0 {
		for i := range f.fixtures {
			indices = append(indices, i)
		}
	}
	for _, i := range indices {
		fixture := f.fixtures[i]
		u := manualForwardUpdate()
		u.Message.MessageID = delivery
		u.UpdateID = 300000 + i*1000 + delivery
		fixture.listen(t, u)
		b := fixture.listener.adminHandler.shared
		require.NotNil(t, b)
		b.store = f.store
		b.now = func() time.Time { return f.now }
		f.bridges[i] = b
	}
}

func settleInstructions(t *testing.T, f *sharedFleet) {
	t.Helper()
	for range 3 {
		f.tick(t)
		for _, fixture := range f.fixtures {
			fixture.settle(t)
		}
	}
}

func instructionText(t *testing.T, f *sharedFleet, example string) string {
	t.Helper()
	c := f.one(t)
	text, buttons := sharedViewCard(moderation.SharedView{Case: c}, f.bridges[0].config.Peers, f.now)
	for _, row := range buttons.InlineKeyboard {
		for _, button := range row {
			require.True(t, strings.HasPrefix(*button.CallbackData, "S:v"), "only presentation callbacks")
		}
	}
	require.NotContains(t, text, "receipt")
	require.NotContains(t, text, "lease")
	if dir := os.Getenv("SHARED_INSTRUCTION_EXAMPLES"); dir != "" {
		require.NoError(t, os.MkdirAll(dir, 0o700))
		raw, err := json.MarshalIndent(buttons, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, example+".txt"), []byte(text), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, example+".json"), raw, 0o600))
	}
	return text
}

func runInstructionPipeline(t *testing.T, open func(string) *engine.SQL) {
	t.Helper()
	f := instructionFleet(t, []int{2, 3, 2, 0, 2}, open)
	instructionForward(t, f, 1349)
	f.tick(t)
	c := f.one(t)
	require.Equal(t, moderation.ForwardAllPolicy, c.Policy)
	require.True(t, c.Decided)
	require.True(t, c.Automatic)
	require.Len(t, c.Choices, 4)
	require.Contains(t, instructionText(t, f, "processing"), "Принято. Обрабатываю найденный спам")
	settleInstructions(t, f)
	for _, report := range f.one(t).Reports {
		for _, action := range report.Actions {
			require.Equal(t, report.ChatID, action.ChatID)
			if action.Kind == "ban" {
				require.Equal(t, report.Targets[0].BanDeadline, action.Deadline)
				continue
			}
			for _, target := range report.Targets {
				if action.MessageID == target.MessageID {
					require.Positive(t, target.MessageDate)
					require.Equal(t, target.MessageDate, action.MessageDate)
					require.Equal(t, target.Deadline, action.Deadline)
				}
			}
		}
	}
	for i, fixture := range f.fixtures {
		expected := []int{2, 3, 2, 0, 2}[i]
		require.Len(t, fixture.deleteCalls, expected)
		if expected > 0 {
			require.Equal(t, map[int64]int{501 + int64(i): 1}, fixture.banCalls)
		}
		for _, n := range fixture.deleteCalls {
			require.Equal(t, 1, n)
		}
		if i > 0 {
			require.Empty(t, fixture.cards)
		}
	}
	require.Len(t, f.fixtures[0].cards, 1)
	text := instructionText(t, f, "success")
	require.Contains(t, text, "✅ Спам обработан")
	require.Contains(t, text, "удалено ботом: 3; авторов заблокировано: 1")
	require.Contains(t, text, "ЛЕС: совпадений не найдено")
	checks := f.fixtures[0].operatorChecks
	f.tick(t)
	require.Equal(t, checks, f.fixtures[0].operatorChecks, "completed history does not poll operator roles")
	f.store = f.reopenStore()
	for i, fixture := range f.fixtures {
		db := f.openLocal(i)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		journal, err := moderation.NewJournal(context.Background(), db, moderation.DefaultLimits())
		require.NoError(t, err)
		require.NoError(t, journal.EnableSharedOutbox(context.Background()))
		fixture.db, fixture.j, fixture.listener.RecoveryJournal = db, journal, journal
	}
	for _, b := range f.bridges {
		b.store, b.cache = f.store, map[string]string{}
	}
	instructionForward(t, f, 1349) // redelivery after restart cannot upgrade/reenqueue
	settleInstructions(t, f)
	for _, fixture := range f.fixtures {
		for _, n := range fixture.banCalls {
			require.Equal(t, 1, n)
		}
		for _, n := range fixture.deleteCalls {
			require.Equal(t, 1, n)
		}
	}
	require.Len(t, f.fixtures[0].cards, 1)
	old := f.one(t)
	f.choose(t, old, "all", nil)
	require.Equal(t, old.Choices, f.one(t).Choices)
}

func TestInstructionListenerPipelineSQLite(t *testing.T) {
	t.Parallel()
	runInstructionPipeline(t, nil)
}

func TestInstructionSingleAndUnknownAuthor(t *testing.T) {
	t.Parallel()
	for _, known := range []bool{true, false} {
		t.Run(fmt.Sprint(known), func(t *testing.T) {
			f := instructionFleet(t, []int{1}, nil)
			if !known {
				_, err := f.fixtures[0].db.Exec(`DELETE FROM message_authors`)
				require.NoError(t, err)
			}
			instructionForward(t, f, 1349)
			settleInstructions(t, f)
			require.Equal(t, map[int]int{800: 1}, f.fixtures[0].deleteCalls)
			if known {
				require.Equal(t, map[int64]int{501: 1}, f.fixtures[0].banCalls)
			} else {
				require.Empty(t, f.fixtures[0].banCalls)
			}
		})
	}
}

func TestInstructionOpenOriginDoesNotOverrideOtherOriginalAuthors(t *testing.T) {
	t.Parallel()
	f := instructionFleet(t, []int{1, 1, 1, 0, 0}, nil)
	_, err := f.fixtures[2].db.Exec(`DELETE FROM message_authors`)
	require.NoError(t, err)
	for i, fixture := range f.fixtures {
		u := manualForwardUpdate()
		u.Message.MessageID, u.UpdateID = 1349, 600000+i
		u.Message.ForwardOrigin = &tbapi.MessageOrigin{Type: "user", SenderUser: &tbapi.User{ID: 501}}
		fixture.listen(t, u)
		f.bridges[i] = fixture.listener.adminHandler.shared
		f.bridges[i].store = f.store
		f.bridges[i].now = func() time.Time { return f.now }
	}
	settleInstructions(t, f)
	require.Equal(t, map[int64]int{501: 1}, f.fixtures[0].banCalls)
	require.Equal(t, map[int64]int{502: 1}, f.fixtures[1].banCalls)
	require.Equal(t, map[int]int{900: 1}, f.fixtures[1].deleteCalls)
	require.Empty(t, f.fixtures[2].banCalls, "the forward's author cannot supply a different unknown author's ID")
	require.Equal(t, map[int]int{1000: 1}, f.fixtures[2].deleteCalls)
	for _, fixture := range f.fixtures[3:] {
		require.Empty(t, fixture.banCalls)
		require.Empty(t, fixture.deleteCalls)
	}
}

func TestInstructionLateResultsAndFrozenChoices(t *testing.T) {
	t.Parallel()
	runInstructionLateResults(t, nil)
}

func runInstructionLateResults(t *testing.T, open func(string) *engine.SQL) {
	t.Helper()
	f := instructionFleet(t, []int{2, 2}, open)
	instructionForward(t, f, 1349, 0)
	settleInstructions(t, f)
	first := f.one(t)
	require.Len(t, first.Choices, 1)
	require.Contains(t, instructionText(t, f, "late-waiting"), "ещё не проверен")
	require.Len(t, f.fixtures[0].deleteCalls, 2)
	require.Empty(t, f.fixtures[1].deleteCalls)
	instructionForward(t, f, 1349, 1)
	settleInstructions(t, f)
	late := f.one(t)
	require.Len(t, late.Choices, 2)
	require.Equal(t, first.Choices[0], late.Choices[0])
	require.Equal(t, first.Expires, late.Expires)
	require.Equal(t, first.Operator, late.DecisionOperator)
	require.Equal(t, first.Expires, late.Choices[1].Report.Expires)
	require.Len(t, f.fixtures[1].deleteCalls, 2)
	// New search results cannot alter an already accepted delivery's target set.
	report := late.Reports[1]
	report.Targets = append(report.Targets, moderation.ForwardTarget{MessageID: 9999})
	require.Error(t, f.store.Submit(context.Background(), report, f.now))
	require.Equal(t, late.Choices, f.one(t).Choices)
}

func TestInstructionExpiryAndRevokedRole(t *testing.T) {
	t.Parallel()
	for _, expired := range []bool{true, false} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			f := instructionFleet(t, []int{2, 2}, nil)
			instructionForward(t, f, 1349, 0)
			settleInstructions(t, f)
			first := f.one(t)
			if expired {
				f.now = f.now.Add(16 * time.Minute)
			} else {
				f.fixtures[0].operatorRole = "member"
				f.fixtures[1].operatorRole = "member"
			}
			instructionForward(t, f, 1349, 1)
			settleInstructions(t, f)
			require.Empty(t, f.fixtures[1].deleteCalls)
			require.Empty(t, f.fixtures[1].banCalls)
			require.Equal(t, first.Choices, f.one(t).Choices)
		})
	}
	f := instructionFleet(t, []int{2}, nil)
	instructionForward(t, f, 1349)
	f.tick(t) // decision exists; revoke before the actual worker
	f.fixtures[0].operatorRole = "member"
	settleInstructions(t, f)
	require.Empty(t, f.fixtures[0].deleteCalls)
	require.Empty(t, f.fixtures[0].banCalls)
}

func TestInstructionBackloggedForwardKeepsTelegramExpiry(t *testing.T) {
	t.Parallel()
	f := instructionFleet(t, []int{2}, nil)
	u := manualForwardUpdate()
	u.Message.Date -= 16 * 60
	f.fixtures[0].listen(t, u)
	f.bridges[0] = f.fixtures[0].listener.adminHandler.shared
	f.bridges[0].store = f.store
	settleInstructions(t, f)
	c := f.one(t)
	require.Equal(t, time.Unix(int64(u.Message.Date), 0).Add(15*time.Minute).UnixMilli(), c.Expires)
	require.False(t, c.Decided)
	require.Empty(t, f.fixtures[0].deleteCalls)
	require.Empty(t, f.fixtures[0].banCalls)
}

func TestInstructionExpiryDuringRoleLookupAndDelayedImport(t *testing.T) {
	t.Parallel()
	for _, afterRole := range []bool{true, false} {
		t.Run(fmt.Sprint(afterRole), func(t *testing.T) {
			f := instructionFleet(t, []int{2}, nil)
			instructionForward(t, f, 1349)
			if afterRole {
				base := f.fixtures[0].api.call
				f.fixtures[0].api.call = func(ctx context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
					response, err := base(ctx, req)
					if member, ok := req.(tbapi.GetChatMemberConfig); ok && member.ChatID == 456 {
						f.now = f.now.Add(16 * time.Minute)
					}
					return response, err
				}
				f.tick(t)
				require.False(t, f.one(t).Decided)
			} else {
				f.tick(t)
				require.True(t, f.one(t).Decided)
				started := f.now
				f.now = f.now.Add(16 * time.Minute)
				require.NoError(t, f.bridges[0].step(context.Background(), started))
			}
			settleInstructions(t, f)
			require.Empty(t, f.fixtures[0].deleteCalls)
			require.Empty(t, f.fixtures[0].banCalls)
		})
	}
}

func TestInstructionPermanentRefusalNeedsNewForward(t *testing.T) {
	t.Parallel()
	f := instructionFleet(t, []int{2}, nil)
	f.fixtures[0].failDelete = true
	instructionForward(t, f, 1349)
	settleInstructions(t, f)
	old := f.one(t)
	deadline := old.Reports[0].Targets[0].Deadline
	require.Contains(t, instructionText(t, f, "partial"), "Обработано частично")
	require.Contains(t, instructionText(t, f, "partial"), "не удалось: 2")
	for range 3 {
		settleInstructions(t, f)
	}
	for _, calls := range f.fixtures[0].deleteCalls {
		require.Equal(t, 1, calls)
	}
	f.fixtures[0].failDelete = false
	instructionForward(t, f, 1350)
	settleInstructions(t, f)
	cases, err := f.store.Cases(context.Background())
	require.NoError(t, err)
	require.Len(t, cases, 2)
	for _, c := range cases {
		require.Equal(t, deadline, c.Reports[0].Targets[0].Deadline)
	}
	for _, calls := range f.fixtures[0].deleteCalls {
		require.Equal(t, 2, calls)
	}
	require.Equal(t, 1, f.fixtures[0].banCalls[501])
}

func TestInstructionGatesAndOrdinaryAdminText(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"ordinary", "bot", "non-admin", "dry", "training", "protected"} {
		t.Run(mode, func(t *testing.T) {
			f := instructionFleet(t, []int{2}, nil)
			fixture := f.fixtures[0]
			u := manualForwardUpdate()
			switch mode {
			case "ordinary":
				u.Message.ForwardOrigin = nil
			case "bot":
				u.Message.From.IsBot = true
			case "non-admin":
				fixture.operatorRole = "member"
			case "dry":
				fixture.listener.Dry = true
			case "training":
				fixture.listener.TrainingMode = true
			case "protected":
				fixture.listener.SuperUsers = SuperUsers{"501"}
			}
			fixture.listen(t, u)
			f.bridges[0] = fixture.listener.adminHandler.shared
			f.bridges[0].store = f.store
			settleInstructions(t, f)
			require.Empty(t, fixture.deleteCalls)
			require.Empty(t, fixture.banCalls)
			if mode == "protected" {
				require.Contains(t, instructionText(t, f, "protected"), "отказ")
			}
		})
	}
}

func TestInstructionHistoricalIntakeCannotBeUpgraded(t *testing.T) {
	t.Parallel()
	f := newSharedFleet(t, []int{2}, nil) // explicit legacy policy fixture
	f.forward(t, 1349)
	f.tick(t)
	old := f.one(t)
	require.False(t, old.Decided)
	f.fixtures[0].listener.SharedForward = &SharedForwardConfig{Policy: moderation.ForwardAllPolicy,
		Peer: 100, Owner: 100, Peers: []int64{100}, Open: func(context.Context) (*moderation.SharedStore, func(), error) {
			return f.store, func() {}, nil
		}}
	// Replay the exact saved update through the new listener.
	u := manualForwardUpdate()
	u.Message.MessageID, u.UpdateID = 1349, 200000+1349
	f.fixtures[0].listen(t, u)
	f.bridges[0] = f.fixtures[0].listener.adminHandler.shared
	f.bridges[0].store = f.store
	settleInstructions(t, f)
	c := f.one(t)
	require.Empty(t, c.Policy)
	require.Equal(t, old.Choices, c.Choices)
	require.False(t, c.Decided)
	require.Empty(t, f.fixtures[0].deleteCalls)
}

func TestInstructionUnknownCardDoesNotBlockIndependent(t *testing.T) {
	t.Parallel()
	for _, abandoned := range []bool{false, true} {
		t.Run(fmt.Sprint(abandoned), func(t *testing.T) { runInstructionUnknownCard(t, abandoned) })
	}
}

func runInstructionUnknownCard(t *testing.T, abandoned bool) {
	t.Helper()
	f := instructionFleet(t, []int{2}, nil)
	base := f.fixtures[0].api.call
	f.fixtures[0].api.call = func(ctx context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
		if _, ok := req.(tbapi.MessageConfig); ok && !abandoned {
			return nil, io.EOF
		}
		return base(ctx, req)
	}
	instructionForward(t, f, 1349)
	if abandoned {
		reports, err := f.fixtures[0].j.SharedOutbox(context.Background())
		require.NoError(t, err)
		require.NoError(t, f.store.Submit(context.Background(), reports[0], f.now))
		initial := f.one(t)
		_, claimed, err := f.store.ClaimCard(context.Background(), 100, initial.Ref, initial.Revision,
			"abandoned send", f.now.Add(3*time.Second))
		require.NoError(t, err)
		require.True(t, claimed)
		f.now = f.now.Add(35 * time.Second)
	}
	settleInstructions(t, f)
	a := f.one(t)
	require.Equal(t, "unknown", a.SendState)
	f.fixtures[0].api.call = base
	u := originalForwardUpdate(999, 777)
	u.Message.Chat.ID, u.Message.Text = -1000, "independent spam"
	require.NoError(t, f.fixtures[0].listener.procEvents(u))
	forward := manualForwardUpdate()
	forward.Message.MessageID, forward.Message.Text, forward.UpdateID = 1350, "independent spam", 400000
	f.fixtures[0].listen(t, forward)
	f.bridges[0] = f.fixtures[0].listener.adminHandler.shared
	f.bridges[0].store = f.store
	settleInstructions(t, f)
	cases, err := f.store.Cases(context.Background())
	require.NoError(t, err)
	require.Len(t, cases, 2)
	require.Equal(t, 1, f.fixtures[0].deleteCalls[999])
	require.Equal(t, 1, f.fixtures[0].banCalls[777])
	for _, c := range cases {
		if c.Ref == a.Ref {
			require.Equal(t, "unknown", c.SendState)
		} else {
			require.Positive(t, c.NotificationID)
		}
	}
}

func TestInstructionConcurrentCoordinatorDecisions(t *testing.T) {
	t.Parallel()
	f := instructionFleet(t, []int{2}, nil)
	instructionForward(t, f, 1349)
	require.NoError(t, f.bridges[0].step(context.Background(), f.now))
	c := f.one(t)
	require.False(t, c.Decided)
	f.now = f.now.Add(3 * time.Second)
	stores := []*moderation.SharedStore{f.store, f.reopenStore(), f.reopenStore()}
	var wg sync.WaitGroup
	errors := make(chan error, len(stores))
	for _, store := range stores {
		wg.Go(func() { errors <- store.AutoChoose(context.Background(), 100, c.Ref, f.now) })
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.Len(t, f.one(t).Choices, 1)
	settleInstructions(t, f)
	require.Equal(t, 1, f.fixtures[0].banCalls[501])
}

func TestInstructionListenerPipelinePostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration runs in full CI")
	}
	pg := containers.NewPostgresTestContainerWithDB(context.Background(), t, "instruction_forward")
	t.Cleanup(func() { pg.Close(context.Background()) })
	for _, scenario := range []struct {
		name string
		run  func(*testing.T, func(string) *engine.SQL)
	}{{"pipeline", runInstructionPipeline}, {"late-results", runInstructionLateResults}} {
		t.Run(scenario.name, func(t *testing.T) {
			scenario.run(t, func(gid string) *engine.SQL {
				db, err := engine.NewPostgres(context.Background(), pg.ConnectionString(), scenario.name+"-"+gid)
				require.NoError(t, err)
				return db
			})
		})
	}
}

func TestInstructionCompatiblePolicyRollbackPreservesPending(t *testing.T) {
	t.Parallel()
	f := instructionFleet(t, []int{2}, nil)
	instructionForward(t, f, 1349)
	f.tick(t)
	before := f.one(t)
	require.True(t, before.Decided)
	// Keep the compatible executable and coordinator; change only new admission.
	f.fixtures[0].listener.SharedForward.Policy = ""
	u := manualForwardUpdate()
	u.UpdateID, u.Message.MessageID = 500000, 1350
	f.fixtures[0].listen(t, u)
	f.bridges[0] = f.fixtures[0].listener.adminHandler.shared
	f.bridges[0].store = f.store
	settleInstructions(t, f)
	cases, err := f.store.Cases(context.Background())
	require.NoError(t, err)
	require.Len(t, cases, 2)
	for _, c := range cases {
		if c.Ref == before.Ref {
			require.Equal(t, before.Choices, c.Choices)
			require.Equal(t, before.Expires, c.Expires)
		} else {
			require.Empty(t, c.Policy)
			require.False(t, c.Decided)
		}
	}
	require.Equal(t, map[int]int{800: 1, 801: 1}, f.fixtures[0].deleteCalls)
	require.Equal(t, map[int64]int{501: 1}, f.fixtures[0].banCalls)
}

func TestInstructionConfirmedAbsentAndUnknownReports(t *testing.T) {
	t.Parallel()
	f := instructionFleet(t, []int{2, 2, 2}, nil)
	prior := moderation.Plan{Key: "source:-1000:800:user:501", ChatID: -1000, IdentityID: 501, IdentityKind: "user",
		MessageID: 800, Destination: 456, Origin: "detector", Actions: []moderation.Action{
			{Kind: "ban", ChatID: -1000, IdentityID: 501, IdentityKind: "user"},
			{Kind: "original_delete", ChatID: -1000, IdentityID: 501, IdentityKind: "user", MessageID: 800,
				MessageDate: time.Now().Unix()},
			{Kind: "original_delete", ChatID: -1000, IdentityID: 501, IdentityKind: "user", MessageID: 801,
				MessageDate: time.Now().Unix()},
		}}
	_, _, _, err := f.fixtures[0].j.Accept(context.Background(), prior, time.Now())
	require.NoError(t, err)
	f.fixtures[0].settle(t)
	base := f.fixtures[1].api.call
	f.fixtures[1].api.call = func(ctx context.Context, request tbapi.Chattable) (*tbapi.APIResponse, error) {
		if _, ok := request.(tbapi.DeleteMessageConfig); ok {
			return nil, &tbapi.Error{Code: 400, Message: "Bad Request: message to delete not found"}
		}
		return base(ctx, request)
	}
	f.fixtures[2].unknownDelete = true
	instructionForward(t, f, 1349)
	settleInstructions(t, f)
	text := instructionText(t, f, "mixed-results")
	require.Contains(t, text, "Обработано частично")
	require.Contains(t, text, "ранее обработано: 2")
	require.Contains(t, text, "уже отсутствовало или недоступно: 2")
	require.Contains(t, text, "результат неизвестен: 2")
	require.Equal(t, 1, f.fixtures[0].banCalls[501])
	for _, n := range f.fixtures[0].deleteCalls {
		require.Equal(t, 1, n)
	}
}

func TestInstructionRefusalAfterIntakeCannotAcquireNewAttempt(t *testing.T) {
	t.Parallel()
	f := instructionFleet(t, []int{2, 2}, nil)
	fixture := f.fixtures[0]
	prior := moderation.Plan{Key: "source:-1000:800:user:501", ChatID: -1000, IdentityID: 501, IdentityKind: "user",
		MessageID: 800, Destination: 456, Origin: "detector", Actions: []moderation.Action{
			{Kind: "original_delete", ChatID: -1000, IdentityID: 501, IdentityKind: "user", MessageID: 800,
				MessageDate: time.Now().Unix()},
		}}
	_, _, _, err := fixture.j.Accept(context.Background(), prior, time.Now())
	require.NoError(t, err)
	fixture.failDelete = true
	instructionForward(t, f, 1349)
	fixture.settle(t) // permanent refusal appears AFTER the explicit intake
	settleInstructions(t, f)
	c := f.one(t)
	require.True(t, c.NeedsReview)
	require.NotEmpty(t, c.Choices[0].Refusal)
	require.Len(t, c.Choices, 2)
	require.Equal(t, 1, fixture.deleteCalls[800])
	require.Empty(t, fixture.banCalls)
	require.Len(t, f.fixtures[1].deleteCalls, 2, "another participant remains independent")
	for range 3 {
		settleInstructions(t, f)
	}
	require.Equal(t, 1, fixture.deleteCalls[800])
	require.Equal(t, c.Choices, f.one(t).Choices)
}

func TestInstructionNumericNamespacesAndNoCleanup(t *testing.T) {
	t.Parallel()
	f := instructionFleet(t, []int{0}, nil)
	for i, identity := range []int64{777, -777} {
		u := originalForwardUpdate(800+i, identity)
		u.Message.Chat.ID = -1000
		require.NoError(t, f.fixtures[0].listener.procEvents(u))
	}
	instructionForward(t, f, 1349)
	settleInstructions(t, f)
	require.Equal(t, map[int64]int{777: 1, -777: 1}, f.fixtures[0].banCalls)
	require.Equal(t, map[int]int{800: 1, 801: 1}, f.fixtures[0].deleteCalls)
	var extra int
	require.NoError(t, f.fixtures[0].db.Get(&extra,
		f.fixtures[0].db.Adopt(`SELECT count(*) FROM moderation_actions WHERE gid=? AND kind NOT IN ('original_delete','ban','restrict')`),
		f.fixtures[0].db.GID()))
	require.Zero(t, extra, "no training, cleanup or separate local cards")
}

func TestInstructionSoftBanReportsActualRestriction(t *testing.T) {
	t.Parallel()
	f := instructionFleet(t, []int{2}, nil)
	f.fixtures[0].listener.SoftBanMode = true
	instructionForward(t, f, 1349)
	settleInstructions(t, f)
	text := instructionText(t, f, "restricted")
	require.Contains(t, text, "авторов с ограниченной отправкой: 1")
	require.NotContains(t, text, "авторов заблокировано")
	require.Equal(t, map[int64]int{501: 1}, f.fixtures[0].banCalls)
}

func TestInstructionLateProofRetiresProvisionalCard(t *testing.T) {
	t.Parallel()
	f := instructionFleet(t, []int{2, 2}, nil)
	instructionForward(t, f, 1349, 0)
	settleInstructions(t, f)
	instructionForward(t, f, 1350, 1)
	f.now = f.now.Add(20 * time.Second) // deterministic end of the bounded proof-collection window
	settleInstructions(t, f)
	require.Len(t, f.fixtures[0].cards, 2)
	instructionForward(t, f, 1349, 1)
	instructionForward(t, f, 1350, 0)
	settleInstructions(t, f)
	cases, err := f.store.Cases(context.Background())
	require.NoError(t, err)
	require.Len(t, cases, 2)
	active, alias := 0, 0
	for _, c := range cases {
		if c.Canonical == "" {
			active++
			continue
		}
		alias++
		text, buttons := sharedViewCard(moderation.SharedView{Case: c}, f.bridges[0].config.Peers, f.now)
		require.Contains(t, text, "Используйте общую карточку")
		require.Empty(t, buttons.InlineKeyboard)
	}
	require.Equal(t, 1, active)
	require.Equal(t, 1, alias)
	for _, fixture := range f.fixtures {
		require.Len(t, fixture.deleteCalls, 2)
		for _, calls := range fixture.deleteCalls {
			require.Equal(t, 1, calls)
		}
		for _, calls := range fixture.banCalls {
			require.Equal(t, 1, calls)
		}
	}
}

func TestInstructionRendererDoesNotInventMissingSuccess(t *testing.T) {
	t.Parallel()
	c := moderation.SharedCase{Policy: moderation.ForwardAllPolicy, Ref: strings.Repeat("a", 32), Revision: 100,
		Expires: time.Now().Add(time.Hour).UnixMilli()}
	for i := range 5 {
		r := moderation.SharedReport{Peer: int64(100 + i), Receipt: "current", Name: "<chat & title>", State: "accepted"}
		for j := range 20 {
			r.Targets = append(r.Targets, moderation.ForwardTarget{MessageID: 800 + j, IdentityID: 501,
				IdentityKind: "user", Deadline: time.Now().UnixMilli()})
			r.Actions = append(r.Actions, moderation.Action{Receipt: "current", MessageID: 800 + j,
				Kind: "original_delete", State: moderation.Confirmed})
		}
		c.Reports = append(c.Reports, r)
	}
	for page := range 26 {
		c.Page = page
		text, buttons := sharedViewCard(moderation.SharedView{Case: c}, []int64{100, 101, 102, 103, 104}, time.Now())
		require.NotContains(t, text, "✅ Спам обработан", "missing bans are not confirmed success")
		require.Less(t, len([]rune(text)), 4096)
		for _, row := range buttons.InlineKeyboard {
			for _, button := range row {
				require.LessOrEqual(t, len(*button.CallbackData), 64)
				require.True(t, strings.HasPrefix(*button.CallbackData, "S:v"))
			}
		}
	}
}
