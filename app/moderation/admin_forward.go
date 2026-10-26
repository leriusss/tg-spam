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

// ForwardTarget is an immutable exact-original snapshot. Identity is independent
// of locator matching: an empty identity authorizes only the exact deletion.
type ForwardTarget struct {
	MessageID    int
	MessageDate  int64
	IdentityID   int64
	IdentityKind string
	Evidence     string
	BanKind      string
	Text         string
	Blocked      string
	Deadline     int64
}

// ForwardBatch stores an immutable candidate set and its one explicit decision.
type ForwardBatch struct {
	Requester   int64           `db:"requester"`
	Expires     int64           `db:"expires_at"`
	State       string          `db:"state"`
	RawTargets  string          `db:"targets"`
	RawSelected string          `db:"selected"`
	Targets     []ForwardTarget `db:"-"`
	Selected    []int           `db:"-"`
}

func (j *Journal) forwardBatch(ctx context.Context, tx journalTx, receipt string) (*ForwardBatch, error) {
	var rows []ForwardBatch
	if err := tx.SelectContext(ctx, &rows, j.db.Adopt(`SELECT requester,expires_at,state,targets,selected FROM moderation_forward_batches WHERE gid=? AND receipt=?`), j.db.GID(), receipt); err != nil {
		return nil, journalError(err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	b := rows[0]
	if err := json.Unmarshal([]byte(b.RawTargets), &b.Targets); err != nil {
		return nil, journalError(err)
	}
	if err := json.Unmarshal([]byte(b.RawSelected), &b.Selected); err != nil {
		return nil, journalError(err)
	}
	return &b, nil
}

// AcceptForward creates one persistent selection/result card. A unique original
// is an explicit decision already; ambiguous sets wait for a bound selection.
// Re-forwards of the same set cannot renew selection or deletion deadlines.
func (j *Journal) AcceptForward(
	ctx context.Context,
	chat, destination, operator int64,
	targets []ForwardTarget,
	now time.Time,
) (receipt string, fresh bool, err error) {
	if chat == 0 || destination == 0 || operator <= 0 || len(targets) == 0 || len(targets) > 20 {
		return "", false, errors.New("invalid forward scope")
	}
	targets = slices.Clone(targets)
	slices.SortFunc(targets, func(a, b ForwardTarget) int { return a.MessageID - b.MessageID })
	ids := []int{}
	for i, t := range targets {
		if t.MessageID <= 0 || i > 0 && t.MessageID == targets[i-1].MessageID || len(t.Text) > 32768 ||
			t.IdentityKind != "message" && t.IdentityKind != "user" && t.IdentityKind != "sender_chat" ||
			t.IdentityKind == "message" &&
				t.IdentityID != 0 || t.IdentityKind == "user" && t.IdentityID <= 0 || t.IdentityKind == "sender_chat" && t.IdentityID >= 0 ||
			(t.BanKind != "" && t.BanKind != "ban" && t.BanKind != "restrict") ||
			t.IdentityID != 0 && t.Evidence != "telegram_update" {
			return "", false, errors.New("invalid forward target evidence")
		}
		ids = append(ids, t.MessageID)
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%d:%v", chat, ids))
	receipt = "forward:" + hex.EncodeToString(sum[:])
	err = j.transaction(ctx, func(tx journalTx) error {
		fresh = false
		var prior int
		if e := tx.GetContext(ctx, &prior, j.db.Adopt(`SELECT count(*) FROM moderation_receipts WHERE gid=? AND id=?`), j.db.GID(), receipt); e != nil {
			return journalError(e)
		}
		if prior > 0 {
			return nil
		}
		var total, live, queued, rate int
		for _, query := range []struct {
			dst  *int
			q    string
			args []any
		}{
			{&total, `SELECT count(*) FROM moderation_receipts WHERE gid=?`, []any{j.db.GID()}},
			{&live, `SELECT count(*) FROM moderation_receipts WHERE gid=? AND state IN ('ready','preparing','preparing_optional')`, []any{j.db.GID()}},
			{&queued, `SELECT count(*) FROM moderation_actions WHERE gid=? AND state IN ('pending','running')`, []any{j.db.GID()}},
			{&rate, `SELECT count(*) FROM moderation_forward_batches b JOIN moderation_receipts r ON b.gid=r.gid AND b.receipt=r.id WHERE b.gid=? AND r.created_at>?`, []any{j.db.GID(), now.Add(-time.Minute).UnixMilli()}},
		} {
			if e := tx.GetContext(ctx, query.dst, j.db.Adopt(query.q), query.args...); e != nil {
				return journalError(e)
			}
		}
		if total >= j.limits.RetainedReceipts || live >= j.limits.Receipts || queued+len(targets)*2+1 > j.limits.Actions {
			return ErrCapacity
		}
		if rate >= 20 {
			return ErrRequestLimit
		}
		// Persist earliest known deadline independently of repeated forward arrival.
		for i := range targets {
			t := &targets[i]
			t.Deadline = now.Add(24 * time.Hour).UnixMilli()
			if t.MessageDate > 0 && t.MessageDate <= now.Unix() {
				t.Deadline = min(t.Deadline, time.Unix(t.MessageDate, 0).Add(48*time.Hour-2*time.Minute).UnixMilli())
			} else {
				t.MessageDate = 0
			}
			var deadlines []int64
			if e := tx.SelectContext(ctx, &deadlines, j.db.Adopt(`SELECT deadline FROM moderation_actions WHERE gid=? AND chat_id=? AND message_id=? AND kind IN ('original_delete','cleanup_delete')`), j.db.GID(), chat, t.MessageID); e != nil {
				return journalError(e)
			}
			for _, d := range deadlines {
				t.Deadline = min(t.Deadline, d)
			}
			// Include still-unselected sets, which have no destructive actions yet.
			var saved []string
			if e := tx.SelectContext(ctx, &saved, j.db.Adopt(`SELECT b.targets FROM moderation_forward_batches b JOIN moderation_receipts r ON r.gid=b.gid AND r.id=b.receipt WHERE b.gid=? AND r.chat_id=?`), j.db.GID(), chat); e != nil {
				return journalError(e)
			}
			for _, raw := range saved {
				var old []ForwardTarget
				if e := json.Unmarshal([]byte(raw), &old); e != nil {
					return journalError(e)
				}
				for _, x := range old {
					if x.MessageID == t.MessageID {
						t.Deadline = min(t.Deadline, x.Deadline)
					}
				}
			}
		}
		raw, e := json.Marshal(targets)
		if e != nil {
			return journalError(e)
		}
		if e := j.exec(ctx, tx, `INSERT INTO moderation_receipts VALUES (?,?,?,?,?,?,?,?,?,?,?)`, j.db.GID(), receipt, chat, 0, "message", targets[0].MessageID, destination, "ready", "", 0, now.UnixMilli()); e != nil {
			return journalError(e)
		}
		if e := j.exec(ctx, tx, `INSERT INTO moderation_forward_batches VALUES (?,?,?,?,?,?,?)`, j.db.GID(), receipt, operator, now.Add(15*time.Minute).UnixMilli(), "pending", string(raw), "[]"); e != nil {
			return journalError(e)
		}
		p := Plan{
			Key:          receipt,
			ChatID:       chat,
			IdentityKind: "message",
			MessageID:    targets[0].MessageID,
			Destination:  destination,
			Card:         true,
			SourceText:   targets[0].Text,
			Origin:       "admin_forward",
		}
		if e := j.registerPlan(ctx, tx, p, now); e != nil {
			return journalError(e)
		}
		if len(targets) == 1 {
			b, e := j.forwardBatch(ctx, tx, receipt)
			if e != nil {
				return journalError(e)
			}
			if e := j.selectForwardTX(ctx, tx, receipt, chat, b, []int{0}, operator, now); e != nil {
				return journalError(e)
			}
		}
		fresh = true
		return nil
	})
	return receipt, fresh, err
}

// DecideForward verifies SQL-bound card, original requester, frozen set and expiry.
// It is atomic with action admission: partial selection can never escape a rollback.
func (j *Journal) DecideForward(
	ctx context.Context,
	ref string,
	destination int64,
	notification int,
	operator int64,
	selected []int,
	now time.Time,
) (fresh bool, err error) {
	err = j.transaction(ctx, func(tx journalTx) error {
		fresh = false
		var c Card
		if e := tx.GetContext(ctx, &c, j.db.Adopt(`SELECT c.receipt,c.chat_id FROM moderation_cards c JOIN moderation_actions a ON a.gid=c.gid AND a.receipt=c.receipt AND a.kind='card' WHERE c.gid=? AND c.ref=? AND c.destination=? AND a.chat_id=? AND a.notification_id=? AND a.notification_id>0`), j.db.GID(), ref, destination, destination, notification); e != nil {
			return journalError(e)
		}
		b, e := j.forwardBatch(ctx, tx, c.Receipt)
		if e != nil {
			return journalError(e)
		}
		if b == nil || operator <= 0 || b.Requester != operator {
			return errors.New("forward requester mismatch")
		}
		if b.State != "pending" {
			return nil
		}
		if b.Expires <= now.UnixMilli() {
			return errors.New("forward selection expired")
		}
		if e := j.selectForwardTX(ctx, tx, c.Receipt, c.ChatID, b, selected, operator, now); e != nil {
			return journalError(e)
		}
		fresh = true
		return nil
	})
	return fresh, err
}

func (j *Journal) selectForwardTX(
	ctx context.Context,
	tx journalTx,
	receipt string,
	chat int64,
	b *ForwardBatch,
	selected []int,
	operator int64,
	now time.Time,
) error {
	if len(selected) == 0 || len(selected) > len(b.Targets) {
		return errors.New("invalid forward selection")
	}
	seen := map[int]bool{}
	bans := map[string]bool{}
	var queued int
	if e := tx.GetContext(ctx, &queued, j.db.Adopt(`SELECT count(*) FROM moderation_actions WHERE gid=? AND state IN ('pending','running')`), j.db.GID()); e != nil {
		return journalError(e)
	}
	if queued+2*len(selected) > j.limits.Actions {
		return ErrCapacity
	}
	for _, i := range selected {
		if i < 0 || i >= len(b.Targets) || seen[i] {
			return errors.New("selection outside saved originals")
		}
		seen[i] = true
		t := b.Targets[i]
		del := Action{
			ID:           fmt.Sprintf("%s:delete:%d", receipt, t.MessageID),
			Receipt:      receipt,
			Kind:         "original_delete",
			ChatID:       chat,
			IdentityID:   t.IdentityID,
			IdentityKind: t.IdentityKind,
			MessageID:    t.MessageID,
			MessageDate:  t.MessageDate,
			Deadline:     t.Deadline,
		}
		actions := []Action{del}
		banKey := fmt.Sprintf("%s:%d", t.IdentityKind, t.IdentityID)
		if t.IdentityID != 0 && !bans[banKey] {
			bans[banKey] = true
			kind := "ban"
			if t.BanKind == "restrict" && t.IdentityKind == "user" {
				kind = "restrict"
			}
			// Ban policy is frozen with the request; SenderChat always uses a sender-chat ban.
			actions = append(
				actions,
				Action{
					ID:           receipt + ":" + banKey,
					Receipt:      receipt,
					Kind:         kind,
					ChatID:       chat,
					IdentityID:   t.IdentityID,
					IdentityKind: t.IdentityKind,
					Deadline:     now.Add(24 * time.Hour).UnixMilli(),
				},
			)
		}
		for _, a := range actions {
			a.State = Pending
			a.NextAt = now.UnixMilli()
			if t.Blocked != "" {
				a.State = Blocked
				a.Code = t.Blocked
			}
			if a.Kind == "original_delete" {
				var done int
				if e := tx.GetContext(ctx, &done, j.db.Adopt(`SELECT count(*) FROM moderation_actions WHERE gid=? AND chat_id=? AND message_id=? AND kind IN ('original_delete','cleanup_delete') AND state='confirmed'`), j.db.GID(), chat, t.MessageID); e != nil {
					return journalError(e)
				}
				if done > 0 {
					a.State = Confirmed
					a.Code = "previously_confirmed_pair"
				}
			}
			if e := j.insertAction(ctx, tx, a); e != nil {
				return journalError(e)
			}
			if e := j.exec(ctx, tx, `INSERT INTO moderation_demands VALUES (?,?,?, ?,TRUE,?)`, j.db.GID(), receipt, a.ID, "admin_forward", now.UnixMilli()); e != nil {
				return journalError(e)
			}
		}
	}
	raw, e := json.Marshal(selected)
	if e != nil {
		return journalError(e)
	}
	if e := j.exec(ctx, tx, `UPDATE moderation_forward_batches SET state='accepted',selected=? WHERE gid=? AND receipt=?`, string(raw), j.db.GID(), receipt); e != nil {
		return journalError(e)
	}
	if e := j.exec(ctx, tx, `INSERT INTO moderation_decisions VALUES (?,?,?,?,?,'done')`, j.db.GID(), "forward:"+receipt, receipt, "forward_selected", operator); e != nil {
		return journalError(e)
	}
	if e := j.exec(ctx, tx, `UPDATE moderation_receipts SET state='ready' WHERE gid=? AND id=?`, j.db.GID(), receipt); e != nil {
		return journalError(e)
	}
	return j.dirtyCard(ctx, tx, receipt, now)
}

func (j *Journal) forwardDeleteAuthority(ctx context.Context, a Action) (bool, error) {
	var n int
	err := j.db.GetContext(
		ctx,
		&n,
		j.db.Adopt(
			`SELECT count(*) FROM moderation_forward_batches b JOIN moderation_actions a ON a.gid=b.gid AND a.receipt=b.receipt WHERE b.gid=? AND b.state='accepted' AND a.id=? AND a.receipt=? AND a.kind='original_delete' AND a.chat_id=? AND a.message_id=? AND a.identity_kind='message' AND a.identity_id=0`,
		),
		j.db.GID(),
		a.ID,
		a.Receipt,
		a.ChatID,
		a.MessageID,
	)
	return n == 1, journalError(err)
}

func (j *Journal) expireForwardTX(ctx context.Context, tx journalTx, now time.Time) error {
	for _, table := range []string{"moderation_forward_batches", "moderation_requests"} {
		var receipts []string
		query := "SELECT receipt FROM " + table + " WHERE gid=? AND state='pending' AND expires_at<=?"
		if e := tx.SelectContext(ctx, &receipts, j.db.Adopt(query), j.db.GID(), now.UnixMilli()); e != nil {
			return journalError(e)
		}
		for _, id := range receipts {
			if e := j.exec(ctx, tx, "UPDATE "+table+" SET state='expired' WHERE gid=? AND receipt=?", j.db.GID(), id); e != nil {
				return e
			}
			if e := j.dirtyCard(ctx, tx, id, now); e != nil {
				return e
			}
		}
	}
	return nil
}

// ForwardRequester returns numeric authorization provenance only for a new forward batch.
func (j *Journal) ForwardRequester(ctx context.Context, receipt string) (int64, error) {
	var ids []int64
	err := j.db.SelectContext(
		ctx,
		&ids,
		j.db.Adopt(`SELECT requester FROM moderation_forward_batches WHERE gid=? AND receipt=? AND state='accepted'`),
		j.db.GID(),
		receipt,
	)
	if err != nil {
		return 0, journalError(err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return ids[0], nil
}
