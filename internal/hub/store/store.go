// Package store owns the SQLite database of the hub: schema, migrations and
// every query. No other package writes SQL.
package store

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

var ErrNotFound = errors.New("store: not found")

type Store struct {
	db *sql.DB
}

// Open opens (or creates) the database at path and applies pending migrations.
// Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := "file::memory:?_pragma=foreign_keys(1)"
	if path != ":memory:" {
		dsn = "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// In-memory databases are per connection; keep exactly one.
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
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	current, err := s.SchemaVersion()
	if err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var n int
		if _, err := fmt.Sscanf(name, "%04d_", &n); err != nil {
			return fmt.Errorf("migration name %q: %w", name, err)
		}
		if n <= current {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		// Two separate Exec calls: a multi-statement Exec with a bound parameter
		// is not portable across drivers.
		if _, err := tx.Exec(`DELETE FROM schema_version`); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`INSERT INTO schema_version(version) VALUES (?)`, n); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		current = n
	}
	return nil
}

func (s *Store) SchemaVersion() (int, error) {
	var v sql.NullInt64
	err := s.db.QueryRow(`SELECT max(version) FROM schema_version`).Scan(&v)
	if err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

func (s *Store) GetSetting(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// SettingInt returns the integer value of a setting or def when absent or invalid.
func (s *Store) SettingInt(key string, def int) int {
	v, ok, err := s.GetSetting(key)
	if err != nil || !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// Time helpers: every timestamp is stored as RFC3339 UTC so lexical order == chronological order.
func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }
