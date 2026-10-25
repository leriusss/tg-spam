package bot

import (
	"context"
	"path/filepath"
	"strings"
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

func TestOnMessageHanDoesNotAdvancePersistedAutoProgress(t *testing.T) {
	for _, count := range []int{0, 2, 3} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "han-progress.sqlite")
			var db *engine.SQL
			var users *storage.ApprovedUsers
			open := func() *SpamFilter {
				var err error
				db, err = engine.NewSqlite(path, "han-progress")
				require.NoError(t, err)
				users, err = storage.NewApprovedUsers(ctx, db)
				require.NoError(t, err)
				d := tgspam.NewDetector(tgspam.Config{FirstMessagesCount: 3, MinMsgLen: 1, MaxAllowedEmoji: -1})
				_, err = d.WithUserStorage(users)
				require.NoError(t, err)
				return NewSpamFilter(d, SpamConfig{Han: HanConfig{Enabled: true, Ratio: 0.7, MinCount: 20}})
			}
			filter := open()
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			for mid := 1; mid <= count; mid++ {
				filter.OnMessage(Message{ID: mid, ChatID: -100, From: User{ID: 42}, Text: "normal friendly conversation"}, false)
			}
			before, err := users.Read(ctx)
			require.NoError(t, err)
			msg := Message{ID: 100, ChatID: -100, From: User{ID: 42}, Text: strings.Repeat("中", 24)}
			for range 3 {
				res := filter.OnMessage(msg, false)
				require.True(t, res.HanOnly)
				require.Positive(t, res.BanInterval)
			}
			after, err := users.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.NoError(t, db.Close())
			filter = open()
			res := filter.OnMessage(msg, false)
			require.True(t, res.HanOnly)
			after, err = users.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, count == 3, filter.IsApprovedUserInChat(42, -100))
		})
	}
}
