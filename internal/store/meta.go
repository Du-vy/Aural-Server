package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Meta keys held in the meta table.
const (
	// MetaOwnerTokenHash holds the hash of the one-time token that grants the
	// admin role. The row is deleted the moment the token is redeemed.
	MetaOwnerTokenHash = "owner_token_hash"
)

// Meta reads a value from the key/value table.
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: read meta %s: %w", key, err)
	}
	return value, nil
}

// SetMeta writes a value, replacing any previous one.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("store: write meta %s: %w", key, err)
	}
	return nil
}

// DeleteMeta removes a key, idempotently.
func (s *Store) DeleteMeta(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, key); err != nil {
		return fmt.Errorf("store: delete meta %s: %w", key, err)
	}
	return nil
}

// CountUsersWithRole reports how many users hold a role explicitly. It is what
// tells the server whether anybody has claimed ownership yet.
func (s *Store) CountUsersWithRole(ctx context.Context, roleID int64) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM user_roles WHERE role_id = ?`, roleID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count role members: %w", err)
	}
	return n, nil
}
