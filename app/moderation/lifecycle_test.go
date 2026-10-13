package moderation

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func lifecyclePlan(msg int, origin string, now time.Time) Plan {
	p := plan(msg, "ban", "original_delete")
	p.Learn = false
	p.Origin = origin
	p.Card = true
	p.SourceText = "original text, never notification statuses"
	p.UserName = "subject"
	p.Actions[0].MessageID = 0
	p.Actions[0].UntilDate = now.Add(time.Hour).Unix()
	p.Actions[1].MessageDate = now.Unix()
	return p
}

func TestLifecycleRejectPreservesDetectorAndRestart(t *testing.T) {
	j, db := testJournal(t, DefaultLimits())
	now := time.Now()
	p := lifecyclePlan(2001, "report", now)
	accept(t, j, p, now)
	p.Origin = "detector"
	_, _, fresh, err := j.Accept(testCtx, p, now)
	require.NoError(t, err)
	require.False(t, fresh)
	require.NoError(t, j.Withdraw(testCtx, p.Key, "report", now))
	require.Equal(t, Pending, find(t, j, "ban", 0).State)
	c, err := j.CardView(testCtx, p.Key)
	require.NoError(t, err)
	for _, d := range c.Origins {
		if d.Origin == "report" {
			require.False(t, d.Active)
		} else {
			require.True(t, d.Active)
		}
	}
	restarted, err := NewJournal(testCtx, db, DefaultLimits())
	require.NoError(t, err)
	c, err = restarted.CardView(testCtx, p.Key)
	require.NoError(t, err)
	calls := 0
	w := Worker{
		Journal: restarted,
		Now:     func() time.Time { return now },
		Executor: executeFunc(func(_ context.Context, a Action) Outcome {
			if a.Kind == "ban" || deleteKind(a.Kind) {
				calls++
			}
			return Outcome{State: Confirmed, NotificationID: 900}
		}),
	}
	drive(t, &w)
	require.Equal(t, 2, calls)
	// Report-only cancellation persists, and the same threshold demand cannot revive it.
	p = lifecyclePlan(2002, "report", now)
	accept(t, j, p, now)
	require.NoError(t, j.Withdraw(testCtx, p.Key, "report", now))
	_, _, _, err = restarted.Accept(testCtx, p, now)
	require.NoError(t, err)
	require.Equal(t, Canceled, find(t, j, "original_delete", 2002).State)
	// A new independent detector decision is not the rejected report.
	p.Origin = "detector"
	_, _, _, err = restarted.Accept(testCtx, p, now)
	require.NoError(t, err)
	require.Equal(t, Pending, find(t, j, "original_delete", 2002).State)
}

func TestLifecycleMergeUpgradeCleanupAndDelivery(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	p := lifecyclePlan(2101, "detector", now)
	accept(t, j, p, now)
	w := Worker{
		Journal: j,
		Now:     func() time.Time { return now },
		Executor: executeFunc(
			func(context.Context, Action) Outcome { return Outcome{State: Confirmed, NotificationID: 901} },
		),
	}
	drive(t, &w)
	p.Origin = "forward:1:99"
	p.DeliveryChat = 1
	p.DeliveryMessage = 99
	p.Actions[0].UntilDate = now.Add(9999 * time.Hour).Unix()
	p.Actions = append(
		p.Actions,
		Action{
			Kind:         "cleanup_delete",
			ChatID:       p.ChatID,
			IdentityID:   p.IdentityID,
			IdentityKind: p.IdentityKind,
			MessageID:    2102,
			MessageDate:  now.Unix(),
		},
	)
	_, _, fresh, err := j.Accept(testCtx, p, now)
	require.NoError(t, err)
	require.False(t, fresh)
	c, err := j.CardView(testCtx, p.Key)
	require.NoError(t, err)
	bans := 0
	for _, a := range c.Actions {
		if a.Kind == "ban" {
			bans++
			if permanent(a.UntilDate, now) {
				require.Equal(t, Pending, a.State)
			} else {
				require.Equal(t, Confirmed, a.State)
			}
		}
	}
	require.Equal(t, 2, bans)
	require.Equal(t, Confirmed, find(t, j, "original_delete", 2101).State)
	require.Equal(t, Pending, find(t, j, "cleanup_delete", 2102).State)
	p.Actions = append(
		p.Actions,
		Action{
			Kind:         "cleanup_delete",
			ChatID:       p.ChatID,
			IdentityID:   p.IdentityID,
			IdentityKind: p.IdentityKind,
			MessageID:    2103,
		},
	)
	_, _, _, err = j.Accept(testCtx, p, now)
	require.NoError(t, err)
	rows, err := j.Snapshot(testCtx)
	require.NoError(t, err)
	for _, a := range rows {
		require.NotEqual(t, 2103, a.MessageID, "same admin delivery does not manufacture new requirements")
	}
	p.DeliveryMessage = 100
	p.Origin = "forward:1:100"
	_, _, _, err = j.Accept(testCtx, p, now)
	require.NoError(t, err)
	require.Equal(t, Pending, find(t, j, "cleanup_delete", 2103).State)
	p2 := lifecyclePlan(2201, "detector", now)
	p2.SourceText = p.SourceText
	accept(t, j, p2, now)
	require.Equal(t, Pending, find(t, j, "original_delete", 2201).State)
}

