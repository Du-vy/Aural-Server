// Package store is the SQLite persistence layer. It owns the schema, its
// migrations, and every query the gateway runs.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	// Pure Go SQLite. No cgo means the server cross-compiles to every target
	// with a plain "go build", which is the whole point of writing it in Go.
	_ "modernc.org/sqlite"
)

// ErrNotFound is returned by every lookup that finds no row.
var ErrNotFound = errors.New("store: not found")

// ErrConflict is returned when a write violates a uniqueness constraint, such
// as claiming a username somebody already holds.
var ErrConflict = errors.New("store: conflict")

// Store is a handle on the database.
type Store struct {
	db *sql.DB
}

// Open connects to the SQLite file at path, applying every pending migration.
// The file and its parent directory are created if missing.
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := ensureDir(dir); err != nil {
			return nil, err
		}
	}

	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	// SQLite takes one writer at a time. Serialising every connection here
	// trades a little read parallelism, which a chat server of this size never
	// misses, for the complete absence of SQLITE_BUSY retries.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for the rare caller that needs a raw query.
func (s *Store) DB() *sql.DB { return s.db }

// now is the timestamp format used across every table: Unix seconds.
func now() int64 { return time.Now().Unix() }

// tx runs fn inside a transaction, rolling back on error.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
