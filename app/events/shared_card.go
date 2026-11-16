package events

import (
	"fmt"
	"html"
	"slices"
	"strconv"
	"strings"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/umputun/tg-spam/app/moderation"
)

func sharedViewCard(view moderation.SharedView, peers []int64, now time.Time) (string, tbapi.InlineKeyboardMarkup) {
	c := view.Case
	text, keyboard := sharedCard(c, peers, now)
	if len(c.Reports) > 0 && c.Reports[0].Delivery.MessageID > 0 {
		text = fmt.Sprintf("Пересылка №%d.\n", c.Reports[0].Delivery.MessageID) + text
	}

	return text, keyboard
}

func sharedCard(c moderation.SharedCase, peers []int64, now time.Time) (string, tbapi.InlineKeyboardMarkup) {
	if c.Canonical != "" {
		anchor := fmt.Sprintf("№%d", c.CanonicalAnchor)
		chat := strconv.FormatInt(c.AdminID, 10)
		if strings.HasPrefix(chat, "-100") && c.CanonicalAnchor > 0 {
			anchor = fmt.Sprintf("<a href=\"https://t.me/c/%s/%d\">№%d</a>", chat[4:], c.CanonicalAnchor, c.CanonicalAnchor)
		}
		return "Найден тот же сохранённый набор оригиналов. Используйте общую карточку " + anchor + ". Новых действий нет.",
			tbapi.InlineKeyboardMarkup{InlineKeyboard: [][]tbapi.InlineKeyboardButton{}}
	}
	var out strings.Builder
	out.WriteString("Ручная модерация — общее обращение.\n")
	if c.NeedsReview {
		out.WriteString("После поиска получен отказ. Нужен новый явный выбор; автоматического повтора нет.\n")
	}
	canChoose := !c.Decided && len(c.PriorIntents) <= 20 && c.Expires > now.UnixMilli()
	if c.Decided {
		out.WriteString("Выбранный набор сохранён. Поздние результаты не расширяют решение.\n")
	} else if len(c.PriorIntents) > 20 {
		out.WriteString("Достигнут лимит пересмотра. Нужны новая пересылка и явное решение. История сохранена.\n")
	} else if !canChoose {
		out.WriteString("Срок выбора истёк. Новая попытка требует новой пересылки и явного решения.\n")
	} else {
		out.WriteString("Выберите перечисленные оригиналы; совпадение текста не подтверждает автора.\n")
	}
	button := func(label, verb string) tbapi.InlineKeyboardButton {
		return tbapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("S:%s:%s:%d", verb, c.Ref, c.Revision))
	}
	rows := [][]tbapi.InlineKeyboardButton{}
	for _, r := range c.Reports {
		name := html.EscapeString(truncateRunes(r.Name, 32))
		if r.SearchError != "" {
			fmt.Fprintf(&out, "%s — поиск недоступен; действий нет.\n", name)
			continue
		}
		if len(r.Targets) == 0 {
			fmt.Fprintf(&out, "%s — не найдено.\n", name)
			continue
		}
		confirmed, blocked := 0, 0
		for _, t := range r.Targets {
			if t.DeleteConfirmed && (t.IdentityID == 0 || t.BanConfirmed) {
				confirmed++
			}
			if t.Blocked != "" {
				blocked++
			}
		}
		label := fmt.Sprintf("найдено: %d", len(r.Targets))
		if confirmed == len(r.Targets) {
			label = fmt.Sprintf("ранее обработано: %d сообщения", confirmed)
		}
		if blocked > 0 {
			label += fmt.Sprintf("; запрещено: %d", blocked)
		}
		selected := false
		for _, choice := range c.Choices {
			selected = selected || choice.Report.Peer == r.Peer
		}
		for _, intent := range c.PriorIntents {
			for _, choice := range intent.Choices {
				selected = selected || choice.Peer == r.Peer
			}
		}
		if selected {
			deletes, bans := sharedOutcomes(r)
			label = fmt.Sprintf("удаление: %s; блокировка: %s", deletes, bans)
		} else if c.Decided {
			label += "; не выбрано"
		}
		if c.NeedsReview && r.State != "accepted" {
			del, bans := sharedPendingFacts(r)
			label = fmt.Sprintf("удаление: %s; блокировка: %s", del, bans)
		} else if len(r.Refusals) > 0 {
			label += fmt.Sprintf("; прежних отказов: %d", len(r.Refusals))
		}
		fmt.Fprintf(&out, "%s — %s.\n", name, label)
		if canChoose && blocked < len(r.Targets) {
			prefix := "Обработать: "
			if c.NeedsReview || len(r.Refusals) > 0 {
				prefix = "Новая попытка: "
			}
			rows = append(rows, tbapi.NewInlineKeyboardRow(button(prefix+truncateRunes(r.Name, 32), fmt.Sprint(r.Peer))))
		}
	}
	missing := 0
	for _, peer := range peers {
		if !slices.ContainsFunc(c.Reports, func(r moderation.SharedReport) bool { return r.Peer == peer }) {
			missing++
		}
	}
	if missing > 0 {
		fmt.Fprintf(&out, "Ожидается ответ инстансов: %d. Доступен выбор полученных результатов.\n", missing)
	}
	if canChoose {
		label := "Обработать все перечисленные допустимые цели"
		if c.NeedsReview || slices.ContainsFunc(c.Reports, func(r moderation.SharedReport) bool { return len(r.Refusals) > 0 }) {
			label = "Новая попытка: все перечисленные цели"
		}
		rows = append(rows, tbapi.NewInlineKeyboardRow(button(label, "all")))
	}
	if c.Page == 0 {
		rows = append(rows, tbapi.NewInlineKeyboardRow(button("Подробнее", "v1")))
	} else {
		out.WriteString("\nПодробнее (отдельные оригиналы и история):\n")
		type item struct {
			report moderation.SharedReport
			index  int
		}
		items := []item{}
		for _, r := range c.Reports {
			for i := range r.Targets {
				items = append(items, item{r, i})
			}
		}
		targetPages := (len(items) + 4) / 5
		if c.Page <= targetPages && c.ReviewReason != "" {
			out.WriteString(html.EscapeString(truncateRunes(c.ReviewReason, 200)) + "\n")
		}
		start := (c.Page - 1) * 5
		if start < len(items) {
			for _, entry := range items[start:min(start+5, len(items))] {
				r, t := entry.report, entry.report.Targets[entry.index]
				fmt.Fprintf(&out, "%s (chat %d), оригинал №%d: %s; источник: %s; delete-deadline: %s.\n",
					html.EscapeString(truncateRunes(r.Name, 32)), r.ChatID, t.MessageID,
					html.EscapeString(r.Reason), html.EscapeString(t.Evidence),
					time.UnixMilli(t.Deadline).UTC().Format(time.RFC3339))
				if t.Blocked != "" {
					out.WriteString("Отказ: " + html.EscapeString(forwardReason(t.Blocked)) + ".\n")
				}
				if t.IdentityID == 0 {
					out.WriteString("Числовой автор не доказан: допустимо только точное удаление.\n")
				}
				for _, a := range r.Actions {
					if a.MessageID == t.MessageID || a.MessageID == 0 && a.IdentityID == t.IdentityID {
						fmt.Fprintf(&out, "%s: %s.\n", html.EscapeString(a.Kind), html.EscapeString(forwardActionLabel(a)))
					}
				}
				for _, refusal := range r.Refusals {
					if refusal.MessageID == t.MessageID || refusal.MessageID == 0 && refusal.IdentityID == t.IdentityID {
						fmt.Fprintf(&out, "Прежний %s: %s.\n", html.EscapeString(refusal.Kind), html.EscapeString(forwardReason(refusal.Code)))
					}
				}
				if canChoose && t.Blocked == "" {
					label := fmt.Sprintf("%s: №%d", truncateRunes(r.Name, 24), t.MessageID)
					rows = append(rows, tbapi.NewInlineKeyboardRow(button(label, fmt.Sprintf("%d.%d", r.Peer, entry.index))))
				}
			}
		}
		if c.Page > targetPages && c.Page-targetPages-1 < len(c.PriorIntents) {
			intent := c.PriorIntents[c.Page-targetPages-1]
			fmt.Fprintf(&out, "Прежнее решение: оператор %d, %s, automatic=%t.\n%s\n", intent.Operator,
				time.UnixMilli(intent.DecidedAt).UTC().Format(time.RFC3339), intent.Automatic,
				html.EscapeString(truncateRunes(intent.Reason, 200)))
			for _, choice := range intent.Choices {
				fmt.Fprintf(&out, "Инстанс %d, оригиналы: %v.\n", choice.Peer, choice.MessageIDs)
			}
		}
		nav := []tbapi.InlineKeyboardButton{button("Кратко", "v0")}
		if c.Page > 1 {
			nav = append(nav, button("←", fmt.Sprintf("v%d", c.Page-1)))
		}
		if c.Page < targetPages+len(c.PriorIntents) {
			nav = append(nav, button("→", fmt.Sprintf("v%d", c.Page+1)))
		}
		rows = append(rows, nav)
	}
	return out.String(), tbapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func sharedPendingFacts(r moderation.SharedReport) (deletion, ban string) {
	deletion = "нужен новый выбор"
	ban = "нужен новый выбор"
	del := 0
	known := map[int64]bool{}
	all := map[int64]bool{}
	for _, t := range r.Targets {
		if t.DeleteConfirmed {
			del++
		}
		if t.IdentityID != 0 {
			all[t.IdentityID] = true
			if t.BanConfirmed {
				known[t.IdentityID] = true
			}
		}
	}
	if del == len(r.Targets) {
		deletion = fmt.Sprintf("ранее подтверждено: %d", del)
	}
	if len(all) == 0 {
		ban = "нет доказанной числовой цели"
	} else if len(known) == len(all) {
		ban = fmt.Sprintf("ранее подтверждено: %d", len(known))
	}
	return deletion, ban
}

