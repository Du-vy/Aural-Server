package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
)

// migrations are applied in order. Each entry moves the schema from version
// index to index+1, so entries are append-only: never edit one that has
// shipped, add another below it.
var migrations = []string{
	// 1: initial schema.
	`
	CREATE TABLE users (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		nickname      TEXT    NOT NULL,
		username      TEXT    UNIQUE COLLATE NOCASE,
		password_hash TEXT,
		registered_at INTEGER,
		created_at    INTEGER NOT NULL,
		last_seen_at  INTEGER NOT NULL
	);

	CREATE TABLE tokens (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		token_hash   TEXT    NOT NULL UNIQUE,
		label        TEXT    NOT NULL DEFAULT '',
		created_at   INTEGER NOT NULL,
		last_used_at INTEGER NOT NULL
	);
	CREATE INDEX idx_tokens_user ON tokens(user_id);

	CREATE TABLE channels (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		parent_id  INTEGER REFERENCES channels(id) ON DELETE CASCADE,
		name       TEXT    NOT NULL,
		type       TEXT    NOT NULL,
		topic      TEXT    NOT NULL DEFAULT '',
		position   INTEGER NOT NULL DEFAULT 0,
		user_limit INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE INDEX idx_channels_parent ON channels(parent_id);

	CREATE TABLE roles (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		name        TEXT    NOT NULL,
		color       TEXT    NOT NULL DEFAULT '',
		permissions INTEGER NOT NULL DEFAULT 0,
		position    INTEGER NOT NULL DEFAULT 0,
		hoist       INTEGER NOT NULL DEFAULT 0,
		managed     TEXT    NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL
	);
	CREATE UNIQUE INDEX idx_roles_managed ON roles(managed) WHERE managed <> '';

	CREATE TABLE user_roles (
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		role_id INTEGER NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
		PRIMARY KEY (user_id, role_id)
	);

	CREATE TABLE channel_overwrites (
		channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
		role_id    INTEGER NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
		allow      INTEGER NOT NULL DEFAULT 0,
		deny       INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (channel_id, role_id)
	);

	CREATE TABLE meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	`,
}

// migrate brings the schema up to len(migrations) using SQLite's own
// user_version counter, so no bookkeeping table is needed.
func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("store: database schema is version %d but this build only knows %d, refusing to downgrade",
			version, len(migrations))
	}

	for i := version; i < len(migrations); i++ {
		err := s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return fmt.Errorf("store: migration %d: %w", i+1, err)
			}
			// PRAGMA does not take a bound parameter.
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
				return fmt.Errorf("store: stamp schema version %d: %w", i+1, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return s.seed(ctx)
}

// seed installs the managed roles and a starter channel tree the first time a
// database is opened. It is a no-op on an existing server.
func (s *Store) seed(ctx context.Context) error {
	var roleCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM roles`).Scan(&roleCount); err != nil {
		return fmt.Errorf("store: count roles: %w", err)
	}
	if roleCount > 0 {
		return nil
	}

	ts := now()
	return s.tx(ctx, func(tx *sql.Tx) error {
		insertRole := func(name, color string, perms permissions.Permission, position int, hoist bool, managed string) error {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO roles (name, color, permissions, position, hoist, managed, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				name, color, int64(perms), position, boolToInt(hoist), managed, ts)
			return err
		}
		if err := insertRole("everyone", "", permissions.DefaultEveryone, 0, false, protocol.ManagedEveryone); err != nil {
			return fmt.Errorf("store: seed everyone role: %w", err)
		}
		if err := insertRole("Member", "#3ba55d", permissions.DefaultRegistered, 1, false, protocol.ManagedRegistered); err != nil {
			return fmt.Errorf("store: seed registered role: %w", err)
		}
		if err := insertRole("Admin", "#e8544a", permissions.DefaultAdmin, 100, true, protocol.ManagedAdmin); err != nil {
			return fmt.Errorf("store: seed admin role: %w", err)
		}

		res, err := tx.ExecContext(ctx,
			`INSERT INTO channels (parent_id, name, type, position, created_at) VALUES (NULL, ?, ?, 0, ?)`,
			"General", protocol.ChannelCategory, ts)
		if err != nil {
			return fmt.Errorf("store: seed category: %w", err)
		}
		categoryID, err := res.LastInsertId()
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO channels (parent_id, name, type, topic, position, created_at) VALUES (?, ?, ?, ?, 0, ?)`,
			categoryID, "general", protocol.ChannelText, "Welcome to Aural", ts); err != nil {
			return fmt.Errorf("store: seed text channel: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO channels (parent_id, name, type, position, created_at) VALUES (?, ?, ?, 1, ?)`,
			categoryID, "Lobby", protocol.ChannelVoice, ts); err != nil {
			return fmt.Errorf("store: seed voice channel: %w", err)
		}
		return nil
	})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: create %s: %w", dir, err)
	}
	return nil
}

// isUniqueViolation recognises the constraint failures the callers turn into
// ErrConflict.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
