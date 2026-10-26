package events

import (
	"context"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/events/mocks"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/app/storage/engine"
)

type forwardTestAPI struct {
	*mocks.TbAPIMock
	call recoveryAPIFunc
}

func (a *forwardTestAPI) RequestWithContext(ctx context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
	return a.call(ctx, c)
}

type forwardFixture struct {
	db            *engine.SQL
	loc           *storage.Locator
	j             *moderation.Journal
	listener      *TelegramListener
	bot           *mocks.BotMock
	api           *forwardTestAPI
	mu            sync.Mutex
	banned        map[int64]bool
	banCalls      map[int64]int
	deleteCalls   map[int]int
	cards         map[int]string
	nextCard      int
	failBan       bool
	failDelete    bool
	unknownBan    bool
	unknownDelete bool
	currentAdmin  bool
	now           time.Time
}

func newForwardFixture(t *testing.T) *forwardFixture {
	t.Helper()
	ctx := context.Background()
	db, err := engine.NewSqlite(filepath.Join(t.TempDir(), "forward.db"), "forward-test")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	loc, err := storage.NewLocator(ctx, time.Hour, 1000, db)
	require.NoError(t, err)
	j, err := moderation.NewJournal(ctx, db, moderation.DefaultLimits())
	require.NoError(t, err)
	f := &forwardFixture{db: db, loc: loc, j: j, banned: map[int64]bool{}, banCalls: map[int64]int{}, deleteCalls: map[int]int{}, cards: map[int]string{}, nextCard: 1000, now: time.Now()}
	f.bot = &mocks.BotMock{OnMessageFunc: func(bot.Message, bool) bot.Response { return bot.Response{} }, IsApprovedUserFunc: func(int64) bool { return false }}
	f.api = &forwardTestAPI{TbAPIMock: &mocks.TbAPIMock{GetChatFunc: func(c tbapi.ChatInfoConfig) (tbapi.ChatFullInfo, error) {
		return tbapi.ChatFullInfo{Chat: tbapi.Chat{ID: c.ChatID, Title: "Fixture"}}, nil
	}, GetChatAdministratorsFunc: func(tbapi.ChatAdministratorsConfig) ([]tbapi.ChatMember, error) { return nil, nil }, SendFunc: func(c tbapi.Chattable) (tbapi.Message, error) { return tbapi.Message{MessageID: 1900}, nil }}, call: f.request}
	f.listener = &TelegramListener{TbAPI: f.api, Bot: f.bot, Locator: f.loc, Group: "123", AdminGroup: "456", BotUsername: "fixture_bot", SuperUsers: SuperUsers{"11"}, RecoveryJournal: j, AggressiveCleanup: true, AggressiveCleanupLimit: 50}
	return f
}

func (f *forwardFixture) request(_ context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch v := c.(type) {
	case tbapi.GetChatMemberConfig:
		status := "member"
		if f.banned[v.UserID] {
			status = "kicked"
		}
		if f.currentAdmin {
			status = "administrator"
		}
		return apiOK(tbapi.ChatMember{User: &tbapi.User{ID: v.UserID}, Status: status}), nil
	case tbapi.ChatInfoConfig:
		return apiOK(tbapi.ChatFullInfo{Chat: tbapi.Chat{ID: v.ChatID}}), nil
	case tbapi.BanChatMemberConfig:
		f.banCalls[v.UserID]++
		if f.unknownBan {
			f.banned[v.UserID] = true
			return nil, io.EOF
		}
		if f.failBan {
			return nil, &tbapi.Error{Code: 403, Message: "fixture forbidden"}
		}
		f.banned[v.UserID] = true
		return apiOK(true), nil
	case tbapi.RestrictChatMemberConfig:
		f.banCalls[v.UserID]++
		return apiOK(true), nil
	case tbapi.BanChatSenderChatConfig:
		f.banCalls[v.SenderChatID]++
		return apiOK(true), nil
	case tbapi.DeleteMessageConfig:
		f.deleteCalls[v.MessageID]++
		if f.failDelete {
			return nil, &tbapi.Error{Code: 403, Message: "fixture forbidden"}
		}
		if f.unknownDelete {
			return nil, io.EOF
		}
		return apiOK(true), nil
	case tbapi.MessageConfig:
		f.nextCard++
		f.cards[f.nextCard] = v.Text
		return apiOK(tbapi.Message{MessageID: f.nextCard}), nil
	case tbapi.EditMessageTextConfig:
		f.cards[v.MessageID] = v.Text
		return apiOK(tbapi.Message{MessageID: v.MessageID}), nil
	case tbapi.CallbackConfig:
		return apiOK(true), nil
	default:
		return nil, fmt.Errorf("unexpected Telegram mock request %T", c)
	}
}

