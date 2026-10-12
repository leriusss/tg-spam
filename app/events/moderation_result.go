package events

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"

	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/storage"
)

type actionResult struct {
	ChatID    int64
	MessageID int
	Status    string
	Reason    string
}

var errUnconfirmedAPIResponse = errors.New("API success not confirmed")

type cleanupResult struct {
	Selected  int
	Deleted   int
	Absent    int
	Expired   int
	Failed    int
	Unknown   int
	Remaining int
	Items     []actionResult
}

func (r cleanupResult) String() string {
	return fmt.Sprintf("selected=%d, deleted=%d, absent=%d, expired=%d, failed=%d, unknown=%d, remaining=%d",
		r.Selected, r.Deleted, r.Absent, r.Expired, r.Failed, r.Unknown, r.Remaining)
}

func (r *cleanupResult) add(item actionResult) {
	r.Items = append(r.Items, item)
	switch item.Status {
	case "confirmed":
		r.Deleted++
	case "absent":
		r.Absent++
	case "expired":
		r.Expired++
	case "unknown":
		r.Unknown++
	default:
		r.Failed++
	}
}

func (r actionResult) String() string {
	if r.Reason == "" {
		return r.Status
	}
	return r.Status + " (" + r.Reason + ")"
}

func failedAction(err error) actionResult {
	if errors.Is(err, errUnconfirmedAPIResponse) {
		return actionResult{Status: "unknown", Reason: "API success not confirmed"}
	}
	var apiErr *tbapi.Error
	if errors.As(err, &apiErr) {
		status := "failed"
		if apiErr.Code >= 500 {
			status = "unknown"
		}
		return actionResult{Status: status, Reason: fmt.Sprintf("telegram_%d: %s", apiErr.Code, apiErr.Message)}
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.Canceled) {
		return actionResult{Status: "unknown", Reason: "transport_error; API outcome not confirmed"}
	}
	return actionResult{Status: "failed", Reason: "API request failed; see sanitized logs"}
}

// deleteOriginal checks the Telegram send date, never the locator observation time.
func deleteOriginal(api TbAPI, chatID int64, msgID int, sentAt int64, now time.Time) (actionResult, error) {
	r := actionResult{ChatID: chatID, MessageID: msgID}
	if sentAt > 0 && !now.Before(time.Unix(sentAt, 0).Add(48*time.Hour)) {
		r.Status, r.Reason = "expired", "48-hour Bot API window exceeded; manual deletion required"
		return r, nil
	}
	resp, err := api.Request(tbapi.DeleteMessageConfig{BaseChatMessage: tbapi.BaseChatMessage{
		MessageID: msgID, ChatConfig: tbapi.ChatConfig{ChatID: chatID},
	}})
	if err != nil {
		if isSoftDeleteMiss(err) {
			r.Status, r.Reason = "absent", "message absent or inaccessible; deletion by this bot not confirmed"
			return r, nil
		}
		failure := failedAction(err)
		r.Status, r.Reason = failure.Status, failure.Reason
		if strings.Contains(strings.ToLower(err.Error()), "message can't be deleted") {
			r.Reason = "Telegram refused deletion; manual deletion required; age alone does not establish cause"
		}
		return r, fmt.Errorf("original_delete_failed chat_id=%d message_id=%d: %w", chatID, msgID, err)
	}
	if resp == nil || !resp.Ok {
		r.Status, r.Reason = "unknown", "API success not confirmed"
		return r, fmt.Errorf("original_delete_unconfirmed chat_id=%d message_id=%d", chatID, msgID)
	}
	r.Status = "confirmed"
	if sentAt <= 0 || time.Unix(sentAt, 0).After(now) {
		r.Reason = "send date unknown; Telegram confirmed deletion"
	}
	return r, nil
}

// messageDateLocator is an additive capability; older locators have unknown send dates.
type messageDateLocator interface {
	RecordMessageDate(context.Context, int64, int, int64) error
	MessageDates(context.Context, int64, int) (storage.MessageDates, error)
}

