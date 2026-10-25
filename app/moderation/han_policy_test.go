package moderation

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/storage/engine"
)

func newHanPlan(message int, now time.Time) Plan {
	p := plan(message, "ban", "original_delete")
	p.Learn, p.Card, p.Origin = false, true, "detector"
	p.Reason = "Преобладает письменность Han: 24 из 24 букв (100.0%); порог 70.0%, минимум 20."
	p.Actions[0].UntilDate = now.Add(400 * 24 * time.Hour).Unix()
	p.Actions[0].MessageID = 0
	p.Actions[1].MessageDate = now.Unix()
	p.HanActions, p.Actions = p.Actions, nil
	return p
}

func checkHanPolicyContract(t *testing.T, j *Journal, db *engine.SQL) {
	t.Helper()
	ctx, now := context.Background(), time.Now()
	legacy := plan(901, "original_delete")
	legacy.Learn, legacy.Card, legacy.Origin, legacy.Reason = false, true, "detector", "Историческое Han: только удаление"
	legacy.Actions[0].MessageDate = now.Add(-time.Hour).Unix()
	old := accept(t, j, legacy, now)
	before, err := j.CardView(ctx, old)
	require.NoError(t, err)
	require.NoError(t, j.EnableHanBans(ctx, now.Add(-time.Minute)))
	require.NoError(t, j.EnableHanBans(ctx, now.Add(time.Hour)))
	var since int64
	require.NoError(t, db.GetContext(ctx, &since, db.Adopt(`SELECT since_date FROM moderation_han_policy WHERE gid=?`), db.GID()))
	require.Equal(t, now.Add(-time.Minute).Unix(), since, "restart/reenable cannot move the boundary")
	for _, identity := range []string{"user", "message", "sender_chat"} {
		p := newHanPlan(901, now)
		if identity != "user" {
			p.IdentityKind = identity
			p.IdentityID = 0
			if identity == "sender_chat" {
				p.IdentityID = -777
			}
			p.Key = fmt.Sprintf("source:%d:%d:%s:%d", p.ChatID, p.MessageID, p.IdentityKind, p.IdentityID)
			for i := range p.HanActions {
				p.HanActions[i].IdentityID, p.HanActions[i].IdentityKind = p.IdentityID, p.IdentityKind
			}
			if identity == "message" {
				p.HanActions = p.HanActions[1:]
			}
		}
		_, _, fresh, e := j.Accept(ctx, p, now)
		require.NoError(t, e)
		require.False(t, fresh)
	}
	after, err := j.CardView(ctx, old)
	require.NoError(t, err)
	require.Equal(t, before, after, "old card/history never upgraded by automatic Han")
	// Existing automatic Han history must not suppress a new independent demand.
	independent := newHanPlan(901, now)
	independent.Actions = slices.Clone(independent.HanActions)
	independent.Reason = "Независимое основание: external button"
	_, _, _, err = j.Accept(ctx, independent, now)
	require.NoError(t, err)
	updated, err := j.CardView(ctx, old)
	require.NoError(t, err)
	require.Contains(t, updated.Reason, legacy.Reason)
	require.Contains(t, updated.Reason, independent.Reason)
	bans := 0
	for _, a := range updated.Actions {
		if a.Kind == "ban" {
			bans++
		}
	}
	require.Equal(t, 1, bans)
	for _, d := range updated.Origins {
		require.NotEqual(t, HanOrigin, d.Origin, "old events acquire only independent authority")
	}
	for index, date := range []int64{since - 1, since, 0, now.Add(time.Hour).Unix()} {
		p := newHanPlan(910+index, now)
		p.HanActions[1].MessageDate = date
		key := accept(t, j, p, now)
		c, e := j.CardView(ctx, key)
		require.NoError(t, e)
		for _, a := range c.Actions {
			require.NotEqual(t, "ban", a.Kind)
		}
		require.Contains(t, c.Reason, "Блокировка по Han не запрошена")
	}
	for i, independent := range []bool{false, true} {
		p := newHanPlan(920+i, now)
		if independent {
			p.Actions = slices.Clone(p.HanActions)
		}
		key := accept(t, j, p, now)
		c, e := j.CardView(ctx, key)
		require.NoError(t, e)
		counts := map[string]int{}
		for _, a := range c.Actions {
			counts[a.Kind]++
		}
		require.Equal(t, 1, counts["ban"])
		require.Equal(t, 1, counts["original_delete"])
		for _, a := range c.Actions {
			if a.Kind == "ban" || a.Kind == "original_delete" {
				origins := map[string]bool{}
				for _, d := range c.Origins {
					if d.ActionID == a.ID && d.Active {
						origins[d.Origin] = true
					}
				}
				require.True(t, origins[HanOrigin])
				require.Equal(t, independent, origins["detector"])
			}
		}
		require.NoError(t, j.StopHanBan(ctx, key, now))
		c, e = j.CardView(ctx, key)
		require.NoError(t, e)
		for _, a := range c.Actions {
			if a.Kind == "ban" {
				want := Canceled
				if independent {
					want = Pending
				}
				require.Equal(t, want, a.State)
			}
			if a.Kind == "original_delete" {
				require.Equal(t, Pending, a.State)
			}
		}
		_, _, fresh, e := j.Accept(ctx, p, now.Add(time.Minute))
		require.NoError(t, e)
		require.False(t, fresh)
		unchanged, e := j.CardView(ctx, key)
		require.NoError(t, e)
		require.Equal(t, c, unchanged, "re-delivery cannot reauthorize stopped Han bans or slide deadlines")
		if !independent {
			p.Actions = slices.Clone(p.HanActions)
			_, _, _, e = j.Accept(ctx, p, now.Add(2*time.Minute))
			require.NoError(t, e)
			c, e = j.CardView(ctx, key)
			require.NoError(t, e)
			for _, d := range c.Origins {
				if strings.Contains(d.ActionID, ":ban:") {
					require.Equal(t, d.Origin != HanOrigin, d.Active, "new detector demand must not reactivate stopped Han")
				}
			}
			p.Actions = nil
			_, _, _, e = j.Accept(ctx, p, now.Add(3*time.Minute))
			require.NoError(t, e)
			unchanged, e = j.CardView(ctx, key)
			require.NoError(t, e)
			require.Equal(t, c, unchanged, "pure Han replay cannot renew independent or withdrawn work")
		}
	}
	// Unknown authors carry only exact-message deletion authority, never a fake user.
	p := newHanPlan(930, now)
	p.IdentityID, p.IdentityKind = 0, "message"
	p.Key = "source:-100:930:message:0"
	p.HanActions = p.HanActions[1:]
	p.HanActions[0].IdentityID, p.HanActions[0].IdentityKind = 0, "message"
	key := accept(t, j, p, now)
	c, err := j.CardView(ctx, key)
	require.NoError(t, err)
	for _, a := range c.Actions {
		if a.Kind != "original_delete" {
			continue
		}
		ok, e := j.DeleteAuthority(ctx, a)
		require.NoError(t, e)
		require.True(t, ok)
		a.MessageID++
		ok, e = j.DeleteAuthority(ctx, a)
		require.NoError(t, e)
		require.False(t, ok)
	}
	// The per-gid transaction lock admits only one concurrent decision per original.
	p = newHanPlan(940, now)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Go(func() { _, _, _, e := j.Accept(ctx, p, now); errs <- e })
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	c, err = j.CardView(ctx, p.Key)
	require.NoError(t, err)
	count := 0
	for _, a := range c.Actions {
		if a.Kind == "ban" {
			count++
		}
	}
	require.Equal(t, 1, count)
}

