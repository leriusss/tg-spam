// Command moderation-migrate-probe verifies additive migration on an offline database copy.
// It never creates a Telegram client or starts a moderation worker.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/umputun/tg-spam/app/moderation"
	"github.com/umputun/tg-spam/app/storage/engine"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("copy-db", "", "offline SQLite copy path")
	gid := flag.String("gid", "tg-spam", "database instance ID")
	flag.Parse()
	if *path == "" {
		return fmt.Errorf("--copy-db is required")
	}
	ctx := context.Background()
	db, err := engine.NewSqlite(*path, *gid)
	if err != nil {
		return fmt.Errorf("opening offline copy failed")
	}
	defer db.Close()
	j, err := moderation.NewJournal(ctx, db, moderation.DefaultLimits())
	if err != nil {
		return fmt.Errorf("offline migration failed")
	}
	// Both boolean values and unfinished work exercise the old backup/export code path.
	p := moderation.Plan{
		Key:          "source:-1:900001:user:7",
		ChatID:       -1,
		MessageID:    900001,
		IdentityID:   7,
		IdentityKind: "user",
		Actions: []moderation.Action{
			{Kind: "original_delete", ChatID: -1, IdentityID: 7, IdentityKind: "user", MessageID: 900001},
		},
	}
	_, _, _, err = j.Accept(ctx, p, time.Now())
	if err != nil {
		return fmt.Errorf("offline fixture preparation failed")
	}
	if _, err = db.Exec(`UPDATE moderation_actions SET uncertain=TRUE WHERE kind='original_delete'`); err != nil {
		return fmt.Errorf("offline fixture failed")
	}
	fmt.Println("offline migration fixture ready; no worker/API started")
	return nil
}
