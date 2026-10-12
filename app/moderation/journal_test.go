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

var testCtx = context.Background()

type executeFunc func(context.Context, Action) Outcome

func (f executeFunc) Execute(ctx context.Context, a Action) Outcome { return f(ctx, a) }

func testJournal(t *testing.T, limits Limits) (*Journal, *engine.SQL) {
	t.Helper()
	db, err := engine.NewSqlite(filepath.Join(t.TempDir(), "journal.db"), "test")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	j, err := NewJournal(testCtx, db, limits)
	require.NoError(t, err)
	return j, db
}

func plan(msg int, kinds ...string) Plan {
	p := Plan{
		Key:          fmt.Sprintf("source:-100:%d:user:7", msg),
		ChatID:       -100,
		MessageID:    msg,
		IdentityID:   7,
		IdentityKind: "user",
		Destination:  -900,
		Learn:        true,
	}
	for i, k := range kinds {
		id := msg
		if k == "cleanup_delete" {
			id = msg + i + 1
		}
		p.Actions = append(p.Actions, Action{Kind: k, ChatID: -100, IdentityID: 7, IdentityKind: "user", MessageID: id})
	}
	return p
}

func accept(t *testing.T, j *Journal, p Plan, now time.Time) string {
	t.Helper()
	id, owner, created, err := j.Accept(testCtx, p, now)
	require.NoError(t, err)
	require.True(t, created)
	if owner != "" {
		require.NoError(t, j.Activate(testCtx, id, owner, now, true))
	}
	return id
}

func drive(t *testing.T, w *Worker) {
	t.Helper()
	for range 200 {
		worked, err := w.Step(testCtx)
		require.NoError(t, err)
		if !worked {
			return
		}
	}
	t.Fatal("worker did not become idle")
}

func find(t *testing.T, j *Journal, kind string, msg int) Action {
	t.Helper()
	rows, err := j.Snapshot(testCtx)
	require.NoError(t, err)
	for _, a := range rows {
		if a.Kind == kind && a.MessageID == msg {
			return a
		}
	}
	t.Fatalf("missing %s:%d", kind, msg)
	return Action{}
}

func TestIndependentRecoveryAndPartialCleanup(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	accept(t, j, plan(10, "ban", "original_delete", "cleanup_delete", "cleanup_delete"), now)
	calls := map[string]int{}
	w := Worker{
		Journal: j,
		Now:     func() time.Time { return now },
		Jitter:  func() float64 { return 0 },
		Executor: executeFunc(func(_ context.Context, a Action) Outcome {
			if a.Kind == "notice" {
				return Outcome{State: Confirmed, NotificationID: 88}
			}
			calls[a.ID]++
			if a.Kind == "original_delete" && calls[a.ID] == 1 {
				return Outcome{Retry: true, Uncertain: true, Code: "transport"}
			}
			if a.Kind == "cleanup_delete" && a.MessageID == 13 {
				return Outcome{State: Failed, Code: "telegram_400"}
			}
			return Outcome{State: Confirmed}
		}),
	}
	drive(t, &w)
	require.Equal(t, Confirmed, find(t, j, "ban", 10).State)
	require.Equal(t, Pending, find(t, j, "original_delete", 10).State)
	require.Equal(t, Failed, find(t, j, "cleanup_delete", 13).State)
	require.Equal(t, Confirmed, find(t, j, "cleanup_delete", 14).State)
	now = now.Add(2 * time.Second)
	drive(t, &w)
	require.Equal(t, Confirmed, find(t, j, "original_delete", 10).State)
	require.Equal(t, 1, calls[find(t, j, "ban", 10).ID])
	require.Equal(t, 2, calls[find(t, j, "original_delete", 10).ID])
	require.Equal(t, 1, calls[find(t, j, "cleanup_delete", 13).ID])
	require.Equal(t, 1, calls[find(t, j, "cleanup_delete", 14).ID])
}

func TestPermanentDeleteDoesNotReplayConfirmedBan(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	accept(t, j, plan(20, "ban", "original_delete"), now)
	calls := map[string]int{}
	w := Worker{
		Journal: j,
		Now:     func() time.Time { return now },
		Executor: executeFunc(func(_ context.Context, a Action) Outcome {
			calls[a.Kind]++
			if a.Kind == "original_delete" {
				return Outcome{State: Failed, Code: "telegram_403"}
			}
			return Outcome{State: Confirmed, NotificationID: 9}
		}),
	}
	drive(t, &w)
	now = now.Add(time.Hour)
	drive(t, &w)
	require.Equal(t, 1, calls["ban"])
	require.Equal(t, 1, calls["original_delete"])
	require.Equal(t, Confirmed, find(t, j, "ban", 20).State)
}

