package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/umputun/tg-spam/app/moderation"
)

// SharedForwardConfig connects to coordination metadata only. Opening it runs
// asynchronously: ordinary automatic moderation does not depend on this DB.
type SharedForwardConfig struct {
	Policy      string
	Peer, Owner int64
	Peers       []int64
	Open        func(context.Context) (*moderation.SharedStore, func(), error)
}

type sharedForward struct {
	now    func() time.Time
	config SharedForwardConfig
	admin  *admin
	mu     sync.RWMutex
	store  *moderation.SharedStore
	cache  map[string]string // owned only by the single background loop
}

func (s *sharedForward) clockNow() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Callback data is untrusted. Legacy dispatch must never reinterpret this message.
func (s *sharedForward) rejectForeignCallback(ctx context.Context, q *tbapi.CallbackQuery) (bool, error) {
	if q == nil || q.Message == nil || strings.HasPrefix(q.Data, "S:") || s.config.Peer != s.config.Owner ||
		q.Message.Chat.ID != s.admin.adminChatID {
		return false, nil
	}
	legacyMarkup := false
	if q.Message.ReplyMarkup != nil {
		for _, row := range q.Message.ReplyMarkup.InlineKeyboard {
			for _, button := range row {
				if button.CallbackData != nil && strings.HasPrefix(*button.CallbackData, "S:") {
					return true, s.admin.answerRecoveryCallback(ctx, q, "Изменённые кнопки этой карточки не дают полномочий.")
				}
				legacyMarkup = true
			}
		}
	}
	store := s.current()
	blocked := !legacyMarkup
	if store != nil {
		known, e := store.ForeignNamespaceBlocked(ctx, q.Message.Chat.ID, q.Message.MessageID)
		if e == nil {
			blocked = known || !legacyMarkup
		} else if !legacyMarkup {
			blocked = true
		}
	}
	if blocked {
		return true, s.admin.answerRecoveryCallback(ctx, q, "Для общей карточки доступны только её актуальные кнопки.")
	}
	return false, nil
}

func deliveryForShared(u tbapi.Update) moderation.ForwardDelivery {
	return moderation.ForwardDelivery{UpdateID: u.UpdateID, ChatID: u.Message.Chat.ID, MessageID: u.Message.MessageID}
}

func (s *sharedForward) captureFailure(delivery moderation.ForwardDelivery, operator int64, reason string, sentAt ...int) error {
	r := moderation.SharedReport{Policy: s.config.Policy, Peer: s.config.Peer, ChatID: s.admin.primChatID, Operator: operator,
		Name: truncateRunes(s.admin.recovery.chatName, 80), Delivery: delivery, SearchError: truncateRunes(reason, 500)}
	if len(sentAt) == 1 {
		r.RequestedAt = int64(sentAt[0]) * 1000
	}
	if err := s.admin.recovery.journal.AcceptSharedForward(context.Background(), r, time.Now()); err != nil {
		if s.config.Peer != s.config.Owner {
			log.Printf("[WARN] shared search refusal could not be journaled")
			return nil
		}
		return fmt.Errorf("save shared search refusal: %w", err)
	}
	return nil
}

func (s *sharedForward) current() *moderation.SharedStore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.store
}

func (s *sharedForward) capture(delivery moderation.ForwardDelivery, operator int64,
	targets []moderation.ForwardTarget, sentAt ...int) error {
	r := moderation.SharedReport{Policy: s.config.Policy, Peer: s.config.Peer, ChatID: s.admin.primChatID, Operator: operator,
		Name: truncateRunes(s.admin.recovery.chatName, 80), Delivery: delivery, Targets: targets}
	if len(sentAt) == 1 {
		r.RequestedAt = int64(sentAt[0]) * 1000
	}
	if err := s.admin.recovery.journal.AcceptSharedForward(context.Background(), r, time.Now()); err != nil {
		if s.config.Peer != s.config.Owner {
			log.Printf("[WARN] shared forward could not be journaled")
			return nil
		}
		return fmt.Errorf("save shared forward: %w", err)
	}
	return nil
}

