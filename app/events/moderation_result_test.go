package events

import (
	"context"
	"errors"
	"net"
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

func TestDeleteOriginalAgeAndAPIOutcomes(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 24, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		date     int64
		response *tbapi.APIResponse
		err      error
		status   string
		calls    int
		wantErr  bool
	}{
		{"expired", now.Add(-53 * time.Hour).Unix(), nil, nil, "expired", 0, false},
		{"exact boundary", now.Add(-48 * time.Hour).Unix(), nil, nil, "expired", 0, false},
		{"inside window", now.Add(-48*time.Hour + time.Second).Unix(), &tbapi.APIResponse{Ok: true}, nil, "confirmed", 1, false},
		{"unknown date succeeds", 0, &tbapi.APIResponse{Ok: true}, nil, "confirmed", 1, false},
		{"unknown date refused", 0, nil, &tbapi.Error{Code: 400, Message: "message can't be deleted"}, "failed", 1, true},
		{"lost response", now.Unix(), nil, &net.OpError{Op: "read", Net: "tcp", Err: errors.New("reset")}, "unknown", 1, true},
		{"not found is not confirmed deletion", 0, nil, &tbapi.Error{Code: 400, Message: "message to delete not found"}, "absent", 1, false},
		{"empty API response", 0, &tbapi.APIResponse{}, nil, "unknown", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &mocks.TbAPIMock{RequestFunc: func(c tbapi.Chattable) (*tbapi.APIResponse, error) {
				cfg := c.(tbapi.DeleteMessageConfig)
				assert.Equal(t, int64(123), cfg.ChatID)
				assert.Equal(t, 91693, cfg.MessageID)
				return tc.response, tc.err
			}}
			result, err := deleteOriginal(api, 123, 91693, tc.date, now)
			assert.Equal(t, tc.status, result.Status)
			assert.Len(t, api.RequestCalls(), tc.calls)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tc.date == 0 && tc.err != nil {
				assert.NotContains(t, result.Reason, "window exceeded")
			}
		})
	}
}

func TestBanWithoutAcknowledgementIsUnknown(t *testing.T) {
	api := &mocks.TbAPIMock{RequestFunc: func(tbapi.Chattable) (*tbapi.APIResponse, error) { return nil, nil }}
	result, err := banResult(banRequest{tbAPI: api, chatID: 123, userID: 501, duration: time.Hour})
	require.Error(t, err)
	assert.Equal(t, "unknown", result.Status)
}

func TestAdminReportingIndependentActionsAndNotification(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		banErr, deleteErr, sendErr error
		banStatus, deleteStatus    string
	}{
		{"ban succeeds delete fails", nil, &tbapi.Error{Code: 403, Message: "not enough rights"}, nil, "confirmed", "failed"},
		{"ban fails delete succeeds", &tbapi.Error{Code: 403, Message: "not enough rights"}, nil, nil, "failed", "confirmed"},
		{"notification fails after success", nil, nil, errors.New("send failed"), "confirmed", "confirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAdminForwardReplay(t, storage.MsgMeta{ChatID: 123, UserID: 501, MsgID: 701}, true)
			r.api.RequestFunc = func(c tbapi.Chattable) (*tbapi.APIResponse, error) {
				if _, ok := c.(tbapi.DeleteMessageConfig); ok {
					return &tbapi.APIResponse{Ok: true}, tc.deleteErr
				}
				return &tbapi.APIResponse{Ok: true}, tc.banErr
			}
			r.api.SendFunc = func(c tbapi.Chattable) (tbapi.Message, error) {
				assert.Len(t, r.api.RequestCalls(), 2, "no success feedback before both API actions finish")
				text := c.(tbapi.MessageConfig).Text
				assert.Contains(t, text, "ban: "+tc.banStatus)
				assert.Contains(t, text, "original delete: "+tc.deleteStatus)
				assert.NotContains(t, text, "the user banned and message deleted")
				return tbapi.Message{}, tc.sendErr
			}
			err := r.handler.legacyForward(tbapi.Update{Message: replayAdminMessage(userOrigin(501, "source"), "text")})
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "spam notification failed")
			if tc.sendErr != nil {
				assert.Contains(t, err.Error(), "notification_failed")
				assert.NotContains(t, err.Error(), "moderation_failed")
			}
		})
	}
}

