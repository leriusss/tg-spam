package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SharedReport contains one bot's search, never another bot's authority or token.
type SharedReport struct {
	Peer, ChatID, Operator                    int64
	Name, Receipt, State, Reason, SearchError string
	Delivery                                  ForwardDelivery
	Targets                                   []ForwardTarget
	Actions                                   []Action
}

// EnableSharedOutbox adds an opt-in durable handoff alongside the local journal.
func (j *Journal) EnableSharedOutbox(ctx context.Context) error {
	_, err := j.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS moderation_shared_outbox (
 gid TEXT NOT NULL, id TEXT NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(gid,id))`)
	return journalError(err)
}

func (j *Journal) saveSharedOutbox(ctx context.Context, tx journalTx, r SharedReport) error {
	var n int
	if err := tx.GetContext(ctx, &n, j.db.Adopt(`SELECT count(*) FROM moderation_shared_outbox WHERE gid=?`), j.db.GID()); err != nil {
		return journalError(err)
	}
	id := fmt.Sprintf("%d:%d", r.Delivery.ChatID, r.Delivery.MessageID)
	var exists int
	if err := tx.GetContext(ctx, &exists, j.db.Adopt(`SELECT count(*) FROM moderation_shared_outbox WHERE gid=? AND id=?`), j.db.GID(), id); err != nil {
		return journalError(err)
	}
	if n >= 500 && exists == 0 {
		return ErrCapacity
	}
	// The authoritative target snapshot is loaded from the local receipt on export.
	r.Targets, r.Actions = nil, nil
	raw, err := json.Marshal(r)
	if err != nil {
		return journalError(err)
	}
	return j.exec(ctx, tx, `INSERT INTO moderation_shared_outbox VALUES (?,?,?) ON CONFLICT(gid,id) DO NOTHING`, j.db.GID(), id, string(raw))
}

// AcceptSharedForward never auto-selects and never schedules a local card.
// Receipt and outbox commit together, including deliveries with no matches.
func (j *Journal) AcceptSharedForward(ctx context.Context, r SharedReport, now time.Time) error {
	if r.Peer <= 0 || r.ChatID == 0 || r.Operator <= 0 || r.Delivery.ChatID == 0 ||
		r.Delivery.UpdateID <= 0 || r.Delivery.MessageID <= 0 || len(r.Name) > 160 {
		return errors.New("invalid shared delivery")
	}
	if len(r.Targets) == 0 {
		return j.transaction(ctx, func(tx journalTx) error { return j.saveSharedOutbox(ctx, tx, r) })
	}
	_, err := j.acceptForward(ctx, r.ChatID, r.Delivery.ChatID, r.Operator, r.Delivery, r.Targets, now, &r)
	return err
}

// SharedOutbox reads a bounded immutable handoff and current local outcomes.
func (j *Journal) SharedOutbox(ctx context.Context) ([]SharedReport, error) {
	var raw []string
	if err := j.db.SelectContext(ctx, &raw, j.db.Adopt(`SELECT payload FROM moderation_shared_outbox WHERE gid=? ORDER BY id LIMIT 500`), j.db.GID()); err != nil {
		return nil, journalError(err)
	}
	out := make([]SharedReport, 0, len(raw))
	for _, s := range raw {
		var r SharedReport
		if err := json.Unmarshal([]byte(s), &r); err != nil {
			return nil, journalError(err)
		}
		if r.Receipt != "" {
			c, err := j.ReadCardView(ctx, r.Receipt)
			if err != nil {
				return nil, err
			}
			if c.Forward == nil {
				return nil, errors.New("shared forward snapshot missing")
			}
			r.Targets, r.Actions, r.State, r.Reason = c.Forward.Targets, c.Actions, c.Forward.State, c.Forward.Reason
			// Quotes and unrelated technical action fields are unnecessary in the shared DB.
			for i := range r.Targets {
				r.Targets[i].Text = ""
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// DecideSharedForward imports a frozen coordinator decision into this bot only.
// The existing worker rechecks roles and safety gates immediately before effects.
func (j *Journal) DecideSharedForward(ctx context.Context, r SharedReport, operator int64, selected []int, now time.Time) error {
	return j.transaction(ctx, func(tx journalTx) error {
		b, err := j.forwardBatch(ctx, tx, r.Receipt)
		if err != nil {
			return err
		}
		if b == nil || b.Delivery != r.Delivery || r.ChatID == 0 || operator <= 0 {
			return errors.New("shared decision authority mismatch")
		}
		var chat int64
		if err = tx.GetContext(ctx, &chat, j.db.Adopt(`SELECT chat_id FROM moderation_receipts WHERE gid=? AND id=? AND destination=?`), j.db.GID(), r.Receipt, r.Delivery.ChatID); err != nil {
			return journalError(err)
		}
		if chat != r.ChatID {
			return errors.New("shared decision chat mismatch")
		}
		if b.State == "accepted" {
			return nil
		}
		if b.Requester != r.Operator {
			return errors.New("shared requester mismatch")
		}
		if b.State != "pending" || b.Expires <= now.UnixMilli() {
			return errors.New("shared local choice expired")
		}
		if updateErr := j.exec(ctx, tx, `UPDATE moderation_forward_batches SET requester=? WHERE gid=? AND receipt=?`, operator, j.db.GID(), r.Receipt); updateErr != nil {
			return updateErr
		}
		return j.selectForwardTX(ctx, tx, r.Receipt, chat, b, selected, operator, now)
	})
}