func locatorMessageDate(locator Locator, chatID int64, msgID int) int64 {
	if dated, ok := locator.(messageDateLocator); ok {
		dates, err := dated.MessageDates(context.Background(), chatID, msgID)
		if err != nil {
			log.Printf("[WARN] message date unavailable: chat_id=%d message_id=%d: %v", chatID, msgID, err)
			return 0
		}
		return dates.MessageDate
	}
	return 0
}

func banResult(req banRequest) (actionResult, error) {
	if req.dry {
		return actionResult{Status: "not_attempted", Reason: "dry mode"}, nil
	}
	if req.training {
		return actionResult{Status: "not_attempted", Reason: "training mode"}, nil
	}
	if err := banUserOrChannel(req); err != nil {
		return failedAction(err), fmt.Errorf("ban_failed: %w", err)
	}
	if req.restrict && req.channelID == 0 {
		return actionResult{Status: "confirmed", Reason: "permissions restricted (soft ban)"}, nil
	}
	return actionResult{Status: "confirmed"}, nil
}

// finishModeration separates action failures from report delivery failures.
func (a *admin) finishModeration(text string, moderationErr error) error {
	if moderationErr != nil {
		log.Printf("[WARN] moderation_failed: %v", moderationErr)
		moderationErr = fmt.Errorf("moderation_failed: %w", moderationErr)
	}
	if err := send(tbapi.NewMessage(a.adminChatID, text), a.tbAPI); err != nil {
		return errors.Join(moderationErr, fmt.Errorf("notification_failed: %w", err))
	}
	return moderationErr
}

func moderationText(ban, original actionResult, cleanup string) string {
	return fmt.Sprintf("\n\nban: %s\noriginal delete: %s\ncleanup: %s", ban, original, cleanup)
}

func (a *admin) startCleanup(userID int64, name string) {
	go func() {
		result, err := a.deleteUserMessages(userID)
		var text strings.Builder
		fmt.Fprintf(&text, "cleanup for %q (%d): %s", escapeMarkDownV1Text(name), userID, result)
		for i, item := range result.Items {
			log.Printf("[INFO] cleanup item: chat_id=%d message_id=%d result=%s", item.ChatID, item.MessageID, item)
			if i >= 10 {
				continue
			}
			if item.Status != "confirmed" {
				fmt.Fprintf(&text, "\nmessage %d: %s", item.MessageID, item)
			}
		}
		if err != nil {
			text.WriteString("\ncleanup incomplete; see sanitized logs")
		}
		if reportErr := a.finishModeration(text.String(), err); reportErr != nil {
			log.Printf("[WARN] cleanup report: %v", reportErr)
		}
	}()
}

// reportAutomatic renders API outcomes independently of the listener's control authorization.
func (a *admin) reportAutomatic(name string, msg *bot.Message, ban, original actionResult,
	cleanup cleanupResult, allowControls bool) error {
	target := msg.From
	if msg.SenderChat.ID != 0 {
		target = bot.User{ID: msg.SenderChat.ID, Username: msg.SenderChat.UserName}
	}
	heading := fmt.Sprintf("moderation result for [%s](tg://user?id=%d)", escapeMarkDownV1Text(name), target.ID)
	if msg.SenderChat.ID != 0 {
		if msg.SenderChat.UserName != "" {
			heading = fmt.Sprintf("moderation result for [%s](https://t.me/%s)",
				escapeMarkDownV1Text(name), msg.SenderChat.UserName)
		} else {
			heading = fmt.Sprintf("moderation result for %s (%d)", escapeMarkDownV1Text(name), target.ID)
		}
	}
	originalText := msg.Text
	if msg.Quote != "" {
		originalText += "\n" + msg.Quote
	}
	originalText = strings.ReplaceAll(escapeMarkDownV1Text(originalText), "\n", " ")
	text := heading + "\n\n" + originalText + "\n\n**moderation results**" +
		escapeMarkDownV1Text(moderationText(ban, original, cleanup.String()))

	if allowControls {
		action := "change ban"
		if a.trainingMode {
			action = "review detection"
		}
		if err := a.sendWithUnbanMarkup(text, action, target, msg.ID, a.adminChatID); err != nil {
			return fmt.Errorf("notification_failed: %w", err)
		}
		return nil
	}
	return a.finishModeration(text, nil)
}
