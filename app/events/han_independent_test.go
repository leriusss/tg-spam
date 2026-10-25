package events

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage/engine"
	"github.com/umputun/tg-spam/lib/tgspam"
)

func TestHanIndependentModerationQueueAndWorker(t *testing.T) {
	for _, mode := range []string{"han", "external inline button", "stopword", "burst", "auto", "late button"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "journal.db")
			db, err := engine.NewSqlite(path, "fixture")
			require.NoError(t, err)
			defer func() { require.NoError(t, db.Close()) }()
			j, err := moderation.NewJournal(ctx, db, moderation.DefaultLimits())
			require.NoError(t, err)
			require.NoError(t, j.EnableHanBans(ctx, time.Unix(1, 0)))
			l, _, _, loc, update := trainingReportFixture(t, false)
			d := tgspam.NewDetector(tgspam.Config{FirstMessageOnly: mode == "auto", MinMsgLen: 1, MaxAllowedEmoji: -1})
			if mode == "stopword" {
				_, err = d.LoadStopWords(strings.NewReader("广告"))
				require.NoError(t, err)
			}
			config := bot.SpamConfig{Han: bot.HanConfig{Enabled: true, Ratio: 0.7, MinCount: 20}}
			if mode == "burst" {
				config.Burst = bot.BurstConfig{Enabled: true, Threshold: 3, CleanupEnabled: true}
			}
			filter := bot.NewSpamFilter(d, config)
			l.Bot, l.adminHandler.bot = filter, filter
			l.TrainingMode, l.adminHandler.trainingMode = false, false
			l.RecoveryJournal = j
			l.adminHandler.recovery = &recovery{journal: j}
			update.Message.Quote = nil
			if mode == "auto" {
				filter.OnMessage(bot.Message{ID: 10, From: bot.User{ID: 501}, Text: "обычный нормальный текст"}, false)
			}
			before := d.ApprovedUsers()
			update.Message.Text = strings.Repeat("你好世界", 7) + " 广告"
			if mode == "external inline button" {
				update.Message.ReplyMarkup = &tbapi.InlineKeyboardMarkup{InlineKeyboard: [][]tbapi.InlineKeyboardButton{{tbapi.NewInlineKeyboardButtonURL("link", "https://example.invalid")}}}
			}
			count := 1
			if mode == "burst" {
				count = 3
			}
			for i := 0; i < count; i++ {
				update.Message.MessageID = 701 + i
				require.NoError(t, l.procEvents(update))
			}
			require.Equal(t, before, d.ApprovedUsers(), "Han must never accumulate trust")
			rows, err := j.Snapshot(ctx)
			require.NoError(t, err)
			independent := mode != "han" && mode != "auto" && mode != "late button"
			kinds := map[string]int{}
			for _, a := range rows {
				kinds[a.Kind]++
			}
			require.Equal(t, count, kinds["ban"], "one ban decision per new original; shared grounds coalesce")
			require.Equal(t, count, kinds["original_delete"])
			if mode == "burst" {
				require.Equal(t, 2, kinds["cleanup_delete"], "independent burst cleanup must be admitted")
			} else {
				require.Zero(t, kinds["cleanup_delete"])
			}
			// burst cleanup overlaps earlier originals: the journal must coalesce each message pair.
			deletes := map[int]int{}
			bans := 0
			r := &recovery{journal: j, gate: func(moderation.Action) bool { return true }, superName: func(string, int64) bool { return false },
				api: recoveryAPIFunc(func(callCtx context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
					_, ok := callCtx.Deadline()
					require.True(t, ok)
					switch req := c.(type) {
					case tbapi.GetChatMemberConfig:
						status := "member"
						if bans > 0 {
							status = "kicked"
						}
						return apiOK(tbapi.ChatMember{User: &tbapi.User{ID: 501}, Status: status}), nil
					case tbapi.DeleteMessageConfig:
						deletes[req.MessageID]++
						return apiOK(true), nil
					case tbapi.BanChatMemberConfig:
						bans++
						return apiOK(true), nil
					case tbapi.MessageConfig, tbapi.EditMessageTextConfig:
						return apiOK(tbapi.Message{MessageID: 999}), nil
					default:
						t.Fatalf("unexpected API %T", c)
						return nil, nil
					}
				})}
			now := time.Now()
			driveWorker := func(journal *moderation.Journal) {
				w := moderation.Worker{Journal: journal, Executor: r, Now: func() time.Time { return now }}
				for range 50 {
					worked, stepErr := w.Step(ctx)
					require.NoError(t, stepErr)
					if !worked {
						return
					}
				}
				t.Fatal("worker did not become idle")
			}
			driveWorker(j)
			card, err := j.CardView(ctx, fmt.Sprintf("source:123:%d:user:501", update.Message.MessageID))
			require.NoError(t, err)
			require.Contains(t, card.Reason, "Преобладает письменность Han")
			if independent {
				require.Contains(t, card.Reason, "Независимое основание")
			}
			require.Contains(t, compactCardText(card, "fixture"), "Сообщение удалено")
			if mode == "burst" {
				require.Contains(t, card.Reason, "burst")
				require.Len(t, loc.AddSpamCalls(), 1)
			}
			for id := 701; id < 701+count; id++ {
				require.Equal(t, 1, deletes[id])
			}
			require.NoError(t, db.Close())
			db, err = engine.NewSqlite(path, "fixture")
			require.NoError(t, err)
			j, err = moderation.NewJournal(ctx, db, moderation.DefaultLimits())
			require.NoError(t, err)
			l.RecoveryJournal, l.adminHandler.recovery.journal = j, j
			if mode == "late button" {
				update.Message.ReplyMarkup = &tbapi.InlineKeyboardMarkup{InlineKeyboard: [][]tbapi.InlineKeyboardButton{{tbapi.NewInlineKeyboardButtonURL("link", "https://example.invalid")}}}
			}
			r.journal = j
			for range 2 {
				require.NoError(t, l.procEvents(update))
				now = now.Add(time.Minute)
				driveWorker(j)
			}
			for reason := range strings.SplitSeq(card.Reason, "\n") {
				require.Contains(t, mustHanCard(t, j, card.Receipt).Reason, reason)
			}
			if mode == "late button" {
				require.NotContains(t, mustHanCard(t, j, card.Receipt).Reason, "внешняя кнопка со ссылкой", "re-delivery does not rewrite accepted automatic decisions")
			}
			for id := 701; id < 701+count; id++ {
				require.Equal(t, 1, deletes[id], "confirmed delete cannot be replayed after restart")
			}
			require.Len(t, deletes, count)
			require.Equal(t, 1, bans)
		})
	}
}

func mustHanCard(t *testing.T, j *moderation.Journal, key string) moderation.Card {
	t.Helper()
	c, err := j.CardView(context.Background(), key)
	require.NoError(t, err)
	return c
}
