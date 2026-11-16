package moderation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/umputun/tg-spam/app/storage/engine"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// claimDue is only an idle-worker optimization, never permission to execute.
// Its caller supplies the same bounded context used by the atomic transaction.
func (j *Journal) claimDue(ctx context.Context, now time.Time) (int, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("claim availability read: %w", err)
		}
		var due int
		err := j.db.GetContext(ctx, &due, j.db.Adopt(`SELECT
 (SELECT count(*) FROM moderation_actions WHERE gid=? AND ((state='pending' AND next_at<=?) OR
(state='running' AND lease_until<?))) +
 (SELECT count(*) FROM moderation_receipts WHERE gid=? AND state IN ('preparing','preparing_optional') AND
lease_until<?) +
(SELECT count(*) FROM moderation_forward_batches WHERE gid=? AND state='pending' AND expires_at<=?) +
(SELECT count(*) FROM moderation_requests WHERE gid=? AND state='pending' AND expires_at<=?)`),
			j.db.GID(), now.UnixMilli(), now.Add(-15*time.Second).UnixMilli(), j.db.GID(),
			now.Add(-15*time.Second).UnixMilli(), j.db.GID(), now.UnixMilli(), j.db.GID(), now.UnixMilli())
		if ctx.Err() != nil {
			return 0, fmt.Errorf("claim availability read: %w", ctx.Err())
		}
		if err == nil {
			return due, nil
		}
		if j.db.Type() != engine.Sqlite || !sqliteClaimReadLocked(err) {
			return 0, fmt.Errorf("claim availability read: %w", err)
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("claim availability read: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func sqliteClaimReadLocked(err error) bool {
	var sqlErr *sqlite.Error
	if !errors.As(err, &sqlErr) {
		return false
	}
	// Extended SQLite result codes retain the primary code in the low byte.
	code := sqlErr.Code() & 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}
