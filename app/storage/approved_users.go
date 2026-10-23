package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite" // sqlite driver loaded here

	"github.com/umputun/tg-spam/app/storage/engine"
	"github.com/umputun/tg-spam/lib/approved"
)

// ApprovedUsers is a storage for approved users
type ApprovedUsers struct {
	*engine.SQL
	engine.RWLocker
}

// approvedUsersInfo is a struct to store approved user info in the database
type approvedUsersInfo struct {
	UserID       string    `db:"uid"`
	GroupID      string    `db:"gid"`
	UserName     string    `db:"name"`
	Timestamp    time.Time `db:"timestamp"`
	Source       string    `db:"source"`
	AutoProgress string    `db:"auto_progress"`
}

// all approved users queries
const (
	CmdCreateApprovedUsersTable engine.DBCmd = iota + 100
	CmdCreateApprovedUsersIndexes
	CmdAddApprovedUser
	CmdAddUIDColumn
	CmdAddGIDColumn
	CmdAddSourceColumn
)

// queries holds all approved users queries
var approvedUsersQueries = engine.NewQueryMap().
	Add(CmdCreateApprovedUsersTable, engine.Query{
		Sqlite: `CREATE TABLE IF NOT EXISTS approved_users (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            uid TEXT,
            gid TEXT DEFAULT '',
            name TEXT,
            timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
            source TEXT DEFAULT 'unknown',
            auto_progress TEXT,
            UNIQUE(gid, uid)
        )`,
		Postgres: `CREATE TABLE IF NOT EXISTS approved_users (
            id SERIAL PRIMARY KEY,
            uid TEXT,
            gid TEXT DEFAULT '',
            name TEXT,
            timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            source TEXT DEFAULT 'unknown',
            auto_progress TEXT,
            UNIQUE(gid, uid)
        )`,
	}).
	AddSame(CmdCreateApprovedUsersIndexes, `
        CREATE UNIQUE INDEX IF NOT EXISTS idx_approved_users_identity ON approved_users(gid, uid);
        CREATE INDEX IF NOT EXISTS idx_approved_users_uid ON approved_users(uid);
        CREATE INDEX IF NOT EXISTS idx_approved_users_gid ON approved_users(gid);
        CREATE INDEX IF NOT EXISTS idx_approved_users_name ON approved_users(name);
        CREATE INDEX IF NOT EXISTS idx_approved_users_timestamp ON approved_users(timestamp)
    `).
	Add(CmdAddApprovedUser, engine.Query{
		Sqlite: "INSERT INTO approved_users (uid, gid, name, timestamp, source, auto_progress) VALUES (?, ?, ?, ?, ?, ?) " +
			"ON CONFLICT (gid, uid) DO UPDATE SET name=excluded.name, timestamp=excluded.timestamp, " +
			"source=CASE WHEN excluded.source='unknown' AND (approved_users.source IS NULL OR approved_users.source='') " +
			"THEN approved_users.source ELSE excluded.source END, " +
			"auto_progress=excluded.auto_progress",
		Postgres: "INSERT INTO approved_users (uid, gid, name, timestamp, source, auto_progress) VALUES ($1, $2, $3, $4, $5, $6) " +
			"ON CONFLICT (gid, uid) DO UPDATE SET name=EXCLUDED.name, timestamp=EXCLUDED.timestamp, " +
			"source=CASE WHEN EXCLUDED.source='unknown' AND (approved_users.source IS NULL OR approved_users.source='') " +
			"THEN approved_users.source ELSE EXCLUDED.source END, " +
			"auto_progress=EXCLUDED.auto_progress",
	}).
	Add(CmdAddUIDColumn, engine.Query{
		Sqlite:   "ALTER TABLE approved_users ADD COLUMN uid TEXT",
		Postgres: "ALTER TABLE approved_users ADD COLUMN IF NOT EXISTS uid TEXT",
	}).
	Add(CmdAddGIDColumn, engine.Query{
		Sqlite:   "ALTER TABLE approved_users ADD COLUMN gid TEXT DEFAULT ''",
		Postgres: "ALTER TABLE approved_users ADD COLUMN IF NOT EXISTS gid TEXT DEFAULT ''",
	}).
	Add(CmdAddSourceColumn, engine.Query{
		Sqlite:   "ALTER TABLE approved_users ADD COLUMN source TEXT DEFAULT 'unknown'",
		Postgres: "ALTER TABLE approved_users ADD COLUMN IF NOT EXISTS source TEXT DEFAULT 'unknown'",
	})

// storedAutoProgress keeps unidentified library calls separate from Telegram chat progress.
type storedAutoProgress struct {
	Count int                              `json:"count"`
	Chats map[int64]approved.CheckProgress `json:"chats,omitempty"`
}

// NewApprovedUsers creates a new ApprovedUsers storage
func NewApprovedUsers(ctx context.Context, db *engine.SQL) (*ApprovedUsers, error) {
	if db == nil {
		return nil, fmt.Errorf("db connection is nil")
	}
	res := &ApprovedUsers{SQL: db, RWLocker: db.MakeLock()}
	cfg := engine.TableConfig{
		Name:          "approved_users",
		CreateTable:   CmdCreateApprovedUsersTable,
		CreateIndexes: CmdCreateApprovedUsersIndexes,
		MigrateFunc:   res.migrate,
		QueriesMap:    approvedUsersQueries,
	}
	if err := engine.InitTable(ctx, db, cfg); err != nil {
		return nil, fmt.Errorf("failed to init approved users storage: %w", err)
	}
	return res, nil
}

