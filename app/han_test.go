package main

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestValidateHanOptions(t *testing.T) {
	var opts options
	require.NoError(t, validateHanOptions(opts))
	opts.Han.Enabled, opts.Han.Ratio, opts.Han.MinCount = true, 0.7, 20
	require.ErrorContains(t, validateHanOptions(opts), "moderation-recovery")
	opts.ModerationRecovery = true
	require.NoError(t, validateHanOptions(opts))
	opts.Han.Ratio = 0.5
	require.Error(t, validateHanOptions(opts))
}
