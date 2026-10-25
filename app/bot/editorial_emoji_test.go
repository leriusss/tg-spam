package bot

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/lib/spamcheck"
	"github.com/umputun/tg-spam/lib/tgspam"
	detectormocks "github.com/umputun/tg-spam/lib/tgspam/mocks"
)

const editorialEmojiText = "normal friendly conversation 💍 💀 💃 📅 ⏰ 🎟️ 🔥 ❗️"

func TestEditorialEmojiConfiguration(t *testing.T) {
	for _, raw := range []string{"", "   ", "-100:-200", " -100 : -200 , -300:-200 ", "-100:-200,-100:-200"} {
		cfg, err := ParseEditorialEmojiExemptions(raw)
		require.NoError(t, err)
		require.Equal(t, strings.TrimSpace(raw) != "", cfg.Allows(-100, -200))
		require.False(t, cfg.Allows(-100, 0))
		require.False(t, cfg.Allows(-100, 200))
		require.False(t, cfg.Allows(100, -200))
		require.NotContains(t, cfg.String(), "-100")
	}
	for _, raw := range []string{"-100", ":-200", "-100:", "-100:0", "0:-200", "100:-200", "-100:200", "-100:@channel", "@group:-200", "-100:-200,", "-100:-200:1", "-9223372036854775809:-200", "-100:-9223372036854775809", "-100:-200,bad"} {
		cfg, err := ParseEditorialEmojiExemptions(raw)
		require.Error(t, err)
		require.False(t, cfg.Allows(-100, -200), "invalid config must not grant a partial rule")
	}
}

func editorialEmojiModel(t *testing.T, config tgspam.Config) *tgspam.Detector {
	t.Helper()
	d := tgspam.NewDetector(config)
	_, err := d.LoadSamples(strings.NewReader(""), []io.Reader{strings.NewReader("win lottery prize now")}, []io.Reader{strings.NewReader("normal friendly conversation")})
	require.NoError(t, err)
	return d
}

func TestEditorialEmojiExactSourceScope(t *testing.T) {
	for _, mode := range []string{"exact", "empty", "wrong chat", "wrong sender", "same name", "From is not SenderChat", "reply context", "forward context"} {
		t.Run(mode, func(t *testing.T) {
			cfg, err := ParseEditorialEmojiExemptions("-100:-200")
			require.NoError(t, err)
			if mode == "empty" {
				cfg = EditorialEmojiExemptions{}
			}
			d := editorialEmojiModel(t, tgspam.Config{MaxAllowedEmoji: 6, MinMsgLen: 1, FirstMessagesCount: 3})
			f := NewSpamFilter(d, SpamConfig{EditorialEmoji: cfg})
			msg := Message{ID: 1, ChatID: -100, From: User{ID: 136817688}, SenderChat: SenderChat{ID: -200, UserName: "editorial"}, Text: editorialEmojiText}
			switch mode {
			case "wrong chat":
				msg.ChatID = -101
			case "wrong sender", "same name":
				msg.SenderChat.ID = -201
			case "From is not SenderChat":
				msg.From.ID = -200
				msg.SenderChat = SenderChat{}
			case "reply context":
				msg.ReplyTo.SenderChat = msg.SenderChat
				msg.SenderChat = SenderChat{}
			case "forward context":
				msg.WithForward = true
				msg.SenderChat = SenderChat{}
			}
			res := f.OnMessage(msg, false)
			emojiFound := false
			for _, c := range res.CheckResults {
				if c.Name == "emoji" {
					emojiFound = true
					require.True(t, c.Spam)
					require.Equal(t, "8/6", c.Details)
				}
			}
			require.Equal(t, mode != "exact", emojiFound)
			if mode == "exact" {
				require.Zero(t, res.BanInterval)
				require.False(t, res.DeleteReplyTo)
				require.False(t, f.IsExplicitTrustedUser(-200))
				require.False(t, f.IsApprovedUserInChat(-200, -100))
			} else {
				require.Positive(t, res.BanInterval)
			}
			require.Equal(t, 6, d.MaxAllowedEmoji)
		})
	}
}

