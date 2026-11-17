package moderation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// ErrSharedReview means a refusal appeared after the coordinator's saved intent.
var ErrSharedReview = errors.New("shared decision needs fresh explicit review")

// ForwardAllPolicy marks a newly admitted explicit forward, never legacy work.
const ForwardAllPolicy = "forward_all_v1"

// SharedRefusal is a causal snapshot of a known terminal local operation.
type SharedRefusal struct {
	Key, Kind, Code string
	MessageID       int
	IdentityID      int64
}

// SharedReport contains one bot's search, never another bot's authority or token.
type SharedReport struct {
	Policy                                    string
	RequestedAt                               int64
	Peer, ChatID, Operator                    int64
	Expires                                   int64
	Name, Receipt, State, Reason, SearchError string
	Delivery                                  ForwardDelivery
	Targets                                   []ForwardTarget
	Actions                                   []Action
	Refusals                                  []SharedRefusal
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
	if r.Policy != ForwardAllPolicy {
		r.Refusals = nil
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return journalError(err)
	}
	return j.exec(ctx, tx, `INSERT INTO moderation_shared_outbox VALUES (?,?,?) ON CONFLICT(gid,id) DO NOTHING`, j.db.GID(), id, string(raw))
}

// AcceptSharedForward never auto-selects and never schedules a local card.
// Receipt and outbox commit together, including deliveries with no matches.
func (j *Journal) AcceptSharedForward(ctx context.Context, r SharedReport, now time.Time) error {
	if r.Policy != "" && r.Policy != ForwardAllPolicy {
		return errors.New("unsupported forward policy")
	}
	if r.Policy == ForwardAllPolicy {
		if r.RequestedAt <= 0 || r.RequestedAt > now.Add(5*time.Second).UnixMilli() {
			return errors.New("forward instruction timestamp unavailable or in the future")
		}
		r.Expires = min(now.Add(15*time.Minute).UnixMilli(), r.RequestedAt+int64(15*time.Minute/time.Millisecond))
	}
	if r.Peer <= 0 || r.ChatID == 0 || r.Operator <= 0 || r.Delivery.ChatID == 0 ||
		r.Delivery.UpdateID <= 0 || r.Delivery.MessageID <= 0 || len(r.Name) > 320 || len(r.SearchError) > 2000 {
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
			r.Expires = c.Forward.Expires
			if r.State == "pending" && r.Policy != ForwardAllPolicy {
				var e error
				r.Refusals, e = j.sharedRefusals(ctx, r, c.Forward.Targets)
				if e != nil {
					return nil, e
				}
			}
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
func (j *Journal) DecideSharedForward(ctx context.Context, r SharedReport, operator int64,
	selected []int, automatic bool, now time.Time) error {
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
		if r.Policy == ForwardAllPolicy {
			if err := j.forwardAllAuthority(ctx, tx, r, operator, now); err != nil {
				return err
			}
		}
		if b.State != "pending" || b.Expires <= now.UnixMilli() {
			return errors.New("shared local choice expired")
		}
		for _, index := range selected {
			if index < 0 || index >= len(b.Targets) {
				return errors.New("shared selection outside frozen local targets")
			}
			for _, wanted := range forwardActions(b.Targets[index], r.Receipt, chat, now) {
				prior, reused, review, e := j.reusableForwardAction(ctx, tx, wanted, now)
				if e != nil {
					return e
				}
				observed := slices.ContainsFunc(r.Refusals, func(f SharedRefusal) bool {
					return f.Key == sharedRefusalKey(prior, wanted.Kind)
				})
				if !reused && review && (automatic && r.Policy != ForwardAllPolicy || !observed) {
					return fmt.Errorf("%w: %s %s", ErrSharedReview, wanted.Kind, prior.Code)
				}
			}
		}
		if updateErr := j.exec(ctx, tx, `UPDATE moderation_forward_batches SET requester=? WHERE gid=? AND receipt=?`, operator, j.db.GID(), r.Receipt); updateErr != nil {
			return updateErr
		}
		return j.selectForwardTX(ctx, tx, r.Receipt, chat, b, selected, operator, now)
	})
}

func sharedRefusalKey(a Action, kind string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s:%s:%s:%s:%d:%d", kind, a.ID, a.State, a.Code, a.Attempts, a.Generation))
	return hex.EncodeToString(sum[:])
}

func (j *Journal) sharedRefusals(ctx context.Context, r SharedReport, targets []ForwardTarget) (out []SharedRefusal, err error) {
	err = j.readCardTransaction(ctx, func(tx journalTx) error {
		out, err = j.sharedRefusalsTX(ctx, tx, r, targets, time.Now())
		return err
	})
	return out, err
}

func (j *Journal) sharedRefusalsTX(ctx context.Context, tx journalTx, r SharedReport,
	targets []ForwardTarget, now time.Time) (out []SharedRefusal, err error) {
	seen := map[string]bool{}
	for i := range targets {
		t := &targets[i]
		for _, wanted := range forwardActions(*t, r.Receipt, r.ChatID, now) {
			prior, reused, review, e := j.reusableForwardAction(ctx, tx, wanted, now)
			if e != nil {
				return nil, e
			}
			confirmed := reused && prior.State == Confirmed && !prior.Uncertain
			if wanted.Kind == "original_delete" {
				t.DeleteConfirmed = confirmed
			} else {
				t.BanConfirmed = confirmed
			}
			if reused || !review {
				continue
			}
			key := sharedRefusalKey(prior, wanted.Kind)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, SharedRefusal{Key: key, Kind: wanted.Kind, Code: prior.Code,
				MessageID: wanted.MessageID, IdentityID: wanted.IdentityID})
		}
	}
	return out, nil
}

// A coordinator cannot promote a legacy receipt or import a later refusal as a
// fresh instruction. The original local intake is the authority for this policy.
func (j *Journal) forwardAllAuthority(ctx context.Context, tx journalTx, r SharedReport,
	operator int64, now time.Time) error {
	var raw string
	id := fmt.Sprintf("%d:%d", r.Delivery.ChatID, r.Delivery.MessageID)
	if err := tx.GetContext(ctx, &raw, j.db.Adopt(`SELECT payload FROM moderation_shared_outbox WHERE gid=? AND id=?`), j.db.GID(), id); err != nil {
		return journalError(err)
	}
	var saved SharedReport
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		return journalError(err)
	}
	if saved.Policy != ForwardAllPolicy || saved.Receipt != r.Receipt || saved.Delivery != r.Delivery ||
		saved.Operator != operator || operator != r.Operator || r.Expires <= now.UnixMilli() ||
		r.RequestedAt != saved.RequestedAt || r.Expires > saved.Expires ||
		!slices.Equal(saved.Refusals, r.Refusals) {
		return errors.New("forward instruction does not match immutable local intake")
	}
	return nil
}
