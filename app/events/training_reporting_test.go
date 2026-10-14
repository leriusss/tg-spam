package events

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/events/mocks"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/lib/spamcheck"
)

func trainingReportFixture(t *testing.T, channel bool) (*TelegramListener, *mocks.TbAPIMock, *mocks.BotMock,
	*mocks.LocatorMock, tbapi.Update) {
	t.Helper()
	api := &mocks.TbAPIMock{
		SendFunc:    func(tbapi.Chattable) (tbapi.Message, error) { return tbapi.Message{}, nil },
		RequestFunc: func(tbapi.Chattable) (*tbapi.APIResponse, error) { return &tbapi.APIResponse{Ok: true}, nil },
	}
	checks := []spamcheck.Response{{Name: "stopword", Spam: true, Details: "fixture detection evidence"}}
	loc := &mocks.LocatorMock{
		AddMessageFunc:   func(context.Context, string, int64, int64, string, int) error { return nil },
		AddSpamFunc:      func(context.Context, int64, []spamcheck.Response) error { return nil },
		SpamFunc:         func(context.Context, int64) (storage.SpamData, bool) { return storage.SpamData{Checks: checks}, true },
		UserNameByIDFunc: func(context.Context, int64) string { return "source" },
	}
	channelID := int64(0)
	if channel {
		channelID = -1001
	}
	b := &mocks.BotMock{
		OnMessageFunc: func(bot.Message, bool) bot.Response {
			return bot.Response{Send: true, BanInterval: time.Hour, User: bot.User{ID: 501, Username: "source"},
				ChannelID: channelID, ReplyTo: 701, DeleteReplyTo: true, CheckResults: checks}
		},
		UpdateSpamFunc: func(string) error { return nil }, UpdateHamFunc: func(string) error { return nil },
		AddApprovedUserFunc: func(int64, string) error { return nil },
	}
	l := &TelegramListener{TbAPI: api, Locator: loc, Bot: b, TrainingMode: true, NoSpamReply: true,
		chatID: 123, adminChatID: 456, SpamLogger: SpamLoggerFunc(func(*bot.Message, *bot.Response) {})}
	l.adminHandler = &admin{tbAPI: api, bot: b, locator: loc, primChatID: 123, adminChatID: 456, trainingMode: true}
	update := tbapi.Update{Message: &tbapi.Message{Chat: tbapi.Chat{ID: 123}, From: &tbapi.User{ID: 501, UserName: "source"},
		MessageID: 701, Date: int(time.Now().Unix()), Text: "original spam", Quote: &tbapi.TextQuote{Text: "quoted spam"}}}
	if channel {
		update.Message.From = &tbapi.User{ID: 136817688, UserName: "Channel_Bot"}
		update.Message.SenderChat = &tbapi.Chat{ID: channelID, UserName: "sourcechannel"}
	}
	return l, api, b, loc, update
}

// callbackText models rendered report headings, while keeping link fixtures readable.
func callbackText(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "**", ""), `\_`, "_")
}

