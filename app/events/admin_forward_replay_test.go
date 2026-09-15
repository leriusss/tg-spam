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
	api     *mocks.TbAPIMock
	bot     *mocks.BotMock
	locator *mocks.LocatorMock
	handler *admin
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
		GetUserMessageIDsFunc: func(context.Context, int64, int) ([]int, error) {
			return nil, nil
		},
	}
	return &adminForwardReplay{
		api: api, bot: botMock, locator: locator,
		handler: &admin{
			tbAPI: api, bot: botMock, locator: locator,
			primChatID: replayPrimaryChatID, adminChatID: replayAdminChatID,
			superUsers: SuperUsers{"admin", "11"},
		},
	}
}

func (r *adminForwardReplay) actions() replayActions {
	res := replayActions{locatorLookups: len(r.locator.MessageCalls()), feedback: len(r.api.SendCalls())}
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
		assert.Contains(t, logs, "message in admin chat 456")
		assert.Contains(t, logs, "message from admin chat")
		assert.Empty(t, api.RequestCalls())
		assert.Empty(t, api.SendCalls())
		assert.Empty(t, botMock.UpdateSpamCalls())
	})

	t.Run("B unauthorized admin-chat sender is rejected before handler", func(t *testing.T) {
		logs, api, botMock := run(t, replayAdminMessage(userOrigin(501, "source"), "forward"), nil, false)
		assert.Contains(t, logs, "is not superuser in admin chat, ignored")
		assert.NotContains(t, logs, "message from admin chat")
		assert.Empty(t, api.RequestCalls())
		assert.Empty(t, api.SendCalls())
		assert.Empty(t, botMock.UpdateSpamCalls())
	})

	t.Run("C disabled forwarding silently stops after authorization", func(t *testing.T) {
		logs, api, botMock := run(t, replayAdminMessage(userOrigin(501, "source"), "forward"), SuperUsers{"11"}, true)
		assert.Contains(t, logs, "message in admin chat 456")
		assert.NotContains(t, logs, "message from admin chat")
		assert.Empty(t, api.RequestCalls())
		assert.Empty(t, api.SendCalls())
		assert.Empty(t, botMock.UpdateSpamCalls())
	})
}

