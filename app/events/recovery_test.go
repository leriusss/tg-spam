package events

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/app/storage/engine"
)

type recoveryAPIFunc func(context.Context, tbapi.Chattable) (*tbapi.APIResponse, error)

func (f recoveryAPIFunc) RequestWithContext(ctx context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
	return f(ctx, c)
}

func recoveryJournal(t *testing.T) *moderation.Journal {
	t.Helper()
	db, err := engine.NewSqlite(filepath.Join(t.TempDir(), "journal.db"), "fixture")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	j, err := moderation.NewJournal(context.Background(), db, moderation.DefaultLimits())
	require.NoError(t, err)
	return j
}

func apiOK(v any) *tbapi.APIResponse {
	b, _ := json.Marshal(v)
	return &tbapi.APIResponse{Ok: true, Result: b}
}

func TestRecoveryTelegramClassification(t *testing.T) {
	for _, code := range []int{400, 403, 429, 500, 501, 502, 503, 504} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			out := telegramOutcome(
				nil,
				&tbapi.Error{
					Code:               code,
					Message:            "not persisted",
					ResponseParameters: tbapi.ResponseParameters{RetryAfter: 123},
				},
			)
			require.Equal(t, code == 429 || code == 500 || code == 502 || code == 503 || code == 504, out.Retry)
			if code == 429 {
				require.Equal(t, 123*time.Second, out.RetryAfter)
			}
		})
	}
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF, context.DeadlineExceeded, &net.OpError{Op: "read", Err: fmt.Errorf("reset")}} {
		out := telegramOutcome(nil, err)
		require.True(t, out.Retry)
		require.True(t, out.Uncertain)
	}
	require.True(t, telegramOutcome(&tbapi.APIResponse{ErrorCode: 503}, nil).Retry)
}

func TestRecoveryLostBanResponseReconcilesWithoutReban(t *testing.T) {
	calls := 0
	r := &recovery{
		gate:      func(moderation.Action) bool { return true },
		superName: func(string, int64) bool { return false },
		api: recoveryAPIFunc(func(_ context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
			calls++
			require.IsType(t, tbapi.GetChatMemberConfig{}, c)
			return apiOK(tbapi.ChatMember{User: &tbapi.User{ID: 7}, Status: "kicked", UntilDate: 0}), nil
		}),
	}
	out := r.Execute(
		context.Background(),
		moderation.Action{
			Kind:         "ban",
			ChatID:       -100,
			IdentityID:   7,
			IdentityKind: "user",
			Uncertain:    true,
			UntilDate:    time.Now().Add(400 * 24 * time.Hour).Unix(),
		},
	)
	require.Equal(t, moderation.Confirmed, out.State)
	require.Equal(t, "reconciled_membership", out.Code)
	require.Equal(t, 1, calls)
	// Sender-chat state is not queryable: do not blindly replay its uncertain ban.
	r.api = recoveryAPIFunc(func(_ context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
		require.IsType(t, tbapi.ChatInfoConfig{}, c)
		return apiOK(tbapi.ChatFullInfo{Chat: tbapi.Chat{ID: -7, Type: "channel"}}), nil
	})
	out = r.Execute(
		context.Background(),
		moderation.Action{Kind: "ban", ChatID: -100, IdentityID: -7, IdentityKind: "sender_chat", Uncertain: true},
	)
	require.Equal(t, moderation.Manual, out.State)
}