func (s *sharedForward) run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var closeDB func()
	defer func() {
		if closeDB != nil {
			closeDB()
		}
	}()
	for {
		if s.current() == nil {
			openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			store, closer, err := s.config.Open(openCtx)
			cancel()
			if err == nil {
				s.mu.Lock()
				s.store = store
				s.mu.Unlock()
				closeDB = closer
			}
		}
		if s.current() != nil {
			if err := s.step(ctx, time.Now()); err != nil && ctx.Err() == nil {
				log.Printf("[WARN] shared forward handoff unavailable; local automatic moderation continues")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *sharedForward) step(ctx context.Context, now time.Time) error {
	store := s.current()
	if store == nil {
		return errors.New("shared store unavailable")
	}
	reports, err := s.admin.recovery.journal.SharedOutbox(ctx)
	if err != nil {
		return fmt.Errorf("read shared outbox: %w", err)
	}
	var submitFailure error
	for _, r := range reports {
		raw, e := json.Marshal(r)
		if e != nil {
			return fmt.Errorf("encode shared outbox: %w", e)
		}
		key := fmt.Sprintf("%d:%d", r.Delivery.ChatID, r.Delivery.MessageID)
		if s.cache[key] == string(raw) {
			continue
		}
		if e := store.Submit(ctx, r, now); e != nil {
			submitFailure = fmt.Errorf("submit shared report: %w", e)
			if errors.Is(e, moderation.ErrCapacity) {
				continue
			}
			break // existing decisions must still reach local journals if new intake is full
		}
		s.cache[key] = string(raw)
	}
	if s.config.Peer == s.config.Owner {
		if e := store.Reconcile(ctx, s.config.Peer, now); e != nil {
			return fmt.Errorf("reconcile shared inquiries: %w", e)
		}
	}
	cases, err := store.Cases(ctx)
	if err != nil {
		return fmt.Errorf("read shared cases: %w", err)
	}
	for _, c := range cases {
		if c.AdminID != s.admin.adminChatID {
			continue
		}
		for _, choice := range c.Choices {
			if choice.Refusal != "" || choice.Report.Peer != s.config.Peer || choice.Report.ChatID != s.admin.primChatID {
				continue
			}
			key := "decision:" + c.Ref + ":" + choice.Report.Receipt
			if s.cache[key] == "done" {
				continue
			}
			// The frozen local receipt additionally binds requester, delivery and expiry.
			e := s.admin.recovery.journal.DecideSharedForward(ctx, choice.Report, c.DecisionOperator,
				choice.Selected, c.Automatic, now)
			if errors.Is(e, moderation.ErrSharedReview) {
				if reviewErr := store.RequireReview(ctx, c.Ref, s.config.Peer, e.Error()); reviewErr != nil {
					return fmt.Errorf("record fresh review: %w", reviewErr)
				}
			} else if e != nil {
				log.Printf("[WARN] shared decision not admitted to local journal; expired or unavailable authority")
			} else {
				s.cache[key] = "done"
			}
		}
		if s.config.Peer != s.config.Owner {
			continue
		}
		legacyChoice := c.Policy == "" && !c.Decided && len(c.Reports) == len(s.config.Peers)
		if c.Canonical == "" && (c.HasPendingInstruction(now) || legacyChoice) {
			if c.Policy == moderation.ForwardAllPolicy {
				if code := s.admin.forwardOperator(ctx, &tbapi.User{ID: c.Operator}); code != "" {
					continue // workers independently recheck before every effect
				}
			}
			if e := store.AutoChoose(ctx, s.config.Peer, c.Ref, now); e != nil {
				return fmt.Errorf("admit unique shared original: %w", e)
			}
		}
	}
	if s.config.Peer == s.config.Owner {
		peerNames := map[int64]string{}
		for _, previous := range cases {
			for _, report := range previous.Reports {
				peerNames[report.Peer] = report.Name
			}
		}
		for _, c := range cases {
			if c.AdminID != s.admin.adminChatID {
				continue
			}
			view, found, e := store.View(ctx, s.config.Peer, c.Ref, now)
			if e != nil {
				return fmt.Errorf("load shared presentation: %w", e)
			}
			if found {
				view.PeerNames = peerNames
				if e := s.render(ctx, store, view, now); e != nil {
					return e
				}
			}
		}
	}
	return submitFailure
}

func (s *sharedForward) render(ctx context.Context, store *moderation.SharedStore,
	view moderation.SharedView, now time.Time) error {
	c := view.Case
	if c.Canonical != "" && c.NotificationID == 0 {
		return nil
	}
	text, buttons := sharedViewCard(view, s.config.Peers, now)
	raw, err := json.Marshal(buttons)
	if err != nil {
		return fmt.Errorf("encode shared keyboard: %w", err)
	}
	fingerprint := text + string(raw)
	if c.LastRender == fingerprint {
		return nil
	}
	claimed, ok, err := store.ClaimCard(ctx, s.config.Peer, c.Ref, c.Revision, fingerprint, now)
	if err != nil {
		return fmt.Errorf("claim shared card: %w", err)
	}
	if !ok {
		return nil
	}
	// Never execute an automatic single-target decision without a current role.
	// Every worker checks the role again immediately before ban/delete as well.
	var req tbapi.Chattable
	if claimed.NotificationID > 0 {
		edit := tbapi.NewEditMessageText(c.AdminID, claimed.NotificationID, text)
		edit.ParseMode = tbapi.ModeHTML
		edit.ReplyMarkup = &buttons
		req = edit
	} else {
		msg := tbapi.NewMessage(c.AdminID, text)
		msg.ParseMode = tbapi.ModeHTML
		msg.ReplyMarkup = buttons
		if len(c.Reports) > 0 {
			msg.ReplyParameters = tbapi.ReplyParameters{MessageID: c.Reports[0].Delivery.MessageID}
		}
		req = msg
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	resp, callErr := s.admin.recovery.api.RequestWithContext(callCtx, req)
	cancel()
	out := telegramOutcome(resp, callErr)
	anchor := claimed.NotificationID
	result := "failed"
	var apiErr *tbapi.Error
	if claimed.NotificationID > 0 && errors.As(callErr, &apiErr) && apiErr.Code == 400 &&
		strings.Contains(strings.ToLower(apiErr.Message), "message is not modified") {
		out = moderation.Outcome{State: moderation.Confirmed}
	}
	if out.State == moderation.Confirmed {
		var msg tbapi.Message
		if claimed.NotificationID > 0 {
			result = "ok"
		} else if resp != nil && json.Unmarshal(resp.Result, &msg) == nil && msg.MessageID > 0 && msg.Chat.ID == c.AdminID {
			anchor = msg.MessageID
			result = "ok"
		} else {
			result = "unknown"
		}
	} else if claimed.NotificationID == 0 && out.Uncertain {
		result = "unknown"
	} else if out.Retry {
		result = "retry"
	}
	// Finish with a fresh context even if the outbound request timed out.
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishCancel()
	if err := store.FinishCard(finishCtx, claimed, anchor, fingerprint, result, now); err != nil {
		return fmt.Errorf("finish shared card: %w", err)
	}
	return nil
}

func (s *sharedForward) callback(ctx context.Context, q *tbapi.CallbackQuery) error {
	if q == nil || q.From == nil || q.Message == nil || q.Message.From == nil ||
		q.Message.From.ID != s.config.Peer || s.config.Peer != s.config.Owner || q.Message.Chat.ID != s.admin.adminChatID {
		return errors.New("shared callback owner mismatch")
	}
	if len(q.Data) > 64 {
		return s.admin.answerRecoveryCallback(ctx, q, "Некорректные данные кнопки; действий нет.")
	}
	if code := s.admin.forwardOperator(ctx, q.From); code != "" {
		return s.admin.answerRecoveryCallback(ctx, q, forwardOperatorFeedback(code))
	}
	parts := strings.Split(q.Data, ":")
	if len(parts) != 4 || parts[0] != "S" {
		return errors.New("invalid shared callback")
	}
	revision, err := strconv.Atoi(parts[3])
	if err != nil {
		return fmt.Errorf("parse shared revision: %w", err)
	}
	store := s.current()
	if store == nil {
		return s.admin.answerRecoveryCallback(ctx, q, "Координатор недоступен. Выбор не принят; повторите позже.")
	}
	verb := parts[1]
	if strings.HasPrefix(verb, "c") || strings.HasPrefix(verb, "p") {
		return s.admin.answerRecoveryCallback(ctx, q, "У каждого обращения своя карточка. Эти старые кнопки не дают полномочий.")
	} else if page, ok := strings.CutPrefix(verb, "v"); ok {
		n, e := strconv.Atoi(page)
		if e != nil {
			return fmt.Errorf("parse shared page: %w", e)
		}
		err = store.Navigate(ctx, s.config.Peer, parts[2], revision, q.Message.Chat.ID, q.Message.MessageID, n)
	} else {
		if s.admin.dry || s.admin.trainingMode || s.admin.forwardingDisabled {
			return s.admin.answerRecoveryCallback(ctx, q, "Текущие настройки запрещают модерацию")
		}
		peer, index := int64(0), -1
		if verb != "all" {
			selection := strings.Split(verb, ".")
			if len(selection) > 2 {
				return errors.New("invalid shared selection")
			}
			peer, err = strconv.ParseInt(selection[0], 10, 64)
			if err != nil || peer <= 0 {
				return errors.New("invalid shared peer")
			}
			if len(selection) == 2 {
				index, err = strconv.Atoi(selection[1])
				if err != nil || index < 0 {
					return errors.New("invalid shared original")
				}
			}
		}
		err = store.Decide(ctx, s.config.Peer, parts[2], revision,
			q.Message.Chat.ID, q.Message.MessageID, q.From.ID, peer, index, s.clockNow())
	}
	if err != nil {
		return s.admin.answerRecoveryCallback(ctx, q,
			"Не принято: карточка, набор целей или срок изменились. Используйте актуальные кнопки.")
	}
	return s.admin.answerRecoveryCallback(ctx, q, "Принято. Результаты появятся в этой карточке.")
}
