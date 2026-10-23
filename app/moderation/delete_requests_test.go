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

func deletePlan(mid int) Plan {
	p := plan(mid, "original_delete")
	p.DeleteOnly = true
	p.Key = fmt.Sprintf("delete:%d:%d", p.ChatID, mid)
	p.Learn = false
	p.Card = true
	p.RequesterID = 11
	p.Origin = "delete_request"
	p.Actions[0].State = Manual
	p.Actions[0].Code = "delete_pending_confirmation"
	p.Actions[0].MessageDate = time.Now().Unix()
	return p
}

func checkDeleteRequest(t *testing.T, j *Journal, db *engine.SQL) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	p := deletePlan(91001)
	accept(t, j, p, now)
	require.NoError(t, j.BindCard(ctx, p.Key, 600))
	c, err := j.CardView(ctx, p.Key)
	require.NoError(t, err)
	require.True(t, c.DeleteOnly)
	require.EqualValues(t, 11, c.RequesterID)
	for _, verb := range []string{"approve", "approve_report", "cancel", "unban", "restore"} {
		_, err = j.Decide(ctx, "forged:"+verb, c, verb, 11, now)
		require.Error(t, err)
	}
	for _, bad := range []struct {
		chat int64
		msg  int
		op   int64
	}{{-901, 600, 11}, {-900, 601, 11}, {-900, 600, 12}, {-900, 600, 0}} {
		_, err = j.DecideDelete(ctx, c.Ref, bad.chat, bad.msg, bad.op, true, now)
		require.Error(t, err)
	}
	_, err = j.DecideDelete(ctx, c.Ref, -900, 600, 11, true, now.Add(16*time.Minute))
	require.ErrorContains(t, err, "expired")
	var owners atomic.Int32
	var wg sync.WaitGroup
	fail := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			fresh, e := j.DecideDelete(ctx, c.Ref, -900, 600, 11, true, now)
			if e != nil {
				fail <- e
			}
			if fresh {
				owners.Add(1)
			}
		})
	}
	wg.Wait()
	close(fail)
	for e := range fail {
		require.NoError(t, e)
	}
	require.EqualValues(t, 1, owners.Load())
	restarted, err := NewJournal(ctx, db, DefaultLimits())
	require.NoError(t, err)
	calls := 0
	w := Worker{Journal: restarted, Now: func() time.Time { return now }, Executor: executeFunc(func(_ context.Context, a Action) Outcome {
		if a.Kind != "card" && a.Kind != "notice" {
			require.Equal(t, "original_delete", a.Kind)
			require.Equal(t, p.MessageID, a.MessageID)
			calls++
		}
		return Outcome{State: Confirmed}
	})}
	drive(t, &w)
	require.Equal(t, 1, calls)
	_, _, created, err := restarted.Accept(ctx, p, now)
	require.NoError(t, err)
	require.False(t, created)
	fresh, err := restarted.DecideDelete(ctx, c.Ref, -900, 600, 11, true, now)
	require.NoError(t, err)
	require.False(t, fresh)
	drive(t, &w)
	require.Equal(t, 1, calls)
}

func TestDeleteRequestBoundConcurrentRestart(t *testing.T) {
	j, db := testJournal(t, DefaultLimits())
	checkDeleteRequest(t, j, db)
}

func TestDeleteRequestUnknownAuthorAndInvalidPlans(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	p := deletePlan(91002)
	p.IdentityID = 0
	p.IdentityKind = "message"
	p.Actions[0].IdentityID = 0
	p.Actions[0].IdentityKind = "message"
	accept(t, j, p, now)
	c, err := j.CardView(testCtx, p.Key)
	require.NoError(t, err)
	require.Zero(t, c.IdentityID)
	for _, change := range []func(*Plan){func(p *Plan) { p.Learn = true }, func(p *Plan) { p.Actions[0].Kind = "ban" }, func(p *Plan) { p.Actions[0].State = Pending }, func(p *Plan) { p.RequesterID = 0 }, func(p *Plan) { p.Actions = append(p.Actions, p.Actions[0]) }, func(p *Plan) { p.DeleteOnly = false }} {
		bad := deletePlan(91003)
		bad.IdentityID = 0
		bad.IdentityKind = "message"
		bad.Actions[0].IdentityID = 0
		bad.Actions[0].IdentityKind = "message"
		change(&bad)
		_, _, _, err = j.Accept(testCtx, bad, now)
		require.Error(t, err)
	}
	require.NoError(t, j.BindCard(testCtx, p.Key, 601))
	_, err = j.DecideDelete(testCtx, c.Ref, -900, 601, 11, false, now.Add(16*time.Minute))
	require.NoError(t, err)
	after, err := j.CardView(testCtx, p.Key)
	require.NoError(t, err)
	require.Equal(t, Canceled, after.Actions[0].State)
}