func TestHanPolicySQLiteContract(t *testing.T) {
	j, db := testJournal(t, DefaultLimits())
	checkHanPolicyContract(t, j, db)
}

func TestHanPolicyStopInFlightAndConfirmed(t *testing.T) {
	for _, ack := range []bool{false, true} {
		t.Run(fmt.Sprint(ack), func(t *testing.T) {
			j, _ := testJournal(t, DefaultLimits())
			now := time.Now()
			require.NoError(t, j.EnableHanBans(testCtx, now.Add(-time.Minute)))
			p := newHanPlan(950, now)
			key := accept(t, j, p, now)
			a, ok, err := j.Claim(testCtx, now)
			require.NoError(t, err)
			require.True(t, ok)
			for a.Kind == "notice" || a.Kind == "card" {
				require.NoError(t, j.Finish(testCtx, a, Outcome{State: Confirmed}, now, now))
				a, ok, err = j.Claim(testCtx, now)
				require.NoError(t, err)
				require.True(t, ok)
			}
			require.Equal(t, "ban", a.Kind)
			require.NoError(t, j.StopHanBan(testCtx, key, now))
			mode, err := j.Authorize(testCtx, a)
			require.NoError(t, err)
			require.Equal(t, "withdrawn", mode)
			out := Outcome{Retry: true, Uncertain: true}
			if ack {
				out = Outcome{State: Confirmed}
			}
			require.NoError(t, j.Finish(testCtx, a, out, now, now))
			state := find(t, j, "ban", 0).State
			if ack {
				require.Equal(t, Confirmed, state, "withdrawal cannot undo Telegram acknowledgement")
			} else {
				require.Equal(t, Manual, state, "an unacknowledged dispatched ban is never called canceled/confirmed")
			}
		})
	}
}

func TestHanPolicyExplicitOperatorDecisions(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	require.NoError(t, j.EnableHanBans(testCtx, now.Add(-time.Minute)))
	legacy := plan(960, "original_delete")
	legacy.Learn, legacy.Card = false, true
	key := accept(t, j, legacy, now)
	c, err := j.CardView(testCtx, key)
	require.NoError(t, err)
	fresh, err := j.Decide(testCtx, "explicit-old-approval", c, "approve", 42, now)
	require.NoError(t, err)
	require.True(t, fresh, "an operator may explicitly request a ban on a legacy card")
	c, err = j.CardView(testCtx, key)
	require.NoError(t, err)
	bans := 0
	for _, a := range c.Actions {
		if a.Kind == "ban" {
			bans++
		}
	}
	require.Equal(t, 1, bans)
	p := newHanPlan(961, now)
	key = accept(t, j, p, now)
	c, err = j.CardView(testCtx, key)
	require.NoError(t, err)
	fresh, err = j.Decide(testCtx, "explicit-new-cancel", c, "cancel", 42, now)
	require.NoError(t, err)
	require.True(t, fresh)
	c, err = j.CardView(testCtx, key)
	require.NoError(t, err)
	for _, a := range c.Actions {
		if a.Kind == "ban" || a.Kind == "original_delete" {
			require.Equal(t, Canceled, a.State, "explicit cancel withdraws Han as well as other authority")
		}
	}
}

func TestHanPolicyAncientRedeliveryWithoutRetainedReceipt(t *testing.T) {
	j, _ := testJournal(t, DefaultLimits())
	now := time.Now()
	require.NoError(t, j.EnableHanBans(testCtx, now.Add(-60*24*time.Hour)))
	p := newHanPlan(970, now.Add(-40*24*time.Hour))
	key := accept(t, j, p, now)
	c, err := j.CardView(testCtx, key)
	require.NoError(t, err)
	for _, a := range c.Actions {
		require.NotEqual(t, "ban", a.Kind, "expired originals cannot acquire a new Han ban after journal retention")
	}
	require.Contains(t, c.Reason, "вне окна удаления")
}