func originalForwardUpdate(mid int, author int64) tbapi.Update {
	m := &tbapi.Message{Chat: tbapi.Chat{ID: 123}, MessageID: mid, From: &tbapi.User{ID: author}, Text: "same fixture original", Date: int(time.Now().Unix())}
	if author < 0 {
		m.SenderChat = &tbapi.Chat{ID: author}
		m.From = &tbapi.User{ID: 136817688, IsBot: true}
	}
	return tbapi.Update{UpdateID: mid + 10000, Message: m}
}
func manualForwardUpdate() tbapi.Update {
	return tbapi.Update{UpdateID: 20000, Message: &tbapi.Message{Chat: tbapi.Chat{ID: 456}, MessageID: 900, From: &tbapi.User{ID: 11}, Text: "same fixture original", ForwardOrigin: &tbapi.MessageOrigin{Type: "hidden_user", SenderUserName: "Untrusted name"}}}
}

// Exercise the real listener routing and source recorder, not direct enqueue helpers.
func (f *forwardFixture) listen(t *testing.T, updates ...tbapi.Update) {
	t.Helper()
	ch := make(chan tbapi.Update, len(updates))
	for _, u := range updates {
		ch <- u
	}
	close(ch)
	f.api.GetUpdatesChanFunc = func(tbapi.UpdateConfig) tbapi.UpdatesChannel { return ch }
	err := f.listener.Do(context.Background())
	require.EqualError(t, err, "telegram update chan closed")
}
func (f *forwardFixture) settle(t *testing.T) {
	t.Helper()
	if current := time.Now().Add(time.Second); current.After(f.now) {
		f.now = current
	}
	w := moderation.Worker{Journal: f.j, Executor: f.listener.adminHandler.recovery, Now: func() time.Time { return f.now }, Jitter: func() float64 { return 0 }}
	for range 100 {
		worked, err := w.Step(context.Background())
		require.NoError(t, err)
		if !worked {
			return
		}
	}
	t.Fatal("worker did not settle")
}
func (f *forwardFixture) card(t *testing.T) moderation.Card {
	t.Helper()
	var id string
	require.NoError(t, f.db.Get(&id, "SELECT receipt FROM moderation_forward_batches LIMIT 1"))
	c, err := f.j.CardView(context.Background(), id)
	require.NoError(t, err)
	return c
}
func (f *forwardFixture) choose(t *testing.T, verb string, alter func(*tbapi.CallbackQuery)) {
	t.Helper()
	c := f.card(t)
	var anchor int
	require.NoError(t, f.db.Get(&anchor, "SELECT notification_id FROM moderation_actions WHERE kind='card' AND receipt=?", c.Receipt))
	require.Positive(t, anchor)
	q := &tbapi.CallbackQuery{ID: "fixture-callback", From: &tbapi.User{ID: 11}, Data: verb + ":" + c.Ref, Message: &tbapi.Message{Chat: tbapi.Chat{ID: 456}, MessageID: anchor, Text: "forged text and identity 999"}}
	if alter != nil {
		alter(q)
	}
	err := f.listener.adminHandler.InlineCallbackHandler(q)
	if alter == nil {
		require.NoError(t, err)
	}
}
func (f *forwardFixture) noLearning(t *testing.T) {
	t.Helper()
	require.Empty(t, f.bot.UpdateSpamCalls())
	require.Empty(t, f.bot.UpdateHamCalls())
	require.Empty(t, f.bot.RemoveApprovedUserCalls())
	require.Empty(t, f.bot.AddApprovedUserCalls())
	rows, err := f.j.Snapshot(context.Background())
	require.NoError(t, err)
	for _, a := range rows {
		require.NotEqual(t, "cleanup_delete", a.Kind)
	}
}

