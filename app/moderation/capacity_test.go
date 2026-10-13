package moderation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCleanupCapacityDoesNotDiscardBanOrOriginal(t *testing.T) {
	j, _ := testJournal(t, Limits{Receipts: 10, Actions: 3})
	now := time.Now()
	p := plan(1301, "ban", "original_delete", "cleanup_delete", "cleanup_delete")
	p.Destination = 0
	accept(t, j, p, now)
	require.Equal(t, Pending, find(t, j, "ban", 1301).State)
	require.Equal(t, Pending, find(t, j, "original_delete", 1301).State)
	require.Equal(t, Manual, find(t, j, "cleanup_delete", 1305).State)
	calls := 0
	w := Worker{
		Journal:  j,
		Now:      func() time.Time { return now },
		Executor: executeFunc(func(context.Context, Action) Outcome { calls++; return Outcome{State: Confirmed} }),
	}
	drive(t, &w)
	require.Equal(t, 3, calls)
	require.Equal(t, Confirmed, find(t, j, "ban", 1301).State)
}
