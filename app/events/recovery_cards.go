package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage"
)

func cardDetails(c moderation.Card) string {
	return operatorDetailPages(c, time.Now())[0]
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

func cardKeyboard(c moderation.Card, allowed bool) tbapi.InlineKeyboardMarkup {
	if c.DeleteOnly {
		return deleteOnlyKeyboard(c, allowed)
	}
	if newHanCard(c) && c.IdentityKind == "message" {
		return tbapi.NewInlineKeyboardMarkup(tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Подробнее", "M?:"+c.Ref)))
	}
	if c.RequestState == "pending" || c.RequestState == "expired" || c.RequestState == "reject_report" {
		rows := [][]tbapi.InlineKeyboardButton{tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Подробнее", "M?:"+c.Ref))}
		if allowed && c.RequestState == "pending" {
			rows = append(rows, tbapi.NewInlineKeyboardRow(
				tbapi.NewInlineKeyboardButtonData("Подтвердить модерацию", "M+:"+c.Ref),
				tbapi.NewInlineKeyboardButtonData("Отклонить запрос", "MR:"+c.Ref)))
		}
		if c.RequestState == "expired" || c.RequestState == "reject_report" {
			rows = append(rows, tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Закрыть запрос", "MC:"+c.Ref)))
		}
		return tbapi.InlineKeyboardMarkup{InlineKeyboard: rows}
	}
	live, manual, decision, _ := cardWork(c)
	rows := [][]tbapi.InlineKeyboardButton{}
	main := []tbapi.InlineKeyboardButton{}
	if allowed && cardHasBan(c) {
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
		label := "Подтвердить модерацию"
		if !cardHasBan(c) {
			label = "Запросить блокировку и удаление"
		}
		rows = append(rows, tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData(label, "M+:"+c.Ref)))
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
	if c.DeleteOnly {
		authority.Kind = "original_delete"
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
	read := j.CardView
	if cardNavigation(parts[0]) || parts[0] == "MA" {
		read = j.ReadCardView
	}
	c, err := read(ctx, parts[1])
	if err != nil {
		stale, lookupErr := j.SupersededDeleteCard(ctx, parts[1])
		if lookupErr == nil && stale {
			return a.answerRecoveryCallback(ctx, q, "Этот запрос заменён. Используйте новую карточку; прежнее подтверждение не действует")
		}
		return fmt.Errorf("load moderation card: %w", err)
	}
	bound, err := j.CardBound(ctx, c, q.Message.Chat.ID, q.Message.MessageID)
	if err != nil {
		return fmt.Errorf("card authority: %w", err)
	}
	if !bound {
		return fmt.Errorf("callback is not bound to this moderation card")
	}
	if cardNavigation(parts[0]) {
		return a.navigateCard(ctx, q, c, parts[0])
	}
	if c.DeleteOnly {
		return a.originalDeleteCallback(ctx, q, c, parts[0])
	}
	request, err := j.IsRequest(ctx, c.Receipt)
	if err != nil {
		return fmt.Errorf("request authority unavailable: %w", err)
	}
	if request {
		if q.From.ID <= 0 || q.From.IsBot || sharedTelegramUser(q.From.ID) || !a.superUsers.IsSuper("", q.From.ID) {
			return a.answerRecoveryCallback(ctx, q, "Подтверждение доступно только уполномоченному пользователю с числовым ID")
		}
		if c.RequestState == "expired" && (parts[0] == "M+" || parts[0] == "MR") {
			return a.answerRecoveryCallback(ctx, q, "Срок запроса истёк. Модерация и обучение не разрешены")
		}
		closedRequest := c.RequestState == "expired" || c.RequestState == "reject_report"
		if c.RequestState != "approve_report" && (!closedRequest || parts[0] != "MC") && parts[0] != "M+" && parts[0] != "MR" &&
			parts[0] != "M?" && parts[0] != "MB" && !strings.HasPrefix(parts[0], "MP") {
			return a.answerRecoveryCallback(ctx, q, "Для запроса доступны подтверждение, отказ и подробности")
		}
		if a.superUsers.IsSuper(c.UserName, c.IdentityID) || a.targetExplicitlyApproved(c.IdentityID) {
			return a.answerRecoveryCallback(ctx, q, "Цель защищена политикой полномочий или явного доверия")
		}
	}
	verb := ""
	switch parts[0] {
	case "MA":
		if a.dry || a.trainingMode {
			return fmt.Errorf("training/dry blocks durable undo")
		}
		label := "Подтвердить снятие блокировки"
		if a.softBan && c.IdentityKind == "user" {
			label = "Подтвердить снятие ограничений"
		}
		buttons := tbapi.NewInlineKeyboardMarkup(
			tbapi.NewInlineKeyboardRow(
				tbapi.NewInlineKeyboardButtonData(label, "MU:"+c.Ref),
				tbapi.NewInlineKeyboardButtonData("← К карточке", "MB:"+c.Ref),
			),
		)
		_, e := a.recovery.api.RequestWithContext(ctx, tbapi.NewEditMessageReplyMarkup(q.Message.Chat.ID, q.Message.MessageID, buttons))
		if e != nil {
			return fmt.Errorf("undo confirmation delivery: %w", e)
		}
		return nil
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
	decisionID := q.ID
	if request && (verb == "approve_report" || verb == "reject_report") {
		decisionID = "sender-request:" + c.Receipt
	}
	fresh, err := j.Decide(ctx, decisionID, c, verb, q.From.ID, time.Now())
	if err != nil {
		return fmt.Errorf("record moderation decision: %w", err)
	}
	if fresh {
		if request && verb == "approve_report" {
			if e := a.bot.RemoveApprovedUser(c.IdentityID); e != nil {
				return fmt.Errorf("trust update failed; decision retained")
			}
		}
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
		if err = j.EndDecision(ctx, decisionID); err != nil {
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
	isRequest, requestErr := j.IsRequest(ctx, receipt)
	if requestErr != nil || isRequest {
		return fmt.Errorf("sender request requires its bound moderation card")
	}
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
	return a.renderCardScreen(ctx, q, c, page, -1)
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
	pendingRequest, requestErr := a.recovery.journal.IsRequest(context.Background(), key)
	if requestErr != nil || pendingRequest {
		return fmt.Errorf("sender request cannot adopt legacy callback authority")
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
	if c.DeleteOnly {
		authority.Kind = "original_delete"
	}
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
