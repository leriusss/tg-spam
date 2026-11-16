package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
		local, err := filepath.Abs(opts.DataBaseURL)
		if err != nil {
			return nil, fmt.Errorf("resolve local database: %w", err)
		}
		if filepath.Clean(local) == filepath.Clean(opts.SharedForwardDB) {
			return nil, errors.New("coordination and runtime databases must differ")
		}
	} else if opts.DataBaseURL == opts.SharedForwardDB {
		return nil, errors.New("coordination and runtime databases must differ")
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
