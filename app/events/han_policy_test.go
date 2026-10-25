package events

import (
	"context"
	"fmt"
	"net"
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

func TestHanPolicyTelegramOutcomes(t *testing.T) {
	for _, mode := range []string{"user", "channel", "no author", "technical From", "Telegram service",
		"invalid channel", "genuine bot", "user named Telegram", "ban refused", "delete refused", "delete absent",
		"ban retry", "rate limited", "delete retry", "channel uncertain", "soft ban", "editorial emoji"} {
		t.Run(mode, func(t *testing.T) {
			ctx, now := context.Background(), time.Now()
			path := filepath.Join(t.TempDir(), "queue.db")
			db, err := engine.NewSqlite(path, "fixture")
			require.NoError(t, err)
			defer func() { require.NoError(t, db.Close()) }()
			j, err := moderation.NewJournal(ctx, db, moderation.DefaultLimits())
			require.NoError(t, err)
			require.NoError(t, j.EnableHanBans(ctx, time.Unix(1, 0)))
			channel := mode == "channel" || mode == "channel uncertain" || mode == "editorial emoji"
			l, _, _, loc, update := trainingReportFixture(t, channel)
			l.TrainingMode, l.SoftBanMode = false, mode == "soft ban"
			l.adminHandler.trainingMode = false
			l.RecoveryJournal = j
			l.adminHandler.recovery = &recovery{journal: j}
			params := bot.SpamConfig{Han: bot.HanConfig{Enabled: true, Ratio: 0.7, MinCount: 20}}
			if mode == "editorial emoji" {
				l.chatID, l.adminHandler.primChatID, update.Message.Chat.ID = -100, -100, -100
				params.EditorialEmoji, err = bot.ParseEditorialEmojiExemptions("-100:-1001")
				require.NoError(t, err)
			}
			d := tgspam.NewDetector(tgspam.Config{FirstMessagesCount: 3, MaxAllowedEmoji: 6, MinMsgLen: 1})
			filter := bot.NewSpamFilter(d, params)
			l.Bot, l.adminHandler.bot = filter, filter
			update.Message.Quote = nil
			update.Message.Text = strings.Repeat("中", 24)
			if mode == "editorial emoji" {
				update.Message.Text += " 💍💀💃📅⏰🎟️🔥❗️"
			}
			switch mode {
			case "no author":
				update.Message.From = nil
			case "technical From":
				update.Message.From = &tbapi.User{ID: 136817688, UserName: "looks_reliable", IsBot: true}
			case "Telegram service":
				update.Message.From = &tbapi.User{ID: 777000}
			case "invalid channel":
				update.Message.SenderChat = &tbapi.Chat{ID: 124, UserName: "looks_reliable"}
			case "genuine bot":
				update.Message.From.IsBot = true
			case "user named Telegram":
				update.Message.From.FirstName, update.Message.From.UserName = "Telegram", "Telegram"
			}
			reliable := mode != "no author" && mode != "technical From" && mode != "Telegram service" && mode != "invalid channel"
			require.NoError(t, l.procEvents(update))
			target, identity, _ := hanOriginalIdentity(update.Message)
			key := fmt.Sprintf("source:%d:%d:%s:%d", update.Message.Chat.ID, update.Message.MessageID, identity, target)
			initial := mustHanCard(t, j, key)
			require.NotContains(t, compactCardText(initial, "fixture"), "Сообщение удалено")
			require.NotContains(t, compactCardText(initial, "fixture"), "заблокирован навсегда")
			require.Contains(t, initial.Reason, "24 из 24 букв (100.0%); порог 70.0%, минимум 20")
			if !reliable {
				require.Equal(t, "message", initial.IdentityKind)
				require.Zero(t, initial.IdentityID)
				require.Contains(t, initial.Reason, "не содержит надёжной числовой цели")
				require.Len(t, cardKeyboard(initial, true).InlineKeyboard, 1)
			}
			banCalls, deleteCalls := 0, 0
			banAcknowledged := false
			var deliveredCards []string
			r := &recovery{journal: j, gate: func(moderation.Action) bool { return true }, superName: func(string, int64) bool { return false },
				api: recoveryAPIFunc(func(callCtx context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
					deadline, ok := callCtx.Deadline()
					require.True(t, ok)
					require.Equal(t, 15*time.Second, deadline.Sub(now), "unchanged per-action request deadline")
					switch req := c.(type) {
					case tbapi.GetChatMemberConfig:
						require.True(t, reliable)
						require.Equal(t, target, req.UserID)
						status := "member"
						if banAcknowledged && mode != "soft ban" {
							status = "kicked"
						}
						return apiOK(tbapi.ChatMember{User: &tbapi.User{ID: target}, Status: status}), nil
					case tbapi.ChatInfoConfig:
						require.True(t, channel)
						require.Equal(t, target, req.ChatID)
						return apiOK(tbapi.ChatFullInfo{Chat: tbapi.Chat{ID: target}}), nil
					case tbapi.BanChatMemberConfig, tbapi.BanChatSenderChatConfig, tbapi.RestrictChatMemberConfig:
						require.True(t, reliable)
						switch v := req.(type) {
						case tbapi.BanChatMemberConfig:
							require.Equal(t, target, v.UserID)
						case tbapi.BanChatSenderChatConfig:
							require.Equal(t, target, v.SenderChatID)
						case tbapi.RestrictChatMemberConfig:
							require.Equal(t, "soft ban", mode)
							require.Equal(t, target, v.UserID)
						}
						banCalls++
						if mode == "ban refused" {
							return nil, &tbapi.Error{Code: 403, Message: "fixture refusal"}
						}
						if mode == "ban retry" && banCalls == 1 {
							return nil, &tbapi.Error{Code: 503, Message: "fixture transient refusal"}
						}
						if mode == "rate limited" && banCalls == 1 {
							return nil, &tbapi.Error{Code: 429, ResponseParameters: tbapi.ResponseParameters{RetryAfter: 2}}
						}
						if mode == "channel uncertain" {
							return nil, &net.OpError{Op: "read", Err: context.DeadlineExceeded}
						}
						banAcknowledged = true
						return apiOK(true), nil
					case tbapi.DeleteMessageConfig:
						require.Equal(t, update.Message.Chat.ID, req.ChatID)
						require.Equal(t, update.Message.MessageID, req.MessageID)
						deleteCalls++
						if mode == "delete refused" {
							return nil, &tbapi.Error{Code: 403, Message: "fixture refusal"}
						}
						if mode == "delete absent" {
							return nil, &tbapi.Error{Code: 400, Message: "message to delete not found"}
						}
						if mode == "delete retry" && deleteCalls == 1 {
							return nil, &tbapi.Error{Code: 429, ResponseParameters: tbapi.ResponseParameters{RetryAfter: 2}}
						}
						return apiOK(true), nil
					case tbapi.MessageConfig:
						if strings.Contains(req.Text, "Преобладает письменность Han") {
							deliveredCards = append(deliveredCards, req.Text)
						}
						return apiOK(tbapi.Message{MessageID: 999}), nil
					case tbapi.EditMessageTextConfig:
						if strings.Contains(req.Text, "Преобладает письменность Han") {
							deliveredCards = append(deliveredCards, req.Text)
						}
						return apiOK(tbapi.Message{MessageID: 999}), nil
					default:
						t.Fatalf("unexpected API %T", c)
						return nil, nil
					}
				})}
			drive := func() {
				w := moderation.Worker{Journal: j, Executor: r, Now: func() time.Time { return now }, Jitter: func() float64 { return 0 }}
				for range 40 {
					worked, e := w.Step(ctx)
					require.NoError(t, e)
					if !worked {
						return
					}
				}
				t.Fatal("worker did not become idle")
			}
			now = time.Now() // Admission sets next_at; do not start the clock before it.
			drive()
			first := mustHanCard(t, j, key)
			states := hanActionStates(first)
			banKind := "ban"
			if mode == "soft ban" {
				banKind = "restrict"
			}
			if mode == "ban refused" {
				require.Equal(t, moderation.Failed, states[banKind])
				require.Equal(t, moderation.Confirmed, states["original_delete"])
			}
			if mode == "delete refused" || mode == "delete absent" {
				require.Equal(t, moderation.Confirmed, states[banKind])
				require.NotEqual(t, moderation.Confirmed, states["original_delete"])
			}
			if mode == "ban retry" || mode == "channel uncertain" {
				require.Equal(t, moderation.Pending, states[banKind])
				require.Equal(t, moderation.Confirmed, states["original_delete"])
			}
			if mode == "delete retry" {
				require.Equal(t, moderation.Confirmed, states[banKind])
				require.Equal(t, moderation.Pending, states["original_delete"])
			}
			if mode == "rate limited" {
				require.Equal(t, moderation.Pending, states[banKind])
				require.Equal(t, moderation.Pending, states["original_delete"])
				require.Zero(t, deleteCalls, "existing Telegram-wide retry_after remains binding")
			}
			// Real storage reopen, repeated update, then scheduled retries.
			require.NoError(t, db.Close())
			db, err = engine.NewSqlite(path, "fixture")
			require.NoError(t, err)
			j, err = moderation.NewJournal(ctx, db, moderation.DefaultLimits())
			require.NoError(t, err)
			l.RecoveryJournal, l.adminHandler.recovery.journal, r.journal = j, j, j
			for range 2 {
				require.NoError(t, l.procEvents(update))
				now = now.Add(3 * time.Second)
				drive()
			}
			now = now.Add(time.Minute)
			drive()
			final := mustHanCard(t, j, key)
			text := compactCardText(final, "fixture")
			require.Contains(t, text, "Удаление:")
			if reliable {
				require.Contains(t, text, map[bool]string{false: "Блокировка:", true: "Ограничение сообщений:"}[mode == "soft ban"])
			} else {
				require.Zero(t, banCalls)
				require.NotContains(t, text, "tg://user?id=")
			}
			if mode == "channel uncertain" {
				require.Equal(t, moderation.Manual, hanActionStates(final)[banKind])
				require.Contains(t, text, "не подтверждён")
				require.NotContains(t, text, "Канал заблокирован")
			}
			if mode == "delete refused" || mode == "delete absent" {
				require.NotContains(t, text, "Сообщение удалено")
			}
			require.NotEmpty(t, deliveredCards, "card delivery uses persisted actual outcomes")
			require.Contains(t, deliveredCards[len(deliveredCards)-1], "Удаление:")
			wantBan, wantDelete := 1, 1
			if mode == "ban retry" || mode == "rate limited" {
				wantBan++
			}
			if mode == "delete retry" {
				wantDelete++
			}
			if reliable {
				require.Equal(t, wantBan, banCalls)
			}
			require.Equal(t, wantDelete, deleteCalls)
			require.Empty(t, d.ApprovedUsers(), "Han must not accumulate automatic trust")
			require.Empty(t, loc.AddSpamCalls(), "Han alone does not add author history or learning")
			for _, a := range final.Actions {
				require.NotEqual(t, "cleanup_delete", a.Kind)
			}
		})
	}
}

func hanActionStates(c moderation.Card) map[string]string {
	states := map[string]string{}
	for _, a := range c.Actions {
		states[a.Kind] = a.State
	}
	return states
}
