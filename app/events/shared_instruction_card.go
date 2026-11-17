package events

import (
	"fmt"
	"html"
	"slices"
	"strings"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/umputun/tg-spam/app/moderation"
)

type instructionResult struct {
	deleted, absent, previous, banned, priorBans int
	restricted, priorRestricted                  int
	waiting, failed, unknown                     int
}

// Only journal outcomes establish success. Matching text and saved intentions
// are never evidence that a Telegram effect happened.
func instructionOutcomes(r moderation.SharedReport) instructionResult {
	var result instructionResult
	seen := map[string]bool{}
	for _, a := range r.Actions {
		key := fmt.Sprintf("delete:%d", a.MessageID)
		ban := a.Kind == "ban" || a.Kind == "restrict"
		if ban {
			key = fmt.Sprintf("%s:%d", a.IdentityKind, a.IdentityID)
		} else if a.Kind != "original_delete" {
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		switch {
		case a.Uncertain:
			result.unknown++
		case a.State == moderation.Confirmed:
			switch {
			case a.Kind == "restrict" && (a.Receipt != r.Receipt || a.Code == "reconciled_membership"):
				result.priorRestricted++
			case a.Kind == "restrict":
				result.restricted++
			case ban && (a.Receipt != r.Receipt || a.Code == "reconciled_membership"):
				result.priorBans++
			case ban:
				result.banned++
			case a.Receipt != r.Receipt:
				result.previous++
			default:
				result.deleted++
			}
		case a.State == moderation.Absent && !ban:
			result.absent++
		case a.State == moderation.Pending || a.State == moderation.Running:
			result.waiting++
		default:
			result.failed++
		}
	}
	for _, target := range r.Targets {
		if target.Blocked != "" {
			result.failed++
			continue
		}
		if r.State != "accepted" {
			continue
		}
		if !seen[fmt.Sprintf("delete:%d", target.MessageID)] {
			result.waiting++
		}
		key := fmt.Sprintf("%s:%d", target.IdentityKind, target.IdentityID)
		if target.IdentityID != 0 && !seen[key] {
			result.waiting++
			seen[key] = true
		}
	}
	return result
}

func instructionSummary(r instructionResult) string {
	parts := []string{}
	for _, entry := range []struct {
		n     int
		label string
	}{
		{r.deleted, "удалено ботом"}, {r.absent, "уже отсутствовало или недоступно"},
		{r.previous, "ранее обработано"}, {r.banned, "авторов заблокировано"},
		{r.priorBans, "авторов ранее заблокировано"}, {r.restricted, "авторов с ограниченной отправкой"},
		{r.priorRestricted, "авторов с прежним ограничением отправки"}, {r.waiting, "ожидает выполнения или повтора"},
		{r.failed, "не удалось"}, {r.unknown, "результат неизвестен"},
	} {
		if entry.n > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d", entry.label, entry.n))
		}
	}
	return strings.Join(parts, "; ")
}

func sharedInstructionCard(c moderation.SharedCase, peers []int64, names map[int64]string,
	now time.Time) (string, tbapi.InlineKeyboardMarkup) {
	var body strings.Builder
	waiting, partial, found := false, false, false
	for _, r := range c.Reports {
		name := html.EscapeString(truncateRunes(r.Name, 40))
		result := instructionOutcomes(r)
		label := instructionSummary(result)
		switch {
		case r.SearchError != "":
			label = "поиск недоступен; чат не проверен"
			partial = true
		case len(r.Targets) == 0:
			label = "совпадений не найдено"
		case r.State != "accepted":
			found = true
			refused := slices.ContainsFunc(c.Choices, func(ch moderation.SharedChoice) bool {
				return ch.Report.Peer == r.Peer && ch.Refusal != ""
			})
			switch {
			case refused:
				label = "не удалось: прежний отказ изменился; нужна новая пересылка"
				partial = true
			case result.failed > 0:
				label = "отказ: " + html.EscapeString(forwardReason(r.Targets[0].Blocked))
				partial = true
			case c.Expires <= now.UnixMilli():
				label = "срок поручения истёк; новых действий нет"
				partial = true
			default:
				label = "принято, ожидает обработки"
				waiting = true
			}
		default:
			found = true
			waiting = waiting || result.waiting > 0
			partial = partial || result.failed > 0 || result.unknown > 0
			if label == "" {
				label = "подтверждённых результатов пока нет"
				partial = true
			}
		}
		fmt.Fprintf(&body, "%s: %s.\n", name, label)
	}
	for _, peer := range peers {
		if slices.ContainsFunc(c.Reports, func(r moderation.SharedReport) bool { return r.Peer == peer }) {
			continue
		}
		name := names[peer]
		if name == "" {
			name = fmt.Sprintf("Чат бота %d", peer)
		}
		fmt.Fprintf(&body, "%s: ещё не проверен.\n", html.EscapeString(truncateRunes(name, 40)))
		if c.Expires > now.UnixMilli() {
			waiting = true
		} else {
			partial = true
		}
	}
	title := "✅ Спам обработан\n"
	switch {
	case partial:
		title = "Обработано частично\n"
	case waiting:
		title = "Принято. Обрабатываю найденный спам…\n"
	case !found:
		title = "Совпадений не найдено\n"
	}
	button := func(label string, page int) tbapi.InlineKeyboardButton {
		return tbapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("S:v%d:%s:%d", page, c.Ref, c.Revision))
	}
	rows := [][]tbapi.InlineKeyboardButton{{button("Подробнее", 1)}}
	if c.Page > 0 {
		body.WriteString("\nПодробнее:\n")
		type item struct {
			r moderation.SharedReport
			t moderation.ForwardTarget
		}
		items := []item{}
		for _, r := range c.Reports {
			for _, target := range r.Targets {
				items = append(items, item{r, target})
			}
		}
		start := (c.Page - 1) * 4
		if start < len(items) {
			for _, entry := range items[start:min(start+4, len(items))] {
				fmt.Fprintf(&body, "%s, оригинал №%d; срок удаления %s.\n",
					html.EscapeString(truncateRunes(entry.r.Name, 32)), entry.t.MessageID,
					time.UnixMilli(entry.t.Deadline).UTC().Format(time.RFC3339))
				if entry.t.IdentityID == 0 {
					body.WriteString("Автор не доказан: только точное удаление.\n")
				}
				if entry.t.Blocked != "" {
					body.WriteString("Отказ: " + html.EscapeString(forwardReason(entry.t.Blocked)) + ".\n")
				}
				for _, a := range entry.r.Actions {
					if a.MessageID == entry.t.MessageID || a.MessageID == 0 && a.IdentityID == entry.t.IdentityID {
						fmt.Fprintf(&body, "%s: %s.\n", html.EscapeString(a.Kind), html.EscapeString(forwardActionLabel(a)))
					}
				}
			}
		}
		nav := []tbapi.InlineKeyboardButton{button("Кратко", 0)}
		if c.Page > 1 {
			nav = append(nav, button("←", c.Page-1))
		}
		if start+4 < len(items) {
			nav = append(nav, button("→", c.Page+1))
		}
		rows = [][]tbapi.InlineKeyboardButton{nav}
	}
	return title + body.String(), tbapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}
