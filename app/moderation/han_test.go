package moderation

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHanDeleteRetryDeadlineAndRestart(t *testing.T) {
	for _, mode := range []string{"retry", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			j, db := testJournal(t, DefaultLimits())
			now := time.Now()
			p := plan(701, "original_delete")
			p.Learn, p.Card, p.Origin, p.Reason = false, true, "detector", "Преобладает письменность Han: 28 из 34 букв"
			p.SourceText = "original"
			p.Actions[0].MessageDate = now.Unix()
			receipt := accept(t, j, p, now)
			require.Equal(t, now.Add(24*time.Hour).UnixMilli(), find(t, j, "original_delete", 701).Deadline)
			var calls atomic.Int32
			executor := executeFunc(func(ctx context.Context, a Action) Outcome {
				if a.Kind != "original_delete" {
					return Outcome{State: Confirmed}
				}
				_, ok := ctx.Deadline()
				require.True(t, ok)
				if calls.Add(1) == 1 {
					delay := time.Second
					if mode == "deadline" {
						delay = 48 * time.Hour
					}
					return Outcome{Retry: true, RetryAfter: delay, Code: "telegram_429"}
				}
				return Outcome{State: Confirmed}
			})
			w := Worker{Journal: j, Executor: executor, Now: func() time.Time { return now }, Jitter: func() float64 { return 0 }}
			drive(t, &w)
			a := find(t, j, "original_delete", 701)
			require.EqualValues(t, 1, calls.Load())
			if mode == "deadline" {
				require.Equal(t, Expired, a.State)
				require.Equal(t, "retry_after_or_deadline_manual_action_required", a.Code)
				return
			}
			require.Equal(t, Pending, a.State)
			now = now.Add(2 * time.Second)
			restarted, err := NewJournal(testCtx, db, DefaultLimits())
			require.NoError(t, err)
			w.Journal = restarted
			drive(t, &w)
			require.Equal(t, Confirmed, find(t, j, "original_delete", 701).State)
			for range 3 {
				_, _, fresh, acceptErr := restarted.Accept(testCtx, p, now)
				require.NoError(t, acceptErr)
				require.False(t, fresh)
				drive(t, &w)
			}
			require.EqualValues(t, 2, calls.Load(), "confirmed delete is not replayed")
			card, err := restarted.CardView(testCtx, receipt)
			require.NoError(t, err)
			require.Equal(t, p.Reason, card.Reason)
			require.Equal(t, p.SourceText, card.Text)
		})
	}
}
