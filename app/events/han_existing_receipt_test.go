package events

import (
	"context"
	"strings"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/lib/tgspam"
)

func TestHanExistingReceiptIndependentListenerWorker(t *testing.T) {
	for _, prior := range []string{"sender request", "delete request", "historical Han"} {
		for _, verdict := range []string{"button", "burst"} {
			for _, reverse := range []bool{false, true} {
				t.Run(prior+"/"+verdict+"/"+map[bool]string{false: "request first", true: "detector first"}[reverse], func(t *testing.T) {
					f := newSenderProvenanceFixture(t)
					ctx, now := context.Background(), time.Now()
					require.NoError(t, f.journal.EnableHanBans(ctx, now.Add(-time.Minute)))
					l, _, _, _, u := trainingReportFixture(t, false)
					l.TrainingMode, l.adminHandler.trainingMode = false, false
					l.SuperUsers, l.adminHandler.superUsers = SuperUsers{"11"}, SuperUsers{"11"}
					loc, err := storage.NewLocator(ctx, time.Hour, 1000, f.db)
					require.NoError(t, err)
					l.Locator, l.adminHandler.locator = loc, loc
					l.RecoveryJournal, l.adminHandler.recovery = f.journal, f.adapter
					u.Message.Quote, u.Message.Text = nil, strings.Repeat("中", 24)
					require.NoError(t, loc.AddMessage(ctx, u.Message.Text, 123, 501, "source", 701))
					require.NoError(t, loc.RecordMessageDate(ctx, 123, 701, int64(u.Message.Date)))
					config := bot.SpamConfig{Han: bot.HanConfig{Enabled: true, Ratio: .7, MinCount: 20}}
					if verdict == "burst" {
						config.Burst = bot.BurstConfig{Enabled: true, Threshold: 3, CleanupEnabled: true}
					}
					filter := bot.NewSpamFilter(tgspam.NewDetector(tgspam.Config{MinMsgLen: 1, MaxAllowedEmoji: -1}), config)
					l.Bot, l.adminHandler.bot = filter, filter
					request := func() {
						switch prior {
						case "sender request":
							cmd := tbapi.Update{Message: &tbapi.Message{MessageID: 900, Chat: tbapi.Chat{ID: 123}, Text: "/spam",
								From: &tbapi.User{ID: 136817688, IsBot: true}, SenderChat: &tbapi.Chat{ID: -777}, ReplyToMessage: u.Message}}
							require.True(t, l.procSenderSpam(cmd))
						case "delete request":
							forward := tbapi.Update{Message: &tbapi.Message{MessageID: 901, Chat: tbapi.Chat{ID: 456}, From: &tbapi.User{ID: 11},
								Text: u.Message.Text, ForwardOrigin: &tbapi.MessageOrigin{Type: "hidden_user", SenderUserName: "unknown"}}}
							require.NoError(t, l.adminHandler.legacyForward(forward))
						case "historical Han":
							p := senderProvenancePlan(now, "detector")
							p.Actions, p.Reason = p.Actions[:1], "Историческое Han: только удаление"
							f.accept(t, p, now)
						}
					}
					if !reverse {
						request()
						// A Han-only replay may neither add a ban nor rewrite the old card.
						before, e := f.journal.Snapshot(ctx)
						require.NoError(t, e)
						require.NoError(t, l.procEvents(u))
						after, e := f.journal.Snapshot(ctx)
						require.NoError(t, e)
						require.Equal(t, before, after)
					}
					if verdict == "button" {
						u.Message.ReplyMarkup = &tbapi.InlineKeyboardMarkup{InlineKeyboard: [][]tbapi.InlineKeyboardButton{{tbapi.NewInlineKeyboardButtonURL("link", "https://example.invalid")}}}
					} else {
						// Populate the ordinary burst history without Han authority for the warmups.
						empty := ""
						for _, mid := range []int{702, 703} {
							filter.OnMessage(bot.Message{ID: mid, ChatID: 123, From: bot.User{ID: 501}, Text: u.Message.Text, HanText: &empty}, false)
						}
					}
					require.NoError(t, l.procEvents(u))
					if reverse {
						request()
					}
					c := f.view(t)
					require.Contains(t, c.Reason, "Независимое основание")
					require.Contains(t, compactCardText(c, "fixture"), "Независимое основание")
					if !reverse {
						for _, d := range c.Origins {
							require.NotEqual(t, moderation.HanOrigin, d.Origin, "independent admission does not upgrade old Han authority")
						}
					}
					f.drain(t, time.Now().Add(time.Minute))
					require.Equal(t, 1, f.bans)
					require.Equal(t, 1, f.deletes[701])
					want := 1
					if verdict == "burst" {
						want = 3
						require.Equal(t, 1, f.deletes[702])
						require.Equal(t, 1, f.deletes[703])
						require.Contains(t, c.Reason, "burst")
					}
					require.Len(t, f.deletes, want)
					f.reopen(t)
					l.RecoveryJournal = f.journal
					// Rebind the locator as well: no closed database handles across restart.
					loc, err = storage.NewLocator(ctx, time.Hour, 1000, f.db)
					require.NoError(t, err)
					l.Locator, l.adminHandler.locator = loc, loc
					for range 2 {
						require.NoError(t, l.procEvents(u))
						request()
						f.drain(t, time.Now().Add(2*time.Minute))
					}
					require.Equal(t, 1, f.bans)
					require.Len(t, f.deletes, want)
					for _, count := range f.deletes {
						require.Equal(t, 1, count)
					}
					require.Contains(t, f.view(t).Reason, "Независимое основание")
				})
			}
		}
	}
}
