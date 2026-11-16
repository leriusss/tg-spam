package events

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/go-pkgz/testutils/containers"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage/engine"
)

type sharedFleet struct {
	fixtures    []*forwardFixture
	bridges     []*sharedForward
	store       *moderation.SharedStore
	now         time.Time
	reopenStore func() *moderation.SharedStore
}

func newSharedFleet(t *testing.T, counts []int, open func(string) *engine.SQL) *sharedFleet {
	t.Helper()
	if open == nil {
		baseDir := t.TempDir()
		open = func(gid string) *engine.SQL {
			db, err := engine.NewSqlite(filepath.Join(baseDir, gid+".db"), gid)
			require.NoError(t, err)
			return db
		}
	}
	db := open("shared")
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	peers := make([]int64, len(counts))
	for i := range peers {
		peers[i] = int64(100 + i)
	}
	store, err := moderation.NewSharedStore(context.Background(), db, 100, peers)
	require.NoError(t, err)
	fleet := &sharedFleet{store: store, now: time.Now().Add(3 * time.Second)}
	fleet.reopenStore = func() *moderation.SharedStore {
		freshDB := open("shared")
		t.Cleanup(func() { require.NoError(t, freshDB.Close()) })
		fresh, err := moderation.NewSharedStore(context.Background(), freshDB, 100, peers)
		require.NoError(t, err)
		return fresh
	}
	for i, n := range counts {
		f := newForwardFixtureDB(t, open(fmt.Sprintf("local-%d", i)))
		chat := int64(-1000 - i)
		f.listener.Group = strconv.FormatInt(chat, 10)
		updates := []tbapi.Update{}
		for j := range n {
			u := originalForwardUpdate(800+i*100+j, int64(501+i))
			u.Message.Chat.ID = chat
			updates = append(updates, u)
		}
		f.listen(t, updates...)
		require.NoError(t, f.j.EnableSharedOutbox(context.Background()))
		bridge := &sharedForward{config: SharedForwardConfig{Peer: peers[i], Owner: 100, Peers: peers}, admin: f.listener.adminHandler, store: store, cache: map[string]string{}}
		bridge.now = func() time.Time { return fleet.now }
		f.listener.adminHandler.shared = bridge
		f.listener.adminHandler.recovery.chatName = []string{"Балтия", "Европа", "Неопорт", "ЛЕС", "Мультиверс"}[i]
		base := f.api.call
		f.api.call = func(ctx context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
			resp, e := base(ctx, c)
			if msg, ok := c.(tbapi.MessageConfig); ok && e == nil {
				return apiOK(tbapi.Message{Chat: tbapi.Chat{ID: msg.ChatID}, MessageID: f.nextCard}), nil
			}
			return resp, e
		}
		fleet.fixtures = append(fleet.fixtures, f)
		fleet.bridges = append(fleet.bridges, bridge)
	}
	return fleet
}

func (f *sharedFleet) forward(t *testing.T, delivery int, indices ...int) {
	t.Helper()
	if len(indices) == 0 {
		for i := range f.bridges {
			indices = append(indices, i)
		}
	}
	for _, i := range indices {
		u := manualForwardUpdate()
		u.Message.MessageID = delivery
		u.UpdateID = 200000 + i*1000 + delivery
		require.NoError(t, f.bridges[i].admin.MsgHandler(u))
	}
}

func (f *sharedFleet) tick(t *testing.T) {
	t.Helper()
	for _, b := range f.bridges {
		require.NoError(t, b.step(context.Background(), f.now))
	}
	f.now = f.now.Add(3 * time.Second)
	require.NoError(t, f.bridges[0].step(context.Background(), f.now))
	for _, fixture := range f.fixtures {
		fixture.now = f.now
	}
}

func (f *sharedFleet) one(t *testing.T) moderation.SharedCase {
	t.Helper()
	cases, err := f.store.Cases(context.Background())
	require.NoError(t, err)
	require.Len(t, cases, 1)
	return cases[0]
}

func (f *sharedFleet) choose(t *testing.T, c moderation.SharedCase, verb string, alter func(*tbapi.CallbackQuery)) {
	t.Helper()
	q := &tbapi.CallbackQuery{ID: "shared-callback", From: &tbapi.User{ID: 11}, Data: fmt.Sprintf("S:%s:%s:%d", verb, c.Ref, c.Revision), Message: &tbapi.Message{Chat: tbapi.Chat{ID: 456}, MessageID: c.NotificationID, From: &tbapi.User{ID: 100}}}
	if alter != nil {
		alter(q)
	}
	err := f.bridges[0].admin.InlineCallbackHandler(q)
	if alter == nil {
		require.NoError(t, err)
	}
}

