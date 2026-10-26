package events

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/moderation"
)

type detailExample struct {
	Name, Title, Want string
	Card              moderation.Card
}

func detailExamples(now time.Time) []detailExample {
	base := moderation.Card{Ref: "0123456789abcdef0123456789abcdef", Receipt: "source:-100:701:user:501", ChatID: -100,
		MessageID: 701, IdentityID: 501, IdentityKind: "user", DisplayName: "Анна Примерова", Text: strings.Repeat("中", 248),
		Reason:     "Преобладает письменность Han: 248 из 248 букв (100.0%); порог 70.0%, минимум 20. Удаление и блокировка учитываются отдельно.",
		RecordedAt: now.Add(-time.Minute).UnixMilli(),
		Actions: []moderation.Action{
			{ID: "delete", Kind: "original_delete", MessageID: 701, ChatID: -100, State: moderation.Confirmed, MessageDate: now.Add(-2 * time.Minute).Unix(), Attempts: 1, Deadline: now.Add(time.Hour).UnixMilli(), NextAt: now.Add(-30 * time.Second).UnixMilli()},
			{ID: "ban", Kind: "ban", State: moderation.Confirmed, Attempts: 1, UntilDate: now.Add(400 * 24 * time.Hour).Unix(), NextAt: now.Add(-20 * time.Second).UnixMilli(), Deadline: now.Add(time.Hour).UnixMilli()},
		}}
	examples := []detailExample{}
	add := func(name, title, want string, change func(*moderation.Card)) {
		c := base
		c.Actions = slices.Clone(base.Actions)
		if change != nil {
			change(&c)
		}
		examples = append(examples, detailExample{name, title, want, c})
	}
	add("01-han-confirmed", "Han: удаление и блокировка подтверждены", "Сообщение удалено, автор заблокирован", nil)
	add("02-historical-han", "Исторический Han: только удаление", "Блокировка не запрашивалась", func(c *moderation.Card) { c.Actions = c.Actions[:1] })
	add("03-ban-waits", "Удалено, блокировка ожидается", "Сообщение удалено, блокировка ожидается", func(c *moderation.Card) {
		c.Actions[1].State, c.Actions[1].Attempts = moderation.Pending, 0
		c.IdentityID, c.IdentityKind, c.DisplayName = -1001, "sender_chat", "Канал «Пример»"
		c.Receipt = "source:-100:701:sender_chat:-1001"
	})
	add("04-ban-refused", "Удалено, блокировка отклонена", "Часть действий не выполнена", func(c *moderation.Card) { c.Actions[1].State = moderation.Failed })
	add("05-delete-expired", "Блокировка подтверждена, удаление просрочено", "срок истёк", func(c *moderation.Card) {
		c.Actions[0].State, c.Actions[0].MessageDate = moderation.Expired, now.Add(-49*time.Hour).Unix()
		c.Actions[0].Deadline = now.Add(-time.Hour).UnixMilli()
	})
	add("06-absent", "Сообщение отсутствует или недоступно", "удаление ботом не подтверждено", func(c *moderation.Card) { c.Actions[0].State = moderation.Absent })
	add("07-unknown-outcome", "Неизвестный результат запроса", "Требуется ручная проверка", func(c *moderation.Card) { c.Actions[1].State, c.Actions[1].Uncertain = moderation.Manual, true })
	add("08-unknown-author", "Автор не установлен", "Автор не установлен", func(c *moderation.Card) {
		c.IdentityID, c.IdentityKind, c.Actions = 0, "message", c.Actions[:1]
		c.Reason += "\nБлокировка по Han невозможна: оригинал не содержит надёжной числовой цели User/SenderChat."
	})
	for _, state := range []string{"pending", "reject_report", "expired"} {
		title := map[string]string{"pending": "ожидание", "reject_report": "отказ", "expired": "истечение"}[state]
		add("09-request-"+state, "Операторский запрос: "+title, "Операторский запрос", func(c *moderation.Card) {
			c.Reason, c.RequestState, c.RequesterID = "", state, -777
			c.RequestExpires = now.Add(time.Minute).UnixMilli()
			if state == "expired" {
				c.RequestExpires = now.Add(-time.Minute).UnixMilli()
			}
			c.Actions = c.Actions[:1]
			c.Actions[0].State, c.Actions[0].Code = moderation.Manual, "sender_command_pending_confirmation"
		})
	}
	add("10-ban-unban-new", "Блокировка, снятие, новое решение", "блокировка ожидается", func(c *moderation.Card) {
		c.Actions[1].NextAt = now.Add(-3 * time.Minute).UnixMilli()
		c.Actions = append(c.Actions, moderation.Action{ID: "unban", Kind: "unban", State: moderation.Confirmed, NextAt: now.Add(-2 * time.Minute).UnixMilli()},
			moderation.Action{ID: "new-ban", Kind: "ban", State: moderation.Pending, NextAt: now.Add(time.Minute).UnixMilli(), Deadline: now.Add(time.Hour).UnixMilli()})
	})
	add("11-long-history", "Длинные Unicode, история и очистка", "Очистка других сообщений", func(c *moderation.Card) {
		c.Reason, c.Text = "", strings.Repeat("🧑🏽‍💻東京é<&> ", 500)+" https://example.invalid/unsafe"
		for i := range 120 {
			c.Actions = append(c.Actions, moderation.Action{ID: fmt.Sprintf("cleanup-%d", i), Kind: "cleanup_delete", MessageID: 702 + i, State: moderation.Confirmed, Attempts: 3})
			c.Audit = append(c.Audit, moderation.CardAudit{Type: "decision", ID: fmt.Sprintf("decision-%d", i), Kind: "approve", State: "done"})
		}
	})
	add("12-old-incomplete", "Старая карточка без детекции и даты", "Подробный разбор проверок не сохранён", func(c *moderation.Card) {
		c.Reason, c.RecordedAt, c.Actions = "", 0, c.Actions[:1]
		c.Actions[0].MessageDate = 0
	})
	return examples
}

