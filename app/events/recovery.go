package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage"
)

type contextualTelegram interface {
	RequestWithContext(context.Context, tbapi.Chattable) (*tbapi.APIResponse, error)
}

type recovery struct {
	chatName  string
	journal   *moderation.Journal
	api       contextualTelegram
	gate      func(moderation.Action) bool
	superName func(string, int64) bool
}

// telegramOutcome classifies typed errors, not strings that might contain credentials.
func telegramOutcome(resp *tbapi.APIResponse, err error) moderation.Outcome {
	if err == nil && resp != nil && resp.Ok {
		return moderation.Outcome{State: moderation.Confirmed}
	}
	var apiErr *tbapi.Error
	if errors.As(err, &apiErr) {
		if apiErr.Code == 0 {
			return moderation.Outcome{Retry: true, Uncertain: true, Code: "api_response_unknown"}
		}
		if apiErr.Code == 429 {
			return moderation.Outcome{
				Retry:      true,
				Code:       "telegram_429",
				RetryAfter: time.Duration(apiErr.RetryAfter) * time.Second,
			}
		}
		switch apiErr.Code {
		case 500, 502, 503, 504:
			return moderation.Outcome{Retry: true, Uncertain: true, Code: fmt.Sprintf("telegram_%d", apiErr.Code)}
		}
		return moderation.Outcome{State: moderation.Failed, Code: fmt.Sprintf("telegram_%d", apiErr.Code)}
	}
	if err == nil && resp != nil {
		e := &tbapi.Error{Code: resp.ErrorCode, Message: resp.Description}
		if resp.Parameters != nil {
			e.ResponseParameters = *resp.Parameters
		}
		return telegramOutcome(nil, e)
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) ||
		err == nil {
		return moderation.Outcome{Retry: true, Uncertain: true, Code: "transport_outcome_unknown"}
	}
	return moderation.Outcome{State: moderation.Failed, Code: "unclassified_permanent_error"}
}

