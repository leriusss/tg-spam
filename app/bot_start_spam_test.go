package main

import (
	"os"
	"testing"

	"github.com/jessevdk/go-flags"
	"github.com/stretchr/testify/require"
)

func TestBotStartSpamOptions(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		args      []string
		want      bool
	}{
		{name: "disabled by default"},
		{name: "environment", env: "true", want: true},
		{name: "explicitly disabled", env: "false"},
		{name: "CLI", args: []string{"--bot-start-spam.enabled"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BOT_START_SPAM_ENABLED", tc.env)
			if tc.env == "" {
				require.NoError(t, os.Unsetenv("BOT_START_SPAM_ENABLED"))
			}
			var opts options
			_, err := flags.NewParser(&opts, flags.None).ParseArgs(tc.args)
			require.NoError(t, err)
			require.Equal(t, tc.want, opts.BotStartSpam.Enabled)
		})
	}
}