func assertDetailHTML(t *testing.T, s string) {
	t.Helper()
	require.True(t, utf8.ValidString(s))
	require.LessOrEqual(t, len(utf16.Encode([]rune(s))), 4096)
	d := xml.NewDecoder(strings.NewReader("<root>" + s + "</root>"))
	for {
		_, err := d.Token()
		if err == io.EOF {
			break
		}
		require.NoError(t, err, s)
	}
	require.NotContains(t, s, "href=")
	require.NotContains(t, s, "map[")
}

func TestOperatorDetailsFixturesAndExport(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	a := &admin{recovery: &recovery{}}
	var document strings.Builder
	document.WriteString(`<!doctype html><meta charset="utf-8"><title>Подробнее — реальные renderer fixtures</title><style>
body{background:#101a26;color:#eff4fa;font:16px/1.45 Arial;margin:0;padding:20px}main{display:flex;flex-wrap:wrap;gap:24px;align-items:flex-start}section{width:360px;min-width:0}h2{font-size:17px;color:#9eb4ca}article{background:#1c2c3d;border-radius:14px;padding:16px;white-space:pre-wrap;overflow-wrap:anywhere}blockquote{margin:8px 0;padding:8px;border-left:3px solid #78c4e4;background:#152232}pre{white-space:pre-wrap;overflow-wrap:anywhere;font:13px/1.4 Consolas}nav{display:flex;flex-direction:column;gap:4px}nav div{display:flex;gap:4px}button{flex:1;min-width:0;background:#234057;color:#eff4fa;border:0;border-radius:5px;padding:10px;font:14px Arial}</style><h1>Экран «Подробнее»</h1><p>Синтетические данные. Настоящие Go renderer и клавиатуры; HTML-предпросмотр, не Telegram-клиент. Кнопки не выполняют действий.</p><main>`)
	for _, example := range detailExamples(now) {
		t.Run(example.Name, func(t *testing.T) {
			pages := operatorDetailPages(example.Card, now)
			require.Contains(t, strings.Join(pages, "\n"), example.Want)
			for _, page := range append(slices.Clone(pages), technicalDetailPages(example.Card)...) {
				assertDetailHTML(t, page)
			}
			if example.Name == "02-historical-han" {
				require.NotContains(t, pages[0], "автор заблокирован")
			}
			if example.Name == "08-unknown-author" {
				require.NotContains(t, strings.Join(pages, "\n"), "Анна")
			}
			if example.Name == "10-ban-unban-new" {
				require.NotContains(t, pages[0], "автор заблокирован")
				require.Contains(t, pages[0], "Снятие блокировки — подтверждено")
			}
			if example.Name == "11-long-history" {
				require.Greater(t, len(pages), 10)
				joined := strings.Join(technicalDetailPages(example.Card), "\n")
				for i := range 120 {
					require.Contains(t, joined, fmt.Sprintf("action ID: cleanup-%d\n", i))
					require.Contains(t, joined, fmt.Sprintf("ID: decision-%d\n", i))
				}
			}
			if example.Name == "12-old-incomplete" {
				require.Contains(t, strings.Join(pages, "\n"), "Настоящая дата сообщения неизвестна")
			}
			buttons := a.detailKeyboard(example.Card, 0, len(pages), -1, 0)
			fmt.Fprintf(&document, "<section id=%q><h2>%s</h2><article>%s</article><nav>", example.Name, html.EscapeString(example.Title), pages[0])
			for _, row := range buttons.InlineKeyboard {
				document.WriteString("<div>")
				for _, button := range row {
					fmt.Fprintf(&document, "<button>%s</button>", html.EscapeString(button.Text))
				}
				document.WriteString("</div>")
			}
			document.WriteString("</nav></section>")
			if dir := os.Getenv("DETAILS_PREVIEW_DIR"); dir != "" {
				require.NoError(t, os.MkdirAll(dir, 0o755))
				labels := func(keys tbapi.InlineKeyboardMarkup) [][]string {
					rows := [][]string{}
					for _, row := range keys.InlineKeyboard {
						names := []string{}
						for _, button := range row {
							names = append(names, button.Text)
						}
						rows = append(rows, names)
					}
					return rows
				}
				tech := technicalDetailPages(example.Card)
				detailButtons, techButtons := [][][]string{}, [][][]string{}
				for i := range pages {
					detailButtons = append(detailButtons, labels(a.detailKeyboard(example.Card, i, len(pages), -1, 0)))
				}
				for i := range tech {
					techButtons = append(techButtons, labels(a.detailKeyboard(example.Card, 0, len(pages), i, len(tech))))
				}
				raw, err := json.MarshalIndent(struct {
					Title                           string
					Details, Technical              []string
					DetailButtons, TechnicalButtons [][][]string
				}{example.Title, pages, tech, detailButtons, techButtons}, "", "  ")
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(dir, example.Name+".json"), raw, 0o600))
				plain := html.UnescapeString(regexp.MustCompile(`</?(?:b|pre|blockquote)>`).ReplaceAllString(strings.Join(pages, "\n\n"), ""))
				lines := strings.Split(plain, "\n")
				for i := range lines {
					lines[i] = strings.TrimRight(lines[i], " \t")
				}
				require.NoError(t, os.WriteFile(filepath.Join(dir, example.Name+".txt"), []byte(strings.Join(lines, "\n")), 0o600))
			}
		})
	}
	document.WriteString("</main>")
	if dir := os.Getenv("DETAILS_PREVIEW_DIR"); dir != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "examples.html"), []byte(document.String()), 0o600))
	}
}