func TestAtomicClaimsAcrossConnectionsAndFencedTakeover(t *testing.T) {
	j, db := testJournal(t, DefaultLimits())
	var files []struct {
		Name string `db:"name"`
		File string `db:"file"`
		Seq  int    `db:"seq"`
	}
	require.NoError(t, db.Select(&files, "PRAGMA database_list"))
	otherDB, err := engine.NewSqlite(files[0].File, "test")
	require.NoError(t, err)
	defer otherDB.Close()
	other, err := NewJournal(testCtx, otherDB, DefaultLimits())
	require.NoError(t, err)
	now := time.Now()
	p := plan(30, "ban")
	p.Destination = 0
	var won atomic.Int32
	failures := make(chan error, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := j
			if i%2 == 0 {
				store = other
			}
			id, owner, created, e := store.Accept(testCtx, p, now)
			if e != nil {
				failures <- e
				return
			}
			if created {
				won.Add(1)
				if activateErr := store.Activate(testCtx, id, owner, now, true); activateErr != nil {
					failures <- activateErr
				}
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		require.NoError(t, failure)
	}
	require.EqualValues(t, 1, won.Load())
	a, ok, err := j.Claim(testCtx, now)
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = other.Claim(testCtx, now)
	require.NoError(t, err)
	require.False(t, ok)
	_, ok, err = other.Claim(testCtx, now.Add(100*time.Second))
	require.NoError(t, err)
	require.False(t, ok, "takeover grace must cover bounded in-flight requests")
	b, ok, err := other.Claim(testCtx, now.Add(106*time.Second))
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, b.Uncertain)
	require.ErrorIs(t, j.Finish(testCtx, a, Outcome{State: Confirmed}, now, now), ErrLease)
	require.NoError(
		t,
		other.Finish(testCtx, b, Outcome{State: Confirmed, Code: "reconciled"}, now, now.Add(106*time.Second)),
	)
}

func TestRestartKeepsWorkAndDoesNotReplayLearning(t *testing.T) {
	j, db := testJournal(t, DefaultLimits())
	now := time.Now()
	p := plan(40, "ban", "original_delete")
	p.Destination = 0
	accept(t, j, p, now)
	a, ok, err := j.Claim(testCtx, now)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "ban", a.Kind)
	// Simulate a successful external call followed by death before Finish.
	restarted, err := NewJournal(testCtx, db, DefaultLimits())
	require.NoError(t, err)
	now = now.Add(106 * time.Second)
	seen := false
	w := Worker{
		Journal: restarted,
		Now:     func() time.Time { return now },
		Executor: executeFunc(func(_ context.Context, a Action) Outcome {
			if a.Kind == "ban" {
				seen = a.Uncertain
				return Outcome{State: Confirmed, Code: "reconciled_membership"}
			}
			return Outcome{State: Confirmed}
		}),
	}
	drive(t, &w)
	require.True(t, seen)
	require.Equal(t, Confirmed, find(t, j, "original_delete", 40).State)
	p = plan(41, "ban")
	p.Destination = -900
	_, _, created, err := j.Accept(testCtx, p, now)
	require.NoError(t, err)
	require.True(t, created)
	now = now.Add(106 * time.Second)
	drive(t, &w)
	require.Equal(t, Manual, find(t, j, "ban", 41).State, "uncertain classifier learning is not blindly rerun")
}

func TestDeleteDeadlineUnknownDateAndRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dateOffset time.Duration
		unknown    bool
		after      time.Duration
		expected   string
	}{
		{"boundary", -48*time.Hour + 2*time.Minute + 15*time.Second, false, 0, Expired},
		{"expired", -49 * time.Hour, false, 0, Expired},
		{"unknown", 0, true, 0, Confirmed},
		{"retry_after_exceeds_window", -47 * time.Hour, false, 2 * time.Hour, Expired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j, _ := testJournal(t, DefaultLimits())
			now := time.Now().Truncate(time.Second)
			p := plan(50, "original_delete")
			p.Destination = 0
			if !tc.unknown {
				p.Actions[0].MessageDate = now.Add(tc.dateOffset).Unix()
			}
			accept(t, j, p, now)
			calls := 0
			w := Worker{
				Journal: j,
				Now:     func() time.Time { return now },
				Executor: executeFunc(func(_ context.Context, a Action) Outcome {
					calls++
					if tc.after > 0 {
						return Outcome{Retry: true, Code: "telegram_429", RetryAfter: tc.after}
					}
					return Outcome{State: Confirmed}
				}),
			}
			drive(t, &w)
			a := find(t, j, "original_delete", 50)
			require.Equal(t, tc.expected, a.State)
			if tc.name == "boundary" || tc.name == "expired" {
				require.Zero(t, calls)
			}
			if tc.unknown {
				require.Zero(t, a.MessageDate)
				require.Equal(t, now.Add(24*time.Hour).UnixMilli(), a.Deadline)
			}
			if tc.after > 0 {
				until, err := j.Cooldown(testCtx)
				require.NoError(t, err)
				require.Equal(t, now.Add(tc.after).UnixMilli(), until.UnixMilli())
			}
		})
	}
}

