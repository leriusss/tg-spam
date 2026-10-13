package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage"
)

func cardDetails(c moderation.Card) string {
	var text strings.Builder
	text.WriteString("Подробности модерации\n")
	if c.ChatID != 0 {
		fmt.Fprintf(&text, "Чат: %d\n", c.ChatID)
	}
	if c.MessageID > 0 {
		fmt.Fprintf(&text, "Оригинал: %d\n", c.MessageID)
	}
	if c.IdentityID != 0 {
		fmt.Fprintf(&text, "Объект: %s ID=%d\n", c.IdentityKind, c.IdentityID)
	}
	if c.Ref != "" {
		fmt.Fprintf(&text, "Карточка: %s\n", c.Ref)
	}
	if strings.HasPrefix(c.Receipt, "delivery:") || strings.HasPrefix(c.Receipt, "command:") {
		text.WriteString("Точный оригинал не установлен\n")
	}
	for i, op := range c.Actions {
		if i >= 12 {
			fmt.Fprintf(&text, "Ещё операций в журнале: %d\n", len(c.Actions)-i)
			break
		}
		fmt.Fprintf(&text, "\n%s: %s\n", op.Kind, op.State)
		if op.MessageID > 0 {
			fmt.Fprintf(&text, "Сообщение: %d\n", op.MessageID)
		}
		if op.Code != "" {
			fmt.Fprintf(&text, "Причина: %s\n", op.Code)
		}
		if op.Uncertain {
			text.WriteString("Результат не подтверждён\n")
		}
		if op.ID != "" {
			fmt.Fprintf(&text, "ID: %s\n", op.ID)
		}
		fmt.Fprintf(&text, "Попыток: %d\n", op.Attempts)
		if op.MessageDate > 0 {
			fmt.Fprintf(&text, "Дата сообщения: %s\n", cardDate(op.MessageDate))
		}
		if op.Deadline > 0 {
			fmt.Fprintf(&text, "Срок выполнения: %s\n", cardDate(op.Deadline/1000))
		}
		if (op.State == moderation.Pending || op.State == moderation.Running) && op.NextAt > 0 {
			fmt.Fprintf(&text, "Следующее выполнение: %s\n", cardDate(op.NextAt/1000))
		}
		if (op.Kind == "ban" || op.Kind == "restrict") && op.UntilDate > 0 {
			fmt.Fprintf(&text, "Запрошено до: %s\n", cardDate(op.UntilDate))
		}

	}
	origins := map[string]bool{}
	for _, d := range c.Origins {
		origins[d.Origin] = origins[d.Origin] || d.Active
	}
	if len(origins) > 0 {
		fmt.Fprintf(&text, "Основания: %v\n", origins)
	}
	if len(c.Resolved) > 0 {
		fmt.Fprintf(&text, "Ручных решений: %d\n", len(c.Resolved))
	}
	if c.IncompleteDecisions > 0 {
		fmt.Fprintf(&text, "Незавершённых решений: %d\n", c.IncompleteDecisions)
	}
	text.WriteString("Отмена повторов не отменяет уже отправленный запрос. Ручное закрытие не подтверждает успех API.")
	return truncateTelegramText(text.String(), 2800)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

func cardKeyboard(c moderation.Card, allowed bool) tbapi.InlineKeyboardMarkup {
	live, manual, decision, _ := cardWork(c)
	rows := [][]tbapi.InlineKeyboardButton{}
	main := []tbapi.InlineKeyboardButton{}
	if allowed {
		main = append(main, tbapi.NewInlineKeyboardButtonData("Изменить бан", "MA:"+c.Ref))
	}
	main = append(main, tbapi.NewInlineKeyboardButtonData("Подробнее", "M?:"+c.Ref))
	rows = append(rows, main)
	if allowed && live {
		rows = append(rows, tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Отменить повторы", "M-:"+c.Ref)))
	}
	if manual && !live {
		rows = append(rows, tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Закрыть ручную работу", "MC:"+c.Ref)))
	}
	report := false
	for _, d := range c.Origins {
		report = report || d.Active && (d.Origin == "report" || strings.HasPrefix(d.Origin, "report:"))
	}
	if allowed && report && decision {
		rows = append(rows, tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Отклонить жалобу", "MR:"+c.Ref)))
	}
	if allowed && decision {
		rows = append(rows, tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Подтвердить модерацию", "M+:"+c.Ref)))
	}
	return tbapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (r *recovery) notifyCard(ctx context.Context, a moderation.Action) moderation.Outcome {
	if !r.gate(a) {
		return moderation.Outcome{State: moderation.Undelivered, Code: "notification_destination_changed"}
	}
	c, err := r.journal.CardView(ctx, a.Receipt)
	if err != nil {
		return moderation.Outcome{Retry: true, Code: "card_context_unavailable"}
	}
	authority := moderation.Action{
		Kind:         "ban",
		ChatID:       c.ChatID,
		IdentityID:   c.IdentityID,
		IdentityKind: c.IdentityKind,
		MessageID:    c.MessageID,
	}
	allowed := r.gate(authority)
	if r.superName != nil && r.superName(c.UserName, c.IdentityID) {
		allowed = false
	}
	for _, op := range c.Actions {
		if op.State == moderation.Blocked {
			allowed = false
		}
	}
	buttons := cardKeyboard(c, allowed)
	var req tbapi.Chattable
	if a.NotificationID > 0 {
		m := tbapi.NewEditMessageText(a.ChatID, a.NotificationID, compactCardText(c, r.chatName))
		m.ParseMode = tbapi.ModeHTML
		m.LinkPreviewOptions = tbapi.LinkPreviewOptions{IsDisabled: true}
		m.ReplyMarkup = &buttons
		req = m
	} else {
		m := tbapi.NewMessage(a.ChatID, compactCardText(c, r.chatName))
		m.ParseMode = tbapi.ModeHTML
		m.LinkPreviewOptions = tbapi.LinkPreviewOptions{IsDisabled: true}
		m.ReplyMarkup = buttons
		req = m
	}
	resp, err := r.api.RequestWithContext(ctx, req)
	var apiErr *tbapi.Error
	if errors.As(err, &apiErr) && apiErr.Code == 400 && a.NotificationID > 0 {
		if strings.Contains(strings.ToLower(apiErr.Message), "message is not modified") {
			return moderation.Outcome{State: moderation.Confirmed, NotificationID: a.NotificationID}
		}
		if strings.Contains(strings.ToLower(apiErr.Message), "message to edit not found") {
			return moderation.Outcome{Retry: true, Code: "notification_anchor_lost", NotificationID: -1}
		}
	}
	out := telegramOutcome(resp, err)
	if out.State == moderation.Confirmed {
		var sent tbapi.Message
		if json.Unmarshal(resp.Result, &sent) != nil || sent.MessageID <= 0 {
			return moderation.Outcome{Retry: true, Uncertain: true, Code: "notification_ack_unknown"}
		}
		out.NotificationID = sent.MessageID
	}
	return out
}

func (a *admin) recoveryCardCallback(q *tbapi.CallbackQuery) error {
	if q.Message == nil || q.From == nil || q.Message.Chat.ID != a.adminChatID {
		return fmt.Errorf("invalid card callback destination")
	}
	parts := strings.SplitN(q.Data, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid card callback")
	}
	ctx := context.Background()
	j := a.recovery.journal
	c, err := j.CardView(ctx, parts[1])
	if err != nil {
		return fmt.Errorf("load moderation card: %w", err)
	}
	bound, err := j.CardBound(ctx, c, q.Message.Chat.ID, q.Message.MessageID)
	if err != nil {
		return fmt.Errorf("card authority: %w", err)
	}
	if !bound {
		return fmt.Errorf("callback is not bound to this moderation card")
	}
	if pageText, historyPage := strings.CutPrefix(parts[0], "MP"); historyPage {
		page, parseErr := strconv.Atoi(pageText)
		if parseErr != nil || page < 0 || page >= max(1, (len(c.Actions)+1)/2) {
			return fmt.Errorf("invalid history page")
		}
		return a.recoveryCardInfoPage(ctx, q, c, page)
	}
	verb := ""
	switch parts[0] {
	case "MA":
		if a.dry || a.trainingMode {
			return fmt.Errorf("training/dry blocks durable undo")
		}
		buttons := tbapi.NewInlineKeyboardMarkup(
			tbapi.NewInlineKeyboardRow(
				tbapi.NewInlineKeyboardButtonData("Подтвердить", "MU:"+c.Ref),
				tbapi.NewInlineKeyboardButtonData("Подробнее", "M?:"+c.Ref),
			),
		)
		_, e := a.recovery.api.RequestWithContext(ctx, tbapi.NewEditMessageReplyMarkup(q.Message.Chat.ID, q.Message.MessageID, buttons))
		if e != nil {
			return fmt.Errorf("undo confirmation delivery: %w", e)
		}
		return nil
	case "M?":
		return a.recoveryCardInfo(ctx, q, c)
	case "MB":
		return a.recoveryCardDisplay(ctx, q, c)
	case "M+":
		verb = "approve"
		for _, d := range c.Origins {
			if d.Origin == "report" || strings.HasPrefix(d.Origin, "report:") {
				verb = "approve_report"
			}
		}
	case "M-":
		verb = "cancel"
	case "MR":
		verb = "reject_report"
	case "MU":
		verb = "unban"
		if a.softBan && c.IdentityKind == "user" {
			verb = "restore"
		}
	case "MC":
		verb = "close"
	default:
		return fmt.Errorf("unsupported card command")
	}
	if (a.trainingMode || a.dry) && verb != "close" {
		return fmt.Errorf("training/dry mode blocks durable moderation commands")
	}
	target := moderation.Action{
		Kind:         "ban",
		ChatID:       c.ChatID,
		IdentityID:   c.IdentityID,
		IdentityKind: c.IdentityKind,
		MessageID:    c.MessageID,
	}
	if a.recovery.gate != nil && !a.recovery.gate(target) && (verb == "approve" || verb == "approve_report") {
		return fmt.Errorf("current safety gate blocks approval")
	}
	c.RequestedBanKind = "ban"
	if a.softBan {
		c.RequestedBanKind = "restrict"
	}
	fresh, err := j.Decide(ctx, q.ID, c, verb, q.From.ID, time.Now())
	if err != nil {
		return fmt.Errorf("record moderation decision: %w", err)
	}
	if fresh {
		var learningErr error
		if (verb == "approve" || verb == "approve_report") && c.Text != "" {
			learningErr = a.bot.UpdateSpam(c.Text)
		}
		if (verb == "unban" || verb == "restore") && c.Text != "" {
			learningErr = a.bot.UpdateHam(c.Text)
		}
		if verb == "unban" || verb == "restore" {
			if e := a.bot.AddApprovedUser(c.IdentityID, c.UserName); e != nil {
				learningErr = e
			}
		}
		if learningErr != nil {
			return fmt.Errorf("local learning/approval failed; decision retained for manual review")
		}
		if err = j.EndDecision(ctx, q.ID); err != nil {
			return fmt.Errorf("record callback completion: %w", err)
		}
	}
	return a.answerRecoveryCallback(
		ctx,
		q,
		"Решение записано. Карточка обновится после выполнения",
	)
}

func (a *admin) answerRecoveryCallback(ctx context.Context, q *tbapi.CallbackQuery, text string) error {
	_, err := a.recovery.api.RequestWithContext(ctx, tbapi.NewCallback(q.ID, text))
	if err != nil {
		return fmt.Errorf("callback response delivery: %w", err)
	}
	return nil
}

func (r *userReports) recoveryReportDecision(ctx context.Context, q *tbapi.CallbackQuery, approve bool) error {
	uid, msgID, err := parseCallbackData(q.Data)
	if err != nil {
		return fmt.Errorf("report callback identity: %w", err)
	}
	if q.Message == nil || q.From == nil || q.Message.Chat.ID != r.adminChatID {
		return fmt.Errorf("report callback context mismatch")
	}
	a := r.recoveryAdmin
	j := a.recovery.journal
	receipt := fmt.Sprintf("source:%d:%d:user:%d", r.primChatID, msgID, uid)
	c, contextErr := j.CardView(ctx, receipt)
	var reports []storage.Report
	if r.Storage != nil {
		reports, err = r.Storage.GetByMessage(ctx, msgID, r.primChatID)
		if err != nil {
			return fmt.Errorf("load report context: %w", err)
		}
	}
	if len(reports) > 0 {
		report := reports[0]
		if report.ReportedUserID != uid || report.ChatID != r.primChatID {
			return fmt.Errorf("report callback target mismatch")
		}
		if report.AdminMsgID > 0 && report.AdminMsgID != q.Message.MessageID {
			return fmt.Errorf("report callback card mismatch")
		}
		if contextErr != nil {
			p := moderation.Plan{
				Key:          receipt,
				ChatID:       report.ChatID,
				MessageID:    msgID,
				IdentityID:   uid,
				IdentityKind: "user",
				Destination:  r.adminChatID,
				Origin:       "report",
				Card:         true,
				SourceText:   report.MsgText,
				UserName:     report.ReportedUserName,
				Actions: []moderation.Action{
					{
						Kind:         "original_delete",
						ChatID:       report.ChatID,
						IdentityID:   uid,
						IdentityKind: "user",
						MessageID:    msgID,
						MessageDate:  locatorMessageDate(r.locator, report.ChatID, msgID),
						State:        moderation.Manual,
						Code:         "manual_report_pending_decision",
					},
				},
			}
			if _, _, _, err = j.Accept(ctx, p, time.Now()); err != nil {
				return fmt.Errorf("persist report card: %w", err)
			}
			c, err = j.CardView(ctx, receipt)
			if err != nil {
				return fmt.Errorf("read report card: %w", err)
			}
		}
		if err = j.BindCard(ctx, receipt, q.Message.MessageID); err != nil {
			return fmt.Errorf("bind report card: %w", err)
		}
	} else {
		// report-storage TTL must not invalidate the durable original context.
		if contextErr != nil {
			return fmt.Errorf("no durable report context")
		}
		reportOrigin := false
		for _, d := range c.Origins {
			reportOrigin = reportOrigin || (d.Origin == "report" || strings.HasPrefix(d.Origin, "report:"))
		}
		bound, e := j.CardBound(ctx, c, q.Message.Chat.ID, q.Message.MessageID)
		if e != nil {
			return fmt.Errorf("report card authority: %w", e)
		}
		if !reportOrigin || !bound {
			return fmt.Errorf("missing bound report authority")
		}
	}
	if c.IdentityID != uid || c.ChatID != r.primChatID || r.superUsers.IsSuper(c.UserName, uid) {
		return fmt.Errorf("report target safety gate")
	}
	verb := "reject_report"
	if approve {
		verb = "approve_report"
	}
	c.RequestedBanKind = "ban"
	if r.softBanMode {
		c.RequestedBanKind = "restrict"
	}
	fresh, err := j.Decide(ctx, q.ID, c, verb, q.From.ID, time.Now())
	if err != nil {
		return fmt.Errorf("persist report decision: %w", err)
	}
	if fresh {
		if !approve && r.Storage != nil && len(reports) > 0 {
			if e := r.Storage.DeleteByMessage(ctx, msgID, r.primChatID); e != nil {
				return fmt.Errorf("report vote cleanup failed; recorded rejection and manual decision retained")
			}
		}
		if approve {
			_ = r.bot.RemoveApprovedUser(uid)
			if c.Text != "" {
				if e := r.bot.UpdateSpam(c.Text); e != nil {
					return fmt.Errorf("report learning failed; decision retained")
				}
			}
		}
		if err = j.EndDecision(ctx, q.ID); err != nil {
			return fmt.Errorf("complete report decision: %w", err)
		}
	}
	return a.answerRecoveryCallback(
		ctx,
		q,
		"Решение по жалобе записано. Независимые действия сохраняются",
	)
}

func (a *admin) recoveryCardInfo(ctx context.Context, q *tbapi.CallbackQuery, c moderation.Card) error {
	return a.recoveryCardInfoPage(ctx, q, c, 0)
}

func (a *admin) recoveryCardInfoPage(ctx context.Context, q *tbapi.CallbackQuery, c moderation.Card, page int) error {
	pageCard := c
	start := min(page*2, len(c.Actions))
	pageCard.Actions = c.Actions[start:min(start+2, len(c.Actions))]
	history := cardDetails(pageCard)
	pages := max(1, (len(c.Actions)+1)/2)
	if pages > 1 {
		history += fmt.Sprintf("\nИстория: страница %d из %d", page+1, pages)
	}
	details := []string{"Данные детекции:"}
	if a.locator != nil {
		if info, found := a.locator.Spam(ctx, c.IdentityID); found {
			for _, check := range info.Checks {
				details = append(details, check.String())
			}
		}
	}
	if len(details) == 1 {
		details = append(details, "Данные детекции недоступны; результаты сохранены в журнале")
	}
	msg := tbapi.NewEditMessageText(
		q.Message.Chat.ID,
		q.Message.MessageID,
		truncateTelegramText(history+"\n\n"+inertSpamText(strings.Join(details, "\n")), 4000),
	)
	authority := moderation.Action{
		Kind:         "ban",
		ChatID:       c.ChatID,
		IdentityID:   c.IdentityID,
		IdentityKind: c.IdentityKind,
		MessageID:    c.MessageID,
	}
	buttons := cardKeyboard(c, a.recovery.gate == nil || a.recovery.gate(authority))
	back := tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("К карточке", "MB:"+c.Ref))
	buttons.InlineKeyboard = append([][]tbapi.InlineKeyboardButton{back}, buttons.InlineKeyboard...)
	navigation := []tbapi.InlineKeyboardButton{}
	if page > 0 {
		navigation = append(navigation, tbapi.NewInlineKeyboardButtonData("← Предыдущие", fmt.Sprintf("MP%d:%s", page-1, c.Ref)))
	}
	if page+1 < pages {
		navigation = append(navigation, tbapi.NewInlineKeyboardButtonData("Следующие →", fmt.Sprintf("MP%d:%s", page+1, c.Ref)))
	}
	if len(navigation) > 0 {
		buttons.InlineKeyboard = append(buttons.InlineKeyboard, navigation)
	}
	msg.LinkPreviewOptions = tbapi.LinkPreviewOptions{IsDisabled: true}
	msg.ReplyMarkup = &buttons
	_, err := a.recovery.api.RequestWithContext(ctx, msg)
	if err != nil {
		return fmt.Errorf("card info delivery: %w", err)
	}
	return a.answerRecoveryCallback(ctx, q, "Показаны подробности")
}

func (a *admin) recoveryLegacyCallback(q *tbapi.CallbackQuery) error {
	uid, msgID, err := parseCallbackData(q.Data)
	if err != nil {
		return fmt.Errorf("legacy callback identity: %w", err)
	}
	text, _ := a.getCleanMessage(q.Message.Text)
	kind := "user"
	if uid < 0 {
		kind = "sender_chat"
	}
	key := fmt.Sprintf("source:%d:%d:%s:%d", a.primChatID, msgID, kind, uid)
	if msgID <= 0 {
		return fmt.Errorf("legacy callback lacks exact source message ID")
	}
	name, _ := a.extractUsername(q.Message.Text)
	p := moderation.Plan{
		UserName:     name,
		Key:          key,
		ChatID:       a.primChatID,
		IdentityID:   uid,
		IdentityKind: kind,
		MessageID:    msgID,
		Destination:  a.adminChatID,
		Origin:       "legacy",
		Card:         true,
		SourceText:   text,
		Actions: []moderation.Action{
			{
				Kind:         "original_delete",
				ChatID:       a.primChatID,
				IdentityID:   uid,
				IdentityKind: kind,
				MessageID:    msgID,
				State:        moderation.Manual,
				Code:         "legacy_context_only",
			},
		},
	}
	if _, _, _, err = a.recovery.journal.Accept(context.Background(), p, time.Now()); err != nil {
		return fmt.Errorf("adopt legacy card: %w", err)
	}
	c, err := a.recovery.journal.CardView(context.Background(), key)
	if err != nil {
		return fmt.Errorf("legacy card: %w", err)
	}
	if err = a.recovery.journal.BindCard(context.Background(), key, q.Message.MessageID); err != nil {
		return fmt.Errorf("bind legacy card: %w", err)
	}
	copyQuery := *q
	prefix := "MU:"
	if strings.HasPrefix(q.Data, "+") {
		prefix = "M+:"
	}
	copyQuery.Data = prefix + c.Ref
	return a.recoveryCardCallback(&copyQuery)
}

func (r *userReports) recoveryBanReporter(ctx context.Context, q *tbapi.CallbackQuery) error {
	uid, msgID, err := parseCallbackData(q.Data)
	if err != nil {
		return fmt.Errorf("reporter callback identity: %w", err)
	}
	if q.Message == nil || q.From == nil || q.Message.Chat.ID != r.adminChatID || r.Storage == nil {
		return fmt.Errorf("reporter callback context unavailable")
	}
	j := r.recoveryAdmin.recovery.journal
	key := "command:" + q.ID
	if c, e := j.CardView(ctx, key); e == nil && c.IdentityID == uid {
		return r.recoveryAdmin.answerRecoveryCallback(
			ctx,
			q,
			"Решение уже записано; результаты — в карточке",
		)
	}
	reports, err := r.Storage.GetByMessage(ctx, msgID, r.primChatID)
	if err != nil {
		return fmt.Errorf("reporter context: %w", err)
	}
	name := ""
	found := false
	for _, report := range reports {
		if report.ReporterUserID == uid {
			if report.AdminMsgID > 0 && report.AdminMsgID != q.Message.MessageID {
				return fmt.Errorf("reporter card mismatch")
			}
			found = true
			name = report.ReporterUserName
		}
	}
	if !found || uid <= 0 || r.superUsers.IsSuper(name, uid) {
		return fmt.Errorf("reporter target safety gate")
	}
	// this command does not know the reporter's original message ID. Never assign the reported user's ID to it.
	p := moderation.Plan{
		Key:          key,
		CommandID:    q.ID,
		ChatID:       r.primChatID,
		IdentityID:   uid,
		IdentityKind: "user",
		Destination:  r.adminChatID,
		Origin:       "operator_reporter",
		DisplayName:  fmt.Sprintf("reporter for report on message %d", msgID),
		Card:         true,
		UserName:     name,
		Learn:        true,
		Actions: []moderation.Action{
			{
				Kind:         "ban",
				ChatID:       r.primChatID,
				IdentityID:   uid,
				IdentityKind: "user",
				UntilDate:    time.Now().Add(9999 * time.Hour).Unix(),
			},
		},
	}
	id, owner, fresh, err := j.Accept(ctx, p, time.Now())
	if err != nil {
		return fmt.Errorf("record reporter moderation: %w", err)
	}
	if fresh {
		if _, err = j.BeginDecision(ctx, q.ID, id, "reporter", q.From.ID); err != nil {
			return fmt.Errorf("record reporter decision: %w", err)
		}
		if err = r.bot.RemoveApprovedUser(uid); err != nil {
			_ = j.Activate(ctx, id, owner, time.Now(), false)
			return fmt.Errorf("reporter approval removal failed; manual review retained")
		}
		if err = j.Activate(ctx, id, owner, time.Now(), true); err != nil {
			return fmt.Errorf("activate reporter moderation: %w", err)
		}
		// withdrawal of the false complaint is independent of the ban API result.
		if err = r.Storage.DeleteReporter(ctx, uid, msgID, r.primChatID); err != nil {
			if len(reports) > 0 {
				_ = j.Withdraw(ctx, fmt.Sprintf("source:%d:%d:user:%d", r.primChatID, msgID, reports[0].ReportedUserID), "report", time.Now())
			}
			return fmt.Errorf("reporter complaint withdrawal failed; ban result remains independent")
		}
		if r.AutoBanThreshold > 0 && len(reports)-1 < r.AutoBanThreshold {
			reportedReceipt := fmt.Sprintf("source:%d:%d:user:%d", r.primChatID, msgID, reports[0].ReportedUserID)
			if err = j.Withdraw(ctx, reportedReceipt, "report", time.Now()); err != nil {
				return fmt.Errorf("withdraw invalidated report authority: %w", err)
			}
		}
		if err = j.EndDecision(ctx, q.ID); err != nil {
			return fmt.Errorf("complete reporter decision: %w", err)
		}
	}
	return r.recoveryAdmin.answerRecoveryCallback(
		ctx,
		q,
		"Решение записано; результаты появятся в карточке",
	)
}

func (a *admin) recoveryCardDisplay(ctx context.Context, q *tbapi.CallbackQuery, c moderation.Card) error {
	authority := moderation.Action{Kind: "ban", ChatID: c.ChatID, IdentityID: c.IdentityID,
		IdentityKind: c.IdentityKind, MessageID: c.MessageID}
	allowed := a.recovery.gate == nil || a.recovery.gate(authority)
	if a.recovery.superName != nil && a.recovery.superName(c.UserName, c.IdentityID) {
		allowed = false
	}
	for _, op := range c.Actions {
		if op.State == moderation.Blocked {
			allowed = false
		}
	}
	buttons := cardKeyboard(c, allowed)
	msg := tbapi.NewEditMessageText(q.Message.Chat.ID, q.Message.MessageID, compactCardText(c, a.recovery.chatName))
	msg.ParseMode = tbapi.ModeHTML
	msg.LinkPreviewOptions = tbapi.LinkPreviewOptions{IsDisabled: true}
	msg.ReplyMarkup = &buttons
	if _, err := a.recovery.api.RequestWithContext(ctx, msg); err != nil {
		return fmt.Errorf("compact card delivery: %w", err)
	}
	return a.answerRecoveryCallback(ctx, q, "Показана карточка")
}
