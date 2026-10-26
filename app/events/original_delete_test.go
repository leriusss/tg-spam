package events

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/events/mocks"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage"
	"github.com/umputun/tg-spam/app/storage/engine"
)

func originalDeleteFixture(t *testing.T, author int64) (*admin, *engine.SQL, tbapi.Update) {
	t.Helper()
	db, err := engine.NewSqlite(filepath.Join(t.TempDir(), "delete.db"), "delete-test")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	j, err := moderation.NewJournal(context.Background(), db, moderation.DefaultLimits())
	require.NoError(t, err)
	loc, err := storage.NewLocator(context.Background(), time.Hour, 1000, db)
	require.NoError(t, err)
	for _, mid := range []int{801, 802} {
		require.NoError(t, loc.AddMessage(context.Background(), "identical original", 123, author, "saved-author", mid))
		require.NoError(t, loc.RecordMessageDate(context.Background(), 123, mid, time.Now().Unix()))
	}
	require.NoError(t, loc.AddMessage(context.Background(), "identical original", 999, author, "foreign-chat", 803))
	a := &admin{primChatID: 123, adminChatID: 456, locator: loc, superUsers: SuperUsers{"11"}, bot: &mocks.BotMock{}, tbAPI: &mocks.TbAPIMock{SendFunc: func(tbapi.Chattable) (tbapi.Message, error) { return tbapi.Message{MessageID: 999}, nil }}}
	a.recovery = &recovery{journal: j, api: recoveryAPIFunc(func(context.Context, tbapi.Chattable) (*tbapi.APIResponse, error) { return apiOK(true), nil }), gate: func(moderation.Action) bool { return true }, superName: func(string, int64) bool { return false }}
	u := tbapi.Update{Message: &tbapi.Message{MessageID: 900, Chat: tbapi.Chat{ID: 456}, From: &tbapi.User{ID: 11}, Text: "identical original", ForwardOrigin: &tbapi.MessageOrigin{Type: "hidden_user", SenderUserName: "untrusted display"}}}
	return a, db, u
}

func TestOriginalDeleteHiddenForwardChoiceWorkerRestart(t *testing.T) {
	for _, author := range []int64{501, 0} {
		t.Run(fmt.Sprint(author), func(t *testing.T) {
			a, db, u := originalDeleteFixture(t, author)
			ctx := context.Background()
			j := a.recovery.journal
			require.NoError(t, a.legacyForward(u))
			require.NoError(t, a.legacyForward(u))
			rows, err := j.Snapshot(ctx)
			require.NoError(t, err)
			core := 0
			for _, op := range rows {
				if op.Kind != "card" && op.Kind != "notice" {
					core++
					require.Equal(t, "original_delete", op.Kind)
					require.Equal(t, moderation.Manual, op.State)
					require.Contains(t, []int{801, 802}, op.MessageID)
				}
			}
			require.Equal(t, 2, core)
			c, err := j.CardView(ctx, "delete:123:801")
			require.NoError(t, err)
			require.True(t, c.DeleteOnly)
			require.NoError(t, j.BindCard(ctx, c.Receipt, 600))
			q := cardQuery(c, "MD", "select-one")
			q.Message.Text = "forged target 999:803"
			require.NoError(t, a.recoveryCardCallback(q))
			require.NoError(t, a.recoveryCardCallback(q))
			require.Empty(t, a.bot.(*mocks.BotMock).UpdateSpamCalls())
			require.Empty(t, a.bot.(*mocks.BotMock).UpdateHamCalls())
			require.Empty(t, a.bot.(*mocks.BotMock).RemoveApprovedUserCalls())
			require.Empty(t, a.bot.(*mocks.BotMock).AddApprovedUserCalls())
			calls := map[int]int{}
			a.recovery.api = recoveryAPIFunc(func(_ context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
				switch v := req.(type) {
				case tbapi.GetChatMemberConfig:
					require.NotZero(t, author)
					return apiOK(tbapi.ChatMember{User: &tbapi.User{ID: author}, Status: "member"}), nil
				case tbapi.DeleteMessageConfig:
					require.EqualValues(t, 123, v.ChatID)
					calls[v.MessageID]++
					return apiOK(true), nil
				case tbapi.CallbackConfig:
					return apiOK(true), nil
				case tbapi.MessageConfig, tbapi.EditMessageTextConfig:
					return apiOK(tbapi.Message{MessageID: 600}), nil
				default:
					t.Fatalf("delete-only called unexpected API %T", req)
					return nil, nil
				}
			})
			now := time.Now()
			run := func(journal *moderation.Journal) {
				w := moderation.Worker{Journal: journal, Executor: a.recovery, Now: func() time.Time { return now }}
				for range 30 {
					worked, e := w.Step(ctx)
					require.NoError(t, e)
					if !worked {
						return
					}
				}
				t.Fatal("worker did not settle")
			}
			run(j)
			require.Equal(t, map[int]int{801: 1}, calls)
			restarted, err := moderation.NewJournal(ctx, db, moderation.DefaultLimits())
			require.NoError(t, err)
			a.recovery.journal = restarted
			require.NoError(t, a.legacyForward(u))
			require.NoError(t, a.recoveryCardCallback(q))
			run(restarted)
			require.Equal(t, map[int]int{801: 1}, calls)
			untouched, err := j.CardView(ctx, "delete:123:802")
			require.NoError(t, err)
			require.Equal(t, "pending", untouched.RequestState)
		})
	}
}

