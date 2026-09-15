package storage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/tg-spam/app/storage/engine"
)

func newMediaTestLocator(t *testing.T, gid string, ttl time.Duration, minSize int) (*Locator, *engine.SQL) {
	t.Helper()
	db, err := engine.NewSqlite(filepath.Join(t.TempDir(), "locator.db"), gid)
	require.NoError(t, err)
	locator, err := NewLocator(context.Background(), ttl, minSize, db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return locator, db
}

func locatorMediaKey(kind MediaKind, id string) MediaLocatorKey {
	return MediaLocatorKey{Version: MediaLocatorVersion, Kind: kind, StableMediaID: id}
}

func TestLocatorMediaIdentityScope(t *testing.T) {
	ctx := context.Background()
	locator, db := newMediaTestLocator(t, "instance", time.Hour, 1000)
	key := locatorMediaKey(MediaPhoto, "same-media")
	user101 := LocatorIdentity{Kind: LocatorIdentityUser, ID: 101}
	user202 := LocatorIdentity{Kind: LocatorIdentityUser, ID: 202}

	require.NoError(t, locator.AddMediaMessage(ctx, key, 123, user101, "first", 1001))
	require.NoError(t, locator.AddMediaMessage(ctx, key, 123, user202, "newer-other-user", 2002))
	baseHash := locator.MsgHash(mediaLocatorPreimage(key))
	_, err := db.Exec(`INSERT INTO messages (hash, gid, time, chat_id, user_id, user_name, msg_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, baseHash+":9999", "other-instance", time.Now().Add(time.Minute), 123, 101, "other", 9999)
	require.NoError(t, err)

	got, ok := locator.MessageByMedia(ctx, key, 123, user101)
	require.True(t, ok)
	assert.Equal(t, int64(101), got.UserID)
	assert.Equal(t, 1001, got.MsgID)

	_, ok = locator.MessageByMedia(ctx, key, 999, user101)
	assert.False(t, ok, "source chat is part of media lookup scope")

	require.NoError(t, locator.AddMediaMessage(ctx, key, 123, user101, "first", 1003))
	got, ok = locator.MessageByMedia(ctx, key, 123, user101)
	require.True(t, ok)
	assert.Equal(t, 1003, got.MsgID, "newest match within one identity wins")
}

func TestLocatorMediaSenderChatAndTypedKeys(t *testing.T) {
	ctx := context.Background()
	locator, _ := newMediaTestLocator(t, "instance", time.Hour, 1000)
	user := LocatorIdentity{Kind: LocatorIdentityUser, ID: 101}
	channel := LocatorIdentity{Kind: LocatorIdentitySenderChat, ID: -100123}
	photo := locatorMediaKey(MediaPhoto, "same-id")
	video := locatorMediaKey(MediaVideo, "same-id")
	document := locatorMediaKey(MediaDocument, "document-id")
	animation := locatorMediaKey(MediaAnimation, "animation-id")

	require.NoError(t, locator.AddMediaMessage(ctx, photo, 123, user, "user", 1))
	require.NoError(t, locator.AddMediaMessage(ctx, photo, 123, channel, "channel", 2))
	require.NoError(t, locator.AddMediaMessage(ctx, video, 123, user, "user", 3))
	require.NoError(t, locator.AddMediaMessage(ctx, document, 123, user, "user", 5))
	require.NoError(t, locator.AddMediaMessage(ctx, animation, 123, user, "user", 6))

	got, ok := locator.MessageByMedia(ctx, photo, 123, channel)
	require.True(t, ok)
	assert.Equal(t, channel.ID, got.UserID)
	assert.Equal(t, 2, got.MsgID)

	got, ok = locator.MessageByMedia(ctx, video, 123, user)
	require.True(t, ok)
	assert.Equal(t, 3, got.MsgID, "media kind participates in canonical key")

	got, ok = locator.MessageByMedia(ctx, document, 123, user)
	require.True(t, ok)
	assert.Equal(t, 5, got.MsgID)
	got, ok = locator.MessageByMedia(ctx, animation, 123, user)
	require.True(t, ok)
	assert.Equal(t, 6, got.MsgID)

	_, ok = locator.MessageByMedia(ctx, locatorMediaKey(MediaPhoto, "different"), 123, user)
	assert.False(t, ok)

	// A legacy empty-text record and a record with the same filename-like metadata
	// are not candidates for a typed media lookup.
	require.NoError(t, locator.AddMessage(ctx, "", 123, user.ID, "user", 4))
	_, ok = locator.MessageByMedia(ctx, locatorMediaKey(MediaDocument, "filename-only"), 123, user)
	assert.False(t, ok)
}

func TestLocatorMediaHashDomainIsolatedFromText(t *testing.T) {
	ctx := context.Background()
	locator, _ := newMediaTestLocator(t, "instance", time.Hour, 1000)
	identity := LocatorIdentity{Kind: LocatorIdentityUser, ID: 101}
	key := locatorMediaKey(MediaPhoto, "same-visible-preimage")

	require.NoError(t, locator.AddMediaMessage(ctx, key, 123, identity, "user", 10))
	// This exact text hashes to the media digest unless the shared hash column has
	// an explicit media namespace. Make it newer to expose candidate shadowing.
	require.NoError(t, locator.AddMessage(ctx, mediaLocatorPreimage(key), 123, identity.ID, "user", 20))

	got, ok := locator.MessageByMedia(ctx, key, 123, identity)
	require.True(t, ok)
	assert.Equal(t, 10, got.MsgID)
	assert.NotEqual(t, locator.MsgHash(mediaLocatorPreimage(key)), locator.mediaLocatorHash(key))
}

func TestLocatorMediaInputValidation(t *testing.T) {
	ctx := context.Background()
	locator, _ := newMediaTestLocator(t, "instance", time.Hour, 1000)
	valid := locatorMediaKey(MediaPhoto, "media")

	for _, tt := range []struct {
		name     string
		key      MediaLocatorKey
		identity LocatorIdentity
	}{
		{"empty id", locatorMediaKey(MediaPhoto, ""), LocatorIdentity{Kind: LocatorIdentityUser, ID: 1}},
		{"unknown version", MediaLocatorKey{Version: 2, Kind: MediaPhoto, StableMediaID: "x"}, LocatorIdentity{Kind: LocatorIdentityUser, ID: 1}},
		{"unknown kind", locatorMediaKey("audio", "x"), LocatorIdentity{Kind: LocatorIdentityUser, ID: 1}},
		{"negative user", valid, LocatorIdentity{Kind: LocatorIdentityUser, ID: -1}},
		{"positive sender chat", valid, LocatorIdentity{Kind: LocatorIdentitySenderChat, ID: 1}},
		{"unknown identity", valid, LocatorIdentity{Kind: "chat", ID: -1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Error(t, locator.AddMediaMessage(ctx, tt.key, 123, tt.identity, "", 1))
			_, ok := locator.MessageByMedia(ctx, tt.key, 123, tt.identity)
			assert.False(t, ok)
		})
	}
}

func TestLocatorMediaPruningAndQueryPlan(t *testing.T) {
	ctx := context.Background()
	locator, db := newMediaTestLocator(t, "instance", time.Millisecond, 1)
	identity := LocatorIdentity{Kind: LocatorIdentityUser, ID: 101}
	oldKey := locatorMediaKey(MediaDocument, "old")
	require.NoError(t, locator.AddMediaMessage(ctx, oldKey, 123, identity, "user", 1))
	_, err := db.Exec("UPDATE messages SET time = ?", time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.NoError(t, locator.AddMediaMessage(ctx, locatorMediaKey(MediaDocument, "new"), 123, identity, "user", 2))
	_, ok := locator.MessageByMedia(ctx, oldKey, 123, identity)
	assert.False(t, ok, "media rows use ordinary locator TTL cleanup")

	baseHash := locator.mediaLocatorHash(locatorMediaKey(MediaDocument, "new"))
	query := `EXPLAIN QUERY PLAN SELECT time, chat_id, user_id, user_name, msg_id FROM messages
		WHERE gid = ? AND chat_id = ? AND user_id = ? AND (hash = ? OR hash LIKE ?)
		ORDER BY time DESC, msg_id DESC LIMIT 1`
	var plan []struct {
		ID      int    `db:"id"`
		Parent  int    `db:"parent"`
		NotUsed int    `db:"notused"`
		Detail  string `db:"detail"`
	}
	require.NoError(t, db.Select(&plan, query, "instance", int64(123), int64(101), baseHash, baseHash+":%"))
	require.NotEmpty(t, plan)
	details := ""
	for _, row := range plan {
		details += row.Detail + "\n"
	}
	assert.Contains(t, details, "idx_messages_gid_user_id_time")
	assert.NotContains(t, strings.ToUpper(details), "SCAN MESSAGES")
	t.Logf("media lookup query plan: %s", strings.TrimSpace(details))
}