func TestRecoveryGatesAndAmbiguousDeletion(t *testing.T) {
	allowed := false
	calls := 0
	r := &recovery{
		gate:      func(moderation.Action) bool { return allowed },
		superName: func(name string, _ int64) bool { return name == "operator" },
		api: recoveryAPIFunc(func(_ context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
			calls++
			if _, ok := c.(tbapi.GetChatMemberConfig); ok {
				return apiOK(tbapi.ChatMember{User: &tbapi.User{ID: 7, UserName: "operator"}, Status: "member"}), nil
			}
			t.Fatal("safety gate allowed a mutation")
			return nil, nil
		}),
	}
	a := moderation.Action{Kind: "original_delete", ChatID: -100, IdentityID: 7, IdentityKind: "user", MessageID: 99}
	require.Equal(t, moderation.Blocked, r.Execute(context.Background(), a).State)
	require.Zero(t, calls)
	allowed = true
	require.Equal(t, moderation.Blocked, r.Execute(context.Background(), a).State)
	require.Equal(t, 1, calls)
	r.superName = func(string, int64) bool { return false }
	r.api = recoveryAPIFunc(func(_ context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
		if _, ok := c.(tbapi.GetChatMemberConfig); ok {
			return apiOK(tbapi.ChatMember{User: &tbapi.User{ID: 7}, Status: "member"}), nil
		}
		return nil, &tbapi.Error{Code: 400, Message: "Bad Request: message to delete not found"}
	})
	a.Uncertain = true
	out := r.Execute(context.Background(), a)
	require.Equal(t, moderation.Absent, out.State)
	require.True(t, out.Uncertain)
	require.NotEqual(t, moderation.Confirmed, out.State)
	r.api = recoveryAPIFunc(func(_ context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
		if _, ok := c.(tbapi.GetChatMemberConfig); ok {
			return apiOK(tbapi.ChatMember{User: &tbapi.User{ID: 7}, Status: "member"}), nil
		}
		return apiOK(false), nil
	})
	out = r.Execute(context.Background(), a)
	require.True(t, out.Retry)
	require.True(t, out.Uncertain, "ok=true with missing/false mutation result is not confirmed")
}

func TestRecoveryForwardRedeliveryAndUnknownOriginal(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(fmt.Sprint(exact), func(t *testing.T) {
			r := newAdminForwardReplay(t, storage.MsgMeta{ChatID: 123, UserID: 501, MsgID: 701}, true)
			j := recoveryJournal(t)
			r.handler.recovery = &recovery{journal: j}
			msg := replayAdminMessage(userOrigin(501, "source"), "same text")
			msg.Chat = tbapi.Chat{ID: 999}
			msg.MessageID = 100
			if exact {
				r.handler.primChatID = -123
				msg.ForwardOrigin = &tbapi.MessageOrigin{Type: "channel", Chat: &tbapi.Chat{ID: -123}, MessageID: 701}
				r.locator.MessageFunc = func(context.Context, string) (storage.MsgMeta, bool) {
					return storage.MsgMeta{ChatID: -123, UserID: -123, MsgID: 701}, true
				}
			}
			update := tbapi.Update{Message: msg}
			require.NoError(t, r.handler.MsgHandler(update))
			require.NoError(t, r.handler.MsgHandler(update))
			msg.MessageID = 101
			require.NoError(t, r.handler.MsgHandler(update))
			require.Empty(t, r.api.RequestCalls(), "handler enqueues before API; worker is the API owner")
			if exact {
				require.Len(t, r.bot.UpdateSpamCalls(), 1)
			} else {
				require.Len(t, r.bot.UpdateSpamCalls(), 2, "a new user-origin forward without original ID is not the same delivery")
			}
			rows, err := j.Snapshot(context.Background())
			require.NoError(t, err)
			receipts := map[string]bool{}
			for _, a := range rows {
				if a.Kind != "notice" {
					receipts[a.Receipt] = true
				}
			}
			if exact {
				require.Len(t, receipts, 1)
			} else {
				require.Len(t, receipts, 2)
			}
		})
	}
}

func TestRecoveryFallbackKeepsOwnChatAndManualOriginal(t *testing.T) {
	r := newAdminForwardReplay(t, storage.MsgMeta{}, false)
	j := recoveryJournal(t)
	r.handler.recovery = &recovery{journal: j}
	m := replayAdminMessage(userOrigin(501, "source"), "spam")
	m.Chat = tbapi.Chat{ID: 999}
	m.MessageID = 44
	require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: m}))
	rows, err := j.Snapshot(context.Background())
	require.NoError(t, err)
	for _, a := range rows {
		if a.Kind == "notice" {
			continue
		}
		require.EqualValues(t, 123, a.ChatID)
		if a.Kind == "original_delete" {
			require.Equal(t, moderation.Manual, a.State)
			require.Zero(t, a.MessageID)
		}
	}
	require.Empty(t, r.api.RequestCalls())
}

