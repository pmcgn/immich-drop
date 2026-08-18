// Package store wraps the local SQLite state database (dedupe cache, invites,
// upload audit log). It runs the same idempotent migrations as the Python
// version against pre-existing state.db files, and additionally repairs the
// legacy no-underscore upload_events schema (a known bug in the Python code:
// db_init created uploadedat/useragent/immichassetid while all inserts and
// reads used the snake_case names, so audit logging silently failed).
package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the state database and applies migrations.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// Serialize access; the workload is light and this avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS uploads (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			checksum TEXT UNIQUE,
			filename TEXT,
			size INTEGER,
			device_asset_id TEXT,
			immich_asset_id TEXT,
			created_at TEXT,
			inserted_at TEXT DEFAULT CURRENT_TIMESTAMP
		);`); err != nil {
		return fmt.Errorf("create uploads: %w", err)
	}

	if err := s.migrateUploadEvents(); err != nil {
		return err
	}

	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS invites (
			token TEXT PRIMARY KEY,
			album_id TEXT,
			album_name TEXT,
			max_uses INTEGER DEFAULT 1,
			used_count INTEGER DEFAULT 0,
			expires_at TEXT,
			created_at TEXT DEFAULT CURRENT_TIMESTAMP
		);`); err != nil {
		return fmt.Errorf("create invites: %w", err)
	}
	// Best-effort column additions for databases created by older versions;
	// "duplicate column name" errors are expected and ignored.
	for _, ddl := range []string{
		"ALTER TABLE invites ADD COLUMN claimed INTEGER DEFAULT 0",
		"ALTER TABLE invites ADD COLUMN claimed_at TEXT",
		"ALTER TABLE invites ADD COLUMN claimed_by_session TEXT",
		"ALTER TABLE invites ADD COLUMN password_hash TEXT",
		"ALTER TABLE invites ADD COLUMN owner_user_id TEXT",
		"ALTER TABLE invites ADD COLUMN owner_email TEXT",
		"ALTER TABLE invites ADD COLUMN owner_name TEXT",
		"ALTER TABLE invites ADD COLUMN name TEXT",
		"ALTER TABLE invites ADD COLUMN disabled INTEGER DEFAULT 0",
	} {
		if _, err := s.db.Exec(ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			slog.Debug("invites migration step skipped", "ddl", ddl, "err", err)
		}
	}
	return nil
}

// migrateUploadEvents standardizes upload_events on the snake_case schema,
// renaming columns of a legacy table created by the buggy Python db_init.
func (s *Store) migrateUploadEvents() error {
	cols, err := s.tableColumns("upload_events")
	if err != nil {
		return err
	}
	renames := map[string]string{
		"uploadedat":    "uploaded_at",
		"useragent":     "user_agent",
		"immichassetid": "immich_asset_id",
	}
	for old, new_ := range renames {
		if cols[old] && !cols[new_] {
			if _, err := s.db.Exec(fmt.Sprintf(
				"ALTER TABLE upload_events RENAME COLUMN %s TO %s", old, new_)); err != nil {
				return fmt.Errorf("rename upload_events.%s: %w", old, err)
			}
			slog.Info("migrated legacy upload_events column", "from", old, "to", new_)
		}
	}
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS upload_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			token TEXT,
			uploaded_at TEXT DEFAULT CURRENT_TIMESTAMP,
			ip TEXT,
			user_agent TEXT,
			fingerprint TEXT,
			filename TEXT,
			size INTEGER,
			checksum TEXT,
			immich_asset_id TEXT
		);`); err != nil {
		return fmt.Errorf("create upload_events: %w", err)
	}
	return nil
}

func (s *Store) tableColumns(table string) (map[string]bool, error) {
	rows, err := s.db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[strings.ToLower(name)] = true
	}
	return cols, rows.Err()
}