func sharedOutcomes(r moderation.SharedReport) (deletion, ban string) {
	if r.State == "expired" {
		return "отказ: срок выбора истёк", "отказ: срок выбора истёк"
	}
	if r.State != "accepted" {
		return "ожидание локального журнала", "ожидание локального журнала"
	}
	deletes, bans := []string{}, []string{}
	for _, a := range r.Actions {
		label := "отказ"
		switch {
		case a.Uncertain:
			label = "результат неизвестен"
		case a.State == moderation.Confirmed:
			label = "подтверждено"
		case a.State == moderation.Absent:
			label = "уже отсутствует; удаление ботом не подтверждено"
		case a.State == moderation.Pending || a.State == moderation.Running:
			label = "ожидается"
		}
		switch a.Kind {
		case "original_delete", "cleanup_delete":
			deletes = append(deletes, label)
		case "ban", "restrict":
			bans = append(bans, label)
		}
	}
	summarize := func(labels []string) string {
		if len(labels) == 0 {
			return "нет разрешённого действия"
		}
		all := true
		for _, v := range labels {
			all = all && v == "подтверждено"
		}
		if all {
			return fmt.Sprintf("подтверждено: %d", len(labels))
		}
		unique := []string{}
		counts := map[string]int{}
		for _, v := range labels {
			if counts[v] == 0 {
				unique = append(unique, v)
			}
			counts[v]++
		}
		for i, v := range unique {
			unique[i] = fmt.Sprintf("%s: %d", v, counts[v])
		}
		return strings.Join(unique, "; ")
	}
	return summarize(deletes), summarize(bans)
}