func TestAdminForwardListenerJournalWorker(t *testing.T) {
	for _, author := range []int64{501, -501} {
		t.Run(fmt.Sprint(author), func(t *testing.T) {
			f := newForwardFixture(t)
			f.listen(t, originalForwardUpdate(801, author), manualForwardUpdate())
			f.settle(t)
			require.Equal(t, 1, f.banCalls[author])
			require.Equal(t, map[int]int{801: 1}, f.deleteCalls)
			c := f.card(t)
			require.Equal(t, "accepted", c.Forward.State)
			text := forwardCardText(c, "Fixture")
			require.Contains(t, text, "Принято: удалить сообщение №801 и заблокировать автора")
			require.Contains(t, text, "Блокировка: подтверждена")
			require.Contains(t, text, "Удаление: подтверждено")
			before := c.Actions
			restarted, err := moderation.NewJournal(context.Background(), f.db, moderation.DefaultLimits())
			require.NoError(t, err)
			f.j = restarted
			f.listener.RecoveryJournal = restarted
			f.listen(t, manualForwardUpdate())
			f.settle(t)
			require.Equal(t, 1, f.banCalls[author])
			require.Equal(t, map[int]int{801: 1}, f.deleteCalls)
			require.Equal(t, before, f.card(t).Actions)
			f.noLearning(t)
		})
	}
}

func TestAdminForwardMultipleOriginals(t *testing.T) {
	for _, authors := range [][]int64{{501, 501}, {501, 502}} {
		for _, verb := range []string{"MF0", "MFA"} {
			t.Run(fmt.Sprint(authors)+verb, func(t *testing.T) {
				f := newForwardFixture(t)
				f.listen(t, originalForwardUpdate(801, authors[0]), originalForwardUpdate(802, authors[1]), manualForwardUpdate())
				f.settle(t)
				require.Empty(t, f.banCalls)
				require.Empty(t, f.deleteCalls)
				c := f.card(t)
				require.Contains(t, forwardCardText(c, "Fixture"), "Найдены 2 разных сообщения. Выберите одно или обработайте оба")
				f.choose(t, verb, nil)
				f.choose(t, verb, nil)
				f.settle(t)
				require.Equal(t, 1, f.banCalls[501])
				require.Equal(t, 1, f.deleteCalls[801])
				if verb == "MFA" {
					require.Equal(t, 1, f.deleteCalls[802])
					if authors[1] != 501 {
						require.Equal(t, 1, f.banCalls[502])
					}
				} else {
					require.Zero(t, f.deleteCalls[802])
				}
				f.noLearning(t)
			})
		}
	}
}

