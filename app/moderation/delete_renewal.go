package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RenewDelete creates a new confirmation epoch, not a new original-delete operation.
// The old card, requester and expiry remain archived; the action's deadline is immutable.
func (j *Journal) RenewDelete(ctx context.Context, ref string, destination int64, notification int,
	operator int64, decision string, now time.Time) (bool, error) {
	if operator <= 0 || decision == "" || len(decision) > 256 {
		return false, errors.New("invalid explicit renewal decision")
	}
	fresh := false
	var rejection error
	err := j.transaction(ctx, func(tx journalTx) error {
		fresh = false
		rejection = nil
		var seen int
		decisionID := "delete-renew:" + decision
		if e := tx.GetContext(ctx, &seen, j.db.Adopt(`SELECT count(*) FROM moderation_decisions WHERE gid=? AND id=?`), j.db.GID(), decisionID); e != nil {
			return journalError(e)
		}
		if seen > 0 {
			return nil
		}
		var prior struct {
			Receipt           string `db:"receipt"`
			Requester         int64  `db:"requester"`
			Created           int64  `db:"created_at"`
			Expires           int64  `db:"expires_at"`
			State             string `db:"state"`
			NotificationState string `db:"notification_state"`
		}
		q := `SELECT c.receipt,r.requester,r.created_at,r.expires_at,r.state,a.state AS notification_state FROM moderation_cards c JOIN moderation_delete_requests d ON c.gid=d.gid AND c.receipt=d.receipt JOIN moderation_requests r ON c.gid=r.gid AND c.receipt=r.receipt JOIN moderation_actions a ON c.gid=a.gid AND c.receipt=a.receipt AND a.kind='card' WHERE c.gid=? AND c.ref=? AND c.destination=? AND a.chat_id=? AND a.notification_id=? AND a.notification_id>0`
		if e := tx.GetContext(ctx, &prior, j.db.Adopt(q), j.db.GID(), ref, destination, destination, notification); e != nil {
			return journalError(e)
		}
		if e := j.exec(ctx, tx, `INSERT INTO moderation_decisions VALUES (?,?,?,?,?,'done')`, j.db.GID(), decisionID, prior.Receipt, "renew_delete_noop", operator); e != nil {
			return e
		}
		if prior.NotificationState == Running {
			rejection = errors.New("card delivery is in progress")
			return nil
		}
		var action Action
		if e := tx.GetContext(ctx, &action, j.db.Adopt(`SELECT id,receipt,kind,chat_id,identity_id,identity_kind,message_id,message_date,until_date,state,code,attempts,deadline,next_at,owner,lease_until,uncertain,generation,delivered,notification_id FROM moderation_actions WHERE gid=? AND receipt=? AND kind='original_delete'`), j.db.GID(), prior.Receipt); e != nil {
			return journalError(e)
		}
		if action.Uncertain {
			rejection = errors.New("previous delete outcome requires manual review")
			return nil
		}
		var settledOrBusy int
		if e := tx.GetContext(ctx, &settledOrBusy, j.db.Adopt(`SELECT count(*) FROM moderation_actions WHERE gid=? AND chat_id=? AND message_id=? AND kind IN ('original_delete','cleanup_delete') AND state IN ('confirmed','absent','pending','running')`), j.db.GID(), action.ChatID, action.MessageID); e != nil {
			return journalError(e)
		}
		if settledOrBusy > 0 || prior.State == "pending" && prior.Expires > now.UnixMilli() && prior.Requester == operator {
			return nil
		}
		if e := j.checkRequestRate(ctx, tx, operator, true, now); e != nil {
			if errors.Is(e, ErrRequestLimit) {
				rejection = e
				return nil
			}
			return e
		}
		newRef := token()
		snapshot, e := json.Marshal(action)
		if e != nil {
			return fmt.Errorf("archive previous delete action: %w", e)
		}
		if e := j.exec(ctx, tx, `INSERT INTO moderation_delete_request_history VALUES (?,?,?,?,?,?,?,?,?,?)`, j.db.GID(), ref, prior.Receipt, prior.Requester, prior.Created, prior.Expires, prior.State, notification, newRef, string(snapshot)); e != nil {
			return e
		}
		if e := j.exec(ctx, tx, `UPDATE moderation_decisions SET verb='renew_delete' WHERE gid=? AND id=?`, j.db.GID(), decisionID); e != nil {
			return e
		}
		if e := j.exec(ctx, tx, `UPDATE moderation_requests SET requester=?,created_at=?,expires_at=?,state='pending' WHERE gid=? AND receipt=?`, operator, now.UnixMilli(), now.Add(15*time.Minute).UnixMilli(), j.db.GID(), prior.Receipt); e != nil {
			return e
		}
		if e := j.exec(ctx, tx, `UPDATE moderation_cards SET ref=? WHERE gid=? AND receipt=?`, newRef, j.db.GID(), prior.Receipt); e != nil {
			return e
		}
		if e := j.exec(ctx, tx, `UPDATE moderation_actions SET state='manual_review',code='delete_pending_confirmation' WHERE gid=? AND id=?`, j.db.GID(), action.ID); e != nil {
			return e
		}
		if e := j.exec(ctx, tx, `UPDATE moderation_demands SET active=FALSE WHERE gid=? AND receipt=? AND origin='delete_request'`, j.db.GID(), prior.Receipt); e != nil {
			return e
		}
		if e := j.exec(ctx, tx, `UPDATE moderation_actions SET notification_id=0,state='pending',code='',attempts=0,next_at=? WHERE gid=? AND receipt=? AND kind='card'`, now.UnixMilli(), j.db.GID(), prior.Receipt); e != nil {
			return e
		}
		fresh = true
		return j.dirtyCard(ctx, tx, prior.Receipt, now)
	})
	if err != nil {
		return false, err
	}
	return fresh, rejection
}

// SupersededDeleteCard permits a helpful stale-card response, never new authority.
func (j *Journal) SupersededDeleteCard(ctx context.Context, ref string) (bool, error) {
	var n int
	err := j.db.GetContext(ctx, &n, j.db.Adopt(`SELECT count(*) FROM moderation_delete_request_history WHERE gid=? AND ref=?`), j.db.GID(), ref)
	return n > 0, journalError(err)
}
