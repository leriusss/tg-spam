package events

import (
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/umputun/tg-spam/app/moderation"
)

type detailSection struct{ title, body string }

var (
	hanDetailPattern  = regexp.MustCompile(`^Преобладает письменность Han: (\d+) из (\d+) букв \(([\d.]+)%\); порог ([\d.]+)%, минимум (\d+)`)
	credentialPattern = regexp.MustCompile(`(?i)(?:https?://[^\s<>]*bot\d+:[^\s<>]+|\b\d{5,}:[a-z0-9_-]{15,}|bearer\s+[a-z0-9._-]+|(?:token|api_key|password|secret|authorization)\s*[=:]\s*[^\s&<>]+)`)
	detailLinkPattern = regexp.MustCompile(`(?i)(?:https?://[^\s<>]+|tg://[^\s<>]+|@[a-z0-9_]+|[a-z0-9._+-]+@[a-z0-9.-]+\.[a-z]{2,}|(?:[\p{L}0-9-]+\.)+[\p{L}]{2,}(?:[/?:][^\s<>]*)?)`)
)

func safeDetail(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = credentialPattern.ReplaceAllString(s, "[секрет скрыт]")
	return html.EscapeString(detailLinkPattern.ReplaceAllStringFunc(s, inertSpamText))
}

func detailActor(c moderation.Card) string {
	if c.IdentityKind == "sender_chat" && c.IdentityID < 0 {
		return "Канал"
	}
	if c.IdentityKind == "user" && c.IdentityID > 0 && !sharedTelegramUser(c.IdentityID) && c.IdentityID != 777000 {
		return "Пользователь"
	}
	return "Автор не установлен"
}

func detailActionName(a moderation.Action) string {
	switch a.Kind {
	case "original_delete":
		return "Удаление сообщения"
	case "cleanup_delete":
		return "Очистка других сообщений"
	case "ban":
		return "Блокировка автора"
	case "restrict":
		return "Ограничение сообщений автора"
	case "unban":
		return "Снятие блокировки"
	case "restore":
		return "Снятие ограничений"
	case "learning_note":
		return "Обучение"
	case "notice", "card":
		return "Доставка уведомления"
	default:
		return "Дополнительное действие"
	}
}

func detailRetry(a moderation.Action, now time.Time) bool {
	return a.State == moderation.Pending && a.Attempts > 0 && a.Attempts < 320 && a.Deadline > 0 &&
		now.Add(15*time.Second).UnixMilli() < a.Deadline && a.NextAt+15000 < a.Deadline
}

func detailRequestPlaceholder(a moderation.Action) bool {
	return a.State == moderation.Manual && slices.Contains([]string{
		"sender_command_pending_confirmation", "delete_pending_confirmation", "manual_report_pending_decision",
	}, a.Code)
}

func detailCardStatus(c moderation.Card, a moderation.Action, now time.Time) string {
	if detailRequestPlaceholder(a) {
		switch c.RequestState {
		case "expired":
			return "не разрешено: срок подтверждения запроса истёк"
		case "reject_report", "cancel", "dismissed":
			return "не разрешено: запрос отклонён"
		}
	}
	return detailStatus(a, now)
}

func detailStatus(a moderation.Action, now time.Time) string {
	if a.Uncertain && a.State != moderation.Confirmed {
		if a.State == moderation.Running {
			return "выполняется; результат предыдущего запроса неизвестен"
		}
		if detailRetry(a, now) {
			return "результат предыдущего запроса неизвестен; назначен повтор"
		}
		return "результат неизвестен; требуется ручная проверка"
	}
	switch a.State {
	case moderation.Confirmed:
		if a.Kind == "ban" && a.Code == "reconciled_membership" {
			return "Telegram подтвердил уже существующую блокировку"
		}
		return "подтверждено"
	case moderation.Pending:
		if a.Deadline > 0 && now.Add(15*time.Second).UnixMilli() >= a.Deadline {
			return "ожидается, но доступный срок уже истёк"
		}
		if detailRetry(a, now) {
			return "назначен повтор"
		}
		return "ожидается"
	case moderation.Running:
		return "выполняется"
	case moderation.Absent:
		return "сообщение уже отсутствует или недоступно; удаление ботом не подтверждено"
	case moderation.Failed:
		return "отклонено"
	case moderation.Expired:
		return "срок истёк"
	case moderation.Canceled:
		return "отменено до выполнения"
	case moderation.Blocked:
		return "остановлено правилами безопасности; требуется ручная проверка"
	case moderation.Manual:
		switch a.Code {
		case "sender_command_pending_confirmation", "delete_pending_confirmation", "manual_report_pending_decision":
			return "ожидает решения оператора"
		default:
			return "требуется ручная проверка"
		}
	default:
		return "результат неизвестен"
	}
}

