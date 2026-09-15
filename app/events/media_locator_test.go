package events

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/events/mocks"
	"github.com/umputun/tg-spam/app/storage"
)

func TestExtractMediaLocatorKey(t *testing.T) {
	tests := []struct {
		name   string
		msg    *tbapi.Message
		key    storage.MediaLocatorKey
		status mediaLocatorStatus
	}{
		{"photo one", &tbapi.Message{Photo: []tbapi.PhotoSize{{FileUniqueID: "p", Width: 2, Height: 3}}}, mediaKeyForTest(storage.MediaPhoto, "p"), mediaLocatorExtracted},
		{"photo unordered largest", &tbapi.Message{Photo: []tbapi.PhotoSize{{FileUniqueID: "large", Width: 4, Height: 5}, {FileUniqueID: "small", Width: 2, Height: 2}}}, mediaKeyForTest(storage.MediaPhoto, "large"), mediaLocatorExtracted},
		{"photo deterministic tie", &tbapi.Message{Photo: []tbapi.PhotoSize{{FileUniqueID: "a", Width: 4, Height: 5}, {FileUniqueID: "b", Width: 5, Height: 4}}}, mediaKeyForTest(storage.MediaPhoto, "b"), mediaLocatorExtracted},
		{"photo empty selected id", &tbapi.Message{Photo: []tbapi.PhotoSize{{FileUniqueID: "usable", Width: 2, Height: 2}, {Width: 4, Height: 4}}}, storage.MediaLocatorKey{}, mediaLocatorUnavailable},
		{"video", &tbapi.Message{Video: &tbapi.Video{FileUniqueID: "v"}}, mediaKeyForTest(storage.MediaVideo, "v"), mediaLocatorExtracted},
		{"document", &tbapi.Message{Document: &tbapi.Document{FileUniqueID: "d", FileName: "ignored"}}, mediaKeyForTest(storage.MediaDocument, "d"), mediaLocatorExtracted},
		{"animation before compatibility document", &tbapi.Message{Animation: &tbapi.Animation{FileUniqueID: "a"}, Document: &tbapi.Document{FileUniqueID: "d"}}, mediaKeyForTest(storage.MediaAnimation, "a"), mediaLocatorExtracted},
		{"video empty", &tbapi.Message{Video: &tbapi.Video{}}, storage.MediaLocatorKey{}, mediaLocatorUnavailable},
		{"document empty", &tbapi.Message{Document: &tbapi.Document{}}, storage.MediaLocatorKey{}, mediaLocatorUnavailable},
		{"animation empty", &tbapi.Message{Animation: &tbapi.Animation{}}, storage.MediaLocatorKey{}, mediaLocatorUnavailable},
		{"audio unsupported", &tbapi.Message{Audio: &tbapi.Audio{FileUniqueID: "a"}}, storage.MediaLocatorKey{}, mediaLocatorUnsupported},
		{"voice unsupported", &tbapi.Message{Voice: &tbapi.Voice{FileUniqueID: "v"}}, storage.MediaLocatorKey{}, mediaLocatorUnsupported},
		{"video note unsupported", &tbapi.Message{VideoNote: &tbapi.VideoNote{FileUniqueID: "n"}}, storage.MediaLocatorKey{}, mediaLocatorUnsupported},
		{"sticker unsupported", &tbapi.Message{Sticker: &tbapi.Sticker{FileUniqueID: "s"}}, storage.MediaLocatorKey{}, mediaLocatorUnsupported},
		{"album deferred", &tbapi.Message{MediaGroupID: "g", Photo: []tbapi.PhotoSize{{FileUniqueID: "p"}}}, storage.MediaLocatorKey{}, mediaLocatorAlbum},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, status := extractMediaLocatorKey(tt.msg)
			assert.Equal(t, tt.key, key)
			assert.Equal(t, tt.status, status)
		})
	}

	t.Run("kind participates in typed key", func(t *testing.T) {
		photo, _ := extractMediaLocatorKey(&tbapi.Message{Photo: []tbapi.PhotoSize{{FileUniqueID: "same"}}})
		video, _ := extractMediaLocatorKey(&tbapi.Message{Video: &tbapi.Video{FileUniqueID: "same"}})
		assert.NotEqual(t, photo.Kind, video.Kind)
	})

	t.Run("file id and document metadata are not canonical", func(t *testing.T) {
		first, status := extractMediaLocatorKey(&tbapi.Message{Document: &tbapi.Document{
			FileID: "download-a", FileUniqueID: "stable", FileName: "a.pdf", MimeType: "application/pdf",
		}})
		require.Equal(t, mediaLocatorExtracted, status)
		second, status := extractMediaLocatorKey(&tbapi.Message{Document: &tbapi.Document{
			FileID: "download-b", FileUniqueID: "stable", FileName: "b.bin", MimeType: "application/octet-stream",
		}})
		require.Equal(t, mediaLocatorExtracted, status)
		assert.Equal(t, first, second)
	})

	t.Run("same filename with different stable ids remains distinct", func(t *testing.T) {
		first, _ := extractMediaLocatorKey(&tbapi.Message{Document: &tbapi.Document{FileUniqueID: "stable-a", FileName: "same.pdf"}})
		second, _ := extractMediaLocatorKey(&tbapi.Message{Document: &tbapi.Document{FileUniqueID: "stable-b", FileName: "same.pdf"}})
		assert.NotEqual(t, first, second)
	})
}