func runSharedScenarios(t *testing.T, open func(string) *engine.SQL) {
	t.Helper()
	f := newSharedFleet(t, []int{2, 3, 2, 0, 2}, open)
	// Same text, four different numeric authors, three distinct bot-scoped IDs.
	for _, delivery := range []int{1324, 1325, 1326} {
		f.forward(t, delivery)
	}
	f.tick(t)
	c := f.one(t)
	require.Len(t, c.Reports, 5)
	require.False(t, c.Decided)
	require.Positive(t, c.NotificationID)
	require.Len(t, f.fixtures[0].cards, 1)
	for _, fixture := range f.fixtures[1:] {
		require.Empty(t, fixture.cards)
	}
	for _, fixture := range f.fixtures {
		require.Empty(t, fixture.banCalls)
		require.Empty(t, fixture.deleteCalls)
	}
	text, _ := sharedViewCard(moderation.SharedView{Case: c}, f.bridges[0].config.Peers, f.now)
	require.NotContains(t, text, "same fixture original")
	require.NotContains(t, text, "№800")
	if dir := os.Getenv("SHARED_CARD_EXAMPLES"); dir != "" {
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "pending.txt"), []byte(text), 0o600))
	}
	// Forged anchor, different operator and stale revision have no authority.
	f.choose(t, c, "all", func(q *tbapi.CallbackQuery) { q.Message.MessageID++ })
	f.choose(t, c, "all", func(q *tbapi.CallbackQuery) { q.From.ID = 12 })
	require.False(t, f.one(t).Decided)
	f.choose(t, c, "101", nil)
	f.choose(t, c, "all", nil)
	c = f.one(t)
	require.True(t, c.Decided)
	require.Len(t, c.Choices, 1)
	require.EqualValues(t, 101, c.Choices[0].Report.Peer)
	f.tick(t)
	for _, fixture := range f.fixtures {
		fixture.settle(t)
	}
	f.tick(t)
	require.Len(t, f.fixtures[1].deleteCalls, 3)
	require.Equal(t, 1, f.fixtures[1].banCalls[502])
	require.Empty(t, f.fixtures[0].banCalls)
	require.Empty(t, f.fixtures[2].banCalls)
	// Reopening the coordinator and local worker does not repeat effects.
	f.store = f.reopenStore()
	for _, b := range f.bridges {
		b.store = f.store
		b.cache = map[string]string{}
	}
	f.tick(t)
	for _, fixture := range f.fixtures {
		fixture.settle(t)
	}
	require.Equal(t, 1, f.fixtures[1].banCalls[502])
	for _, n := range f.fixtures[1].deleteCalls {
		require.Equal(t, 1, n)
	}
	text, _ = sharedViewCard(moderation.SharedView{Case: f.one(t)}, f.bridges[0].config.Peers, f.now)
	require.Contains(t, text, "подтверждено: 3")
	if dir := os.Getenv("SHARED_CARD_EXAMPLES"); dir != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "selected.txt"), []byte(text), 0o600))
	}
}

func TestSharedNewOriginalDoesNotExpandConfirmedSet(t *testing.T) {
	t.Parallel()
	f := newSharedFleet(t, []int{2, 2}, nil)
	f.forward(t, 1324)
	f.tick(t)
	old := f.one(t)
	f.choose(t, old, "100", nil)
	f.tick(t)
	u := originalForwardUpdate(950, 502)
	u.Message.Chat.ID = -1001
	require.NoError(t, f.fixtures[1].listener.procEvents(u))
	f.forward(t, 1325)
	f.tick(t)
	cases, err := f.store.Cases(context.Background())
	require.NoError(t, err)
	require.Len(t, cases, 2)
	for _, c := range cases {
		if c.Ref == old.Ref {
			require.True(t, c.Decided)
			require.Len(t, c.Choices, 1)
			require.Len(t, c.Reports[1].Targets, 2)
		} else {
			require.False(t, c.Decided)
			require.Len(t, c.Reports[1].Targets, 3)
		}
	}
	for _, fixture := range f.fixtures {
		fixture.settle(t)
	}
	require.Empty(t, f.fixtures[1].banCalls)
	require.Empty(t, f.fixtures[1].deleteCalls)
}

func TestSharedRepeatedForwardByAnotherCurrentAdmin(t *testing.T) {
	t.Parallel()
	f := newSharedFleet(t, []int{2, 2}, nil)
	f.forward(t, 1324)
	f.tick(t)
	for i, b := range f.bridges {
		f.fixtures[i].operatorRoles = map[int64]string{12: "administrator"}
		u := manualForwardUpdate()
		u.Message.MessageID = 1325
		u.UpdateID = 300000 + i
		u.Message.From.ID = 12
		require.NoError(t, b.admin.MsgHandler(u))
	}
	f.tick(t)
	c := f.one(t)
	f.choose(t, c, "all", func(q *tbapi.CallbackQuery) { q.From.ID = 12 })
	require.EqualValues(t, 12, f.one(t).DecisionOperator)
	f.tick(t)
	for _, fixture := range f.fixtures {
		fixture.settle(t)
	}
	require.Equal(t, 1, f.fixtures[0].banCalls[501])
	require.Equal(t, 1, f.fixtures[1].banCalls[502])
}