func TestRecoveryAutomaticUsesSameJournalAndTrainingStaysLegacy(t *testing.T) {
	l, api, _, _, update := trainingReportFixture(t, false)
	j := recoveryJournal(t)
	l.RecoveryJournal = j
	l.adminHandler.recovery = &recovery{journal: j}
	l.TrainingMode = false
	l.adminHandler.trainingMode = false
	l.NoSpamReply = true
	require.NoError(t, l.procEvents(update))
	require.NoError(t, l.procEvents(update))
	require.Empty(t, api.RequestCalls())
	rows, err := j.Snapshot(context.Background())
	require.NoError(t, err)
	sources := map[string]bool{}
	for _, a := range rows {
		if a.Kind != "notice" {
			sources[a.Receipt] = true
		}
	}
	require.Len(t, sources, 1)
	// Training remains outside durable destructive admission and keeps its existing buttons.
	trainingL, trainingAPI, b, _, trainingUpdate := trainingReportFixture(t, false)
	l, api, update = trainingL, trainingAPI, trainingUpdate
	l.RecoveryJournal = recoveryJournal(t)
	l.adminHandler.recovery = &recovery{journal: l.RecoveryJournal}
	require.NoError(t, l.procEvents(update))
	require.Empty(t, api.RequestCalls())
	require.NotEmpty(t, api.SendCalls())
	msg := api.SendCalls()[0].C.(tbapi.MessageConfig)
	require.NotNil(t, msg.ReplyMarkup)
	markup, err := json.Marshal(msg.ReplyMarkup)
	require.NoError(t, err)
	require.Contains(t, string(markup), "review detection")
	require.Empty(t, b.UpdateSpamCalls())
	rows, err = l.RecoveryJournal.Snapshot(context.Background())
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestRecoveryNotificationLostAckAndKnownMessageEdit(t *testing.T) {
	j := recoveryJournal(t)
	r := &recovery{
		journal: j,
		gate:    func(moderation.Action) bool { return true },
		api: recoveryAPIFunc(func(_ context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
			require.IsType(t, tbapi.MessageConfig{}, c)
			return nil, io.EOF
		}),
	}
	a := moderation.Action{Kind: "notice", ChatID: -900, Generation: 1}
	out := r.Execute(context.Background(), a)
	require.True(t, out.Retry)
	require.True(t, out.Uncertain)
	a.NotificationID = 10
	r.api = recoveryAPIFunc(func(_ context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
		edit, ok := c.(tbapi.EditMessageTextConfig)
		require.True(t, ok)
		require.Equal(t, 10, edit.MessageID)
		return apiOK(tbapi.Message{MessageID: 10}), nil
	})
	out = r.Execute(context.Background(), a)
	require.Equal(t, moderation.Confirmed, out.State)
	require.Equal(t, 10, out.NotificationID)
}

func TestRecoveryAdminSafetyRejectCreatesNoReceipt(t *testing.T) {
	r := newAdminForwardReplay(t, storage.MsgMeta{ChatID: 123, UserID: 501, MsgID: 701}, true)
	j := recoveryJournal(t)
	r.handler.recovery = &recovery{journal: j}
	r.handler.isExplicitlyApproved = func(int64) bool { return true }
	m := replayAdminMessage(userOrigin(501, "source"), "spam")
	m.Chat = tbapi.Chat{ID: 999}
	m.MessageID = 44
	require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: m}))
	rows, err := j.Snapshot(context.Background())
	require.NoError(t, err)
	require.Empty(t, rows)
	require.Empty(t, r.bot.UpdateSpamCalls())
	require.Empty(t, r.api.RequestCalls())
}