func TestAutomaticTrainingReportCallbacks(t *testing.T) {
	for _, tc := range []struct {
		name           string
		channel, super bool
	}{
		{name: "user"}, {name: "channel", channel: true}, {name: "superuser legacy training", super: true},
	} {
		for _, decision := range []string{"confirm", "reject"} {
			t.Run(tc.name+"/"+decision, func(t *testing.T) {
				l, api, b, loc, update := trainingReportFixture(t, tc.channel)
				if tc.super {
					l.SuperUsers = SuperUsers{"501"}
					l.adminHandler.superUsers = l.SuperUsers
				}
				require.NoError(t, l.procEvents(update))
				assert.Empty(t, api.RequestCalls(), "training detection must not ban or delete")
				require.Len(t, api.SendCalls(), 1)
				report := api.SendCalls()[0].C.(tbapi.MessageConfig)
				assert.NotContains(t, report.Text, "ban: confirmed")
				assert.NotContains(t, report.Text, "original delete: confirmed")
				assert.NotContains(t, report.Text, "permanently banned")
				assert.Contains(t, report.Text, "**moderation results**")
				assert.NotContains(t, report.Text, "spam detection results")
				markup := report.ReplyMarkup.(tbapi.InlineKeyboardMarkup)
				require.Len(t, markup.InlineKeyboard, 1)
				require.Len(t, markup.InlineKeyboard[0], 2)
				assert.Contains(t, markup.InlineKeyboard[0][0].Text, "Проверить детекцию")
				targetID := int64(501)
				if tc.channel {
					targetID = -1001
				}
				data := fmt.Sprintf("%d:701", targetID)
				assert.Equal(t, "?"+data, *markup.InlineKeyboard[0][0].CallbackData)
				assert.Equal(t, "!"+data, *markup.InlineKeyboard[0][1].CallbackData)
				query := &tbapi.CallbackQuery{ID: "training", Data: "!" + data, From: &tbapi.User{ID: 11, UserName: "admin"},
					Message: &tbapi.Message{MessageID: 801, Chat: tbapi.Chat{ID: 456}, Text: callbackText(report.Text),
						Date: int(time.Now().Unix()), ReplyMarkup: &markup}}

				require.NoError(t, l.adminHandler.InlineCallbackHandler(query))
				require.Len(t, loc.SpamCalls(), 1)
				assert.Equal(t, targetID, loc.SpamCalls()[0].UserID)
				info := api.SendCalls()[1].C.(tbapi.EditMessageTextConfig)
				assert.Contains(t, info.Text, "spam detection results")
				assert.Contains(t, info.Text, "fixture detection evidence")
				query.Message.Text = callbackText(info.Text)
				query.Message.ReplyMarkup = info.ReplyMarkup
				clean, err := l.adminHandler.getCleanMessage(query.Message.Text)
				require.NoError(t, err)
				assert.Equal(t, "original spam quoted spam", clean)
				assert.NotContains(t, clean, "moderation results")
				assert.NotContains(t, clean, "fixture detection evidence")

				query.Data = "?" + data
				require.NoError(t, l.adminHandler.InlineCallbackHandler(query))
				dialog := api.SendCalls()[2].C.(tbapi.EditMessageReplyMarkupConfig)
				require.Len(t, dialog.ReplyMarkup.InlineKeyboard[0], 2)
				assert.Equal(t, data, *dialog.ReplyMarkup.InlineKeyboard[0][0].CallbackData)
				assert.Equal(t, "+"+data, *dialog.ReplyMarkup.InlineKeyboard[0][1].CallbackData)
				assert.Equal(t, "Подтвердить", dialog.ReplyMarkup.InlineKeyboard[0][1].Text)
				assert.Empty(t, api.RequestCalls(), "info and confirmation dialog are not moderation actions")

				query.Data = data
				if decision == "confirm" {
					query.Data = "+" + data
				}
				require.NoError(t, l.adminHandler.InlineCallbackHandler(query))
				last := api.SendCalls()[3].C.(tbapi.EditMessageTextConfig)
				assert.Empty(t, last.ReplyMarkup.InlineKeyboard)
				if decision == "reject" {
					require.Len(t, b.UpdateHamCalls(), 1)
					assert.Equal(t, clean, b.UpdateHamCalls()[0].Msg)
					assert.Empty(t, b.UpdateSpamCalls())
					require.Len(t, b.AddApprovedUserCalls(), 1)
					assert.Equal(t, targetID, b.AddApprovedUserCalls()[0].ID)
					require.Len(t, api.RequestCalls(), 1)
					assert.IsType(t, tbapi.CallbackConfig{}, api.RequestCalls()[0].C, "no ban/delete/unban on training rejection")
					return
				}
				require.Len(t, b.UpdateSpamCalls(), 1)
				assert.Equal(t, clean, b.UpdateSpamCalls()[0].Msg)
				assert.Empty(t, b.UpdateHamCalls())
				assert.Empty(t, b.AddApprovedUserCalls())
				requests := api.RequestCalls()
				if tc.super {
					require.Len(t, requests, 1, "legacy training permits manual deletion, never a superuser ban")
				} else {
					require.Len(t, requests, 2)
					if tc.channel {
						ban := requests[0].C.(tbapi.BanChatSenderChatConfig)
						assert.Equal(t, targetID, ban.SenderChatID)
						assert.Equal(t, int64(123), ban.ChatID)
					} else {
						ban := requests[0].C.(tbapi.BanChatMemberConfig)
						assert.Equal(t, targetID, ban.UserID)
						assert.Equal(t, int64(123), ban.ChatID)
					}
				}
				deleted := requests[len(requests)-1].C.(tbapi.DeleteMessageConfig)
				assert.Equal(t, 701, deleted.MessageID)
				assert.Equal(t, int64(123), deleted.ChatID)
			})
		}
	}
}

func TestAutomaticTrainingControlsRespectExistingGates(t *testing.T) {
	for _, gate := range []string{"dry", "anonymous group post", "superuser outside training", "missing sender ID"} {
		t.Run(gate, func(t *testing.T) {
			l, api, _, _, update := trainingReportFixture(t, false)
			switch gate {
			case "dry":
				l.Dry, l.adminHandler.dry = true, true
			case "anonymous group post":
				update.Message.SenderChat = &tbapi.Chat{ID: 123}
			case "superuser outside training":
				l.TrainingMode, l.adminHandler.trainingMode = false, false
				l.SuperUsers, l.adminHandler.superUsers = SuperUsers{"501"}, SuperUsers{"501"}
			case "missing sender ID":
				update.Message.From.ID = 0
			}
			require.NoError(t, l.procEvents(update))
			assert.Empty(t, api.RequestCalls())
			for _, sent := range api.SendCalls() {
				assert.Nil(t, sent.C.(tbapi.MessageConfig).ReplyMarkup, "gate must not gain moderation controls")
			}
		})
	}
}

func TestAutomaticControlAuthorizationIsIndependentOfAPIResult(t *testing.T) {
	api := &mocks.TbAPIMock{SendFunc: func(tbapi.Chattable) (tbapi.Message, error) { return tbapi.Message{}, nil }}
	a := &admin{tbAPI: api, adminChatID: 456}
	msg := &bot.Message{ID: 701, From: bot.User{ID: 501}, Text: "original"}
	require.NoError(t, a.reportAutomatic("source", msg, actionResult{Status: "confirmed"}, actionResult{}, cleanupResult{}, false))
	assert.Nil(t, api.SendCalls()[0].C.(tbapi.MessageConfig).ReplyMarkup)
}

func TestModerationHeadingDoesNotConsumeOriginalText(t *testing.T) {
	a := &admin{}
	for _, heading := range []string{"moderation results", "**moderation results**"} {
		text := "report\n\nmoderation results\n\n" + heading + "\n\nban: not_attempted"
		clean, err := a.getCleanMessage(text)
		require.NoError(t, err)
		assert.Equal(t, "moderation results", clean)
	}
}
