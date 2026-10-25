package events

import (
	"fmt"
	"strings"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/moderation"
)

// Only the original update supplies authority. Shared Telegram From records and
// display names cannot identify the owner of a channel or an unknown author.
func hanOriginalIdentity(m *tbapi.Message) (id int64, kind, username string) {
	if m.SenderChat != nil {
		if m.SenderChat.ID < 0 {
			return m.SenderChat.ID, "sender_chat", m.SenderChat.UserName
		}
		return 0, "message", ""
	}
	if m.From != nil && m.From.ID > 0 && !sharedTelegramUser(m.From.ID) && m.From.ID != 777000 {
		return m.From.ID, "user", m.From.UserName
	}
	return 0, "message", ""
}

func (l *TelegramListener) enqueueHanAutomatic(update tbapi.Update, msg *bot.Message, resp bot.Response) (bool, error) {
	m := update.Message
	if m == nil || m.Chat.ID == 0 || m.MessageID <= 0 || m.MessageID != msg.ID || m.Chat.ID != msg.ChatID {
		return false, fmt.Errorf("han requires an exact original chat/message")
	}
	id, kind, name := hanOriginalIdentity(m)
	if l.SuperUsers.IsSuper(msg.From.Username, msg.From.ID) || l.SuperUsers.IsSuper(name, id) || id == m.Chat.ID {
		return false, nil
	}
	if checker, ok := l.Bot.(interface{ IsExplicitTrustedUser(int64) bool }); ok && id != 0 && checker.IsExplicitTrustedUser(id) {
		return false, nil
	}
	p := moderation.Plan{Origin: "detector", Card: true, SourceText: msg.Text, UserName: name,
		Key:    fmt.Sprintf("source:%d:%d:%s:%d", m.Chat.ID, m.MessageID, kind, id),
		ChatID: m.Chat.ID, MessageID: m.MessageID, IdentityKind: kind, IdentityID: id, Destination: l.adminChatID,
		Reason: hanModerationReason(resp.CheckResults, !resp.HanOnly)}
	if kind == "user" {
		p.DisplayName = msg.From.DisplayName
	}
	base := moderation.Action{ChatID: p.ChatID, IdentityKind: kind, IdentityID: id}
	del := base
	del.Kind, del.MessageID, del.MessageDate = "original_delete", m.MessageID, int64(m.Date)
	if del.MessageDate < 0 {
		del.MessageDate = 0
	}
	p.HanActions = []moderation.Action{del}
	if id != 0 {
		ban := base
		ban.Kind = "ban"
		if l.SoftBanMode && kind == "user" {
			ban.Kind = "restrict"
		}
		duration := resp.BanInterval
		if duration <= 0 {
			duration = bot.PermanentBanDuration
		}
		if duration < 30*time.Second {
			duration = time.Minute
		}
		ban.UntilDate = time.Now().Add(duration).Unix()
		p.HanActions = append(p.HanActions, ban)
		if !resp.HanOnly {
			p.Actions = append(p.Actions, ban, del)
			for _, check := range resp.CheckResults {
				if !check.Spam || check.Name == "han script" {
					continue
				}
				for _, messageID := range check.ExtraDeleteIDs {
					cleanup := base
					cleanup.Kind, cleanup.MessageID = "cleanup_delete", messageID
					cleanup.MessageDate = locatorMessageDate(l.Locator, p.ChatID, messageID)
					p.Actions = append(p.Actions, cleanup)
				}
			}
		}
	} else {
		p.Reason += "\nБлокировка по Han невозможна: оригинал не содержит надёжной числовой цели User/SenderChat."
	}
	return l.adminHandler.acceptRecovery(p, nil)
}

func newHanCard(c moderation.Card) bool {
	for _, demand := range c.Origins {
		if demand.Origin == moderation.HanOrigin {
			return true
		}
	}
	return false
}

func hanActionText(a moderation.Action) string {
	label := ""
	switch a.Kind {
	case "ban":
		label = "Блокировка: "
	case "restrict":
		label = "Ограничение сообщений: "
	case "original_delete":
		label = "Удаление: "
	}
	text := label + compactAction(a)
	if a.Uncertain && a.State != moderation.Confirmed && !strings.Contains(text, "не подтверждён") {
		text += ". Результат запроса не подтверждён"
	}
	return text
}