func TestSharedExpiryRequiresNewExplicitChoiceWithoutDeadlineExtension(t *testing.T) {
	t.Parallel()
	f := newSharedFleet(t, []int{1}, nil)
	// Use two candidates to keep the initial decision pending.
	u := originalForwardUpdate(801, 501)
	u.Message.Chat.ID = -1000
	require.NoError(t, f.fixtures[0].listener.procEvents(u))
	f.forward(t, 1324)
	f.tick(t)
	old := f.one(t)
	f.now = f.now.Add(16 * time.Minute)
	f.tick(t)
	require.Error(t, f.store.Decide(context.Background(), 100, old.Ref, old.Revision, 456, old.NotificationID, 11, 0, -1, f.now))
	require.False(t, f.one(t).Decided)
	r := old.Reports[0]
	r.Delivery.MessageID = 1325
	r.Delivery.UpdateID++
	r.Receipt = ""
	require.NoError(t, f.fixtures[0].j.AcceptSharedForward(context.Background(), r, f.now))
	f.tick(t)
	cases, err := f.store.Cases(context.Background())
	require.NoError(t, err)
	require.Len(t, cases, 2)
	for _, c := range cases {
		if c.Ref == old.Ref {
			continue
		}
		require.False(t, c.Decided)
		require.Equal(t, old.Reports[0].Targets[0].Deadline, c.Reports[0].Targets[0].Deadline)
	}
	require.Empty(t, f.fixtures[0].banCalls)
}

func TestSharedForwardIncidentSQLite(t *testing.T) {
	t.Parallel()
	runSharedScenarios(t, nil)
}

func TestSharedDoesNotImportLegacyCards(t *testing.T) {
	t.Parallel()
	f := newSharedFleet(t, []int{2}, nil)
	a := f.bridges[0].admin
	a.shared = nil
	u := manualForwardUpdate()
	require.NoError(t, a.MsgHandler(u))
	f.fixtures[0].settle(t)
	require.Len(t, f.fixtures[0].cards, 1)
	a.shared = f.bridges[0]
	require.NoError(t, a.MsgHandler(u))
	f.tick(t)
	cases, err := f.store.Cases(context.Background())
	require.NoError(t, err)
	require.Empty(t, cases)
	require.Len(t, f.fixtures[0].cards, 1)
}

