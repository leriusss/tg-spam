#!/usr/bin/env bash
set -euo pipefail

# Runs only against temporary fixtures and an optional isolated PostgreSQL service.
root=$(git rev-parse --show-toplevel)
scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT
baseline=3f5def3cd3845ace790a66c2af595e49ac32f252
mkdir "$scratch/old"
git archive "$baseline" | tar -x -C "$scratch/old"
cat > "$scratch/check.go" <<'GO'
package main

import (
 "context"
 "fmt"
 "os"

 "github.com/umputun/tg-spam/app/storage"
 "github.com/umputun/tg-spam/app/storage/engine"
 "github.com/umputun/tg-spam/lib/approved"
 "github.com/umputun/tg-spam/lib/spamcheck"
 "github.com/umputun/tg-spam/lib/tgspam"
)

func main() {
 ctx := context.Background()
 for _, kind := range []string{"sqlite", "postgres"} {
  var db *engine.SQL
  var err error
  if kind == "sqlite" {
   db, err = engine.NewSqlite(os.Getenv("COMPAT_SQLITE_PATH"), "auto-progress-compat")
  } else {
   url := os.Getenv("COMPAT_POSTGRES_URL")
   if url == "" { continue }
   db, err = engine.NewPostgres(ctx, url, "auto-progress-compat")
  }
  must(err)
  users, err := storage.NewApprovedUsers(ctx, db)
  must(err)
  if os.Getenv("COMPAT_MODE") == "write" {
   must(users.Write(ctx, approved.UserInfo{UserID:"auto", Source:approved.SourceAuto, Count:2}))
   must(users.Write(ctx, approved.UserInfo{UserID:"manual", Source:approved.SourceManual}))
  }
  rows, err := users.Read(ctx)
  must(err)
  if len(rows) != 2 { panic("fixture rows lost") }
  for _, row := range rows {
   if row.UserID == "auto" && os.Getenv("COMPAT_MODE") != "old" && row.Count != 2 {
    panic("persisted count changed")
   }
   if row.UserID == "manual" && row.Source != approved.SourceManual { panic("manual source lost") }
  }
  d := tgspam.NewDetector(tgspam.Config{FirstMessagesCount:3, MaxAllowedEmoji:-1})
  _, err = d.WithUserStorage(users)
  must(err)
  _, checks := d.Check(spamcheck.Request{UserID:"auto", Msg:"normal friendly conversation", CheckOnly:true})
  bypass := len(checks) > 0 && checks[0].Name == "pre-approved"
  if bypass != (os.Getenv("COMPAT_MODE") == "old") { panic("unexpected trust behavior") }
  must(db.Close())
  fmt.Printf("%s %s: readable; expected trust behavior verified\n", kind, os.Getenv("COMPAT_MODE"))
 }
}
func must(err error) { if err != nil { panic(err) } }
GO
export COMPAT_SQLITE_PATH="$scratch/compat.sqlite"
cd "$root"
COMPAT_MODE=write go run "$scratch/check.go"
cd "$scratch/old"
COMPAT_MODE=old go run "$scratch/check.go"
cd "$root"
COMPAT_MODE=verify go run "$scratch/check.go"
