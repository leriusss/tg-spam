package moderation

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/storage/engine"
)

func checkDeleteRenewalContract(t *testing.T, j *Journal, db *engine.SQL) {
	t.Helper()
	ctx := context.Background()
	for i, mode := range []string{"expired", "dismissed", "second operator"} {
		t.Run(mode, func(t *testing.T) {
			start := time.Now()
			p := deletePlan(92001 + i)
			accept(t, j, p, start)
			notification := 700 + i
			require.NoError(t, j.BindCard(ctx, p.Key, notification))
			old, err := j.CardView(ctx, p.Key)
			require.NoError(t, err)
			at := start.Add(16 * time.Minute)
			if mode == "dismissed" {
				_, err = j.DecideDelete(ctx, old.Ref, -900, notification, 11, false, start)
				require.NoError(t, err)
				at = start.Add(time.Minute)
			}
			if mode == "second operator" {
				at = start.Add(time.Minute)
			}
			decision := "renew:" + mode
			fresh, err := j.RenewDelete(ctx, old.Ref, -900, notification, 12, decision, at)
			require.NoError(t, err)
			require.True(t, fresh)
			next, err := j.CardView(ctx, p.Key)
			require.NoError(t, err)
			require.NotEqual(t, old.Ref, next.Ref)
			require.EqualValues(t, 12, next.RequesterID)
			require.Equal(t, at.Add(15*time.Minute).UnixMilli(), next.RequestExpires)
			require.Equal(t, old.Actions[0].ID, next.Actions[0].ID)
			require.Equal(t, old.Actions[0].Deadline, next.Actions[0].Deadline)
			require.Equal(t, old.Actions[0].MessageDate, next.Actions[0].MessageDate)
			stale, err := j.SupersededDeleteCard(ctx, old.Ref)
			require.NoError(t, err)
			require.True(t, stale)
			var archived struct {
				Requester int64  `db:"requester"`
				State     string `db:"state"`
				Next      string `db:"replaced_by"`
			}
			require.NoError(t, db.Get(&archived, db.Adopt(`SELECT requester,state,replaced_by FROM moderation_delete_request_history WHERE gid=? AND ref=?`), db.GID(), old.Ref))
			require.EqualValues(t, 11, archived.Requester)
			require.Equal(t, next.Ref, archived.Next)
			if mode == "dismissed" {
				require.Equal(t, "dismiss", archived.State)
			}
			var rawSnapshot string
			require.NoError(t, db.Get(&rawSnapshot, db.Adopt(`SELECT action_snapshot FROM moderation_delete_request_history WHERE gid=? AND ref=?`), db.GID(), old.Ref))
			var snapshot Action
			require.NoError(t, json.Unmarshal([]byte(rawSnapshot), &snapshot))
			require.Equal(t, old.Actions[0].ID, snapshot.ID)
			require.Equal(t, old.Actions[0].Deadline, snapshot.Deadline)
			if mode == "dismissed" {
				require.Equal(t, Canceled, snapshot.State)
				require.Equal(t, "delete_request_dismissed", snapshot.Code)
			} else {
				require.Equal(t, Manual, snapshot.State)
			}

			// Delivery replay never renews or changes the current requester/expiry.
			_, _, created, err := j.Accept(ctx, p, at.Add(time.Minute))
			require.NoError(t, err)
			require.False(t, created)
			fresh, err = j.RenewDelete(ctx, old.Ref, -900, notification, 12, decision, at.Add(time.Minute))
			require.NoError(t, err)
			require.False(t, fresh)
			require.NoError(t, j.BindCard(ctx, p.Key, 800+i))
			_, err = j.DecideDelete(ctx, old.Ref, -900, 800+i, 11, true, at)
			require.Error(t, err)
			_, err = j.RenewDelete(ctx, old.Ref, -900, notification, 11, "stale:"+mode, at)
			require.Error(t, err)
			unchanged, err := j.CardView(ctx, p.Key)
			require.NoError(t, err)
			require.Equal(t, next.Ref, unchanged.Ref)
			require.Equal(t, next.RequestExpires, unchanged.RequestExpires)
			reopened, err := NewJournal(ctx, db, DefaultLimits())
			require.NoError(t, err)
			var won atomic.Int32
			var wg sync.WaitGroup
			fail := make(chan error, 8)
			for range 8 {
				wg.Go(func() {
					ok, e := reopened.DecideDelete(ctx, next.Ref, -900, 800+i, 12, true, at)
					if e != nil {
						fail <- e
					}
					if ok {
						won.Add(1)
					}
				})
			}
			wg.Wait()
			close(fail)
			for e := range fail {
				require.NoError(t, e)
			}
			require.EqualValues(t, 1, won.Load())
			// Neither a confirmed request nor a second operator can duplicate a working delete.
			fresh, err = reopened.RenewDelete(ctx, next.Ref, -900, 800+i, 11, "busy:"+mode, at)
			require.NoError(t, err)
			require.False(t, fresh)
			calls := 0
			w := Worker{Journal: reopened, Now: func() time.Time { return at }, Executor: executeFunc(func(_ context.Context, a Action) Outcome {
				if a.Kind == "original_delete" {
					calls++
					require.Equal(t, p.MessageID, a.MessageID)
				}
				return Outcome{State: Confirmed}
			})}
			drive(t, &w)
			require.Equal(t, 1, calls)
			fresh, err = reopened.RenewDelete(ctx, next.Ref, -900, 800+i, 11, "confirmed:"+mode, at.Add(time.Minute))
			require.NoError(t, err)
			require.False(t, fresh)
			drive(t, &w)
			require.Equal(t, 1, calls)
		})
	}
}

