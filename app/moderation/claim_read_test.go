package moderation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/storage/engine"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Observe real SELECT results; neither read errors nor rows are fabricated.
// A channel handshake can hold the return of the first real SQLITE_BUSY while
// the test releases its exclusive lock. This deterministically fails old Claim.
type claimReadProbe struct {
	enabled     atomic.Bool
	reads       atomic.Int32
	writes      atomic.Int32
	onRead      func(context.Context, error)
	onBegin     func(context.Context, error)
	beginError  error
	onCommit    func()
	onRollback  func(error)
	commitError error
}

type claimProbeConnector struct {
	driver *sqlite.Driver
	path   string
	probe  *claimReadProbe
}

func (c *claimProbeConnector) Driver() driver.Driver { return c.driver }

func (c *claimProbeConnector) Connect(_ context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.path)
	if err != nil {
		return nil, err
	}
	return &claimProbeConn{Conn: conn, probe: c.probe}, nil
}

type claimProbeConn struct {
	driver.Conn
	probe *claimReadProbe
}

func (c *claimProbeConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *claimProbeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if c.probe.enabled.Load() && strings.Contains(query, "SELECT count(*) FROM moderation_actions") &&
		strings.Contains(query, "moderation_requests") {
		c.probe.reads.Add(1)
		if c.probe.onRead != nil {
			c.probe.onRead(ctx, err)
		}
	}
	return rows, err
}

func (c *claimProbeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.probe.enabled.Load() {
		c.probe.writes.Add(1)
		if query == "COMMIT" && c.probe.commitError != nil {
			if c.probe.onCommit != nil {
				c.probe.onCommit()
			}
			return nil, c.probe.commitError
		}
	}
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if c.probe.enabled.Load() && query == "ROLLBACK" && c.probe.onRollback != nil {
		c.probe.onRollback(err)
	}
	if c.probe.enabled.Load() && query == "BEGIN IMMEDIATE" {
		if c.probe.onBegin != nil {
			c.probe.onBegin(ctx, err)
		}
		if err == nil && c.probe.beginError != nil {
			return nil, c.probe.beginError
		}
	}
	return result, err
}

func (c *claimProbeConn) CheckNamedValue(v *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(v)
	}
	return driver.ErrSkip
}

func claimReadFixture(t *testing.T) (*Journal, *engine.SQL, *sqlx.Conn, *claimReadProbe, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claim.db")
	db, err := engine.NewSqlite(path, "claim-read")
	require.NoError(t, err)
	j, err := NewJournal(testCtx, db, DefaultLimits())
	require.NoError(t, err)
	require.NoError(t, db.Close())
	probe := &claimReadProbe{}
	pool := sql.OpenDB(&claimProbeConnector{driver: &sqlite.Driver{}, path: path, probe: probe})
	db.DB = *sqlx.NewDb(pool, "sqlite")
	db.SetMaxOpenConns(1) // retain NewJournal's real per-handle pool configuration
	require.NoError(t, db.PingContext(testCtx))
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	holder, err := engine.NewSqlite(path, "claim-read")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, holder.Close()) })
	conn, err := holder.Connx(testCtx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = conn.ExecContext(testCtx, "ROLLBACK")
		require.NoError(t, conn.Close())
	})
	return j, db, conn, probe, path
}

func seedClaimRead(t *testing.T, j *Journal) time.Time {
	t.Helper()
	now := time.Now()
	p := plan(2001, "ban")
	p.Destination, p.Learn = 0, false
	accept(t, j, p, now)
	return now
}

type claimReadResult struct {
	a   Action
	ok  bool
	err error
}

func startClaimRead(ctx context.Context, j *Journal, now time.Time) <-chan claimReadResult {
	out := make(chan claimReadResult, 1)
	go func() {
		a, ok, err := j.Claim(ctx, now)
		out <- claimReadResult{a, ok, err}
	}()
	return out
}

func receiveClaimRead(t *testing.T, ch <-chan claimReadResult) claimReadResult {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(7 * time.Second):
		t.Fatal("Claim exceeded its five-second budget")
		return claimReadResult{}
	}
}

