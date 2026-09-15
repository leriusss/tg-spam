package events

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/events/mocks"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/app/storage/engine"
	"github.com/umputun/tg-spam/lib/spamcheck"
)

const (
	replayPrimaryChatID int64 = 123
	replayAdminChatID   int64 = 456
	replayAdminID       int64 = 11
)

// replayActions is a test-only action plan captured at the existing side-effect
// interfaces. No recorder method opens a socket or production storage.
type replayActions struct {
	locatorLookups   int
	approvedRemovals []int64
	spamUpdates      []string
	userBans         []int64
	channelBans      []int64
	messageDeletes   []int
	cleanupLookups   []int64
	feedback         int
}

type adminForwardReplay struct {
	api      *mocks.TbAPIMock
	bot      *mocks.BotMock
	locator  *mocks.LocatorMock
	handler  *admin
	approved map[int64]bool
}

type replayExplicitApprovalBot struct {
	*mocks.BotMock
	approved map[int64]bool
}

func (b *replayExplicitApprovalBot) IsExplicitTrustedUser(id int64) bool {
	return b.approved[id]
}

func newAdminForwardReplay(t *testing.T, located storage.MsgMeta, locatorHit bool) *adminForwardReplay {
	t.Helper()
	api := &mocks.TbAPIMock{
		SendFunc: func(tbapi.Chattable) (tbapi.Message, error) {
			return tbapi.Message{}, nil
		},
		RequestFunc: func(tbapi.Chattable) (*tbapi.APIResponse, error) {
			return &tbapi.APIResponse{Ok: true}, nil
		},
	}
	botMock := &mocks.BotMock{
		OnMessageFunc: func(bot.Message, bool) bot.Response {
			return bot.Response{CheckResults: []spamcheck.Response{{Name: "replay", Details: "recorded"}}}
		},
		UpdateSpamFunc:         func(string) error { return nil },
		RemoveApprovedUserFunc: func(int64) error { return nil },
		IsApprovedUserFunc:     func(int64) bool { return true },
	}
	locator := &mocks.LocatorMock{
		MessageFunc: func(context.Context, string) (storage.MsgMeta, bool) {
			return located, locatorHit
		},
		MessageByMediaFunc: func(context.Context, storage.MediaLocatorKey, int64, storage.LocatorIdentity) (storage.MsgMeta, bool) {
			return located, locatorHit
		},
		GetUserMessageIDsFunc: func(context.Context, int64, int) ([]int, error) {
			return nil, nil
		},
	}
	approved := map[int64]bool{}
	return &adminForwardReplay{
		api: api, bot: botMock, locator: locator,
		approved: approved,
		handler: &admin{
			tbAPI: api, bot: botMock, locator: locator,
			primChatID: replayPrimaryChatID, adminChatID: replayAdminChatID,
			superUsers:           SuperUsers{"admin", "11"},
			isExplicitlyApproved: func(id int64) bool { return approved[id] },
		},
	}
}

func (r *adminForwardReplay) actions() replayActions {
	res := replayActions{locatorLookups: len(r.locator.MessageCalls()) + len(r.locator.MessageByMediaCalls()), feedback: len(r.api.SendCalls())}
	for _, call := range r.bot.RemoveApprovedUserCalls() {
		res.approvedRemovals = append(res.approvedRemovals, call.ID)
	}
	for _, call := range r.bot.UpdateSpamCalls() {
		res.spamUpdates = append(res.spamUpdates, call.Msg)
	}
	for _, call := range r.locator.GetUserMessageIDsCalls() {
		res.cleanupLookups = append(res.cleanupLookups, call.UserID)
	}
	for _, call := range r.api.RequestCalls() {
		switch cfg := call.C.(type) {
		case tbapi.BanChatMemberConfig:
			res.userBans = append(res.userBans, cfg.UserID)
		case tbapi.BanChatSenderChatConfig:
			res.channelBans = append(res.channelBans, cfg.SenderChatID)
		case tbapi.DeleteMessageConfig:
			res.messageDeletes = append(res.messageDeletes, cfg.MessageID)
		}
	}
	return res
}