func TestLifecycleRunningWithdrawalDoesNotPromiseRecall(t *testing.T) {
	for _, result := range []Outcome{{State: Confirmed}, {Retry: true, Uncertain: true, Code: "transport"}} {
		t.Run(result.Code+result.State, func(t *testing.T) {
			j, _ := testJournal(t, DefaultLimits())
			now := time.Now()
			p := lifecyclePlan(2301, "report", now)
			p.Card = false
			p.Destination = 0
			accept(t, j, p, now)
			a, ok, err := j.Claim(testCtx, now)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, "ban", a.Kind)
			require.NoError(t, j.Withdraw(testCtx, p.Key, "report", now))
			require.Equal(t, Running, find(t, j, "ban", 0).State)
			require.NoError(t, j.Finish(testCtx, a, result, now, now))
			a = find(t, j, "ban", 0)
			if result.State == Confirmed {
				require.Equal(t, Confirmed, a.State)
			} else {
				require.Equal(t, Manual, a.State)
				require.True(t, a.Uncertain)
			}
			require.Equal(t, Canceled, find(t, j, "original_delete", 2301).State)
		})
	}
}

func TestLifecycleStopSerializesUnbanAndSurvivesRestart(t *testing.T) {
	j, db := testJournal(t, DefaultLimits())
	now := time.Now()
	p := lifecyclePlan(2401, "detector", now)
	accept(t, j, p, now)
	// Drain the coalesced notice so the next claim is the ban.
	a, ok, err := j.Claim(testCtx, now)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "notice", a.Kind)
	require.NoError(t, j.Finish(testCtx, a, Outcome{State: Confirmed}, now, now))
	ban, ok, err := j.Claim(testCtx, now)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "ban", ban.Kind)
	c, err := j.CardView(testCtx, p.Key)
	require.NoError(t, err)
	fresh, err := j.Decide(testCtx, "unban-request", c, "unban", 11, now)
	require.NoError(t, err)
	require.True(t, fresh)
	for range 4 {
		a, ok, err = j.Claim(testCtx, now)
		require.NoError(t, err)
		if !ok {
			break
		}
		require.Equal(t, "card", a.Kind, "unban cannot overtake the running ban")
		require.NoError(t, j.Finish(testCtx, a, Outcome{State: Confirmed}, now, now))
	}
	require.NoError(t, j.Finish(testCtx, ban, Outcome{State: Confirmed}, now, now))
	restarted, err := NewJournal(testCtx, db, DefaultLimits())
	require.NoError(t, err)
	c, err = restarted.CardView(testCtx, p.Key)
	require.NoError(t, err)
	require.True(t, c.Held)
	fresh, err = restarted.Decide(testCtx, "unban-request", c, "unban", 11, now)
	require.NoError(t, err)
	require.False(t, fresh)
	p2 := lifecyclePlan(2402, "detector", now)
	accept(t, restarted, p2, now)
	require.Equal(t, Canceled, find(t, restarted, "original_delete", 2402).State)
}

func TestLifecyclePruningRetainsUnresolvedWorkUntilExplicitClose(t *testing.T) {
	j, _ := testJournal(t, Limits{Receipts: 500, Actions: 5000, RetainedReceipts: 1})
	now := time.Now()
	p := lifecyclePlan(2501, "detector", now)
	p.Actions[1].State = Manual
	p.Actions[1].Code = "original_unlocated"
	accept(t, j, p, now)
	w := Worker{
		Journal: j,
		Now:     func() time.Time { return now },
		Executor: executeFunc(func(_ context.Context, a Action) Outcome {
			if a.Kind == "ban" {
				return Outcome{State: Failed, Code: "telegram_403"}
			}
			return Outcome{State: Confirmed, NotificationID: 902}
		}),
	}
	drive(t, &w)
	now = now.Add(31 * 24 * time.Hour)
	require.NoError(t, j.Prune(testCtx, now))
	c, err := j.CardView(testCtx, p.Key)
	require.NoError(t, err)
	require.NotEmpty(t, c.Ref)
	require.Equal(t, Failed, find(t, j, "ban", 0).State)
	_, _, _, capacityErr := j.Accept(testCtx, lifecyclePlan(2502, "detector", now), now)
	require.ErrorIs(t, capacityErr, ErrCapacity)
	require.NoError(t, j.Close(testCtx, p.Key, 11, now))
	c, err = j.CardView(testCtx, p.Key)
	require.NoError(t, err)
	require.Len(t, c.Resolved, 2)
	// Even explicit closure does not drop an undelivered final card.
	require.NoError(t, j.Prune(testCtx, now))
	_, err = j.CardView(testCtx, p.Key)
	require.NoError(t, err)
	drive(t, &w)
	require.NoError(t, j.Prune(testCtx, now))
	_, err = j.CardView(testCtx, p.Key)
	require.Error(t, err)
}

