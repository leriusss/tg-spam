package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MessageDates separates the Telegram send time from the first local observation.
// Zero MessageDate denotes a legacy or unavailable Telegram date.
type MessageDates struct {
	MessageDate int64 `db:"message_date"`
	ObservedAt  int64 `db:"observed_at"`
}

func (l *Locator) initMessageDates(ctx context.Context) error {
	_, err := l.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS message_dates (
		gid TEXT NOT NULL, chat_id BIGINT NOT NULL, msg_id INTEGER NOT NULL,
		message_date BIGINT NOT NULL, observed_at BIGINT NOT NULL,
		PRIMARY KEY (gid, chat_id, msg_id))`)
	if err != nil {
		return fmt.Errorf("create locator message dates: %w", err)
	}
	_, err = l.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_messages_gid_chat_msg
 ON messages (gid, chat_id, msg_id)`)
	if err != nil {
		return fmt.Errorf("index locator message dates: %w", err)
	}
	return nil
}

// RecordMessageDate persists Message.Date independently of locator processing time.
// Edits preserve the first observation and cannot replace a known date with zero.
func (l *Locator) RecordMessageDate(ctx context.Context, chatID int64, msgID int, date int64) error {
	l.Lock()
	defer l.Unlock()
	if date < 0 || date > time.Now().Unix() {
		date = 0
	}
	query := l.Adopt(`INSERT INTO message_dates (gid, chat_id, msg_id, message_date, observed_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (gid, chat_id, msg_id) DO UPDATE SET
		message_date = CASE WHEN message_dates.message_date > 0
		THEN message_dates.message_date ELSE excluded.message_date END`)
	if _, err := l.ExecContext(ctx, query, l.GID(), chatID, msgID, date, time.Now().Unix()); err != nil {
		return fmt.Errorf("record locator message date: %w", err)
	}
	_, err := l.ExecContext(ctx, l.Adopt(`DELETE FROM message_dates WHERE gid = ? AND NOT EXISTS (
		SELECT 1 FROM messages WHERE messages.gid = message_dates.gid
		AND messages.chat_id = message_dates.chat_id AND messages.msg_id = message_dates.msg_id)`), l.GID())
	if err != nil {
		return fmt.Errorf("prune locator message dates: %w", err)
	}
	return nil
}

// MessageDates returns zero dates for legacy rows, without inventing a send time.
func (l *Locator) MessageDates(ctx context.Context, chatID int64, msgID int) (MessageDates, error) {
	l.RLock()
	defer l.RUnlock()
	var dates MessageDates
	err := l.GetContext(ctx, &dates, l.Adopt(`SELECT message_date, observed_at FROM message_dates
		WHERE gid = ? AND chat_id = ? AND msg_id = ?`), l.GID(), chatID, msgID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return dates, fmt.Errorf("read locator message dates: %w", err)
	}
	return dates, nil
}
