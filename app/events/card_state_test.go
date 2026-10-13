package events

import (
	"context"
	"strings"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/moderation"
)

func TestCardStateButtons(t *testing.T) {
	for _, tc := range []struct {
		name    string
		card    moderation.Card
		allowed bool
		want    []string
	}{
		{"success", moderation.Card{Actions: []moderation.Action{{Kind: "ban", State: moderation.Confirmed},
			{Kind: "original_delete", State: moderation.Confirmed}}}, true, []string{"MA", "M?"}},
		{"pending", moderation.Card{Actions: []moderation.Action{{Kind: "original_delete", State: moderation.Pending}}},
			true, []string{"MA", "M?", "M-"}},
		{"manual", moderation.Card{Actions: []moderation.Action{{ID: "delete", Kind: "original_delete", State: moderation.Failed}}},
			true, []string{"MA", "M?", "MC", "M+"}},
		{"closed", moderation.Card{Resolved: []string{"delete"}, Actions: []moderation.Action{
			{ID: "delete", Kind: "original_delete", State: moderation.Failed}}}, true, []string{"MA", "M?"}},
		{"manual plus pending", moderation.Card{Actions: []moderation.Action{{Kind: "cleanup_delete", State: moderation.Failed},
			{Kind: "original_delete", State: moderation.Pending}}}, true, []string{"MA", "M?", "M-"}},
		{"undo unfinished", moderation.Card{Actions: []moderation.Action{{Kind: "unban", State: moderation.Failed}}},
			true, []string{"MA", "M?", "MC"}},
		{"safety", moderation.Card{Actions: []moderation.Action{{Kind: "ban", State: moderation.Blocked}}},
			false, []string{"M?", "MC"}},
		{"notification is not retry", moderation.Card{Actions: []moderation.Action{{Kind: "card", State: moderation.Pending}}},
			true, []string{"MA", "M?"}},
		{"expired window cannot be approved", moderation.Card{Actions: []moderation.Action{
			{Kind: "original_delete", State: moderation.Expired, MessageDate: time.Now().Add(-49 * time.Hour).Unix()}}},
			true, []string{"MA", "M?", "MC"}},
		{"report decision", moderation.Card{Origins: []moderation.Demand{{Origin: "report:1", Active: true}},
			Actions: []moderation.Action{{Kind: "original_delete", State: moderation.Manual}}},
			true, []string{"MA", "M?", "MC", "MR", "M+"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.card.Ref = "bound-ref"
			got := []string{}
			for _, row := range cardKeyboard(tc.card, tc.allowed).InlineKeyboard {
				for _, button := range row {
					prefix, ref, ok := strings.Cut(*button.CallbackData, ":")
					require.True(t, ok)
					require.Equal(t, tc.card.Ref, ref)
					got = append(got, prefix)
					if prefix == "M+" {
						require.Equal(t, "Подтвердить модерацию", button.Text)
					}
				}
			}
			require.Equal(t, tc.want, got)
		})
	}
}

type stateExecutor func(context.Context, moderation.Action) moderation.Outcome

func (f stateExecutor) Execute(ctx context.Context, a moderation.Action) moderation.Outcome {
	return f(ctx, a)
}

func TestCardStateApproveAndUndoJournalHistory(t *testing.T) {
	a, j, c := cardFixture(t, "detector")
	now := time.Now().Add(time.Second)
	failBan := true
	w := moderation.Worker{Journal: j, Now: func() time.Time { return now },
		Executor: stateExecutor(func(_ context.Context, op moderation.Action) moderation.Outcome {
			if op.Kind == "ban" && failBan {
				return moderation.Outcome{State: moderation.Failed, Code: "telegram_403"}
			}
			return moderation.Outcome{State: moderation.Confirmed, NotificationID: 600}
		})}
	drain := func() {
		for range 20 {
			worked, err := w.Step(context.Background())
			require.NoError(t, err)
			now = now.Add(time.Second)
			if !worked {
				return
			}
		}
		t.Fatal("worker did not drain")
	}
	drain()
	c, err := j.CardView(context.Background(), c.Ref)
	require.NoError(t, err)
	require.Contains(t, cardText(c), "Действие отклонено")
	failBan = false
	require.NoError(t, a.recoveryCardCallback(cardQuery(c, "M+", "approve-success")))
	drain()
	c, err = j.CardView(context.Background(), c.Ref)
	require.NoError(t, err)
	require.Contains(t, cardText(c), "заблокирован")
	require.NotContains(t, cardText(c), "отклонено")
	require.Contains(t, cardDetails(c), "ban: failed")
	require.Contains(t, cardDetails(c), "ban: confirmed")
	require.NoError(t, a.recoveryCardCallback(cardQuery(c, "MU", "undo-success")))
	drain()
	c, err = j.CardView(context.Background(), c.Ref)
	require.NoError(t, err)
	require.Contains(t, cardText(c), "Блокировка снята")
	require.NotContains(t, cardText(c), "заблокирован")
	require.Contains(t, cardDetails(c), "ban: confirmed")
	require.Contains(t, cardDetails(c), "unban: confirmed")
	var pages strings.Builder
	a.recovery.api = recoveryAPIFunc(func(_ context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
		if edit, ok := req.(tbapi.EditMessageTextConfig); ok {
			pages.WriteString(edit.Text)
		}
		return apiOK(true), nil
	})
	for page := 0; page < (len(c.Actions)+1)/2; page++ {
		verb := "MP" + string(rune('0'+page))
		require.NoError(t, a.recoveryCardCallback(cardQuery(c, verb, "history")))
	}
	for _, op := range c.Actions {
		require.Contains(t, pages.String(), op.ID)
	}
	require.Error(t, a.recoveryCardCallback(cardQuery(c, "MP9999", "bad-page")))
	wrong := cardQuery(c, "MP0", "wrong-card")
	wrong.Message.MessageID++
	require.Error(t, a.recoveryCardCallback(wrong))
}

func TestCardStateCanceledRetryDoesNotOverrideNewResult(t *testing.T) {
	c := moderation.Card{Actions: []moderation.Action{
		{Kind: "ban", State: moderation.Canceled, NextAt: 500},
		{Kind: "ban", State: moderation.Confirmed, NextAt: 200},
		{Kind: "unban", State: moderation.Confirmed, NextAt: 300},
	}}
	require.Contains(t, cardText(c), "Блокировка снята")
	require.NotContains(t, cardText(c), "заблокирован")
	require.NotContains(t, cardText(c), "Действие отменено")
}

func TestCardReadableDatesAndRelevantFields(t *testing.T) {
	d := time.Date(2026, 10, 13, 19, 45, 0, 0, time.UTC)
	text := cardDetails(moderation.Card{Actions: []moderation.Action{
		{Kind: "ban", State: moderation.Confirmed, UntilDate: d.Unix()},
		{Kind: "original_delete", State: moderation.Pending, MessageID: 701, MessageDate: d.Unix(),
			Deadline: d.Add(time.Hour).UnixMilli(), NextAt: d.Add(time.Minute).UnixMilli()},
	}})
	require.Contains(t, text, "13.10.2026 21:45:00 (Калининград)")
	require.NotContains(t, text, "Причина:")
	require.NotContains(t, text, "message=0")
	require.NotContains(t, text, "дата=0")
	require.Equal(t, 1, strings.Count(text, "Запрошено до:"))
	require.Equal(t, 1, strings.Count(text, "Следующее выполнение:"))
}
