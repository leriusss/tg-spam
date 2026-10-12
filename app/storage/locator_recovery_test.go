package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/storage/engine"
)

func TestDurableCleanupSelectionDoesNotMixChatMessageIDs(t *testing.T) {
	ctx := context.Background()
	db, err := engine.NewSqlite(filepath.Join(t.TempDir(), "locator.db"), "group")
	require.NoError(t, err)
	defer db.Close()
	loc, err := NewLocator(ctx, time.Hour, 1000, db)
	require.NoError(t, err)
	require.NoError(t, loc.AddMessage(ctx, "primary", -100, 7, "user", 11))
	require.NoError(t, loc.AddMessage(ctx, "testing chat", -200, 7, "user", 22))
	require.NoError(t, loc.AddMessage(ctx, "other sender", -100, 8, "other", 33))
	ids, err := loc.GetUserMessageIDsInChat(ctx, 7, -100, 50)
	require.NoError(t, err)
	require.Equal(t, []int{11}, ids)
}
