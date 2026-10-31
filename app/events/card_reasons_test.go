package events

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/lib/spamcheck"
)

func TestSavedDetailReasons(t *testing.T) {
	for _, tt := range []struct {
		name, details, want string
	}{
		{"emoji", "8/6", "Превышен лимит эмодзи: обнаружено 8, лимит — 6."},
		{"emoji", "old-format", "Количество и лимит не сохранены или не распознаны"},
		{"emoji", "2/6", unparsedDetailReason},
		{"classifier", "probability of spam: 97.25%", "Классификатор определил спам. Вероятность спама: 97.25%. Порог не сохранён"},
		{"classifier", "probability of spam: 97.25%; threshold: 80.00%", "Вероятность спама: 97.25%. Порог: 80.00%."},
		{"classifier", "", "Вероятность и порог не сохранены или не распознаны"},
		{"classifier", "probability of spam: 1000%", unparsedDetailReason},
		{"classifier", "probability of ham: 97.25%", unparsedDetailReason},
		{"similarity", "0.92/0.80", "Обнаружено сходство с примерами спама. Значение: 0.92; порог: 0.80."},
		{"similarity", "old-format", "Значение и порог не сохранены или не распознаны"},
		{"cas", "spam detected", "CAS: числовая цель найдена в антиспам-базе; сохранён положительный результат."},
		{"cas", "known spammer", "числовая цель найдена в антиспам-базе"},
		{"duplicate", "message repeated 4 times in 1m0s", "Сохранённое число повторений: 4. Период повторов: 60 с."},
		{"duplicate", "message repeated 4 times in instantly", "Повторы зафиксированы одновременно"},
		{"burst", "duplicate_content: repeated 5 times in 60s", "Сохранённое число повторений: 5; окно: 60 с."},
		{"links", "too many links 3/2", "количество ссылок»: обнаружено 3, лимит — 2"},
		{"mentions", "too many mentions 4/2", "количество упоминаний»: обнаружено 4, лимит — 2"},
		{"multi-lingual", "5/2", "смешение письменностей внутри слов»: обнаружено 5, лимит — 2"},
		{"stopword", "запрещённая фраза", "Найденное выражение: «запрещённая фраза»"},
		{"stopword", "error: matched literal", "Найденное выражение: «error: matched literal»"},
		{"external inline button", "message contains an external inline-link button", "внешняя кнопка со ссылкой"},
		{"images", "image with short text (19 chars)", "Длина подписи: 19 символов"},
		{"videos", "video without text", "видео без достаточного текста"},
		{"audio", "audio without text", "аудио без достаточного текста"},
		{"contact", "contact without text", "контакт без текста"},
		{"forward", "forwarded message", "пересылка"},
		{"keyboard", "message with keyboard", "кнопки в сообщении"},
		{"link-only", "message contains links only", "только ссылки"},
		{"giveaway", "giveaway message", "розыгрыш"},
		{"word-spacing", "abnormal (ratio: 0.45, short: 80%)", "Доля пробелов: 0.45; коротких слов: 80%"},
		{"username-symbols", "username contains prohibited symbol '$'", "запрещённый настройкой символ"},
		{"confirmed exact text", "спам-фраза", "точное совпадение со спам-фразой: «спам-фраза»"},
		{"bot-start spam", "start command, integer payload and at least three distinct bot mentions", "минимум трёх разных"},
		{"openai", "реклама, confidence: 98%", "Модель OpenAI определила сообщение как спам"},
	} {
		t.Run(tt.name+"/"+tt.details, func(t *testing.T) {
			check := spamcheck.Response{Name: tt.name, Spam: true, Details: tt.details}
			c := moderation.Card{IdentityID: 501, IdentityKind: "user", Reason: check.String()}
			require.Contains(t, detailReasons(c), tt.want)
			original := c.Reason
			pages := operatorDetailPages(c, time.Now())
			for _, page := range pages {
				assertDetailHTML(t, page)
			}
			require.Equal(t, original, c.Reason)
			// Exercise the actual positive-only reason writer as well as Response.String.
			c.Reason = hanModerationReason([]spamcheck.Response{
				{Name: "han script", Spam: true, Details: "Преобладает письменность Han: 20 из 20 букв (100.0%); порог 70.0%, минимум 20."}, check,
			}, true)
			require.Contains(t, detailReasons(c), tt.want)
		})
	}
}

func TestSavedDetailErrorsAreNotSpamReasons(t *testing.T) {
	for _, reason := range []string{
		"cas: ham, failed to send request: timeout",
		"cas: ham, failed to parse response: Преобладает письменность Han: 20 из 20 букв (100.0%); порог 70.0%, минимум 20.",
		"cas: error, spam detected",
		"cas: ham, record not found",
		"cas: spam, failed to parse response from https://cas.invalid/check: invalid JSON",
		"Независимое основание модерации: cas — failed to make request: timeout",
		"Независимое основание модерации: cas — not found",
		"classifier: error, probability of spam: 97.25%",
		"openai: spam, OpenAI error: timeout",
		"similarity: ham, 0.10/0.80",
		"emoji: ham, 0/6",
	} {
		t.Run(reason, func(t *testing.T) {
			text := detailReasons(moderation.Card{Reason: reason, IdentityID: 501, IdentityKind: "user"})
			require.Contains(t, text, "это не основание спама")
			require.NotContains(t, text, "найдена в антиспам-базе")
			require.NotContains(t, text, "определил спам")
			require.NotContains(t, text, "Превышен лимит")
			require.NotContains(t, text, "Сработало правило Han")
		})
	}
	checks := []spamcheck.Response{{Name: "han script", Spam: true, Details: "Han"},
		{Name: "cas", Error: errors.New("timeout"), Details: "failed to send request: timeout"}}
	require.NotContains(t, hanModerationReason(checks, true), "cas", "actual writer does not persist failed CAS as an independent verdict")
}

func TestSavedDetailReasonEvidenceAndDiagnostics(t *testing.T) {
	c := moderation.Card{IdentityID: 501, IdentityKind: "user", Reason: "emoji: spam, 8/6\nemoji: spam, 8/6\nclassifier: spam, probability of spam: 97.25%\nsimilarity: spam, 0.92/0.80\ncas: spam, spam detected"}
	text := detailReasons(c)
	require.Equal(t, 1, strings.Count(text, "Превышен лимит эмодзи"))
	require.Contains(t, text, "Классификатор определил спам")
	require.Contains(t, text, "сходство с примерами спама")
	require.Contains(t, text, "числовая цель найдена в антиспам-базе")
	require.NotContains(t, text, "Порог: 80.00%", "do not invent a classifier threshold from another check")
	c.IdentityID, c.IdentityKind = 0, "message"
	require.NotContains(t, detailReasons(c), "числовая цель найдена")
	require.Contains(t, detailReasons(c), "числовая цель проверки не установлена")
	for _, reason := range []string{"legacy-v0 score=123 category=?", "cas: timeout", "cas: spam detected", "emoji: 8/6", "ошибка CAS: spam detected", "some new check: spam, 8/6"} {
		c.Reason = reason
		require.Equal(t, unparsedDetailReason, detailReasons(c))
		technical := strings.Join(technicalDetailPages(c), "\n")
		require.Contains(t, technical, safeDetail(reason), "keep safe saved diagnostics even when verdict cannot be parsed")
	}
	c.Reason, c.Text = "", "emoji: spam, 999/1"
	require.Equal(t, "Подробный разбор проверок не сохранён", detailReasons(c), "source text is not saved detector evidence")
}
