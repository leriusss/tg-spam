package moderation

import (
	"context"
	"database/sql"
	"time"

	"github.com/umputun/tg-spam/app/storage/engine"
)

// CardAudit exposes only already persisted history, never inferred API timestamps.
type CardAudit struct {
	Type       string `db:"-"`
	ID         string `db:"id"`
	Kind       string `db:"kind"`
	State      string `db:"state"`
	OperatorID int64  `db:"operator_id"`
	Time       int64  `db:"time"`
	Snapshot   string `db:"snapshot"`
}

// ReadCardView is a consistent read-only navigation snapshot. It cannot renew work.
func (j *Journal) ReadCardView(ctx context.Context, ref string) (Card, error) {
	return j.cardView(ctx, ref, true)
}

func (j *Journal) readCardTransaction(ctx context.Context, fn func(journalTx) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	options := &sql.TxOptions{ReadOnly: true}
	if j.db.Type() == engine.Postgres {
		options.Isolation = sql.LevelRepeatableRead
	}
	tx, err := j.db.BeginTxx(ctx, options)
	if err != nil {
		return journalError(err)
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return journalError(err)
	}
	return journalError(tx.Commit())
}

func (j *Journal) readCardAudit(ctx context.Context, tx journalTx, c *Card) error {
	if err := tx.GetContext(ctx, &c.RecordedAt, j.db.Adopt(`SELECT created_at FROM moderation_receipts WHERE gid=? AND id=?`), j.db.GID(), c.Receipt); err != nil {
		return journalError(err)
	}
	queries := []struct{ kind, sql string }{
		{"decision", `SELECT id,verb AS kind,state,operator_id FROM moderation_decisions WHERE gid=? AND receipt=? ORDER BY id`},
		{"closure", `SELECT action_id AS id,operator_id,closed_at AS time FROM moderation_resolutions WHERE gid=? AND receipt=? ORDER BY closed_at,action_id`},
		{"origin", `SELECT origin AS kind,min(accepted_at) AS time FROM moderation_demands WHERE gid=? AND receipt=? GROUP BY origin ORDER BY min(accepted_at),origin`},
		{"request", `SELECT state,created_at AS time,requester AS operator_id FROM moderation_requests WHERE gid=? AND receipt=?`},
		{"past_request", `SELECT ref AS id,state,requester AS operator_id,created_at AS time,action_snapshot AS snapshot FROM moderation_delete_request_history WHERE gid=? AND receipt=? ORDER BY created_at,ref`},
	}
	for _, query := range queries {
		var entries []CardAudit
		if err := tx.SelectContext(ctx, &entries, j.db.Adopt(query.sql), j.db.GID(), c.Receipt); err != nil {
			return journalError(err)
		}
		for _, entry := range entries {
			entry.Type = query.kind
			c.Audit = append(c.Audit, entry)
		}
	}
	return nil
}
