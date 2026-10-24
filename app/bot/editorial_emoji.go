package bot

import (
	"fmt"
	"strconv"
	"strings"
)

type editorialEmojiPair struct{ chatID, senderChatID int64 }

// EditorialEmojiExemptions authorizes only the emoji check for exact source pairs.
// It neither adds manual trust nor identifies the human behind a SenderChat.
type EditorialEmojiExemptions struct {
	pairs map[editorialEmojiPair]struct{}
}

// ParseEditorialEmojiExemptions parses comma-separated discussion:sender-chat IDs.
// Both IDs must be negative Telegram chat IDs. The empty value grants nothing.
func ParseEditorialEmojiExemptions(raw string) (EditorialEmojiExemptions, error) {
	result := EditorialEmojiExemptions{pairs: make(map[editorialEmojiPair]struct{})}
	if strings.TrimSpace(raw) == "" {
		return result, nil
	}
	for i, entry := range strings.Split(raw, ",") {
		parts := strings.Split(strings.TrimSpace(entry), ":")
		if len(parts) != 2 {
			return EditorialEmojiExemptions{}, fmt.Errorf("editorial emoji entry %d must be discussion:sender-chat numeric IDs", i+1)
		}
		chat, e1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
		sender, e2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
		if e1 != nil || e2 != nil || chat >= 0 || sender >= 0 {
			return EditorialEmojiExemptions{}, fmt.Errorf("editorial emoji entry %d requires two negative int64 chat IDs", i+1)
		}
		result.pairs[editorialEmojiPair{chat, sender}] = struct{}{}
	}
	return result, nil
}

// Allows requires the actual protected chat and actual SenderChat of this message.
func (e EditorialEmojiExemptions) Allows(chatID, senderChatID int64) bool {
	if chatID >= 0 || senderChatID >= 0 {
		return false
	}
	_, ok := e.pairs[editorialEmojiPair{chatID, senderChatID}]
	return ok
}

// String avoids exposing configured identities in the startup configuration log.
func (e EditorialEmojiExemptions) String() string {
	return fmt.Sprintf("%d configured pairs", len(e.pairs))
}
