package events

import (
	"context"
	"log"

	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/lib/spamcheck"
)

func (l *TelegramListener) botStartCleanup(ctx context.Context, msg bot.Message, checks []spamcheck.Response) {
	if !l.BotStartSpamEnabled || !l.AggressiveCleanup || l.AggressiveCleanupLimit <= 0 ||
		l.RecoveryJournal == nil || l.Dry || l.TrainingMode || msg.From.ID <= 0 || msg.SenderChat.ID != 0 {
		return
	}
	for i := range checks {
		if checks[i].Name != "bot-start spam" || !checks[i].Spam {
			continue
		}
		scoped, ok := l.Locator.(interface {
			GetUserMessageIDsInChat(context.Context, int64, int64, int) ([]int, error)
		})
		if !ok {
			return
		}
		ids, err := scoped.GetUserMessageIDsInChat(ctx, msg.From.ID, msg.ChatID, l.AggressiveCleanupLimit)
		if err != nil {
			log.Print("[WARN] bot-start cleanup candidates unavailable")
			return
		}
		seen := make(map[int]struct{}, len(ids))
		for _, id := range ids {
			if id <= 0 || id == msg.ID {
				continue
			}
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			checks[i].ExtraDeleteIDs = append(checks[i].ExtraDeleteIDs, id)
			if len(checks[i].ExtraDeleteIDs) >= l.AggressiveCleanupLimit {
				break
			}
		}
		return
	}
}
