package events

import (
	"context"
	"fmt"
	"html"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage"
)

type authorEvidenceLocator interface {
	MessageAuthor(context.Context, int64, int) (storage.MessageAuthor, error)
}

// MsgHandler treats only authenticated personal forwards as manual decisions.
// Matching content locates originals; it never establishes the author's identity.
func (a *admin) MsgHandler(u tbapi.Update) error {
	m := u.Message
	if m == nil || m.Chat.ID != a.adminChatID || m.From == nil || m.From.IsBot || m.SenderChat != nil {
		return nil
	}
	if m.ForwardOrigin != nil && m.ForwardOrigin.SenderUser != nil && m.ForwardOrigin.SenderUser.IsBot {
		return nil
	}
	diagnostic := false
	command := "/forward_status@" + a.botUsername
	if a.botUsername != "" && strings.EqualFold(strings.TrimSpace(m.Text), command) && m.ReplyToMessage != nil {
		diagnostic = true
	}
	if m.ForwardOrigin == nil && !diagnostic {
		log.Printf("[DEBUG] admin report ignored: outcome=missing_origin admin_msg_id=%d", m.MessageID)
		return nil
	}
	reply := func(s string) error {
		out := tbapi.NewMessage(a.adminChatID, s)
		out.ReplyParameters = tbapi.ReplyParameters{MessageID: m.MessageID}
		if _, err := a.tbAPI.Send(out); err != nil {
			log.Printf("[WARN] admin forward feedback unavailable")
		}
		return nil
	}
	if m.From.ID <= 0 || sharedTelegramUser(m.From.ID) || !a.superUsers.IsSuper("", m.From.ID) {
		return reply("Отказ: пересылка требует уполномоченного личного пользователя с проверенным числовым ID.")
	}

	delivery := moderation.ForwardDelivery{UpdateID: u.UpdateID, ChatID: m.Chat.ID, MessageID: m.MessageID}
	if !diagnostic && a.recovery != nil {
		_, seen, e := a.recovery.journal.LookupForwardDelivery(context.Background(), delivery, m.From.ID)
		if e != nil {
			return reply(
				"Пересылка не принята: отсутствует идентификатор Telegram update или он противоречит сохранённой доставке.",
			)
		}
		if seen {
			return nil
		} // the existing persistent card/decision is unchanged, even after locator retention
	}

	source := m
	if diagnostic {
		source = m.ReplyToMessage
		if source.ForwardOrigin == nil {
			return reply("Это не пересылка. Обычный текст не является решением модерации.")
		}
	}
	locator, ok := a.locator.(deleteCandidateLocator)
	if !ok {
		return reply("Отказ: недоступен безопасный поиск всех оригиналов. Действия не приняты.")
	}
	text := transform(source).Text
	var media *storage.MediaLocatorKey
	if strings.TrimSpace(text) == "" {
		key, status := extractMediaLocatorKey(source)
		if status != mediaLocatorExtracted {
			return reply(
				"Поиск недоступен для этого типа пересылки. Используйте оригинал в защищаемом чате; действия не приняты.",
			)
		}
		media = &key
	}
	rows, err := locator.DeleteCandidates(context.Background(), text, media, a.primChatID, 21)
	if err != nil {
		return reply("Отказ: locator недоступен или содержит конфликтующие оригиналы. Попробуйте позже; действий нет.")
	}
	if len(rows) == 0 {
		log.Printf(
			"[INFO] admin forward search: outcome=locator_miss primary_chat=%d delivery=%d diagnostic=%t",
			a.primChatID,
			source.MessageID,
			diagnostic,
		)
		if diagnostic {
			return reply(
				"Сохранённый оригинал в моём защищаемом чате не найден. Нет полномочий на точное удаление; " +
					"скрытый автор и совпадение текста не разрешают бан. " +
					"Допустим существующий /spam ответом на настоящий оригинал уполномоченным пользователем. " +
					"Поиск ничего не переигрывает.",
			)
		}
		// Other bots in a shared admin chat must not emit an identical refusal stream.
		return nil
	}
	if len(rows) > 20 {
		return reply("Найдено более 20 оригиналов. Уточните сообщение в защищаемом чате; массовые действия не приняты.")
	}
	targets := make([]moderation.ForwardTarget, 0, len(rows))
	for _, row := range rows {
		if row.ChatID != a.primChatID || row.MsgID <= 0 {
			return reply("Отказ: оригинал не принадлежит защищаемому чату.")
		}
		target := moderation.ForwardTarget{
			MessageID:    row.MsgID,
			MessageDate:  locatorMessageDate(a.locator, row.ChatID, row.MsgID),
			IdentityKind: "message",
			Text:         text,
		}
		if loc, ok := a.locator.(authorEvidenceLocator); ok {
			author, e := loc.MessageAuthor(context.Background(), row.ChatID, row.MsgID)
			if e != nil {
				return reply("Отказ: не удалось проверить происхождение ID автора. Действия не приняты.")
			}
			if author.Source == "conflict" {
				target.Blocked = "source_author_conflict"
			}
			valid := author.Source == "telegram_update" && author.UpdateID > 0 && author.ID == row.UserID &&
				(author.Kind == "user" && author.ID > 0 && !sharedTelegramUser(author.ID) || author.Kind == "sender_chat" && author.ID < 0)
			if valid {
				target.IdentityKind = author.Kind
				target.IdentityID = author.ID
				target.Evidence = author.Source
				target.EvidenceUpdateID = author.UpdateID
				target.EvidenceMessageID = row.MsgID
			}
		}
		origin := adminForwardOriginIdentity(source.ForwardOrigin)
		// An open Telegram forward can independently supply the numeric author.
		// The legacy locator ID is only a consistency check, never positive evidence.
		if target.IdentityID == 0 && u.UpdateID > 0 && origin.Reliable && origin.ID == row.UserID &&
			(origin.Kind == adminReportIdentityUser && origin.ID > 0 && !sharedTelegramUser(origin.ID) ||
				origin.Kind == adminReportIdentitySenderChat && origin.ID < 0) {
			target.IdentityID = origin.ID
			target.IdentityKind = string(origin.Kind)
			target.Evidence = "telegram_forward_origin"
			target.EvidenceUpdateID = u.UpdateID
			target.EvidenceMessageID = source.MessageID
		}

		if target.IdentityID != 0 {
			target.BanKind = "ban"
			if a.softBan && target.IdentityKind == "user" {
				target.BanKind = "restrict"
			}
			switch {
			case target.IdentityID == a.primChatID:
				target.Blocked = "anonymous_admin_gate"
			case a.superUsers.IsSuper(row.UserName, target.IdentityID):
				target.Blocked = "current_superuser_gate"
			case a.targetExplicitlyApproved(target.IdentityID):
				target.Blocked = "explicit_trust_gate"
			}
		}
		// Legacy IDs are not positive authority, but can still conservatively veto a protected target.
		if a.superUsers.IsSuper(row.UserName, row.UserID) {
			target.Blocked = "current_superuser_gate"
		}
		if a.targetExplicitlyApproved(row.UserID) {
			target.Blocked = "explicit_trust_gate"
		}
		if row.UserID == a.primChatID {
			target.Blocked = "anonymous_admin_gate"
		}
		if len(rows) == 1 && origin.Reliable && target.IdentityID != 0 &&
			(origin.ID != target.IdentityID || string(origin.Kind) != target.IdentityKind) {
			target.Blocked = "forward_identity_mismatch"
		}
		targets = append(targets, target)
	}
	if diagnostic {
		return reply(
			fmt.Sprintf(
				"В моём защищаемом чате найдено оригиналов: %d. "+
					"Данные автора проверяются отдельно по сохранённому исходному update; "+
					"диагностика не создаёт действий.",
				len(targets),
			),
		)
	}
	if a.recovery == nil {
		return reply("Отказ: recovery journal отключён. Действия не приняты.")
	}
	blockReason := ""
	switch {
	case a.forwardingDisabled:
		blockReason = "forwarding_disabled"
	case a.dry:
		blockReason = "dry_mode"
	case a.trainingMode:
		blockReason = "training_mode"
	}
	if blockReason != "" {
		for i := range targets {
			targets[i].Blocked = blockReason
		}
	}

	_, err = a.recovery.journal.AcceptForward(
		context.Background(),
		a.primChatID,
		a.adminChatID,
		m.From.ID,
		delivery,
		targets,
		time.Now(),
	)
	if err != nil {
		log.Printf("[WARN] admin forward admission failed: %T", err)
		return reply("Не принято: лимит журнала или ошибка сохранения. Никаких новых действий не разрешено.")
	}
	return nil // persistent card is sent/edited by the journal worker, never a second receipt message
}

