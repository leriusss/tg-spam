package tgspam

import (
	"fmt"
	"slices"
	"time"

	"github.com/umputun/tg-spam/lib/approved"
	"github.com/umputun/tg-spam/lib/spamcheck"
)

const autoProgressReplayWindow = 128

// approvedForRequest requires the caller to hold the detector read lock.
func (d *Detector) approvedForRequest(req spamcheck.Request) bool {
	u := d.approvedUsers[req.UserID]
	if u.Source == approved.SourceManual {
		return true
	}
	count := u.Count
	if req.Meta.ChatID != 0 {
		count = u.Progress[req.Meta.ChatID].Count
	}
	return count >= d.FirstMessagesCount
}

// creditAutoProgress publishes memory only after a successful storage write.
func (d *Detector) creditAutoProgress(req spamcheck.Request) error {
	d.trustLock.Lock()
	defer d.trustLock.Unlock()
	d.lock.RLock()
	u := d.approvedUsers[req.UserID].Clone()
	storage := d.userStorage
	d.lock.RUnlock()
	if u.Source == approved.SourceManual {
		return nil
	}
	chat, message := req.Meta.ChatID, req.Meta.MessageID
	if (chat == 0) != (message == 0) || message < 0 {
		return fmt.Errorf("incomplete source message identity")
	}
	if u.Progress == nil {
		u.Progress = make(map[int64]approved.CheckProgress)
	}
	p := u.Progress[chat]
	if chat != 0 && slices.Contains(p.Messages, message) {
		return nil
	}
	if chat == 0 {
		u.Count++
	} else {
		p.Count++
		p.Messages = append(p.Messages, message)
		if len(p.Messages) > autoProgressReplayWindow {
			p.Messages = slices.Clone(p.Messages[1:])
		}
		u.Progress[chat] = p
	}
	u.UserID, u.UserName, u.Timestamp = req.UserID, req.UserName, time.Now()
	if u.Source == "" {
		u.Source = approved.SourceAuto
	}
	if storage != nil {
		ctx, cancel := d.ctxWithStoreTimeout()
		defer cancel()
		if err := storage.Write(ctx, u); err != nil {
			return fmt.Errorf("write checked progress: %w", err)
		}
	}
	d.lock.Lock()
	d.approvedUsers[req.UserID] = u
	d.lock.Unlock()
	return nil
}
