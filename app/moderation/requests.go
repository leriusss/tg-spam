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
	if err := j.checkRequestRate(ctx, tx, p.RequesterID, p.DeleteOnly, now); err != nil {
		return err
	}
	if p.DeleteOnly {
		if err := j.exec(ctx, tx, `INSERT INTO moderation_delete_requests VALUES (?,?)`, j.db.GID(), p.Key); err != nil {
			return err
		}
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
	if rows[0].State == "expired" && (verb == "approve_report" || verb == "reject_report") {
		var closed int
		if e := tx.GetContext(ctx, &closed, j.db.Adopt(`SELECT count(*) FROM moderation_decisions WHERE gid=? AND receipt=? AND verb='close'`), j.db.GID(), c.Receipt); e != nil {
			return true, false, journalError(e)
		}
		if closed > 0 {
			return true, false, nil
		}
		return true, false, errors.New("confirmation request expired; no moderation authorized")
	}
	if rows[0].State != "pending" {
		if verb == "approve_report" || verb == "reject_report" || verb == "cancel" {
			return true, false, nil
		}
		return false, true, nil
	}
	if rows[0].Expires <= now.UnixMilli() {
		if verb != "close" {
			return true, false, errors.New("confirmation request expired; no moderation authorized")
		}
		if e := j.exec(ctx, tx, `UPDATE moderation_requests SET state='expired' WHERE gid=? AND receipt=?`, j.db.GID(), c.Receipt); e != nil {
			return true, false, e
		}
		return false, true, j.withdrawTX(ctx, tx, c.Receipt, "report", now)
	}
	if verb != "approve_report" && verb != "reject_report" && verb != "cancel" {
		return true, false, errors.New("request requires explicit approval or cancellation")
	}
	err = j.exec(ctx, tx, `UPDATE moderation_requests SET state=? WHERE gid=? AND receipt=?`, verb, j.db.GID(), c.Receipt)
	return true, true, err
}

// ClaimCommandReply persists one feedback slot per sender/target per minute across restarts.
func (j *Journal) ClaimCommandReply(ctx context.Context, sender int64, scope string, now time.Time) (bool, error) {
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
		result, e := tx.ExecContext(ctx, j.db.Adopt(`INSERT INTO moderation_command_replies VALUES (?,?,?,?) ON CONFLICT(gid,sender,scope) DO UPDATE SET next_at=excluded.next_at WHERE moderation_command_replies.next_at<=?`), j.db.GID(), sender, scope, now.Add(time.Minute).UnixMilli(), now.UnixMilli())
		if e != nil {
			return journalError(e)
		}
		n, e := result.RowsAffected()
		fresh = n == 1
		return journalError(e)
	})
	return fresh, err
}

// Count prior request epochs as well as current requests; renewal cannot evade rate limits.
func (j *Journal) checkRequestRate(ctx context.Context, tx journalTx, requester int64, deleteOnly bool, now time.Time) error {
	var total, sender int
	q := `SELECT count(*) FROM (SELECT requester,created_at FROM moderation_requests WHERE gid=? UNION ALL SELECT requester,created_at FROM moderation_delete_request_history WHERE gid=?) AS requests WHERE created_at>?`
	if e := tx.GetContext(ctx, &total, j.db.Adopt(q), j.db.GID(), j.db.GID(), now.Add(-time.Minute).UnixMilli()); e != nil {
		return journalError(e)
	}
	if e := tx.GetContext(ctx, &sender, j.db.Adopt(q+` AND requester=?`), j.db.GID(), j.db.GID(), now.Add(-time.Minute).UnixMilli(), requester); e != nil {
		return journalError(e)
	}
	senderLimit := 3
	if deleteOnly {
		senderLimit = 20
	}
	if total >= 20 || sender >= senderLimit {
		return ErrRequestLimit
	}
	return nil
}