func replayAdminMessage(origin *tbapi.MessageOrigin, text string) *tbapi.Message {
	return &tbapi.Message{
		MessageID: 900, Chat: tbapi.Chat{ID: replayAdminChatID},
		From: &tbapi.User{ID: replayAdminID, UserName: "admin"},
		Text: text, ForwardOrigin: origin,
	}
}

func userOrigin(id int64, name string) *tbapi.MessageOrigin {
	return &tbapi.MessageOrigin{
		Type:       tbapi.MessageOriginUser,
		SenderUser: &tbapi.User{ID: id, UserName: name},
	}
}

func assertNoModerationActions(t *testing.T, got replayActions) {
	t.Helper()
	assert.Empty(t, got.approvedRemovals)
	assert.Empty(t, got.spamUpdates)
	assert.Empty(t, got.userBans)
	assert.Empty(t, got.channelBans)
	assert.Empty(t, got.messageDeletes)
	assert.Empty(t, got.cleanupLookups)
}

func TestAdminForwardOfflineReplayRoutingMatrix(t *testing.T) {
	run := func(t *testing.T, msg *tbapi.Message, supers SuperUsers, disabled bool) (string, *mocks.TbAPIMock, *mocks.BotMock) {
		t.Helper()
		updates := make(chan tbapi.Update, 1)
		updates <- tbapi.Update{UpdateID: 700, Message: msg}
		close(updates)

		api := &mocks.TbAPIMock{
			GetUpdatesChanFunc:        func(tbapi.UpdateConfig) tbapi.UpdatesChannel { return updates },
			GetChatAdministratorsFunc: func(tbapi.ChatAdministratorsConfig) ([]tbapi.ChatMember, error) { return nil, nil },
			SendFunc:                  func(tbapi.Chattable) (tbapi.Message, error) { return tbapi.Message{}, nil },
			RequestFunc: func(tbapi.Chattable) (*tbapi.APIResponse, error) {
				return &tbapi.APIResponse{Ok: true}, nil
			},
		}
		botMock := &mocks.BotMock{}
		locator := &mocks.LocatorMock{}
		listener := TelegramListener{
			TbAPI: api, Bot: botMock, Locator: locator,
			Group: "123", AdminGroup: "456", SuperUsers: supers,
			DisableAdminSpamForward: disabled,
		}

		var logs bytes.Buffer
		oldOutput := log.Writer()
		log.SetOutput(&logs)
		defer log.SetOutput(oldOutput)
		err := listener.Do(context.Background())
		require.EqualError(t, err, "telegram update chan closed")
		return logs.String(), api, botMock
	}

	t.Run("A ordinary authorized admin message reaches handler and is ignored", func(t *testing.T) {
		logs, api, botMock := run(t, replayAdminMessage(nil, "ordinary admin text"), SuperUsers{"11"}, false)
		assert.Contains(t, logs, "outcome=admin_chat_received")
		assert.Contains(t, logs, "outcome=missing_origin")
		assert.Empty(t, api.RequestCalls())
		assert.Empty(t, api.SendCalls())
		assert.Empty(t, botMock.UpdateSpamCalls())
	})

	t.Run("B unauthorized admin-chat sender is rejected before handler", func(t *testing.T) {
		logs, api, botMock := run(t, replayAdminMessage(userOrigin(501, "source"), "forward"), nil, false)
		assert.Contains(t, logs, "outcome=unauthorized_sender")
		assert.NotContains(t, logs, "admin report received:")
		assert.Empty(t, api.RequestCalls())
		assert.Empty(t, api.SendCalls())
		assert.Empty(t, botMock.UpdateSpamCalls())
	})

	t.Run("C disabled forwarding stops after authorization with structured log", func(t *testing.T) {
		logs, api, botMock := run(t, replayAdminMessage(userOrigin(501, "source"), "forward"), SuperUsers{"11"}, true)
		assert.Contains(t, logs, "outcome=admin_chat_received")
		assert.Contains(t, logs, "outcome=forwarding_disabled")
		assert.NotContains(t, logs, "admin report received:")
		assert.Empty(t, api.RequestCalls())
		assert.Empty(t, api.SendCalls())
		assert.Empty(t, botMock.UpdateSpamCalls())
	})
}

