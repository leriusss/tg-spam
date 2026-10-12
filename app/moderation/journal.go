package moderation

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/umputun/tg-spam/app/storage/engine"
)

// Journal owns receipts, action scheduling, delivery deduplication and coalesced notices.
type Journal struct {
	db     *engine.SQL
	limits Limits
}

// NewJournal adds isolated metadata tables without rewriting legacy application data.
func NewJournal(ctx context.Context, db *engine.SQL, limits Limits) (*Journal, error) {
	if db == nil || limits.Receipts < 1 || limits.Actions < 1 {
		return nil, errors.New("invalid moderation journal configuration")
	}
	if limits.RetainedReceipts <= 0 {
		limits.RetainedReceipts = 5000
	}
	j := &Journal{db: db, limits: limits}
	// Serialize shared SQLite connections with locator/learning, not merely this store's mutex.
	if db.Type() == engine.Sqlite {
		db.SetMaxOpenConns(1)
	}
	ddl := []string{
		`CREATE TABLE IF NOT EXISTS moderation_control (gid TEXT PRIMARY KEY, version BIGINT NOT NULL DEFAULT 0, cooldown BIGINT NOT NULL DEFAULT 0, rejected BIGINT NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS moderation_receipts (gid TEXT NOT NULL, id TEXT NOT NULL, chat_id BIGINT NOT NULL, identity_id BIGINT NOT NULL, identity_kind TEXT NOT NULL, message_id INTEGER NOT NULL, destination BIGINT NOT NULL, state TEXT NOT NULL, owner TEXT NOT NULL, lease_until BIGINT NOT NULL, created_at BIGINT NOT NULL, PRIMARY KEY(gid,id))`,
		`CREATE TABLE IF NOT EXISTS moderation_deliveries (gid TEXT NOT NULL, chat_id BIGINT NOT NULL, message_id INTEGER NOT NULL, receipt TEXT NOT NULL, PRIMARY KEY(gid,chat_id,message_id))`,
		`CREATE TABLE IF NOT EXISTS moderation_actions (gid TEXT NOT NULL, id TEXT NOT NULL, receipt TEXT NOT NULL, kind TEXT NOT NULL, chat_id BIGINT NOT NULL, identity_id BIGINT NOT NULL, identity_kind TEXT NOT NULL, message_id INTEGER NOT NULL, message_date BIGINT NOT NULL, until_date BIGINT NOT NULL, state TEXT NOT NULL, code TEXT NOT NULL, attempts INTEGER NOT NULL, deadline BIGINT NOT NULL, next_at BIGINT NOT NULL, owner TEXT NOT NULL, lease_until BIGINT NOT NULL, uncertain BOOLEAN NOT NULL, generation BIGINT NOT NULL, delivered BIGINT NOT NULL, notification_id INTEGER NOT NULL, PRIMARY KEY(gid,id))`,
		`CREATE INDEX IF NOT EXISTS idx_moderation_due ON moderation_actions(gid,state,next_at)`,
		`CREATE INDEX IF NOT EXISTS idx_moderation_receipt_actions ON moderation_actions(gid,receipt)`,
		`CREATE INDEX IF NOT EXISTS idx_moderation_deleted_pair ON moderation_actions(gid,chat_id,message_id,state)`,
		`CREATE INDEX IF NOT EXISTS idx_moderation_retention ON moderation_receipts(gid,state,created_at)`,
	}
	for _, q := range ddl {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return nil, fmt.Errorf("moderation migration: %w", err)
		}
	}
	_, err := db.ExecContext(
		ctx,
		db.Adopt(`INSERT INTO moderation_control(gid) VALUES (?) ON CONFLICT(gid) DO NOTHING`),
		db.GID(),
	)
	return j, journalError(err)
}

func token() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("claim entropy unavailable")
	}
	return hex.EncodeToString(b[:])
}

// Every write transaction first locks the per-gid control row: SQL is the atomic boundary.
type journalTx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	GetContext(context.Context, any, string, ...any) error
	SelectContext(context.Context, any, string, ...any) error
}

