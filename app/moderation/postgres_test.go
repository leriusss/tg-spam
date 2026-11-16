package moderation

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-pkgz/testutils/containers"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/storage/engine"
)

func TestPostgresJournalClaimsAndGIDIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL runs in the full Linux checks")
	}
	ctx := context.Background()
	pg := containers.NewPostgresTestContainerWithDB(ctx, t, "moderation")
	defer pg.Close(ctx)
	db, err := engine.NewPostgres(ctx, pg.ConnectionString(), "one")
	require.NoError(t, err)
	defer db.Close()
	j, err := NewJournal(ctx, db, DefaultLimits())
	require.NoError(t, err)
	otherDB, err := engine.NewPostgres(ctx, pg.ConnectionString(), "one")
	require.NoError(t, err)
	defer otherDB.Close()
	other, err := NewJournal(ctx, otherDB, DefaultLimits())
	require.NoError(t, err)
	now := time.Now()
	p := plan(900, "ban", "original_delete")
	p.Destination = 0
	p.Learn = false
	failures := make(chan error, 8)
	var wg sync.WaitGroup
	var owners atomic.Int32
	for _, store := range []*Journal{j, other, j, other} {
		wg.Add(1)
		go func(s *Journal) {
			defer wg.Done()
			_, _, fresh, e := s.Accept(ctx, p, now)
			if e != nil {
				failures <- e
				return
			}
			if fresh {
				owners.Add(1)
			}
		}(store)
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		require.NoError(t, failure)
	}
	require.EqualValues(t, 1, owners.Load())
	a, ok, err := j.Claim(ctx, now)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, j.Finish(ctx, a, Outcome{State: Confirmed}, now, now))
	otherGID, err := engine.NewPostgres(ctx, pg.ConnectionString(), "two")
	require.NoError(t, err)
	defer otherGID.Close()
	isolated, err := NewJournal(ctx, otherGID, DefaultLimits())
	require.NoError(t, err)
	_, _, fresh, err := isolated.Accept(ctx, p, now)
	require.NoError(t, err)
	require.True(t, fresh)

	t.Run("concurrent-claim-and-read-only-idle", func(t *testing.T) {
		firstDB, err := engine.NewPostgres(ctx, pg.ConnectionString(), "claim-retry")
		require.NoError(t, err)
		defer firstDB.Close()
		secondDB, err := engine.NewPostgres(ctx, pg.ConnectionString(), "claim-retry")
		require.NoError(t, err)
		defer secondDB.Close()
		first, err := NewJournal(ctx, firstDB, DefaultLimits())
		require.NoError(t, err)
		second, err := NewJournal(ctx, secondDB, DefaultLimits())
		require.NoError(t, err)
		p := plan(2002, "ban")
		p.Destination, p.Learn = 0, false
		accept(t, first, p, now)
		start := make(chan struct{})
		results := make(chan claimReadResult, 8)
		var wg sync.WaitGroup
		for i := range 8 {
			store := first
			if i%2 == 0 {
				store = second
			}
			wg.Go(func() {
				<-start
				a, ok, err := store.Claim(ctx, now)
				results <- claimReadResult{a, ok, err}
			})
		}
		close(start)
		wg.Wait()
		close(results)
		winners := 0
		owner := ""
		for result := range results {
			require.NoError(t, result.err)
			if result.ok {
				winners++
				owner = result.a.Owner
				require.NotEmpty(t, owner)
				require.Equal(t, 1, result.a.Attempts)
			}
		}
		require.Equal(t, 1, winners)
		var stored Action
		require.NoError(t, firstDB.Get(&stored,
			firstDB.Adopt(`SELECT attempts,owner,state FROM moderation_actions WHERE gid=?`), firstDB.GID()))
		require.Equal(t, 1, stored.Attempts)
		require.Equal(t, owner, stored.Owner)
		require.Equal(t, Running, stored.State)
		var before, after int
		require.NoError(t, firstDB.Get(&before,
			firstDB.Adopt(`SELECT version FROM moderation_control WHERE gid=?`), firstDB.GID()))
		for range 3 {
			a, ok, err := second.Claim(ctx, now)
			require.NoError(t, err)
			require.False(t, ok)
			require.Equal(t, Action{}, a)
		}
		require.NoError(t, firstDB.Get(&after,
			firstDB.Adopt(`SELECT version FROM moderation_control WHERE gid=?`), firstDB.GID()))
		require.Equal(t, before, after, "idle PostgreSQL polling does not lock/update the control row")
	})
}