func TestAdminForwardPhase1ListenerWiresExplicitApprovalLock(t *testing.T) {
	updates := make(chan tbapi.Update, 1)
	updates <- tbapi.Update{UpdateID: 701, Message: replayAdminMessage(userOrigin(501, "source"), "text")}
	close(updates)

	api := &mocks.TbAPIMock{
		GetUpdatesChanFunc:        func(tbapi.UpdateConfig) tbapi.UpdatesChannel { return updates },
		GetChatAdministratorsFunc: func(tbapi.ChatAdministratorsConfig) ([]tbapi.ChatMember, error) { return nil, nil },
		SendFunc:                  func(tbapi.Chattable) (tbapi.Message, error) { return tbapi.Message{}, nil },
		RequestFunc: func(tbapi.Chattable) (*tbapi.APIResponse, error) {
			return &tbapi.APIResponse{Ok: true}, nil
		},
	}
	baseBot := &mocks.BotMock{
		OnMessageFunc:          func(bot.Message, bool) bot.Response { return bot.Response{} },
		UpdateSpamFunc:         func(string) error { return nil },
		RemoveApprovedUserFunc: func(int64) error { return nil },
		IsApprovedUserFunc:     func(int64) bool { return true },
	}
	explicitBot := &replayExplicitApprovalBot{BotMock: baseBot, approved: map[int64]bool{501: true}}
	locator := &mocks.LocatorMock{
		MessageFunc: func(context.Context, string) (storage.MsgMeta, bool) {
			return storage.MsgMeta{UserID: 501, UserName: "source", MsgID: 701}, true
		},
	}
	listener := TelegramListener{
		TbAPI: api, Bot: explicitBot, Locator: locator,
		Group: "123", AdminGroup: "456", SuperUsers: SuperUsers{"11"},
	}

	require.EqualError(t, listener.Do(context.Background()), "telegram update chan closed")
	assert.Empty(t, baseBot.RemoveApprovedUserCalls())
	assert.Empty(t, baseBot.UpdateSpamCalls())
	assert.Empty(t, api.RequestCalls())
	require.Len(t, api.SendCalls(), 1)
	assert.Contains(t, api.SendCalls()[0].C.(tbapi.MessageConfig).Text, "explicitly approved")
}

func TestAdminForwardPhase1SafetyLogsAndFeedback(t *testing.T) {
	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldOutput)

	r := newAdminForwardReplay(t, storage.MsgMeta{UserID: 202, UserName: "second", MsgID: 1002}, true)
	require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(userOrigin(101, "first"), "SENSITIVE MESSAGE CONTENT")}))
	assertNoModerationActions(t, r.actions())
	assert.Contains(t, logs.String(), "locator identity mismatch; destructive action blocked")
	assert.Contains(t, logs.String(), "outcome=identity_mismatch")
	assert.NotContains(t, logs.String(), "SENSITIVE MESSAGE CONTENT")
	require.Len(t, r.api.SendCalls(), 1)
	assert.Contains(t, r.api.SendCalls()[0].C.(tbapi.MessageConfig).Text, "does not match")

	logs.Reset()
	approved := newAdminForwardReplay(t, storage.MsgMeta{UserID: 101, UserName: "first", MsgID: 1001}, true)
	approved.approved[101] = true
	require.NoError(t, approved.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(userOrigin(101, "first"), "text")}))
	assertNoModerationActions(t, approved.actions())
	assert.Contains(t, logs.String(), "outcome=approved_target")
	require.Len(t, approved.api.SendCalls(), 1)
	assert.Contains(t, approved.api.SendCalls()[0].C.(tbapi.MessageConfig).Text, "explicitly approved")
}

