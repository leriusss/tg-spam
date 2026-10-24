package tgspam

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/lib/spamcheck"
	"testing"
)

func TestEmojiExemptionCannotBeGrantedByJSON(t *testing.T) {
	var req spamcheck.Request
	require.NoError(t, json.Unmarshal([]byte(`{"SkipEmojiCheck":true,"skip_emoji_check":true,"msg":"💍💀💃📅⏰🎟️🔥❗️"}`), &req))
	require.False(t, req.SkipEmojiCheck)
	d := NewDetector(Config{MaxAllowedEmoji: 6, MinMsgLen: 1})
	spam, checks := d.Check(req)
	require.True(t, spam)
	require.True(t, checks[0].Spam)
	require.Equal(t, "emoji", checks[0].Name)
	req.SkipEmojiCheck = true
	spam, checks = d.Check(req)
	require.False(t, spam)
	for _, c := range checks {
		require.NotEqual(t, "emoji", c.Name)
	}
	raw, err := json.Marshal(req)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "SkipEmojiCheck")
	require.NotContains(t, string(raw), "skip_emoji_check")
}
