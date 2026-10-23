package main

import (
	"fmt"

	"github.com/umputun/tg-spam/app/bot"
)

func validateHanOptions(opts options) error {
	cfg := bot.HanConfig{Enabled: opts.Han.Enabled, Ratio: opts.Han.Ratio, MinCount: opts.Han.MinCount}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("han configuration: %w", err)
	}
	if cfg.Enabled && !opts.ModerationRecovery {
		return fmt.Errorf("han rule requires --moderation-recovery")
	}
	return nil
}
