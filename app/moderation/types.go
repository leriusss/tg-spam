// Package moderation implements one durable authority for moderation and its notifications.
package moderation

import (
	"context"
	"errors"
	"time"
)

// Persistent admission and ownership errors.
var (
	ErrCapacity = errors.New("moderation journal capacity reached; no API actions accepted")
	ErrLease    = errors.New("moderation claim ownership lost")
)

// Action states separate pending/unknown effects from acknowledged or terminal outcomes.
const (
	Pending     = "pending"
	Running     = "running"
	Confirmed   = "confirmed"
	Absent      = "absent"
	Failed      = "failed"
	Expired     = "expired"
	Manual      = "manual_review"
	Blocked     = "blocked"
	Canceled    = "canceled"
	Undelivered = "notification_undelivered"
)

// Limits bound unfinished work, not an in-memory queue competing with the journal.
type Limits struct{ Receipts, Actions, RetainedReceipts int }

// DefaultLimits bounds each instance independently.
func DefaultLimits() Limits { return Limits{Receipts: 500, Actions: 5000, RetainedReceipts: 5000} }

// Plan contains validated authority and original card context; never Telegram credentials.
type Plan struct {
	CommandID                         string
	Origin                            string
	Card                              bool
	SourceText, UserName, DisplayName string
	Key                               string
	ChatID, IdentityID, Destination   int64
	MessageID                         int
	IdentityKind                      string
	DeliveryChat                      int64
	DeliveryMessage                   int
	Learn                             bool
	LearningOptional                  bool
	Actions                           []Action
}

// Action is an independent API operation; times are Unix milliseconds except MessageDate/UntilDate.
type Action struct {
	ID             string `db:"id"`
	Receipt        string `db:"receipt"`
	Kind           string `db:"kind"`
	ChatID         int64  `db:"chat_id"`
	IdentityID     int64  `db:"identity_id"`
	IdentityKind   string `db:"identity_kind"`
	MessageID      int    `db:"message_id"`
	MessageDate    int64  `db:"message_date"`
	UntilDate      int64  `db:"until_date"`
	State          string `db:"state"`
	Code           string `db:"code"`
	Attempts       int    `db:"attempts"`
	Deadline       int64  `db:"deadline"`
	NextAt         int64  `db:"next_at"`
	Owner          string `db:"owner"`
	LeaseUntil     int64  `db:"lease_until"`
	Uncertain      bool   `db:"uncertain"`
	Generation     int64  `db:"generation"`
	Delivered      int64  `db:"delivered"`
	NotificationID int    `db:"notification_id"`
}

// Outcome stores a safe classification, not arbitrary error strings or token-bearing URLs.
type Outcome struct {
	State, Code    string
	Retry          bool
	Uncertain      bool
	RetryAfter     time.Duration
	NotificationID int
}

// Executor performs one bounded operation under current safety gates.
type Executor interface {
	Execute(context.Context, Action) Outcome
}

// Schedule is also used by virtual-clock tests; retry_after is never capped or shortened.
func Schedule(attempt int, jitter float64, retryAfter time.Duration) time.Duration {
	delay := 2 * time.Second
	for i := 1; i < attempt && delay < 10*time.Minute; i++ {
		delay *= 2
	}
	if delay > 10*time.Minute {
		delay = 10 * time.Minute
	}
	if jitter < 0 {
		jitter = 0
	}
	if jitter > 1 {
		jitter = 1
	}
	delay = max(retryAfter, time.Duration(float64(delay)*(0.5+0.5*jitter)))
	return delay
}

func deleteKind(kind string) bool { return kind == "original_delete" || kind == "cleanup_delete" }

func final(state string) bool { return state != Pending && state != Running }