func TestOriginalDeleteCallbackGatesAndNoBanEscalation(t *testing.T) {
	for _, mode := range []string{"outsider", "username-only", "technical", "other-operator", "wrong-chat", "wrong-card", "dry", "training", "manual-trust", "superuser", "ban", "undo", "cleanup"} {
		t.Run(mode, func(t *testing.T) {
			a, _, u := originalDeleteFixture(t, 501)
			require.NoError(t, a.legacyForward(u))
			j := a.recovery.journal
			c, err := j.CardView(context.Background(), "delete:123:801")
			require.NoError(t, err)
			require.NoError(t, j.BindCard(context.Background(), c.Receipt, 600))
			q := cardQuery(c, "MD", mode)
			switch mode {
			case "outsider":
				q.From.ID = 12
			case "username-only":
				q.From.ID = 12
				q.From.UserName = "11"
			case "technical":
				q.From.ID = 136817688
			case "other-operator":
				q.From.ID = 12
				a.superUsers = SuperUsers{"11", "12"}
			case "wrong-chat":
				q.Message.Chat.ID = 999
			case "wrong-card":
				q.Message.MessageID = 601
			case "dry":
				a.dry = true
			case "training":
				a.trainingMode = true
			case "manual-trust":
				a.isExplicitlyApproved = func(int64) bool { return true }
			case "superuser":
				a.superUsers = SuperUsers{"11", "501"}
			case "ban":
				q.Data = "M+:" + c.Ref
			case "undo":
				q.Data = "MU:" + c.Ref
			case "cleanup":
				q.Data = "M-:" + c.Ref
			}
			// Callback acknowledgements are the only allowed API boundary.
			a.recovery.api = recoveryAPIFunc(func(_ context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
				_, ok := req.(tbapi.CallbackConfig)
				require.True(t, ok)
				return apiOK(true), nil
			})
			_ = a.recoveryCardCallback(q)
			after, err := j.CardView(context.Background(), c.Receipt)
			require.NoError(t, err)
			require.Equal(t, "pending", after.RequestState)
			for _, op := range after.Actions {
				require.Equal(t, moderation.Manual, op.State)
				require.Equal(t, "original_delete", op.Kind)
			}
		})
	}
}

