package events

import (
	"context"
	"encoding/json"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/umputun/tg-spam/app/moderation"
)

// forwardOperatorRole grants only manual-forward authority. It never reads or
// changes superusers/trust. No role cache: every boundary asks Telegram again.
func forwardOperatorRole(ctx context.Context, api contextualTelegram, chat, uid int64) string {
	if chat == 0 || uid <= 0 || sharedTelegramUser(uid) {
		return "forward_operator_invalid"
	}
	if api == nil {
		return "forward_operator_role_unavailable"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := api.RequestWithContext(ctx, tbapi.NewGetChatMember(chat, uid))
	if err != nil || resp == nil || !resp.Ok {
		return "forward_operator_role_unavailable"
	}
	var member tbapi.ChatMember
	if json.Unmarshal(resp.Result, &member) != nil || member.User == nil || member.User.ID != uid ||
		member.User.IsBot || member.Status == "" {
		return "forward_operator_role_unavailable"
	}
	switch member.Status {
	case "creator", "administrator":
		return ""
	case "member", "restricted", "left", "kicked":
		return "forward_operator_revoked"
	default:
		return "forward_operator_role_unavailable"
	}
}

func (a *admin) forwardOperator(ctx context.Context, user *tbapi.User) string {
	if user == nil || user.IsBot || user.ID <= 0 || sharedTelegramUser(user.ID) {
		return "forward_operator_invalid"
	}
	api, _ := a.tbAPI.(contextualTelegram)
	return forwardOperatorRole(ctx, api, a.adminChatID, user.ID)
}

func forwardOperatorFeedback(code string) string {
	if code == "forward_operator_role_unavailable" {
		return "Права не подтверждены: Telegram временно недоступен для проверки роли в админ-чате. " +
			"Новые действия не приняты. Повторите пересылку или выбор позже."
	}
	return "Отказ: ручная модерация доступна только личному пользователю, " +
		"который сейчас владелец или администратор этого админ-чата."
}

// The saved destination prevents a changed configuration from borrowing operator
// authority from another chat. A failed lookup is recoverable, not a ban failure.
func (r *recovery) forwardOperatorGate(ctx context.Context, a moderation.Action) *moderation.Outcome {
	if r.journal == nil {
		return nil
	}
	requester, destination, err := r.journal.ForwardAuthority(ctx, a.Receipt)
	if err != nil {
		return &moderation.Outcome{Retry: true, Code: "forward_authority_unavailable", Uncertain: a.Uncertain}
	}
	if requester == 0 {
		return nil // Other moderation paths retain their existing authority rules.
	}
	code := "forward_operator_destination_changed"
	if destination == r.adminChatID && destination != 0 {
		code = forwardOperatorRole(ctx, r.api, destination, requester)
	}
	if code == "" {
		return nil
	}
	if code == "forward_operator_role_unavailable" {
		return &moderation.Outcome{Retry: true, Code: code, Uncertain: a.Uncertain}
	}
	if a.Uncertain {
		return &moderation.Outcome{State: moderation.Manual, Code: "forward_operator_revoked_outcome_unknown", Uncertain: true}
	}
	return &moderation.Outcome{State: moderation.Blocked, Code: code}
}
