package events

import (
	"context"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/go-pkgz/testutils/containers"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/app/storage/engine"
)

func (f *forwardFixture) chooseSaved(t *testing.T, c moderation.Card, verb string) {
	t.Helper()
	var anchor int
	require.NoError(t, f.db.Get(&anchor, f.db.Adopt(`SELECT notification_id FROM moderation_actions WHERE gid=? AND receipt=? AND kind='card'`), f.db.GID(), c.Receipt))
	require.Positive(t, anchor)
	q := &tbapi.CallbackQuery{ID: "callback:" + c.Ref, From: &tbapi.User{ID: 11}, Data: verb + ":" + c.Ref,
		Message: &tbapi.Message{Chat: tbapi.Chat{ID: 456}, MessageID: anchor, Text: "copied card with a forged new target"}}
	require.NoError(t, f.listener.adminHandler.InlineCallbackHandler(q))
}
func (f *forwardFixture) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.Get(&n, f.db.Adopt("SELECT count(*) FROM "+table+" WHERE gid=?"), f.db.GID()))
	return n
}
func (f *forwardFixture) reopen(t *testing.T, open func() *engine.SQL) {
	t.Helper()
	require.NoError(t, f.db.Close())
	f.db = open()
	t.Cleanup(func() { require.NoError(t, f.db.Close()) })
	loc, err := storage.NewLocator(context.Background(), time.Hour, 1000, f.db)
	require.NoError(t, err)
	j, err := moderation.NewJournal(context.Background(), f.db, moderation.DefaultLimits())
	require.NoError(t, err)
	f.loc = loc
	f.j = j
	f.listener.Locator = loc
	f.listener.RecoveryJournal = j
}

// These scenarios run through the actual listener -> SQL journal -> worker. The
// PostgreSQL suite uses a separate container and per-scenario gid, never production.
func TestAdminForwardDeliverySQLite(t *testing.T) {
	runForwardDeliveryScenarios(t, func(t *testing.T) (*forwardFixture, func() *engine.SQL) {
		path := filepath.Join(t.TempDir(), "delivery.db")
		open := func() *engine.SQL { db, err := engine.NewSqlite(path, "delivery"); require.NoError(t, err); return db }
		return newForwardFixtureDB(t, open()), open
	})
}
func TestAdminForwardDeliveryPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration runs in full CI")
	}
	pg := containers.NewPostgresTestContainerWithDB(context.Background(), t, "forward_delivery")
	t.Cleanup(func() { pg.Close(context.Background()) })
	next := 0
	runForwardDeliveryScenarios(t, func(t *testing.T) (*forwardFixture, func() *engine.SQL) {
		next++
		gid := fmt.Sprintf("delivery-%d", next)
		open := func() *engine.SQL {
			db, err := engine.NewPostgres(context.Background(), pg.ConnectionString(), gid)
			require.NoError(t, err)
			return db
		}
		return newForwardFixtureDB(t, open()), open
	})
}

type forwardDeliveryFactory func(*testing.T) (*forwardFixture, func() *engine.SQL)

