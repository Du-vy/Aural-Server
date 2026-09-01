package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetLinkPreview returns the cached JSON metadata for a URL hash if it was fetched
// on or after validAfter (UNIX seconds). Returns ErrNotFound if missing or expired.
func (s *Store) GetLinkPreview(ctx context.Context, urlHash string, validAfter int64) (string, error) {
	var dataJSON string
	err := s.db.QueryRowContext(ctx,
		`SELECT data_json FROM link_previews WHERE url_hash = ? AND fetched_at >= ?`,
		urlHash, validAfter,
	).Scan(&dataJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: get link preview: %w", err)
	}
	return dataJSON, nil
}

// SaveLinkPreview inserts or updates the cached preview for a URL hash.
func (s *Store) SaveLinkPreview(ctx context.Context, urlHash, rawURL, dataJSON string, fetchedAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO link_previews (url_hash, url, data_json, fetched_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(url_hash) DO UPDATE SET
		   url = excluded.url,
		   data_json = excluded.data_json,
		   fetched_at = excluded.fetched_at`,
		urlHash, rawURL, dataJSON, fetchedAt,
	)
	if err != nil {
		return fmt.Errorf("store: save link preview: %w", err)
	}
	return nil
}

// PruneLinkPreviews deletes all cached previews older than cutoff (UNIX seconds).
func (s *Store) PruneLinkPreviews(ctx context.Context, cutoff int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM link_previews WHERE fetched_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune link previews: %w", err)
	}
	return res.RowsAffected()
}
