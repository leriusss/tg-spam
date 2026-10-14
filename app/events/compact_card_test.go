package events

import (
	"context"
	"html"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/moderation"
)

func TestCompactActionTruthfulOutcomes(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		a    moderation.Action
		want string
	}{
		{"deleted", moderation.Action{Kind: "original_delete", State: moderation.Confirmed}, "✅ Сообщение удалено"},
		{"pending", moderation.Action{Kind: "original_delete", State: moderation.Pending}, "⏳ Удаление ожидает выполнения"},
		{"retry", moderation.Action{Kind: "original_delete", State: moderation.Pending, Attempts: 1,
			NextAt: now.Add(time.Minute).UnixMilli(), Deadline: now.Add(time.Hour).UnixMilli()}, "Повторим автоматически"},
		{"retry exhausted", moderation.Action{Kind: "original_delete", State: moderation.Pending, Attempts: 320}, "ожидает выполнения"},
		{"absent", moderation.Action{Kind: "original_delete", State: moderation.Absent}, "удаление ботом не подтверждено"},
		{"expired", moderation.Action{Kind: "original_delete", State: moderation.Expired,
			MessageDate: now.Add(-49 * time.Hour).Unix()}, "окно удаления ботом истекло"},
		{"unknown date", moderation.Action{Kind: "original_delete", State: moderation.Manual}, "Дата или оригинал неизвестны"},
		{"retry horizon", moderation.Action{Kind: "original_delete", State: moderation.Expired,
			MessageDate: now.Unix()}, "срок выполнения истёк"},
		{"refusal", moderation.Action{Kind: "original_delete", State: moderation.Failed}, "Удаление отклонено"},
		{"uncertain", moderation.Action{Kind: "original_delete", State: moderation.Failed, Uncertain: true}, "Результат не подтверждён"},
		{"reconciled", moderation.Action{Kind: "ban", State: moderation.Confirmed, Code: "reconciled_membership"}, "уже заблокирован"},
		{"permanent", moderation.Action{Kind: "ban", State: moderation.Confirmed}, "заблокирован навсегда"},
		{"temporary", moderation.Action{Kind: "ban", State: moderation.Confirmed, UntilDate: now.Add(time.Hour).Unix()}, "заблокирован до"},
		{"restricted", moderation.Action{Kind: "restrict", State: moderation.Confirmed}, "Отправка сообщений ограничена"},
		{"channel", moderation.Action{Kind: "ban", State: moderation.Confirmed, IdentityKind: "sender_chat"}, "Канал заблокирован"},
		{"canceled", moderation.Action{State: moderation.Canceled}, "Действие отменено"},
		{"safety", moderation.Action{State: moderation.Blocked}, "остановлено защитой"},
		{"unknown", moderation.Action{}, "Результат неизвестен"},
	} {
		t.Run(tc.name, func(t *testing.T) { require.Contains(t, compactAction(tc.a), tc.want) })
	}
}

func TestCompactCardEscapingLimitsAndPartialCleanup(t *testing.T) {
	c := moderation.Card{DisplayName: "<Анна & Иван>", IdentityKind: "user", IdentityID: 501,
		Text: strings.Repeat("<>& https://t.me/spam @spam example.com\n", 1000),
		Actions: []moderation.Action{{Kind: "ban", State: moderation.Confirmed},
			{Kind: "original_delete", MessageID: 701, State: moderation.Pending}, {Kind: "card", State: moderation.Pending},
			{Kind: "cleanup_delete", MessageID: 702, State: moderation.Confirmed}, {Kind: "cleanup_delete", MessageID: 703, State: moderation.Absent}}}
	original := c.Text
	text := compactCardText(c, "Балтия")
	require.Contains(t, text, "🛡 Спам — Балтия")
	require.Contains(t, text, `href="tg://user?id=501"`)
	require.Contains(t, text, "&lt;Анна &amp; Иван&gt;")
	require.NotContains(t, text, "https://t.me/spam")
	require.NotContains(t, text, "@spam")
	require.Contains(t, text, "Дополнительно удалено: 1")
	require.Contains(t, text, "очистка не завершена: 1")
	require.NotContains(t, text, "card")
	require.Equal(t, original, c.Text)
	require.Less(t, len([]rune(text)), 4096)
	c.IdentityKind, c.UserName = "sender_chat", "valid_channel"
	require.Contains(t, cardText(c), `href="https://t.me/valid_channel"`)
	c.UserName = `bad"><script>`
	require.NotContains(t, cardText(c), "href=")
}

