package moderation

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func validDeleteRequest(p Plan) bool {
	if p.Key != fmt.Sprintf("delete:%d:%d",
		p.ChatID,
		p.MessageID) ||
		p.RequesterID <= 0 ||
		p.Learn ||
		!p.Card ||
		p.Destination == 0 ||
		p.Origin != "delete_request" ||
		len(p.Actions) != 1 {
		return false
	}
	a := p.Actions[0]
	return a.Kind == "original_delete" &&
		a.MessageID == p.MessageID &&
		p.MessageID > 0 &&
		a.MessageDate >= 0 &&
		a.State == Manual &&
		a.Code == "delete_pending_confirmation"
}

// DeleteAuthority verifies a message-scoped operation without inventing an author.
func (j *Journal) DeleteAuthority(ctx context.Context, a Action) (bool, error) {
	var n int
	err := j.db.GetContext(ctx, &n, j.db.Adopt(`SELECT count(*) FROM moderation_delete_requests d JOIN moderation_actions a ON d.gid=a.gid AND d.receipt=a.receipt JOIN moderation_requests r ON d.gid=r.gid AND d.receipt=r.receipt WHERE d.gid=? AND a.id=? AND a.receipt=? AND a.chat_id=? AND a.message_id=? AND a.identity_kind=? AND a.identity_id=? AND a.kind='original_delete' AND r.state='delete'`), j.db.GID(), a.ID, a.Receipt, a.ChatID, a.MessageID, a.IdentityKind, a.IdentityID)
	return n == 1, journalError(err)
}

// DecideDelete binds authority to the persisted card, exact message and numeric requester.
// It has no learning/trust preparation and cannot schedule ban, restrict or cleanup.
func (j *Journal) DecideDelete(ctx context.Context,
	ref string,
	destination int64,
	notification int,
	operator int64,
	remove bool,
	now time.Time) (bool,
	error) {
	fresh := false
	err := j.transaction(ctx, func(tx journalTx) error {
		fresh = false
		var c struct {
			Receipt   string `db:"receipt"`
			Requester int64  `db:"requester"`
			State     string `db:"state"`
			Expires   int64  `db:"expires_at"`
		}
		q := `SELECT c.receipt,r.requester,r.state,r.expires_at FROM moderation_cards c JOIN moderation_delete_requests d ON c.gid=d.gid AND c.receipt=d.receipt JOIN moderation_requests r ON c.gid=r.gid AND c.receipt=r.receipt JOIN moderation_actions a ON c.gid=a.gid AND c.receipt=a.receipt AND a.kind='card' WHERE c.gid=? AND c.ref=? AND c.destination=? AND a.chat_id=? AND a.notification_id=? AND a.notification_id>0`
		if e := tx.GetContext(ctx, &c, j.db.Adopt(q), j.db.GID(), ref, destination, destination, notification); e != nil {
			return journalError(e)
		}
		if operator <= 0 || operator != c.Requester {
			return errors.New("delete operator does not match saved requester")
		}
		if c.State != "pending" {
			return nil
		}
		if remove && c.Expires <= now.UnixMilli() {
			return errors.New("delete request expired")
		}
		var a Action
		if e := tx.GetContext(ctx, &a, j.db.Adopt(`SELECT id,receipt,kind,chat_id,identity_id,identity_kind,message_id,message_date,until_date,state,code,attempts,deadline,next_at,owner,lease_until,uncertain,generation,delivered,notification_id FROM moderation_actions WHERE gid=? AND receipt=? AND kind='original_delete'`), j.db.GID(), c.Receipt); e != nil {
			return journalError(e)
		}

		if remove {
			var busy int
			if e := tx.GetContext(ctx, &busy, j.db.Adopt(`SELECT count(*) FROM moderation_actions WHERE gid=? AND chat_id=? AND message_id=? AND kind IN ('original_delete','cleanup_delete') AND state IN ('pending','running') AND id!=?`), j.db.GID(), a.ChatID, a.MessageID, a.ID); e != nil {
				return journalError(e)
			}
			if busy > 0 {
				return nil
			}
		}
		state := "dismiss"
		if remove {
			state = "delete"
		}
		if e := j.exec(ctx, tx, `INSERT INTO moderation_decisions VALUES (?,?,?,?,?,'done')`, j.db.GID(), "delete-only:"+ref, c.Receipt, state, operator); e != nil {
			return e
		}
		if e := j.exec(ctx, tx, `UPDATE moderation_requests SET state=? WHERE gid=? AND receipt=?`, state, j.db.GID(), c.Receipt); e != nil {
			return e
		}
		if remove && a.State == Manual {
			if e := j.exec(ctx, tx, `UPDATE moderation_demands SET active=TRUE,accepted_at=? WHERE gid=? AND receipt=? AND origin='delete_request'`, now.UnixMilli(), j.db.GID(), c.Receipt); e != nil {
				return e
			}
			if e := j.exec(ctx, tx, `UPDATE moderation_actions SET state='pending',code='',next_at=? WHERE gid=? AND id=?`, now.UnixMilli(), j.db.GID(), a.ID); e != nil {
				return e
			}
			if e := j.exec(ctx, tx, `UPDATE moderation_receipts SET state='ready' WHERE gid=? AND id=?`, j.db.GID(), c.Receipt); e != nil {
				return e
			}
		}
		if !remove {
			if e := j.exec(ctx, tx, `UPDATE moderation_demands SET active=FALSE WHERE gid=? AND receipt=?`, j.db.GID(), c.Receipt); e != nil {
				return e
			}
			if e := j.exec(ctx, tx, `UPDATE moderation_actions SET state='canceled',code='delete_request_dismissed' WHERE gid=? AND id=? AND state='manual_review'`, j.db.GID(), a.ID); e != nil {
				return e
			}
		}
		fresh = true
		return j.dirtyCard(ctx, tx, c.Receipt, now)
	})
	return fresh, err
}