func TestAdminForwardUnknownAuthorAndMiss(t *testing.T) {
	t.Run("legacy author is not provenance", func(t *testing.T) {
		f := newForwardFixture(t)
		require.NoError(t, f.loc.AddMessage(context.Background(), "same fixture original", 123, 501, "misleading saved name", 801))
		require.NoError(t, f.loc.RecordMessageDate(context.Background(), 123, 801, time.Now().Unix()))
		f.listen(t, manualForwardUpdate())
		f.settle(t)
		require.Empty(t, f.banCalls)
		require.Equal(t, 1, f.deleteCalls[801])
		require.Contains(t, forwardCardText(f.card(t), "Fixture"), "нет надёжной числовой цели")
		f.noLearning(t)
	})
	t.Run("locator miss is quiet but addressable", func(t *testing.T) {
		f := newForwardFixture(t)
		f.listen(t, manualForwardUpdate())
		f.settle(t)
		require.Empty(t, f.banCalls)
		require.Empty(t, f.deleteCalls)
		require.Empty(t, f.api.SendCalls())
		u := manualForwardUpdate()
		u.Message.MessageID = 901
		u.Message.ReplyToMessage = manualForwardUpdate().Message
		u.Message.ForwardOrigin = nil
		u.Message.Text = "/forward_status@fixture_bot"
		f.listen(t, u)
		require.Len(t, f.api.SendCalls(), 1)
		require.Contains(t, f.api.SendCalls()[0].C.(tbapi.MessageConfig).Text, "не найден")
		f.noLearning(t)
	})
}

func TestAdminForwardGatesAndSpoofing(t *testing.T) {
	for _, gate := range []string{"outsider", "username-only", "bot", "sender-chat", "plain", "service-forward", "forwarded-bot-card", "dry", "training", "superuser", "manual-trust", "admin-target", "other-chat", "operator-revoked"} {
		t.Run(gate, func(t *testing.T) {
			f := newForwardFixture(t)
			u := manualForwardUpdate()
			orig := originalForwardUpdate(801, 501)
			switch gate {
			case "outsider":
				u.Message.From.ID = 12
			case "username-only":
				u.Message.From.ID = 12
				u.Message.From.UserName = "11"
			case "forwarded-bot-card":
				u.Message.ForwardOrigin = &tbapi.MessageOrigin{Type: "user", SenderUser: &tbapi.User{ID: 501, IsBot: true}}
			case "bot":
				u.Message.From.IsBot = true
			case "sender-chat":
				u.Message.SenderChat = &tbapi.Chat{ID: -88}
			case "plain":
				u.Message.ForwardOrigin = nil
			case "service-forward":
				u.Message.NewChatTitle = "service"
				u.Message.ForwardOrigin = nil
			case "dry":
				f.listener.Dry = true
			case "training":
				f.listener.TrainingMode = true
			case "superuser":
				f.listener.SuperUsers = SuperUsers{"11", "501"}
			case "manual-trust":
				f.listener.Bot = &forwardTrustedBot{BotMock: f.bot}
			case "admin-target":
				f.currentAdmin = true
			case "other-chat":
				orig.Message.Chat.ID = 999
			}
			f.listen(t, orig, u)
			if gate == "operator-revoked" {
				f.listener.SuperUsers = SuperUsers{}
				f.listener.adminHandler.superUsers = SuperUsers{}
			}
			f.settle(t)
			require.Empty(t, f.banCalls)
			require.Empty(t, f.deleteCalls)
			f.noLearning(t)
		})
	}
	for _, attack := range []string{"card", "chat", "operator", "bot", "out-of-set", "ban-escalation"} {
		t.Run("callback-"+attack, func(t *testing.T) {
			f := newForwardFixture(t)
			f.listen(t, originalForwardUpdate(801, 501), originalForwardUpdate(802, 502), manualForwardUpdate())
			f.settle(t)
			verb := "MFA"
			if attack == "out-of-set" {
				verb = "MF20"
			}
			if attack == "ban-escalation" {
				verb = "M+"
			}
			f.choose(t, verb, func(q *tbapi.CallbackQuery) {
				switch attack {
				case "card":
					q.Message.MessageID++
				case "chat":
					q.Message.Chat.ID++
				case "operator":
					q.From.ID = 12
				case "bot":
					q.From.IsBot = true
				}
			})
			f.settle(t)
			require.Empty(t, f.banCalls)
			require.Empty(t, f.deleteCalls)
			f.noLearning(t)
		})
	}
}