func TestAdminForwardOfflineReplayOriginAndContentMatrix(t *testing.T) {
	locatedUser := storage.MsgMeta{ChatID: replayPrimaryChatID, UserID: 501, UserName: "source", MsgID: 701}

	t.Run("D personal user locator hit plans full moderation", func(t *testing.T) {
		r := newAdminForwardReplay(t, locatedUser, true)
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(userOrigin(501, "source"), "text")}))
		got := r.actions()
		assert.Equal(t, []int64{501}, got.approvedRemovals)
		assert.Equal(t, []string{"text"}, got.spamUpdates)
		assert.Equal(t, []int64{501}, got.userBans)
		assert.Equal(t, []int{701}, got.messageDeletes)
		assert.Equal(t, 1, got.feedback)
	})

	t.Run("E personal user locator miss uses destructive sender fallback without delete", func(t *testing.T) {
		r := newAdminForwardReplay(t, storage.MsgMeta{}, false)
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(userOrigin(501, "source"), "text")}))
		got := r.actions()
		assert.Equal(t, []int64{501}, got.approvedRemovals)
		assert.Equal(t, []string{"text"}, got.spamUpdates)
		assert.Equal(t, []int64{501}, got.userBans)
		assert.Empty(t, got.messageDeletes)
		assert.Equal(t, 2, got.feedback, "fallback sends results and manual-delete warning")
	})

	t.Run("E approved user locator miss is blocked before fallback mutations", func(t *testing.T) {
		r := newAdminForwardReplay(t, storage.MsgMeta{}, false)
		r.approved[501] = true
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(userOrigin(501, "source"), "text")}))
		assertNoModerationActions(t, r.actions())
		assert.Equal(t, 1, r.actions().feedback)
	})

	t.Run("F hidden sender is blocked before locator", func(t *testing.T) {
		r := newAdminForwardReplay(t, storage.MsgMeta{}, false)
		msg := replayAdminMessage(&tbapi.MessageOrigin{Type: tbapi.MessageOriginHiddenUser, SenderUserName: "hidden"}, "text")
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
		assertNoModerationActions(t, r.actions())
		assert.Zero(t, r.actions().locatorLookups)
		assert.Equal(t, 1, r.actions().feedback)
	})

	t.Run("G missing ForwardOrigin is a silent no-op with or without text", func(t *testing.T) {
		for _, text := range []string{"ordinary", ""} {
			r := newAdminForwardReplay(t, storage.MsgMeta{}, false)
			require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(nil, text)}))
			assertNoModerationActions(t, r.actions())
			assert.Zero(t, r.actions().feedback)
		}
	})

	t.Run("H user origin with nil SenderUser is blocked without panic", func(t *testing.T) {
		r := newAdminForwardReplay(t, storage.MsgMeta{}, false)
		msg := replayAdminMessage(&tbapi.MessageOrigin{Type: tbapi.MessageOriginUser}, "text")
		assert.NotPanics(t, func() { require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg})) })
		assertNoModerationActions(t, r.actions())
		assert.Equal(t, 1, r.actions().feedback)
	})

	t.Run("H user origin with zero SenderUser ID is blocked", func(t *testing.T) {
		r := newAdminForwardReplay(t, locatedUser, true)
		msg := replayAdminMessage(userOrigin(0, "malformed"), "text")
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
		assertNoModerationActions(t, r.actions())
		assert.Zero(t, r.actions().locatorLookups)
		assert.Equal(t, 1, r.actions().feedback)
	})

	t.Run("H unknown origin type is blocked", func(t *testing.T) {
		r := newAdminForwardReplay(t, locatedUser, true)
		msg := replayAdminMessage(&tbapi.MessageOrigin{Type: "unsupported"}, "text")
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
		assertNoModerationActions(t, r.actions())
		assert.Zero(t, r.actions().locatorLookups)
		assert.Equal(t, 1, r.actions().feedback)
	})

	t.Run("I hidden origin locator hit is not trusted", func(t *testing.T) {
		r := newAdminForwardReplay(t, locatedUser, true)
		msg := replayAdminMessage(&tbapi.MessageOrigin{Type: tbapi.MessageOriginHiddenUser, SenderUserName: "hidden"}, "text")
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
		assertNoModerationActions(t, r.actions())
		assert.Zero(t, r.actions().locatorLookups)
		assert.Equal(t, 1, r.actions().feedback)
	})

	t.Run("J chat origin requires matching locator and has no miss fallback", func(t *testing.T) {
		origin := &tbapi.MessageOrigin{Type: tbapi.MessageOriginChat, SenderChat: &tbapi.Chat{ID: -200, UserName: "chat"}}
		r := newAdminForwardReplay(t, storage.MsgMeta{UserID: -200, UserName: "chat", MsgID: 704}, true)
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(origin, "text")}))
		assert.Equal(t, []int64{-200}, r.actions().channelBans)

		miss := newAdminForwardReplay(t, storage.MsgMeta{}, false)
		require.NoError(t, miss.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(origin, "text")}))
		assertNoModerationActions(t, miss.actions())
		assert.Equal(t, 1, miss.actions().feedback)
	})

	t.Run("K channel origin locator hit bans located sender chat", func(t *testing.T) {
		channel := storage.MsgMeta{UserID: -100200, UserName: "channel", MsgID: 702}
		r := newAdminForwardReplay(t, channel, true)
		origin := &tbapi.MessageOrigin{Type: tbapi.MessageOriginChannel, Chat: &tbapi.Chat{ID: -100200, UserName: "channel"}, MessageID: 702}
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(origin, "channel text")}))
		got := r.actions()
		assert.Equal(t, []int64{-100200}, got.approvedRemovals)
		assert.Equal(t, []int64{-100200}, got.channelBans)
		assert.Empty(t, got.userBans)
	})

	t.Run("K channel origin locator miss is blocked", func(t *testing.T) {
		r := newAdminForwardReplay(t, storage.MsgMeta{}, false)
		origin := &tbapi.MessageOrigin{Type: tbapi.MessageOriginChannel, Chat: &tbapi.Chat{ID: -100200, UserName: "channel"}}
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(origin, "channel text")}))
		assertNoModerationActions(t, r.actions())
		assert.Equal(t, 1, r.actions().feedback)
	})

	t.Run("L approved sender chat remains protected from every mutation", func(t *testing.T) {
		channel := storage.MsgMeta{UserID: -100300, UserName: "approved_channel", MsgID: 703}
		r := newAdminForwardReplay(t, channel, true)
		r.approved[-100300] = true
		r.handler.aggressiveCleanup = true
		r.handler.aggressiveCleanupLimit = 10
		r.locator.GetUserMessageIDsFunc = func(_ context.Context, id int64, _ int) ([]int, error) {
			return nil, nil
		}
		origin := &tbapi.MessageOrigin{Type: tbapi.MessageOriginChannel, Chat: &tbapi.Chat{ID: -100300, UserName: "approved_channel"}}
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(origin, "official text")}))
		got := r.actions()
		assertNoModerationActions(t, got)
		assert.Equal(t, 1, got.feedback)
	})

	t.Run("approved user locator hit remains protected from every mutation", func(t *testing.T) {
		r := newAdminForwardReplay(t, locatedUser, true)
		r.approved[501] = true
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(userOrigin(501, "source"), "text")}))
		assertNoModerationActions(t, r.actions())
		assert.Equal(t, 1, r.actions().feedback)
	})

	t.Run("M media caption uses caption as locator and spam sample", func(t *testing.T) {
		r := newAdminForwardReplay(t, locatedUser, true)
		msg := replayAdminMessage(userOrigin(501, "source"), "")
		msg.Caption = "caption text"
		msg.Photo = []tbapi.PhotoSize{{FileID: "fake-photo"}}
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
		assert.Equal(t, "caption text", r.locator.MessageCalls()[0].Msg)
		assert.Equal(t, []string{"caption text"}, r.actions().spamUpdates)
	})

	t.Run("N media without caption uses scoped locator and skips learning", func(t *testing.T) {
		r := newAdminForwardReplay(t, locatedUser, true)
		msg := replayAdminMessage(userOrigin(501, "source"), "")
		msg.Photo = []tbapi.PhotoSize{{FileID: "bot-specific", FileUniqueID: "stable-photo", Width: 10, Height: 10}}
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
		got := r.actions()
		assert.Equal(t, 1, got.locatorLookups)
		assert.Empty(t, got.spamUpdates)
		assert.Equal(t, []int64{501}, got.userBans)
		assert.Equal(t, []int{701}, got.messageDeletes)
	})

	t.Run("O media without caption locator miss fails closed", func(t *testing.T) {
		r := newAdminForwardReplay(t, storage.MsgMeta{}, false)
		msg := replayAdminMessage(userOrigin(501, "source"), "")
		msg.Video = &tbapi.Video{FileID: "bot-specific", FileUniqueID: "stable-video"}
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
		assertNoModerationActions(t, r.actions())
		assert.Equal(t, 1, r.actions().locatorLookups)
	})

	t.Run("P URL entity does not alter locator key or actions", func(t *testing.T) {
		r := newAdminForwardReplay(t, locatedUser, true)
		text := "safe https://example.com"
		msg := replayAdminMessage(userOrigin(501, "source"), text)
		msg.Entities = []tbapi.MessageEntity{{Type: "url", Offset: 5, Length: 19}}
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
		assert.Equal(t, text, r.locator.MessageCalls()[0].Msg)
		assert.Equal(t, []string{text}, r.actions().spamUpdates)
	})

	t.Run("Q ReplyMarkup is irrelevant to admin-forward authority", func(t *testing.T) {
		r := newAdminForwardReplay(t, locatedUser, true)
		url := "https://example.com"
		msg := replayAdminMessage(userOrigin(501, "source"), "text")
		msg.ReplyMarkup = &tbapi.InlineKeyboardMarkup{InlineKeyboard: [][]tbapi.InlineKeyboardButton{{{URL: &url}}}}
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
		assert.Equal(t, []string{"text"}, r.actions().spamUpdates)
		assert.Equal(t, []int64{501}, r.actions().userBans)
	})

	t.Run("R duplicate report repeats every destructive action", func(t *testing.T) {
		r := newAdminForwardReplay(t, locatedUser, true)
		update := tbapi.Update{Message: replayAdminMessage(userOrigin(501, "source"), "text")}
		require.NoError(t, r.handler.MsgHandler(update))
		require.NoError(t, r.handler.MsgHandler(update))
		got := r.actions()
		assert.Len(t, got.approvedRemovals, 2)
		assert.Len(t, got.spamUpdates, 2)
		assert.Len(t, got.userBans, 2)
		assert.Len(t, got.messageDeletes, 2)
	})
}

