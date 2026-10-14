package events

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/umputun/tg-spam/app/moderation"
)

func inertSpamText(s string) string {
	// break automatic links only in the displayed copy
	return strings.NewReplacer(".", ".\u200b", ":", ":\u200b", "@", "@\u200b", "/", "/\u200b").Replace(s)
}

func safeTelegramUsername(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func cardText(c moderation.Card) string { return compactCardText(c, "") }

func compactCardText(c moderation.Card, chatName string) string {
	var text strings.Builder
	heading := "🛡 Спам"
	if chatName != "" {
		heading += " — " + truncateRunes(chatName, 100)
	}
	name := c.DisplayName
	if name == "" {
		name = c.UserName
	}
	if name == "" {
		name = "Пользователь"
		if c.IdentityKind == "sender_chat" {
			name = "Канал"
		}
	}
	name = html.EscapeString(truncateRunes(name, 150))
	if c.IdentityKind == "user" && c.IdentityID > 0 {
		name = fmt.Sprintf(`<a href="tg://user?id=%d">%s</a>`, c.IdentityID, name)
	} else if c.IdentityKind == "sender_chat" && safeTelegramUsername(c.UserName) {
		name = fmt.Sprintf(`<a href="https://t.me/%s">%s</a>`, c.UserName, name)
	}
	fmt.Fprintf(&text, "%s\n\n%s\n«%s»\n", html.EscapeString(heading), name,
		html.EscapeString(truncateRunes(inertSpamText(c.Text), 650)))
	deleted, remaining, shown := 0, 0, 0
	for _, op := range currentCardActions(c) {
		if op.Kind == "card" || op.Kind == "notice" {
			continue
		}
		if op.Kind == "cleanup_delete" {
			if op.State == moderation.Confirmed {
				deleted++
			} else {
				remaining++
			}
			continue
		}
		if shown < 12 {
			text.WriteString("\n" + html.EscapeString(compactAction(op)))
		}
		shown++
	}
	if shown > 12 {
		text.WriteString("\nОстальные результаты — в подробностях")
	}
	if deleted > 0 {
		fmt.Fprintf(&text, "\nДополнительно удалено: %s", messageCount(deleted))
	}
	if remaining > 0 {
		fmt.Fprintf(&text, "\n⚠️ Дополнительная очистка не завершена: %s. Подробнее — в деталях", messageCount(remaining))
	}
	if c.Held {
		text.WriteString("\n⏸ Повторы отменены")
	}
	if c.IncompleteDecisions > 0 {
		text.WriteString("\n⚠️ Решение требует ручной проверки")
	}
	if strings.HasPrefix(c.Receipt, "delivery:") || strings.HasPrefix(c.Receipt, "command:") {
		text.WriteString("\n⚠️ Оригинал сообщения не установлен")
	}
	return text.String()
}

func compactAction(a moderation.Action) string {
	deleting := a.Kind == "original_delete"
	subject := "Пользователь"
	if a.IdentityKind == "sender_chat" {
		subject = "Канал"
	}
	switch a.State {
	case moderation.Confirmed:
		if deleting {
			return "✅ Сообщение удалено"
		}
		if a.Kind == "restrict" {
			return "✅ Отправка сообщений ограничена"
		}
		if a.Kind == "restore" {
			return "✅ Ограничения сняты"
		}
		if a.Kind == "unban" {
			return "✅ Блокировка снята"
		}
		if a.Kind != "ban" {
			return "✅ Действие выполнено"
		}
		if a.Code == "reconciled_membership" {
			return "✅ " + subject + " уже заблокирован"
		}
		if a.UntilDate == 0 || a.UntilDate > time.Now().Add(366*24*time.Hour).Unix() {
			return "✅ " + subject + " заблокирован навсегда"
		}
		return "✅ " + subject + " заблокирован до " + cardDate(a.UntilDate)
	case moderation.Pending, moderation.Running:
		if deleting {
			if a.State == moderation.Pending && a.Attempts > 0 && a.Attempts < 320 && a.NextAt > 0 &&
				a.NextAt+15000 < a.Deadline && a.Deadline > time.Now().Add(15*time.Second).UnixMilli() {
				return "⏳ Удаление не подтверждено. Повторим автоматически"
			}
			return "⏳ Удаление ожидает выполнения"
		}
		switch a.Kind {
		case "ban":
			return "⏳ Блокировка ожидает выполнения"
		case "restrict":
			return "⏳ Ограничение сообщений ожидает выполнения"
		default:
			return "⏳ Действие ожидает выполнения"
		}
	case moderation.Absent:
		return "ℹ️ Сообщение отсутствует или недоступно; удаление ботом не подтверждено"
	case moderation.Expired:
		if deleting && a.MessageDate > 0 && time.Now().Unix() >= a.MessageDate+48*60*60 {
			return "⚠️ Требуется ручное удаление: окно удаления ботом истекло"
		}
		return "⚠️ Требуется ручная проверка: срок выполнения истёк"
	case moderation.Canceled:
		return "⏸ Действие отменено"
	case moderation.Blocked:
		return "⚠️ Действие остановлено защитой модерации"
	case moderation.Manual:
		if deleting && a.MessageDate == 0 {
			return "⚠️ Дата или оригинал неизвестны; требуется ручная проверка"
		}
		return "⚠️ Результат не установлен; требуется ручная проверка"
	case moderation.Failed:
		if a.Uncertain {
			return "⚠️ Результат не подтверждён; требуется ручная проверка"
		}
		if deleting {
			return "⚠️ Удаление отклонено; требуется ручная работа"
		}
		return "⚠️ Действие отклонено; требуется ручная проверка"
	default:
		return "⚠️ Результат неизвестен"
	}
}

func warningSummary(counts map[string]int, rows []moderation.Action, rejected int64) string {
	problems := 0
	for key, n := range counts {
		kind, state, ok := strings.Cut(key, "=")
		if !ok || kind == "notice" {
			continue
		}
		if state == moderation.Failed || state == moderation.Expired || state == moderation.Manual ||
			state == moderation.Blocked || state == moderation.Undelivered {
			problems += n
		}
	}
	for _, a := range rows {
		if a.Kind != "card" && a.Kind != "notice" && (a.State == moderation.Pending || a.State == moderation.Running) &&
			(a.Attempts > 0 && a.State == moderation.Pending || a.Attempts > 1 || a.Uncertain) {
			problems++
		}
	}
	if problems == 0 && rejected == 0 {
		return ""
	}
	text := "⚠️ Модерация требует внимания"
	if problems > 0 {
		text += fmt.Sprintf("\nПроблемных операций: %d", problems)
	}
	if rejected > 0 {
		text += fmt.Sprintf("\nНе принято из-за переполнения: %d", rejected)
	}
	return text + "\nРезультаты — в карточках; полный список — в журнале"
}

func russianActionLabel(action string) string {
	switch action {
	case "change ban":
		return "Изменить бан"
	case "review detection":
		return "Проверить детекцию"
	default:
		return action
	}
}

func messageCount(n int) string {
	word := "сообщений"
	if n%100 < 11 || n%100 > 14 {
		switch n % 10 {
		case 1:
			word = "сообщение"
		case 2, 3, 4:
			word = "сообщения"
		}
	}
	return fmt.Sprintf("%d %s", n, word)
}

func truncateTelegramText(s string, limit int) string {
	units := 0
	for i, r := range s {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > limit-1 {
			return s[:i] + "…"
		}
		units += width
	}
	return s
}