type forwardTrustedBot struct{ *mocks.BotMock }

func (*forwardTrustedBot) IsExplicitTrustedUser(int64) bool { return true }

func TestAdminForwardPartialDeadlineAndOldRequests(t *testing.T) {
	for _, which := range []string{"ban", "delete", "unknown", "deadline"} {
		t.Run(which, func(t *testing.T) {
			f := newForwardFixture(t)
			f.failBan = which == "ban"
			f.failDelete = which == "delete"
			f.unknownDelete = which == "unknown"
			orig := originalForwardUpdate(801, 501)
			if which == "deadline" {
				orig.Message.Date = int(time.Now().Add(-49 * time.Hour).Unix())
			}
			f.listen(t, orig, manualForwardUpdate())
			f.settle(t)
			c := f.card(t)
			if which == "ban" {
				require.Equal(t, 1, f.deleteCalls[801])
				require.Contains(t, forwardCardText(c, "Fixture"), "telegram_403")
			}
			if which == "delete" {
				require.Equal(t, 1, f.banCalls[501])
				require.Contains(t, forwardCardText(c, "Fixture"), "telegram_403")
			}
			if which == "unknown" {
				require.Equal(t, 1, f.banCalls[501])
				require.Contains(t, forwardCardText(c, "Fixture"), "результат API неизвестен")
				for _, a := range c.Actions {
					if a.Kind == "original_delete" {
						require.True(t, a.Uncertain)
						require.Equal(t, moderation.Pending, a.State)
					}
				}
			}
			if which == "deadline" {
				require.Empty(t, f.deleteCalls)
				require.Equal(t, 1, f.banCalls[501])
			}
			f.noLearning(t)
		})
	}
	t.Run("old delete-only is never upgraded", func(t *testing.T) {
		a, db, u := originalDeleteFixture(t, 501)
		require.NoError(t, a.legacyForward(u))
		j := a.recovery.journal
		var before int
		require.NoError(t, db.Get(&before, "SELECT count(*) FROM moderation_actions WHERE kind='ban'"))
		require.Zero(t, before)
		require.NoError(t, a.MsgHandler(u))
		rows, err := j.Snapshot(context.Background())
		require.NoError(t, err)
		for _, op := range rows {
			require.NotEqual(t, "ban", op.Kind)
		}
		c, err := j.CardView(context.Background(), "delete:123:801")
		require.NoError(t, err)
		require.True(t, c.DeleteOnly)
		require.Equal(t, "pending", c.RequestState)
	})
}

func TestAdminForwardExpiryAndConcurrentChoice(t *testing.T) {
	f := newForwardFixture(t)
	f.listen(t, originalForwardUpdate(801, 501), originalForwardUpdate(802, 501), manualForwardUpdate())
	f.settle(t)
	c := f.card(t)
	var anchor int
	require.NoError(t, f.db.Get(&anchor, "SELECT notification_id FROM moderation_actions WHERE receipt=? AND kind='card'", c.Receipt))
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			fresh, err := f.j.DecideForward(context.Background(), c.Ref, 456, anchor, 11, []int{0, 1}, time.Now())
			results <- fresh
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	count := 0
	for fresh := range results {
		if fresh {
			count++
		}
	}
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, 1, count)
	f.settle(t)
	require.Equal(t, 1, f.banCalls[501])
	require.Equal(t, map[int]int{801: 1, 802: 1}, f.deleteCalls)
	expired := newForwardFixture(t)
	expired.listen(t, originalForwardUpdate(801, 501), originalForwardUpdate(802, 502), manualForwardUpdate())
	expired.settle(t)
	_, err := expired.db.Exec("UPDATE moderation_forward_batches SET expires_at=?", time.Now().Add(-time.Minute).UnixMilli())
	require.NoError(t, err)
	c = expired.card(t)
	require.Contains(t, forwardCardText(c, "Fixture"), "Срок выбора истёк")
	require.Empty(t, forwardKeyboard(c).InlineKeyboard)
	expired.choose(t, "MFA", nil)
	expired.settle(t)
	require.Empty(t, expired.banCalls)
	require.Empty(t, expired.deleteCalls)
}