// Read returns a list of all approved users
func (au *ApprovedUsers) Read(ctx context.Context) ([]approved.UserInfo, error) {
	au.RLock()
	defer au.RUnlock()

	query := au.Adopt("SELECT uid, gid, name, timestamp, COALESCE(NULLIF(source, ''), 'unknown') AS source, " +
		"COALESCE(auto_progress, '') AS auto_progress FROM approved_users WHERE gid = ? ORDER BY uid ASC")
	users := []approvedUsersInfo{}
	if err := au.SelectContext(ctx, &users, query, au.GID()); err != nil {
		return nil, fmt.Errorf("failed to get approved users: %w", err)
	}

	res := make([]approved.UserInfo, len(users))
	for i, u := range users {
		res[i] = approved.UserInfo{
			UserID:    u.UserID,
			UserName:  u.UserName,
			Timestamp: u.Timestamp,
			Source:    u.Source,
		}
		if u.AutoProgress != "" {
			var progress storedAutoProgress
			if err := json.Unmarshal([]byte(u.AutoProgress), &progress); err != nil {
				return nil, fmt.Errorf("invalid approved progress: %w", err)
			}
			if progress.Count < 0 {
				return nil, fmt.Errorf("negative approved progress")
			}
			for _, chat := range progress.Chats {
				if chat.Count < 0 || len(chat.Messages) > 128 {
					return nil, fmt.Errorf("invalid approved chat progress")
				}
			}
			res[i].Count, res[i].Progress = progress.Count, progress.Chats
		}
	}
	log.Printf("[DEBUG] read %d approved users", len(res))
	return res, nil
}

// Write adds a user to the approved list
func (au *ApprovedUsers) Write(ctx context.Context, user approved.UserInfo) error {
	if user.UserID == "" {
		return fmt.Errorf("user id can't be empty")
	}

	au.Lock()
	defer au.Unlock()

	if user.Timestamp.IsZero() {
		user.Timestamp = time.Now()
	}
	if user.Source == "" {
		user.Source = approved.SourceUnknown
	}
	progress, err := json.Marshal(storedAutoProgress{Count: user.Count, Chats: user.Progress})
	if err != nil {
		return fmt.Errorf("encode approved progress: %w", err)
	}

	query, err := approvedUsersQueries.Pick(au.Type(), CmdAddApprovedUser)
	if err != nil {
		return fmt.Errorf("failed to get write query: %w", err)
	}

	_, err = au.ExecContext(ctx, query, user.UserID, au.GID(), user.UserName, user.Timestamp, user.Source, string(progress))
	if err != nil {
		return fmt.Errorf("failed to insert user %+v: %w", user, err)
	}

	log.Printf("[INFO] user %q (%s) added to approved users", user.UserName, user.UserID)
	return nil
}

// Delete removes a user from the approved list
func (au *ApprovedUsers) Delete(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("user id can't be empty")
	}

	au.Lock()
	defer au.Unlock()

	// check if user exists first
	var user approvedUsersInfo
	query := au.Adopt("SELECT uid, gid, name, timestamp, COALESCE(NULLIF(source, ''), 'unknown') AS source " +
		"FROM approved_users WHERE uid = ? AND gid = ?")
	if err := au.GetContext(ctx, &user, query, id, au.GID()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			log.Printf("[DEBUG] approved user %q not found, nothing to delete", id)
			return nil
		}
		return fmt.Errorf("failed to get approved user for id %s: %w", id, err)
	}

	// delete user  "DELETE FROM approved_users WHERE uid = ? AND gid = ?"
	query = au.Adopt("DELETE FROM approved_users WHERE uid = ? AND gid = ?")
	if _, err := au.ExecContext(ctx, query, id, au.GID()); err != nil {
		return fmt.Errorf("failed to delete id %s: %w", id, err)
	}

	log.Printf("[INFO] user %q (%s) deleted from approved users", user.UserName, id)
	return nil
}

// migrate adds nullable progress without rewriting existing source or user data.
func (au *ApprovedUsers) migrate(ctx context.Context, tx *sqlx.Tx, gid string) error {
	identityAdded := false
	for _, column := range []struct{ name, definition string }{
		{"uid", "TEXT"}, {"gid", "TEXT DEFAULT ''"},
		{"source", "TEXT DEFAULT 'unknown'"}, {"auto_progress", "TEXT"},
	} {
		query := "SELECT COUNT(*) FROM pragma_table_info('approved_users') WHERE name=?"
		if au.Type() == engine.Postgres {
			query = "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=current_schema() " +
				"AND table_name='approved_users' AND column_name=?"
		}
		var count int
		if err := tx.GetContext(ctx, &count, au.Adopt(query), column.name); err != nil {
			return fmt.Errorf("inspect approved column %s: %w", column.name, err)
		}
		if count != 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, "ALTER TABLE approved_users ADD COLUMN "+column.name+" "+column.definition); err != nil {
			return fmt.Errorf("add approved column %s: %w", column.name, err)
		}
		identityAdded = identityAdded || column.name == "uid" || column.name == "gid"
	}
	if identityAdded {
		query := au.Adopt("UPDATE approved_users SET uid=CAST(id AS TEXT), gid=? WHERE uid IS NULL OR uid=''")
		if _, err := tx.ExecContext(ctx, query, gid); err != nil {
			return fmt.Errorf("migrate approved identity: %w", err)
		}
	}
	return nil
}