func TestDetailNavigationDoesNotModerate(t *testing.T) {
	a, j, c := cardFixture(t, "detector")
	var edited tbapi.EditMessageTextConfig
	a.recovery.api = recoveryAPIFunc(func(_ context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
		switch q := req.(type) {
		case tbapi.EditMessageTextConfig:
			edited = q
			if !strings.HasPrefix(q.Text, "🛡") {
				assertDetailHTML(t, q.Text)
			}
			require.True(t, q.LinkPreviewOptions.IsDisabled)
		case tbapi.CallbackConfig:
		default:
			t.Fatalf("navigation issued %T", req)
		}
		return apiOK(true), nil
	})
	before, err := j.ReadCardView(context.Background(), c.Ref)
	require.NoError(t, err)
	for _, verb := range []string{"M?", "MP0", "MT0.0", "MP0", "MB", "M?"} {
		require.NoError(t, a.recoveryCardCallback(cardQuery(c, verb, "duplicate-navigation")))
	}
	for _, row := range edited.ReplyMarkup.InlineKeyboard {
		for _, b := range row {
			require.NotEqual(t, "Подробнее", b.Text)
		}
	}
	after, err := j.ReadCardView(context.Background(), c.Ref)
	require.NoError(t, err)
	require.Equal(t, before, after)
	for _, verb := range []string{"MP-1", "MP9999999", "MT0.-1", "MTx.0", "MT0.99999"} {
		require.Error(t, a.recoveryCardCallback(cardQuery(c, verb, "invalid")))
	}
	bad := cardQuery(c, "M?", "forged")
	bad.Message.MessageID++
	require.Error(t, a.recoveryCardCallback(bad))
	bad = cardQuery(c, "MT0.0", "bad-user")
	bad.From.IsBot = true
	require.Error(t, a.recoveryCardCallback(bad))
	bad = cardQuery(c, "M?", "wrong-chat")
	bad.Message.Chat.ID++
	require.Error(t, a.recoveryCardCallback(bad))
}

