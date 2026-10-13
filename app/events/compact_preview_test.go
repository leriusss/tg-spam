package events

import (
	"fmt"
	"html"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/moderation"
)

func TestCompactPreview(t *testing.T) {
	path := os.Getenv("COMPACT_PREVIEW_PATH")
	if path == "" {
		t.Skip("set COMPACT_PREVIEW_PATH to export synthetic review examples")
	}
	now := time.Now()
	base := moderation.Card{Ref: "example", DisplayName: "Анна Примерова", IdentityID: 501, IdentityKind: "user",
		Text: "Добрый день, набираем людей на временную подработку… https://example.com"}
	ban := moderation.Action{Kind: "ban", State: moderation.Confirmed}
	deleteOp := moderation.Action{Kind: "original_delete", State: moderation.Confirmed}
	var document strings.Builder
	document.WriteString(`<!doctype html><meta charset="utf-8"><title>Карточки модерации — примеры</title>
<style>body{background:#0e1621;color:#e9eef5;font:16px/1.45 Arial;margin:30px}main{display:grid;
grid-template-columns:repeat(3,minmax(280px,1fr));gap:24px;max-width:1450px}section{min-width:0}
h2{font-size:17px;color:#9bb2c7}article{background:#182533;border-radius:14px;padding:18px;
white-space:pre-wrap;overflow-wrap:anywhere}a{color:#55baff;text-decoration:none}nav{display:grid;
grid-template-columns:1fr 1fr;gap:3px;margin-top:4px}button{background:#203343;border:0;
color:#e9eef5;border-radius:6px;padding:12px 6px;font:14px Arial}small{color:#8da3b7}</style>
<h1>Карточки модерации</h1><p>Синтетические примеры из Go renderer. Кнопки в этом макете не выполняют операции.</p><main>`)
	for _, tc := range []struct {
		title   string
		actions []moderation.Action
	}{
		{"Успех", []moderation.Action{ban, deleteOp}},
		{"Промежуточное состояние", []moderation.Action{ban, {Kind: "original_delete", State: moderation.Pending}}},
		{"Автоматический повтор", []moderation.Action{ban, {Kind: "original_delete", State: moderation.Pending,
			Attempts: 1, NextAt: now.Add(time.Minute).UnixMilli(), Deadline: now.Add(time.Hour).UnixMilli()}}},
		{"Частичная очистка", []moderation.Action{ban, deleteOp, {Kind: "cleanup_delete", State: moderation.Confirmed},
			{Kind: "cleanup_delete", State: moderation.Confirmed}, {Kind: "cleanup_delete", State: moderation.Confirmed},
			{Kind: "cleanup_delete", State: moderation.Failed}}},
		{"Истекло окно удаления", []moderation.Action{ban, {Kind: "original_delete", State: moderation.Expired,
			MessageDate: now.Add(-49 * time.Hour).Unix()}}},
		{"Сообщение недоступно", []moderation.Action{{Kind: "ban", State: moderation.Confirmed,
			Code: "reconciled_membership"}, {Kind: "original_delete", State: moderation.Absent}}},
	} {
		c := base
		c.Actions = tc.actions
		fmt.Fprintf(&document, "<section><h2>%s</h2><article>%s</article><nav>", html.EscapeString(tc.title),
			compactCardText(c, "Балтия"))
		for _, row := range cardKeyboard(c, true).InlineKeyboard {
			for _, button := range row {
				fmt.Fprintf(&document, "<button>%s</button>", html.EscapeString(button.Text))
			}
		}
		document.WriteString("</nav></section>")
	}
	fmt.Fprintf(&document, "<section><h2>Подробнее</h2><article>%s</article><nav><button>К карточке</button></nav></section>",
		html.EscapeString(cardDetails(moderation.Card{Ref: "example", IdentityID: 501, IdentityKind: "user", ChatID: 123, MessageID: 701,
			Actions: []moderation.Action{ban, deleteOp}})))
	document.WriteString("</main>")
	require.NoError(t, os.WriteFile(path, []byte(document.String()), 0o600))
}
