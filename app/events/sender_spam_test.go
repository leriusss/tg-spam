package events

import (
	"context"
	"fmt"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/events/mocks"
	"github.com/umputun/tg-spam/app/moderation"
)

func senderFixture(t *testing.T) (*admin, tbapi.Update) {
	t.Helper()
	a, _, _ := cardFixture(t, "detector")
	a.superUsers = SuperUsers{"11"}
	u := tbapi.Update{Message: &tbapi.Message{MessageID: 900, Chat: tbapi.Chat{ID: 123}, Text: "/spam",
		From: &tbapi.User{ID: 136817688, IsBot: true}, SenderChat: &tbapi.Chat{ID: -777},
		ReplyToMessage: &tbapi.Message{MessageID: 702, Chat: tbapi.Chat{ID: 123}, Date: int(time.Now().Unix()),
			Text: "original spam\ntext", From: &tbapi.User{ID: 502, FirstName: "Deleted Account"}}}}
	return a, u
}

func senderCard(t *testing.T, a *admin, u tbapi.Update) moderation.Card {
	t.Helper()
	require.Contains(t, a.senderSpamRequest(u, -777), "принята")
	c, err := a.recovery.journal.CardView(context.Background(), "source:123:702:user:502")
	require.NoError(t, err)
	require.NoError(t, a.recovery.journal.BindCard(context.Background(), c.Receipt, 600))
	return c
}

func TestSenderSpamPendingOriginalAndApproval(t *testing.T) {
	a, u := senderFixture(t)
	c := senderCard(t, a, u)
	require.Equal(t, "pending", c.RequestState)
	require.Equal(t, u.Message.ReplyToMessage.Text, c.Text)
	for _, op := range c.Actions {
		require.Equal(t, moderation.Manual, op.State)
	}
	b := a.bot.(*mocks.BotMock)
	require.Empty(t, b.UpdateSpamCalls())
	require.Empty(t, b.RemoveApprovedUserCalls())
	require.Contains(t, compactCardText(c, "test"), "ожидает подтверждения")
	require.Len(t, cardKeyboard(c, true).InlineKeyboard, 2)
	q := cardQuery(c, "M+", "approve")
	q.Message.Text = "forged display with another target and text"
	require.NoError(t, a.recoveryCardCallback(q))
	require.Len(t, b.UpdateSpamCalls(), 1)
	require.Equal(t, c.Text, b.UpdateSpamCalls()[0].Msg)
	require.Len(t, b.RemoveApprovedUserCalls(), 1)
	require.NoError(t, a.recoveryCardCallback(q))
	q.ID = "another callback"
	require.NoError(t, a.recoveryCardCallback(q))
	require.Len(t, b.UpdateSpamCalls(), 1)
	require.Contains(t, a.senderSpamRequest(u, -777), "уже зарегистрирована")
	after, err := a.recovery.journal.CardView(context.Background(), c.Receipt)
	require.NoError(t, err)
	var pending int
	for _, op := range after.Actions {
		if op.State == moderation.Pending {
			pending++
		}
	}
	require.Equal(t, 2, pending)
}

func TestSenderSpamCallbackAuthority(t *testing.T) {
	for _, tc := range []struct {
		name     string
		id       int64
		username string
		chat     int64
		message  int
		denied   bool
	}{
		{"outsider", 12, "operator", 456, 600, true}, {"username alone", 13, "11", 456, 600, true},
		{"technical", 136817688, "", 456, 600, true}, {"wrong chat", 11, "", 999, 600, true},
		{"wrong card", 11, "", 456, 601, true}, {"authorized numeric", 11, "", 456, 600, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, u := senderFixture(t)
			c := senderCard(t, a, u)
			q := cardQuery(c, "M+", tc.name)
			q.From.ID = tc.id
			q.From.UserName = tc.username
			q.Message.Chat.ID = tc.chat
			q.Message.MessageID = tc.message
			_ = a.recoveryCardCallback(q)
			if tc.denied {
				require.Empty(t, a.bot.(*mocks.BotMock).UpdateSpamCalls())
			} else {
				require.Len(t, a.bot.(*mocks.BotMock).UpdateSpamCalls(), 1)
			}
		})
	}
}

