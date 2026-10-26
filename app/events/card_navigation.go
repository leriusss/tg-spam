package events

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/umputun/tg-spam/app/moderation"
)

func cardNavigation(verb string) bool {
	return verb == "M?" || verb == "MB" || strings.HasPrefix(verb, "MP") || strings.HasPrefix(verb, "MT")
}

func cardHasBan(c moderation.Card) bool {
	for _, op := range c.Actions {
		if (op.Kind == "ban" || op.Kind == "restrict") && op.State != moderation.Canceled {
			return true
		}
	}
	return false
}

func (a *admin) navigateCard(ctx context.Context, q *tbapi.CallbackQuery, c moderation.Card, verb string) error {
	if q.From.ID <= 0 || q.From.IsBot || sharedTelegramUser(q.From.ID) {
		return fmt.Errorf("invalid navigation operator")
	}
	request, err := a.recovery.journal.IsRequest(ctx, c.Receipt)
	if err != nil {
		return fmt.Errorf("navigation request context unavailable: %w", err)
	}
	if c.DeleteOnly || request {
		if !a.superUsers.IsSuper("", q.From.ID) {
			return a.answerRecoveryCallback(ctx, q, "Просмотр запроса доступен уполномоченному пользователю с числовым ID")
		}
	}
	if request && !c.DeleteOnly && (a.superUsers.IsSuper(c.UserName, c.IdentityID) || a.targetExplicitlyApproved(c.IdentityID)) {
		return a.answerRecoveryCallback(ctx, q, "Цель защищена политикой полномочий или явного доверия")
	}
	if verb == "MB" {
		return a.recoveryCardDisplay(ctx, q, c)
	}
	if c.OriginalDate == 0 {
		c.OriginalDate = locatorMessageDate(a.locator, c.ChatID, c.MessageID)
	}
	if verb == "M?" {
		return a.recoveryCardInfo(ctx, q, c)
	}
	page, technical := 0, -1
	if value, ok := strings.CutPrefix(verb, "MP"); ok {
		page, err = strconv.Atoi(value)
	}
	if value, ok := strings.CutPrefix(verb, "MT"); ok {
		parts := strings.Split(value, ".")
		if len(parts) != 2 {
			return fmt.Errorf("invalid technical page")
		}
		page, err = strconv.Atoi(parts[0])
		if err == nil {
			technical, err = strconv.Atoi(parts[1])
		}
		if technical < 0 {
			return fmt.Errorf("invalid technical page")
		}
	}
	if err != nil {
		return fmt.Errorf("invalid navigation page: %w", err)
	}
	if technical < 0 {
		return a.recoveryCardInfoPage(ctx, q, c, page)
	}
	return a.renderCardScreen(ctx, q, c, page, technical)
}

func (a *admin) detailKeyboard(c moderation.Card, page, count, technical, techCount int) tbapi.InlineKeyboardMarkup {
	if technical >= 0 {
		rows := [][]tbapi.InlineKeyboardButton{tbapi.NewInlineKeyboardRow(
			tbapi.NewInlineKeyboardButtonData("← В Подробнее", fmt.Sprintf("MP%d:%s", page, c.Ref)))}
		nav := []tbapi.InlineKeyboardButton{}
		if technical > 0 {
			nav = append(nav, tbapi.NewInlineKeyboardButtonData("← Предыдущая", fmt.Sprintf("MT%d.%d:%s", page, technical-1, c.Ref)))
		}
		if technical+1 < techCount {
			nav = append(nav, tbapi.NewInlineKeyboardButtonData("Следующая →", fmt.Sprintf("MT%d.%d:%s", page, technical+1, c.Ref)))
		}
		if len(nav) > 0 {
			rows = append(rows, nav)
		}
		return tbapi.InlineKeyboardMarkup{InlineKeyboard: rows}
	}
	rows := [][]tbapi.InlineKeyboardButton{tbapi.NewInlineKeyboardRow(
		tbapi.NewInlineKeyboardButtonData("← К карточке", "MB:"+c.Ref)),
		tbapi.NewInlineKeyboardRow(tbapi.NewInlineKeyboardButtonData("Технические данные", fmt.Sprintf("MT%d.0:%s", page, c.Ref)))}
	nav := []tbapi.InlineKeyboardButton{}
	if page > 0 {
		nav = append(nav, tbapi.NewInlineKeyboardButtonData("← Предыдущая", fmt.Sprintf("MP%d:%s", page-1, c.Ref)))
	}
	if page+1 < count {
		nav = append(nav, tbapi.NewInlineKeyboardButtonData("Следующая →", fmt.Sprintf("MP%d:%s", page+1, c.Ref)))
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	target := moderation.Action{Kind: "ban", ChatID: c.ChatID, IdentityID: c.IdentityID,
		IdentityKind: c.IdentityKind, MessageID: c.MessageID}
	if c.DeleteOnly {
		target.Kind = "original_delete"
	}
	allowed := !a.dry && !a.trainingMode && (a.recovery.gate == nil || a.recovery.gate(target))
	if a.recovery.superName != nil && a.recovery.superName(c.UserName, c.IdentityID) {
		allowed = false
	}
	for _, op := range c.Actions {
		if op.State == moderation.Blocked {
			allowed = false
		}
	}
	for _, row := range cardKeyboard(c, allowed).InlineKeyboard {
		filtered := []tbapi.InlineKeyboardButton{}
		for _, button := range row {
			if button.CallbackData != nil && !strings.HasPrefix(*button.CallbackData, "M?:") {
				filtered = append(filtered, button)
			}
		}
		if len(filtered) > 0 {
			rows = append(rows, filtered)
		}
	}
	return tbapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (a *admin) renderCardScreen(ctx context.Context, q *tbapi.CallbackQuery, c moderation.Card, page, technical int) error {
	pages := operatorDetailPages(c, time.Now())
	if page < 0 || page >= len(pages) {
		return fmt.Errorf("invalid details page")
	}
	text, techCount := pages[page], 0
	if technical >= 0 {
		tech := technicalDetailPages(c)
		techCount = len(tech)
		if technical >= techCount {
			return fmt.Errorf("invalid technical page")
		}
		text = tech[technical]
	}
	buttons := a.detailKeyboard(c, page, len(pages), technical, techCount)
	if c.DeleteOnly && q.From.ID != c.RequesterID {
		rows := [][]tbapi.InlineKeyboardButton{}
		for _, row := range buttons.InlineKeyboard {
			filtered := []tbapi.InlineKeyboardButton{}
			for _, button := range row {
				if button.CallbackData != nil && (strings.HasPrefix(*button.CallbackData, "MD:") ||
					strings.HasPrefix(*button.CallbackData, "MX:")) {
					continue
				}
				filtered = append(filtered, button)
			}
			if len(filtered) > 0 {
				rows = append(rows, filtered)
			}
		}
		buttons.InlineKeyboard = rows
	}
	msg := tbapi.NewEditMessageText(q.Message.Chat.ID, q.Message.MessageID, text)
	msg.ParseMode = tbapi.ModeHTML
	msg.LinkPreviewOptions = tbapi.LinkPreviewOptions{IsDisabled: true}
	msg.ReplyMarkup = &buttons
	if _, err := a.recovery.api.RequestWithContext(ctx, msg); err != nil {
		return fmt.Errorf("card screen delivery: %w", err)
	}
	return a.answerRecoveryCallback(ctx, q, "Экран обновлён")
}