func (r *recovery) Execute(ctx context.Context, a moderation.Action) moderation.Outcome {
	if ctx.Err() != nil {
		return moderation.Outcome{Retry: true, Code: "request_not_started"}
	}
	if a.Kind == "card" {
		return r.notifyCard(ctx, a)
	}
	if a.Kind == "notice" {
		return r.notify(ctx, a)
	}
	if !r.gate(a) {
		return moderation.Outcome{State: moderation.Blocked, Code: "current_safety_gate", Uncertain: a.Uncertain}
	}
	if a.ChatID == 0 || a.IdentityID == 0 || (a.IdentityKind != "user" && a.IdentityKind != "sender_chat") ||
		a.IdentityKind == "user" && a.IdentityID < 0 ||
		a.IdentityKind == "sender_chat" && a.IdentityID > 0 ||
		(a.Kind == "original_delete" || a.Kind == "cleanup_delete") && a.MessageID <= 0 {
		return moderation.Outcome{State: moderation.Blocked, Code: "invalid_persisted_authority", Uncertain: a.Uncertain}
	}
	// resolve current usernames/admin status rather than persisting names with delayed work.
	if out := r.authorization(ctx, a); out != nil {
		return *out
	}

	var member tbapi.ChatMember
	if a.IdentityKind == "user" {
		resp, err := r.api.RequestWithContext(ctx, tbapi.NewGetChatMember(a.ChatID, a.IdentityID))
		if out := telegramOutcome(resp, err); out.State != moderation.Confirmed {
			out.Uncertain = a.Uncertain
			return out
		}
		if err = json.Unmarshal(resp.Result, &member); err != nil {
			return moderation.Outcome{Retry: true, Uncertain: a.Uncertain, Code: "membership_response_invalid"}
		}
		if member.User == nil || member.User.ID != a.IdentityID || member.Status == "" {
			return moderation.Outcome{Retry: true, Uncertain: a.Uncertain, Code: "membership_identity_unavailable"}
		}
		name := ""
		if member.User != nil {
			name = member.User.UserName
		}
		if member.Status == "administrator" || member.Status == "creator" || r.superName(name, a.IdentityID) {
			return moderation.Outcome{State: moderation.Blocked, Code: "current_superuser_gate", Uncertain: a.Uncertain}
		}
	} else {
		resp, err := r.api.RequestWithContext(ctx, tbapi.ChatInfoConfig{ChatConfig: tbapi.ChatConfig{ChatID: a.IdentityID}})
		if out := telegramOutcome(resp, err); out.State != moderation.Confirmed {
			out.Uncertain = a.Uncertain
			return out
		}
		var chat tbapi.ChatFullInfo
		if json.Unmarshal(resp.Result, &chat) != nil || chat.ID != a.IdentityID {
			return moderation.Outcome{Retry: true, Uncertain: a.Uncertain, Code: "sender_chat_identity_unavailable"}
		}
		if r.superName(chat.UserName, a.IdentityID) {
			return moderation.Outcome{State: moderation.Blocked, Code: "current_superuser_gate", Uncertain: a.Uncertain}
		}
	}
	if ctx.Err() != nil {
		return moderation.Outcome{Retry: true, Uncertain: a.Uncertain, Code: "request_not_started"}
	}
	if !r.gate(a) {
		return moderation.Outcome{State: moderation.Blocked, Code: "current_safety_gate", Uncertain: a.Uncertain}
	}
	if a.Kind == "ban" || a.Kind == "restrict" {
		if a.Uncertain && a.IdentityKind == "sender_chat" {
			return moderation.Outcome{
				State:     moderation.Manual,
				Code:      "sender_chat_ban_cannot_be_reconciled",
				Uncertain: true,
			}
		}
		requestedPermanent := a.UntilDate == 0 || a.UntilDate >= time.Now().Add(366*24*time.Hour).Unix()
		expiryMatches := member.UntilDate == 0 || !requestedPermanent && member.UntilDate >= a.UntilDate
		fullyRestricted := !member.CanSendMessages && !member.CanSendAudios && !member.CanSendDocuments &&
			!member.CanSendPhotos &&
			!member.CanSendVideos &&
			!member.CanSendVideoNotes &&
			!member.CanSendVoiceNotes &&
			!member.CanSendPolls &&
			!member.CanSendOtherMessages &&
			!member.CanChangeInfo &&
			!member.CanInviteUsers &&
			!member.CanPinMessages
		if expiryMatches &&
			(a.Kind == "ban" && member.Status == "kicked" || a.Kind == "restrict" && member.Status == "restricted" && fullyRestricted) {
			return moderation.Outcome{State: moderation.Confirmed, Code: "reconciled_membership"}
		}
	}
	if a.IdentityKind == "user" &&
		((a.Kind == "unban" && member.Status != "kicked") || (a.Kind == "restore" && member.Status != "restricted")) {
		return moderation.Outcome{State: moderation.Confirmed, Code: "reconciled_unbanned_membership"}
	}
	var request tbapi.Chattable
	switch a.Kind {
	case "ban":
		if a.IdentityKind == "sender_chat" {
			request = tbapi.BanChatSenderChatConfig{
				ChatConfig:   tbapi.ChatConfig{ChatID: a.ChatID},
				SenderChatID: a.IdentityID,
				UntilDate:    int(a.UntilDate),
			}
		} else {
			request = tbapi.BanChatMemberConfig{
				ChatMemberConfig: tbapi.ChatMemberConfig{ChatConfig: tbapi.ChatConfig{ChatID: a.ChatID}, UserID: a.IdentityID},
				UntilDate:        a.UntilDate,
			}
		}
	case "unban":
		if a.IdentityKind == "sender_chat" {
			request = tbapi.UnbanChatSenderChatConfig{
				ChatConfig:   tbapi.ChatConfig{ChatID: a.ChatID},
				SenderChatID: a.IdentityID,
			}
		} else {
			request = tbapi.UnbanChatMemberConfig{
				ChatMemberConfig: tbapi.ChatMemberConfig{ChatConfig: tbapi.ChatConfig{ChatID: a.ChatID}, UserID: a.IdentityID},
				OnlyIfBanned:     true,
			}
		}
	case "restore":
		request = tbapi.RestrictChatMemberConfig{
			ChatMemberConfig: tbapi.ChatMemberConfig{ChatConfig: tbapi.ChatConfig{ChatID: a.ChatID}, UserID: a.IdentityID},
			Permissions: &tbapi.ChatPermissions{
				CanSendMessages:   true,
				CanSendAudios:     true,
				CanSendDocuments:  true,
				CanSendPhotos:     true,
				CanSendVideos:     true,
				CanSendVideoNotes: true,
				CanSendVoiceNotes: true,

				CanSendOtherMessages: true,

				CanInviteUsers: true,
				CanChangeInfo:  true, CanPinMessages: true,
			},
		}
	case "restrict":
		request = tbapi.RestrictChatMemberConfig{
			ChatMemberConfig: tbapi.ChatMemberConfig{ChatConfig: tbapi.ChatConfig{ChatID: a.ChatID}, UserID: a.IdentityID},
			UntilDate:        a.UntilDate,
			Permissions:      &tbapi.ChatPermissions{},
		}
	case "original_delete", "cleanup_delete":
		request = tbapi.DeleteMessageConfig{
			BaseChatMessage: tbapi.BaseChatMessage{ChatConfig: tbapi.ChatConfig{ChatID: a.ChatID}, MessageID: a.MessageID},
		}
	default:
		return moderation.Outcome{State: moderation.Blocked, Code: "invalid_action_kind"}
	}
	if out := r.authorization(ctx, a); out != nil {
		return *out
	}

	resp, err := r.api.RequestWithContext(ctx, request)
	out := telegramOutcome(resp, err)
	if out.State != moderation.Confirmed {
		out.Uncertain = out.Uncertain || a.Uncertain
	}
	if out.State == moderation.Confirmed {
		var acknowledged bool
		if json.Unmarshal(resp.Result, &acknowledged) != nil || !acknowledged {
			out = moderation.Outcome{Retry: true, Uncertain: true, Code: "mutation_ack_unknown"}
		}
	}
	if (a.Kind == "original_delete" || a.Kind == "cleanup_delete") && isSoftDeleteMiss(err) {
		return moderation.Outcome{
			State:     moderation.Absent,
			Code:      "absent_or_inaccessible_not_confirmed",
			Uncertain: a.Uncertain,
		}
	}
	var refusal *tbapi.Error
	if (a.Kind == "original_delete" || a.Kind == "cleanup_delete") && errors.As(err, &refusal) && refusal.Code == 400 &&
		strings.Contains(strings.ToLower(refusal.Message), "message can't be deleted") {
		out.Code = "delete_refused_age_not_proven_manual_required"
	}
	return out
}

