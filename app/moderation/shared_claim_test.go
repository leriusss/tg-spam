package moderation

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/storage/engine"
)

func sharedClaimFixture(t *testing.T) (*SharedStore, *claimReadProbe, string, SharedCase, time.Time) {
	t.Helper()
	_, db, _, probe, path := claimReadFixture(t)
	store, err := NewSharedStore(testCtx, db, 100, []int64{100})
	require.NoError(t, err)
	now := time.Now()
	require.NoError(t, store.Submit(testCtx, SharedReport{Peer: 100, ChatID: 123, Operator: 11,
		Delivery: ForwardDelivery{UpdateID: 1, ChatID: 456, MessageID: 1}}, now))
	cases, err := store.Cases(testCtx)
	require.NoError(t, err)
	require.Len(t, cases, 1)
	return store, probe, path, cases[0], now.Add(3 * time.Second)
}

func TestSharedCardClaimFailedCommitReturnsNoOwner(t *testing.T) {
	store, probe, _, c, now := sharedClaimFixture(t)
	probe.commitError = errors.New("forced shared claim COMMIT failure")
	probe.enabled.Store(true)
	out, claimed, err := store.ClaimCard(testCtx, 100, c.Ref, c.Revision, "render", now)
	require.ErrorIs(t, err, probe.commitError)
	require.False(t, claimed)
	require.Empty(t, out.Lease)
	probe.enabled.Store(false)
	current, err := store.Case(testCtx, c.Ref)
	require.NoError(t, err)
	require.Empty(t, current.Lease)
	require.Zero(t, current.Attempts)
	_, claimed, err = store.ClaimCard(testCtx, 100, c.Ref, c.Revision, "render", now)
	require.NoError(t, err)
	require.True(t, claimed)
}

func TestSharedCardClaimRetryCannotRetainRolledBackOwner(t *testing.T) {
	store, probe, path, c, now := sharedClaimFixture(t)
	otherDB, err := engine.NewSqlite(path, store.journal.db.GID())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, otherDB.Close()) })
	other, err := NewSharedStore(testCtx, otherDB, 100, []int64{100})
	require.NoError(t, err)
	var winner SharedCase
	rollbacks := 0
	// The first real transaction writes its lease, but its COMMIT is refused.
	// After the real ROLLBACK, let a separately opened SQL handle acquire and
	// commit the only lease before the original caller retries. No sleeps or
	// scheduler luck select the interleaving; the existing concurrent SQL-handle
	// scenarios still exercise genuine simultaneous calls independently.
	probe.commitError = errors.New("database is locked")
	probe.onRollback = func(err error) {
		require.NoError(t, err)
		rollbacks++
		probe.enabled.Store(false)
		var claimed bool
		winner, claimed, err = other.ClaimCard(testCtx, 100, c.Ref, c.Revision, "winner render", now)
		require.NoError(t, err)
		require.True(t, claimed)
	}
	probe.enabled.Store(true)
	loser, claimed, err := store.ClaimCard(testCtx, 100, c.Ref, c.Revision, "loser render", now)
	require.NoError(t, err)
	require.Equal(t, 1, rollbacks)
	require.False(t, claimed, "a rolled-back attempt cannot authorize a send after retry loses the lease")
	require.Empty(t, loser.Lease)
	current, err := store.Case(testCtx, c.Ref)
	require.NoError(t, err)
	require.Equal(t, winner.Lease, current.Lease)
	require.Equal(t, 1, current.Attempts, "only the committed claim counts")
	require.Zero(t, store.journal.db.Stats().InUse)
}
