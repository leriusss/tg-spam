package events

import (
	"context"
	"io"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage"
)

func TestRecoveryWorkerLostAcknowledgementAndDeleteRecovery(t *testing.T) {
	j := recoveryJournal(t)
	now := time.Now()
	p := moderation.Plan{
		Key:          "source:-100:701:user:501",
		ChatID:       -100,
		MessageID:    701,
		IdentityID:   501,
		IdentityKind: "user",
		Destination:  -900,
		Actions: []moderation.Action{
			{Kind: "ban", ChatID: -100, IdentityID: 501, IdentityKind: "user"},
			{
				Kind:         "original_delete",
				ChatID:       -100,
				IdentityID:   501,
				IdentityKind: "user",
				MessageID:    701,
				MessageDate:  now.Unix(),
			},
		},
	}
	_, _, fresh, err := j.Accept(context.Background(), p, now)
	require.NoError(t, err)
	require.True(t, fresh)
	banCalls, deleteCalls := 0, 0
	serverBanned := false
	r := &recovery{
		journal:   j,
		gate:      func(moderation.Action) bool { return true },
		superName: func(string, int64) bool { return false },
		api: recoveryAPIFunc(func(ctx context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
			_, ok := ctx.Deadline()
			require.True(t, ok)
			switch c.(type) {
			case tbapi.GetChatMemberConfig:
				status := "member"
				if serverBanned {
					status = "kicked"
				}
				return apiOK(tbapi.ChatMember{User: &tbapi.User{ID: 501}, Status: status}), nil
			case tbapi.BanChatMemberConfig:
				banCalls++
				serverBanned = true
				return nil, io.EOF
			case tbapi.DeleteMessageConfig:
				deleteCalls++
				if deleteCalls == 1 {
					return nil, io.ErrUnexpectedEOF
				}
				return apiOK(true), nil
			case tbapi.MessageConfig, tbapi.EditMessageTextConfig:
				return apiOK(tbapi.Message{MessageID: 999}), nil
			default:
				t.Fatalf("unexpected API %T", c)
				return nil, nil
			}
		}),
	}
	w := moderation.Worker{
		Journal:  j,
		Executor: r,
		Now:      func() time.Time { return now },
		Jitter:   func() float64 { return 0 },
	}
	for range 20 {
		worked, e := w.Step(context.Background())
		require.NoError(t, e)
		if !worked {
			break
		}
	}
	rows, err := j.Snapshot(context.Background())
	require.NoError(t, err)
	for _, a := range rows {
		if a.Kind == "ban" || a.Kind == "original_delete" {
			require.Equal(t, moderation.Pending, a.State)
			require.True(t, a.Uncertain)
		}
	}
	now = now.Add(2 * time.Second)
	for range 20 {
		worked, e := w.Step(context.Background())
		require.NoError(t, e)
		if !worked {
			break
		}
	}
	rows, err = j.Snapshot(context.Background())
	require.NoError(t, err)
	for _, a := range rows {
		if a.Kind == "ban" || a.Kind == "original_delete" {
			require.Equal(t, moderation.Confirmed, a.State)
		}
	}
	require.Equal(t, 1, banCalls, "lost ban response is reconciled, not replayed")
	require.Equal(t, 2, deleteCalls)
}

func TestRecoveryCommunityThresholdUsesJournalAndBestEffortLearning(t *testing.T) {
	r := newAdminForwardReplay(t, storage.MsgMeta{}, false)
	j := recoveryJournal(t)
	r.handler.recovery = &recovery{journal: j}
	r.bot.UpdateSpamFunc = func(string) error { return io.ErrUnexpectedEOF }
	reports := &userReports{
		bot:           r.bot,
		locator:       r.locator,
		superUsers:    SuperUsers{},
		primChatID:    123,
		adminChatID:   456,
		recoveryAdmin: r.handler,
	}
	data := []storage.Report{{MsgID: 701, ChatID: 123, ReportedUserID: 501, MsgText: "fixture spam"}}
	require.NoError(t, reports.executeAutoBan(context.Background(), data, time.Now().Unix()))
	require.NoError(t, reports.executeAutoBan(context.Background(), data, time.Now().Unix()))
	require.Len(t, r.bot.UpdateSpamCalls(), 1)
	require.Empty(t, r.api.RequestCalls())
	rows, err := j.Snapshot(context.Background())
	require.NoError(t, err)
	for _, a := range rows {
		if a.Kind == "ban" || a.Kind == "original_delete" {
			require.Equal(t, moderation.Pending, a.State)
		}
		if a.Kind == "learning_note" {
			require.Equal(t, moderation.Manual, a.State)
		}
	}
}