func (r *recovery) notify(ctx context.Context, a moderation.Action) moderation.Outcome {
	if !r.gate(a) {
		return moderation.Outcome{State: moderation.Blocked, Code: "notification_destination_changed"}
	}
	counts, rows, rejected, err := r.journal.Digest(ctx, a.ChatID)
	if err != nil {
		return moderation.Outcome{Retry: true, Code: "journal_summary_unavailable"}
	}
	text := warningSummary(counts, rows, rejected)
	if text == "" {
		if a.NotificationID <= 0 {
			return moderation.Outcome{State: moderation.Confirmed}
		}
		text = "✅ Проблемы модерации устранены. Результаты — в карточках"
	}
	var request tbapi.Chattable = tbapi.NewMessage(a.ChatID, text)
	if a.NotificationID > 0 {
		request = tbapi.NewEditMessageText(a.ChatID, a.NotificationID, text)
	}
	resp, err := r.api.RequestWithContext(ctx, request)
	var apiErr *tbapi.Error
	if errors.As(err, &apiErr) && apiErr.Code == 400 && a.NotificationID > 0 &&
		(strings.Contains(strings.ToLower(apiErr.Message), "message to edit not found") ||
			strings.Contains(strings.ToLower(apiErr.Message), "message can't be edited")) {
		return moderation.Outcome{Retry: true, Code: "notification_anchor_lost", NotificationID: -1}
	}
	if errors.As(err, &apiErr) && apiErr.Code == 400 &&
		strings.Contains(strings.ToLower(apiErr.Message), "message is not modified") {
		return moderation.Outcome{State: moderation.Confirmed, NotificationID: a.NotificationID}
	}
	out := telegramOutcome(resp, err)
	if out.State == moderation.Confirmed {
		var sent tbapi.Message
		if json.Unmarshal(resp.Result, &sent) == nil && sent.MessageID > 0 {
			out.NotificationID = sent.MessageID
		} else {
			return moderation.Outcome{Retry: true, Uncertain: true, Code: "notification_ack_unknown"}
		}
	}
	return out
}

