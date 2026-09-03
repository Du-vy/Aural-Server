package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// Meta keys held in the meta table.
const (
	// MetaOwnerTokenHash holds the hash of the one-time token that claims the
	// server. The row is deleted the moment the token is redeemed.
	MetaOwnerTokenHash = "owner_token_hash"
	// MetaOwnerUserID holds the id of the identity that owns this server.
	//
	// Ownership is not a role. It is a property of the identity itself, which
	// is what keeps it out of reach of the role editor: no permission grants
	// it, no permission takes it away, and the owner keeps every authority
	// even holding no role at all.
	MetaOwnerUserID = "owner_user_id"
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

// OwnerUserID is the identity that owns this server, or zero when nobody has
// claimed it yet. A stored id that no longer names a user reads as zero too:
// the account being gone is the same as there being no owner.
func (s *Store) OwnerUserID(ctx context.Context) (int64, error) {
	value, err := s.Meta(ctx, MetaOwnerUserID)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, nil
	}

	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE id = ?`, id).Scan(&exists); err != nil {
		return 0, fmt.Errorf("store: read owner: %w", err)
	}
	if exists == 0 {
		return 0, nil
	}
	return id, nil
}

// SetOwnerUserID records who owns the server, replacing any previous owner.
func (s *Store) SetOwnerUserID(ctx context.Context, id int64) error {
	return s.SetMeta(ctx, MetaOwnerUserID, strconv.FormatInt(id, 10))
}
