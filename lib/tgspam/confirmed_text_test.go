package tgspam

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/lib/approved"
	"github.com/umputun/tg-spam/lib/spamcheck"
)

func TestDetector_ConfirmedExactText(t *testing.T) {
	for _, tc := range []struct {
		text string
		spam bool
	}{
		{"Есть проба 0.5г", true},
		{"  ЕСТЬ   ПРОБА  0.5Г  ", true},
		{"Есть\u00a0проба 0.5г", true},
		{"Есть про\u200bба 0.5г", true},
		{"Есть проба 0.5г 🔥", true},
		{"Есть\nпроба 0.5г", false}, // control removal joins words, matching the existing stopword pipeline
		{"проба микрофона", false},
		{"проба золота", false},
		{"есть проба?", false},
		{"Добавьте 0.5г соли", false},
		{"Есть проба 0.5г?", false},
		{"Есть проба 0,5г", false},
		{"Есть проба 1г", false},
		{"Жалоба: «Есть проба 0.5г»", false},
		{"«Есть проба 0.5г»", false},
		{"связь", false}, {"тест", false}, {"привет", false},
	} {
		t.Run(tc.text, func(t *testing.T) {
			d := NewDetector(Config{MaxAllowedEmoji: -1, MinMsgLen: 1, FirstMessageOnly: true, FirstMessagesCount: 1})
			_, err := d.LoadStopWords(strings.NewReader("=есть проба 0.5г"))
			require.NoError(t, err)
			require.Equal(t, tc.spam, d.isStopWord(d.cleanText(tc.text), spamcheck.Request{}).Spam)
			spam, _ := d.Check(spamcheck.Request{UserID: "42", Msg: tc.text, CheckOnly: true})
			require.Equal(t, tc.spam, spam)
		})
	}
}

func TestDetector_ConfirmedExactTextApproval(t *testing.T) {
	d := NewDetector(Config{MaxAllowedEmoji: -1, MinMsgLen: 1, FirstMessageOnly: true, FirstMessagesCount: 1})
	spam, _ := d.Check(spamcheck.Request{UserID: "42", Msg: "обычное сообщение"})
	require.False(t, spam)
	require.Equal(t, approved.SourceAuto, d.approvedUsers["42"].Source)
	for range 2 {
		matched, checks := d.Check(spamcheck.Request{UserID: "42", Msg: "Есть проба 0.5г"})
		require.True(t, matched)
		require.Equal(t, "confirmed exact text", checks[0].Name)
	}
	require.NoError(t, d.AddApprovedUser(approved.UserInfo{UserID: "42"}))
	spam, checks := d.Check(spamcheck.Request{UserID: "42", Msg: "Есть проба 0.5г"})
	require.False(t, spam)
	require.Equal(t, "pre-approved", checks[0].Name)
}

func TestDetector_ConfirmedExactTextContext(t *testing.T) {
	d := NewDetector(Config{MaxAllowedEmoji: -1, MinMsgLen: 1})
	for _, own := range []string{"", "Удалите это сообщение", "Есть проба 0.5г"} {
		spam, _ := d.Check(spamcheck.Request{
			Msg: "Есть проба 0.5г", AuthoredText: &own, CheckOnly: true,
		})
		require.Equal(t, own == "Есть проба 0.5г", spam)
	}
}