func TestOriginalDeleteAbsentRetryAndDeadline(t *testing.T) {
	for _, mode := range []string{"absent", "retry", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			a, db, u := originalDeleteFixture(t, 0)
			require.NoError(t, a.legacyForward(u))
			ctx := context.Background()
			j := a.recovery.journal
			c, err := j.CardView(ctx, "delete:123:801")
			require.NoError(t, err)
			require.NoError(t, j.BindCard(ctx, c.Receipt, 600))
			now := time.Now()
			_, err = j.DecideDelete(ctx, c.Ref, 456, 600, 11, true, now)
			require.NoError(t, err)
			calls := 0
			a.recovery.api = recoveryAPIFunc(func(_ context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
				switch req.(type) {
				case tbapi.DeleteMessageConfig:
					calls++
					if mode == "absent" {
						return nil, &tbapi.Error{Code: 400, Message: "Bad Request: message to delete not found"}
					}
					if calls == 1 {
						return nil, &tbapi.Error{Code: 429, ResponseParameters: tbapi.ResponseParameters{RetryAfter: 5}}
					}
					return apiOK(true), nil
				case tbapi.CallbackConfig:
					return apiOK(true), nil
				case tbapi.MessageConfig, tbapi.EditMessageTextConfig:
					return apiOK(tbapi.Message{MessageID: 600}), nil
				default:
					t.Fatalf("unexpected %T", req)
					return nil, nil
				}
			})
			if mode == "deadline" {
				now = now.Add(25 * time.Hour)
			}
			run := func() {
				w := moderation.Worker{Journal: j, Executor: a.recovery, Now: func() time.Time { return now }}
				for range 20 {
					worked, e := w.Step(ctx)
					require.NoError(t, e)
					if !worked {
						return
					}
				}
				t.Fatal("worker did not settle")
			}
			run()
			j, err = moderation.NewJournal(ctx, db, moderation.DefaultLimits())
			require.NoError(t, err)
			a.recovery.journal = j
			now = now.Add(time.Minute)
			run()
			after, err := j.CardView(ctx, c.Receipt)
			require.NoError(t, err)
			op := after.Actions[0]
			switch mode {
			case "absent":
				require.Equal(t, moderation.Absent, op.State)
				require.Equal(t, 1, calls)
				require.Contains(t, originalDeleteCardText(after, "test"), "удаление ботом не подтверждено")
			case "retry":
				require.Equal(t, moderation.Confirmed, op.State)
				require.Equal(t, 2, calls)
			case "deadline":
				require.Equal(t, moderation.Expired, op.State)
				require.Zero(t, calls)
			}
		})
	}
}
func TestOriginalDeleteCaptionMediaAndMissingCandidates(t *testing.T) {
	for _, mode := range []string{"caption", "media", "missing", "reliable-forward", "dry", "training", "numeric-operator", "protected"} {
		t.Run(mode, func(t *testing.T) {
			a, _, u := originalDeleteFixture(t, 501)
			ctx := context.Background()
			switch mode {
			case "caption":
				u.Message.Text = ""
				u.Message.Caption = "identical original"
			case "media":
				u.Message.Text = ""
				u.Message.Photo = []tbapi.PhotoSize{{FileUniqueID: "fixture-photo", Width: 100, Height: 100}}
				loc := a.locator.(*storage.Locator)
				key := storage.MediaLocatorKey{Version: 1, Kind: storage.MediaPhoto, StableMediaID: "fixture-photo"}
				require.NoError(t, loc.AddMediaMessage(ctx, key, 123, storage.LocatorIdentity{Kind: storage.LocatorIdentityUser, ID: 501}, "saved", 801))
				require.NoError(t, loc.AddMediaMessage(ctx, key, 123, storage.LocatorIdentity{Kind: storage.LocatorIdentityUser, ID: 501}, "saved", 802))
			case "missing":
				u.Message.Text = "not saved"
			case "reliable-forward":
				u.Message.ForwardOrigin = userOrigin(999, "different author is not ban authority")
			case "dry":
				a.dry = true
			case "training":
				a.trainingMode = true
			case "numeric-operator":
				u.Message.From.ID = 12
				u.Message.From.UserName = "11"
			case "protected":
				a.isExplicitlyApproved = func(id int64) bool { return id == 501 }
			}
			require.NoError(t, a.legacyForward(u))
			rows, err := a.recovery.journal.Snapshot(ctx)
			require.NoError(t, err)
			count := 0
			for _, op := range rows {
				if op.Kind != "card" && op.Kind != "notice" {
					count++
					require.Equal(t, "original_delete", op.Kind)
					require.Equal(t, moderation.Manual, op.State)
				}
			}
			if mode == "caption" || mode == "media" || mode == "reliable-forward" {
				require.Equal(t, 2, count)
				c, e := a.recovery.journal.CardView(ctx, "delete:123:801")
				require.NoError(t, e)
				require.Contains(t, originalDeleteCardText(c, "Чат"), "Только удаление")
				buttons := deleteOnlyKeyboard(c, true)
				require.Len(t, buttons.InlineKeyboard, 3)
				require.Equal(t, "Создать новый запрос", buttons.InlineKeyboard[2][0].Text)
				require.Equal(t, "Удалить сообщение", buttons.InlineKeyboard[1][0].Text)
			} else {
				require.Zero(t, count)
			}
			require.Empty(t, a.bot.(*mocks.BotMock).UpdateSpamCalls())
			require.Empty(t, a.bot.(*mocks.BotMock).RemoveApprovedUserCalls())
		})
	}
}
func TestOriginalDeleteUsesSavedSendDateNotObservation(t *testing.T) {
	for _, mode := range []string{"different hours", "outside window", "unknown date", "missing observation"} {
		t.Run(mode, func(t *testing.T) {
			a, db, u := originalDeleteFixture(t, 0)
			ctx := context.Background()
			now := time.Now()
			sent := now.Add(-40 * time.Hour).Truncate(time.Second).Unix()
			observed := now.Add(-time.Hour)
			if mode == "outside window" {
				sent = now.Add(-50 * time.Hour).Truncate(time.Second).Unix()
			}
			if mode == "unknown date" {
				_, err := db.Exec(`DELETE FROM message_dates WHERE gid=? AND chat_id=? AND msg_id=?`, db.GID(), 123, 801)
				require.NoError(t, err)
				sent = 0
			} else {
				_, err := db.Exec(`UPDATE message_dates SET message_date=? WHERE gid=? AND chat_id=? AND msg_id=?`, sent, db.GID(), 123, 801)
				require.NoError(t, err)
			}
			if mode == "missing observation" {
				_, err := db.Exec(`UPDATE messages SET time=NULL WHERE gid=? AND chat_id=? AND msg_id=?`, db.GID(), 123, 801)
				require.NoError(t, err)
			} else {
				_, err := db.Exec(`UPDATE messages SET time=? WHERE gid=? AND chat_id=? AND msg_id=?`, observed, db.GID(), 123, 801)
				require.NoError(t, err)
			}
			require.NoError(t, a.legacyForward(u))
			j := a.recovery.journal
			c, err := j.CardView(ctx, "delete:123:801")
			require.NoError(t, err)
			require.Len(t, c.Actions, 1)
			op := c.Actions[0]
			require.Equal(t, sent, op.MessageDate)
			rendered := originalDeleteCardText(c, "fixture")
			if sent == 0 {
				require.Contains(t, rendered, "Дата оригинала неизвестна")
				require.InDelta(t, now.Add(24*time.Hour).UnixMilli(), op.Deadline, 5000)
			} else {
				expected := time.Unix(sent, 0).Add(48*time.Hour - 2*time.Minute).UnixMilli()
				require.Equal(t, expected, op.Deadline)
				require.Contains(t, rendered, "Дата оригинала: "+cardDate(sent))
				require.Contains(t, rendered, "Срок удаления: "+cardDate(expected/1000))
				require.NotContains(t, rendered, "Дата оригинала: "+cardDate(observed.Unix()))
			}
			if mode == "outside window" {
				require.NoError(t, j.BindCard(ctx, c.Receipt, 600))
				require.NoError(t, a.recoveryCardCallback(cardQuery(c, "MD", "dated")))
				deletes := 0
				a.recovery.api = recoveryAPIFunc(func(_ context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
					if _, ok := req.(tbapi.DeleteMessageConfig); ok {
						deletes++
					}
					return apiOK(tbapi.Message{MessageID: 600}), nil
				})
				w := moderation.Worker{Journal: j, Executor: a.recovery}
				for range 20 {
					worked, e := w.Step(ctx)
					require.NoError(t, e)
					if !worked {
						break
					}
				}
				after, e := j.CardView(ctx, c.Receipt)
				require.NoError(t, e)
				require.Equal(t, moderation.Expired, after.Actions[0].State)
				require.Zero(t, deletes)
			}
		})
	}
}
func TestOriginalDeleteExplicitNewRequestAndStaleDelivery(t *testing.T) {
	a, db, u := originalDeleteFixture(t, 0)
	ctx := context.Background()
	a.superUsers = SuperUsers{"11", "12"}
	a.recovery.api = recoveryAPIFunc(func(_ context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
		_, ok := req.(tbapi.CallbackConfig)
		require.True(t, ok, "renewal and confirmation must not call moderation API")
		return apiOK(true), nil
	})
	require.NoError(t, a.legacyForward(u))
	j := a.recovery.journal
	old, err := j.CardView(ctx, "delete:123:801")
	require.NoError(t, err)
	require.NoError(t, j.BindCard(ctx, old.Receipt, 600))
	require.NoError(t, a.recoveryCardCallback(cardQuery(old, "MX", "dismiss-first")))
	newRequest := cardQuery(old, "MN", "new-explicit")
	newRequest.From.ID = 12
	require.NoError(t, a.recoveryCardCallback(newRequest))
	current, err := j.CardView(ctx, old.Receipt)
	require.NoError(t, err)
	require.NotEqual(t, old.Ref, current.Ref)
	require.EqualValues(t, 12, current.RequesterID)
	require.Equal(t, old.Actions[0].Deadline, current.Actions[0].Deadline)
	require.NoError(t, j.BindCard(ctx, current.Receipt, 601))
	// A real database close/reopen preserves the new epoch and makes old refs inert.
	var path string
	require.NoError(t, db.Get(&path, `SELECT file FROM pragma_database_list WHERE name='main'`))
	require.NoError(t, db.Close())
	reopened, err := engine.NewSqlite(path, "delete-test")
	require.NoError(t, err)
	defer reopened.Close()
	j, err = moderation.NewJournal(ctx, reopened, moderation.DefaultLimits())
	require.NoError(t, err)
	a.recovery.journal = j
	a.locator, err = storage.NewLocator(ctx, time.Hour, 1000, reopened)
	require.NoError(t, err)
	require.NoError(t, a.legacyForward(u))
	require.NoError(t, a.recoveryCardCallback(newRequest))
	require.NoError(t, a.recoveryCardCallback(cardQuery(old, "MD", "old-confirmation")))
	wrongOperator := cardQuery(current, "MD", "old-operator-new-ref")
	wrongOperator.Message.MessageID = 601
	require.NoError(t, a.recoveryCardCallback(wrongOperator))
	still, err := j.CardView(ctx, current.Receipt)
	require.NoError(t, err)
	require.Equal(t, current.RequestExpires, still.RequestExpires)
	require.Equal(t, "pending", still.RequestState)
	confirmed := cardQuery(current, "MD", "current-confirmation")
	confirmed.From.ID = 12
	confirmed.Message.MessageID = 601
	require.NoError(t, a.recoveryCardCallback(confirmed))
	deletes := 0
	a.recovery.api = recoveryAPIFunc(func(_ context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
		switch req.(type) {
		case tbapi.DeleteMessageConfig:
			deletes++
			return apiOK(true), nil
		case tbapi.CallbackConfig:
			return apiOK(true), nil
		default:
			return apiOK(tbapi.Message{MessageID: 601}), nil
		}
	})
	w := moderation.Worker{Journal: j, Executor: a.recovery}
	for range 20 {
		worked, e := w.Step(ctx)
		require.NoError(t, e)
		if !worked {
			break
		}
	}
	require.Equal(t, 1, deletes)
	require.NoError(t, a.legacyForward(u))
	require.NoError(t, a.recoveryCardCallback(newRequest))
	require.NoError(t, a.recoveryCardCallback(confirmed))
	for range 20 {
		worked, e := w.Step(ctx)
		require.NoError(t, e)
		if !worked {
			break
		}
	}
	require.Equal(t, 1, deletes)
	require.Empty(t, a.bot.(*mocks.BotMock).UpdateSpamCalls())
	require.Empty(t, a.bot.(*mocks.BotMock).UpdateHamCalls())
}
