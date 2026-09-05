// Package store is the SQLite persistence layer. It owns the schema, its
// migrations, and every query the gateway runs.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
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
	db   *sql.DB
	path string
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
		"&_pragma=synchronous(NORMAL)" +
		// Every transaction this package opens is a write, and several of them
		// read a row before rewriting it — replacing an avatar, claiming the
		// files of a message. Under the default deferred locking such a
		// transaction takes the write lock only when it reaches its first
		// write, and if another connection has written in the meantime SQLite
		// refuses it with SQLITE_BUSY_SNAPSHOT: the one busy error that
		// busy_timeout cannot wait out, because the transaction is reading
		// from a snapshot that can no longer be extended.
		//
		// Taking the lock up front is what removes that case entirely, which
		// is what makes opening the pool below safe. A read-only transaction
		// still gets a plain deferred BEGIN, so readers never queue behind
		// this. See modernc.org/sqlite's tx.go.
		"&_txlock=immediate"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	// SQLite in WAL mode takes one writer at a time but any number of readers
	// alongside it, and this server reads far more than it writes — every
	// history page, every search, every attachment served.
	//
	// The pool used to be one connection, which made those reads serialise
	// with everything else: a search walks the whole of a channel's history,
	// and while it did so nothing else on the server could touch the database
	// at all. Measured on two hundred thousand messages with four people
	// searching, sending a message took 173ms; with the pool open it takes
	// 1.4ms, and the searches themselves finish sooner too.
	//
	// The width is what the machine can actually read in parallel, with a
	// floor because a connection waiting on the disk is not using a core, and
	// a ceiling because each one carries its own page cache and past a point
	// they only queue.
	db.SetMaxOpenConns(poolSize())
	// Idle connections are kept rather than closed and reopened: a new
	// connection re-runs the whole _pragma list above, and foreign_keys is
	// per-connection, so churning them would pay for that on the way into
	// queries rather than once.
	db.SetMaxIdleConns(poolSize())
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}

	s := &Store{db: db, path: path}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// poolSize is how many connections the database is read through at once.
const (
	minPoolConns = 4
	maxPoolConns = 16
)

func poolSize() int {
	n := runtime.NumCPU()
	return max(minPoolConns, min(n, maxPoolConns))
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for the rare caller that needs a raw query.
func (s *Store) DB() *sql.DB { return s.db }

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// DatabaseFileSize returns the total bytes occupied by the database file
// and its associated write-ahead logs on disk.
func (s *Store) DatabaseFileSize() int64 {
	if s.path == "" || s.path == ":memory:" {
		return 0
	}
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := filepath.Glob(s.path + suffix); err == nil && len(fi) > 0 {
			for _, p := range fi {
				if stat, err := os.Stat(p); err == nil {
					total += stat.Size()
				}
			}
		}
	}
	return total
}

// now is the timestamp format used across every table: Unix seconds.
func now() int64 { return time.Now().Unix() }

// tx runs fn inside a transaction, rolling back on error.
//
// It is BEGIN IMMEDIATE, from the _txlock in the DSN: every transaction here
// writes, and taking the write lock at the start rather than at the first
// write is what keeps one that reads a row before rewriting it from being
// refused outright when another connection got there first.
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