func TestDeletePairSerializesIndependentReceipts(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	p := deletePlan(91004)
	p.IdentityID = 0
	p.IdentityKind = "message"
	p.Actions[0].IdentityID = 0
	p.Actions[0].IdentityKind = "message"
	accept(t, j, p, now)
	require.NoError(t, j.BindCard(testCtx, p.Key, 600))
	c, err := j.CardView(testCtx, p.Key)
	require.NoError(t, err)
	_, err = j.DecideDelete(testCtx, c.Ref, -900, 600, 11, true, now)
	require.NoError(t, err)
	other := plan(p.MessageID, "original_delete")
	other.Learn = false
	other.Origin = "detector"
	other.Destination = 0
	other.Actions[0].MessageDate = now.Unix()
	accept(t, j, other, now)
	first, ok, err := j.Claim(testCtx, now)
	require.NoError(t, err)
	require.True(t, ok)
	// Notifications may precede deletes; finish them until one exact delete is running.
	for first.Kind == "card" || first.Kind == "notice" {
		require.NoError(t, j.Finish(testCtx, first, Outcome{State: Confirmed}, now, now))
		first, ok, err = j.Claim(testCtx, now)
		require.NoError(t, err)
		require.True(t, ok)
	}
	for range 5 {
		a, claimed, e := j.Claim(testCtx, now)
		require.NoError(t, e)
		if !claimed {
			break
		}
		require.NotEqual(t, "original_delete", a.Kind)
		require.NoError(t, j.Finish(testCtx, a, Outcome{State: Confirmed}, now, now))
	}
	require.NoError(t, j.Finish(testCtx, first, Outcome{State: Confirmed}, now, now))
	calls := 0
	w := Worker{Journal: j, Now: func() time.Time { return now }, Executor: executeFunc(func(_ context.Context, a Action) Outcome {
		if a.Kind == "original_delete" {
			calls++
		}
		return Outcome{State: Confirmed}
	})}
	drive(t, &w)
	require.Zero(t, calls)
}
func TestDeleteRequestDoesNotWithdrawIndependentModeration(t *testing.T) {
	j, db := testJournal(t, DefaultLimits())
	now := time.Now()
	independent := plan(91005, "ban", "original_delete", "cleanup_delete")
	independent.Learn = false
	independent.Origin = "detector"
	independent.Card = true
	accept(t, j, independent, now)
	before, err := j.CardView(testCtx, independent.Key)
	require.NoError(t, err)
	p := deletePlan(91005)
	accept(t, j, p, now)
	require.NoError(t, j.BindCard(testCtx, p.Key, 600))
	c, err := j.CardView(testCtx, p.Key)
	require.NoError(t, err)
	_, err = j.DecideDelete(testCtx, c.Ref, -900, 600, 11, false, now)
	require.NoError(t, err)
	after, err := j.CardView(testCtx, independent.Key)
	require.NoError(t, err)
	require.Equal(t, before.Actions, after.Actions)
	require.Equal(t, before.Origins, after.Origins)
	var decisionCount int
	require.NoError(t, db.Get(&decisionCount, `SELECT count(*) FROM moderation_decisions WHERE verb IN ('approve','approve_report','unban','restore')`))
	require.Zero(t, decisionCount)
}

func TestDeleteRequestActualDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")
	now := time.Now()
	db, err := engine.NewSqlite(path, "reopen")
	require.NoError(t, err)
	j, err := NewJournal(ctx, db, DefaultLimits())
	require.NoError(t, err)
	p := deletePlan(91006)
	accept(t, j, p, now)
	require.NoError(t, j.BindCard(ctx, p.Key, 600))
	c, err := j.CardView(ctx, p.Key)
	require.NoError(t, err)
	_, err = j.DecideDelete(ctx, c.Ref, -900, 600, 11, true, now)
	require.NoError(t, err)
	calls := 0
	executor := executeFunc(func(_ context.Context, a Action) Outcome {
		if a.Kind == "original_delete" {
			calls++
		}
		return Outcome{State: Confirmed}
	})
	w := Worker{Journal: j, Executor: executor, Now: func() time.Time { return now }}
	drive(t, &w)
	require.Equal(t, 1, calls)
	require.NoError(t, db.Close())
	db, err = engine.NewSqlite(path, "reopen")
	require.NoError(t, err)
	defer db.Close()
	j, err = NewJournal(ctx, db, DefaultLimits())
	require.NoError(t, err)
	_, _, fresh, err := j.Accept(ctx, p, now)
	require.NoError(t, err)
	require.False(t, fresh)
	fresh, err = j.DecideDelete(ctx, c.Ref, -900, 600, 11, true, now)
	require.NoError(t, err)
	require.False(t, fresh)
	w.Journal = j
	drive(t, &w)
	require.Equal(t, 1, calls)
}

func TestDeleteRequestAdmissionWaitsWithinTransactionDeadline(t *testing.T) {
	j, db := testJournal(t, DefaultLimits())
	var path string
	require.NoError(t, db.Get(&path, `SELECT file FROM pragma_database_list WHERE name='main'`))
	blocker, err := engine.NewSqlite(path, "test")
	require.NoError(t, err)
	defer blocker.Close()
	blocker.SetMaxOpenConns(1)
	conn, err := blocker.Connx(testCtx)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(testCtx, "BEGIN IMMEDIATE")
	require.NoError(t, err)
	unlocked := make(chan error, 1)
	timer := time.AfterFunc(2*time.Second, func() { _, e := conn.ExecContext(testCtx, "COMMIT"); unlocked <- e })
	defer timer.Stop()
	started := time.Now()
	_, _, fresh, err := j.Accept(testCtx, deletePlan(91007), started)
	require.NoError(t, <-unlocked)
	require.NoError(t, err)
	require.True(t, fresh)
	require.GreaterOrEqual(t, time.Since(started), 2*time.Second)
	// The admission remains bounded by the original five-second transaction budget.
}
