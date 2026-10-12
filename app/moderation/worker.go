package moderation

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"time"
)

// Worker has no private queue; restart discovery and scheduling come exclusively from Journal.
type Worker struct {
	Journal  *Journal
	Executor Executor
	Now      func() time.Time
	Jitter   func() float64
}

func (w *Worker) clock() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Step executes at most one owned action. API calls have an absolute context deadline.
func (w *Worker) Step(ctx context.Context) (bool, error) {
	now := w.clock()
	a, ok, err := w.Journal.Claim(ctx, now)
	if err != nil || !ok {
		return ok, err
	}
	var result Outcome
	if a.Code == "previously_confirmed_pair" && deleteKind(a.Kind) {
		result = Outcome{State: Confirmed, Code: a.Code}
	} else if a.Attempts > 320 {
		result = Outcome{State: Failed, Code: "attempt_budget_exhausted", Uncertain: a.Uncertain}
	} else if !now.Add(15 * time.Second).Before(time.UnixMilli(a.Deadline)) {
		result = Outcome{State: Expired, Code: "deadline_manual_action_required", Uncertain: a.Uncertain}
	} else {
		callCtx, cancel := context.WithDeadline(ctx, now.Add(15*time.Second))
		owned, e := w.Journal.Owned(callCtx, a, w.clock())
		if e != nil || !owned {
			cancel()
			if e != nil {
				return true, e
			}
			return true, ErrLease
		}
		result = w.Executor.Execute(callCtx, a)
		cancel()
	}
	now = w.clock()
	next := now
	if result.Retry {
		result.State = Pending
		jitter := rand.Float64() //nolint:gosec // Scheduling jitter is not a security token.
		if w.Jitter != nil {
			jitter = w.Jitter()
		}
		next = now.Add(Schedule(a.Attempts, jitter, result.RetryAfter))
		if a.Attempts >= 320 {
			result.State = Failed
			result.Code = "attempt_budget_exhausted"
			result.Retry = false
		} else if !next.Add(15 * time.Second).Before(time.UnixMilli(a.Deadline)) {
			result.State = Expired
			result.Code = "retry_after_or_deadline_manual_action_required"
			result.Retry = false
		}
	}
	if a.Kind == "notice" {
		if !result.Retry && result.State != Confirmed {
			result.State = Undelivered
		}
		// Bound all notification retries, including transport failures, to at most one per minute.
		if next.Before(now.Add(time.Minute)) {
			next = now.Add(time.Minute)
		}
	}
	if finishErr := w.Journal.Finish(ctx, a, result, next, now); finishErr != nil {
		return true, fmt.Errorf("finish moderation action: %w", finishErr)
	}
	log.Printf(
		"[INFO] durable moderation action: receipt=%s kind=%s chat_id=%d message_id=%d "+
			"identity_id=%d status=%s code=%s uncertain=%t attempts=%d",
		a.Receipt,
		a.Kind,
		a.ChatID,
		a.MessageID,
		a.IdentityID,
		result.State,
		result.Code,
		result.Uncertain,
		a.Attempts,
	)
	return true, nil
}

// Run uses one bounded worker per instance; empty polling is 0.2 queries/s.
func (w *Worker) Run(ctx context.Context) error {
	pruneAt := w.clock().Add(time.Hour)
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("moderation worker context: %w", ctx.Err())
		}
		worked, err := w.Step(ctx)
		if err != nil && !errors.Is(err, ErrLease) && ctx.Err() == nil {
			log.Printf("[WARN] moderation journal worker unavailable; persisted actions retained")
		}
		if w.clock().After(pruneAt) {
			if e := w.Journal.Prune(ctx, w.clock()); e != nil {
				log.Printf("[WARN] moderation journal retention failed")
			}
			pruneAt = w.clock().Add(time.Hour)
		}
		delay := 5 * time.Second
		if worked {
			delay = time.Second
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("moderation worker stopped: %w", ctx.Err())
		case <-time.After(delay):
		}
	}
}
