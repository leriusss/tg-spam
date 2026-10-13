package events

import (
	"context"
	"fmt"
	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/events/mocks"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func cardFixture(t *testing.T, origin string) (*admin, *moderation.Journal, moderation.Card) {
	t.Helper()
	j := recoveryJournal(t)
	now := time.Now()
	p := moderation.Plan{
		Key:          "source:123:701:user:501",
		ChatID:       123,
		MessageID:    701,
		IdentityID:   501,
		IdentityKind: "user",
		Destination:  456,
		Origin:       origin,
		Card:         true,
		SourceText:   "original text\nban: this line belongs to the source",
		UserName:     "subject",
		Actions: []moderation.Action{
			{Kind: "ban", ChatID: 123, IdentityID: 501, IdentityKind: "user", UntilDate: now.Add(time.Hour).Unix()},
			{
				Kind:         "original_delete",
				ChatID:       123,
				IdentityID:   501,
				IdentityKind: "user",
				MessageID:    701,
				MessageDate:  now.Unix(),
			},
		},
	}
	_, _, _, err := j.Accept(context.Background(), p, now)
	require.NoError(t, err)
	require.NoError(t, j.BindCard(context.Background(), p.Key, 600))
	a := &admin{
		primChatID:  123,
		adminChatID: 456,
		bot: &mocks.BotMock{
			UpdateSpamFunc:         func(string) error { return nil },
			UpdateHamFunc:          func(string) error { return nil },
			AddApprovedUserFunc:    func(int64, string) error { return nil },
			RemoveApprovedUserFunc: func(int64) error { return nil },
		},
		recovery: &recovery{
			journal:   j,
			gate:      func(moderation.Action) bool { return true },
			superName: func(string, int64) bool { return false },
			api: recoveryAPIFunc(
				func(context.Context, tbapi.Chattable) (*tbapi.APIResponse, error) { return apiOK(true), nil },
			),
		},
	}
	c, err := j.CardView(context.Background(), p.Key)
	require.NoError(t, err)
	return a, j, c
}

func cardQuery(c moderation.Card, verb, id string) *tbapi.CallbackQuery {
	return &tbapi.CallbackQuery{
		ID:   id,
		Data: verb + ":" + c.Ref,
		From: &tbapi.User{ID: 11, UserName: "operator"},
		Message: &tbapi.Message{
			Chat:      tbapi.Chat{ID: 456},
			MessageID: 600,
			Text:      "DISPLAYED DIGEST AND STATUSES ARE NOT ORIGINAL",
		},
	}
}

func TestRecoveryCardLearningUsesStoredOriginalAndSafeButtons(t *testing.T) {
	a, j, c := cardFixture(t, "detector")
	require.NoError(t, a.recoveryCardCallback(cardQuery(c, "M+", "approve-1")))
	require.Equal(t, c.Text, a.bot.(*mocks.BotMock).UpdateSpamCalls()[0].Msg)
	require.NoError(t, a.recoveryCardCallback(cardQuery(c, "M+", "approve-1")))
	require.Len(t, a.bot.(*mocks.BotMock).UpdateSpamCalls(), 1, "same callback delivery does not repeat learning")
	require.NoError(t, a.recoveryCardCallback(cardQuery(c, "MU", "unban-1")))
	require.Equal(t, c.Text, a.bot.(*mocks.BotMock).UpdateHamCalls()[0].Msg)
	c, err := j.CardView(context.Background(), c.Ref)
	require.NoError(t, err)
	require.True(t, c.Held)
	text := cardText(c)
	require.Contains(t, text, "original text")
	require.Contains(t, text, "ожидает выполнения")
	require.Contains(t, text, "Повторы отменены")
	require.NotContains(t, text, "successfully banned")
	keyboard := cardKeyboard(c, false)
	for _, row := range keyboard.InlineKeyboard {
		for _, button := range row {
			require.NotContains(t, *button.CallbackData, "M+")
			require.NotContains(t, *button.CallbackData, "MU")
		}
	}
	bad := cardQuery(c, "MU", "forged")
	bad.Message.MessageID = 601
	require.Error(t, a.recoveryCardCallback(bad))
	a.dry = true
	require.Error(t, a.recoveryCardCallback(cardQuery(c, "M+", "dry")))
	a.dry = false
	a.trainingMode = true
	require.Error(t, a.recoveryCardCallback(cardQuery(c, "MU", "training")))
	require.Len(t, a.bot.(*mocks.BotMock).UpdateHamCalls(), 1)
}