func TestDetailsRedactionAndDeadlines(t *testing.T) {
	now := time.Now()
	c := detailExamples(now)[0].Card
	token := "123456789:" + strings.Repeat("x", 35)
	c.Text = "<b>text & 😀</b> https://api.telegram.org/bot" + token + "/getMe https://example.invalid/?token=private"
	c.Reason = c.Text
	before := c.Text
	for _, page := range append(operatorDetailPages(c, now), technicalDetailPages(c)...) {
		assertDetailHTML(t, page)
		require.NotContains(t, page, token)
		require.NotContains(t, page, "token=private")
		require.NotContains(t, page, "https://")
	}
	require.Equal(t, before, c.Text, "display redaction must not alter training input")
	c.Actions[0].State, c.Actions[0].Attempts, c.Actions[0].Deadline = moderation.Pending, 3, now.Add(-time.Second).UnixMilli()
	text := strings.Join(operatorDetailPages(c, now), "\n")
	require.NotContains(t, text, "Автоматический повтор назначен")
	require.Contains(t, text, "Автоматический повтор не обещается")
	c.Resolved = []string{c.Actions[0].ID}
	c.Actions[0].State = moderation.Failed
	text = strings.Join(operatorDetailPages(c, now), "\n")
	require.Contains(t, text, "это не подтверждает успех Telegram API")
	require.NotContains(t, text, "Сообщение удалено, автор заблокирован")
}

func TestDetailsPageReturnAndOriginalDate(t *testing.T) {
	a, j, _ := cardFixture(t, "detector")
	now := time.Now()
	p := moderation.Plan{Key: "source:123:702:user:501", ChatID: 123, MessageID: 702, IdentityKind: "user", IdentityID: 501,
		Origin: "detector", Card: true, Destination: 456, SourceText: strings.Repeat("😀東京<&> ", 800),
		Actions: []moderation.Action{{Kind: "original_delete", ChatID: 123, MessageID: 702, MessageDate: now.Add(-4 * time.Hour).Unix(), IdentityID: 501, IdentityKind: "user"}}}
	_, _, _, err := j.Accept(context.Background(), p, now)
	require.NoError(t, err)
	require.NoError(t, j.BindCard(context.Background(), p.Key, 600))
	c, err := j.ReadCardView(context.Background(), p.Key)
	require.NoError(t, err)
	pages := operatorDetailPages(c, now)
	require.Greater(t, len(pages), 3)
	var edit tbapi.EditMessageTextConfig
	a.recovery.api = recoveryAPIFunc(func(_ context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
		if e, ok := req.(tbapi.EditMessageTextConfig); ok {
			edit = e
		}
		return apiOK(true), nil
	})
	require.NoError(t, a.recoveryCardCallback(cardQuery(c, "MP2", "page")))
	require.Contains(t, edit.Text, fmt.Sprintf("Страница 3 из %d", len(pages)))
	tech := *edit.ReplyMarkup.InlineKeyboard[1][0].CallbackData
	require.Equal(t, "MT2.0:"+c.Ref, tech)
	q := cardQuery(c, "M?", "tech")
	q.Data = tech
	require.NoError(t, a.recoveryCardCallback(q))
	back := *edit.ReplyMarkup.InlineKeyboard[0][0].CallbackData
	require.Equal(t, "MP2:"+c.Ref, back)
	q.Data = back
	require.NoError(t, a.recoveryCardCallback(q))
	require.Contains(t, edit.Text, fmt.Sprintf("Страница 3 из %d", len(pages)))
	after, err := j.ReadCardView(context.Background(), c.Ref)
	require.NoError(t, err)
	require.Equal(t, c, after)
	full := strings.Join(pages, "\n")
	require.Contains(t, full, "Дата оригинала: "+cardDate(p.Actions[0].MessageDate))
	// No observation/receipt timestamp can fill in a missing true date.
	c.Actions[0].MessageDate = 0
	full = strings.Join(operatorDetailPages(c, now), "\n")
	require.Contains(t, full, "Настоящая дата сообщения неизвестна")
}