// submit owns non-replayable learning, then releases only persistent API actions to the worker.
func (a *admin) submitRecovery(p moderation.Plan, learn func() error) error {
	_, err := a.acceptRecovery(p, learn)
	if errors.Is(err, moderation.ErrCapacity) {
		log.Printf("[WARN] durable moderation capacity rejection; coalesced administrator signal retained")
		return nil
	}
	if err != nil {
		return fmt.Errorf("moderation recovery: %w", err)
	}
	return nil
}

func (a *admin) acceptRecovery(p moderation.Plan, learn func() error) (bool, error) {
	p.Learn = learn != nil
	ctx := context.Background()
	now := time.Now()
	id, owner, created, err := a.recovery.journal.Accept(ctx, p, now)
	if err != nil {
		return false, fmt.Errorf("accept moderation: %w", err)
	}
	if !created {
		return false, nil
	}
	if !p.Learn {
		return true, nil
	}
	if a.recovery.gate != nil && !a.recovery.gate(p.Actions[0]) {
		_ = a.recovery.journal.Activate(ctx, id, owner, time.Now(), false)
		return true, fmt.Errorf("current safety gate blocked moderation preparation")
	}
	if learn != nil {
		if err = learn(); err != nil {
			_ = a.recovery.journal.Activate(ctx, id, owner, time.Now(), false)
			return true, fmt.Errorf("durable moderation learning failed; manual review required")
		}
	}
	if err := a.recovery.journal.Activate(ctx, id, owner, time.Now(), true); err != nil {
		return true, fmt.Errorf("activate durable moderation: %w", err)
	}
	return true, nil
}

func (a *admin) enqueueForward(
	update tbapi.Update,
	identityID int64,
	msgID int,
	knownExact bool,
	learnText string,
	cleanup bool,
) error {
	if update.Message == nil || update.Message.Chat.ID == 0 {
		return fmt.Errorf("missing delivery chat; durable action blocked")
	}
	kind := "user"
	if identityID < 0 {
		kind = "sender_chat"
	}
	key := fmt.Sprintf("delivery:%d:%d", update.Message.Chat.ID, update.Message.MessageID)
	if knownExact {
		key = fmt.Sprintf("source:%d:%d:%s:%d", a.primChatID, msgID, kind, identityID)
	}
	p := moderation.Plan{
		Origin: "forward", Card: true, SourceText: learnText,
		Key:             key,
		ChatID:          a.primChatID,
		MessageID:       msgID,
		IdentityID:      identityID,
		IdentityKind:    kind,
		Destination:     a.adminChatID,
		DeliveryChat:    update.Message.Chat.ID,
		DeliveryMessage: update.Message.MessageID,
	}
	p.Origin = fmt.Sprintf("forward:%d:%d", p.DeliveryChat, p.DeliveryMessage)
	identity := adminForwardOriginIdentity(update.Message.ForwardOrigin)
	p.UserName = identity.Name
	base := moderation.Action{ChatID: a.primChatID, IdentityID: identityID, IdentityKind: kind}
	if identityID != a.primChatID {
		ban := base
		ban.Kind = "ban"
		ban.UntilDate = time.Now().Add(bot.PermanentBanDuration).Unix()
		p.Actions = append(p.Actions, ban)
	}
	if msgID > 0 {
		del := base
		del.Kind = "original_delete"
		del.MessageID = msgID
		del.MessageDate = locatorMessageDate(a.locator, a.primChatID, msgID)
		p.Actions = append(p.Actions, del)
	} else {
		del := base
		del.Kind = "original_delete"
		del.State = moderation.Manual
		del.Code = "locator_fallback_original_unlocated"
		p.Actions = append(p.Actions, del)
	}
	if cleanup && identityID != a.primChatID {
		var ids []int
		var err error
		if scoped, ok := a.locator.(interface {
			GetUserMessageIDsInChat(context.Context, int64, int64, int) ([]int, error)
		}); ok {
			ids, err = scoped.GetUserMessageIDsInChat(
				context.Background(),
				identityID,
				a.primChatID,
				a.aggressiveCleanupLimit,
			)
		} else {
			ids, err = a.locator.GetUserMessageIDs(context.Background(), identityID, a.aggressiveCleanupLimit)
		}
		if err != nil {
			item := base
			item.Kind = "cleanup_delete"
			item.State = moderation.Manual
			item.Code = "cleanup_selection_unavailable"
			p.Actions = append(p.Actions, item)
		}
		for _, id := range ids {
			if id == msgID {
				continue
			}
			del := base
			del.Kind = "cleanup_delete"
			del.MessageID = id
			del.MessageDate = locatorMessageDate(a.locator, a.primChatID, id)
			p.Actions = append(p.Actions, del)
		}
	}
	return a.submitRecovery(p, func() error {
		_ = a.bot.RemoveApprovedUser(identityID)
		if learnText != "" {
			return a.bot.UpdateSpam(learnText)
		}
		return nil
	})
}