func TestRecoveryReportRejectAndApproveUseJournal(t *testing.T) {
	a, j, c := cardFixture(t, "report")
	reports := &mocks.ReportsMock{
		DeleteByMessageFunc: func(context.Context, int, int64) error { return nil },
		GetByMessageFunc: func(context.Context, int, int64) ([]storage.Report, error) {
			return []storage.Report{
				{
					MsgID:            701,
					ChatID:           123,
					ReportedUserID:   501,
					ReportedUserName: "subject",
					MsgText:          c.Text,
					AdminMsgID:       600,
				},
			}, nil
		},
	}
	r := &userReports{
		ReportConfig:  ReportConfig{Storage: reports},
		recoveryAdmin: a,
		primChatID:    123,
		adminChatID:   456,
		bot:           a.bot,
		locator:       &mocks.LocatorMock{},
	}
	q := cardQuery(c, "M-", "reject-report")
	q.Data = "R-501:701"
	require.NoError(t, r.callbackReportReject(context.Background(), q))
	c, err := j.CardView(context.Background(), c.Ref)
	require.NoError(t, err)
	for _, op := range c.Actions {
		require.Equal(t, moderation.Canceled, op.State)
	}
	q.ID = "approve-report"
	q.Data = "R+501:701"
	require.NoError(t, r.callbackReportBan(context.Background(), q))
	c, err = j.CardView(context.Background(), c.Ref)
	require.NoError(t, err)
	require.Equal(t, c.Text, a.bot.(*mocks.BotMock).UpdateSpamCalls()[0].Msg)
	permanent := false
	for _, op := range c.Actions {
		if op.Kind == "ban" && op.UntilDate > time.Now().Add(366*24*time.Hour).Unix() {
			permanent = true
			require.Equal(t, moderation.Pending, op.State)
		}
	}
	require.True(t, permanent)
	// callback path sends only an acknowledgement, no ban/delete API outside the journal.
}

func TestRecoveryCallbackWorkerCompetitionAndUnknownResult(t *testing.T) {
	for _, loseAck := range []bool{false, true} {
		t.Run(fmt.Sprint(loseAck), func(t *testing.T) {
			a, j, c := cardFixture(t, "report")
			entered := make(chan struct{})
			release := make(chan struct{})
			banned := atomic.Bool{}
			banCalls := atomic.Int32{}
			unbanCalls := atomic.Int32{}
			a.recovery.api = recoveryAPIFunc(func(ctx context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
				switch req.(type) {
				case tbapi.GetChatMemberConfig:
					status := "member"
					if banned.Load() {
						status = "kicked"
					}
					return apiOK(tbapi.ChatMember{User: &tbapi.User{ID: 501}, Status: status}), nil
				case tbapi.BanChatMemberConfig:
					banCalls.Add(1)
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					banned.Store(true)
					if loseAck {
						return nil, io.EOF
					}
					return apiOK(true), nil
				case tbapi.UnbanChatMemberConfig:
					unbanCalls.Add(1)
					banned.Store(false)
					return apiOK(true), nil
				case tbapi.MessageConfig, tbapi.EditMessageTextConfig:
					return apiOK(tbapi.Message{MessageID: 600}), nil
				case tbapi.CallbackConfig:
					return apiOK(true), nil
				default:
					return nil, fmt.Errorf("unexpected request %T", req)
				}
			})
			w := moderation.Worker{Journal: j, Executor: a.recovery}
			_, err := w.Step(context.Background())
			require.NoError(t, err) // initial coalesced notice
			finished := make(chan error, 1)
			go func() { _, e := w.Step(context.Background()); finished <- e }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("ban never entered API")
			}
			require.NoError(t, a.recoveryCardCallback(cardQuery(c, "MU", "stop-during-api")))
			require.Zero(t, unbanCalls.Load(), "callback cannot overtake running Telegram request")
			close(release)
			require.NoError(t, <-finished)
			for range 20 {
				worked, e := w.Step(context.Background())
				require.NoError(t, e)
				if !worked {
					break
				}
			}
			require.EqualValues(t, 1, banCalls.Load())
			require.EqualValues(t, 1, unbanCalls.Load())
			require.False(t, banned.Load())
			c, err = j.CardView(context.Background(), c.Ref)
			require.NoError(t, err)
			require.True(t, c.Held)
			for _, op := range c.Actions {
				if op.Kind == "ban" {
					if loseAck {
						require.Equal(t, moderation.Manual, op.State)
						require.True(t, op.Uncertain)
					} else {
						require.Equal(t, moderation.Confirmed, op.State)
					}
				}
			}
			require.NoError(t, a.recoveryCardCallback(cardQuery(c, "MU", "stop-during-api")))
			require.EqualValues(t, 1, unbanCalls.Load())
		})
	}
}

