package moderation

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/storage/engine"
)

func pendingPlan(msg int) Plan {
	p := plan(msg, "original_delete", "cleanup_delete")
	p.Learn = false
	p.Card = true
	p.RequesterID = -777
	p.Origin = "report:sender:-777"
	p.SourceText = "immutable original"
	for i := range p.Actions {
		p.Actions[i].State = Manual
		p.Actions[i].Code = "sender_command_pending_confirmation"
	}
	return p
}

func TestPendingRequestExpiryCancellationAndLimits(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	j, _ := testJournal(t, DefaultLimits())
	p := pendingPlan(100)
	_, _, created, err := j.Accept(ctx, p, now)
	require.NoError(t, err)
	require.True(t, created)
	c, err := j.CardView(ctx, p.Key)
	require.NoError(t, err)
	fresh, err := j.Decide(ctx, "expired", c, "approve_report", 11, now.Add(15*time.Minute))
	require.ErrorContains(t, err, "expired")
	require.False(t, fresh)
	fresh, err = j.Decide(ctx, "cancel", c, "reject_report", 11, now)
	require.NoError(t, err)
	require.True(t, fresh)
	fresh, err = j.Decide(ctx, "later", c, "approve_report", 11, now)
	require.NoError(t, err)
	require.False(t, fresh)
	for i := 101; i < 103; i++ {
		_, _, _, err = j.Accept(ctx, pendingPlan(i), now)
		require.NoError(t, err)
	}
	_, _, _, err = j.Accept(ctx, pendingPlan(103), now)
	require.ErrorIs(t, err, ErrRequestLimit)
	_, _, created, err = j.Accept(ctx, p, now)
	require.NoError(t, err)
	require.False(t, created)
	fresh, err = j.ClaimCommandReply(ctx, -777, now)
	require.NoError(t, err)
	require.True(t, fresh)
	fresh, err = j.ClaimCommandReply(ctx, -777, now)
	require.NoError(t, err)
	require.False(t, fresh)
}

func TestPendingRequestGlobalRateAndConcurrentDecision(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	for i := range 20 {
		p := pendingPlan(100 + i)
		p.RequesterID = int64(-1000 - i)
		_, _, _, err := j.Accept(testCtx, p, now)
		require.NoError(t, err)
	}
	p := pendingPlan(200)
	p.RequesterID = -9000
	_, _, _, err := j.Accept(testCtx, p, now)
	require.ErrorIs(t, err, ErrRequestLimit)
	c, err := j.CardView(testCtx, pendingPlan(100).Key)
	require.NoError(t, err)
	var n atomic.Int32
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			fresh, e := j.Decide(testCtx, fmt.Sprint("decision", time.Now().UnixNano()), c, "approve_report", 11, now)
			require.NoError(t, e)
			if fresh {
				n.Add(1)
			}
		})
	}
	wg.Wait()
	require.EqualValues(t, 1, n.Load())
}

func TestPendingRequestReopenDoesNotReplayConfirmedActions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")
	db, err := engine.NewSqlite(path, "test")
	require.NoError(t, err)
	j, err := NewJournal(ctx, db, DefaultLimits())
	require.NoError(t, err)
	p := pendingPlan(300)
	now := time.Now()
	_, _, _, err = j.Accept(ctx, p, now)
	require.NoError(t, err)
	require.NoError(t, j.BindCard(ctx, p.Key, 600))
	fresh, err := j.ClaimCommandReply(ctx, -777, now)
	require.NoError(t, err)
	require.True(t, fresh)
	require.NoError(t, db.Close())
	db, err = engine.NewSqlite(path, "test")
	require.NoError(t, err)
	j, err = NewJournal(ctx, db, DefaultLimits())
	require.NoError(t, err)
	c, err := j.CardView(ctx, p.Key)
	require.NoError(t, err)
	require.Equal(t, "pending", c.RequestState)
	bound, err := j.CardBound(ctx, c, -900, 600)
	require.NoError(t, err)
	require.True(t, bound)
	fresh, err = j.ClaimCommandReply(ctx, -777, now)
	require.NoError(t, err)
	require.False(t, fresh)
	fresh, err = j.Decide(ctx, "decision", c, "approve_report", 11, now)
	require.NoError(t, err)
	require.True(t, fresh)
	require.NoError(t, j.EndDecision(ctx, "decision"))
	// mark API outcomes independently, as a worker would after typed acknowledgements.
	_, err = db.ExecContext(ctx, `UPDATE moderation_actions SET state='confirmed' WHERE kind NOT IN ('card','notice') AND state='pending'`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	db, err = engine.NewSqlite(path, "test")
	require.NoError(t, err)
	defer db.Close()
	j, err = NewJournal(ctx, db, DefaultLimits())
	require.NoError(t, err)
	_, _, created, err := j.Accept(ctx, p, now)
	require.NoError(t, err)
	require.False(t, created)
	c, err = j.CardView(ctx, p.Key)
	require.NoError(t, err)
	fresh, err = j.Decide(ctx, "redelivered callback", c, "approve_report", 11, now)
	require.NoError(t, err)
	require.False(t, fresh)
	var count int
	require.NoError(t, db.GetContext(ctx, &count, `SELECT count(*) FROM moderation_actions WHERE kind NOT IN ('card','notice') AND state='pending'`))
	require.Zero(t, count)
}

func TestPendingRequestExpiredClosureDoesNotAuthorizeActions(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	p := pendingPlan(400)
	_, _, _, err := j.Accept(testCtx, p, now)
	require.NoError(t, err)
	c, err := j.CardView(testCtx, p.Key)
	require.NoError(t, err)
	fresh, err := j.Decide(testCtx, "close expired", c, "close", 11, now.Add(16*time.Minute))
	require.NoError(t, err)
	require.True(t, fresh)
	require.NoError(t, j.EndDecision(testCtx, "close expired"))
	after, err := j.CardView(testCtx, p.Key)
	require.NoError(t, err)
	require.Equal(t, "expired", after.RequestState)
	for _, op := range after.Actions {
		require.NotEqual(t, Pending, op.State)
		require.NotEqual(t, Running, op.State)
		require.NotEqual(t, Confirmed, op.State)
	}
	fresh, err = j.Decide(testCtx, "late approve", after, "approve_report", 11, now.Add(17*time.Minute))
	require.NoError(t, err)
	require.False(t, fresh)
}
