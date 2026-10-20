package events

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/umputun/tg-spam/app/moderation"
)

func sharedTelegramUser(id int64) bool { return id == 136817688 || id == 1087968824 }

// procSenderSpam intercepts channel commands before ordinary filtering can silently consume them.
func (l *TelegramListener) procSenderSpam(u tbapi.Update) bool {
	m := u.Message
	if m == nil || m.Chat.ID != l.chatID || !strings.EqualFold(strings.TrimSpace(m.Text), "/spam") {
		return false
	}
	if m.SenderChat == nil && m.From != nil && !sharedTelegramUser(m.From.ID) {
		return false
	}
	a := l.adminHandler
	if a == nil {
		return true
	}
	sender := m.Chat.ID
	if m.SenderChat != nil && m.SenderChat.ID != 0 {
		sender = m.SenderChat.ID
	}
	text := "Не удалось принять /spam: необходимы recovery journal и админ-чат. Действия не выполнялись."
	if a.recovery != nil && a.adminChatID != 0 {
		text = a.senderSpamRequest(u, sender)
		scope := "no-target"
		if m.ReplyToMessage != nil {
			scope = fmt.Sprint(m.ReplyToMessage.MessageID)
		}
		fresh, err := a.recovery.journal.ClaimCommandReply(context.Background(), sender, scope, time.Now())
		if err != nil {
			log.Printf("[WARN] sender /spam feedback throttle unavailable: message_id=%d", m.MessageID)
			if time.Now().Before(l.senderFeedbackAt) {
				return true
			}
			l.senderFeedbackAt = time.Now().Add(time.Minute)
		} else if !fresh {
			return true
		}
	} else {
		if time.Now().Before(l.senderFeedbackAt) {
			return true
		}
		l.senderFeedbackAt = time.Now().Add(time.Minute)
	}
	reply := tbapi.NewMessage(m.Chat.ID, text)
	reply.ReplyParameters = tbapi.ReplyParameters{MessageID: m.MessageID}
	if _, err := a.tbAPI.Send(reply); err != nil {
		log.Printf("[WARN] sender /spam feedback unavailable: message_id=%d", m.MessageID)
	}
	return true
}

func (a *admin) senderSpamRequest(u tbapi.Update, sender int64) string {
	m := u.Message
	orig := m.ReplyToMessage
	deny := func(reason string) string {
		return "Не удалось выполнить /spam: " + reason + ". Модерация и обучение не выполнялись."
	}
	if a.dry || a.trainingMode {
		return deny("режим dry/training не допускает подтверждаемую модерацию")
	}
	if orig == nil {
		return deny("ответьте на оригинал сообщения")
	}
	if orig.Chat.ID != a.primChatID || orig.MessageID <= 0 || orig.ForumTopicCreated != nil {
		return deny("оригинал не относится к защищаемому чату")
	}
	id := int64(0)
	kind := "user"
	name := ""
	if orig.SenderChat != nil {
		id = orig.SenderChat.ID
		kind = "sender_chat"
		name = orig.SenderChat.UserName
		if id >= 0 || id == a.primChatID {
			return deny("нет надёжной цели для модерации")
		}
	} else if orig.From != nil && orig.From.ID > 0 && !orig.From.IsBot && !sharedTelegramUser(orig.From.ID) {
		id = orig.From.ID
		name = orig.From.UserName
	}
	if id == 0 {
		return deny("Telegram не передал надёжный числовой ID автора оригинала")
	}
	if a.superUsers.IsSuper(name, id) || a.targetExplicitlyApproved(id) {
		return deny("цель защищена политикой полномочий или явного доверия")
	}
	source := transform(orig).Text
	if orig.Quote != nil && orig.Quote.Text != "" {
		source += "\n" + orig.Quote.Text
	}
	p := moderation.Plan{Key: fmt.Sprintf("source:%d:%d:%s:%d", a.primChatID, orig.MessageID, kind, id),
		ChatID: a.primChatID, MessageID: orig.MessageID, IdentityID: id, IdentityKind: kind, Destination: a.adminChatID,
		DeliveryChat: m.Chat.ID, DeliveryMessage: m.MessageID, Card: true, SourceText: source, UserName: name,
		RequesterID: sender, Origin: fmt.Sprintf("report:sender:%d", sender)}
	base := moderation.Action{Kind: "original_delete", ChatID: p.ChatID, MessageID: p.MessageID, IdentityID: id, IdentityKind: kind,
		MessageDate: int64(orig.Date), State: moderation.Manual, Code: "sender_command_pending_confirmation"}
	p.Actions = []moderation.Action{base}
	if a.aggressiveCleanup {
		scoped, ok := a.locator.(interface {
			GetUserMessageIDsInChat(context.Context, int64, int64, int) ([]int, error)
		})
		if ok {
			ids, err := scoped.GetUserMessageIDsInChat(context.Background(), id, a.primChatID, a.aggressiveCleanupLimit)
			if err != nil {
				return deny("не удалось безопасно выбрать сообщения для очистки")
			}
			for _, mid := range ids {
				if mid == orig.MessageID {
					continue
				}
				item := base
				item.Kind = "cleanup_delete"
				item.MessageID = mid
				item.MessageDate = locatorMessageDate(a.locator, a.primChatID, mid)
				p.Actions = append(p.Actions, item)
			}
		}
	}
	_, _, created, err := a.recovery.journal.Accept(context.Background(), p, time.Now())
	if err != nil {
		log.Printf("[WARN] sender /spam request rejected: chat_id=%d message_id=%d", p.ChatID, m.MessageID)
		return deny("запрос не сохранён: лимит запросов либо недоступен журнал")
	}
	log.Printf("[INFO] sender /spam request recorded: command_id=%d original_id=%d target_kind=%s",
		m.MessageID, orig.MessageID, kind)
	if !created {
		return "Цель /spam уже зарегистрирована в журнале. Проверьте карточку в админ-чате; повторных действий не создано."
	}
	return "Команда /spam принята. Запрос ожидает подтверждения в админ-чате (15 минут). " +
		"До подтверждения модерация и обучение не выполняются."
}