func (l *TelegramListener) enqueueAutomatic(update tbapi.Update, msg *bot.Message, resp bot.Response) (bool, error) {
	id := msg.From.ID
	kind := "user"
	if msg.SenderChat.ID != 0 {
		id = msg.SenderChat.ID
		kind = "sender_chat"
	}
	if id == 0 || l.SuperUsers.IsSuper(msg.From.Username, msg.From.ID) {
		return false, nil
	}
	p := moderation.Plan{
		Origin: "detector", Card: true, SourceText: msg.Text + sourceQuote(msg),
		UserName: msg.From.Username, DisplayName: msg.From.DisplayName,
		Key:          fmt.Sprintf("source:%d:%d:%s:%d", update.Message.Chat.ID, msg.ID, kind, id),
		ChatID:       update.Message.Chat.ID,
		MessageID:    msg.ID,
		IdentityKind: kind,
		IdentityID:   id,
		Destination:  l.adminChatID,
	}
	base := moderation.Action{ChatID: p.ChatID, IdentityKind: kind, IdentityID: id}
	if resp.HanDeleteOnly {
		checker, ok := l.Bot.(interface{ IsExplicitTrustedUser(int64) bool })
		if ok && checker.IsExplicitTrustedUser(id) || l.SuperUsers.IsSuper(msg.SenderChat.UserName, id) {
			return false, nil
		}
		p.SourceText = msg.Text
		for _, check := range resp.CheckResults {
			if check.Name == "han script" {
				p.Reason = check.Details
			}
		}
		p.Actions = []moderation.Action{{Kind: "original_delete", ChatID: p.ChatID, IdentityKind: kind,
			IdentityID: id, MessageID: msg.ID, MessageDate: int64(update.Message.Date)}}
		return l.adminHandler.acceptRecovery(p, nil)
	}
	if resp.BanInterval > 0 {
		b := base
		b.Kind = "ban"
		if l.SoftBanMode && kind == "user" {
			b.Kind = "restrict"
		}
		d := resp.BanInterval
		if d < 30*time.Second {
			d = time.Minute
		}
		b.UntilDate = time.Now().Add(d).Unix()
		p.Actions = append(p.Actions, b)
	}
	if resp.DeleteReplyTo && resp.ReplyTo != 0 {
		d := base
		d.Kind = "original_delete"
		d.MessageID = resp.ReplyTo
		d.MessageDate = int64(update.Message.Date)
		if resp.ReplyTo != msg.ID {
			d.MessageDate = locatorMessageDate(l.Locator, p.ChatID, resp.ReplyTo)
		}
		p.Actions = append(p.Actions, d)
	}
	for _, check := range resp.CheckResults {
		if !check.Spam {
			continue
		}
		for _, id := range check.ExtraDeleteIDs {
			d := base
			d.Kind = "cleanup_delete"
			d.MessageID = id
			d.MessageDate = locatorMessageDate(l.Locator, p.ChatID, id)
			p.Actions = append(p.Actions, d)
		}
	}
	if len(p.Actions) == 0 {
		return false, nil
	}
	return l.adminHandler.acceptRecovery(p, nil)
}

