package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/umputun/tg-spam/app/events"
	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage/engine"
)

func sharedForwardOptions(opts options, peer int64) (*events.SharedForwardConfig, error) {
	if opts.SharedForwardDB == "" {
		return nil, nil
	}
	if !opts.ModerationRecovery || opts.SharedForwardOwner <= 0 || !slices.Contains(opts.SharedForwardPeers, peer) ||
		!slices.Contains(opts.SharedForwardPeers, opts.SharedForwardOwner) ||
		len(opts.SharedForwardPeers) > 5 || opts.AdminGroup == "" {
		return nil, errors.New("shared forwards require recovery, admin chat, owner and a complete roster")
	}
	for i, p := range opts.SharedForwardPeers {
		if p <= 0 || slices.Contains(opts.SharedForwardPeers[:i], p) {
			return nil, errors.New("invalid shared peer roster")
		}
	}
	postgres := strings.HasPrefix(opts.SharedForwardDB, "postgres://")
	if !postgres {
		if !filepath.IsAbs(opts.SharedForwardDB) || !strings.HasSuffix(opts.SharedForwardDB, ".db") {
			return nil, errors.New("shared DB must be a separate absolute .db path or postgres URL")
		}
		localURL := opts.DataBaseURL
		for _, prefix := range []string{"file://", "file:", "sqlite://"} {
			localURL = strings.TrimPrefix(localURL, prefix)
		}
		local, err := filepath.Abs(localURL)
		if err != nil {
			return nil, fmt.Errorf("resolve local database: %w", err)
		}
		shared := filepath.Clean(opts.SharedForwardDB)
		if resolved, e := filepath.EvalSymlinks(local); e == nil {
			local = resolved
		}
		if resolved, e := filepath.EvalSymlinks(shared); e == nil {
			shared = resolved
		}
		if filepath.Clean(local) == shared || runtime.GOOS == "windows" && strings.EqualFold(filepath.Clean(local), shared) {
			return nil, errors.New("coordination and runtime databases must differ")
		}
	} else {
		shared, parseErr := url.Parse(opts.SharedForwardDB)
		if parseErr != nil || shared.Hostname() == "" || shared.Path == "" || shared.Path == "/" {
			return nil, errors.New("shared postgres URL must name a separate database")
		}
		local, parseErr := url.Parse(opts.DataBaseURL)
		if parseErr == nil && local.Scheme == "postgres" &&
			strings.EqualFold(local.Hostname(), shared.Hostname()) && local.Path == shared.Path &&
			postgresPort(local) == postgresPort(shared) {
			return nil, errors.New("coordination and runtime databases must differ")
		}
	}
	config := &events.SharedForwardConfig{Peer: peer, Owner: opts.SharedForwardOwner, Peers: slices.Clone(opts.SharedForwardPeers)}
	config.Open = func(ctx context.Context) (*moderation.SharedStore, func(), error) {
		var db *engine.SQL
		var err error
		// Avoid engine.New's connection-string logging: URLs may contain passwords.
		if postgres {
			db, err = engine.NewPostgres(ctx, opts.SharedForwardDB, "shared-forwards")
		} else {
			db, err = engine.NewSqlite(opts.SharedForwardDB, "shared-forwards")
		}
		if err != nil {
			return nil, nil, errors.New("shared database unavailable")
		}
		store, err := moderation.NewSharedStore(ctx, db, opts.SharedForwardOwner, opts.SharedForwardPeers)
		if err != nil {
			_ = db.Close()
			return nil, nil, errors.New("shared database or roster unavailable")
		}
		return store, func() { _ = db.Close() }, nil
	}
	return config, nil
}

func postgresPort(u *url.URL) string {
	if u.Port() == "" {
		return "5432"
	}
	return u.Port()
}
