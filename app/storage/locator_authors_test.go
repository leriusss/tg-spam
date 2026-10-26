package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/storage/engine"
)

func TestLocatorAuthorsRequireOriginalUpdateProvenance(t *testing.T) {
	ctx := context.Background()
	db, err := engine.NewSqlite(filepath.Join(t.TempDir(), "authors.db"), "authors")
	require.NoError(t, err)
	defer db.Close()
	loc, err := NewLocator(ctx, time.Hour, 100, db)
	require.NoError(t, err)
	require.NoError(t, loc.AddMessage(ctx, "legacy message", 123, 501, "saved name", 801))
	a, err := loc.MessageAuthor(ctx, 123, 801)
	require.NoError(t, err)
	require.Empty(t, a.Source, "migration does not backfill provenance")
	good := MessageAuthor{Kind: "user", ID: 501, Source: "telegram_update", UpdateID: 1000}
	require.NoError(t, loc.RecordMessageAuthor(ctx, 123, 801, good))
	a, err = loc.MessageAuthor(ctx, 123, 801)
	require.NoError(t, err)
	require.Equal(t, good, a)
	a, err = loc.MessageAuthor(ctx, 999, 801)
	require.NoError(t, err)
	require.Zero(t, a.ID)
	for _, bad := range []MessageAuthor{{Kind: "user", ID: 501, Source: "forward", UpdateID: 1001}, {Kind: "user", ID: 501, Source: "telegram_update"}, {Kind: "sender_chat", ID: 501, Source: "telegram_update", UpdateID: 1000}} {
		require.Error(t, loc.RecordMessageAuthor(ctx, 123, 801, bad))
	}
	changed := good
	changed.ID = 502
	changed.UpdateID++
	require.NoError(t, loc.RecordMessageAuthor(ctx, 123, 801, changed))
	a, err = loc.MessageAuthor(ctx, 123, 801)
	require.NoError(t, err)
	require.Equal(t, "conflict", a.Source)
	require.NoError(t, loc.RecordMessageAuthor(ctx, 123, 801, good))
	a, err = loc.MessageAuthor(ctx, 123, 801)
	require.NoError(t, err)
	require.Equal(t, "conflict", a.Source)
	reopened, err := NewLocator(ctx, time.Hour, 100, db)
	require.NoError(t, err)
	a, err = reopened.MessageAuthor(ctx, 123, 801)
	require.NoError(t, err)
	require.Equal(t, "conflict", a.Source)
}