type datedTestLocator struct {
	*mocks.LocatorMock
	date int64
}

func (l *datedTestLocator) RecordMessageDate(context.Context, int64, int, int64) error { return nil }
func (l *datedTestLocator) MessageDates(context.Context, int64, int) (storage.MessageDates, error) {
	return storage.MessageDates{MessageDate: l.date}, nil
}

func TestAdminExpiredOriginalStillBansAndDryModeDoesNotClaimSuccess(t *testing.T) {
	for _, dry := range []bool{false, true} {
		r := newAdminForwardReplay(t, storage.MsgMeta{ChatID: 123, UserID: 501, MsgID: 701}, true)
		r.handler.locator = &datedTestLocator{LocatorMock: r.locator, date: time.Now().Add(-53 * time.Hour).Unix()}
		r.handler.dry = dry
		require.NoError(t, r.handler.legacyForward(tbapi.Update{Message: replayAdminMessage(userOrigin(501, "source"), "text")}))
		text := r.api.SendCalls()[0].C.(tbapi.MessageConfig).Text
		if dry {
			assert.Empty(t, r.api.RequestCalls())
			assert.Contains(t, text, "ban: not_attempted")
			assert.NotContains(t, text, "confirmed")
		} else {
			require.Len(t, r.api.RequestCalls(), 1)
			assert.IsType(t, tbapi.BanChatMemberConfig{}, r.api.RequestCalls()[0].C)
			assert.Contains(t, text, "ban: confirmed")
			assert.Contains(t, text, "manual deletion required")
		}
	}
}

func TestCleanupPartialFailureIsReturned(t *testing.T) {
	api := &mocks.TbAPIMock{RequestFunc: func(c tbapi.Chattable) (*tbapi.APIResponse, error) {
		if c.(tbapi.DeleteMessageConfig).MessageID == 2 {
			return nil, &tbapi.Error{Code: 403, Message: "not enough rights"}
		}
		return &tbapi.APIResponse{Ok: true}, nil
	}}
	loc := &mocks.LocatorMock{GetUserMessageIDsFunc: func(context.Context, int64, int) ([]int, error) { return []int{1, 2, 3}, nil }}
	a := &admin{tbAPI: api, locator: loc, primChatID: 123, aggressiveCleanupLimit: 50}
	r, err := a.deleteUserMessages(501)
	require.Error(t, err)
	assert.Equal(t, 3, r.Selected)
	assert.Equal(t, 2, r.Deleted)
	assert.Equal(t, 1, r.Failed)
	require.Len(t, r.Items, 3)
	assert.Contains(t, r.Items[1].Reason, "telegram_403")
}

func TestFallbackReportsAfterBanAndKeepsOwnChatPolicy(t *testing.T) {
	r := newAdminForwardReplay(t, storage.MsgMeta{}, false)
	r.api.SendFunc = func(c tbapi.Chattable) (tbapi.Message, error) {
		require.Len(t, r.api.RequestCalls(), 1)
		cfg := r.api.RequestCalls()[0].C.(tbapi.BanChatMemberConfig)
		assert.Equal(t, replayPrimaryChatID, cfg.ChatID)
		assert.Contains(t, c.(tbapi.MessageConfig).Text, "original message needs manual deletion")
		return tbapi.Message{}, nil
	}
	require.NoError(t, r.handler.legacyForward(tbapi.Update{Message: replayAdminMessage(userOrigin(501, "source"), "text")}))
}