func forwardKeyboard(c moderation.Card) tbapi.InlineKeyboardMarkup { return forwardPageKeyboard(c, 0) }

func forwardPageKeyboard(c moderation.Card, page int) tbapi.InlineKeyboardMarkup {
	rows := [][]tbapi.InlineKeyboardButton{}
	b := c.Forward
	if b.State == "pending" && b.Expires > time.Now().UnixMilli() {
		for i, t := range b.Targets {
			rows = append(
				rows,
				tbapi.NewInlineKeyboardRow(
					tbapi.NewInlineKeyboardButtonData(
						forwardChoiceLabel(t),
						fmt.Sprintf("MF%d:%s", i, c.Ref),
					),
				),
			)
		}
		rows = append(
			rows,
			tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Обработать все найденные", "MFA:"+c.Ref)),
		)
	}
	if b.State == "accepted" && len(b.Targets) > 5 {
		navigation := []tbapi.InlineKeyboardButton{}
		if page > 0 {
			navigation = append(
				navigation,
				tbapi.NewInlineKeyboardButtonData("← Предыдущие", fmt.Sprintf("MFV%d:%s", page-1, c.Ref)),
			)
		}
		if (page+1)*5 < len(b.Targets) {
			navigation = append(
				navigation,
				tbapi.NewInlineKeyboardButtonData("Следующие →", fmt.Sprintf("MFV%d:%s", page+1, c.Ref)),
			)
		}
		rows = append(rows, navigation)
	}
	return tbapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func forwardActionLabel(a moderation.Action) string {
	switch a.State {
	case moderation.Confirmed:
		return "подтверждено"
	case moderation.Absent:
		return "сообщение уже отсутствует (не подтверждение удаления этим ботом)"
	case moderation.Pending, moderation.Running:
		if a.Uncertain {
			return "результат API неизвестен; проверка/повтор ожидается"
		}
		return "ожидается"
	default:
		if a.Uncertain {
			return "результат API неизвестен: " + a.Code
		}
		return forwardReason(a.Code)
	}
}

func forwardCardText(c moderation.Card, chatName string) string {
	return forwardCardPage(c, chatName, 0)
}

func forwardCardPage(c moderation.Card, chatName string, page int) string {
	b := c.Forward
	var out strings.Builder
	fmt.Fprintf(&out, "Ручная модерация — %s\n", html.EscapeString(chatName))
	if b.Delivery.MessageID > 0 {
		fmt.Fprintf(&out, "Пересылка №%d.\n", b.Delivery.MessageID)
	}
	out.WriteString(forwardDecisionReason(b.Reason))
	if b.State == "pending" && b.Expires > time.Now().UnixMilli() {
		if len(b.Targets) == 1 {
			out.WriteString("Подтвердите новое явное решение для указанного оригинала.\n")
		} else if len(b.Targets) == 2 {
			out.WriteString("Найдены 2 разных сообщения. Выберите одно или обработайте оба.\n")
		} else {
			fmt.Fprintf(&out, "Найдено разных сообщений: %d. Выберите одно или обработайте все перечисленные.\n", len(b.Targets))
		}
		fmt.Fprintf(&out, "Выбор действует до %s.\n", cardDate(b.Expires/1000))
	} else if b.State != "accepted" {
		out.WriteString("Срок выбора истёк. Никаких действий не принято. Просмотр не возобновляет полномочия.\n")
	}
	for i, t := range b.Targets {
		if b.State == "accepted" && (i < page*5 || i >= (page+1)*5) {
			continue
		}
		date := "дата оригинала неизвестна"
		if t.MessageDate > 0 {
			date = cardDate(t.MessageDate)
		}
		fmt.Fprintf(&out, "\n№%d · %s\n«%s»\n", t.MessageID, date, html.EscapeString(truncateRunes(inertSpamText(t.Text), 40)))
		if !containsForwardIndex(b.Selected, i) {
			if t.DeleteConfirmed {
				out.WriteString("Удаление уже подтверждено; повторного удаления не будет.\n")
			}
			if t.BanConfirmed {
				out.WriteString("Блокировка уже подтверждена; повторного бана не будет.\n")
			}
			if t.PreviousRefusal != "" {
				out.WriteString("Предыдущий отказ: " + forwardReason(t.PreviousRefusal) + ".\n")
			}
			if t.IdentityID == 0 {
				out.WriteString("Автор не подтверждён: доступно только точное удаление.\n")
			}
			if t.Blocked != "" {
				out.WriteString("Отказ: " + forwardReason(t.Blocked) + ".\n")
			}
			continue
		}
		if b.Reason == "gate_refused" {
			fmt.Fprintf(&out, "Сообщение №%d: новые действия запрещены правилами безопасности.\n", t.MessageID)
		} else if b.Reason == "already_confirmed" || b.Reason == "ongoing" {
			fmt.Fprintf(&out, "Сохранённый результат для сообщения №%d.\n", t.MessageID)
		} else if t.IdentityID != 0 {
			fmt.Fprintf(&out, "Принято: удалить сообщение №%d и заблокировать автора.\n", t.MessageID)
		} else {
			fmt.Fprintf(&out, "Принято: удалить сообщение №%d. Блокировка: отказ — "+
				"нет надёжной числовой цели из исходного update.\n", t.MessageID)
		}
		for _, a := range c.Actions {
			if a.Kind == "original_delete" && a.MessageID == t.MessageID {
				out.WriteString("Удаление: " + forwardActionLabel(a) + ".\n")
			}
			if (a.Kind == "ban" || a.Kind == "restrict") && a.IdentityKind == t.IdentityKind && a.IdentityID == t.IdentityID {
				label := forwardActionLabel(a)
				if a.State == moderation.Confirmed {
					label = "подтверждена"
				}
				out.WriteString("Блокировка: " + label + ".\n")
			}
		}
	}
	return out.String()
}

func containsForwardIndex(indices []int, n int) bool {
	return slices.Contains(indices, n)
}

func (a *admin) forwardCallback(ctx context.Context, q *tbapi.CallbackQuery, c moderation.Card, verb string) error {
	if q.From.IsBot || q.From.ID <= 0 || sharedTelegramUser(q.From.ID) || !a.superUsers.IsSuper("", q.From.ID) {
		return a.answerRecoveryCallback(ctx, q, "Отказ: требуется уполномоченный личный пользователь с числовым ID")
	}
	if a.dry || a.trainingMode {
		return a.answerRecoveryCallback(ctx, q, "Отказ: dry/training запрещает модерацию")
	}
	if c.ChatID != a.primChatID {
		return a.answerRecoveryCallback(ctx, q, "Отказ: другой защищаемый чат")
	}
	if after, ok := strings.CutPrefix(verb, "MFV"); ok {
		page, e := strconv.Atoi(after)
		if e != nil || page < 0 || page*5 >= len(c.Forward.Targets) {
			return a.answerRecoveryCallback(ctx, q, "Некорректная страница")
		}
		buttons := forwardPageKeyboard(c, page)
		edit := tbapi.NewEditMessageText(q.Message.Chat.ID, q.Message.MessageID, forwardCardPage(c, a.recovery.chatName, page))
		edit.ParseMode = tbapi.ModeHTML
		edit.ReplyMarkup = &buttons
		_, e = a.recovery.api.RequestWithContext(ctx, edit)
		if e != nil {
			return fmt.Errorf("render forward page: %w", e)
		}
		return nil
	}
	var selected []int
	if verb == "MFA" {
		for i := range c.Forward.Targets {
			selected = append(selected, i)
		}
	} else if after, ok := strings.CutPrefix(verb, "MF"); ok {
		i, e := strconv.Atoi(after)
		if e != nil {
			return a.answerRecoveryCallback(ctx, q, "Некорректный выбор оригинала")
		}
		selected = []int{i}
	} else {
		return a.answerRecoveryCallback(ctx, q, "Для этой карточки доступны только сохранённые варианты выбора; "+
			"бан, обучение и cleanup через другие кнопки запрещены")
	}
	for _, i := range selected {
		if i < 0 || i >= len(c.Forward.Targets) {
			return a.answerRecoveryCallback(ctx, q, "Выбор вне сохранённого набора")
		}
		t := c.Forward.Targets[i]
		if t.IdentityID != 0 && (a.superUsers.IsSuper("", t.IdentityID) || a.targetExplicitlyApproved(t.IdentityID)) {
			return a.answerRecoveryCallback(ctx, q, "Отказ: цель защищена текущими полномочиями или явным доверием")
		}
	}
	fresh, err := a.recovery.journal.DecideForward(
		ctx,
		c.Ref,
		q.Message.Chat.ID,
		q.Message.MessageID,
		q.From.ID,
		selected,
		time.Now(),
	)
	if err != nil {
		return a.answerRecoveryCallback(ctx, q, "Не принято: проверьте срок выбора, исходную карточку, оператора и лимит журнала")
	}
	if !fresh {
		return a.answerRecoveryCallback(ctx, q, "Решение уже принято или истекло; повторных действий нет")
	}
	return a.answerRecoveryCallback(ctx, q, "Принято. Независимые результаты удаления и блокировки появятся в этой карточке")
}

func forwardChoiceLabel(t moderation.ForwardTarget) string {
	if t.PreviousRefusal != "" {
		return fmt.Sprintf("Подтвердить новую попытку №%d", t.MessageID)
	}
	if t.DeleteConfirmed && t.IdentityID != 0 && !t.BanConfirmed {
		return fmt.Sprintf("Подтвердить блокировку автора №%d", t.MessageID)
	}
	if t.IdentityID == 0 {
		return fmt.Sprintf("Удалить №%d (автор неизвестен)", t.MessageID)
	}
	return fmt.Sprintf("Удалить и заблокировать №%d", t.MessageID)
}

func forwardReason(code string) string {
	reasons := map[string]string{
		"source_author_conflict":                        "сохранённые исходные update противоречат друг другу; цель не подтверждена",
		"telegram_403":                                  "Telegram отказал в операции (telegram_403)",
		"telegram_400":                                  "Telegram отклонил операцию (telegram_400)",
		"current_superuser_gate":                        "цель защищена полномочиями superuser/администратора",
		"explicit_trust_gate":                           "цель имеет явное ручное доверие",
		"anonymous_admin_gate":                          "публикация от имени самого защищаемого чата",
		"forward_identity_mismatch":                     "числовой автор пересылки не совпадает с исходным update",
		"forward_operator_revoked":                      "числовой оператор больше не уполномочен",
		"current_safety_gate":                           "текущие правила безопасности запрещают действие",
		"forwarding_disabled":                           "модерация пересылкой отключена настройкой бота",
		"dry_mode":                                      "включён dry-режим",
		"training_mode":                                 "включён training-режим",
		"current_chat_gate":                             "исходный чат больше не разрешён",
		"deadline_manual_action_required":               "срок выполнения истёк; автоматических действий больше не будет",
		"delete_refused_age_not_proven_manual_required": "Telegram отказал в удалении; возраст сообщения не доказан",
	}
	if reason, ok := reasons[code]; ok {
		return reason
	}
	return code
}

func forwardDecisionReason(reason string) string {
	switch reason {
	case "gate_refused":
		return "Отказ сохранён для этой пересылки. Повтор update не возобновит действия; новое решение требует новой пересылки.\n"
	case "followup":
		return "Новая пересылка — отдельный запрос. Предыдущий выбор сохранён.\n"
	case "expired_choice":
		return "Предыдущий выбор истёк. Это новый запрос; срок удаления оригиналов не продлён.\n"
	case "identity_review":
		return "Новое явное решение: ранее было разрешено только удаление. Подтвердите блокировку автора.\n"
	case "manual_review":
		return "Предыдущий запрос требует ручного решения. Новая пересылка создала отдельный запрос; подтвердите действия.\n"
	case "deadline_review":
		return "Срок предыдущего действия истёк. Новый запрос не продлевает срок удаления оригинала.\n"
	case "retry_review":
		return "Ранее получен постоянный отказ. Новая попытка требует вашего явного подтверждения.\n"
	case "already_confirmed":
		return "Всё уже подтверждено. Показан существующий результат; повторных API-действий нет.\n"
	case "ongoing":
		return "Действия уже выполняются или требуют проверки неизвестного результата. Новых повторов не добавлено.\n"
	default:
		return ""
	}
}