func TestRecoveryConcreteCardDeliveryIncludesContextAndFactualStatuses(t *testing.T) {
	a, j, c := cardFixture(t, "report")
	var rendered tbapi.EditMessageTextConfig
	a.recovery.api = recoveryAPIFunc(func(_ context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
		rendered = req.(tbapi.EditMessageTextConfig)
		return apiOK(tbapi.Message{MessageID: 600}), nil
	})
	rows, err := j.Snapshot(context.Background())
	require.NoError(t, err)
	for _, op := range rows {
		if op.Kind == "card" {
			out := a.recovery.Execute(context.Background(), op)
			require.Equal(t, moderation.Confirmed, out.State)
		}
	}
	require.NotContains(t, rendered.Text, "identity=501")
	require.Contains(t, rendered.Text, inertSpamText(c.Text))
	require.Contains(t, rendered.Text, "Удаление ожидает выполнения")
	require.NotNil(t, rendered.ReplyMarkup)
	require.Contains(t, fmt.Sprint(rendered.ReplyMarkup), "Отклонить жалобу")
}

func TestRecoveryReportCallbackAfterStorageTTLAndInfo(t *testing.T) {
	a, j, c := cardFixture(t, "report")
	r := &userReports{
		ReportConfig: ReportConfig{
			Storage: &mocks.ReportsMock{
				GetByMessageFunc: func(context.Context, int, int64) ([]storage.Report, error) { return nil, nil },
			},
		},
		recoveryAdmin: a,
		primChatID:    123,
		adminChatID:   456,
		bot:           a.bot,
	}
	q := cardQuery(c, "MR", "reject-after-ttl")
	q.Data = "R-501:701"
	require.NoError(t, r.callbackReportReject(context.Background(), q))
	c, err := j.CardView(context.Background(), c.Ref)
	require.NoError(t, err)
	for _, op := range c.Actions {
		require.Equal(t, moderation.Canceled, op.State)
	}
	var edited string
	a.recovery.api = recoveryAPIFunc(func(_ context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
		if edit, ok := req.(tbapi.EditMessageTextConfig); ok {
			edited = edit.Text
		}
		return apiOK(true), nil
	})
	require.NoError(t, a.recoveryCardCallback(cardQuery(c, "M?", "info")))
	require.NotContains(t, edited, c.Text)
	require.Contains(t, edited, "Данные детекции")
	require.Contains(t, edited, "canceled")
	require.Empty(t, a.bot.(*mocks.BotMock).UpdateSpamCalls())
	require.Empty(t, a.bot.(*mocks.BotMock).UpdateHamCalls())
}

func TestRecoveryTemporaryMembershipCannotSatisfyPermanentBan(t *testing.T) {
	for _, until := range []int64{0, time.Now().Add(9999 * time.Hour).Unix()} {
		calls := 0
		r := &recovery{
			gate:      func(moderation.Action) bool { return true },
			superName: func(string, int64) bool { return false },
			api: recoveryAPIFunc(func(_ context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
				if _, ok := req.(tbapi.GetChatMemberConfig); ok {
					return apiOK(
						tbapi.ChatMember{
							User:      &tbapi.User{ID: 7},
							Status:    "kicked",
							UntilDate: time.Now().Add(time.Hour).Unix(),
						},
					), nil
				}
				calls++
				require.IsType(t, tbapi.BanChatMemberConfig{}, req)
				return apiOK(true), nil
			}),
		}
		out := r.Execute(
			context.Background(),
			moderation.Action{Kind: "ban", ChatID: -100, IdentityID: 7, IdentityKind: "user", UntilDate: until},
		)
		require.Equal(t, moderation.Confirmed, out.State)
		require.Equal(t, 1, calls, "temporary membership cannot acknowledge permanent requirement")
	}
}

