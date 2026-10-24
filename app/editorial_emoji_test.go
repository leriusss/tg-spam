package main

import (
	"context"
	"github.com/jessevdk/go-flags"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/bot"
	"testing"
)

func TestEditorialEmojiOptions(t *testing.T) {
	for _, env := range []bool{false, true} {
		t.Run(map[bool]string{false: "CLI", true: "environment"}[env], func(t *testing.T) {
			t.Setenv("EDITORIAL_EMOJI_EXEMPTIONS", "")
			var args []string
			if env {
				t.Setenv("EDITORIAL_EMOJI_EXEMPTIONS", "-100:-200,-300:-400")
			} else {
				args = []string{"--editorial-emoji-exemptions=-100:-200,-300:-400"}
			}
			var opts options
			_, err := flags.NewParser(&opts, flags.Default).ParseArgs(args)
			require.NoError(t, err)
			rules, err := bot.ParseEditorialEmojiExemptions(opts.EditorialEmojiExemptions)
			require.NoError(t, err)
			require.True(t, rules.Allows(-100, -200))
			require.True(t, rules.Allows(-300, -400))
			require.False(t, rules.Allows(-300, -200))
		})
	}
	var opts options
	opts.EditorialEmojiExemptions = "-100:editorial-name"
	require.ErrorContains(t, execute(context.Background(), opts), "negative int64", "invalid config is rejected before storage or Telegram startup")
}