func runForwardDeliveryScenarios(t *testing.T, newFixture forwardDeliveryFactory) {
	for _, secondAuthor := range []int64{501, 502} {
		t.Run(fmt.Sprintf("new-forward-can-select-second-%d", secondAuthor), func(t *testing.T) {
			f, _ := newFixture(t)
			first := manualForwardUpdate()
			f.listen(t, originalForwardUpdate(801, 501), originalForwardUpdate(802, secondAuthor), first)
			f.settle(t)
			old := f.card(t)
			f.chooseSaved(t, old, "MF0")
			f.settle(t)
			require.Equal(t, map[int]int{801: 1}, f.deleteCalls)
			f.listen(t, nextForwardUpdate(1))
			f.settle(t)
			current := f.card(t)
			require.NotEqual(t, old.Receipt, current.Receipt)
			require.NotEqual(t, old.Ref, current.Ref)
			require.Equal(t, "pending", current.Forward.State)
			require.Equal(t, old.Forward.Targets[1].Deadline, current.Forward.Targets[1].Deadline)
			require.Contains(t, forwardCardText(current, "Fixture"), "Новая пересылка")
			// A previously consumed callback cannot decide the new request's second original.
			f.chooseSaved(t, old, "MF1")
			f.settle(t)
			require.Zero(t, f.deleteCalls[802])
			unchanged, err := f.j.CardView(context.Background(), current.Receipt)
			require.NoError(t, err)
			require.Equal(t, "pending", unchanged.Forward.State)
			f.chooseSaved(t, current, "MF1")
			f.chooseSaved(t, current, "MF1")
			f.settle(t)
			require.Equal(t, map[int]int{801: 1, 802: 1}, f.deleteCalls)
			require.Equal(t, 1, f.banCalls[501])
			require.Equal(t, 1, f.banCalls[secondAuthor])
			f.noLearning(t)
		})
	}
	t.Run("expired-choice-new-forward-active-fixed-deadline", func(t *testing.T) {
		f, _ := newFixture(t)
		f.listen(t, originalForwardUpdate(801, 501), originalForwardUpdate(802, 502), manualForwardUpdate())
		f.settle(t)
		old := f.card(t)
		_, err := f.db.Exec(f.db.Adopt(`UPDATE moderation_forward_batches SET expires_at=? WHERE gid=? AND receipt=?`), time.Now().Add(-time.Minute).UnixMilli(), f.db.GID(), old.Receipt)
		require.NoError(t, err)
		f.settle(t)
		f.listen(t, nextForwardUpdate(1))
		f.settle(t)
		current := f.card(t)
		require.Equal(t, "pending", current.Forward.State)
		require.Greater(t, current.Forward.Expires, time.Now().UnixMilli())
		require.Equal(t, "expired_choice", current.Forward.Reason)
		for i := range old.Forward.Targets {
			require.Equal(t, old.Forward.Targets[i].Deadline, current.Forward.Targets[i].Deadline)
		}
		f.chooseSaved(t, old, "MFA")
		f.settle(t)
		require.Empty(t, f.deleteCalls)
		f.chooseSaved(t, current, "MFA")
		f.settle(t)
		require.Equal(t, map[int]int{801: 1, 802: 1}, f.deleteCalls)
	})
	t.Run("delete-only-new-numeric-proof-requires-separate-ban-decision", func(t *testing.T) {
		f, _ := newFixture(t)
		require.NoError(t, f.loc.AddMessage(context.Background(), "same fixture original", 123, 501, "legacy name is not proof", 801))
		require.NoError(t, f.loc.RecordMessageDate(context.Background(), 123, 801, time.Now().Unix()))
		f.listen(t, manualForwardUpdate())
		f.settle(t)
		old := f.card(t)
		require.Empty(t, f.banCalls)
		require.Equal(t, 1, f.deleteCalls[801])
		replayed := manualForwardUpdate()
		replayed.Message.ForwardOrigin = &tbapi.MessageOrigin{Type: "user", SenderUser: &tbapi.User{ID: 501}}
		f.listen(t, replayed)
		f.settle(t)
		require.Empty(t, f.banCalls)
		require.Equal(t, 1, f.count(t, "moderation_forward_batches"))
		next := nextForwardUpdate(1)
		next.Message.ForwardOrigin = &tbapi.MessageOrigin{Type: "user", SenderUser: &tbapi.User{ID: 501}}
		f.listen(t, next)
		f.settle(t)
		current := f.card(t)
		require.Equal(t, "identity_review", current.Forward.Reason)
		require.Equal(t, "pending", current.Forward.State)
		require.Empty(t, f.banCalls)
		require.Contains(t, forwardCardText(current, "Fixture"), "ранее было разрешено только удаление")
		f.chooseSaved(t, old, "MF0")
		f.settle(t)
		require.Empty(t, f.banCalls)
		f.chooseSaved(t, current, "MF0")
		f.settle(t)
		require.Equal(t, 1, f.banCalls[501])
		require.Equal(t, 1, f.deleteCalls[801])
		preserved, err := f.j.CardView(context.Background(), old.Receipt)
		require.NoError(t, err)
		require.Zero(t, preserved.Forward.Targets[0].IdentityID)
		for _, a := range preserved.Actions {
			require.NotEqual(t, "ban", a.Kind)
		}
		require.Equal(t, old.Forward.Targets[0].Deadline, current.Forward.Targets[0].Deadline)
		f.noLearning(t)
	})
	t.Run("confirmed-result-no-replay-after-real-database-reopen", func(t *testing.T) {
		f, open := newFixture(t)
		f.listen(t, originalForwardUpdate(801, -501), manualForwardUpdate())
		f.settle(t)
		old := f.card(t)
		next := nextForwardUpdate(1)
		f.listen(t, next)
		f.settle(t)
		current := f.card(t)
		require.Equal(t, "already_confirmed", current.Forward.Reason)
		require.Contains(t, forwardCardText(current, "Fixture"), "Всё уже подтверждено")
		require.Equal(t, map[int64]int{-501: 1}, f.banCalls)
		require.Equal(t, map[int]int{801: 1}, f.deleteCalls)
		var core int
		require.NoError(t, f.db.Get(&core, f.db.Adopt(`SELECT count(*) FROM moderation_actions WHERE gid=? AND receipt=? AND kind!='card'`), f.db.GID(), current.Receipt))
		require.Zero(t, core)
		// Locator data may expire; a redelivery is resolved by its stored identity first.
		_, err := f.db.Exec(f.db.Adopt(`DELETE FROM messages WHERE gid=?`), f.db.GID())
		require.NoError(t, err)
		f.reopen(t, open)
		f.listen(t, next)
		f.settle(t)
		require.Equal(t, current.Ref, f.card(t).Ref)
		require.Equal(t, 2, f.count(t, "moderation_forward_deliveries"))
		require.Equal(t, 2, f.count(t, "moderation_forward_batches"))
		require.Equal(t, map[int64]int{-501: 1}, f.banCalls)
		require.Equal(t, map[int]int{801: 1}, f.deleteCalls)
		preserved, err := f.j.CardView(context.Background(), old.Receipt)
		require.NoError(t, err)
		require.Equal(t, old.Actions, preserved.Actions)
	})
	t.Run("permanent-refusal-redelivery-never-restarts-explicit-new-review", func(t *testing.T) {
		f, open := newFixture(t)
		f.failBan = true
		f.listen(t, originalForwardUpdate(801, 501), manualForwardUpdate())
		f.settle(t)
		old := f.card(t)
		require.Equal(t, 1, f.banCalls[501])
		require.Equal(t, 1, f.deleteCalls[801])
		f.failBan = false
		f.reopen(t, open)
		f.listen(t, manualForwardUpdate())
		f.settle(t)
		require.Equal(t, 1, f.banCalls[501])
		require.Equal(t, old.Ref, f.card(t).Ref)
		f.listen(t, nextForwardUpdate(1))
		f.settle(t)
		current := f.card(t)
		require.Equal(t, "retry_review", current.Forward.Reason)
		require.Equal(t, "pending", current.Forward.State)
		require.Contains(t, forwardCardText(current, "Fixture"), "постоянный отказ")
		require.Contains(t, forwardCardText(current, "Fixture"), "telegram_403")
		require.Equal(t, 1, f.banCalls[501])
		f.chooseSaved(t, old, "MF0")
		f.settle(t)
		require.Equal(t, 1, f.banCalls[501])
		f.chooseSaved(t, current, "MF0")
		f.settle(t)
		require.Equal(t, 2, f.banCalls[501])
		require.Equal(t, 1, f.deleteCalls[801])
		preserved, err := f.j.CardView(context.Background(), old.Receipt)
		require.NoError(t, err)
		for _, a := range preserved.Actions {
			if a.Kind == "ban" {
				require.Equal(t, moderation.Failed, a.State)
				require.Equal(t, "telegram_403", a.Code)
			}
		}
	})
	t.Run("concurrent-delivery-and-callback-with-restart", func(t *testing.T) {
		f, open := newFixture(t)
		f.listen(t, originalForwardUpdate(801, 501), originalForwardUpdate(802, 502))
		u := manualForwardUpdate()
		f.lastForward = u
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for range 8 {
			wg.Go(func() { errs <- f.listener.adminHandler.MsgHandler(u) })
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		f.settle(t)
		require.Equal(t, 1, f.count(t, "moderation_forward_batches"))
		require.Equal(t, 1, f.count(t, "moderation_forward_deliveries"))
		c := f.card(t)
		var anchor int
		require.NoError(t, f.db.Get(&anchor, f.db.Adopt(`SELECT notification_id FROM moderation_actions WHERE gid=? AND receipt=? AND kind='card'`), f.db.GID(), c.Receipt))
		results := make(chan bool, 8)
		errorsOut := make(chan error, 8)
		for range 8 {
			wg.Go(func() {
				fresh, err := f.j.DecideForward(context.Background(), c.Ref, 456, anchor, 11, []int{0, 1}, time.Now())
				results <- fresh
				errorsOut <- err
			})
		}
		wg.Wait()
		close(results)
		close(errorsOut)
		admitted := 0
		for fresh := range results {
			if fresh {
				admitted++
			}
		}
		for err := range errorsOut {
			require.NoError(t, err)
		}
		require.Equal(t, 1, admitted)
		f.settle(t)
		f.reopen(t, open)
		f.listen(t, u)
		f.settle(t)
		f.chooseSaved(t, c, "MFA")
		f.settle(t)
		require.Equal(t, map[int]int{801: 1, 802: 1}, f.deleteCalls)
		require.Equal(t, map[int64]int{501: 1, 502: 1}, f.banCalls)
		f.noLearning(t)
	})
	t.Run("update-and-forward-identities-cannot-be-rebound", func(t *testing.T) {
		f, _ := newFixture(t)
		f.listen(t, originalForwardUpdate(801, 501), manualForwardUpdate())
		f.settle(t)
		for _, d := range []moderation.ForwardDelivery{{UpdateID: 20000, ChatID: 456, MessageID: 901}, {UpdateID: 20001, ChatID: 456, MessageID: 900}} {
			_, _, err := f.j.LookupForwardDelivery(context.Background(), d, 11)
			require.Error(t, err)
		}
		require.Equal(t, 1, f.count(t, "moderation_forward_deliveries"))
		require.Equal(t, 1, f.banCalls[501])
		require.Equal(t, 1, f.deleteCalls[801])
	})
}
func TestAdminForwardDeliveryRendererExamples(t *testing.T) {
	dir := os.Getenv("FORWARD_CARD_EXAMPLES")
	if dir == "" {
		t.Skip("set FORWARD_CARD_EXAMPLES to export fixtures")
	}
	require.NoError(t, os.MkdirAll(dir, 0o700))
	for _, mode := range []string{"followup", "renewed", "ban-review", "saved-result", "retry-review"} {
		t.Run(mode, func(t *testing.T) {
			f := newForwardFixture(t)
			updates := []tbapi.Update{originalForwardUpdate(801, 501)}
			if mode == "followup" || mode == "renewed" {
				updates = append(updates, originalForwardUpdate(802, 502))
			}
			if mode == "ban-review" {
				updates = nil
				require.NoError(t, f.loc.AddMessage(context.Background(), "same fixture original", 123, 501, "legacy", 801))
			}
			if mode == "retry-review" {
				f.failBan = true
			}
			updates = append(updates, manualForwardUpdate())
			f.listen(t, updates...)
			f.settle(t)
			old := f.card(t)
			if mode == "followup" {
				f.chooseSaved(t, old, "MF0")
				f.settle(t)
			}
			if mode == "renewed" {
				_, err := f.db.Exec(`UPDATE moderation_forward_batches SET expires_at=?`, time.Now().Add(-time.Minute).UnixMilli())
				require.NoError(t, err)
				f.settle(t)
			}
			next := nextForwardUpdate(1)
			if mode == "ban-review" {
				next.Message.ForwardOrigin = &tbapi.MessageOrigin{Type: "user", SenderUser: &tbapi.User{ID: 501}}
			}
			f.listen(t, next)
			f.settle(t)
			c := f.card(t)
			var buttons strings.Builder
			for _, row := range forwardKeyboard(c).InlineKeyboard {
				for _, b := range row {
					fmt.Fprintf(&buttons, "<button disabled>%s</button>", html.EscapeString(b.Text))
				}
				buttons.WriteString("<br>")
			}
			page := "<!doctype html><meta charset=utf-8><title>Renderer delivery fixture</title><style>body{font:16px system-ui;max-width:720px;margin:32px auto}pre{white-space:pre-wrap}button{margin:4px;padding:8px}</style><pre>" + forwardCardText(c, "Тестовый чат") + "</pre>" + buttons.String()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "forward-"+mode+".html"), []byte(page), 0o600))
		})
	}
}
func TestAdminForwardDeniedDeliveryCannotGainAuthorityWhenModeChanges(t *testing.T) {
	f := newForwardFixture(t)
	f.listener.Dry = true
	f.listen(t, originalForwardUpdate(801, 501), manualForwardUpdate())
	f.settle(t)
	old := f.card(t)
	require.Equal(t, "gate_refused", old.Forward.Reason)
	require.Empty(t, f.banCalls)
	require.Empty(t, f.deleteCalls)
	f.listener.Dry = false
	f.listen(t, manualForwardUpdate())
	f.settle(t)
	require.Equal(t, old.Ref, f.card(t).Ref)
	require.Empty(t, f.banCalls)
	require.Empty(t, f.deleteCalls)
	f.listen(t, nextForwardUpdate(1))
	f.settle(t)
	current := f.card(t)
	require.Equal(t, "pending", current.Forward.State)
	require.Equal(t, "retry_review", current.Forward.Reason)
	f.chooseSaved(t, old, "MF0")
	f.settle(t)
	require.Empty(t, f.banCalls)
	f.chooseSaved(t, current, "MF0")
	f.settle(t)
	require.Equal(t, 1, f.banCalls[501])
	require.Equal(t, 1, f.deleteCalls[801])
}
