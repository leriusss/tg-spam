package bot

import (
	"math"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/lib/approved"
	"github.com/umputun/tg-spam/lib/tgspam"
)

func TestHanCounts(t *testing.T) {
	for _, tc := range []struct {
		name, text   string
		han, letters int
	}{
		{"Han", "你好世界", 4, 4},
		{"mixed", "你好世界 привет", 4, 10},
		{"single", "Иероглиф 中", 1, 9},
		{"title", "Название 東京", 2, 10},
		{"link", "https://例子.测试/你好", 0, 0},
		{"neutral", "123 😀!?", 0, 0},
		{"supplementary", "𠀀𰀀", 2, 2},
		{"Japanese", "日本語の文章です", 5, 8},
		{"other scripts", "Русский Ελληνικά العربية 한국어", 0, 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			han, letters := HanCounts(tc.text)
			require.Equal(t, tc.han, han)
			require.Equal(t, tc.letters, letters)
		})
	}
}

func TestHanConfig(t *testing.T) {
	for _, ratio := range []float64{0, 0.5, 1.1, math.NaN(), math.Inf(1)} {
		require.Error(t, (HanConfig{Enabled: true, Ratio: ratio, MinCount: 20}).Validate())
	}
	require.Error(t, (HanConfig{Enabled: true, Ratio: 0.7, MinCount: 1}).Validate())
	require.NoError(t, (HanConfig{}).Validate())
	require.NoError(t, (HanConfig{Enabled: true, Ratio: 0.7, MinCount: 20}).Validate())
}

func TestHanBeforeAutoBypassAndWithoutLearning(t *testing.T) {
	for _, mode := range []string{"auto", "manual", "disabled", "forward", "quote", "reply", "mixed", "short", "below count", "below ratio", "ratio boundary"} {
		t.Run(mode, func(t *testing.T) {
			d := tgspam.NewDetector(tgspam.Config{FirstMessageOnly: true, MaxAllowedEmoji: -1, MinMsgLen: 1})
			filter := NewSpamFilter(d, SpamConfig{Han: HanConfig{Enabled: mode != "disabled", Ratio: 0.7, MinCount: 20}})
			msg := Message{ID: 1, From: User{ID: 42}, Text: "обычный текст"}
			filter.OnMessage(msg, false)
			filter.OnMessage(msg, false)
			require.Equal(t, approved.SourceAuto, d.ApprovedUsers()[0].Source)
			if mode == "manual" {
				require.NoError(t, d.AddApprovedUser(approved.UserInfo{UserID: "42"}))
			}
			msg.Text = strings.Repeat("你好世界", 6)
			want := mode == "auto" || mode == "mixed" || mode == "ratio boundary"
			switch mode {
			case "mixed":
				msg.Text += " привет"
			case "forward":
				msg.WithForward = true
			case "quote":
				msg.Quote, msg.Text = msg.Text, "Удалите эту рекламу"
			case "reply":
				msg.ReplyTo.Text, msg.Text = msg.Text, "Удалите эту рекламу"
			case "below count":
				msg.Text = strings.Repeat("中", 19)
			case "below ratio":
				msg.Text = strings.Repeat("中", 20) + strings.Repeat("я", 9)
			case "short":
				msg.Text = "東京"
			case "ratio boundary":
				msg.Text = strings.Repeat("中", 21) + strings.Repeat("я", 9)
			}
			before := d.ApprovedUsers()
			resp := filter.OnMessage(msg, false)
			require.Equal(t, want, resp.HanDeleteOnly)
			require.Zero(t, resp.BanInterval)
			if want {
				require.True(t, resp.DeleteReplyTo)
				require.Equal(t, msg.ID, resp.ReplyTo)
				require.Equal(t, before, d.ApprovedUsers(), "Han must not change trust or train")
				require.Contains(t, resp.CheckResults[0].Details, "Преобладает письменность Han")
				require.Empty(t, resp.CheckResults[0].ExtraDeleteIDs)
			}
		})
	}
}

func TestHanIncident(t *testing.T) {
	text, err := os.ReadFile("testdata/han_incident.txt")
	require.NoError(t, err)
	han, letters := HanCounts(string(text))
	require.Equal(t, 248, han)
	require.Equal(t, han, letters)
	d := tgspam.NewDetector(tgspam.Config{MaxAllowedEmoji: -1})
	s := NewSpamFilter(d, SpamConfig{Han: HanConfig{Enabled: true, Ratio: 0.7, MinCount: 20}})
	resp := s.OnMessage(Message{ID: 1, From: User{ID: 42}, Text: string(text)}, false)
	require.True(t, resp.HanDeleteOnly)
}

func TestHanOwnText(t *testing.T) {
	text := "😀" + strings.Repeat("中", 24) + " привет"
	for _, kind := range []string{"blockquote", "expandable_blockquote", "url"} {
		entities := []Entity{{Type: kind, Offset: 2, Length: 24}}
		han, letters := HanCounts(HanOwnText(text, &entities))
		require.Zero(t, han)
		require.Equal(t, 6, letters)
	}
	entities := []Entity{{Type: "text_link", Offset: 2, Length: 24, URL: "https://example.invalid"}}
	han, letters := HanCounts(HanOwnText(text, &entities))
	require.Equal(t, 24, han)
	require.Equal(t, 30, letters)
	malformed := []Entity{{Type: "blockquote", Offset: -1, Length: 999}, {Type: "url", Offset: 2, Length: 999}}
	require.Equal(t, text, HanOwnText(text, &malformed))
}