func detailRequestOnly(c moderation.Card) bool {
	if c.RequestState != "pending" {
		return false
	}
	for _, a := range currentCardActions(c) {
		if a.Kind != "notice" && a.Kind != "card" && a.State != moderation.Manual && a.State != moderation.Canceled {
			return false
		}
	}
	return true
}

func detailResult(c moderation.Card, now time.Time) string {
	if detailRequestOnly(c) {
		return "Запрос ожидает подтверждения"
	}
	idle := c
	idle.RequestState = "pending"
	if c.RequestState != "" && detailRequestOnly(idle) {
		switch c.RequestState {
		case "expired":
			return "Срок подтверждения запроса истёк"
		case "reject_report", "cancel", "dismissed":
			return "Запрос отклонён"
		}
	}
	deleted, failed, manual := false, false, c.IncompleteDecisions > 0
	var membership moderation.Action
	var original moderation.Action
	count, confirmed, canceled := 0, 0, 0
	for _, a := range currentCardActions(c) {
		if a.Kind == "notice" || a.Kind == "card" {
			continue
		}
		count++
		if a.State == moderation.Confirmed {
			confirmed++
		}
		if a.State == moderation.Canceled && !a.Uncertain {
			canceled++
		}
		if a.Kind == "original_delete" && a.MessageID == c.MessageID {
			original = a
			deleted = a.State == moderation.Confirmed
		}
		if slices.Contains([]string{"ban", "restrict", "unban", "restore"}, a.Kind) {
			membership = a
		}
		failed = failed || a.State == moderation.Failed || a.State == moderation.Expired || a.State == moderation.Absent
		manual = manual || !slices.Contains(c.Resolved, a.ID) &&
			(a.State == moderation.Manual || a.State == moderation.Blocked ||
				a.Uncertain && !detailRetry(a, now) && a.State != moderation.Confirmed && a.State != moderation.Running)
	}
	if manual {
		return "Требуется ручная проверка"
	}
	if failed {
		if confirmed == 0 {
			return "Действия не выполнены; проверьте результаты"
		}
		return "Часть действий не выполнена"
	}
	if canceled > 0 && canceled < count {
		return "Часть действий отменена до выполнения"
	}
	if deleted {
		if membership.State == moderation.Confirmed {
			switch membership.Kind {
			case "ban":
				return "Сообщение удалено, автор заблокирован"
			case "restrict":
				return "Сообщение удалено, отправка сообщений ограничена"
			case "unban", "restore":
				return "Сообщение удалено, снятие блокировки подтверждено"
			}
		}
		if membership.Kind == "ban" || membership.Kind == "restrict" {
			return "Сообщение удалено, блокировка ожидается"
		}
		return "Удаление сообщения подтверждено"
	}
	if count == 0 {
		return "Сведения о результатах не сохранены"
	}
	if canceled == count {
		return "Действия отменены до выполнения"
	}
	if membership.State == moderation.Confirmed {
		if membership.Kind == "unban" || membership.Kind == "restore" {
			return "Снятие блокировки подтверждено"
		}
		if original.Kind != "" {
			return "Блокировка подтверждена, удаление ожидается"
		}
		return "Блокировка или ограничение подтверждены"
	}
	return "Результаты действий указаны ниже"
}