func mediaKeyForTest(kind storage.MediaKind, id string) storage.MediaLocatorKey {
	return storage.MediaLocatorKey{Version: storage.MediaLocatorVersion, Kind: kind, StableMediaID: id}
}

func TestCaptionlessMediaLogsDoNotExposeIdentifiers(t *testing.T) {
	locator := &mocks.LocatorMock{
		AddMediaMessageFunc: func(context.Context, storage.MediaLocatorKey, int64, storage.LocatorIdentity, string, int) error {
			return nil
		},
	}
	listener := TelegramListener{Locator: locator, chatID: replayPrimaryChatID,
		Bot: &mocks.BotMock{OnMessageFunc: func(bot.Message, bool) bot.Response { return bot.Response{} }}}
	msg := &tbapi.Message{MessageID: 79, Chat: tbapi.Chat{ID: replayPrimaryChatID}, From: &tbapi.User{ID: 101},
		Document: &tbapi.Document{FileID: "raw-file-id", FileUniqueID: "raw-unique-id", FileName: "private-name.pdf", MimeType: "application/pdf"}}

	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(old) })
	require.NoError(t, listener.procEvents(tbapi.Update{Message: msg}))

	for _, secret := range []string{"raw-file-id", "raw-unique-id", "private-name.pdf", "application/pdf"} {
		assert.NotContains(t, logs.String(), secret)
	}
	assert.Contains(t, strings.ToLower(logs.String()), "media locator registered")
}