func Test429CooldownIsDurableAndNeverShortened(t *testing.T) {
	j, db := testJournal(t, DefaultLimits())
	now := time.Now()
	p := plan(60, "ban", "original_delete")
	p.Destination = 0
	accept(t, j, p, now)
	calls := 0
	w := Worker{
		Journal: j,
		Now:     func() time.Time { return now },
		Jitter:  func() float64 { return 0 },
		Executor: executeFunc(func(_ context.Context, a Action) Outcome {
			calls++
			if calls == 1 {
				return Outcome{Retry: true, Code: "telegram_429", RetryAfter: 20 * time.Minute}
			}
			return Outcome{State: Confirmed}
		}),
	}
	_, err := w.Step(testCtx)
	require.NoError(t, err)
	a := find(t, j, "ban", 60)
	require.Equal(t, now.Add(20*time.Minute).UnixMilli(), a.NextAt)
	restarted, err := NewJournal(testCtx, db, DefaultLimits())
	require.NoError(t, err)
	w.Journal = restarted
	now = now.Add(19 * time.Minute)
	drive(t, &w)
	require.Equal(t, 1, calls)
	now = now.Add(time.Minute)
	drive(t, &w)
	require.Equal(t, 3, calls)
}

func TestSchedule320AttemptsAnd24HourHorizon(t *testing.T) {
	for _, jitter := range []float64{0, 1} {
		elapsed := time.Duration(0)
		attempts := 1
		for elapsed < 24*time.Hour && attempts < 320 {
			elapsed += Schedule(attempts, jitter, 0)
			attempts++
		}
		require.Less(t, attempts, 320, "the count must not cut the claimed 24-hour horizon short at either jitter extreme")
	}
	require.Equal(t, 3*time.Hour, Schedule(320, 0, 3*time.Hour), "retry_after is a lower bound, not capped at 10 minutes")
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	p := plan(70, "original_delete")
	p.Destination = 0
	accept(t, j, p, now)
	initial := now
	calls := 0
	w := Worker{
		Journal: j,
		Now:     func() time.Time { return now },
		Jitter:  func() float64 { return 0 },
		Executor: executeFunc(func(_ context.Context, a Action) Outcome {
			calls++
			now = now.Add(15 * time.Second)
			return Outcome{Retry: true, Uncertain: true, Code: "transport"}
		}),
	}
	for range 320 {
		worked, err := w.Step(testCtx)
		require.NoError(t, err)
		require.True(t, worked)
		a := find(t, j, "original_delete", 70)
		if a.State != Pending {
			require.Equal(t, Expired, a.State)
			require.Less(t, calls, 320)
			require.LessOrEqual(t, now.Sub(initial), 24*time.Hour)
			return
		}
		now = time.UnixMilli(a.NextAt)
	}
	t.Fatal("did not stop at horizon")
}

func TestDeliveryIdentityAndConfirmedPairs(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	p := plan(80, "original_delete")
	p.Destination = 0
	p.DeliveryChat = -900
	p.DeliveryMessage = 100
	id := accept(t, j, p, now)
	_, _, fresh, err := j.Accept(testCtx, p, now)
	require.NoError(t, err)
	require.False(t, fresh)
	p.DeliveryMessage = 101
	got, _, fresh, err := j.Accept(testCtx, p, now)
	require.NoError(t, err)
	require.False(t, fresh)
	require.Equal(t, id, got, "new forward with exact source attaches to source receipt")
	w := Worker{
		Journal:  j,
		Now:      func() time.Time { return now },
		Executor: executeFunc(func(context.Context, Action) Outcome { return Outcome{State: Confirmed} }),
	}
	drive(t, &w)
	p.Key = "delivery:-900:102"
	p.DeliveryMessage = 102
	accept(t, j, p, now)
	require.Equal(t, Confirmed, find(t, j, "original_delete", 80).State)
	// Same text is deliberately not an input. Different exact message IDs are independent.
	accept(t, j, plan(81, "original_delete"), now)
	require.Equal(t, Pending, find(t, j, "original_delete", 81).State)
	p = plan(82, "ban")
	p.Key = "delivery:-900:103"
	p.DeliveryChat = -900
	p.DeliveryMessage = 103
	accept(t, j, p, now)
	p.Key = "delivery:-900:104"
	p.DeliveryMessage = 104
	accept(t, j, p, now)
	var count int
	require.NoError(t, j.db.Get(&count, "SELECT count(*) FROM moderation_receipts"))
	require.Equal(t, 5, count, "new unknown-source forwards remain distinct")
}