func TestAutomaticReportingKeepsBanOnDeleteFailure(t *testing.T) {
	var report string
	api := &mocks.TbAPIMock{RequestFunc: func(c tbapi.Chattable) (*tbapi.APIResponse, error) {
		if _, ok := c.(tbapi.DeleteMessageConfig); ok {
			return nil, &tbapi.Error{Code: 403, Message: "not enough rights"}
		}
		return &tbapi.APIResponse{Ok: true}, nil
	}, SendFunc: func(c tbapi.Chattable) (tbapi.Message, error) {
		report = c.(tbapi.MessageConfig).Text
		return tbapi.Message{}, nil
	}}
	loc := &mocks.LocatorMock{AddMessageFunc: func(context.Context, string, int64, int64, string, int) error { return nil },
		AddSpamFunc: func(context.Context, int64, []spamcheck.Response) error { return nil }}
	l := &TelegramListener{TbAPI: api, Locator: loc, chatID: 123, adminChatID: 456, NoSpamReply: true,
		SpamLogger: SpamLoggerFunc(func(*bot.Message, *bot.Response) {}),
		Bot: &mocks.BotMock{OnMessageFunc: func(bot.Message, bool) bot.Response {
			return bot.Response{
				Send: true, BanInterval: time.Hour, User: bot.User{ID: 501}, ReplyTo: 701, DeleteReplyTo: true}
		}}}
	l.adminHandler = &admin{tbAPI: api, adminChatID: 456}
	err := l.procEvents(tbapi.Update{Message: &tbapi.Message{Chat: tbapi.Chat{ID: 123}, From: &tbapi.User{ID: 501},
		MessageID: 701, Date: int(time.Now().Unix()), Text: "spam"}})
	require.Error(t, err)
	assert.Contains(t, report, "ban: confirmed")
	assert.Contains(t, report, "original delete: failed")
}

func TestAutomaticReportPreservesCallbackMessageAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		channel bot.SenderChat
	}{
		{"user", bot.SenderChat{}},
		{"named channel", bot.SenderChat{ID: -1001, UserName: "channel"}},
		{"unnamed channel", bot.SenderChat{ID: -1001}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var text string
			api := &mocks.TbAPIMock{SendFunc: func(c tbapi.Chattable) (tbapi.Message, error) {
				text = c.(tbapi.MessageConfig).Text
				return tbapi.Message{}, nil
			}}
			a := &admin{tbAPI: api, adminChatID: 456}
			msg := &bot.Message{ID: 701, From: bot.User{ID: 501, Username: "source"}, SenderChat: tc.channel, Text: "original spam", Quote: "quoted spam"}
			require.NoError(t, a.reportAutomatic("source", msg, actionResult{Status: "confirmed"},
				actionResult{Status: "failed"}, cleanupResult{}, true))
			clean, err := a.getCleanMessage(text)
			require.NoError(t, err)
			assert.Equal(t, "original spam quoted spam", clean, "unban learning must not include reporting statuses")
			name, err := a.extractUsername(text)
			require.NoError(t, err)
			assert.Equal(t, "source", name)
			markup := api.SendCalls()[0].C.(tbapi.MessageConfig).ReplyMarkup.(tbapi.InlineKeyboardMarkup)
			expected := "?501:701"
			if tc.channel.ID != 0 {
				expected = "?-1001:701"
			}
			assert.Equal(t, expected, *markup.InlineKeyboard[0][0].CallbackData)
		})
	}
}

func TestCleanupReportsAllFailuresAndRemainingCandidates(t *testing.T) {
	api := &mocks.TbAPIMock{RequestFunc: func(tbapi.Chattable) (*tbapi.APIResponse, error) {
		return nil, &tbapi.Error{Code: 403, Message: "not enough rights"}
	}}
	loc := &mocks.LocatorMock{GetUserMessageIDsFunc: func(context.Context, int64, int) ([]int, error) { return []int{1, 2, 3, 4, 5, 6, 7}, nil }}
	a := &admin{tbAPI: api, locator: loc, primChatID: 123, aggressiveCleanupLimit: 50}
	r, err := a.deleteUserMessages(501)
	require.Error(t, err)
	assert.Equal(t, 7, r.Selected)
	assert.Equal(t, 0, r.Deleted)
	assert.Equal(t, 5, r.Failed)
	assert.Equal(t, 2, r.Remaining)
	assert.Len(t, r.Items, 5)
}