// These examples are emitted by the real renderer, for review, using synthetic data only.
func TestAdminForwardRendererExamples(t *testing.T) {
	dir := os.Getenv("FORWARD_CARD_EXAMPLES")
	if dir == "" {
		t.Skip("set FORWARD_CARD_EXAMPLES to export renderer examples")
	}
	require.NoError(t, os.MkdirAll(dir, 0o700))
	for _, mode := range []string{"false", "true", "unknown", "partial", "expired"} {
		f := newForwardFixture(t)
		updates := []tbapi.Update{originalForwardUpdate(801, 501)}
		if mode == "true" || mode == "expired" {
			updates = append(updates, originalForwardUpdate(802, 502))
		}
		if mode == "partial" {
			f.failBan = true
		}
		if mode == "unknown" {
			updates = nil
			require.NoError(t, f.loc.AddMessage(context.Background(), "same fixture original", 123, 501, "legacy", 801))
		}
		updates = append(updates, manualForwardUpdate())
		f.listen(t, updates...)
		f.settle(t)
		if mode == "expired" {
			_, err := f.db.Exec("UPDATE moderation_forward_batches SET expires_at=?", time.Now().Add(-time.Minute).UnixMilli())
			require.NoError(t, err)
		}
		c := f.card(t)
		var buttons strings.Builder
		for _, row := range forwardKeyboard(c).InlineKeyboard {
			for _, button := range row {
				fmt.Fprintf(&buttons, "<button disabled>%s</button>", html.EscapeString(button.Text))
			}
			buttons.WriteString("<br>")
		}
		page := "<!doctype html><meta charset=utf-8><title>Renderer fixture</title><style>body{font:16px system-ui;max-width:720px;margin:32px auto}pre{white-space:pre-wrap}button{margin:4px;padding:8px}</style><pre>" + forwardCardText(c, "Тестовый чат") + "</pre>" + buttons.String()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "forward-"+mode+".html"), []byte(page), 0o600))
	}
}

func TestAdminForwardReplayDeadlineAndOneCard(t *testing.T) {
	f := newForwardFixture(t)
	f.listen(t, originalForwardUpdate(801, -501), manualForwardUpdate())
	f.settle(t)
	c := f.card(t)
	var firstDeadline int64
	for _, a := range c.Actions {
		if a.Kind == "original_delete" {
			firstDeadline = a.Deadline
		}
	}
	f.now = f.now.Add(2 * time.Minute)
	f.settle(t)
	require.Len(t, f.cards, 1, "results edit the initial card instead of emitting another card or digest")
	for _, text := range f.cards {
		require.Contains(t, text, "Удаление: подтверждено")
	}
	u := manualForwardUpdate()
	u.Message.From.ID = 12
	u.Message.MessageID++
	f.listener.SuperUsers = SuperUsers{"11", "12"}
	f.listen(t, u)
	f.settle(t)
	require.Equal(t, 1, f.banCalls[-501])
	require.Equal(t, 1, f.deleteCalls[801])
	for _, a := range f.card(t).Actions {
		if a.Kind == "original_delete" {
			require.Equal(t, firstDeadline, a.Deadline)
		}
	}
	// Different candidate sets must also reuse confirmed SenderChat membership facts.
	f.listen(t, originalForwardUpdate(802, -501), manualForwardUpdate())
	f.settle(t)
	var ref string
	var anchor int
	require.NoError(t, f.db.Get(&ref, "SELECT c.ref FROM moderation_cards c JOIN moderation_forward_batches b ON b.receipt=c.receipt WHERE b.state='pending'"))
	require.NoError(t, f.db.Get(&anchor, "SELECT a.notification_id FROM moderation_actions a JOIN moderation_cards c ON a.receipt=c.receipt WHERE c.ref=? AND a.kind='card'", ref))
	fresh, err := f.j.DecideForward(context.Background(), ref, 456, anchor, 11, []int{0, 1}, time.Now())
	require.NoError(t, err)
	require.True(t, fresh)
	f.settle(t)
	require.Equal(t, 1, f.banCalls[-501])
	require.Equal(t, map[int]int{801: 1, 802: 1}, f.deleteCalls)
	f.noLearning(t)
}

