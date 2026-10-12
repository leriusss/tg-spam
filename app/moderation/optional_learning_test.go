package moderation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOptionalLearningFailureAndInterruptedPreparationKeepCore(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{false: "returned_error", true: "restart"}[interrupted], func(t *testing.T) {
			j, _ := testJournal(t, DefaultLimits())
			now := time.Now()
			p := plan(1001, "ban", "original_delete")
			p.Destination = 0
			p.LearningOptional = true
			id, owner, created, err := j.Accept(testCtx, p, now)
			require.NoError(t, err)
			require.True(t, created)
			if interrupted {
				now = now.Add(106 * time.Second)
			} else {
				require.NoError(t, j.Activate(testCtx, id, owner, now, false))
			}
			calls := 0
			w := Worker{
				Journal: j,
				Now:     func() time.Time { return now },
				Executor: executeFunc(
					func(_ context.Context, a Action) Outcome { calls++; return Outcome{State: Confirmed} },
				),
			}
			drive(t, &w)
			require.Equal(t, 2, calls)
			require.Equal(t, Manual, find(t, j, "learning_note", 0).State)
			require.Equal(t, Confirmed, find(t, j, "ban", 1001).State)
			require.Equal(t, Confirmed, find(t, j, "original_delete", 1001).State)
		})
	}
}

func TestExpiredUnknownActionKeepsUncertaintyAndNoAPICall(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	p := plan(1101, "ban")
	p.Destination = 0
	p.Learn = false
	_, _, _, err := j.Accept(testCtx, p, now)
	require.NoError(t, err)
	_, ok, err := j.Claim(testCtx, now)
	require.NoError(t, err)
	require.True(t, ok)
	now = now.Add(25 * time.Hour)
	calls := 0
	w := Worker{
		Journal:  j,
		Now:      func() time.Time { return now },
		Executor: executeFunc(func(context.Context, Action) Outcome { calls++; return Outcome{State: Confirmed} }),
	}
	drive(t, &w)
	a := find(t, j, "ban", 1101)
	require.Equal(t, Expired, a.State)
	require.True(t, a.Uncertain)
	require.Zero(t, calls)
}

func TestAttemptCeilingAndNoticePermanentFailureDoNotReplay(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	p := plan(1201, "ban")
	p.Learn = false
	_, _, _, err := j.Accept(testCtx, p, now)
	require.NoError(t, err)
	_, err = j.db.Exec("UPDATE moderation_actions SET attempts=319 WHERE kind='ban'")
	require.NoError(t, err)
	calls := 0
	noticeCalls := 0
	w := Worker{
		Journal: j,
		Now:     func() time.Time { return now },
		Executor: executeFunc(func(_ context.Context, a Action) Outcome {
			if a.Kind == "notice" {
				noticeCalls++
				return Outcome{State: Failed, Code: "telegram_403"}
			}
			calls++
			return Outcome{Retry: true, Uncertain: true, Code: "transport"}
		}),
	}
	drive(t, &w)
	require.Equal(t, 320, find(t, j, "ban", 1201).Attempts)
	require.Equal(t, Failed, find(t, j, "ban", 1201).State)
	now = now.Add(time.Minute)
	drive(t, &w)
	require.Equal(t, 1, calls)
	require.Equal(t, 1, noticeCalls)
	accept(t, j, plan(1202, "ban"), now)
	drive(t, &w)
	require.Equal(t, 1, noticeCalls, "new generation is not grounds to retry a permanent 403")
}