func assertUnclaimed(t *testing.T, db *engine.SQL, result claimReadResult) {
	t.Helper()
	require.Error(t, result.err)
	require.False(t, result.ok)
	require.Empty(t, result.a.Owner)
	require.Equal(t, Action{}, result.a)
	var stored Action
	require.NoError(t, db.Get(&stored, "SELECT id,state,attempts,owner,lease_until FROM moderation_actions"))
	require.Equal(t, Pending, stored.State)
	require.Zero(t, stored.Attempts)
	require.Empty(t, stored.Owner)
	require.Zero(t, stored.LeaseUntil)
}

func requireRealBusy(t *testing.T, err error) {
	t.Helper()
	var locked *sqlite.Error
	require.ErrorAs(t, err, &locked)
	require.Equal(t, sqlite3.SQLITE_BUSY, locked.Code()&0xff)
}

func TestClaimReadBusyRecovers(t *testing.T) {
	j, db, holder, probe, _ := claimReadFixture(t)
	now := seedClaimRead(t, j)
	_, err := holder.ExecContext(testCtx, "BEGIN EXCLUSIVE")
	require.NoError(t, err)
	seen, release := make(chan error, 1), make(chan struct{})
	var first atomic.Bool
	probe.onRead = func(ctx context.Context, err error) {
		if first.CompareAndSwap(false, true) {
			seen <- err
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	}
	probe.enabled.Store(true)
	ctx, cancel := context.WithTimeout(testCtx, 3*time.Second)
	defer cancel()
	result := startClaimRead(ctx, j, now)
	select {
	case err = <-seen:
		requireRealBusy(t, err)
	case <-ctx.Done():
		t.Fatal("preliminary SELECT never reached the real exclusive lock")
	}
	_, err = holder.ExecContext(testCtx, "ROLLBACK")
	require.NoError(t, err)
	close(release)
	r := receiveClaimRead(t, result)
	require.NoError(t, r.err)
	require.True(t, r.ok)
	require.NotEmpty(t, r.a.Owner)
	require.Equal(t, 1, r.a.Attempts)
	require.GreaterOrEqual(t, probe.reads.Load(), int32(2))
	var attempts int
	require.NoError(t, db.Get(&attempts, "SELECT attempts FROM moderation_actions"))
	require.Equal(t, 1, attempts)
}

func TestClaimReadDeadlineLeavesNoClaim(t *testing.T) {
	j, db, holder, probe, _ := claimReadFixture(t)
	now := seedClaimRead(t, j)
	before := map[string]any{}
	require.NoError(t, db.QueryRowx("SELECT * FROM moderation_actions").MapScan(before))
	_, err := holder.ExecContext(testCtx, "BEGIN EXCLUSIVE")
	require.NoError(t, err)
	probe.enabled.Store(true)
	ctx, cancel := context.WithTimeout(testCtx, 150*time.Millisecond)
	defer cancel()
	a, ok, err := j.Claim(ctx, now)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Greater(t, probe.reads.Load(), int32(1))
	_, unlockErr := holder.ExecContext(testCtx, "ROLLBACK")
	require.NoError(t, unlockErr)
	assertUnclaimed(t, db, claimReadResult{a, ok, err})
	after := map[string]any{}
	require.NoError(t, db.QueryRowx("SELECT * FROM moderation_actions").MapScan(after))
	require.Equal(t, before, after, "deadline exhaustion changes no attempts, deadlines or persisted ownership")
}

func TestClaimReadCancellationInterruptsWait(t *testing.T) {
	j, db, holder, probe, _ := claimReadFixture(t)
	now := seedClaimRead(t, j)
	_, err := holder.ExecContext(testCtx, "BEGIN EXCLUSIVE")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(testCtx)
	defer cancel()
	seen := make(chan error, 1)
	probe.onRead = func(_ context.Context, err error) {
		select {
		case seen <- err:
		default:
		}
	}
	probe.enabled.Store(true)
	result := startClaimRead(ctx, j, now)
	select {
	case err = <-seen:
		requireRealBusy(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("no locked read to cancel")
	}
	cancel()
	r := receiveClaimRead(t, result)
	require.ErrorIs(t, r.err, context.Canceled)
	_, err = holder.ExecContext(testCtx, "ROLLBACK")
	require.NoError(t, err)
	assertUnclaimed(t, db, r)
}

func TestClaimReadPermanentErrorIsNotRetried(t *testing.T) {
	j, db, _, probe, _ := claimReadFixture(t)
	now := seedClaimRead(t, j)
	_, err := db.Exec(`ALTER TABLE moderation_requests RENAME TO original_requests`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE VIEW moderation_requests AS
SELECT missing_locked_column AS gid,state,expires_at FROM original_requests`)
	require.NoError(t, err)
	probe.enabled.Store(true)
	ctx, cancel := context.WithTimeout(testCtx, 3*time.Second)
	defer cancel()
	a, ok, err := j.Claim(ctx, now)
	require.ErrorContains(t, err, "missing_locked_column")
	require.NoError(t, ctx.Err())
	require.EqualValues(t, 1, probe.reads.Load())
	require.Zero(t, probe.writes.Load())
	assertUnclaimed(t, db, claimReadResult{a, ok, err})
}

func TestClaimIdleReadDoesNotWrite(t *testing.T) {
	j, db, _, probe, path := claimReadFixture(t)
	var before, after int
	require.NoError(t, db.Get(&before, "SELECT total_changes()"))
	bytesBefore, err := os.ReadFile(path)
	require.NoError(t, err)
	probe.enabled.Store(true)
	for range 3 {
		a, ok, claimErr := j.Claim(testCtx, time.Now())
		require.NoError(t, claimErr)
		require.False(t, ok)
		require.Equal(t, Action{}, a)
	}
	require.Zero(t, probe.writes.Load())
	require.EqualValues(t, 3, probe.reads.Load())
	require.NoError(t, db.Get(&after, "SELECT total_changes()"))
	require.Equal(t, before, after)
	bytesAfter, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256(bytesBefore), sha256.Sum256(bytesAfter))
}

func TestClaimFailedCommitReturnsNoOwner(t *testing.T) {
	j, db, _, probe, _ := claimReadFixture(t)
	now := seedClaimRead(t, j)
	probe.commitError = errors.New("forced claim COMMIT failure")
	probe.enabled.Store(true)
	a, ok, err := j.Claim(testCtx, now)
	require.ErrorIs(t, err, probe.commitError)
	assertUnclaimed(t, db, claimReadResult{a, ok, err})
}

func TestClaimCancellationLeavesUsablePool(t *testing.T) {
	for _, phase := range []string{"begin", "commit"} {
		t.Run(phase, func(t *testing.T) {
			j, db, _, probe, _ := claimReadFixture(t)
			now := seedClaimRead(t, j)
			var before, version int64
			query := "SELECT version FROM moderation_control WHERE gid=?"
			require.NoError(t, db.Get(&before, query, db.GID()))
			ctx, cancel := context.WithCancel(testCtx)
			defer cancel()
			if phase == "begin" {
				// modernc stmt.exec may report ctx.Err() from its deferred interrupt
				// check after executing BEGIN. The file-backed BEGIN remains real.
				probe.onBegin = func(_ context.Context, err error) {
					require.NoError(t, err, "SQLite must actually begin before cancellation")
					cancel()
				}
				probe.beginError = context.Canceled
			} else {
				// Cancel after real claim/control writes but before COMMIT; verify
				// the existing cancellation-independent rollback is preserved too.
				probe.onCommit, probe.commitError = cancel, context.Canceled
			}
			probe.enabled.Store(true)
			a, ok, err := j.Claim(ctx, now)
			require.ErrorIs(t, err, context.Canceled)
			probe.enabled.Store(false)
			require.Zero(t, db.Stats().InUse, "failed claim must release its reserved connection")
			assertUnclaimed(t, db, claimReadResult{a, ok, err})
			require.NoError(t, db.Get(&version, query, db.GID()))
			require.Equal(t, before, version, "cancellation must leave no uncommitted control writes")

			// Reuse the same handle: exactly one subsequent owner and attempt.
			a, ok, err = j.Claim(testCtx, now)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, 1, a.Attempts)
			require.NotEmpty(t, a.Owner)
			require.NoError(t, db.Get(&version, query, db.GID()))
			require.Equal(t, before+1, version, "only the successful claim commits a control update")
			other, claimed, err := j.Claim(testCtx, now)
			require.NoError(t, err)
			require.False(t, claimed, "the same action cannot acquire a second owner")
			require.Empty(t, other.Owner)
			require.Zero(t, db.Stats().InUse)
		})
	}
}

type claimReadEvent struct {
	deadline time.Time
	bounded  bool
	err      error
	resume   chan struct{}
}

func pauseClaimProbe(ctx context.Context, events chan<- claimReadEvent, err error) {
	deadline, bounded := ctx.Deadline()
	event := claimReadEvent{deadline: deadline, bounded: bounded, err: err, resume: make(chan struct{})}
	select {
	case events <- event:
	case <-ctx.Done():
		return
	}
	select {
	case <-event.resume:
	case <-ctx.Done():
	}
}

func receiveClaimEvent(t *testing.T, events <-chan claimReadEvent) claimReadEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(7 * time.Second):
		t.Fatal("expected SQL phase was not reached")
		return claimReadEvent{}
	}
}

func TestClaimSharesReadAndTransactionBudget(t *testing.T) {
	for _, callerBudget := range []time.Duration{0, 2 * time.Second} {
		t.Run(callerBudget.String(), func(t *testing.T) {
			j, db, holder, probe, _ := claimReadFixture(t)
			now := seedClaimRead(t, j)
			_, err := holder.ExecContext(testCtx, "BEGIN EXCLUSIVE")
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(testCtx)
			defer cancel()
			if callerBudget > 0 {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, callerBudget)
				defer stop()
			}
			reads, begins := make(chan claimReadEvent, 1), make(chan claimReadEvent, 1)
			probe.onRead = func(ctx context.Context, err error) { pauseClaimProbe(ctx, reads, err) }
			probe.onBegin = func(ctx context.Context, err error) { pauseClaimProbe(ctx, begins, err) }
			probe.enabled.Store(true)
			result := startClaimRead(ctx, j, now)
			lockedRead := receiveClaimEvent(t, reads)
			requireRealBusy(t, lockedRead.err)
			require.True(t, lockedRead.bounded, "the initial read must already have the whole-operation deadline")
			require.False(t, lockedRead.deadline.After(time.Now().Add(5*time.Second)))
			if expected, ok := ctx.Deadline(); ok {
				require.True(t, expected.Equal(lockedRead.deadline), "respect a shorter caller budget")
			}
			_, err = holder.ExecContext(testCtx, "ROLLBACK")
			require.NoError(t, err)
			close(lockedRead.resume)
			read := receiveClaimEvent(t, reads)
			require.NoError(t, read.err)
			require.True(t, lockedRead.deadline.Equal(read.deadline), "read retries do not renew the budget")
			// A RESERVED lock permits this read but blocks the subsequent writer.
			_, err = holder.ExecContext(testCtx, "BEGIN IMMEDIATE")
			require.NoError(t, err)
			close(read.resume)
			begin := receiveClaimEvent(t, begins)
			requireRealBusy(t, begin.err)
			require.True(t, read.deadline.Equal(begin.deadline), "transaction inherits the exact read deadline")
			cancel()
			close(begin.resume)
			r := receiveClaimRead(t, result)
			require.ErrorIs(t, r.err, context.Canceled)
			_, err = holder.ExecContext(testCtx, "ROLLBACK")
			require.NoError(t, err)
			assertUnclaimed(t, db, r)
		})
	}
}