func TestAdminForwardSoftBanAndReadOnlyPages(t *testing.T) {
	f := newForwardFixture(t)
	f.listener.SoftBanMode = true
	updates := []tbapi.Update{}
	for mid := 801; mid <= 807; mid++ {
		updates = append(updates, originalForwardUpdate(mid, 501))
	}
	updates = append(updates, manualForwardUpdate())
	f.listen(t, updates...)
	f.settle(t)
	f.choose(t, "MFA", nil)
	f.settle(t)
	c := f.card(t)
	require.Equal(t, 1, f.banCalls[501])
	require.Len(t, f.deleteCalls, 7)
	for _, a := range c.Actions {
		require.NotEqual(t, "ban", a.Kind)
	}
	before, err := f.j.Snapshot(context.Background())
	require.NoError(t, err)
	f.choose(t, "MFV1", nil)
	after, err := f.j.Snapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, before, after, "page navigation cannot modify journal authority")
	require.Contains(t, forwardCardPage(c, "Fixture", 1), "№807")
	require.NotContains(t, forwardCardPage(c, "Fixture", 1), "№801")
}
func TestAdminForwardUnknownBanReconcilesAfterRestart(t *testing.T) {
	f := newForwardFixture(t)
	f.unknownBan = true
	f.listen(t, originalForwardUpdate(801, 501), manualForwardUpdate())
	f.settle(t)
	require.Equal(t, 1, f.deleteCalls[801])
	require.Equal(t, 1, f.banCalls[501])
	c := f.card(t)
	for _, a := range c.Actions {
		if a.Kind == "ban" {
			require.True(t, a.Uncertain)
			require.Equal(t, moderation.Pending, a.State)
		}
	}
	restarted, err := moderation.NewJournal(context.Background(), f.db, moderation.DefaultLimits())
	require.NoError(t, err)
	f.j = restarted
	f.listener.adminHandler.recovery.journal = restarted
	f.now = f.now.Add(3 * time.Second)
	f.settle(t)
	for _, a := range f.card(t).Actions {
		if a.Kind == "ban" {
			require.Equal(t, moderation.Confirmed, a.State)
			require.Equal(t, "reconciled_membership", a.Code)
		}
	}
	require.Equal(t, 1, f.banCalls[501])
	require.Equal(t, 1, f.deleteCalls[801])
	f.noLearning(t)
}