func detailReasons(c moderation.Card) string {
	lines, seen := []string{}, map[string]bool{}
	if c.RequestState != "" {
		lines = append(lines, "Операторский запрос: "+requestLabel(c))
	} else if c.DeleteOnly {
		lines = append(lines, "Операторский запрос удаления. Состояние подтверждения не сохранено.")
	} else {
		for _, d := range c.Origins {
			if strings.HasPrefix(d.Origin, "report") || strings.HasPrefix(d.Origin, "operator") ||
				strings.HasPrefix(d.Origin, "forward") {
				lines = append(lines, "Сохранена жалоба или явное решение оператора; автоматические причины указаны отдельно.")
				break
			}
		}
	}
	for line := range strings.SplitSeq(c.Reason, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		if match := hanDetailPattern.FindStringSubmatch(line); len(match) == 6 {
			line = fmt.Sprintf("Сработало правило Han: %s из %s букв — %s%%.\n"+
				"Порог: от %s%% и минимум %s букв Han.\nHan — письменность, а не установленный язык автора.",
				match[1], match[2], strings.TrimSuffix(match[3], ".0"), strings.TrimSuffix(match[4], ".0"), match[5])
		} else if !strings.HasPrefix(line, "Блокировка по Han ") {
			line = savedDetailReason(line, c)
		}
		if !slices.Contains(lines, line) {
			lines = append(lines, line)
		}
	}
	if c.Reason == "" {
		lines = append(lines, "Подробный разбор проверок не сохранён")
	}
	return strings.Join(lines, "\n\n")
}

func detailActions(c moderation.Card, now time.Time) string {
	lines, cleanup := []string{}, map[string]int{}
	hasBan, hasDelete := false, false
	for _, a := range currentCardActions(c) {
		if a.Kind == "notice" || a.Kind == "card" {
			continue
		}
		hasBan = hasBan || slices.Contains([]string{"ban", "restrict", "unban", "restore"}, a.Kind)
		hasDelete = hasDelete || a.Kind == "original_delete"
		if a.Kind == "cleanup_delete" {
			cleanup[detailStatus(a, now)]++
			continue
		}
		line := detailActionName(a) + " — " + detailCardStatus(c, a, now)
		if slices.Contains(c.Resolved, a.ID) {
			line += ". Оператор закрыл ручную работу; это не подтверждает успех Telegram API"
		}
		if a.State == moderation.Running && c.Held || a.Uncertain && a.State != moderation.Confirmed {
			line += ". Уже отправленный запрос невозможно отозвать"
		}
		if c.Held && a.State == moderation.Confirmed && (a.Kind == "ban" || a.Kind == "restrict") {
			line += ". Снятие требования само по себе не отменяет подтверждённый результат"
		}
		lines = append(lines, line)
	}
	if !hasBan {
		lines = append(lines, "Блокировка не запрашивалась")
	}
	if !hasDelete {
		lines = append(lines, "Удаление оригинала не запрашивалось")
	}
	statuses := make([]string, 0, len(cleanup))
	for state := range cleanup {
		statuses = append(statuses, state)
	}
	sort.Strings(statuses)
	for _, state := range statuses {
		lines = append(lines, fmt.Sprintf("Очистка других сообщений — %s: %d", state, cleanup[state]))
	}
	return strings.Join(lines, "\n")
}

