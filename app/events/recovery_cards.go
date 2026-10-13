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

func cardText(c moderation.Card) string {
	var text strings.Builder
	scope := "exact original"
	if strings.HasPrefix(c.Receipt, "delivery:") {
		scope = "delivery-scoped locator candidate; exact original identity unavailable"
	}
	if strings.HasPrefix(c.Receipt, "command:") {
		scope = "admin command; original message ID unavailable"
	}
	fmt.Fprintf(&text, "Scope: %s\n", scope)
	fmt.Fprintf(
		&text,
		"Moderation for %s %s (identity=%d, kind=%s)\nsource chat=%d message=%d\n\nOriginal text:\n%s\n\nFactual results:\n",
		c.DisplayName,
		c.UserName,
		c.IdentityID,
		c.IdentityKind,
		c.ChatID,
		c.MessageID,
		truncateRunes(c.Text, 1800),
	)
	for i, a := range c.Actions {
		if i >= 12 {
			fmt.Fprintf(&text, "%d further action results in journal\n", len(c.Actions)-i)
			break
		}
		fmt.Fprintf(&text, "%s message=%d: %s (%s), uncertain=%t\n", a.Kind, a.MessageID, a.State, a.Code, a.Uncertain)
	}
	fmt.Fprintf(
		&text,
		"Explicit operator resolutions: %d; unfinished local decisions: %d\n",
		len(c.Resolved),
		c.IncompleteDecisions,
	)
	originState := map[string]bool{}
	for _, d := range c.Origins {
		originState[d.Origin] = originState[d.Origin] || d.Active
	}
	fmt.Fprintf(&text, "Authorities: %v\n", originState)
	if c.Held {
		text.WriteString("Earlier authorities remain stopped. New independent requirements are separate; " +
			"unfinished undo must settle first.\n")
	}
	text.WriteString(
		"Running/unknown requests may already have reached Telegram; withdrawal cannot undo an in-flight request. " +
			"Manual closure records operator resolution, not API success.",
	)
	return truncateRunes(text.String(), 3900)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

func cardKeyboard(c moderation.Card, allowed bool) tbapi.InlineKeyboardMarkup {
	rows := [][]tbapi.InlineKeyboardButton{
		tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Info", "M?:"+c.Ref)),
	}
	live := false
	for _, a := range c.Actions {
		live = live || a.State == moderation.Pending || a.State == moderation.Running
	}
	if !live {
		rows = append(rows, tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Close manual work", "MC:"+c.Ref)))
	}
	if !allowed {
		return tbapi.InlineKeyboardMarkup{InlineKeyboard: rows}
	}
	rows = append(
		rows,
		tbapi.NewInlineKeyboardRow(
			tbapi.NewInlineKeyboardButtonData("Cancel retries", "M-:"+c.Ref),
			tbapi.NewInlineKeyboardButtonData("Unban / restore", "MA:"+c.Ref),
		),
	)
	report := false
	for _, d := range c.Origins {
		report = report || (d.Origin == "report" || strings.HasPrefix(d.Origin, "report:"))
	}
	if report {
		rows = append(rows, tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Reject report only", "MR:"+c.Ref)))
	}
	if allowed {
		rows = append(rows, tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Approve moderation", "M+:"+c.Ref)))
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
		m := tbapi.NewEditMessageText(a.ChatID, a.NotificationID, cardText(c))
		m.ReplyMarkup = &buttons
		req = m
	} else {
		m := tbapi.NewMessage(a.ChatID, cardText(c))
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
	verb := ""
	switch parts[0] {
	case "MA":
		if a.dry || a.trainingMode {
			return fmt.Errorf("training/dry blocks durable undo")
		}
		buttons := tbapi.NewInlineKeyboardMarkup(
			tbapi.NewInlineKeyboardRow(
				tbapi.NewInlineKeyboardButtonData("Confirm unban / restore", "MU:"+c.Ref),
				tbapi.NewInlineKeyboardButtonData("Back to info", "M?:"+c.Ref),
			),
		)
		_, e := a.recovery.api.RequestWithContext(ctx, tbapi.NewEditMessageReplyMarkup(q.Message.Chat.ID, q.Message.MessageID, buttons))
		if e != nil {
			return fmt.Errorf("undo confirmation delivery: %w", e)
		}
		return nil
	case "M?":
		return a.recoveryCardInfo(ctx, q, c)
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
		"Decision recorded; actual API results follow. In-flight requests cannot be recalled",
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
		// Report-storage TTL must not invalidate the durable original context.
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
		"Report decision recorded; independent detector actions remain. In-flight effects may already have occurred",
	)
}

func (a *admin) recoveryCardInfo(ctx context.Context, q *tbapi.CallbackQuery, c moderation.Card) error {
	details := []string{"Original detection results:"}
	if a.locator != nil {
		if info, found := a.locator.Spam(ctx, c.IdentityID); found {
			for _, check := range info.Checks {
				details = append(details, check.String())
			}
		}
	}
	if len(details) == 1 {
		details = append(details, "Detection evidence unavailable; original context and API results retained in journal")
	}
	msg := tbapi.NewEditMessageText(
		q.Message.Chat.ID,
		q.Message.MessageID,
		truncateRunes(cardText(c)+"\n\n"+strings.Join(details, "\n"), 4000),
	)
	authority := moderation.Action{
		Kind:         "ban",
		ChatID:       c.ChatID,
		IdentityID:   c.IdentityID,
		IdentityKind: c.IdentityKind,
		MessageID:    c.MessageID,
	}
	buttons := cardKeyboard(c, a.recovery.gate == nil || a.recovery.gate(authority))
	msg.ReplyMarkup = &buttons
	_, err := a.recovery.api.RequestWithContext(ctx, msg)
	if err != nil {
		return fmt.Errorf("card info delivery: %w", err)
	}
	return a.answerRecoveryCallback(ctx, q, "Original context and current recorded results shown")
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
			"Reporter decision already recorded; actual results in its card",
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
	// This command does not know the reporter's original message ID. Never assign the reported user's ID to it.
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
		// Withdrawal of the false complaint is independent of the ban API result.
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
		"Reporter moderation recorded; ban API results follow in a separate card",
	)
}