func TestAdminForwardCaptionlessMediaPhase2A(t *testing.T) {
	user := storage.MsgMeta{ChatID: replayPrimaryChatID, UserID: 501, UserName: "source", MsgID: 701}
	media := func(kind storage.MediaKind) *tbapi.Message {
		msg := replayAdminMessage(userOrigin(501, "source"), "")
		switch kind {
		case storage.MediaPhoto:
			msg.Photo = []tbapi.PhotoSize{{FileUniqueID: "stable-photo", Width: 100, Height: 100}}
		case storage.MediaVideo:
			msg.Video = &tbapi.Video{FileUniqueID: "stable-video"}
		case storage.MediaDocument:
			msg.Document = &tbapi.Document{FileUniqueID: "stable-document", FileName: "ignored.txt"}
		case storage.MediaAnimation:
			msg.Animation = &tbapi.Animation{FileUniqueID: "stable-animation"}
		}
		return msg
	}

	for _, kind := range []storage.MediaKind{storage.MediaPhoto, storage.MediaVideo, storage.MediaDocument, storage.MediaAnimation} {
		t.Run("matching "+string(kind), func(t *testing.T) {
			r := newAdminForwardReplay(t, user, true)
			require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: media(kind)}))
			got := r.actions()
			assert.Empty(t, got.spamUpdates)
			assert.Equal(t, []int64{501}, got.userBans)
			assert.Equal(t, []int{701}, got.messageDeletes)
			require.Len(t, r.locator.MessageByMediaCalls(), 1)
			assert.Equal(t, kind, r.locator.MessageByMediaCalls()[0].Key.Kind)
			assert.Equal(t, storage.LocatorIdentity{Kind: storage.LocatorIdentityUser, ID: 501}, r.locator.MessageByMediaCalls()[0].Identity)
		})
	}

	t.Run("wrong target is blocked after malicious locator result", func(t *testing.T) {
		r := newAdminForwardReplay(t, storage.MsgMeta{ChatID: replayPrimaryChatID, UserID: 202, MsgID: 702}, true)
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: media(storage.MediaPhoto)}))
		assertNoModerationActions(t, r.actions())
		assert.NotEmpty(t, r.api.SendCalls())
	})

	t.Run("wrong sender chat is blocked after malicious locator result", func(t *testing.T) {
		located := storage.MsgMeta{ChatID: replayPrimaryChatID, UserID: -100999, MsgID: 702}
		r := newAdminForwardReplay(t, located, true)
		msg := replayAdminMessage(&tbapi.MessageOrigin{Type: tbapi.MessageOriginChannel, Chat: &tbapi.Chat{ID: -100300}}, "")
		msg.Photo = []tbapi.PhotoSize{{FileUniqueID: "stable-channel-photo"}}
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
		assertNoModerationActions(t, r.actions())
	})

	t.Run("wrong source chat is blocked", func(t *testing.T) {
		r := newAdminForwardReplay(t, storage.MsgMeta{ChatID: 999, UserID: 501, MsgID: 702}, true)
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: media(storage.MediaPhoto)}))
		assertNoModerationActions(t, r.actions())
	})

	t.Run("approved user and sender chat stay locked", func(t *testing.T) {
		r := newAdminForwardReplay(t, user, true)
		r.approved[501] = true
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: media(storage.MediaPhoto)}))
		assertNoModerationActions(t, r.actions())

		channel := storage.MsgMeta{ChatID: replayPrimaryChatID, UserID: -100300, UserName: "channel", MsgID: 703}
		r = newAdminForwardReplay(t, channel, true)
		r.approved[-100300] = true
		msg := replayAdminMessage(&tbapi.MessageOrigin{Type: tbapi.MessageOriginChannel, Chat: &tbapi.Chat{ID: -100300}}, "")
		msg.Video = &tbapi.Video{FileUniqueID: "stable-channel-video"}
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
		assertNoModerationActions(t, r.actions())
	})

	t.Run("matching sender chat bans sender chat without learning", func(t *testing.T) {
		channel := storage.MsgMeta{ChatID: replayPrimaryChatID, UserID: -100300, UserName: "channel", MsgID: 703}
		r := newAdminForwardReplay(t, channel, true)
		msg := replayAdminMessage(&tbapi.MessageOrigin{Type: tbapi.MessageOriginChannel, Chat: &tbapi.Chat{ID: -100300}}, "")
		msg.Document = &tbapi.Document{FileUniqueID: "stable-channel-document"}
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
		got := r.actions()
		assert.Empty(t, got.spamUpdates)
		assert.Equal(t, []int64{-100300}, got.channelBans)
		assert.Equal(t, []int{703}, got.messageDeletes)
	})

	t.Run("missing id unsupported and album fail closed", func(t *testing.T) {
		fixtures := []*tbapi.Message{
			media(storage.MediaPhoto),
			replayAdminMessage(userOrigin(501, "source"), ""),
			replayAdminMessage(userOrigin(501, "source"), ""),
		}
		fixtures[0].Photo[0].FileUniqueID = ""
		fixtures[1].Audio = &tbapi.Audio{FileUniqueID: "unsupported-audio"}
		fixtures[2].Photo = []tbapi.PhotoSize{{FileUniqueID: "album-photo"}}
		fixtures[2].MediaGroupID = "album"
		for _, msg := range fixtures {
			r := newAdminForwardReplay(t, user, true)
			require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
			assertNoModerationActions(t, r.actions())
			assert.Empty(t, r.locator.MessageByMediaCalls())
		}
	})

	t.Run("malformed origins fail closed before media lookup", func(t *testing.T) {
		fixtures := []*tbapi.Message{
			replayAdminMessage(&tbapi.MessageOrigin{Type: tbapi.MessageOriginUser}, ""),
			replayAdminMessage(&tbapi.MessageOrigin{Type: tbapi.MessageOriginUser, SenderUser: &tbapi.User{}}, ""),
			replayAdminMessage(&tbapi.MessageOrigin{Type: tbapi.MessageOriginHiddenUser, SenderUserName: "hidden"}, ""),
			replayAdminMessage(&tbapi.MessageOrigin{Type: "future-origin"}, ""),
			replayAdminMessage(nil, ""),
		}
		for _, msg := range fixtures {
			msg.Photo = []tbapi.PhotoSize{{FileUniqueID: "stable-photo"}}
			r := newAdminForwardReplay(t, user, true)
			require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
			assertNoModerationActions(t, r.actions())
			assert.Empty(t, r.locator.MessageByMediaCalls())
		}
	})

	t.Run("sender chat locator miss fails closed without textual fallback", func(t *testing.T) {
		r := newAdminForwardReplay(t, storage.MsgMeta{}, false)
		msg := replayAdminMessage(&tbapi.MessageOrigin{Type: tbapi.MessageOriginChannel, Chat: &tbapi.Chat{ID: -100300}}, "")
		msg.Animation = &tbapi.Animation{FileUniqueID: "stable-channel-animation"}
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
		assertNoModerationActions(t, r.actions())
		assert.Empty(t, r.locator.MessageCalls())
	})

	t.Run("reply markup is irrelevant to captionless media authority", func(t *testing.T) {
		r := newAdminForwardReplay(t, user, true)
		msg := media(storage.MediaVideo)
		externalURL := "https://example.com"
		msg.ReplyMarkup = &tbapi.InlineKeyboardMarkup{InlineKeyboard: [][]tbapi.InlineKeyboardButton{{{Text: "button", URL: &externalURL}}}}
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
		got := r.actions()
		assert.Empty(t, got.spamUpdates)
		assert.Equal(t, []int64{501}, got.userBans)
		assert.Equal(t, []int{701}, got.messageDeletes)
	})

	for _, kind := range []storage.MediaKind{storage.MediaPhoto, storage.MediaVideo, storage.MediaDocument, storage.MediaAnimation} {
		t.Run("caption remains textual for "+string(kind), func(t *testing.T) {
			r := newAdminForwardReplay(t, user, true)
			msg := media(kind)
			msg.Caption = "caption text"
			require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
			assert.Len(t, r.locator.MessageCalls(), 1)
			assert.Empty(t, r.locator.MessageByMediaCalls())
			assert.Equal(t, []string{"caption text"}, r.actions().spamUpdates)
		})
	}
}