func detailIntervention(c moderation.Card, now time.Time) string {
	lines := []string{}
	if detailRequestOnly(c) {
		lines = append(lines, "Проверьте оригинал и причину. Решение принимается кнопками подтверждения или отказа.")
	}
	for _, a := range currentCardActions(c) {
		if slices.Contains(c.Resolved, a.ID) || a.Kind == "notice" || a.Kind == "card" {
			continue
		}
		if detailRequestPlaceholder(a) && c.RequestState != "" && c.RequestState != "pending" {
			continue
		}
		text := ""
		switch {
		case a.State == moderation.Running:
			if a.Uncertain {
				text = "Дождитесь результата текущей проверки. Не дублируйте уже отправленный запрос."
			}
		case detailRetry(a, now):
			text = "Автоматический повтор назначен: " + cardDate(a.NextAt/1000) + ". До него не дублируйте действие вручную."
		case a.State == moderation.Expired ||
			a.State == moderation.Pending && a.Deadline > 0 && now.Add(15*time.Second).UnixMilli() >= a.Deadline:
			text = "Срок выполнения истёк. Автоматический повтор не обещается; проверьте результат и доступность ручного действия."
		case a.Uncertain || a.State == moderation.Failed || a.State == moderation.Blocked || a.State == moderation.Absent ||
			a.State == moderation.Manual && !detailRequestOnly(c):
			text = "Проверьте фактический результат в Telegram. Автоматического повтора этого результата нет; " +
				"при необходимости примите отдельное решение."
		}
		if text != "" && !slices.Contains(lines, text) {
			lines = append(lines, text)
		}
	}
	if c.IncompleteDecisions > 0 {
		lines = append(lines, "Локальное решение оператора не завершено. Проверьте обучение и доверие отдельно; успех не установлен.")
	}
	return strings.Join(lines, "\n")
}

// splitDetail never slices HTML: raw Unicode is split first, each fragment is escaped independently.
func splitDetail(s string, limit int) []string {
	chunks, block, size := []string{}, strings.Builder{}, 0
	for _, r := range s {
		width := len(utf16.Encode([]rune(safeDetail(string(r)))))
		if size+width > limit && block.Len() > 0 {
			chunks = append(chunks, block.String())
			block.Reset()
			size = 0
		}
		block.WriteRune(r)
		size += width
	}
	chunks = append(chunks, block.String())
	return chunks
}

func detailPages(header string, sections []detailSection, technical bool) []string {
	pages, current := []string{}, header
	for _, section := range sections {
		// Redact before splitting so a credential cannot straddle pagination boundaries.
		body := credentialPattern.ReplaceAllString(section.body, "[секрет скрыт]")
		for _, chunk := range splitDetail(body, 1000) {
			bodyText := safeDetail(chunk)
			if technical {
				bodyText = "<pre>" + bodyText + "</pre>"
			} else if section.title == "Сохранённый исходный текст" {
				bodyText = "<blockquote>" + bodyText + "</blockquote>"
			}
			block := "\n\n<b>" + safeDetail(section.title) + "</b>\n" + bodyText
			if len(utf16.Encode([]rune(current+block))) > 3600 && current != header {
				pages = append(pages, current)
				current = header
			}
			current += block
		}
	}
	pages = append(pages, current)
	for i := range pages {
		pages[i] += fmt.Sprintf("\n\nСтраница %d из %d", i+1, len(pages))
	}
	return pages
}