func TestNotificationOutageCoalescingGenerationAndCapacity(t *testing.T) {
	j, _ := testJournal(t, Limits{Receipts: 20, Actions: 20})
	now := time.Now()
	for i := 1; i <= 20; i++ {
		accept(t, j, plan(100+i, "ban"), now)
	}
	_, _, _, err := j.Accept(testCtx, plan(999, "ban"), now)
	require.ErrorIs(t, err, ErrCapacity)
	rows, err := j.Snapshot(testCtx)
	require.NoError(t, err)
	notices := 0
	for _, a := range rows {
		if a.Kind == "notice" {
			notices++
		}
	}
	require.Equal(t, 1, notices)
	_, _, rejected, err := j.Digest(testCtx, -900)
	require.NoError(t, err)
	require.EqualValues(t, 1, rejected)
	// Notice sorts first; fail its delivery without losing any moderation jobs.
	w := Worker{
		Journal: j,
		Now:     func() time.Time { return now },
		Jitter:  func() float64 { return 0 },
		Executor: executeFunc(func(_ context.Context, a Action) Outcome {
			if a.Kind == "notice" {
				return Outcome{Retry: true, Uncertain: true, Code: "transport"}
			}
			return Outcome{State: Confirmed}
		}),
	}
	drive(t, &w)
	notice := find(t, j, "notice", 0)
	require.Equal(t, Pending, notice.State)
	require.GreaterOrEqual(t, notice.NextAt, now.Add(time.Minute).UnixMilli())
	for i := 1; i <= 20; i++ {
		require.Equal(t, Confirmed, find(t, j, "ban", 100+i).State)
	}
	now = now.Add(time.Minute)
	w.Executor = executeFunc(func(_ context.Context, a Action) Outcome {
		require.Equal(t, "notice", a.Kind)
		return Outcome{State: Confirmed, NotificationID: 555}
	})
	drive(t, &w)
	notice = find(t, j, "notice", 0)
	require.Equal(t, notice.Generation, notice.Delivered)
	require.Equal(t, 555, notice.NotificationID)
	// New desired generation while an old digest is in flight cannot be marked delivered.
	accept(t, j, plan(777, "ban"), now)
	now = now.Add(time.Minute)
	a, ok, err := j.Claim(testCtx, now)
	require.NoError(t, err)
	require.True(t, ok)
	if a.Kind != "notice" {
		require.NoError(t, j.Finish(testCtx, a, Outcome{State: Confirmed}, now, now))
		a, ok, err = j.Claim(testCtx, now)
		require.NoError(t, err)
		require.True(t, ok)
	}
	require.Equal(t, "notice", a.Kind)
	accept(t, j, plan(778, "ban"), now)
	require.NoError(t, j.Finish(testCtx, a, Outcome{State: Confirmed, NotificationID: 555}, now, now))
	notice = find(t, j, "notice", 0)
	require.Equal(t, Pending, notice.State)
	require.Greater(t, notice.Generation, notice.Delivered)
}

func TestUndeliveredTerminalNoticeSurvivesPruning(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	accept(t, j, plan(800, "ban"), now)
	w := Worker{
		Journal: j,
		Now:     func() time.Time { return now },
		Executor: executeFunc(func(_ context.Context, a Action) Outcome {
			if a.Kind == "notice" {
				return Outcome{State: Failed, Code: "telegram_403"}
			}
			return Outcome{State: Failed, Code: "telegram_400"}
		}),
	}
	drive(t, &w)
	now = now.Add(time.Minute)
	drive(t, &w)
	require.Equal(t, Undelivered, find(t, j, "notice", 0).State)
	require.NoError(t, j.Prune(testCtx, now.Add(40*24*time.Hour)))
	require.Equal(t, Failed, find(t, j, "ban", 800).State)
}
