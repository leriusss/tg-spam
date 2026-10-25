package events

import (
	"context"
	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage/engine"
	"github.com/umputun/tg-spam/lib/tgspam"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEditorialEmojiQueueAndWorker(t *testing.T) {
	for _, mode := range []string{"emoji only", "stopword", "external inline button", "burst", "han"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			db, err := engine.NewSqlite(filepath.Join(t.TempDir(), "queue.db"), "editorial")
			require.NoError(t, err)
			defer db.Close()
			j, err := moderation.NewJournal(ctx, db, moderation.DefaultLimits())
			require.NoError(t, err)
			require.NoError(t, j.EnableHanBans(ctx, time.Unix(1, 0)))
			l, _, _, _, update := trainingReportFixture(t, true)
			l.chatID = -100
			l.adminHandler.primChatID = -100
			update.Message.Chat.ID = -100
			update.Message.SenderChat.ID = -1001
			update.Message.Quote = nil
			update.Message.Text = "normal friendly conversation 💍 💀 💃 📅 ⏰ 🎟️ 🔥 ❗️"
			d := tgspam.NewDetector(tgspam.Config{MaxAllowedEmoji: 6, MinMsgLen: 1, FirstMessagesCount: 100})
			rules, err := bot.ParseEditorialEmojiExemptions("-100:-1001")
			require.NoError(t, err)
			params := bot.SpamConfig{EditorialEmoji: rules, Han: bot.HanConfig{Enabled: true, Ratio: 0.7, MinCount: 20}}
			if mode == "stopword" {
				_, err = d.LoadStopWords(strings.NewReader("forbidden"))
				require.NoError(t, err)
				update.Message.Text += " forbidden"
			}
			if mode == "external inline button" {
				update.Message.ReplyMarkup = &tbapi.InlineKeyboardMarkup{InlineKeyboard: [][]tbapi.InlineKeyboardButton{{tbapi.NewInlineKeyboardButtonURL("link", "https://example.invalid")}}}
			}
			if mode == "burst" {
				params.Burst = bot.BurstConfig{Enabled: true, Threshold: 3, CleanupEnabled: true}
			}
			if mode == "han" {
				update.Message.Text = strings.Repeat("中", 24) + " 💍 💀 💃 📅 ⏰ 🎟️ 🔥 ❗️"
			}
			filter := bot.NewSpamFilter(d, params)
			l.Bot = filter
			l.adminHandler.bot = filter
			l.TrainingMode = false
			l.adminHandler.trainingMode = false
			l.RecoveryJournal = j
			l.adminHandler.recovery = &recovery{journal: j}
			count := 1
			if mode == "burst" {
				count = 3
			}
			for mid := 1; mid <= count; mid++ {
				update.Message.MessageID = 700 + mid
				require.NoError(t, l.procEvents(update))
			}
			rows, err := j.Snapshot(ctx)
			require.NoError(t, err)
			kinds := map[string]int{}
			for _, a := range rows {
				kinds[a.Kind]++
			}
			require.False(t, filter.IsExplicitTrustedUser(-1001), "an emoji exemption must not create manual trust")
			if mode == "emoji only" {
				require.Empty(t, rows)
				return
			}
			if mode == "han" {
				require.Equal(t, 1, kinds["ban"])
				require.Empty(t, d.ApprovedUsers())
			} else {
				require.Equal(t, 1, kinds["ban"])
			}
			require.Equal(t, 1, kinds["original_delete"])
			if mode == "burst" {
				require.Equal(t, 2, kinds["cleanup_delete"])
			} else {
				require.Zero(t, kinds["cleanup_delete"])
			}
			deletes := map[int]int{}
			bans := 0
			r := &recovery{journal: j, gate: func(moderation.Action) bool { return true }, superName: func(string, int64) bool { return false }, api: recoveryAPIFunc(func(_ context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
				switch v := c.(type) {
				case tbapi.ChatInfoConfig:
					return apiOK(tbapi.ChatFullInfo{Chat: tbapi.Chat{ID: -1001}}), nil
				case tbapi.BanChatSenderChatConfig:
					require.EqualValues(t, -100, v.ChatID)
					require.EqualValues(t, -1001, v.SenderChatID)
					bans++
					return apiOK(true), nil
				case tbapi.DeleteMessageConfig:
					require.EqualValues(t, -100, v.ChatID)
					deletes[v.MessageID]++
					return apiOK(true), nil
				case tbapi.MessageConfig, tbapi.EditMessageTextConfig:
					return apiOK(tbapi.Message{MessageID: 999}), nil
				default:
					t.Fatalf("unexpected Telegram API %T", c)
					return nil, nil
				}
			})}
			now := time.Now()
			w := moderation.Worker{Journal: j, Executor: r, Now: func() time.Time { return now }}
			for pass := range 2 {
				for range 40 {
					worked, e := w.Step(ctx)
					require.NoError(t, e)
					if !worked {
						break
					}
				}
				if pass == 0 {
					update.Message.MessageID = 700 + count
					require.NoError(t, l.procEvents(update))
					now = now.Add(time.Minute)
				}
			}
			require.Equal(t, 1, bans)
			require.Len(t, deletes, count)
			for _, n := range deletes {
				require.Equal(t, 1, n, "redelivery must not repeat a confirmed original deletion")
			}
		})
	}
}