func detailHistory(c moderation.Card, now time.Time) []detailSection {
	sections := []detailSection{}
	if c.RecordedAt > 0 {
		sections = append(sections, detailSection{"История",
			cardDate(c.RecordedAt/1000) + " — событие сохранено в журнале (не время отправки оригинала)."})
	}
	audit := slices.Clone(c.Audit)
	slices.SortStableFunc(audit, func(a, b moderation.CardAudit) int {
		if a.Time == b.Time {
			return 0
		}
		if a.Time == 0 {
			return 1
		}
		if b.Time == 0 || a.Time < b.Time {
			return -1
		}
		return 1
	})
	for _, event := range audit {
		label := "Решение оператора: " + auditLabel(event.Kind) + "; " + auditState(event.State)
		switch event.Type {
		case "origin":
			label = "Принято основание: " + originLabel(event.Kind)
		case "closure":
			label = "Оператор закрыл ручную работу. Это не подтверждение API."
		case "request", "past_request":
			label = "Запрос создан. Сохранённое состояние: " + auditState(event.State) + ". Время решения отдельно не сохранено."
		}
		if event.Time > 0 {
			label = cardDate(event.Time/1000) + " — " + label
		} else {
			label += ". Время не сохранено."
		}
		sections = append(sections, detailSection{"История решений", label})
		var snapshot []moderation.Action
		if event.Snapshot != "" && json.Unmarshal([]byte(event.Snapshot), &snapshot) == nil {
			for _, a := range snapshot {
				sections = append(sections, detailSection{"Архив прежнего запроса",
					fmt.Sprintf("%s — %s; попыток: %d. Время результата не сохранено.",
						detailActionName(a), detailStatus(a, now), a.Attempts)})
			}
		}
	}
	actions := slices.Clone(c.Actions)
	slices.SortStableFunc(actions, func(a, b moderation.Action) int {
		if a.NextAt < b.NextAt {
			return -1
		}
		if a.NextAt > b.NextAt {
			return 1
		}
		return 0
	})
	var history strings.Builder
	for _, a := range actions {
		if a.Kind == "notice" || a.Kind == "card" {
			continue
		}
		label := detailActionName(a)
		if a.Kind == "cleanup_delete" {
			label = fmt.Sprintf("Очистка сообщения №%d", a.MessageID)
		}
		fmt.Fprintf(&history, "%s — %s. Попыток обработки: %d.\n", label, detailCardStatus(c, a, now), a.Attempts)
	}
	if history.Len() > 0 {
		sections = append(sections, detailSection{"История действий",
			"Точное время результатов и отдельных попыток не сохранено. Порядок — по записи планирования.\n\n" + history.String()})
	}
	if len(sections) == 0 {
		sections = append(sections, detailSection{"История", "Подробная история событий не сохранена"})
	}
	return sections
}

func auditLabel(kind string) string {
	return map[string]string{
		"approve":        "подтвердить модерацию",
		"approve_report": "подтвердить жалобу",
		"reject_report":  "отклонить запрос",
		"cancel":         "отменить повторы",
		"unban":          "снять блокировку",
		"restore":        "снять ограничения",
		"close":          "закрыть ручную работу",
	}[kind]
}

func auditState(state string) string {
	switch state {
	case "pending":
		return "ожидает подтверждения"
	case "expired":
		return "срок истёк"
	case "reject_report", "cancel", "dismissed":
		return "запрос отклонён"
	case "started":
		return "локальное решение начато; завершение не установлено"
	case "done", "approve_report", "delete":
		return "локальное решение записано; результаты API указаны отдельно"
	case "closed_by_operator":
		return "закрыто оператором; успех API этим не подтверждён"
	default:
		return "результат решения не установлен"
	}
}

func originLabel(origin string) string {
	switch {
	case origin == moderation.HanOrigin:
		return "правило Han"
	case origin == "detector":
		return "автоматическая проверка"
	case strings.HasPrefix(origin, "report"):
		return "жалоба или запрос оператора"
	case strings.HasPrefix(origin, "operator"), strings.HasPrefix(origin, "forward"), origin == "delete_request":
		return "явное решение оператора"
	default:
		return "ранее сохранённое основание"
	}
}

