package moderation

import (
	"context"
	"errors"
	"time"
)

// ErrRequestLimit rejects new requests before any side effects.
var ErrRequestLimit = errors.New("confirmation request rate limit reached")

func (j *Journal) admitRequest(ctx context.Context, tx journalTx, p Plan, now time.Time) error {
	if p.Learn || !p.Card || p.MessageID <= 0 {
		return errors.New("invalid pending request")
	}
	for _, a := range p.Actions {
		if a.State != Manual {
			return errors.New("request cannot schedule moderation")
		}
	}
	var total, sender int
	q := `SELECT count(*) FROM moderation_requests WHERE gid=? AND created_at>?`
	if err := tx.GetContext(ctx, &total, j.db.Adopt(q), j.db.GID(), now.Add(-time.Minute).UnixMilli()); err != nil {
		return journalError(err)
	}
	if err := tx.GetContext(ctx, &sender, j.db.Adopt(q+` AND requester=?`), j.db.GID(), now.Add(-time.Minute).UnixMilli(), p.RequesterID); err != nil {
		return journalError(err)
	}
	if total >= 20 || sender >= 3 {
		return ErrRequestLimit
	}
	return j.exec(ctx, tx, `INSERT INTO moderation_requests VALUES (?,?,?,?,?,?)`, j.db.GID(), p.Key, p.RequesterID, now.UnixMilli(), now.Add(15*time.Minute).UnixMilli(), "pending")
}

// IsRequest identifies confirmation authority retained alongside the existing journal card.
func (j *Journal) IsRequest(ctx context.Context, receipt string) (bool, error) {
	var n int
	err := j.db.GetContext(ctx, &n, j.db.Adopt(`SELECT count(*) FROM moderation_requests WHERE gid=? AND receipt=?`), j.db.GID(), receipt)
	return n > 0, journalError(err)
}

func (j *Journal) requestDecision(
	ctx context.Context, tx journalTx, c Card, verb string, now time.Time,
) (request, allowed bool, err error) {
	var rows []struct {
		State   string `db:"state"`
		Expires int64  `db:"expires_at"`
	}
	if err = tx.SelectContext(ctx, &rows, j.db.Adopt(`SELECT state,expires_at FROM moderation_requests WHERE gid=? AND receipt=?`), j.db.GID(), c.Receipt); err != nil {
		return false, false, journalError(err)
	}
	if len(rows) == 0 {
		return false, true, nil
	}
	if rows[0].State != "pending" {
		if verb == "approve_report" || verb == "reject_report" || verb == "cancel" {
			return true, false, nil
		}
		return false, true, nil
	}
	if rows[0].Expires <= now.UnixMilli() {
		return true, false, errors.New("confirmation request expired; no moderation authorized")
	}
	if verb != "approve_report" && verb != "reject_report" && verb != "cancel" {
		return true, false, errors.New("request requires explicit approval or cancellation")
	}
	err = j.exec(ctx, tx, `UPDATE moderation_requests SET state=? WHERE gid=? AND receipt=?`, verb, j.db.GID(), c.Receipt)
	return true, true, err
}

// ClaimCommandReply persists one feedback slot per sender per minute across restarts.
func (j *Journal) ClaimCommandReply(ctx context.Context, sender int64, now time.Time) (bool, error) {
	fresh := false
	err := j.transaction(ctx, func(tx journalTx) error {
		fresh = false
		if e := j.exec(ctx, tx, `DELETE FROM moderation_command_replies WHERE gid=? AND next_at<?`, j.db.GID(), now.Add(-time.Hour).UnixMilli()); e != nil {
			return e
		}
		var active int
		if e := tx.GetContext(ctx, &active, j.db.Adopt(`SELECT count(*) FROM moderation_command_replies WHERE gid=? AND next_at>?`), j.db.GID(), now.UnixMilli()); e != nil {
			return journalError(e)
		}
		if active >= 20 {
			return nil
		}
		result, e := tx.ExecContext(ctx, j.db.Adopt(`INSERT INTO moderation_command_replies VALUES (?,?,?) ON CONFLICT(gid,sender) DO UPDATE SET next_at=excluded.next_at WHERE moderation_command_replies.next_at<=?`), j.db.GID(), sender, now.Add(time.Minute).UnixMilli(), now.UnixMilli())
		if e != nil {
			return journalError(e)
		}
		n, e := result.RowsAffected()
		fresh = n == 1
		return journalError(e)
	})
	return fresh, err
}