func TestEditorialEmojiPreservesOtherChecks(t *testing.T) {
	for _, mode := range []string{"stopword", "external inline button", "meta", "classifier", "similarity", "cas", "duplicate", "burst", "han"} {
		t.Run(mode, func(t *testing.T) {
			c := tgspam.Config{MaxAllowedEmoji: 6, MinMsgLen: 1, FirstMessagesCount: 100, HistorySize: 20}
			if mode == "duplicate" {
				c.DuplicateDetection.Threshold = 3
				c.DuplicateDetection.Window = time.Minute
			}
			if mode == "classifier" {
				c.MinSpamProbability = 45
			}
			if mode == "similarity" {
				c.SimilarityThreshold = 0.5
			}
			if mode == "cas" {
				c.CasAPI = "https://cas.invalid"
				c.HTTPClient = &detectormocks.HTTPClientMock{DoFunc: func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"ok":true,"description":"spam detected"}`))}, nil
				}}
			}
			d := editorialEmojiModel(t, c)
			if mode == "stopword" {
				_, err := d.LoadStopWords(strings.NewReader("forbidden"))
				require.NoError(t, err)
			}
			if mode == "meta" {
				d.WithMetaChecks(func(spamcheck.Request) spamcheck.Response { return spamcheck.Response{Name: "meta", Spam: true} })
			}
			cfg, err := ParseEditorialEmojiExemptions("-100:-200")
			require.NoError(t, err)
			params := SpamConfig{EditorialEmoji: cfg, Han: HanConfig{Enabled: true, Ratio: 0.7, MinCount: 20}}
			if mode == "burst" {
				params.Burst = BurstConfig{Enabled: true, Threshold: 3, CleanupEnabled: true}
			}
			f := NewSpamFilter(d, params)
			msg := Message{ID: 1, ChatID: -100, From: User{ID: 136817688}, SenderChat: SenderChat{ID: -200}, Text: editorialEmojiText}
			if mode == "stopword" {
				msg.Text += " forbidden"
			}
			if mode == "external inline button" {
				msg.WithExternalLinkButton = true
			}
			if mode == "classifier" || mode == "similarity" {
				msg.Text = "win lottery prize now 💍 💀 💃 📅 ⏰ 🎟️ 🔥 ❗️"
			}
			if mode == "han" {
				msg.Text = strings.Repeat("中", 24) + " 💍 💀 💃 📅 ⏰ 🎟️ 🔥 ❗️"
			}
			count := 1
			if mode == "burst" || mode == "duplicate" {
				count = 3
			}
			var res Response
			for mid := 1; mid <= count; mid++ {
				msg.ID = mid
				res = f.OnMessage(msg, false)
			}
			if mode == "han" {
				require.True(t, res.HanOnly)
				require.Positive(t, res.BanInterval)
				require.Empty(t, d.ApprovedUsers())
			} else {
				require.Positive(t, res.BanInterval)
			}
			found := false
			for _, check := range res.CheckResults {
				require.NotEqual(t, "emoji", check.Name)
				if check.Spam && (check.Name == mode || mode == "duplicate" && strings.Contains(check.Name, "duplicate") || mode == "han" && strings.Contains(check.Details, "Han")) {
					found = true
				}
				if mode == "burst" && check.Name == "burst" {
					require.Len(t, check.ExtraDeleteIDs, 2)
				}
			}
			require.True(t, found, "independent spam reason must survive: %s", mode)
			require.False(t, f.IsExplicitTrustedUser(-200))
			require.Equal(t, 6, d.MaxAllowedEmoji)
		})
	}
}

func TestEditorialEmojiInterleavedSources(t *testing.T) {
	cfg, err := ParseEditorialEmojiExemptions("-100:-200")
	require.NoError(t, err)
	d := editorialEmojiModel(t, tgspam.Config{MaxAllowedEmoji: 6, MinMsgLen: 1, FirstMessagesCount: 100})
	f := NewSpamFilter(d, SpamConfig{EditorialEmoji: cfg})
	for mid := 1; mid <= 5; mid++ {
		editorial := Message{ID: mid, ChatID: -100, SenderChat: SenderChat{ID: -200}, From: User{ID: 136817688}, Text: editorialEmojiText}
		require.Zero(t, f.OnMessage(editorial, false).BanInterval)
		other := editorial
		other.SenderChat.ID = -201
		require.Positive(t, f.OnMessage(other, false).BanInterval)
		other = editorial
		other.ChatID = -101
		require.Positive(t, f.OnMessage(other, false).BanInterval)
		require.Equal(t, 6, d.MaxAllowedEmoji)
	}
}