func (j *Journal) transaction(ctx context.Context, fn func(journalTx) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for retry := 0; ; retry++ {
		var err error
		if j.db.Type() == engine.Sqlite {
			var conn *sqlx.Conn
			conn, err = j.db.Connx(ctx)
			if err == nil {
				_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
				if err == nil {
					_, err = conn.ExecContext(
						ctx,
						j.db.Adopt("UPDATE moderation_control SET version=version+1 WHERE gid=?"),
						j.db.GID(),
					)
					if err == nil {
						err = fn(conn)
					}
					if err == nil {
						_, err = conn.ExecContext(ctx, "COMMIT")
					}
					if err != nil {
						rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), time.Second)
						_, rollbackErr := conn.ExecContext(rollbackCtx, "ROLLBACK")
						rollbackCancel()
						if rollbackErr != nil {
							_ = conn.Raw(func(any) error { return driver.ErrBadConn })
						}
					}
				}
				_ = conn.Close()
			}
		} else {
			var tx *sqlx.Tx
			tx, err = j.db.BeginTxx(ctx, nil)
			if err == nil {
				_, err = tx.ExecContext(ctx, j.db.Adopt("UPDATE moderation_control SET version=version+1 WHERE gid=?"), j.db.GID())
				if err == nil {
					err = fn(tx)
				}
				if err == nil {
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
			}
		}
		if err == nil {
			return nil
		}
		if retry >= 40 || !strings.Contains(strings.ToLower(err.Error()), "locked") {
			return journalError(err)
		}
		select {
		case <-ctx.Done():
			return journalError(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (j *Journal) exec(ctx context.Context, tx journalTx, q string, args ...any) error {
	_, err := tx.ExecContext(ctx, j.db.Adopt(q), args...)
	return journalError(err)
}

// Accept binds delivery and creates all actions in one transaction, before local learning/API calls.
// The returned owner is required to activate the preparation after non-replayable learning.
func (j *Journal) Accept(ctx context.Context, p Plan, now time.Time) (id, owner string, created bool, err error) {
	if p.Key == "" || p.ChatID == 0 || p.IdentityID == 0 || (p.IdentityKind != "user" && p.IdentityKind != "sender_chat") ||
		(p.IdentityKind == "user" && p.IdentityID < 0) ||
		(p.IdentityKind == "sender_chat" && p.IdentityID > 0) {
		return "", "", false, errors.New("invalid validated moderation target")
	}
	sourceKey := fmt.Sprintf("source:%d:%d:%s:%d", p.ChatID, p.MessageID, p.IdentityKind, p.IdentityID)
	deliveryKey := fmt.Sprintf("delivery:%d:%d", p.DeliveryChat, p.DeliveryMessage)
	if len(p.Actions) == 0 ||
		((p.Key != sourceKey || p.MessageID <= 0) && (p.Key != deliveryKey || p.DeliveryChat == 0 || p.DeliveryMessage <= 0)) {
		return "", "", false, errors.New("invalid exact-source or delivery-scoped identity")
	}
	id = p.Key
	if p.Learn {
		owner = token()
	}
	overflow := false
	err = j.transaction(ctx, func(tx journalTx) error {
		created = false
		overflow = false
		id = p.Key
		if p.DeliveryMessage != 0 {
			var prior string
			e := tx.GetContext(
				ctx,
				&prior,
				j.db.Adopt(`SELECT receipt FROM moderation_deliveries WHERE gid=? AND chat_id=? AND message_id=?`),
				j.db.GID(),
				p.DeliveryChat,
				p.DeliveryMessage,
			)
			if e == nil {
				id = prior
				return nil
			}
			if !errors.Is(e, sqlNoRows) {
				return journalError(e)
			}
		}
		var count int
		if e := tx.GetContext(ctx, &count, j.db.Adopt(`SELECT count(*) FROM moderation_receipts WHERE gid=? AND id=?`), j.db.GID(), id); e != nil {
			return journalError(e)
		}
		if count == 0 {
			var active, actions, total int
			if e := tx.GetContext(ctx, &active, j.db.Adopt(`SELECT count(*) FROM moderation_receipts WHERE gid=? AND state IN ('preparing','preparing_optional','ready')`), j.db.GID()); e != nil {
				return journalError(e)
			}
			if e := tx.GetContext(ctx, &actions, j.db.Adopt(`SELECT count(*) FROM moderation_actions WHERE gid=? AND kind!='notice' AND state IN ('pending','running')`), j.db.GID()); e != nil {
				return journalError(e)
			}
			if e := tx.GetContext(ctx, &total, j.db.Adopt(`SELECT count(*) FROM moderation_receipts WHERE gid=?`), j.db.GID()); e != nil {
				return journalError(e)
			}
			core := 0
			for _, action := range p.Actions {
				if action.Kind != "cleanup_delete" && action.State != Manual {
					core++
				}
			}
			if active >= j.limits.Receipts || total >= j.limits.RetainedReceipts || actions+core > j.limits.Actions ||
				core == 0 && actions >= j.limits.Actions {
				overflow = true
				if e := j.exec(ctx, tx, `UPDATE moderation_control SET rejected=rejected+1 WHERE gid=?`, j.db.GID()); e != nil {
					return journalError(e)
				}
				return j.notice(ctx, tx, p.Destination, now, true)
			}
			state := "ready"
			lease := int64(0)
			if p.Learn {
				state = "preparing"
				if p.LearningOptional {
					state = "preparing_optional"
				}
				lease = now.Add(90 * time.Second).UnixMilli()
			}
			if e := j.exec(ctx, tx, `INSERT INTO moderation_receipts VALUES (?,?,?,?,?,?,?,?,?,?,?)`, j.db.GID(), id, p.ChatID, p.IdentityID, p.IdentityKind, p.MessageID, p.Destination, state, owner, lease, now.UnixMilli()); e != nil {
				return journalError(e)
			}
			seen := map[string]bool{}
			cleanupBudget := j.limits.Actions - actions - core
			for _, a := range p.Actions {
				if a.ChatID == 0 || a.ChatID != p.ChatID || a.IdentityID != p.IdentityID ||
					a.IdentityKind != p.IdentityKind ||
					(deleteKind(a.Kind) && a.MessageID <= 0 && (a.MessageID != 0 || a.State != Manual)) ||
					(a.Kind != "ban" && a.Kind != "restrict" && !deleteKind(a.Kind)) {
					return errors.New("invalid action authority")
				}
				key := fmt.Sprintf("%s:%d", a.Kind, a.MessageID)
				// Original/cleanup overlaps represent one deletion; never call API twice for the pair.
				if deleteKind(a.Kind) {
					key = fmt.Sprintf("delete:%d", a.MessageID)
				}
				if seen[key] {
					continue
				}
				seen[key] = true
				a.ID = id + ":" + key
				a.Receipt = id
				if a.State != Manual {
					a.State = Pending
				}
				if deleteKind(a.Kind) && a.MessageID > 0 {
					var done int
					if lookupErr := tx.GetContext(ctx, &done, j.db.Adopt(`SELECT count(*) FROM moderation_actions WHERE gid=? AND chat_id=? AND message_id=? AND identity_id=? AND kind IN ('original_delete','cleanup_delete') AND state='confirmed'`), j.db.GID(), a.ChatID, a.MessageID, a.IdentityID); lookupErr != nil {
						return journalError(lookupErr)
					}
					if done > 0 {
						a.State = Confirmed
						a.Code = "previously_confirmed_pair"
					}
				}
				if a.Kind == "cleanup_delete" && a.State == Pending {
					if cleanupBudget <= 0 {
						a.State = Manual
						a.Code = "cleanup_capacity_manual_required"
					} else {
						cleanupBudget--
					}
				}
				a.NextAt = now.UnixMilli()
				a.Deadline = now.Add(24 * time.Hour).UnixMilli()
				if deleteKind(a.Kind) && a.MessageDate > 0 && a.MessageDate <= now.Unix() {
					d := time.Unix(a.MessageDate, 0).Add(48*time.Hour - 2*time.Minute).UnixMilli()
					if d < a.Deadline {
						a.Deadline = d
					}
				} else if deleteKind(a.Kind) {
					a.MessageDate = 0
				}
				if (a.Kind == "ban" || a.Kind == "restrict") && a.UntilDate > 0 &&
					a.UntilDate < now.Add(366*24*time.Hour).Unix() {
					d := time.Unix(a.UntilDate, 0).Add(-30 * time.Second).UnixMilli()
					if d < a.Deadline {
						a.Deadline = d
					}
				}
				if e := j.insertAction(ctx, tx, a); e != nil {
					return journalError(e)
				}
			}
			created = true
			if !p.Learn {
				if e := j.notice(ctx, tx, p.Destination, now, false); e != nil {
					return journalError(e)
				}
				if e := j.exec(ctx, tx, `UPDATE moderation_receipts SET state='finished' WHERE gid=? AND id=? AND NOT EXISTS (SELECT 1 FROM moderation_actions WHERE gid=? AND receipt=? AND state IN ('pending','running'))`, j.db.GID(), id, j.db.GID(), id); e != nil {
					return journalError(e)
				}
			}
		}
		if p.DeliveryMessage != 0 {
			return j.exec(
				ctx,
				tx,
				`INSERT INTO moderation_deliveries VALUES (?,?,?,?) ON CONFLICT(gid,chat_id,message_id) DO NOTHING`,
				j.db.GID(),
				p.DeliveryChat,
				p.DeliveryMessage,
				id,
			)
		}
		return nil
	})
	if overflow && err == nil {
		err = ErrCapacity
	}
	return id, owner, created, journalError(err)
}

var sqlNoRows = sql.ErrNoRows

func (j *Journal) insertAction(ctx context.Context, tx journalTx, a Action) error {
	return j.exec(
		ctx,
		tx,
		`INSERT INTO moderation_actions VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.db.GID(),
		a.ID,
		a.Receipt,
		a.Kind,
		a.ChatID,
		a.IdentityID,
		a.IdentityKind,
		a.MessageID,
		a.MessageDate,
		a.UntilDate,
		a.State,
		a.Code,
		a.Attempts,
		a.Deadline,
		a.NextAt,
		a.Owner,
		a.LeaseUntil,
		a.Uncertain,
		a.Generation,
		a.Delivered,
		a.NotificationID,
	)
}

// Activate does not replay learning. A crash during preparation produces durable manual review.
func (j *Journal) learningNote(ctx context.Context, tx journalTx, id, code string) error {
	var a Action
	if err := tx.GetContext(ctx, &a, j.db.Adopt("SELECT chat_id,identity_id,identity_kind FROM moderation_receipts "+
		"WHERE gid=? AND id=?"), j.db.GID(), id); err != nil {
		return journalError(err)
	}
	a.ID = id + ":learning_note"
	a.Receipt = id
	a.Kind = "learning_note"
	a.State = Manual
	a.Code = code
	a.Uncertain = true
	return j.insertAction(ctx, tx, a)
}

// Activate ends fenced local preparation without replaying classifier learning.
func (j *Journal) Activate(ctx context.Context, id, owner string, now time.Time, learningOK bool) error {
	return j.transaction(ctx, func(tx journalTx) error {
		var prior string
		if err := tx.GetContext(ctx, &prior, j.db.Adopt("SELECT state FROM moderation_receipts WHERE gid=? AND id=? AND owner=?"),
			j.db.GID(), id, owner); err != nil {
			if errors.Is(err, sqlNoRows) {
				return ErrLease
			}
			return journalError(err)
		}
		optional := prior == "preparing_optional"
		state := "ready"
		if !learningOK && !optional {
			state = Manual
		}
		result, err := tx.ExecContext(
			ctx,
			j.db.Adopt(
				"UPDATE moderation_receipts SET state=?,owner='',lease_until=0 WHERE gid=? AND id=? "+
					"AND owner=? AND state IN ('preparing','preparing_optional') AND lease_until>?",
			),
			state,
			j.db.GID(),
			id,
			owner,
			now.UnixMilli(),
		)
		if err != nil {
			return journalError(err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return journalError(err)
		}
		if n != 1 {
			return ErrLease
		}
		if !learningOK {
			if optional {
				if err = j.learningNote(ctx, tx, id, "learning_error_core_resumed"); err != nil {
					return journalError(err)
				}
			} else {
				if err = j.exec(ctx, tx, "UPDATE moderation_actions SET state=?,code='learning_uncertain' WHERE gid=? AND "+
					"receipt=? AND state='pending'", Manual, j.db.GID(), id); err != nil {
					return journalError(err)
				}
			}
		}
		var destination int64
		if err = tx.GetContext(ctx, &destination, j.db.Adopt("SELECT destination FROM moderation_receipts WHERE gid=? AND id=?"),
			j.db.GID(), id); err != nil {
			return journalError(err)
		}
		if err = j.notice(ctx, tx, destination, now, !learningOK); err != nil {
			return journalError(err)
		}
		return j.exec(
			ctx,
			tx,
			"UPDATE moderation_receipts SET state='finished' WHERE gid=? AND id=? AND "+
				"state='ready' AND NOT EXISTS (SELECT 1 FROM moderation_actions WHERE "+
				"gid=? AND receipt=? AND state IN ('pending','running'))",
			j.db.GID(),
			id,
			j.db.GID(),
			id,
		)
	})
}

func (j *Journal) notice(ctx context.Context, tx journalTx, destination int64, now time.Time, terminal bool) error {
	if destination == 0 {
		return nil
	}
	id := fmt.Sprintf("notice:%d", destination)
	a := Action{
		ID:         id,
		Receipt:    id,
		Kind:       "notice",
		ChatID:     destination,
		State:      Pending,
		NextAt:     now.UnixMilli(),
		Deadline:   now.Add(24 * time.Hour).UnixMilli(),
		Generation: 1,
	}
	if err := j.exec(ctx, tx, `INSERT INTO moderation_actions VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(gid,id) DO NOTHING`, j.db.GID(), a.ID, a.Receipt, a.Kind, a.ChatID, 0, "system", 0, 0, 0, a.State, "", 0, a.Deadline, a.NextAt, "", 0, false, 1, 0, 0); err != nil {
		return journalError(err)
	}
	// Pending notifications retain their budget. Terminal changes can renew an exhausted period.
	return j.exec(ctx, tx, `UPDATE moderation_actions SET generation=generation+1,
 deadline=CASE WHEN state='notification_undelivered' AND code IN
('telegram_400','telegram_401','telegram_403','telegram_404','telegram_501',
'unclassified_permanent_error','notification_destination_changed') THEN deadline
 WHEN state NOT IN ('pending','running') OR (deadline<=? AND ?) THEN ? ELSE deadline END,
 attempts=CASE WHEN state='notification_undelivered' AND code IN
('telegram_400','telegram_401','telegram_403','telegram_404','telegram_501',
'unclassified_permanent_error','notification_destination_changed') THEN attempts
 WHEN state NOT IN ('pending','running') OR (deadline<=? AND ?) THEN 0 ELSE attempts END,
 state=CASE WHEN state='running' OR (state='notification_undelivered' AND code IN
('telegram_400','telegram_401','telegram_403','telegram_404','telegram_501',
'unclassified_permanent_error','notification_destination_changed'))
 THEN state ELSE 'pending' END WHERE gid=? AND id=?`, now.UnixMilli(), terminal, a.Deadline, now.UnixMilli(),
		terminal, j.db.GID(), id)
}

// Claim uses fenced SQL ownership, including restart reconciliation after an expired lease.
func (j *Journal) Claim(ctx context.Context, now time.Time) (a Action, ok bool, err error) {
	// Idle workers read only; do not fsync a control-row write every five seconds.
	var due int
	err = j.db.GetContext(ctx, &due, j.db.Adopt(`SELECT
 (SELECT count(*) FROM moderation_actions WHERE gid=? AND ((state='pending' AND next_at<=?) OR
(state='running' AND lease_until<?))) +
 (SELECT count(*) FROM moderation_receipts WHERE gid=? AND state IN ('preparing','preparing_optional') AND
lease_until<?)`), j.db.GID(), now.UnixMilli(), now.Add(-15*time.Second).UnixMilli(), j.db.GID(),
		now.Add(-15*time.Second).UnixMilli())
	if err != nil || due == 0 {
		return a, false, journalError(err)
	}
	err = j.transaction(ctx, func(tx journalTx) error {
		// A rolled-back attempt must not leak a prior owner through a successful SQL retry.
		a, ok = Action{}, false
		// Preparation cannot safely resume non-idempotent classifier learning.
		var stale []struct {
			ID          string `db:"id"`
			Destination int64  `db:"destination"`
			State       string `db:"state"`
		}
		if e := tx.SelectContext(ctx, &stale, j.db.Adopt(`SELECT id,destination,state FROM moderation_receipts WHERE gid=? AND state IN ('preparing','preparing_optional') AND lease_until<?`), j.db.GID(), now.Add(-15*time.Second).UnixMilli()); e != nil {
			return journalError(e)
		}
		for _, r := range stale {
			if r.State == "preparing_optional" {
				if e := j.exec(ctx, tx, "UPDATE moderation_receipts SET state='ready',owner='',lease_until=0 "+
					"WHERE gid=? AND id=?", j.db.GID(), r.ID); e != nil {
					return journalError(e)
				}
				if e := j.learningNote(ctx, tx, r.ID, "learning_interrupted_core_resumed"); e != nil {
					return journalError(e)
				}
				if e := j.notice(ctx, tx, r.Destination, now, true); e != nil {
					return journalError(e)
				}
				continue
			}
			if e := j.exec(ctx, tx, `UPDATE moderation_receipts SET state='manual_review',owner='' WHERE gid=? AND id=?`, j.db.GID(), r.ID); e != nil {
				return journalError(e)
			}
			if e := j.exec(ctx, tx, `UPDATE moderation_actions SET state='manual_review',code='learning_uncertain' WHERE gid=? AND receipt=? AND state='pending'`, j.db.GID(), r.ID); e != nil {
				return journalError(e)
			}
			if e := j.notice(ctx, tx, r.Destination, now, true); e != nil {
				return journalError(e)
			}
		}
		var cooldown int64
		if e := tx.GetContext(ctx, &cooldown, j.db.Adopt(`SELECT cooldown FROM moderation_control WHERE gid=?`), j.db.GID()); e != nil {
			return journalError(e)
		}
		if cooldown > now.UnixMilli() {
			return nil
		}
		q := `SELECT a.id,a.receipt,a.kind,a.chat_id,a.identity_id,a.identity_kind,a.message_id,a.message_date,a.until_date,a.state,a.code,a.attempts,a.deadline,a.next_at,a.owner,a.lease_until,a.uncertain,a.generation,a.delivered,a.notification_id FROM moderation_actions a LEFT JOIN moderation_receipts r ON a.gid=r.gid AND a.receipt=r.id WHERE a.gid=? AND (a.kind='notice' OR r.state='ready') AND ((a.state='pending' AND a.next_at<=?) OR (a.state='running' AND a.lease_until<?)) ORDER BY a.next_at,a.id LIMIT 1`
		e := tx.GetContext(ctx, &a, j.db.Adopt(q), j.db.GID(), now.UnixMilli(), now.Add(-15*time.Second).UnixMilli())
		if errors.Is(e, sqlNoRows) {
			return nil
		}
		if e != nil {
			return journalError(e)
		}
		if a.State == Running && a.Kind != "notice" {
			a.Uncertain = true
		}
		if deleteKind(a.Kind) && a.MessageID > 0 {
			var done int
			if lookupErr := tx.GetContext(ctx, &done, j.db.Adopt(`SELECT count(*) FROM moderation_actions WHERE gid=? AND chat_id=? AND message_id=? AND identity_id=? AND kind IN ('original_delete','cleanup_delete') AND state='confirmed'`), j.db.GID(), a.ChatID, a.MessageID, a.IdentityID); lookupErr != nil {
				return journalError(lookupErr)
			}
			if done > 0 {
				a.Code = "previously_confirmed_pair"
			}
		}
		a.Owner = token()
		a.State = Running
		a.LeaseUntil = now.Add(90 * time.Second).UnixMilli()
		a.Attempts++
		if e = j.exec(ctx, tx, `UPDATE moderation_actions SET owner=?,state=?,lease_until=?,uncertain=?,attempts=? WHERE gid=? AND id=?`, a.Owner, a.State, a.LeaseUntil, a.Uncertain, a.Attempts, j.db.GID(), a.ID); e != nil {
			return journalError(e)
		}
		ok = true
		return nil
	})
	return a, ok, journalError(err)
}

// Owned verifies that the bounded API request still fits inside the current lease.
func (j *Journal) Owned(ctx context.Context, a Action, now time.Time) (bool, error) {
	var n int
	err := j.db.GetContext(
		ctx,
		&n,
		j.db.Adopt(
			`SELECT count(*) FROM moderation_actions WHERE gid=? AND id=? AND owner=? AND state='running' AND lease_until>?`,
		),
		j.db.GID(),
		a.ID,
		a.Owner,
		now.Add(15*time.Second).UnixMilli(),
	)
	return n == 1, journalError(err)
}

// Finish conditionally commits independent outcomes and their notification generation.
func (j *Journal) Finish(ctx context.Context, a Action, o Outcome, next, now time.Time) error {
	return j.transaction(ctx, func(tx journalTx) error {
		state := o.State
		if o.Retry {
			state = Pending
		}
		r, e := tx.ExecContext(
			ctx,
			j.db.Adopt(
				`UPDATE moderation_actions SET state=?,code=?,uncertain=?,next_at=?,owner='',lease_until=0,notification_id=CASE WHEN ?<0 THEN 0 WHEN ?>0 THEN ? ELSE notification_id END,delivered=CASE WHEN kind='notice' AND ?='confirmed' THEN ? ELSE delivered END WHERE gid=? AND id=? AND owner=? AND state='running'`,
			),
			state,
			o.Code,
			o.Uncertain,
			next.UnixMilli(),
			o.NotificationID,
			o.NotificationID,
			o.NotificationID,
			state,
			a.Generation,
			j.db.GID(),
			a.ID,
			a.Owner,
		)
		if e != nil {
			return journalError(e)
		}
		n, e := r.RowsAffected()
		if e != nil {
			return journalError(e)
		}
		if n != 1 {
			return ErrLease
		}
		if o.RetryAfter > 0 {
			d := now.Add(o.RetryAfter).UnixMilli()
			if e = j.exec(ctx, tx, `UPDATE moderation_control SET cooldown=CASE WHEN cooldown<? THEN ? ELSE cooldown END WHERE gid=?`, d, d, j.db.GID()); e != nil {
				return journalError(e)
			}
		}
		if a.Kind == "notice" {
			if state == Confirmed {
				return j.exec(
					ctx,
					tx,
					`UPDATE moderation_actions SET state=CASE WHEN generation>delivered THEN 'pending' ELSE state END,attempts=CASE WHEN generation>delivered THEN 0 ELSE attempts END,next_at=? WHERE gid=? AND id=?`,
					now.Add(time.Minute).UnixMilli(),
					j.db.GID(),
					a.ID,
				)
			}
			return nil
		}
		// Only changed states/first failures dirty the digest; retry attempts do not create a storm.
		var destination int64
		if e = tx.GetContext(ctx, &destination, j.db.Adopt(`SELECT destination FROM moderation_receipts WHERE gid=? AND id=?`), j.db.GID(), a.Receipt); e != nil {
			return journalError(e)
		}
		if final(state) || a.Attempts == 1 {
			if e = j.notice(ctx, tx, destination, now, final(state)); e != nil {
				return journalError(e)
			}
		}
		return j.exec(
			ctx,
			tx,
			`UPDATE moderation_receipts SET state='finished' WHERE gid=? AND id=? AND NOT EXISTS (SELECT 1 FROM moderation_actions WHERE gid=? AND receipt=? AND state IN ('pending','running'))`,
			j.db.GID(),
			a.Receipt,
			j.db.GID(),
			a.Receipt,
		)
	})
}

// Snapshot is the operator/outbox view of the very same action rows.
func (j *Journal) Snapshot(ctx context.Context) ([]Action, error) {
	var actions []Action
	err := j.db.SelectContext(
		ctx,
		&actions,
		j.db.Adopt(
			`SELECT id,receipt,kind,chat_id,identity_id,identity_kind,message_id,message_date,until_date,state,code,attempts,deadline,next_at,owner,lease_until,uncertain,generation,delivered,notification_id FROM moderation_actions WHERE gid=? ORDER BY id`,
		),
		j.db.GID(),
	)
	return actions, journalError(err)
}

// Digest bounds notification memory independently of retained history size.
//
//nolint:gocritic // Named aggregate results would shadow the local query containers.
func (j *Journal) Digest(ctx context.Context, destination int64) (map[string]int, []Action, int64, error) {
	var counts []struct {
		Kind  string `db:"kind"`
		State string `db:"state"`
		N     int    `db:"n"`
	}
	query := `SELECT a.kind,a.state,count(*) n FROM moderation_actions a JOIN moderation_receipts r ON a.gid=r.gid AND a.receipt=r.id WHERE a.gid=? AND r.destination=? GROUP BY a.kind,a.state`
	if err := j.db.SelectContext(ctx, &counts, j.db.Adopt(query), j.db.GID(), destination); err != nil {
		return nil, nil, 0, journalError(err)
	}
	result := map[string]int{}
	for _, c := range counts {
		result[c.Kind+"="+c.State] = c.N
	}
	var details []Action
	query = `SELECT a.id,a.receipt,a.kind,a.chat_id,a.identity_id,a.identity_kind,a.message_id,a.message_date,a.until_date,a.state,a.code,a.attempts,a.deadline,a.next_at,a.owner,a.lease_until,a.uncertain,a.generation,a.delivered,a.notification_id FROM moderation_actions a JOIN moderation_receipts r ON a.gid=r.gid AND a.receipt=r.id WHERE a.gid=? AND r.destination=? AND a.state!='confirmed' ORDER BY r.created_at DESC,a.id LIMIT 10`
	if err := j.db.SelectContext(ctx, &details, j.db.Adopt(query), j.db.GID(), destination); err != nil {
		return nil, nil, 0, journalError(err)
	}
	var rejected int64
	err := j.db.GetContext(ctx, &rejected, j.db.Adopt(`SELECT rejected FROM moderation_control WHERE gid=?`), j.db.GID())
	return result, details, rejected, journalError(err)
}

// Cooldown returns the durable per-instance Telegram rate-limit barrier.
func (j *Journal) Cooldown(ctx context.Context) (time.Time, error) {
	var until int64
	err := j.db.GetContext(ctx, &until, j.db.Adopt(`SELECT cooldown FROM moderation_control WHERE gid=?`), j.db.GID())
	return time.UnixMilli(until), err
}

// Pause extends the barrier without shortening an existing retry_after.
func (j *Journal) Pause(ctx context.Context, until time.Time) error {
	return j.transaction(ctx, func(tx journalTx) error {
		return j.exec(
			ctx,
			tx,
			`UPDATE moderation_control SET cooldown=CASE WHEN cooldown<? THEN ? ELSE cooldown END WHERE gid=?`,
			until.UnixMilli(),
			until.UnixMilli(),
			j.db.GID(),
		)
	})
}

// Prune only acknowledged terminal receipts; pending work and undelivered signals survive retention.
func (j *Journal) Prune(ctx context.Context, now time.Time) error {
	return j.transaction(ctx, func(tx journalTx) error {
		q := `SELECT id FROM moderation_receipts WHERE gid=? AND state='finished' AND created_at<? AND NOT EXISTS (SELECT 1 FROM moderation_actions WHERE gid=? AND kind='notice' AND (state!='confirmed' OR generation>delivered))`
		var ids []string
		if e := tx.SelectContext(ctx, &ids, j.db.Adopt(q), j.db.GID(), now.Add(-30*24*time.Hour).UnixMilli(), j.db.GID()); e != nil {
			return journalError(e)
		}
		for _, id := range ids {
			for _, table := range []string{"moderation_deliveries", "moderation_actions"} {
				if e := j.exec(ctx, tx, `DELETE FROM `+table+` WHERE gid=? AND receipt=?`, j.db.GID(), id); e != nil {
					return journalError(e)
				}
			}
			if e := j.exec(ctx, tx, `DELETE FROM moderation_receipts WHERE gid=? AND id=?`, j.db.GID(), id); e != nil {
				return journalError(e)
			}
		}
		return nil
	})
}

func journalError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("moderation journal: %w", err)
}
