package events

import (
	"context"
	"path/filepath"
	"testing"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/app/storage/engine"
	"github.com/umputun/tg-spam/lib/approved"
	"github.com/umputun/tg-spam/lib/tgspam"
)

func TestConfirmedExactTextPipeline(t *testing.T) {
	for _, mode := range []string{"auto", "manual", "superuser", "complaint quote", "complaint reply", "sender chat"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			db, err := engine.NewSqlite(filepath.Join(t.TempDir(), "approved.db"), "fixture")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			users, err := storage.NewApprovedUsers(ctx, db)
			require.NoError(t, err)
			d := tgspam.NewDetector(tgspam.Config{
				MaxAllowedEmoji: -1, MinMsgLen: 1, FirstMessageOnly: true, FirstMessagesCount: 1,
			})
			_, err = d.WithUserStorage(users)
			require.NoError(t, err)
			filter := bot.NewSpamFilter(d, bot.SpamConfig{})
			l, api, _, _, update := trainingReportFixture(t, mode == "sender chat")
			l.Bot, l.adminHandler.bot = filter, filter
			l.TrainingMode, l.adminHandler.trainingMode = false, false
			l.RecoveryJournal = recoveryJournal(t)
			l.adminHandler.recovery = &recovery{journal: l.RecoveryJournal}
			update.Message.Text, update.Message.Quote = "обычное сообщение", nil
			require.NoError(t, l.procEvents(update))
			stored, err := users.Read(ctx)
			require.NoError(t, err)
			require.Len(t, stored, 1)
			require.Equal(t, approved.SourceAuto, stored[0].Source)
			// reloading exercises persisted automatic approval, not only the in-memory counter.
			_, err = d.WithUserStorage(users)
			require.NoError(t, err)
			update.Message.MessageID++
			update.Message.Text = "Есть проба 0.5г"
			update.Message.ReplyToMessage = &tbapi.Message{Text: "Выходные в парке", From: &tbapi.User{ID: 100}}
			switch mode {
			case "manual":
				require.NoError(t, filter.AddApprovedUser(501, "source"))
			case "superuser":
				l.SuperUsers = SuperUsers{"501"}
			case "complaint quote":
				update.Message.Text = "Удалите эту рекламу"
				update.Message.Quote = &tbapi.TextQuote{Text: "Есть проба 0.5г"}
			case "complaint reply":
				update.Message.Text = "Удалите эту рекламу"
				update.Message.ReplyToMessage.Text = "Есть проба 0.5г"
			}
			for range 2 {
				require.NoError(t, l.procEvents(update))
			}
			rows, err := l.RecoveryJournal.Snapshot(ctx)
			require.NoError(t, err)
			if mode == "auto" || mode == "sender chat" {
				require.Len(t, rows, 4, "ban, original deletion, card and internal notice; redelivery is idempotent")
				for _, action := range rows {
					require.Equal(t, "pending", action.State)
				}
			} else {
				require.Empty(t, rows)
			}
			require.Empty(t, api.RequestCalls(), "acceptance must not execute live Telegram actions")
		})
	}
}