func TestRecoveryReporterConfirmationUsesCommandJournalWithoutInventedOriginal(t *testing.T) {
	a, j, c := cardFixture(t, "report")
	store := &mocks.ReportsMock{GetByMessageFunc: func(context.Context, int, int64) ([]storage.Report, error) {
		return []storage.Report{
			{
				MsgID:            701,
				ChatID:           123,
				ReportedUserID:   501,
				ReporterUserID:   777,
				ReporterUserName: "reporter",
				AdminMsgID:       600,
			},
		}, nil
	}, DeleteReporterFunc: func(context.Context, int64, int, int64) error { return nil }}
	r := &userReports{
		ReportConfig:  ReportConfig{Storage: store},
		recoveryAdmin: a,
		bot:           a.bot,
		primChatID:    123,
		adminChatID:   456,
	}
	q := cardQuery(c, "M+", "reporter-confirm")
	q.Data = "R!777:701"
	require.NoError(t, r.callbackReportBanReporterConfirm(context.Background(), q))
	command, err := j.CardView(context.Background(), "command:"+q.ID)
	require.NoError(t, err)
	require.EqualValues(t, 777, command.IdentityID)
	require.Zero(t, command.MessageID)
	require.Empty(t, command.Text)
	require.Len(t, command.Actions, 1)
	require.Equal(t, "ban", command.Actions[0].Kind)
	require.Equal(t, moderation.Pending, command.Actions[0].State)
	require.Empty(t, a.bot.(*mocks.BotMock).UpdateSpamCalls())
	q.ID = "unknown-reporter"
	q.Data = "R!778:701"
	require.Error(t, r.callbackReportBanReporterConfirm(context.Background(), q))
}

func TestRecoveryFirstReportApproveRunsBanAndDelete(t *testing.T) {
	a, _, c := cardFixture(t, "report")
	j := recoveryJournal(t)
	a.recovery.journal = j
	r := &userReports{ReportConfig: ReportConfig{Storage: &mocks.ReportsMock{GetByMessageFunc: func(context.Context, int, int64) ([]storage.Report, error) {
		return []storage.Report{{MsgID: 701, ChatID: 123, ReportedUserID: 501, ReportedUserName: "subject", MsgText: c.Text, AdminMsgID: 600}}, nil
	}}}, recoveryAdmin: a, primChatID: 123, adminChatID: 456, bot: a.bot, locator: &mocks.LocatorMock{}}
	q := cardQuery(c, "M+", "first-approve")
	q.Data = "R+501:701"
	require.NoError(t, r.callbackReportBan(context.Background(), q))
	calls := map[string]int{}
	w := moderation.Worker{Journal: j, Executor: executeRecoveryFunc(func(_ context.Context, op moderation.Action) moderation.Outcome {
		calls[op.Kind]++
		return moderation.Outcome{State: moderation.Confirmed, NotificationID: 600}
	})}
	for range 10 {
		ok, err := w.Step(context.Background())
		require.NoError(t, err)
		if !ok {
			break
		}
	}
	require.Equal(t, 1, calls["ban"])
	require.Equal(t, 1, calls["original_delete"])
	require.NoError(t, r.callbackReportBan(context.Background(), q))
	for range 10 {
		ok, err := w.Step(context.Background())
		require.NoError(t, err)
		if !ok {
			break
		}
	}
	require.Equal(t, 1, calls["ban"])
	require.Equal(t, 1, calls["original_delete"])
}

type executeRecoveryFunc func(context.Context, moderation.Action) moderation.Outcome

func (f executeRecoveryFunc) Execute(ctx context.Context, a moderation.Action) moderation.Outcome {
	return f(ctx, a)
}
