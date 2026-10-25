#!/usr/bin/env bash
set -euo pipefail

# Temporary fixtures only. No Telegram client, worker or production data.
root=$(git rev-parse --show-toplevel)
scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT
baseline=5781651909865f77083ee76fbbc4e6bb389f8ca5
mkdir "$scratch/old"
git archive "$baseline" | tar -x -C "$scratch/old"
cat > "$scratch/create.go" <<'GO'
package main
import (
 "context"
 "os"
 "time"
 "github.com/umputun/tg-spam/app/moderation"
 "github.com/umputun/tg-spam/app/storage/engine"
)
func main() {
 ctx, now := context.Background(), time.Now()
 for _, kind := range []string{"sqlite", "postgres"} {
  var db *engine.SQL
  var err error
  if kind == "sqlite" { db, err = engine.NewSqlite(os.Getenv("HAN_COMPAT_SQLITE"), "han-compat")
  } else {
   if os.Getenv("COMPAT_POSTGRES_URL") == "" { continue }
   db, err = engine.NewPostgres(ctx, os.Getenv("COMPAT_POSTGRES_URL"), "han-compat")
  }
  must(err)
  j, err := moderation.NewJournal(ctx, db, moderation.DefaultLimits()); must(err)
  must(j.EnableHanBans(ctx, now.Add(-time.Minute)))
  for _, id := range []int64{7, 0} {
   key, identity, msg := "source:-100:1:user:7", "user", 1
   if id == 0 { key, identity, msg = "source:-100:2:message:0", "message", 2 }
   p := moderation.Plan{Key:key, ChatID:-100, MessageID:msg, IdentityID:id, IdentityKind:identity,
    Origin:"detector", Card:true, Destination:-200, Reason:"Han fixture"}
   p.HanActions = []moderation.Action{{Kind:"original_delete", ChatID:-100, MessageID:msg,
    MessageDate:now.Unix(), IdentityID:id, IdentityKind:identity}}
   if id != 0 { p.HanActions = append(p.HanActions, moderation.Action{Kind:"ban", ChatID:-100,
    IdentityID:id, IdentityKind:identity, UntilDate:now.Add(400*24*time.Hour).Unix()}) }
   _, _, fresh, err := j.Accept(ctx,p,now); must(err)
   if !fresh { panic("fixture unexpectedly existed") }
  }
  must(db.Close())
 }
}
func must(err error) { if err != nil { panic("isolated fixture failed") } }
GO
cat > "$scratch/read.go" <<'GO'
package main
import (
 "context"
 "fmt"
 "os"
 "github.com/umputun/tg-spam/app/moderation"
 "github.com/umputun/tg-spam/app/storage/engine"
)
func main() {
 ctx := context.Background()
 for _, kind := range []string{"sqlite", "postgres"} {
  var db *engine.SQL
  var err error
  if kind == "sqlite" { db, err = engine.NewSqlite(os.Getenv("HAN_COMPAT_SQLITE"), "han-compat")
  } else {
   if os.Getenv("COMPAT_POSTGRES_URL") == "" { continue }
   db, err = engine.NewPostgres(ctx, os.Getenv("COMPAT_POSTGRES_URL"), "han-compat")
  }
  must(err)
  j, err := moderation.NewJournal(ctx,db,moderation.DefaultLimits()); must(err)
  for _, key := range []string{"source:-100:1:user:7", "source:-100:2:message:0"} {
   c, err := j.CardView(ctx,key); must(err)
   counts := map[string]int{}
   for _, a := range c.Actions { counts[a.Kind]++ }
   expected := 0
   if c.IdentityID != 0 { expected = 1 }
   if counts["ban"] != expected || counts["original_delete"] != 1 { panic("actions changed") }
   if c.Reason != "Han fixture" { panic("card history changed") }
   for _, d := range c.Origins { if d.Origin != "han:v2" || !d.Active { panic("authority changed") } }
  }
  var since int64
  must(db.GetContext(ctx,&since,db.Adopt(`SELECT since_date FROM moderation_han_policy WHERE gid=?`),db.GID()))
  if since <= 0 { panic("policy boundary lost") }
  if kind == "sqlite" {
   var integrity string
   must(db.GetContext(ctx,&integrity,"PRAGMA integrity_check"))
   if integrity != "ok" { panic("integrity check failed") }
  }
  must(db.Close())
  fmt.Printf("%s %s: schema, actions, origins, card and boundary preserved; no worker/API\n",kind,os.Getenv("HAN_COMPAT_MODE"))
 }
}
func must(err error) { if err != nil { panic("isolated compatibility check failed") } }
GO
export HAN_COMPAT_SQLITE="$scratch/han.sqlite"
cd "$root"
go run "$scratch/create.go"
HAN_COMPAT_MODE=new go run "$scratch/read.go"
cd "$scratch/old"
HAN_COMPAT_MODE=previous go run "$scratch/read.go"
cd "$root"
HAN_COMPAT_MODE=returned go run "$scratch/read.go"
