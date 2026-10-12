package storage

import (
	"context"
	"fmt"
)

// GetUserMessageIDsInChat keeps durable cleanup pairs scoped to their actual primary chat.
// The legacy method remains available to preserve existing caller/mock contracts.
func (l *Locator) GetUserMessageIDsInChat(ctx context.Context, userID, chatID int64, limit int) ([]int, error) {
	l.RLock()
	defer l.RUnlock()
	if limit == 0 {
		return nil, nil
	}
	if limit < 0 {
		return nil, fmt.Errorf("durable cleanup requires a bounded nonnegative limit")
	}
	var ids []int
	err := l.SelectContext(ctx, &ids, l.Adopt(`SELECT msg_id FROM messages WHERE gid=? AND user_id=? AND chat_id=?
 GROUP BY msg_id ORDER BY MAX(time) DESC,msg_id DESC LIMIT ?`), l.GID(), userID, chatID, limit)
	if err != nil {
		return nil, fmt.Errorf("select chat-scoped cleanup: %w", err)
	}
	return ids, nil
}