func TestDeleteExplicitRenewalContract(t *testing.T) {
	j, db := testJournal(t, DefaultLimits())
	checkDeleteRenewalContract(t, j, db)
}

func TestDeleteConcurrentRenewalsAndFixedDeadline(t *testing.T) {
	j, db := testJournal(t, DefaultLimits())
	ctx := context.Background()
	now := time.Now()
	p := deletePlan(92010)
	accept(t, j, p, now)
	require.NoError(t, j.BindCard(ctx, p.Key, 700))
	old, err := j.CardView(ctx, p.Key)
	require.NoError(t, err)
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			ok, _ := j.RenewDelete(ctx, old.Ref, -900, 700, 12, fmt.Sprintf("race:%d", i), now.Add(25*time.Hour))
			if ok {
				won.Add(1)
			}
		})
	}
	wg.Wait()
	require.EqualValues(t, 1, won.Load())
	c, err := j.CardView(ctx, p.Key)
	require.NoError(t, err)
	require.Equal(t, old.Actions[0].Deadline, c.Actions[0].Deadline)
	require.NoError(t, j.BindCard(ctx, p.Key, 800))
	_, err = j.DecideDelete(ctx, c.Ref, -900, 800, 12, true, now.Add(25*time.Hour))
	require.NoError(t, err)
	calls := 0
	w := Worker{Journal: j, Now: func() time.Time { return now.Add(25 * time.Hour) }, Executor: executeFunc(func(_ context.Context, a Action) Outcome {
		if a.Kind == "original_delete" {
			calls++
		}
		return Outcome{State: Confirmed}
	})}
	drive(t, &w)
	require.Zero(t, calls)
	after, err := j.CardView(ctx, p.Key)
	require.NoError(t, err)
	require.Equal(t, Expired, after.Actions[0].State)
	var history int
	require.NoError(t, db.Get(&history, `SELECT count(*) FROM moderation_delete_request_history`))
	require.Equal(t, 1, history)
}
func TestDeleteRenewalRateHistoryAndNoAutomaticExtension(t *testing.T) {
	j, db := testJournal(t, DefaultLimits())
	ctx := context.Background()
	now := time.Now()
	p := deletePlan(92011)
	accept(t, j, p, now)
	require.NoError(t, j.BindCard(ctx, p.Key, 700))
	c, err := j.CardView(ctx, p.Key)
	require.NoError(t, err)
	oldExpiry := c.RequestExpires
	fresh, err := j.RenewDelete(ctx, c.Ref, -900, 700, 11, "same-owner", now.Add(time.Second))
	require.NoError(t, err)
	require.False(t, fresh)
	unchanged, err := j.CardView(ctx, p.Key)
	require.NoError(t, err)
	require.Equal(t, oldExpiry, unchanged.RequestExpires)
	for i := 1; i < 20; i++ {
		operator := int64(11)
		if c.RequesterID == 11 {
			operator = 12
		}
		fresh, err = j.RenewDelete(ctx, c.Ref, -900, 700+i-1, operator, fmt.Sprintf("epoch:%d", i), now.Add(time.Duration(i)*time.Second))
		require.NoError(t, err)
		require.True(t, fresh)
		c, err = j.CardView(ctx, p.Key)
		require.NoError(t, err)
		require.NoError(t, j.BindCard(ctx, p.Key, 700+i))
	}
	operator := int64(11)
	if c.RequesterID == 11 {
		operator = 12
	}
	fresh, err = j.RenewDelete(ctx, c.Ref, -900, 719, operator, "over-limit", now.Add(20*time.Second))
	require.ErrorIs(t, err, ErrRequestLimit)
	require.False(t, fresh)
	var history int
	require.NoError(t, db.Get(&history, `SELECT count(*) FROM moderation_delete_request_history`))
	require.Equal(t, 19, history)
}
func TestDeleteNoopRenewalCallbackCannotBecomeFutureConsent(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	ctx := context.Background()
	now := time.Now()
	p := deletePlan(92012)
	accept(t, j, p, now)
	require.NoError(t, j.BindCard(ctx, p.Key, 700))
	old, err := j.CardView(ctx, p.Key)
	require.NoError(t, err)
	fresh, err := j.RenewDelete(ctx, old.Ref, -900, 700, 11, "early-noop", now)
	require.NoError(t, err)
	require.False(t, fresh)
	fresh, err = j.RenewDelete(ctx, old.Ref, -900, 700, 11, "early-noop", now.Add(16*time.Minute))
	require.NoError(t, err)
	require.False(t, fresh)
	c, err := j.CardView(ctx, p.Key)
	require.NoError(t, err)
	require.Equal(t, old.RequestExpires, c.RequestExpires)
	require.Equal(t, old.Ref, c.Ref)
	fresh, err = j.RenewDelete(ctx, old.Ref, -900, 700, 11, "new-explicit-consent", now.Add(16*time.Minute))
	require.NoError(t, err)
	require.True(t, fresh)
}
