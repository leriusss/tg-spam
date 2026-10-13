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