func TestSenderSpamTargetAndModeGates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*admin, *tbapi.Update)
		accept bool
	}{
		{"deleted numeric user", func(*admin, *tbapi.Update) {}, true},
		{"sender chat target", func(_ *admin, u *tbapi.Update) { u.Message.ReplyToMessage.SenderChat = &tbapi.Chat{ID: -555} }, true},
		{"missing original", func(_ *admin, u *tbapi.Update) { u.Message.ReplyToMessage = nil }, false},
		{"hidden identity", func(_ *admin, u *tbapi.Update) { u.Message.ReplyToMessage.From = nil }, false},
		{"technical target", func(_ *admin, u *tbapi.Update) { u.Message.ReplyToMessage.From.ID = 136817688 }, false},
		{"anonymous target", func(_ *admin, u *tbapi.Update) { u.Message.ReplyToMessage.SenderChat = &tbapi.Chat{ID: 123} }, false},
		{"cross chat", func(_ *admin, u *tbapi.Update) { u.Message.ReplyToMessage.Chat.ID = 999 }, false},
		{"super target", func(_ *admin, u *tbapi.Update) { u.Message.ReplyToMessage.From.ID = 11 }, false},
		{"dry", func(a *admin, _ *tbapi.Update) { a.dry = true }, false},
		{"training", func(a *admin, _ *tbapi.Update) { a.trainingMode = true }, false},
		{"explicit trust", func(a *admin, _ *tbapi.Update) { a.isExplicitlyApproved = func(int64) bool { return true } }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, u := senderFixture(t)
			tc.change(a, &u)
			msg := a.senderSpamRequest(u, -777)
			if tc.accept {
				require.Contains(t, msg, "принята")
			} else {
				require.Contains(t, msg, "Не удалось")
			}
			require.Empty(t, a.bot.(*mocks.BotMock).UpdateSpamCalls())
			require.Empty(t, a.bot.(*mocks.BotMock).RemoveApprovedUserCalls())
		})
	}
}

func TestSenderSpamRoutingAndFeedback(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sender  *tbapi.Chat
		from    int64
		handled bool
	}{
		{"production channel", &tbapi.Chat{ID: -777}, 136817688, true},
		{"foreign channel", &tbapi.Chat{ID: -888}, 136817688, true},
		{"anonymous group", &tbapi.Chat{ID: 123}, 1087968824, true},
		{"technical no channel", nil, 1087968824, true},
		{"personal super uses existing handler", nil, 11, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, u := senderFixture(t)
			u.Message.SenderChat = tc.sender
			u.Message.From.ID = tc.from
			api := &mocks.TbAPIMock{SendFunc: func(tbapi.Chattable) (tbapi.Message, error) { return tbapi.Message{}, nil }}
			a.tbAPI = api
			l := &TelegramListener{chatID: 123, adminHandler: a}
			require.Equal(t, tc.handled, l.procSenderSpam(u))
			if tc.handled {
				require.True(t, l.procSenderSpam(u))
				require.Len(t, api.SendCalls(), 1)
				require.Empty(t, a.bot.(*mocks.BotMock).UpdateSpamCalls())
			}
		})
	}
}

func TestSenderSpamCancelAndAlternateCallbacks(t *testing.T) {
	a, u := senderFixture(t)
	c := senderCard(t, a, u)
	require.NoError(t, a.recoveryCardCallback(cardQuery(c, "MR", "reject")))
	require.NoError(t, a.recoveryCardCallback(cardQuery(c, "M+", "approve later")))
	require.Empty(t, a.bot.(*mocks.BotMock).UpdateSpamCalls())
	q := cardQuery(c, "M+", "forged legacy")
	q.Data = fmt.Sprintf("+%d:%d", c.IdentityID, c.MessageID)
	require.Error(t, a.recoveryLegacyCallback(q))
	r := &userReports{recoveryAdmin: a, primChatID: 123, adminChatID: 456}
	q.Data = fmt.Sprintf("R+%d:%d", c.IdentityID, c.MessageID)
	require.Error(t, r.recoveryReportDecision(context.Background(), q, true))
}

func TestSenderSpamLearningFailureKeepsWorkerBlocked(t *testing.T) {
	a, u := senderFixture(t)
	c := senderCard(t, a, u)
	a.bot.(*mocks.BotMock).UpdateSpamFunc = func(string) error { return fmt.Errorf("fixture learning unavailable") }
	require.Error(t, a.recoveryCardCallback(cardQuery(c, "M+", "learning fails")))
	after, err := a.recovery.journal.CardView(context.Background(), c.Receipt)
	require.NoError(t, err)
	require.Equal(t, 1, after.IncompleteDecisions)
	for _, op := range after.Actions {
		if op.State == moderation.Pending {
			mode, e := a.recovery.journal.Authorize(context.Background(), op)
			require.NoError(t, e)
			require.Equal(t, "waiting_for_operator", mode)
		}
	}
	require.NoError(t, a.recoveryCardCallback(cardQuery(c, "M+", "new callback after failure")))
	require.Len(t, a.bot.(*mocks.BotMock).UpdateSpamCalls(), 1)
}

func TestSenderSpamRollbackDisabledRecoveryCallbackIsInert(t *testing.T) {
	a, u := senderFixture(t)
	c := senderCard(t, a, u)
	a.recovery = nil
	a.tbAPI = &mocks.TbAPIMock{RequestFunc: func(c tbapi.Chattable) (*tbapi.APIResponse, error) {
		_, ok := c.(tbapi.CallbackConfig)
		require.True(t, ok, "only callback acknowledgement allowed")
		return apiOK(true), nil
	}}
	require.Error(t, a.InlineCallbackHandler(cardQuery(c, "M+", "stale callback")))
	require.Empty(t, a.bot.(*mocks.BotMock).UpdateSpamCalls())
	require.Empty(t, a.bot.(*mocks.BotMock).RemoveApprovedUserCalls())
}