func TestCaptionlessMediaListenerRegistration(t *testing.T) {
	for _, tt := range []struct {
		name             string
		set              func(*tbapi.Message)
		kind             storage.MediaKind
		legacyModeration bool
	}{
		{"photo", func(m *tbapi.Message) { m.Photo = []tbapi.PhotoSize{{FileUniqueID: "photo"}} }, storage.MediaPhoto, true},
		{"video", func(m *tbapi.Message) { m.Video = &tbapi.Video{FileUniqueID: "video"} }, storage.MediaVideo, true},
		{"document", func(m *tbapi.Message) { m.Document = &tbapi.Document{FileUniqueID: "document"} }, storage.MediaDocument, false},
		{"animation", func(m *tbapi.Message) { m.Animation = &tbapi.Animation{FileUniqueID: "animation"} }, storage.MediaAnimation, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			locator := &mocks.LocatorMock{
				AddMessageFunc: func(context.Context, string, int64, int64, string, int) error { return nil },
				AddMediaMessageFunc: func(context.Context, storage.MediaLocatorKey, int64, storage.LocatorIdentity, string, int) error {
					return nil
				},
			}
			botCalls := 0
			listener := TelegramListener{
				Locator: locator, chatID: replayPrimaryChatID,
				Bot: &mocks.BotMock{OnMessageFunc: func(bot.Message, bool) bot.Response { botCalls++; return bot.Response{} }},
			}
			msg := &tbapi.Message{MessageID: 77, Chat: tbapi.Chat{ID: replayPrimaryChatID}, From: &tbapi.User{ID: 101, UserName: "sender"}}
			tt.set(msg)
			require.NoError(t, listener.procEvents(tbapi.Update{Message: msg}))
			assert.Empty(t, locator.AddMessageCalls(), "captionless media must never use the empty textual key")
			require.Len(t, locator.AddMediaMessageCalls(), 1)
			assert.Equal(t, tt.kind, locator.AddMediaMessageCalls()[0].Key.Kind)
			assert.Equal(t, storage.LocatorIdentity{Kind: storage.LocatorIdentityUser, ID: 101}, locator.AddMediaMessageCalls()[0].Identity)
			if tt.legacyModeration {
				assert.Equal(t, 1, botCalls)
			} else {
				assert.Zero(t, botCalls, "new locator admission must not change direct moderation")
			}
		})
	}

	t.Run("caption remains text only", func(t *testing.T) {
		locator := &mocks.LocatorMock{AddMessageFunc: func(context.Context, string, int64, int64, string, int) error { return nil }}
		listener := TelegramListener{Locator: locator, chatID: replayPrimaryChatID,
			Bot: &mocks.BotMock{OnMessageFunc: func(bot.Message, bool) bot.Response { return bot.Response{} }}}
		msg := &tbapi.Message{MessageID: 78, Chat: tbapi.Chat{ID: replayPrimaryChatID}, From: &tbapi.User{ID: 101},
			Caption: "caption", Photo: []tbapi.PhotoSize{{FileUniqueID: "photo"}}}
		require.NoError(t, listener.procEvents(tbapi.Update{Message: msg}))
		require.Len(t, locator.AddMessageCalls(), 1)
		assert.Equal(t, "caption", locator.AddMessageCalls()[0].Msg)
		assert.Empty(t, locator.AddMediaMessageCalls())
	})

	t.Run("sender chat registration uses negative identity namespace", func(t *testing.T) {
		locator := &mocks.LocatorMock{
			AddMediaMessageFunc: func(context.Context, storage.MediaLocatorKey, int64, storage.LocatorIdentity, string, int) error {
				return nil
			},
		}
		listener := TelegramListener{Locator: locator, chatID: replayPrimaryChatID,
			Bot: &mocks.BotMock{OnMessageFunc: func(bot.Message, bool) bot.Response { return bot.Response{} }}}
		msg := &tbapi.Message{MessageID: 80, Chat: tbapi.Chat{ID: replayPrimaryChatID},
			SenderChat: &tbapi.Chat{ID: -100123, UserName: "channel"}, Photo: []tbapi.PhotoSize{{FileUniqueID: "photo"}}}
		require.NoError(t, listener.procEvents(tbapi.Update{Message: msg}))
		require.Len(t, locator.AddMediaMessageCalls(), 1)
		assert.Equal(t, storage.LocatorIdentity{Kind: storage.LocatorIdentitySenderChat, ID: -100123}, locator.AddMediaMessageCalls()[0].Identity)
	})

	t.Run("unsupported and missing stable id create no locator row", func(t *testing.T) {
		for _, msg := range []*tbapi.Message{
			{MessageID: 81, Chat: tbapi.Chat{ID: replayPrimaryChatID}, From: &tbapi.User{ID: 101}, Audio: &tbapi.Audio{FileUniqueID: "audio"}},
			{MessageID: 82, Chat: tbapi.Chat{ID: replayPrimaryChatID}, From: &tbapi.User{ID: 101}, Photo: []tbapi.PhotoSize{{FileID: "download-only"}}},
			{MessageID: 83, Chat: tbapi.Chat{ID: replayPrimaryChatID}, From: &tbapi.User{ID: 101}, MediaGroupID: "album", Photo: []tbapi.PhotoSize{{FileUniqueID: "photo"}}},
		} {
			locator := &mocks.LocatorMock{}
			listener := TelegramListener{Locator: locator, chatID: replayPrimaryChatID,
				Bot: &mocks.BotMock{OnMessageFunc: func(bot.Message, bool) bot.Response { return bot.Response{} }}}
			require.NoError(t, listener.procEvents(tbapi.Update{Message: msg}))
			assert.Empty(t, locator.AddMessageCalls())
			assert.Empty(t, locator.AddMediaMessageCalls())
		}
	})

	t.Run("external button still reaches existing direct-message guard", func(t *testing.T) {
		externalURL := "https://example.com"
		locator := &mocks.LocatorMock{
			AddMediaMessageFunc: func(context.Context, storage.MediaLocatorKey, int64, storage.LocatorIdentity, string, int) error {
				return nil
			},
		}
		var got []bot.Message
		listener := TelegramListener{Locator: locator, chatID: replayPrimaryChatID,
			Bot: &mocks.BotMock{OnMessageFunc: func(msg bot.Message, _ bool) bot.Response { got = append(got, msg); return bot.Response{} }}}
		msg := &tbapi.Message{MessageID: 84, Chat: tbapi.Chat{ID: replayPrimaryChatID}, From: &tbapi.User{ID: 101},
			Document: &tbapi.Document{FileUniqueID: "document"}, ReplyMarkup: &tbapi.InlineKeyboardMarkup{InlineKeyboard: [][]tbapi.InlineKeyboardButton{{{
				Text: "external", URL: &externalURL,
			}}}}}
		require.NoError(t, listener.procEvents(tbapi.Update{Message: msg}))
		require.Len(t, got, 1)
		assert.True(t, got[0].WithExternalLinkButton)
	})
}
