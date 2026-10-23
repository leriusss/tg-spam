package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/umputun/tg-spam/app/storage/engine"
	"github.com/umputun/tg-spam/lib/approved"
)

func (s *StorageTestSuite) TestApprovedProgressMigration() {
	ctx := context.Background()
	for _, dbt := range s.getTestDB() {
		db := dbt.DB
		s.Run(string(db.Type()), func() {
			defer db.Exec("DROP TABLE approved_users")
			// start with the schema understood by the production binary, without progress.
			idColumn := "INTEGER PRIMARY KEY"
			if db.Type() == engine.Postgres {
				idColumn = "SERIAL PRIMARY KEY"
			}
			_, err := db.Exec(`CREATE TABLE approved_users (
				id ` + idColumn + `, uid TEXT, gid TEXT DEFAULT '', name TEXT,
				timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP, source TEXT DEFAULT 'unknown', UNIQUE(gid,uid))`)
			s.Require().NoError(err)
			stamp := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
			for i, source := range []any{approved.SourceAuto, approved.SourceManual, "", nil, "unknown", "import-v1"} {
				_, err = db.Exec(db.Adopt("INSERT INTO approved_users (id,uid,gid,name,timestamp,source) VALUES (?,?,?,?,?,?)"),
					i+1, fmt.Sprint(i), db.GID(), "fixture", stamp, source)
				s.Require().NoError(err)
			}
			// preserve a raw snapshot: NULL/empty/unrecognized provenance is not rewritten.
			type row struct {
				ID             int
				UID, GID, Name string
				Timestamp      time.Time
				Source         *string
			}
			var before, after []row
			query := "SELECT id,uid,gid,name,timestamp,source FROM approved_users ORDER BY id"
			s.Require().NoError(db.Select(&before, query))
			users, err := NewApprovedUsers(ctx, db)
			s.Require().NoError(err)
			_, err = NewApprovedUsers(ctx, db)
			s.Require().NoError(err)
			s.Require().NoError(db.Select(&after, query))
			s.Equal(before, after)
			loaded, err := users.Read(ctx)
			s.Require().NoError(err)
			s.Require().Len(loaded, 6)
			for _, user := range loaded {
				s.Zero(user.Count)
				s.Empty(user.Progress)
			}
			user := loaded[0]
			user.Count = 2
			user.Progress = map[int64]approved.CheckProgress{-100: {Count: 3, Messages: []int{5, 7, 6}}}
			s.Require().NoError(users.Write(ctx, user))
			loaded, err = users.Read(ctx)
			s.Require().NoError(err)
			s.Equal(user, loaded[0])
			// UPSERT retains the original row ID; the old adapter's explicit SELECT remains valid.
			var id int
			s.Require().NoError(db.Get(&id, db.Adopt("SELECT id FROM approved_users WHERE uid=? AND gid=?"), user.UserID, db.GID()))
			s.Equal(1, id)
			var oldRows []approvedUsersInfo
			s.Require().NoError(db.Select(&oldRows, db.Adopt("SELECT uid,gid,name,timestamp,"+
				"COALESCE(NULLIF(source,''),'unknown') AS source FROM approved_users WHERE gid=? ORDER BY uid"), db.GID()))
			s.Len(oldRows, 6)
			_, err = db.Exec(db.Adopt("UPDATE approved_users SET auto_progress=? WHERE uid=? AND gid=?"),
				"not json", user.UserID, db.GID())
			s.Require().NoError(err)
			_, err = users.Read(ctx)
			s.ErrorContains(err, "invalid approved progress")
		})
	}
}