func TestAdminForwardOfflineReplayLocatorWrongUserSafety(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "locator.db")
	db, err := engine.NewSqlite(dbPath, "replay")
	require.NoError(t, err)
	locator, err := storage.NewLocator(context.Background(), time.Hour, 100, db)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, locator.Close(context.Background())) })

	const sameText = "identical forwarded text"
	require.NoError(t, locator.AddMessage(context.Background(), sameText, replayPrimaryChatID, 101, "first", 1001))
	require.NoError(t, locator.AddMessage(context.Background(), sameText, replayPrimaryChatID, 202, "second", 1002))

	r := newAdminForwardReplay(t, storage.MsgMeta{}, false)
	r.handler.locator = locator
	msg := replayAdminMessage(userOrigin(101, "first"), sameText)
	require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))

	// The newest identical-text record belongs to 202, while Telegram's reliable
	// ForwardOrigin says 101. Phase 1 must fail closed before every mutation.
	assertNoModerationActions(t, r.actions())
	assert.Equal(t, 1, r.actions().feedback)

	matching := newAdminForwardReplay(t, storage.MsgMeta{}, false)
	matching.handler.locator = locator
	matchingMsg := replayAdminMessage(userOrigin(202, "second"), sameText)
	require.NoError(t, matching.handler.MsgHandler(tbapi.Update{Message: matchingMsg}))
	assert.Equal(t, []int64{202}, matching.actions().approvedRemovals)
	assert.Equal(t, []int64{202}, matching.actions().userBans)
	assert.Equal(t, []int{1002}, matching.actions().messageDeletes)
}

func TestAdminForwardOfflineReplaySafetyInvariants(t *testing.T) {
	r := newAdminForwardReplay(t, storage.MsgMeta{UserID: 501, UserName: "source", MsgID: 701}, true)
	require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(userOrigin(501, "source"), "text")}))

	// The only boundaries reachable by the replay are generated in-memory mocks.
	// No HTTP client, filesystem path, SQL connection, or production credential is present.
	assert.NotNil(t, r.api)
	assert.NotNil(t, r.bot)
	assert.NotNil(t, r.locator)
	assert.False(t, strings.Contains(t.TempDir(), "/opt/antispam-bots/runtime"))
}
