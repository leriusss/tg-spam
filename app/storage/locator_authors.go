package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// MessageAuthor is evidence recorded exclusively from the original Telegram update,
// never from a forwarded name, text match, admin report, or legacy locator ID.
type MessageAuthor struct {
	Kind     string `db:"kind"`
	ID       int64  `db:"identity_id"`
	Source   string `db:"source"`
	UpdateID int    `db:"update_id"`
}

func (l *Locator) initMessageAuthors(ctx context.Context) error {
	_, err := l.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS message_authors (
 gid TEXT NOT NULL, chat_id BIGINT NOT NULL, msg_id INTEGER NOT NULL,
 kind TEXT NOT NULL, identity_id BIGINT NOT NULL, source TEXT NOT NULL, update_id BIGINT NOT NULL,
 PRIMARY KEY(gid,chat_id,msg_id))`)
	return authorEvidenceError(err)
}

// RecordMessageAuthor does not upgrade old rows. Conflicting observations invalidate
// the evidence permanently; edits cannot replace an established author.
func (l *Locator) RecordMessageAuthor(ctx context.Context, chat int64, msg int, a MessageAuthor) error {
	if chat == 0 || msg <= 0 || a.UpdateID <= 0 || a.Source != "telegram_update" ||
		(a.Kind != "user" && a.Kind != "sender_chat") || a.Kind == "user" && a.ID <= 0 || a.Kind == "sender_chat" && a.ID >= 0 {
		return fmt.Errorf("invalid original-update author evidence")
	}
	l.Lock()
	defer l.Unlock()
	_, err := l.ExecContext(ctx, l.Adopt(`INSERT INTO message_authors VALUES (?,?,?,?,?,?,?)
 ON CONFLICT(gid,chat_id,msg_id) DO UPDATE SET
 source=CASE WHEN message_authors.kind=excluded.kind AND message_authors.identity_id=excluded.identity_id
 THEN message_authors.source ELSE 'conflict' END`),
		l.GID(), chat, msg, a.Kind, a.ID, a.Source, a.UpdateID)
	return authorEvidenceError(err)
}

// MessageAuthor returns evidence only; legacy rows remain unverified.
func (l *Locator) MessageAuthor(ctx context.Context, chat int64, msg int) (MessageAuthor, error) {
	l.RLock()
	defer l.RUnlock()
	var a MessageAuthor
	err := l.GetContext(
		ctx,
		&a,
		l.Adopt(`SELECT kind,identity_id,source,update_id FROM message_authors WHERE gid=? AND chat_id=? AND msg_id=?`),
		l.GID(),
		chat,
		msg,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return MessageAuthor{}, nil
	}
	return a, authorEvidenceError(err)
}

func authorEvidenceError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("locator author evidence: %w", err)
}