func TestAutomaticAcceptanceSurvivesCrashBeforeHandlerReturns(t *testing.T) {
	j, db := testJournal(t, DefaultLimits())
	now := time.Now()
	p := plan(901, "ban")
	p.Destination = 0
	p.Learn = false
	_, owner, fresh, err := j.Accept(testCtx, p, now)
	require.NoError(t, err)
	require.True(t, fresh)
	require.Empty(t, owner, "automatic jobs require no separate learning activation")
	restarted, err := NewJournal(testCtx, db, DefaultLimits())
	require.NoError(t, err)
	a, ok, err := restarted.Claim(testCtx, now)
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, a.Uncertain, "no external call declared before this claim")
}

func TestQueueCapacityAndMetadataFootprint(t *testing.T) {
	if testing.Short() {
		t.Skip("5000-row load model runs in full checks")
	}
	j, db := testJournal(t, DefaultLimits())
	now := time.Now()
	for i := range 100 {
		p := plan(10000 + i*100)
		p.Learn = false
		p.Actions = make([]Action, 50)
		for n := range p.Actions {
			p.Actions[n] = Action{
				Kind:         "cleanup_delete",
				ChatID:       p.ChatID,
				IdentityKind: p.IdentityKind,
				IdentityID:   p.IdentityID,
				MessageID:    p.MessageID + n,
			}
		}
		_, _, fresh, err := j.Accept(testCtx, p, now)
		require.NoError(t, err)
		require.True(t, fresh)
	}
	_, _, _, err := j.Accept(testCtx, plan(99999, "ban"), now)
	require.ErrorIs(t, err, ErrCapacity)
	var pages, pageSize, actions int
	require.NoError(t, db.Get(&pages, "PRAGMA page_count"))
	require.NoError(t, db.Get(&pageSize, "PRAGMA page_size"))
	require.NoError(t, db.Get(&actions, "SELECT count(*) FROM moderation_actions WHERE kind!='notice'"))
	require.Equal(t, 5000, actions)
	t.Logf("5000 pending actions metadata: %d bytes (%d-byte SQLite pages)", pages*pageSize, pageSize)
}

func TestPostgresDeleteRequestContract(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL runs in full Linux checks")
	}
	ctx := context.Background()
	pg := containers.NewPostgresTestContainerWithDB(ctx, t, "delete_requests")
	defer pg.Close(ctx)
	db, err := engine.NewPostgres(ctx, pg.ConnectionString(), "delete-contract")
	require.NoError(t, err)
	defer db.Close()
	j, err := NewJournal(ctx, db, DefaultLimits())
	require.NoError(t, err)
	checkDeleteRequest(t, j, db)
	checkDeleteRenewalContract(t, j, db)
}

func TestPostgresHanPolicyContract(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL runs in full Linux checks")
	}
	ctx := context.Background()
	pg := containers.NewPostgresTestContainerWithDB(ctx, t, "han_policy")
	defer pg.Close(ctx)
	db, err := engine.NewPostgres(ctx, pg.ConnectionString(), "han-contract")
	require.NoError(t, err)
	defer db.Close()
	j, err := NewJournal(ctx, db, DefaultLimits())
	require.NoError(t, err)
	checkHanPolicyContract(t, j, db)
}
