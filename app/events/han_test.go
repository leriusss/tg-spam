package events

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/lib/approved"
	"github.com/umputun/tg-spam/lib/tgspam"
)

func TestHanPipeline(t *testing.T) {
	for _, mode := range []string{"auto", "manual", "superuser", "sender chat", "sender superuser", "caption", "video caption", "blockquote", "quote", "reply", "forward", "dry", "training", "no journal", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			l, api, _, loc, update := trainingReportFixture(t, mode == "sender chat" || mode == "sender superuser")
			d := tgspam.NewDetector(tgspam.Config{FirstMessageOnly: true, MaxAllowedEmoji: -1, MinMsgLen: 1})
			filter := bot.NewSpamFilter(d, bot.SpamConfig{Han: bot.HanConfig{Enabled: mode != "disabled", Ratio: 0.7, MinCount: 20}})
			l.Bot, l.adminHandler.bot = filter, filter
			l.TrainingMode, l.adminHandler.trainingMode = false, false
			l.RecoveryJournal = recoveryJournal(t)
			l.adminHandler.recovery = &recovery{journal: l.RecoveryJournal}
			update.Message.Text, update.Message.Quote = "обычный текст", nil
			require.NoError(t, l.procEvents(update))
			require.NoError(t, l.procEvents(update))
			require.Equal(t, approved.SourceAuto, d.ApprovedUsers()[0].Source)
			update.Message.MessageID++
			update.Message.Text = strings.Repeat("你好世界", 7) + " привет"
			want := mode == "auto" || mode == "sender chat" || mode == "caption" || mode == "video caption"
			switch mode {
			case "manual":
				require.NoError(t, filter.AddApprovedUser(501, "source"))
			case "superuser":
				l.SuperUsers = SuperUsers{"501"}
			case "sender superuser":
				l.SuperUsers = SuperUsers{"-1001"}
			case "caption":
				update.Message.Caption, update.Message.Text = update.Message.Text, ""
				update.Message.Photo = []tbapi.PhotoSize{{FileID: "fixture"}}
			case "video caption":
				update.Message.Caption, update.Message.Text = update.Message.Text, ""
				update.Message.Video = &tbapi.Video{FileID: "fixture"}
			case "blockquote":
				update.Message.Text = "😀" + strings.Repeat("中", 24) + " привет"
				update.Message.Entities = []tbapi.MessageEntity{{Type: "blockquote", Offset: 2, Length: 24}}
			case "quote":
				update.Message.Quote = &tbapi.TextQuote{Text: update.Message.Text}
				update.Message.Text = "Удалите рекламу"
			case "reply":
				update.Message.ReplyToMessage = &tbapi.Message{Text: update.Message.Text}
				update.Message.Text = "Удалите рекламу"
			case "forward":
				update.Message.ForwardOrigin = &tbapi.MessageOrigin{Type: "hidden_user"}
			case "dry":
				l.Dry = true
			case "training":
				l.TrainingMode = true
			case "no journal":
				l.RecoveryJournal = nil
			}
			j := l.adminHandler.recovery.journal
			require.NoError(t, j.EnableHanBans(context.Background(), time.Unix(1, 0)))
			for range 2 {
				err := l.procEvents(update)
				if mode == "no journal" {
					require.ErrorContains(t, err, "requires durable")
				} else {
					require.NoError(t, err)
				}
			}
			rows, err := j.Snapshot(context.Background())
			require.NoError(t, err)
			if !want {
				require.Empty(t, rows)
			} else {
				require.Len(t, rows, 4, "ban and deletion plus card and notice")
				for _, a := range rows {
					require.Contains(t, []string{"ban", "original_delete", "card", "notice"}, a.Kind)
				}
				id, kind := int64(501), "user"
				if mode == "sender chat" {
					id, kind = -1001, "sender_chat"
				}
				card, err := j.CardView(context.Background(), fmt.Sprintf("source:123:702:%s:%d", kind, id))
				require.NoError(t, err)
				require.Contains(t, card.Reason, "28 из 34 букв")
				require.Contains(t, compactCardText(card, "fixture"), "Удаление и блокировка учитываются отдельно")
				require.Contains(t, cardDetails(card), "Сработало правило Han")
				require.NotContains(t, compactCardText(card, "fixture"), "Сообщение удалено")
				require.Equal(t, strings.Repeat("你好世界", 7)+" привет", card.Text)
			}
			require.Empty(t, api.RequestCalls())
			require.Empty(t, api.SendCalls())
			require.Empty(t, loc.AddSpamCalls())
		})
	}
}

func TestHanConcurrentRedelivery(t *testing.T) {
	l, _, b, _, update := trainingReportFixture(t, false)
	l.TrainingMode = false
	l.RecoveryJournal = recoveryJournal(t)
	l.adminHandler.recovery = &recovery{journal: l.RecoveryJournal}

	msg := transform(update.Message)
	resp := bot.Response{HanDeleteOnly: true, Send: true, DeleteReplyTo: true, ReplyTo: msg.ID, BanInterval: time.Hour}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Go(func() { _, err := l.enqueueAutomatic(update, msg, resp); errs <- err })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	rows, err := l.RecoveryJournal.Snapshot(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 3)
	for _, a := range rows {
		require.Contains(t, []string{"original_delete", "card", "notice"}, a.Kind)
	}
	require.Empty(t, b.UpdateSpamCalls())
	require.Empty(t, b.UpdateHamCalls())
	require.Empty(t, b.RemoveApprovedUserCalls())
}
