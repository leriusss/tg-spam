package bot

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/app/storage/engine"
	"github.com/umputun/tg-spam/lib/tgspam"
)

func TestOnMessageAutoProgressIdentity(t *testing.T) {
	db, err := engine.NewSqlite(filepath.Join(t.TempDir(), "progress.sqlite"), "group")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	users, err := storage.NewApprovedUsers(context.Background(), db)
	require.NoError(t, err)
	makeFilter := func() *SpamFilter {
		d := tgspam.NewDetector(tgspam.Config{FirstMessagesCount: 3, MaxAllowedEmoji: -1})
		_, loadErr := d.WithUserStorage(users)
		require.NoError(t, loadErr)
		return NewSpamFilter(d, SpamConfig{})
	}
	filter := makeFilter()
	msg := Message{ID: 10, ChatID: -100, Text: "normal friendly conversation", From: User{ID: 123}}
	for range 5 {
		filter.OnMessage(msg, false)
	}
	filter = makeFilter()
	filter.OnMessage(msg, false)
	msg.ChatID = -200
	filter.OnMessage(msg, false)
	msg.SenderChat = SenderChat{ID: -123, UserName: "channel"}
	for range 5 {
		filter.OnMessage(msg, false)
	}
	rows, err := users.Read(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.Equal(t, 1, row.Progress[-200].Count)
		if row.UserID == "123" {
			require.Equal(t, 1, row.Progress[-100].Count)
		} else {
			require.Equal(t, "-123", row.UserID)
			require.NotContains(t, row.Progress, int64(-100))
		}
	}
}