func TestSharedOlderWorkerNotificationFencePreservesAuthority(t *testing.T) {
	t.Parallel()
	f := newSharedFleet(t, []int{2}, nil)
	f.forward(t, 1324)
	f.tick(t)
	fixture := f.fixtures[0]
	reports, err := fixture.j.SharedOutbox(context.Background())
	require.NoError(t, err)
	r := reports[0]
	_, err = fixture.db.Exec(fixture.db.Adopt(`UPDATE moderation_cards SET destination=0 WHERE gid=? AND EXISTS (SELECT 1 FROM moderation_silent_cards s WHERE s.gid=moderation_cards.gid AND s.receipt=moderation_cards.receipt)`), fixture.db.GID())
	require.NoError(t, err)
	var destination int64
	require.NoError(t, fixture.db.Get(&destination, fixture.db.Adopt(`SELECT destination FROM moderation_receipts WHERE gid=? AND id=?`), fixture.db.GID(), r.Receipt))
	require.EqualValues(t, 456, destination)
	// Model the older dirtyCard's notification after expiry/worker completion.
	_, err = fixture.db.Exec(fixture.db.Adopt(`INSERT INTO moderation_actions VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), fixture.db.GID(), r.Receipt+":card", r.Receipt, "card", 0, 0, "system", 0, 0, 0, moderation.Pending, "", 0, f.now.Add(time.Hour).UnixMilli(), f.now.UnixMilli(), "", 0, false, 1, 0, 0)
	require.NoError(t, err)
	fixture.settle(t)
	require.Len(t, fixture.cards, 1)
	rows, err := fixture.j.SharedOutbox(context.Background())
	require.NoError(t, err)
	require.Equal(t, r.Targets, rows[0].Targets)
	var code string
	require.NoError(t, fixture.db.Get(&code, fixture.db.Adopt(`SELECT code FROM moderation_actions WHERE gid=? AND id=?`), fixture.db.GID(), r.Receipt+":card"))
	require.Equal(t, "notification_destination_changed", code)
}

func TestSharedHistoricalSuccessAndPartialFailure(t *testing.T) {
	t.Parallel()
	f := newSharedFleet(t, []int{2, 2}, nil)
	prior := moderation.Plan{Key: "source:-1000:800:user:501", ChatID: -1000, IdentityID: 501, IdentityKind: "user", MessageID: 800, Destination: 456, Origin: "detector",
		Actions: []moderation.Action{{Kind: "ban", ChatID: -1000, IdentityID: 501, IdentityKind: "user"},
			{Kind: "original_delete", ChatID: -1000, IdentityID: 501, IdentityKind: "user", MessageID: 800, MessageDate: time.Now().Unix()},
			{Kind: "original_delete", ChatID: -1000, IdentityID: 501, IdentityKind: "user", MessageID: 801, MessageDate: time.Now().Unix()}}}
	_, _, _, err := f.fixtures[0].j.Accept(context.Background(), prior, time.Now())
	require.NoError(t, err)
	f.fixtures[0].settle(t)
	require.Equal(t, 1, f.fixtures[0].banCalls[501])
	f.fixtures[1].failDelete = true
	f.forward(t, 1324)
	f.tick(t)
	c := f.one(t)
	text, _ := sharedViewCard(moderation.SharedView{Case: c}, f.bridges[0].config.Peers, f.now)
	require.Contains(t, text, "ранее обработано: 2")
	if dir := os.Getenv("SHARED_CARD_EXAMPLES"); dir != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "previously-handled.txt"), []byte(text), 0o600))
	}
	f.choose(t, c, "all", nil)
	f.tick(t)
	for _, fixture := range f.fixtures {
		fixture.settle(t)
	}
	f.tick(t)
	require.Equal(t, 1, f.fixtures[0].banCalls[501])
	for _, n := range f.fixtures[0].deleteCalls {
		require.Equal(t, 1, n)
	}
	require.Equal(t, 1, f.fixtures[1].banCalls[502])
	text, _ = sharedViewCard(moderation.SharedView{Case: f.one(t)}, f.bridges[0].config.Peers, f.now)
	require.Contains(t, text, "удаление: отказ: 2; блокировка: подтверждено: 1")
	if dir := os.Getenv("SHARED_CARD_EXAMPLES"); dir != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "partial-failure.txt"), []byte(text), 0o600))
	}
}

func TestSharedSearchFailureCannotAutoSelectSingleOriginal(t *testing.T) {
	t.Parallel()
	f := newSharedFleet(t, []int{1, 0}, nil)
	f.forward(t, 1324, 0)
	u := manualForwardUpdate()
	u.Message.MessageID = 1324
	u.UpdateID = 200000 + 1000 + 1324
	require.NoError(t, f.bridges[1].captureFailure(deliveryForShared(u), 11, "locator unavailable"))
	f.tick(t)
	require.False(t, f.one(t).Decided)
	text, _ := sharedViewCard(moderation.SharedView{Case: f.one(t)}, f.bridges[0].config.Peers, f.now)
	require.Contains(t, text, "поиск недоступен")
	require.Empty(t, f.fixtures[0].banCalls)
}

func TestSharedOfflineOutboxDoesNotBlockOrdinaryWorker(t *testing.T) {
	t.Parallel()
	f := newSharedFleet(t, []int{2}, nil)
	f.bridges[0].store = nil
	f.forward(t, 1324)
	reports, err := f.fixtures[0].j.SharedOutbox(context.Background())
	require.NoError(t, err)
	require.Len(t, reports, 1)
	p := moderation.Plan{Key: "source:-1000:850:user:999", ChatID: -1000, MessageID: 850, IdentityKind: "user", IdentityID: 999, Origin: "detector",
		Actions: []moderation.Action{{Kind: "ban", ChatID: -1000, IdentityKind: "user", IdentityID: 999}}}
	_, _, _, err = f.fixtures[0].j.Accept(context.Background(), p, time.Now())
	require.NoError(t, err)
	f.fixtures[0].settle(t)
	require.Equal(t, 1, f.fixtures[0].banCalls[999])
	require.Empty(t, f.fixtures[0].deleteCalls)
	f.bridges[0].store = f.reopenStore()
	f.tick(t)
	require.Positive(t, f.one(t).NotificationID)
}

func TestSharedCurrentSafetyGates(t *testing.T) {
	t.Parallel()
	for _, gate := range []string{"dry", "training", "superuser", "trust"} {
		t.Run(gate, func(t *testing.T) {
			f := newSharedFleet(t, []int{2}, nil)
			a := f.bridges[0].admin
			switch gate {
			case "dry":
				a.dry = true
			case "training":
				a.trainingMode = true
			case "superuser":
				a.superUsers = append(a.superUsers, "501")
			case "trust":
				a.isExplicitlyApproved = func(int64) bool { return true }
			}
			f.forward(t, 1324)
			f.tick(t)
			c := f.one(t)
			f.choose(t, c, "all", nil)
			f.tick(t)
			f.fixtures[0].settle(t)
			require.False(t, f.one(t).Decided)
			require.Empty(t, f.fixtures[0].banCalls)
			require.Empty(t, f.fixtures[0].deleteCalls)
		})
	}
}

func TestSharedCaptionAndCallbackBounds(t *testing.T) {
	t.Parallel()
	c := moderation.SharedCase{Ref: "0123456789abcdef0123456789abcdef", AdminID: 456, Expires: time.Now().Add(time.Hour).UnixMilli(), Revision: 100}
	peers := []int64{100, 101, 102, 103, 104}
	for _, peer := range peers {
		r := moderation.SharedReport{Peer: peer, Name: "A long protected chat title & <tag> for renderer", State: "accepted", Reason: "retry_review"}
		for i := range 20 {
			r.Targets = append(r.Targets, moderation.ForwardTarget{MessageID: 800 + i, IdentityID: 500 + int64(i), IdentityKind: "user", Evidence: "telegram_update", Deadline: time.Now().UnixMilli()})
			r.Actions = append(r.Actions, moderation.Action{MessageID: 800 + i, IdentityID: 500 + int64(i), Kind: "original_delete", State: moderation.Absent}, moderation.Action{IdentityID: 500 + int64(i), Kind: "ban", State: moderation.Blocked, Code: "forward_operator_revoked_outcome_unknown"})
		}
		c.Reports = append(c.Reports, r)
		c.Choices = append(c.Choices, moderation.SharedChoice{Report: r})
	}
	for page := range 21 {
		c.Page = page
		text, keyboard := sharedViewCard(moderation.SharedView{Case: c}, peers, time.Now())
		require.Less(t, len([]rune(text)), 4096)
		for _, row := range keyboard.InlineKeyboard {
			for _, button := range row {
				require.LessOrEqual(t, len(*button.CallbackData), 64)
			}
		}
	}
	intent := moderation.SharedIntent{Operator: 11, Reason: strings.Repeat("x", 512)}
	for _, peer := range peers {
		entry := moderation.SharedIntentChoice{Peer: peer}
		for i := range 20 {
			entry.MessageIDs = append(entry.MessageIDs, int(^uint(0)>>1)-i)
		}
		intent.Choices = append(intent.Choices, entry)
	}
	c.NeedsReview = true
	c.ReviewReason = intent.Reason
	c.PriorIntents = []moderation.SharedIntent{intent}
	c.Page = 21
	view := moderation.SharedView{Case: c, Page: 99}
	for i := range 500 {
		view.Menu = append(view.Menu, moderation.SharedMenuItem{Ref: fmt.Sprint(i), MessageID: i + 1})
	}
	text, keyboard := sharedViewCard(view, peers, time.Now())
	require.Less(t, len([]rune(text)), 4096)
	for _, row := range keyboard.InlineKeyboard {
		for _, button := range row {
			require.LessOrEqual(t, len(*button.CallbackData), 64)
		}
	}
}

func TestSharedLeaseFencesDifferentInquiryViews(t *testing.T) {
	t.Parallel()
	f := newSharedFleet(t, []int{2, 2}, nil)
	f.forward(t, 1324, 0)
	f.forward(t, 1325, 1)
	f.tick(t)
	view, found, err := f.store.View(context.Background(), 100, 456, f.now)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, view.Menu, 2)
	claim, ok, err := f.store.ClaimCard(context.Background(), 100, view.Case.Ref, "first process", f.now)
	require.NoError(t, err)
	require.True(t, ok)
	next := 0
	if view.Menu[0].Ref == view.Case.Ref {
		next = 1
	}
	require.NoError(t, f.store.SwitchView(context.Background(), 100, view.Case.Ref, view.Case.Revision, 456, view.Case.NotificationID, next, -1))
	_, ok, err = f.store.ClaimCard(context.Background(), 100, view.Menu[next].Ref, "second process", f.now)
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, f.store.FinishCard(context.Background(), claim, view.Case.NotificationID, "first process", "ok", f.now))
	second, ok, err := f.store.ClaimCard(context.Background(), 100, view.Menu[next].Ref, "second process", f.now)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, view.Case.NotificationID, second.NotificationID)
	require.Len(t, f.fixtures[0].cards, 1)
}

func TestSharedForwardIncidentPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL integration runs in full CI")
	}
	pg := containers.NewPostgresTestContainerWithDB(context.Background(), t, "shared_forward")
	t.Cleanup(func() { pg.Close(context.Background()) })
	openScope := func(scope string) func(string) *engine.SQL {
		return func(gid string) *engine.SQL {
			db, err := engine.NewPostgres(context.Background(), pg.ConnectionString(), scope+"-"+gid)
			require.NoError(t, err)
			return db
		}
	}
	t.Run("incident", func(t *testing.T) { runSharedScenarios(t, openScope("incident")) })
	t.Run("independent", func(t *testing.T) { runSharedIndependentInquiries(t, openScope("independent")) })
	t.Run("crossed", func(t *testing.T) { runSharedCrossedDeliveries(t, openScope("crossed")) })
	t.Run("late-refusal", func(t *testing.T) { runSharedDelayedIntent(t, openScope("late-refusal")) })
}

func TestSharedLateResponseAndRoleRevocation(t *testing.T) {
	t.Parallel()
	f := newSharedFleet(t, []int{2, 2}, nil)
	f.forward(t, 1324, 0)
	f.tick(t)
	c := f.one(t)
	require.Len(t, c.Reports, 1)
	text, _ := sharedViewCard(moderation.SharedView{Case: c}, []int64{100, 101}, f.now)
	require.Contains(t, text, "Ожидается ответ инстансов: 1")
	f.choose(t, c, "all", nil)
	f.forward(t, 1324, 1)
	f.tick(t)
	c = f.one(t)
	require.Len(t, c.Reports, 2)
	require.Len(t, c.Choices, 1)
	f.fixtures[0].operatorRole = "member"
	for _, fixture := range f.fixtures {
		fixture.settle(t)
	}
	require.Empty(t, f.fixtures[0].banCalls)
	require.Empty(t, f.fixtures[0].deleteCalls)
	require.Empty(t, f.fixtures[1].banCalls)
}

func TestSharedLateTargetsCanReceiveANewDecisionWhileEarlierWorkIsUnknown(t *testing.T) {
	t.Parallel()
	f := newSharedFleet(t, []int{2, 2}, nil)
	f.forward(t, 1324, 0)
	f.tick(t)
	first := f.one(t)
	f.choose(t, first, "100", nil)
	f.tick(t)
	f.fixtures[0].unknownDelete = true
	f.fixtures[0].settle(t)
	f.tick(t)
	f.forward(t, 1324, 1)
	f.tick(t)
	f.forward(t, 1325)
	f.tick(t)
	view, found, err := f.store.View(context.Background(), 100, 456, f.now)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, view.Menu, 2)
	index := -1
	for i, item := range view.Menu {
		if item.Ref != first.Ref {
			index = i
		}
	}
	require.NotEqual(t, -1, index)
	f.choose(t, view.Case, fmt.Sprintf("c%d", index), nil)
	f.tick(t)
	view, found, err = f.store.View(context.Background(), 100, 456, f.now)
	require.NoError(t, err)
	require.True(t, found)
	f.choose(t, view.Case, "101", nil)
	f.tick(t)
	f.fixtures[1].settle(t)
	f.tick(t)
	require.Equal(t, 1, f.fixtures[1].banCalls[502])
	require.Equal(t, map[int]int{900: 1, 901: 1}, f.fixtures[1].deleteCalls)
	for _, n := range f.fixtures[0].deleteCalls {
		require.Equal(t, 1, n)
	}
	require.Len(t, f.fixtures[0].cards, 1)
	old, err := f.store.Case(context.Background(), first.Ref)
	require.NoError(t, err)
	require.Len(t, old.Choices, 1)
	require.EqualValues(t, 100, old.Choices[0].Report.Peer)
}

func TestSharedCrossedLateDeliveriesLeaveOneActiveCard(t *testing.T) {
	t.Parallel()
	runSharedCrossedDeliveries(t, nil)
}

func runSharedCrossedDeliveries(t *testing.T, open func(string) *engine.SQL) {
	t.Helper()
	f := newSharedFleet(t, []int{2, 2}, open)
	f.forward(t, 1324, 0)
	f.forward(t, 1325, 1)
	f.tick(t)
	cases, err := f.store.Cases(context.Background())
	require.NoError(t, err)
	require.Len(t, cases, 2)
	require.Len(t, f.fixtures[0].cards, 1, "crossed first responses must share the same persistent Telegram anchor")
	f.forward(t, 1324, 1)
	f.forward(t, 1325, 0)
	f.tick(t)
	cases, err = f.store.Cases(context.Background())
	require.NoError(t, err)
	active, aliases := 0, 0
	for _, c := range cases {
		if c.Canonical == "" {
			active++
			continue
		}
		aliases++
		text, keyboard := sharedViewCard(moderation.SharedView{Case: c}, f.bridges[0].config.Peers, f.now)
		require.Contains(t, text, "Используйте общую карточку")
		require.Empty(t, keyboard.InlineKeyboard)
		f.choose(t, c, "all", nil)
	}
	require.Equal(t, 1, active)
	require.LessOrEqual(t, aliases, 1)
	for _, b := range f.bridges {
		reports, e := b.admin.recovery.journal.SharedOutbox(context.Background())
		require.NoError(t, e)
		require.Len(t, reports, 2)
	}
	require.Len(t, f.fixtures[0].cards, 1)
	for _, fixture := range f.fixtures {
		require.Empty(t, fixture.banCalls)
		require.Empty(t, fixture.deleteCalls)
	}
}

func TestSharedIndependentInquiriesSwitchOneAnchorWithoutSharingAuthority(t *testing.T) {
	t.Parallel()
	runSharedIndependentInquiries(t, nil)
}

func runSharedIndependentInquiries(t *testing.T, open func(string) *engine.SQL) {
	t.Helper()
	f := newSharedFleet(t, []int{2, 2}, open)
	f.forward(t, 1324)
	f.tick(t)
	first := f.one(t)
	for _, mid := range []int{951, 952} {
		u := originalForwardUpdate(mid, 701)
		u.Message.Chat.ID = -1000
		u.Message.Text = "independent original"
		require.NoError(t, f.fixtures[0].listener.procEvents(u))
	}
	for i, b := range f.bridges {
		u := manualForwardUpdate()
		u.Message.MessageID = 1325
		u.UpdateID = 300000 + i
		u.Message.Text = "independent original"
		require.NoError(t, b.admin.MsgHandler(u))
	}
	f.tick(t)
	cases, err := f.store.Cases(context.Background())
	require.NoError(t, err)
	require.Len(t, cases, 2)
	require.Len(t, f.fixtures[0].cards, 1)
	view, found, err := f.store.View(context.Background(), 100, 456, f.now)
	require.NoError(t, err)
	require.True(t, found)
	if dir := os.Getenv("SHARED_CARD_EXAMPLES"); dir != "" {
		text, keyboard := sharedViewCard(view, f.bridges[0].config.Peers, f.now)
		raw, e := json.MarshalIndent(struct {
			Caption  string
			Keyboard tbapi.InlineKeyboardMarkup
		}{text, keyboard}, "", "  ")
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "independent-inquiries.json"), raw, 0o600))
	}
	index := -1
	var next moderation.SharedCase
	for i, item := range view.Menu {
		if item.Ref != first.Ref {
			index = i
			for _, c := range cases {
				if c.Ref == item.Ref {
					next = c
				}
			}
		}
	}
	require.NotEqual(t, -1, index)
	f.choose(t, view.Case, fmt.Sprintf("c%d", index), nil)
	f.tick(t)
	f.choose(t, first, "all", nil)
	cases, err = f.store.Cases(context.Background())
	require.NoError(t, err)
	for _, c := range cases {
		require.False(t, c.Decided)
		if c.Ref == next.Ref {
			next = c
		}
	}
	f.choose(t, next, "100", nil)
	f.tick(t)
	for _, fixture := range f.fixtures {
		fixture.settle(t)
	}
	f.tick(t)
	require.Equal(t, 1, f.fixtures[0].banCalls[701])
	require.Zero(t, f.fixtures[0].banCalls[501])
	require.Empty(t, f.fixtures[1].banCalls)
	require.Equal(t, map[int]int{951: 1, 952: 1}, f.fixtures[0].deleteCalls)
	require.Len(t, f.fixtures[0].cards, 1)
}

func TestSharedSingleOriginalAndTerminalRetry(t *testing.T) {
	t.Parallel()
	f := newSharedFleet(t, []int{1, 0}, nil)
	f.fixtures[0].failDelete = true
	f.forward(t, 1324)
	f.tick(t)
	f.tick(t)
	f.fixtures[0].settle(t)
	f.tick(t)
	c := f.one(t)
	require.True(t, c.Decided)
	require.Equal(t, 1, f.fixtures[0].banCalls[501])
	require.Equal(t, 1, f.fixtures[0].deleteCalls[800])
	deadline := c.Choices[0].Report.Targets[0].Deadline
	f.forward(t, 1325)
	f.tick(t)
	cases, err := f.store.Cases(context.Background())
	require.NoError(t, err)
	require.Len(t, cases, 2)
	var retry moderation.SharedCase
	for _, candidate := range cases {
		if candidate.Ref != c.Ref {
			retry = candidate
		}
	}
	require.False(t, retry.Decided)
	require.Equal(t, deadline, retry.Reports[0].Targets[0].Deadline)
	f.choose(t, c, "all", nil)
	require.Equal(t, 1, f.fixtures[0].deleteCalls[800])
	f.fixtures[0].failDelete = false
	f.choose(t, retry, "all", nil)
	f.tick(t)
	f.fixtures[0].settle(t)
	f.tick(t)
	require.Equal(t, 1, f.fixtures[0].banCalls[501])
	require.Equal(t, 2, f.fixtures[0].deleteCalls[800])
}

func TestSharedDelayedIntentCannotRetryARefusalItDidNotObserve(t *testing.T) {
	t.Parallel()
	runSharedDelayedIntent(t, nil)
}

func runSharedDelayedIntent(t *testing.T, open func(string) *engine.SQL) {
	t.Helper()
	for _, automatic := range []bool{true, false} {
		t.Run(fmt.Sprint(automatic), func(t *testing.T) {
			actualOpen := open
			if open != nil {
				actualOpen = func(gid string) *engine.SQL { return open(fmt.Sprint(automatic) + "-" + gid) }
			}
			f := newSharedFleet(t, []int{1, 0}, actualOpen)
			if automatic {
				f.forward(t, 1324)
			} else {
				f.forward(t, 1324, 0)
			}
			f.tick(t)
			before := f.one(t)
			if !automatic {
				// Clock skew must not turn a stale decision into observed refusal authority.
				f.bridges[0].now = func() time.Time { return f.now.Add(time.Second) }
				f.choose(t, before, "all", nil)
			}
			stale := f.one(t)
			require.True(t, stale.Decided)
			fixture := f.fixtures[0]
			fixture.failDelete = true
			p := moderation.Plan{Key: "source:-1000:800:user:501", ChatID: -1000, MessageID: 800, IdentityID: 501,
				IdentityKind: "user", Origin: "detector", Actions: []moderation.Action{
					{Kind: "ban", ChatID: -1000, IdentityID: 501, IdentityKind: "user"},
					{Kind: "original_delete", ChatID: -1000, MessageID: 800, IdentityID: 501, IdentityKind: "user", MessageDate: time.Now().Unix()},
				}}
			_, _, _, err := fixture.j.Accept(context.Background(), p, time.Now())
			require.NoError(t, err)
			fixture.settle(t)
			require.Equal(t, 1, fixture.deleteCalls[800])
			require.Equal(t, 1, fixture.banCalls[501])
			f.tick(t)
			fixture.settle(t)
			fresh := f.one(t)
			require.True(t, fresh.NeedsReview)
			require.False(t, fresh.Decided)
			require.Len(t, fresh.PriorIntents, 1)
			require.Contains(t, fresh.ReviewReason, "telegram_403")
			require.Equal(t, 1, fixture.deleteCalls[800])
			require.Equal(t, before.Reports[0].Targets[0].Deadline, fresh.Reports[0].Targets[0].Deadline)
			f.choose(t, stale, "all", nil)
			require.False(t, f.one(t).Decided)
			fixture.failDelete = false
			f.choose(t, fresh, "all", nil)
			f.tick(t)
			fixture.settle(t)
			f.tick(t)
			require.Equal(t, 2, fixture.deleteCalls[800])
			require.Equal(t, 1, fixture.banCalls[501])
			require.Len(t, fixture.cards, 1)
		})
	}
}

func TestSharedUnknownCardSendIsNeverReplayed(t *testing.T) {
	t.Parallel()
	f := newSharedFleet(t, []int{2, 0}, nil)
	base := f.fixtures[0].api.call
	sends := 0
	f.fixtures[0].api.call = func(ctx context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
		if _, ok := c.(tbapi.MessageConfig); ok {
			sends++
			return nil, io.EOF
		}
		return base(ctx, c)
	}
	f.forward(t, 1324)
	f.tick(t)
	require.Equal(t, "unknown", f.one(t).SendState)
	f.bridges[0].cache = map[string]string{}
	f.now = f.now.Add(time.Minute)
	f.tick(t)
	require.Equal(t, 1, sends)
	require.Empty(t, f.fixtures[0].banCalls)
}

func TestSharedForgedLegacyNamespacesCannotAdoptTheAnchor(t *testing.T) {
	t.Parallel()
	f := newSharedFleet(t, []int{2}, nil)
	f.forward(t, 1324)
	f.tick(t)
	c := f.one(t)
	var before int
	require.NoError(t, f.fixtures[0].db.Get(&before, `SELECT count(*) FROM moderation_receipts`))
	sends := len(f.fixtures[0].api.SendCalls())
	for _, data := range []string{"?501:800", "+501:800", "501:800", "!501:800", "M+:forged",
		"R+501:800", "R!501:800", "S:" + strings.Repeat("x", 65)} {
		q := &tbapi.CallbackQuery{ID: "forged legacy", From: &tbapi.User{ID: 11}, Data: data,
			Message: &tbapi.Message{Chat: tbapi.Chat{ID: 456}, MessageID: c.NotificationID, From: &tbapi.User{ID: 100}}}
		if strings.HasPrefix(data, "R") {
			require.NoError(t, f.fixtures[0].listener.reportsHandler.HandleReportCallback(context.Background(), q))
		} else {
			require.NoError(t, f.bridges[0].admin.InlineCallbackHandler(q))
		}
	}
	f.fixtures[0].settle(t)
	var after int
	require.NoError(t, f.fixtures[0].db.Get(&after, `SELECT count(*) FROM moderation_receipts`))
	require.Equal(t, before, after)
	require.Len(t, f.fixtures[0].api.SendCalls(), sends, "foreign callbacks cannot replace the shared keyboard")
	require.False(t, f.one(t).Decided)
	require.Empty(t, f.fixtures[0].banCalls)
	require.Empty(t, f.fixtures[0].deleteCalls)
	f.fixtures[0].noLearning(t)
}

func TestSharedConcurrentSubmitAndPersistentOwnerClaim(t *testing.T) {
	t.Parallel()
	f := newSharedFleet(t, []int{2, 2}, nil)
	f.forward(t, 1324)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for _, b := range f.bridges {
		reports, err := b.admin.recovery.journal.SharedOutbox(context.Background())
		require.NoError(t, err)
		for range 4 {
			wg.Add(1)
			go func(r moderation.SharedReport) {
				defer wg.Done()
				errs <- f.store.Submit(context.Background(), r, f.now)
			}(reports[0])
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	c := f.one(t)
	f.now = f.now.Add(3 * time.Second)
	claim, ok, err := f.store.ClaimCard(context.Background(), 100, c.Ref, "render", f.now)
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = f.store.ClaimCard(context.Background(), 100, c.Ref, "render", f.now.Add(time.Second))
	require.NoError(t, err)
	require.False(t, ok)
	_, ok, err = f.store.ClaimCard(context.Background(), 100, c.Ref, "render", f.now.Add(time.Minute))
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, "unknown", f.one(t).SendState)
	require.ErrorIs(t, f.store.FinishCard(context.Background(), claim, 123, "render", "ok", f.now), moderation.ErrLease)
}