func TestAdminForwardOfflineReplayOriginAndContentMatrix(t *testing.T) {
	locatedUser := storage.MsgMeta{UserID: 501, UserName: "source", MsgID: 701}

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

	t.Run("F hidden sender locator miss cannot fall back", func(t *testing.T) {
		r := newAdminForwardReplay(t, storage.MsgMeta{}, false)
		msg := replayAdminMessage(&tbapi.MessageOrigin{Type: tbapi.MessageOriginHiddenUser, SenderUserName: "hidden"}, "text")
		err := r.handler.MsgHandler(tbapi.Update{Message: msg})
		require.ErrorContains(t, err, "not found")
		assertNoModerationActions(t, r.actions())
	})

	t.Run("G missing ForwardOrigin is a silent no-op with or without text", func(t *testing.T) {
		for _, text := range []string{"ordinary", ""} {
			r := newAdminForwardReplay(t, storage.MsgMeta{}, false)
			require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(nil, text)}))
			assertNoModerationActions(t, r.actions())
			assert.Zero(t, r.actions().feedback)
		}
	})

	t.Run("H user origin with nil SenderUser currently panics", func(t *testing.T) {
		r := newAdminForwardReplay(t, storage.MsgMeta{}, false)
		msg := replayAdminMessage(&tbapi.MessageOrigin{Type: tbapi.MessageOriginUser}, "text")
		assert.Panics(t, func() { _ = r.handler.MsgHandler(tbapi.Update{Message: msg}) })
		assertNoModerationActions(t, r.actions())
	})

	t.Run("I hidden origin locator hit trusts locator identity", func(t *testing.T) {
		r := newAdminForwardReplay(t, locatedUser, true)
		msg := replayAdminMessage(&tbapi.MessageOrigin{Type: tbapi.MessageOriginHiddenUser, SenderUserName: "hidden"}, "text")
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: msg}))
		got := r.actions()
		assert.Equal(t, []int64{501}, got.approvedRemovals)
		assert.Equal(t, []int64{501}, got.userBans)
	})

	t.Run("J chat origin has no sender fallback but locator hit moderates located identity", func(t *testing.T) {
		origin := &tbapi.MessageOrigin{Type: tbapi.MessageOriginChat, SenderChat: &tbapi.Chat{ID: -200, UserName: "chat"}}
		r := newAdminForwardReplay(t, locatedUser, true)
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(origin, "text")}))
		assert.Equal(t, []int64{501}, r.actions().userBans)

		miss := newAdminForwardReplay(t, storage.MsgMeta{}, false)
		err := miss.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(origin, "text")})
		require.ErrorContains(t, err, "not found")
		assertNoModerationActions(t, miss.actions())
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

	t.Run("L approved sender chat is still revoked banned and cleanup-targeted", func(t *testing.T) {
		channel := storage.MsgMeta{UserID: -100300, UserName: "approved_channel", MsgID: 703}
		r := newAdminForwardReplay(t, channel, true)
		r.handler.aggressiveCleanup = true
		r.handler.aggressiveCleanupLimit = 10
		cleanupDone := make(chan struct{}, 1)
		r.locator.GetUserMessageIDsFunc = func(_ context.Context, id int64, _ int) ([]int, error) {
			cleanupDone <- struct{}{}
			return nil, nil
		}
		origin := &tbapi.MessageOrigin{Type: tbapi.MessageOriginChannel, Chat: &tbapi.Chat{ID: -100300, UserName: "approved_channel"}}
		require.NoError(t, r.handler.MsgHandler(tbapi.Update{Message: replayAdminMessage(origin, "official text")}))
		select {
		case <-cleanupDone:
		case <-time.After(time.Second):
			t.Fatal("cleanup plan was not recorded")
		}
		got := r.actions()
		assert.Empty(t, r.bot.IsApprovedUserCalls(), "MsgHandler never checks current approval")
		assert.Equal(t, []int64{-100300}, got.approvedRemovals)
		assert.Equal(t, []string{"official text"}, got.spamUpdates)
		assert.Equal(t, []int64{-100300}, got.channelBans)
		assert.Equal(t, []int64{-100300}, got.cleanupLookups)
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

	t.Run("N media without caption exits before locator even when a hit exists", func(t *testing.T) {
		r := newAdminForwardReplay(t, locatedUser, true)
		msg := replayAdminMessage(userOrigin(501, "source"), "")
		msg.Photo = []tbapi.PhotoSize{{FileID: "fake-photo"}}
		err := r.handler.MsgHandler(tbapi.Update{Message: msg})
		require.EqualError(t, err, "empty message text")
		assertNoModerationActions(t, r.actions())
		assert.Zero(t, r.actions().locatorLookups)
	})

	t.Run("O media without caption and locator miss has the same pre-locator exit", func(t *testing.T) {
		r := newAdminForwardReplay(t, storage.MsgMeta{}, false)
		msg := replayAdminMessage(userOrigin(501, "source"), "")
		msg.Video = &tbapi.Video{FileID: "fake-video"}
		err := r.handler.MsgHandler(tbapi.Update{Message: msg})
		require.EqualError(t, err, "empty message text")
		assertNoModerationActions(t, r.actions())
		assert.Zero(t, r.actions().locatorLookups)
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

	// Current MsgHandler does not cross-check ForwardOrigin sender 101 against
	// the locator result. The newest identical-text record (202) is acted on.
	assert.Equal(t, []int64{202}, r.actions().approvedRemovals)
	assert.Equal(t, []int64{202}, r.actions().userBans)
	assert.Equal(t, []int{1002}, r.actions().messageDeletes)
	assert.NotEqual(t, msg.ForwardOrigin.SenderUser.ID, r.actions().userBans[0],
		"characterization: identical text can select and ban a different sender")
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
