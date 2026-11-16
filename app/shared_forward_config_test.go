package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jessevdk/go-flags"
	"github.com/stretchr/testify/require"
	"github.com/umputun/tg-spam/app/moderation"
)

func TestSharedForwardConfigDisabledAndScoped(t *testing.T) {
	config, err := sharedForwardOptions(options{}, 100)
	require.NoError(t, err)
	require.Nil(t, config)
	path := filepath.Join(t.TempDir(), "coord.db")
	opts := options{SharedForwardDB: path, SharedForwardOwner: 100, SharedForwardPeers: []int64{100, 101}, ModerationRecovery: true, AdminGroup: "456", DataBaseURL: filepath.Join(t.TempDir(), "runtime.db")}
	config, err = sharedForwardOptions(opts, 101)
	require.NoError(t, err)
	require.EqualValues(t, 100, config.Owner)
	require.Equal(t, moderation.ForwardAllPolicy, config.Policy)
	store, closeDB, err := config.Open(context.Background())
	require.NoError(t, err)
	defer closeDB()
	require.NotNil(t, store)
	opts.SharedForwardPolicy = "legacy"
	legacy, err := sharedForwardOptions(opts, 101)
	require.NoError(t, err)
	require.Empty(t, legacy.Policy)
	opts.SharedForwardPolicy = "unexpected"
	_, err = sharedForwardOptions(opts, 101)
	require.Error(t, err)
	for _, bad := range []options{
		{SharedForwardDB: path},
		{SharedForwardDB: path, SharedForwardOwner: 100, SharedForwardPeers: []int64{100, 100}, ModerationRecovery: true, AdminGroup: "456"},
		{SharedForwardDB: path, SharedForwardOwner: 100, SharedForwardPeers: []int64{100}, ModerationRecovery: true, AdminGroup: "456", DataBaseURL: path},
		{SharedForwardDB: path, SharedForwardOwner: 100, SharedForwardPeers: []int64{100}, ModerationRecovery: true, AdminGroup: "456", DataBaseURL: "file:" + path},
		{SharedForwardDB: "postgres://worker:private@db:5432/runtime?sslmode=require", SharedForwardOwner: 100, SharedForwardPeers: []int64{100}, ModerationRecovery: true, AdminGroup: "456", DataBaseURL: "postgres://different@db/runtime?sslmode=disable"},
		{SharedForwardDB: "postgres://worker:private@db/", SharedForwardOwner: 100, SharedForwardPeers: []int64{100}, ModerationRecovery: true, AdminGroup: "456"},
	} {
		_, err = sharedForwardOptions(bad, 100)
		require.Error(t, err)
	}
}

func TestSharedForwardConfigRosterEnvironment(t *testing.T) {
	t.Setenv("SHARED_FORWARD_PEERS", "100,101,102,103,104")
	var opts options
	_, err := flags.NewParser(&opts, flags.None).ParseArgs(nil)
	require.NoError(t, err)
	require.Equal(t, []int64{100, 101, 102, 103, 104}, opts.SharedForwardPeers)
}
