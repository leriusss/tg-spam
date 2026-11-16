// Command shared-forward-card-resolve binds one unknown inquiry to a message
// independently verified by the operator. It never contacts Telegram.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
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
	path := flag.String("sqlite", "", "existing coordination SQLite file; otherwise SHARED_RESOLVE_POSTGRES_URL")
	gid := flag.String("gid", "shared-forwards", "exact coordination database instance ID")
	owner := flag.Int64("owner", 0, "configured numeric owner bot ID")
	roster := flag.String("peers", "", "exact configured comma-separated numeric bot IDs")
	ref := flag.String("ref", "", "exact inquiry ref from shared_cases")
	revision := flag.Int("revision", 0, "current saved inquiry revision")
	admin := flag.Int64("admin", 0, "verified admin chat ID")
	message := flag.Int("message", 0, "existing message ID independently verified as this inquiry's card")
	apply := flag.Bool("apply", false, "confirm targeted binding after backup and independent message verification")
	flag.Parse()
	pg := os.Getenv("SHARED_RESOLVE_POSTGRES_URL")
	if !*apply || *gid == "" || *ref == "" || *revision <= 0 || *owner <= 0 || *admin == 0 || *message <= 0 ||
		(*path == "") == (pg == "") {
		return errors.New("require --apply, exact owner/peers/ref/revision/admin/message and exactly one existing coordination DB")
	}
	peers := []int64{}
	for item := range strings.SplitSeq(*roster, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(item), 10, 64)
		if err != nil || id <= 0 {
			return errors.New("invalid numeric peer roster")
		}
		peers = append(peers, id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var db *engine.SQL
	var err error
	if *path != "" {
		if info, statErr := os.Stat(*path); statErr != nil || !info.Mode().IsRegular() {
			return errors.New("existing coordination SQLite file required")
		}
		db, err = engine.NewSqlite(*path, *gid)
	} else {
		db, err = engine.NewPostgres(ctx, pg, *gid)
	}
	if err != nil {
		return errors.New("coordination database connection failed")
	}
	defer db.Close()
	var configs int
	if err = db.GetContext(ctx, &configs, db.Adopt(`SELECT count(*) FROM shared_config WHERE gid=?`), *gid); err != nil || configs != 1 {
		return errors.New("existing coordination roster required; no new database will be initialized")
	}
	store, err := moderation.NewSharedStore(ctx, db, *owner, peers)
	if err != nil {
		return errors.New("coordination schema or roster mismatch")
	}
	if err = store.ResolveUnknown(ctx, *owner, *ref, *revision, *admin, *message); err != nil {
		return fmt.Errorf("targeted binding refused: %w", err)
	}
	fmt.Println("Bound the verified existing card; revision invalidated. No send or moderation action was performed.")
	return nil
}
