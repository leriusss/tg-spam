package moderation

import (
	"context"
	"errors"
	"time"
)

// ForwardDelivery identifies the actual Telegram forward, independently of its
// candidate set. Both identifiers come from the authenticated listener update.
type ForwardDelivery struct {
	UpdateID  int   `db:"update_id"`
	ChatID    int64 `db:"chat_id"`
	MessageID int   `db:"message_id"`
}

// ForwardAdmission distinguishes replay, a new decision and a saved result view.
type ForwardAdmission struct {
	Receipt  string          `db:"receipt"`
	Operator int64           `db:"operator_id"`
	Delivery ForwardDelivery `db:"-"`
	Previous string          `db:"previous_receipt"`
	Reason   string          `db:"reason"`
	Fresh    bool            `db:"-"`
}

type forwardDeliveryRow struct {
	ForwardDelivery
	Receipt  string `db:"receipt"`
	Operator int64  `db:"operator_id"`
	Previous string `db:"previous_receipt"`
	Reason   string `db:"reason"`
}

// LookupForwardDelivery is a read-only replay check which also works after locator
// retention. A reused ID with a different operator or forward is not a new grant.
func (j *Journal) LookupForwardDelivery(ctx context.Context, d ForwardDelivery, operator int64) (ForwardAdmission, bool, error) {
	return j.lookupForwardDelivery(ctx, j.db, d, operator)
}

func (j *Journal) lookupForwardDelivery(
	ctx context.Context,
	tx journalTx,
	d ForwardDelivery,
	operator int64,
) (ForwardAdmission, bool, error) {
	if d.UpdateID <= 0 || d.ChatID == 0 || d.MessageID <= 0 || operator <= 0 {
		return ForwardAdmission{}, false, errors.New("missing Telegram forward delivery identity")
	}
	var rows []forwardDeliveryRow
	e := tx.SelectContext(
		ctx,
		&rows,
		j.db.Adopt(
			`SELECT update_id,chat_id,message_id,operator_id,receipt,previous_receipt,reason FROM moderation_forward_deliveries WHERE gid=? AND (update_id=? OR (chat_id=? AND message_id=?))`,
		),
		j.db.GID(),
		d.UpdateID,
		d.ChatID,
		d.MessageID,
	)
	if e != nil {
		return ForwardAdmission{}, false, journalError(e)
	}
	if len(rows) == 0 {
		return ForwardAdmission{}, false, nil
	}
	if len(rows) != 1 || rows[0].UpdateID != d.UpdateID || rows[0].ChatID != d.ChatID || rows[0].MessageID != d.MessageID ||
		rows[0].Operator != operator {
		return ForwardAdmission{}, false, errors.New("telegram delivery identity conflicts with saved forward")
	}
	r := rows[0]
	return ForwardAdmission{
		Receipt:  r.Receipt,
		Operator: r.Operator,
		Delivery: r.ForwardDelivery,
		Previous: r.Previous,
		Reason:   r.Reason,
	}, true, nil
}

// reusableForwardAction returns only facts or unresolved operations. A permanent,
// known-unsent refusal is eligible solely for a separate explicit decision.
func (j *Journal) reusableForwardAction(
	ctx context.Context,
	tx journalTx,
	wanted Action,
	now time.Time,
) (operation Action, reusable, review bool, err error) {
	var rows []Action
	q := `SELECT a.id,a.receipt,a.kind,a.chat_id,a.identity_id,a.identity_kind,a.message_id,a.message_date,a.until_date,a.state,a.code,a.attempts,a.deadline,a.next_at,a.owner,a.lease_until,a.uncertain,a.generation,a.delivered,a.notification_id FROM moderation_actions a WHERE a.gid=? AND a.chat_id=? AND `
	args := []any{j.db.GID(), wanted.ChatID}
	if deleteKind(wanted.Kind) {
		q += `a.message_id=? AND a.kind IN ('original_delete','cleanup_delete')`
		args = append(args, wanted.MessageID)
	} else {
		q += `a.kind=? AND a.identity_id=? AND a.identity_kind=? AND NOT EXISTS(SELECT 1 FROM moderation_actions undo WHERE undo.gid=a.gid AND undo.chat_id=a.chat_id AND undo.identity_id=a.identity_id AND undo.kind IN ('unban','restore') AND undo.state!='canceled' AND undo.next_at>=a.next_at)`
		args = append(args, wanted.Kind, wanted.IdentityID, wanted.IdentityKind)
	}
	q += ` ORDER BY CASE WHEN a.state='confirmed' AND a.uncertain=FALSE THEN 0 ELSE 1 END,a.next_at DESC,a.id`
	if e := tx.SelectContext(ctx, &rows, j.db.Adopt(q), args...); e != nil {
		return Action{}, false, false, journalError(e)
	}
	review = false
	var refused Action
	for _, a := range rows {
		// A temporary membership fact cannot satisfy a permanent request.
		if !deleteKind(a.Kind) && a.UntilDate != 0 && a.UntilDate < now.Add(366*24*time.Hour).Unix() {
			review = true
			continue
		}
		if a.State == Confirmed && !a.Uncertain || a.State == Pending || a.State == Running || a.Uncertain {
			return a, true, review, nil
		}
		review = true
		if refused.ID == "" {
			refused = a
		}
	}
	return refused, false, review, nil
}

func (j *Journal) readForwardLinks(ctx context.Context, tx journalTx, c *Card) error {
	var links []Action
	q := `SELECT a.id,a.receipt,a.kind,a.chat_id,a.identity_id,a.identity_kind,a.message_id,a.message_date,a.until_date,a.state,a.code,a.attempts,a.deadline,a.next_at,a.owner,a.lease_until,a.uncertain,a.generation,a.delivered,a.notification_id FROM moderation_actions a JOIN moderation_forward_links l ON l.gid=a.gid AND l.action_id=a.id WHERE l.gid=? AND l.receipt=? ORDER BY a.id`
	if e := tx.SelectContext(ctx, &links, j.db.Adopt(q), j.db.GID(), c.Receipt); e != nil {
		return journalError(e)
	}
	for _, a := range links {
		found := false
		for _, own := range c.Actions {
			found = found || own.ID == a.ID
		}
		if !found {
			if deleteKind(a.Kind) {
				a.Kind = "original_delete"
			}
			c.Actions = append(c.Actions, a)
		}
	}
	return nil
}
func (j *Journal) dirtyForwardLinks(ctx context.Context, tx journalTx, action string, now time.Time) error {
	var ids []string
	if e := tx.SelectContext(ctx, &ids, j.db.Adopt(`SELECT receipt FROM moderation_forward_links WHERE gid=? AND action_id=?`), j.db.GID(), action); e != nil {
		return journalError(e)
	}
	for _, id := range ids {
		if e := j.dirtyCard(ctx, tx, id, now); e != nil {
			return e
		}
	}
	return nil
}