// Community auto-ban keeps its existing best-effort learning/approval-removal contract.
// Manual report callbacks remain unchanged; reports are retained so their controls stay usable.
func (r *userReports) enqueueThresholdRecovery(ctx context.Context, report storage.Report, date int64) error {
	if report.ReportedUserID <= 0 || r.superUsers.IsSuper(report.ReportedUserName, report.ReportedUserID) {
		return fmt.Errorf("community auto-ban safety gate blocked durable admission")
	}
	p := moderation.Plan{
		Origin: reportBatchOrigin(report), Card: true, SourceText: report.MsgText, UserName: report.ReportedUserName,
		Key:              fmt.Sprintf("source:%d:%d:user:%d", report.ChatID, report.MsgID, report.ReportedUserID),
		ChatID:           report.ChatID,
		MessageID:        report.MsgID,
		IdentityID:       report.ReportedUserID,
		IdentityKind:     "user",
		Destination:      r.adminChatID,
		Learn:            true,
		LearningOptional: true,
	}
	b := moderation.Action{
		Kind:         "ban",
		ChatID:       report.ChatID,
		IdentityID:   report.ReportedUserID,
		IdentityKind: "user",
		UntilDate:    time.Now().Add(bot.PermanentBanDuration).Unix(),
	}
	if r.softBanMode {
		b.Kind = "restrict"
	}
	d := moderation.Action{
		Kind:         "original_delete",
		ChatID:       report.ChatID,
		IdentityID:   report.ReportedUserID,
		IdentityKind: "user",
		MessageID:    report.MsgID,
		MessageDate:  date,
	}
	p.Actions = []moderation.Action{b, d}
	j := r.recoveryAdmin.recovery.journal
	id, owner, created, err := j.Accept(ctx, p, time.Now())
	if err != nil {
		return fmt.Errorf("moderation recovery: %w", err)
	}
	if !created {
		return nil
	}
	if report.AdminMsgID > 0 {
		if err = j.BindCard(ctx, id, report.AdminMsgID); err != nil {
			return fmt.Errorf("bind threshold report card: %w", err)
		}
	}
	_ = r.bot.RemoveApprovedUser(report.ReportedUserID)
	learningOK := true
	if report.MsgText != "" {
		learningOK = r.bot.UpdateSpam(report.MsgText) == nil
	}
	if err := j.Activate(ctx, id, owner, time.Now(), learningOK); err != nil {
		return fmt.Errorf("activate threshold moderation: %w", err)
	}
	return nil
}

// needsRecovery leaves ordinary bot responses on their established notification path.
func needsRecovery(resp bot.Response) bool {
	if resp.BanInterval > 0 || resp.DeleteReplyTo && resp.ReplyTo > 0 {
		return true
	}
	for _, check := range resp.CheckResults {
		if check.Spam && len(check.ExtraDeleteIDs) > 0 {
			return true
		}
	}
	return false
}

func sourceQuote(msg *bot.Message) string {
	if msg.Quote != "" {
		return "\n" + msg.Quote
	}
	return ""
}

func (r *recovery) authorization(ctx context.Context, a moderation.Action) *moderation.Outcome {
	if r.journal == nil {
		return nil
	}
	mode, err := r.journal.Authorize(ctx, a)
	if err != nil {
		return &moderation.Outcome{Retry: true, Uncertain: a.Uncertain, Code: "authority_store_unavailable"}
	}
	if mode == "waiting_for_undo" || mode == "waiting_for_operator" {
		return &moderation.Outcome{Retry: true, Uncertain: a.Uncertain, Code: "operator_undo_pending"}
	}
	if mode == "withdrawn" {
		if a.Uncertain {
			return &moderation.Outcome{State: moderation.Manual, Uncertain: true, Code: "withdrawn_request_outcome_unknown"}
		}
		return &moderation.Outcome{State: moderation.Canceled, Code: "withdrawn_before_execution"}
	}
	return nil
}

func reportBatchOrigin(report storage.Report) string {
	if report.ID > 0 {
		return fmt.Sprintf("report:%d", report.ID)
	}
	return "report"
}