func TestLifecycleRestartAfterWithdrawalNeverReplaysInFlightBan(t *testing.T) {
	j, db := testJournal(t, DefaultLimits())
	now := time.Now()
	p := lifecyclePlan(2601, "report", now)
	p.Destination = 0
	accept(t, j, p, now)
	a, ok, err := j.Claim(testCtx, now)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "ban", a.Kind)
	c, err := j.CardView(testCtx, p.Key)
	require.NoError(t, err)
	require.NoError(t, j.StopTarget(testCtx, c, "unban", now))
	// Process dies before recording the request's outcome; old lease must expire first.
	restarted, err := NewJournal(testCtx, db, DefaultLimits())
	require.NoError(t, err)
	now = now.Add(106 * time.Second)
	bans := 0
	undos := 0
	w := Worker{Journal: restarted, Now: func() time.Time { return now }, Executor: executeFunc(func(_ context.Context, a Action) Outcome {
		if a.Kind == "ban" {
			bans++
		}
		if a.Kind == "unban" {
			undos++
		}
		return Outcome{State: Confirmed, NotificationID: 904}
	})}
	drive(t, &w)
	require.Zero(t, bans)
	require.Equal(t, 1, undos)
	require.Equal(t, Manual, find(t, restarted, "ban", 0).State)
	require.True(t, find(t, restarted, "ban", 0).Uncertain)
}

func TestLifecycleCancellationDoesNotDisableFutureIndependentDetection(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	p := lifecyclePlan(2701, "detector", now)
	accept(t, j, p, now)
	c, err := j.CardView(testCtx, p.Key)
	require.NoError(t, err)
	require.NoError(t, j.StopTarget(testCtx, c, "", now))
	now = now.Add(time.Second)
	// Same detector decision / original remains withdrawn, including newly proposed cleanup IDs.
	p.Actions = append(p.Actions, Action{Kind: "cleanup_delete", ChatID: p.ChatID, IdentityID: p.IdentityID, IdentityKind: p.IdentityKind, MessageID: 2702})
	_, _, _, err = j.Accept(testCtx, p, now)
	require.NoError(t, err)
	require.Equal(t, Canceled, find(t, j, "cleanup_delete", 2702).State)
	// A genuinely new original is governed by the unchanged detector policy, not a permanent global stop.
	fresh := lifecyclePlan(2703, "detector", now)
	accept(t, j, fresh, now)
	require.Equal(t, Pending, find(t, j, "original_delete", 2703).State)
}

func TestLifecycleUnknownPrototypeAuthorityRequiresManualReview(t *testing.T) {
	j, db := testJournal(t, DefaultLimits())
	now := time.Now()
	p := plan(2801, "ban")
	p.Learn = false
	p.Destination = 0
	accept(t, j, p, now)
	_, err := db.Exec("DELETE FROM moderation_demands")
	require.NoError(t, err)
	restarted, err := NewJournal(testCtx, db, DefaultLimits())
	require.NoError(t, err)
	a := find(t, restarted, "ban", 2801)
	require.Equal(t, Manual, a.State)
	require.Equal(t, "legacy_provenance_unknown", a.Code)
	_, ok, err := restarted.Claim(testCtx, now)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestLifecycleNewRequirementWaitsForUnfinishedUndo(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	p := lifecyclePlan(2901, "report", now)
	accept(t, j, p, now)
	c, err := j.CardView(testCtx, p.Key)
	require.NoError(t, err)
	fresh, err := j.Decide(testCtx, "undo", c, "unban", 11, now)
	require.NoError(t, err)
	require.True(t, fresh)
	require.Error(t, j.Close(testCtx, p.Key, 11, now), "pending undo cannot be declared finished")
	now = now.Add(time.Second)
	p.Origin = "forward:1:299"
	p.DeliveryChat = 1
	p.DeliveryMessage = 299
	p.Actions[0].UntilDate = now.Add(9999 * time.Hour).Unix()
	_, _, _, err = j.Accept(testCtx, p, now)
	require.NoError(t, err)
	c, err = j.CardView(testCtx, p.Key)
	require.NoError(t, err)
	for _, a := range c.Actions {
		if a.Kind == "ban" && permanent(a.UntilDate, now) {
			mode, e := j.Authorize(testCtx, a)
			require.NoError(t, e)
			require.Equal(t, "waiting_for_undo", mode)
		}
	}
}

func TestLifecycleNewReportBatchAfterRejectedVotesIsNewAuthority(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	p := lifecyclePlan(3001, "report:10", now)
	accept(t, j, p, now)
	require.NoError(t, j.Withdraw(testCtx, p.Key, "report", now))
	require.Equal(t, Canceled, find(t, j, "original_delete", 3001).State)
	_, _, _, err := j.Accept(testCtx, p, now)
	require.NoError(t, err)
	require.Equal(t, Canceled, find(t, j, "original_delete", 3001).State)
	p.Origin = "report:11"
	now = now.Add(time.Second)
	_, _, _, err = j.Accept(testCtx, p, now)
	require.NoError(t, err)
	require.Equal(t, Pending, find(t, j, "original_delete", 3001).State)
}