func TestDetailsRequestNavigationAuthorizationAndStaleRef(t *testing.T) {
	a, _, u := originalDeleteFixture(t, 501)
	require.NoError(t, a.MsgHandler(u))
	j := a.recovery.journal
	c, err := j.ReadCardView(context.Background(), "delete:123:801")
	require.NoError(t, err)
	require.NoError(t, j.BindCard(context.Background(), c.Receipt, 600))
	edits := 0
	a.recovery.api = recoveryAPIFunc(func(_ context.Context, req tbapi.Chattable) (*tbapi.APIResponse, error) {
		switch req.(type) {
		case tbapi.EditMessageTextConfig:
			edits++
		case tbapi.CallbackConfig:
		default:
			t.Fatalf("navigation API %T", req)
		}
		return apiOK(true), nil
	})
	before, err := j.ReadCardView(context.Background(), c.Ref)
	require.NoError(t, err)
	for _, verb := range []string{"M?", "MT0.0", "MP0", "MB"} {
		require.NoError(t, a.recoveryCardCallback(cardQuery(c, verb, "same-id")))
	}
	after, err := j.ReadCardView(context.Background(), c.Ref)
	require.NoError(t, err)
	require.Equal(t, before, after)
	allowedEdits := edits
	q := cardQuery(c, "MT0.0", "unauthorized")
	q.From.ID = 12
	require.NoError(t, a.recoveryCardCallback(q))
	require.Equal(t, allowedEdits, edits)
	// A terminal request can be explicitly replaced; its old navigation must not open the new card.
	require.NoError(t, a.recoveryCardCallback(cardQuery(c, "MX", "reject")))
	c, err = j.CardView(context.Background(), c.Receipt)
	require.NoError(t, err)
	require.NoError(t, a.recoveryCardCallback(cardQuery(c, "MN", "renew")))
	current, err := j.ReadCardView(context.Background(), c.Receipt)
	require.NoError(t, err)
	require.NotEqual(t, c.Ref, current.Ref)
	allowedEdits = edits
	for _, verb := range []string{"M?", "MP0", "MT0.0", "MB"} {
		require.NoError(t, a.recoveryCardCallback(cardQuery(c, verb, "stale")))
	}
	require.Equal(t, allowedEdits, edits)
	after, err = j.ReadCardView(context.Background(), current.Ref)
	require.NoError(t, err)
	require.Equal(t, current, after)
}

func TestDetailsIndependentReasonsAndNoInventedConfirmation(t *testing.T) {
	now := time.Now()
	c := detailExamples(now)[0].Card
	han := c.Reason
	stop := "Независимое основание модерации: стоп-слово — internal stopword details"
	burst := "Независимое основание модерации: повторяющиеся сообщения (burst) — internal burst details"
	c.Reason = strings.Join([]string{han, han, stop, stop, burst}, "\n")
	c.RequestState = "pending"
	text := detailReasons(c)
	require.Equal(t, 1, strings.Count(text, "Сработало правило Han"))
	require.Equal(t, 1, strings.Count(text, "Отдельное автоматическое правило: стоп-слово"))
	require.Equal(t, 1, strings.Count(text, "серия повторяющихся сообщений"))
	require.Contains(t, text, "Операторский запрос")
	require.NotContains(t, text, "internal")
	c.RecordedAt = 0
	c.Actions[0].Attempts = 3
	var history strings.Builder
	for _, section := range detailHistory(c, now) {
		history.WriteString(section.body)
	}
	require.Contains(t, history.String(), "Попыток обработки: 3")
	require.NotContains(t, history.String(), cardDate(c.Actions[0].NextAt/1000), "scheduling is not an API confirmation date")
	c.Actions[0].State = moderation.Absent
	c.Actions[1].State = moderation.Failed
	c.Reason = "Telegram подтвердил: всё выполнено"
	c.Text = "Сообщение удалено, автор заблокирован"
	require.Equal(t, "Действия не выполнены; проверьте результаты", detailResult(c, now), "free text cannot determine journal outcomes")
}

func TestDetailsCanceledActionIsNotAwaited(t *testing.T) {
	now := time.Now()
	c := detailExamples(now)[0].Card
	c.Actions[1].State = moderation.Canceled
	require.Equal(t, "Часть действий отменена до выполнения", detailResult(c, now))
	require.Contains(t, detailActions(c, now), "Блокировка автора — отменено до выполнения")
	require.NotContains(t, safeDetail("пример.рф/path"), "пример.рф")
}
