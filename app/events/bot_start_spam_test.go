package events

import (
	"context"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/lib/approved"
	"github.com/umputun/tg-spam/lib/tgspam"
)

func TestBotStartSpamListenerJournalAndWorker(t *testing.T) {
	for _, mode := range []string{"auto", "manual", "administrator", "forward", "quote", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			f := newSenderProvenanceFixture(t)
			ctx := context.Background()
			l, _, _, _, update := trainingReportFixture(t, false)
			loc, err := storage.NewLocator(ctx, time.Hour, 1000, f.db)
			require.NoError(t, err)
			l.Locator, l.adminHandler.locator = loc, loc
			l.TrainingMode, l.adminHandler.trainingMode = false, false
			l.RecoveryJournal, l.adminHandler.recovery = f.journal, f.adapter
			l.AggressiveCleanup, l.AggressiveCleanupLimit = true, 50
			l.BotStartSpamEnabled = mode != "disabled"
			d := tgspam.NewDetector(tgspam.Config{FirstMessageOnly: true, FirstMessagesCount: 1, MinMsgLen: 1, MaxAllowedEmoji: -1})
			filter := bot.NewSpamFilter(d, bot.SpamConfig{BotStartSpamEnabled: l.BotStartSpamEnabled})
			l.Bot, l.adminHandler.bot = filter, filter
			update.Message.Quote = nil
			update.Message.Text = "обычный текст"
			for _, id := range []int{701, 702} {
				update.UpdateID, update.Message.MessageID = id+1000, id
				require.NoError(t, l.procEvents(update))
			}
			require.True(t, filter.IsApprovedUserInChat(501, 123))
			if mode == "manual" {
				require.NoError(t, d.AddApprovedUser(approved.UserInfo{UserID: "501", Source: approved.SourceManual}))
			}
			if mode == "administrator" {
				l.SuperUsers, l.adminHandler.superUsers = SuperUsers{"501"}, SuperUsers{"501"}
			}
			update.UpdateID, update.Message.MessageID = 1703, 703
			update.Message.Text = "/start 42 @first_bot\n@second_bot\n@third_bot"
			if mode == "forward" {
				update.Message.ForwardOrigin = &tbapi.MessageOrigin{Type: "user", SenderUser: &tbapi.User{ID: 777}}
			}
			if mode == "quote" {
				update.Message.Quote = &tbapi.TextQuote{Text: update.Message.Text}
				update.Message.Text = "Удалите эту рекламу"
			}
			before := d.ApprovedUsers()
			require.NoError(t, loc.AddMessage(ctx, "other chat", 999, 501, "source", 704))
			require.NoError(t, loc.AddMessage(ctx, "other author", 123, 777, "other", 705))
			require.NoError(t, l.procEvents(update))
			rows, err := f.journal.Snapshot(ctx)
			require.NoError(t, err)
			if mode != "auto" {
				require.Empty(t, rows)
				require.Zero(t, f.bans)
				require.Empty(t, f.deletes)
				return
			}
			require.Equal(t, before, d.ApprovedUsers())
			kinds := map[string]int{}
			for _, row := range rows {
				kinds[row.Kind]++
			}
			require.Equal(t, 1, kinds["ban"])
			require.Equal(t, 1, kinds["original_delete"])
			require.Equal(t, 2, kinds["cleanup_delete"])
			f.drain(t, time.Now())
			require.Equal(t, 1, f.bans)
			require.Equal(t, map[int]int{701: 1, 702: 1, 703: 1}, f.deletes)
			require.NotEmpty(t, f.cards)
			card, err := f.journal.CardView(ctx, "source:123:703:user:501")
			require.NoError(t, err)
			require.Contains(t, detailReasons(card), "минимум трёх разных")
			f.reopen(t)
			l.RecoveryJournal = f.journal
			require.NoError(t, l.procEvents(update))
			f.drain(t, time.Now())
			require.Equal(t, 1, f.bans, "replaying an accepted update must not repeat moderation")
			require.Equal(t, map[int]int{701: 1, 702: 1, 703: 1}, f.deletes)
		})
	}
}