func TestCompactNoticeDoesNotSendForOrdinaryAcceptance(t *testing.T) {
	require.Empty(t, warningSummary(map[string]int{"ban=pending": 1, "card=pending": 1, "original_delete=pending": 1}, nil, 0))
	require.Empty(t, warningSummary(map[string]int{"ban=confirmed": 1, "original_delete=confirmed": 1}, nil, 0))
	require.Contains(t, warningSummary(map[string]int{"original_delete=failed": 1}, nil, 0), "требует внимания")
	require.Contains(t, warningSummary(nil, nil, 1), "переполнения")
	require.Contains(t, warningSummary(nil, []moderation.Action{{Kind: "original_delete", State: moderation.Pending,
		Attempts: 2, Uncertain: true}}, 0), "требует внимания")
	a, j, _ := cardFixture(t, "detector")
	a.recovery.api = recoveryAPIFunc(func(context.Context, tbapi.Chattable) (*tbapi.APIResponse, error) {
		t.Fatal("ordinary acceptance must not send a summary")
		return nil, nil
	})
	rows, err := j.Snapshot(context.Background())
	require.NoError(t, err)
	for _, op := range rows {
		if op.Kind == "notice" {
			require.Equal(t, moderation.Confirmed, a.recovery.notify(context.Background(), op).State)
		}
	}
}

func TestCompactCardDetailsAndReturnStayBound(t *testing.T) {
	a, _, c := cardFixture(t, "detector")
	var edited tbapi.EditMessageTextConfig
	a.recovery.api = recoveryAPIFunc(func(_ context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
		if m, ok := req.(tbapi.EditMessageTextConfig); ok {
			edited = m
		}
		return apiOK(true), nil
	})
	require.NoError(t, a.recoveryCardCallback(cardQuery(c, "M?", "info")))
	require.Contains(t, edited.Text, "ID=501")
	require.Contains(t, edited.Text, "Срок выполнения:")
	require.Contains(t, edited.Text, "Данные детекции")
	require.Contains(t, edited.ReplyMarkup.InlineKeyboard[0][0].Text, "К карточке")
	require.NoError(t, a.recoveryCardCallback(cardQuery(c, "MB", "back")))
	require.Equal(t, tbapi.ModeHTML, edited.ParseMode)
	require.True(t, edited.LinkPreviewOptions.IsDisabled)
	require.NotContains(t, edited.Text, "Срок выполнения:")
	q := cardQuery(c, "MB", "wrong")
	q.Message.MessageID++
	require.Error(t, a.recoveryCardCallback(q))
}

func TestCompactTelegramUTF16Limit(t *testing.T) {
	text := truncateTelegramText(strings.Repeat("😀", 4000), 4000)
	require.LessOrEqual(t, len(utf16.Encode([]rune(text))), 4000)
	require.True(t, strings.HasSuffix(text, "…"))
	require.Equal(t, "короткий текст", truncateTelegramText("короткий текст", 4000))
	for n, expected := range map[int]string{1: "1 сообщение", 3: "3 сообщения", 5: "5 сообщений", 11: "11 сообщений"} {
		require.Equal(t, expected, messageCount(n))
	}
	c := moderation.Card{DisplayName: strings.Repeat("😀", 200), IdentityID: 501, IdentityKind: "user",
		Text: strings.Repeat("😀<&>", 1000)}
	for i := range 5000 {
		c.Actions = append(c.Actions, moderation.Action{MessageID: i + 1, Kind: "original_delete", State: moderation.Expired,
			MessageDate: time.Now().Add(-49 * time.Hour).Unix()})
	}
	markup := compactCardText(c, strings.Repeat("😀", 150))
	visible := html.UnescapeString(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(markup, ""))
	require.LessOrEqual(t, len(utf16.Encode([]rune(visible))), 4096)
	require.Contains(t, markup, "Остальные результаты")
}
