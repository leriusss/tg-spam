package events

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf8"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/go-pkgz/testutils/containers"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage/engine"
)

func TestForwardOperatorSQLite(t *testing.T) {
	runForwardOperatorScenarios(t, func(t *testing.T) (*forwardFixture, func() *engine.SQL) {
		path := filepath.Join(t.TempDir(), "roles.db")
		open := func() *engine.SQL { db, err := engine.NewSqlite(path, "roles"); require.NoError(t, err); return db }
		return newForwardFixtureDB(t, open()), open
	})
}

func TestForwardOperatorPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL runs in full CI")
	}
	pg := containers.NewPostgresTestContainerWithDB(context.Background(), t, "forward_roles")
	t.Cleanup(func() { pg.Close(context.Background()) })
	next := 0
	runForwardOperatorScenarios(t, func(t *testing.T) (*forwardFixture, func() *engine.SQL) {
		next++
		gid := fmt.Sprintf("roles-%d", next)
		open := func() *engine.SQL {
			db, err := engine.NewPostgres(context.Background(), pg.ConnectionString(), gid)
			require.NoError(t, err)
			return db
		}
		return newForwardFixtureDB(t, open()), open
	})
}

func runForwardOperatorScenarios(t *testing.T, fixture func(*testing.T) (*forwardFixture, func() *engine.SQL)) {
	for _, role := range []string{"creator", "administrator"} {
		t.Run("admin-chat-"+role+"-without-superuser", func(t *testing.T) {
			f, _ := fixture(t)
			f.operatorRole = role
			f.listener.SuperUsers = nil
			f.listen(t, originalForwardUpdate(801, 501), manualForwardUpdate())
			f.settle(t)
			require.Equal(t, map[int64]int{501: 1}, f.banCalls)
			require.Equal(t, map[int]int{801: 1}, f.deleteCalls)
			require.GreaterOrEqual(t, f.operatorChecks, 5)
			require.False(t, f.listener.SuperUsers.IsSuper("", 11))
			f.noLearning(t)
		})
	}
	for _, scenario := range []string{"member", "restricted", "left", "kicked", "administrator-other-chat", "bot", "sender-chat", "technical-from"} {
		t.Run("deny-"+scenario, func(t *testing.T) {
			f, _ := fixture(t)
			f.listener.SuperUsers = SuperUsers{"11", "12"}
			u := manualForwardUpdate()
			switch scenario {
			case "administrator-other-chat":
				u.Message.From.ID = 12 // mock confirms role only in a different chat
			case "bot":
				u.Message.From.IsBot = true
			case "sender-chat":
				u.Message.SenderChat = &tbapi.Chat{ID: -99}
			case "technical-from":
				u.Message.From.ID = 136817688
			default:
				f.operatorRole = scenario
			}
			if scenario == "administrator-other-chat" {
				base := f.api.call
				f.api.call = func(ctx context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
					if m, ok := c.(tbapi.GetChatMemberConfig); ok && m.ChatID == 123 && m.UserID == 12 {
						return apiOK(tbapi.ChatMember{User: &tbapi.User{ID: 12}, Status: "administrator"}), nil
					}
					return base(ctx, c)
				}
			}
			f.listen(t, originalForwardUpdate(801, 501), u)
			f.settle(t)
			require.Empty(t, f.banCalls)
			require.Empty(t, f.deleteCalls)
			require.Zero(t, f.count(t, "moderation_forward_batches"))
			f.noLearning(t)
		})
	}

	// Multi-original admission creates a choice but no API work, so each later
	// boundary can be exercised deterministically after the listener has stopped.
	choice := func(t *testing.T) (*forwardFixture, func() *engine.SQL) {
		f, open := fixture(t)
		f.listener.SuperUsers = nil
		f.listen(t, originalForwardUpdate(801, 501), originalForwardUpdate(802, 502), manualForwardUpdate())
		f.settle(t)
		return f, open
	}
	t.Run("revoked-before-choice-and-restored-explicitly", func(t *testing.T) {
		f, _ := choice(t)
		c := f.card(t)
		f.operatorRole = "member"
		f.chooseSaved(t, c, "MF0")
		f.settle(t)
		require.Equal(t, "pending", f.card(t).Forward.State)
		require.Empty(t, f.banCalls)
		require.Empty(t, f.deleteCalls)
		f.operatorRole = "administrator"
		f.chooseSaved(t, c, "MF0")
		f.settle(t)
		require.Equal(t, map[int64]int{501: 1}, f.banCalls)
		f.noLearning(t)
	})
	t.Run("revoked-before-worker-persists-across-restart-and-replay", func(t *testing.T) {
		f, open := choice(t)
		c := f.card(t)
		f.chooseSaved(t, c, "MF0")
		f.operatorRole = "member"
		f.reopen(t, open)
		f.listen(t, manualForwardUpdate())
		f.settle(t)
		require.Empty(t, f.banCalls)
		require.Empty(t, f.deleteCalls)
		for _, a := range f.card(t).Actions {
			if a.Kind != "card" {
				require.Equal(t, moderation.Blocked, a.State)
				require.Equal(t, "forward_operator_revoked", a.Code)
			}
		}
		f.operatorRole = "administrator"
		f.listen(t, manualForwardUpdate())
		f.chooseSaved(t, c, "MF0")
		f.settle(t)
		require.Empty(t, f.banCalls)
		require.Empty(t, f.deleteCalls)
		f.noLearning(t)
	})
	t.Run("revoked-during-target-lookups-before-dispatch", func(t *testing.T) {
		f, _ := choice(t)
		f.chooseSaved(t, f.card(t), "MF0")
		base := f.api.call
		f.api.call = func(ctx context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
			resp, err := base(ctx, c)
			if m, ok := c.(tbapi.GetChatMemberConfig); ok && m.ChatID == 123 {
				f.operatorRole = "member"
			}
			return resp, err
		}
		f.settle(t)
		require.Empty(t, f.banCalls)
		require.Empty(t, f.deleteCalls)
		f.noLearning(t)
	})
	t.Run("already-sent-acknowledged-request-remains-confirmed", func(t *testing.T) {
		f, _ := choice(t)
		f.chooseSaved(t, f.card(t), "MF0")
		base := f.api.call
		f.api.call = func(ctx context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
			resp, err := base(ctx, c)
			switch c.(type) {
			case tbapi.BanChatMemberConfig, tbapi.DeleteMessageConfig:
				f.operatorRole = "member"
			}
			return resp, err
		}
		f.settle(t)
		confirmed, blocked := 0, 0
		for _, a := range f.card(t).Actions {
			if a.Kind == "card" {
				continue
			}
			switch a.State {
			case moderation.Confirmed:
				confirmed++
			case moderation.Blocked:
				blocked++
			}
			require.NotEqual(t, moderation.Canceled, a.State)
		}
		require.Equal(t, 1, confirmed)
		require.Equal(t, 1, blocked)
		f.noLearning(t)
	})
	t.Run("role-check-error-at-choice-keeps-choice", func(t *testing.T) {
		f, _ := choice(t)
		c := f.card(t)
		f.operatorError = io.EOF
		f.chooseSaved(t, c, "MF0")
		f.settle(t)
		require.Equal(t, "pending", f.card(t).Forward.State)
		require.Empty(t, f.banCalls)
		f.operatorError = nil
		f.chooseSaved(t, c, "MF0")
		f.settle(t)
		require.Equal(t, 1, f.banCalls[501])
		f.noLearning(t)
	})
	t.Run("role-check-error-at-worker-retries-after-database-reopen", func(t *testing.T) {
		f, open := choice(t)
		f.chooseSaved(t, f.card(t), "MF0")
		f.operatorError = io.EOF
		f.settle(t)
		require.Empty(t, f.banCalls)
		require.Empty(t, f.deleteCalls)
		for _, a := range f.card(t).Actions {
			if a.Kind != "card" {
				require.Equal(t, moderation.Pending, a.State)
				require.Equal(t, "forward_operator_role_unavailable", a.Code)
				require.False(t, a.Uncertain)
			}
		}
		require.Contains(t, forwardCardText(f.card(t), "Fixture"), "временно не удалось проверить права")
		f.reopen(t, open)
		f.operatorError = nil
		f.listen(t, manualForwardUpdate())
		f.now = f.now.Add(2 * time.Minute)
		f.settle(t)
		require.Equal(t, map[int64]int{501: 1}, f.banCalls)
		require.Equal(t, map[int]int{801: 1}, f.deleteCalls)
		f.listen(t, manualForwardUpdate())
		f.settle(t)
		require.Equal(t, 1, f.banCalls[501])
		f.noLearning(t)
	})
	t.Run("role-check-error-at-admission-accepts-no-actions", func(t *testing.T) {
		f, _ := fixture(t)
		f.operatorError = io.EOF
		f.listen(t, originalForwardUpdate(801, 501), manualForwardUpdate())
		f.settle(t)
		require.Zero(t, f.count(t, "moderation_forward_batches"))
		require.Empty(t, f.banCalls)
		require.Empty(t, f.deleteCalls)
		require.Contains(t, f.api.SendCalls()[0].C.(tbapi.MessageConfig).Text, "Права не подтверждены")
		f.noLearning(t)
	})
	t.Run("changed-admin-chat-cannot-borrow-saved-authority", func(t *testing.T) {
		f, _ := choice(t)
		f.chooseSaved(t, f.card(t), "MF0")
		f.listener.AdminGroup = "789"
		f.listen(t)
		f.settle(t)
		require.Empty(t, f.banCalls)
		require.Empty(t, f.deleteCalls)
		for _, action := range f.card(t).Actions {
			if action.Kind != "card" {
				require.Equal(t, moderation.Blocked, action.State)
				require.Equal(t, "forward_operator_destination_changed", action.Code)
			}
		}
		f.noLearning(t)
	})
	t.Run("uncertain-sent-request-is-not-canceled-by-revocation", func(t *testing.T) {
		f, _ := choice(t)
		f.chooseSaved(t, f.card(t), "MF0")
		f.unknownBan = true
		base := f.api.call
		f.api.call = func(ctx context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
			resp, err := base(ctx, c)
			if _, ok := c.(tbapi.BanChatMemberConfig); ok {
				f.operatorRole = "member"
			}
			return resp, err
		}
		f.settle(t)
		f.now = f.now.Add(2 * time.Minute)
		f.settle(t)
		for _, a := range f.card(t).Actions {
			if a.Kind == "ban" {
				require.Equal(t, moderation.Manual, a.State)
				require.True(t, a.Uncertain)
				require.Equal(t, "forward_operator_revoked_outcome_unknown", a.Code)
			}
		}
		require.Equal(t, 1, f.banCalls[501])
		f.noLearning(t)
	})
	t.Run("target-superuser-and-manual-trust-remain-protected", func(t *testing.T) {
		f, _ := fixture(t)
		f.listener.SuperUsers = SuperUsers{"501"}
		f.listen(t, originalForwardUpdate(801, 501), manualForwardUpdate())
		f.settle(t)
		require.Empty(t, f.banCalls)
		require.Empty(t, f.deleteCalls)
		f.noLearning(t)
	})
}

func TestForwardOperatorRoleRejectsInvalidTelegramProof(t *testing.T) {
	for _, code := range []string{"forward_operator_revoked", "forward_operator_role_unavailable"} {
		require.LessOrEqual(t, utf8.RuneCountInString(forwardOperatorFeedback(code)), 200, "Telegram callback text limit")
	}
	for _, role := range []tbapi.ChatMember{
		{User: &tbapi.User{ID: 12}, Status: "administrator"},
		{User: &tbapi.User{ID: 11, IsBot: true}, Status: "administrator"},
		{Status: "creator"}, {User: &tbapi.User{ID: 11}, Status: "new_unknown_role"},
	} {
		api := recoveryAPIFunc(func(_ context.Context, c tbapi.Chattable) (*tbapi.APIResponse, error) {
			m := c.(tbapi.GetChatMemberConfig)
			require.Equal(t, int64(456), m.ChatID)
			require.Equal(t, int64(11), m.UserID)
			return apiOK(role), nil
		})
		require.Equal(t, "forward_operator_role_unavailable", forwardOperatorRole(context.Background(), api, 456, 11))
	}
}
