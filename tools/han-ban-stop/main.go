// Command han-ban-stop withdraws one receipt's unfinished Han ban authority.
// Stop workers and export the journal first. This tool never contacts Telegram.
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
	path := flag.String("sqlite", "", "existing SQLite path; otherwise HAN_STOP_POSTGRES_URL")
	gid := flag.String("gid", "", "exact database instance ID")
	receipt := flag.String("receipt", "", "exact saved receipt ID")
	apply := flag.Bool("apply", false, "confirm withdrawal; workers must be stopped")
	flag.Parse()
	pg := os.Getenv("HAN_STOP_POSTGRES_URL")
	if !*apply || *gid == "" || *receipt == "" || (*path == "") == (pg == "") {
		return fmt.Errorf("require --apply, --gid, --receipt and exactly one SQLite path or PostgreSQL URL environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var db *engine.SQL
	var err error
	if *path != "" {
		if info, statErr := os.Stat(*path); statErr != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("existing SQLite database required")
		}
		db, err = engine.NewSqlite(*path, *gid)
	} else {
		db, err = engine.NewPostgres(ctx, pg, *gid)
	}
	if err != nil {
		return fmt.Errorf("database connection failed")
	}
	defer db.Close()
	j, err := moderation.NewJournal(ctx, db, moderation.DefaultLimits())
	if err != nil {
		return fmt.Errorf("journal initialization failed")
	}
	if _, err = j.CardView(ctx, *receipt); err != nil {
		return fmt.Errorf("exact saved receipt not found")
	}
	if err = j.StopHanBan(ctx, *receipt, time.Now()); err != nil {
		return fmt.Errorf("withdrawal failed; inspect journal before retry")
	}
	fmt.Println("Unfinished Han ban demand withdrawn; deletion and independent demands retained. Confirmed effects unchanged.")
	return nil
}