func operatorDetailPages(c moderation.Card, now time.Time) []string {
	reasons := detailReasons(c)
	shortReason := reasons
	sections := []detailSection{}
	if len(utf16.Encode([]rune(safeDetail(reasons)))) > 500 {
		shortReason = truncateRunes(reasons, 80) + "\nПолные причины — на страницах ниже."
		sections = append(sections, detailSection{"Причины полностью", reasons})
	}
	actions := detailActions(c, now)
	if len(utf16.Encode([]rune(safeDetail(actions)))) > 1100 {
		sections = append(sections, detailSection{"Все действия", actions})
		brief := []string{}
		for _, op := range currentCardActions(c) {
			if op.Kind == "original_delete" && op.MessageID == c.MessageID ||
				slices.Contains([]string{"ban", "restrict", "unban", "restore"}, op.Kind) {
				brief = append(brief, detailActionName(op)+" — "+detailCardStatus(c, op, now))
			}
		}
		brief = append(brief, "Остальные действия перечислены на страницах ниже; их исходы не объединены с успехом оригинала.")
		actions = strings.Join(brief, "\n")
	}
	header := "<b>" + safeDetail(detailResult(c, now)) + "</b>\n" + safeDetail(detailActor(c)) +
		"\n\n<b>Причина</b>\n" + safeDetail(shortReason) +
		"\n\n<b>Действия</b>\n" + safeDetail(actions)
	if advice := detailIntervention(c, now); advice != "" {
		sections = append(sections, detailSection{"Нужно ли действие оператора", advice})
	}
	name := c.DisplayName
	if name == "" {
		name = c.UserName
	}
	if detailActor(c) == "Автор не установлен" {
		name = ""
	}
	date := c.OriginalDate
	if date == 0 {
		for _, a := range c.Actions {
			if a.ChatID == c.ChatID && a.MessageID == c.MessageID && a.Kind == "original_delete" && a.MessageDate > 0 {
				date = a.MessageDate
				break
			}
		}
	}
	dateText := "Настоящая дата сообщения неизвестна"
	if date > 0 {
		dateText = "Дата оригинала: " + cardDate(date)
	}
	message := "Номер оригинала не установлен"
	if c.MessageID > 0 {
		message = fmt.Sprintf("Номер оригинала: %d", c.MessageID)
	}
	sections = append(sections, detailSection{"Исходное сообщение",
		fmt.Sprintf("Исходный чат: название не сохранено\n%s\n%s\n%s",
			strings.TrimSpace(detailActor(c)+" "+name), message, dateText)},
		detailSection{"Сохранённый исходный текст", c.Text})
	sections = append(sections, detailHistory(c, now)...)
	return detailPages(header, sections, false)
}

func technicalDetailPages(c moderation.Card) []string {
	sections := []detailSection{
		{"Идентификаторы", fmt.Sprintf("chat ID: %d\nmessage ID: %d\n%s ID: %d\nreceipt ID: %s\ncard reference: %s",
			c.ChatID, c.MessageID, c.IdentityKind, c.IdentityID, c.Receipt, c.Ref)},
		{"Сохранённая диагностика события", c.Reason},
	}
	for _, a := range c.Actions {
		sections = append(sections, detailSection{"Действие", detailActionDiagnostics(a)})
	}
	for _, d := range c.Origins {
		sections = append(sections, detailSection{"Основание / demand",
			fmt.Sprintf("action ID: %s\norigin: %s\nactive: %t", d.ActionID, d.Origin, d.Active)})
	}
	for _, event := range c.Audit {
		sections = append(sections, detailSection{"Историческая запись",
			fmt.Sprintf("type: %s\nID: %s\nkind: %s\nstate: %s\noperator ID: %d\nsaved timestamp: %d",
				event.Type, event.ID, event.Kind, event.State, event.OperatorID, event.Time)})
	}
	for _, event := range c.Audit {
		if event.Snapshot == "" {
			continue
		}
		var snapshot []moderation.Action
		if json.Unmarshal([]byte(event.Snapshot), &snapshot) != nil {
			sections = append(sections, detailSection{"Архив запроса",
				"Формат снимка не распознан; небезопасный сырой фрагмент не отображается."})
			continue
		}
		for _, op := range snapshot {
			sections = append(sections, detailSection{"Архив действия", detailActionDiagnostics(op)})
		}
	}
	return detailPages("<b>Технические данные</b>", sections, true)
}

func detailActionDiagnostics(a moderation.Action) string {
	return fmt.Sprintf("action ID: %s\nchat ID: %d\n%s ID: %d\nkind: %s\nstate: %s\ncode: %s\nattempts: %d\nuncertain: %t\n"+
		"message ID: %d\nMessage.Date: %d\ndeadline (Unix ms): %d\n"+
		"next_at (планирование, не подтверждение): %d\nlease_until (Unix ms): %d\nuntil_date (Unix s): %d",
		a.ID, a.ChatID, a.IdentityKind, a.IdentityID, a.Kind, a.State, a.Code, a.Attempts, a.Uncertain, a.MessageID, a.MessageDate,
		a.Deadline, a.NextAt, a.LeaseUntil, a.UntilDate)
}