func TestAdminForwardDeleteRetryIndependentOfFailedBan(t *testing.T) {
	f := newForwardFixture(t)
	f.failBan = true
	f.unknownDelete = true
	f.listen(t, originalForwardUpdate(801, 501), manualForwardUpdate())
	f.settle(t)
	require.Equal(t, 1, f.banCalls[501])
	require.Equal(t, 1, f.deleteCalls[801])
	f.unknownDelete = false
	f.now = f.now.Add(3 * time.Second)
	f.settle(t)
	require.Equal(t, 1, f.banCalls[501])
	require.Equal(t, 2, f.deleteCalls[801])
	for _, a := range f.card(t).Actions {
		switch a.Kind {
		case "ban":
			require.Equal(t, moderation.Failed, a.State)
		case "original_delete":
			require.Equal(t, moderation.Confirmed, a.State)
		}
	}
}
func TestAdminForwardOpenOriginAndExplicitDifferentAuthors(t *testing.T) {
	t.Run("open origin supplies independent numeric proof for legacy row", func(t *testing.T) {
		f := newForwardFixture(t)
		require.NoError(t, f.loc.AddMessage(context.Background(), "same fixture original", 123, 501, "not authority", 801))
		u := manualForwardUpdate()
		u.Message.ForwardOrigin = &tbapi.MessageOrigin{Type: "user", SenderUser: &tbapi.User{ID: 501}}
		f.listen(t, u)
		f.settle(t)
		require.Equal(t, 1, f.banCalls[501])
		require.Equal(t, 1, f.deleteCalls[801])
		require.Equal(t, "telegram_forward_origin", f.card(t).Forward.Targets[0].Evidence)
		evidence, err := f.loc.MessageAuthor(context.Background(), 123, 801)
		require.NoError(t, err)
		require.Empty(t, evidence.Source)
		f.noLearning(t)
	})
	t.Run("explicit set chooses independent saved authors", func(t *testing.T) {
		f := newForwardFixture(t)
		u := manualForwardUpdate()
		u.Message.ForwardOrigin = &tbapi.MessageOrigin{Type: "user", SenderUser: &tbapi.User{ID: 501}}
		f.listen(t, originalForwardUpdate(801, 501), originalForwardUpdate(802, 502), u)
		f.settle(t)
		require.Empty(t, f.banCalls)
		f.choose(t, "MFA", nil)
		f.settle(t)
		require.Equal(t, map[int64]int{501: 1, 502: 1}, f.banCalls)
		require.Equal(t, map[int]int{801: 1, 802: 1}, f.deleteCalls)
		f.noLearning(t)
	})
	t.Run("unique original with contradictory numeric evidence is refused", func(t *testing.T) {
		f := newForwardFixture(t)
		u := manualForwardUpdate()
		u.Message.ForwardOrigin = &tbapi.MessageOrigin{Type: "user", SenderUser: &tbapi.User{ID: 502}}
		f.listen(t, originalForwardUpdate(801, 501), u)
		f.settle(t)
		require.Empty(t, f.banCalls)
		require.Empty(t, f.deleteCalls)
		require.Contains(t, forwardCardText(f.card(t), "Fixture"), "не совпадает")
	})
}

func TestAdminForwardConcurrentWorkers(t *testing.T) {
	f := newForwardFixture(t)
	f.listen(t, originalForwardUpdate(801, -501), manualForwardUpdate())
	f.now = time.Now().Add(time.Second)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			w := moderation.Worker{Journal: f.j, Executor: f.listener.adminHandler.recovery, Now: func() time.Time { return f.now }}
			for range 20 {
				worked, err := w.Step(context.Background())
				if err != nil {
					errs <- err
					return
				}
				if !worked {
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	f.settle(t)
	require.Equal(t, map[int64]int{-501: 1}, f.banCalls)
	require.Equal(t, map[int]int{801: 1}, f.deleteCalls)
	f.noLearning(t)
}
func TestAdminForwardConflictingSourceCannotBeRepairedByNameOrForward(t *testing.T) {
	f := newForwardFixture(t)
	f.listen(t, originalForwardUpdate(801, 501))
	require.NoError(t, f.loc.RecordMessageAuthor(context.Background(), 123, 801, storage.MessageAuthor{Kind: "user", ID: 502, Source: "telegram_update", UpdateID: 30000}))
	u := manualForwardUpdate()
	u.Message.ForwardOrigin = &tbapi.MessageOrigin{Type: "user", SenderUser: &tbapi.User{ID: 501}}
	f.listen(t, u)
	f.settle(t)
	require.Empty(t, f.banCalls)
	require.Empty(t, f.deleteCalls)
	require.Equal(t, "source_author_conflict", f.card(t).Forward.Targets[0].Blocked)
}
