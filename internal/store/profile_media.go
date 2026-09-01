package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ProfileMedia is the file behind one user's avatar or banner.
//
// It is kept apart from Attachment because the two are reclaimed on opposite
// terms: an attachment with no message is abandoned and swept, while a profile
// picture never belongs to a message and must survive exactly as long as the
// user points at it. Sharing a table would mean the sweep deleting every
// avatar on the server.
//
// The row is also what makes the bytes countable. Without it a restart reads
// the quota back from the attachments table alone and forgets every picture,
// which drifts the ceiling further from the disk with each one uploaded.
type ProfileMedia struct {
	ID     int64
	UserID int64
	// Kind is KindAvatar or KindBanner.
	Kind string
	// StorageKey names the file on disk and forms the unguessable part of its
	// URL, exactly as it does for an attachment.
	StorageKey string
	Filename   string
	Size       int64
	CreatedAt  int64
}

// The two kinds of profile media a user may hold, one of each.
const (
	KindAvatar = "avatar"
	KindBanner = "banner"
)

const profileMediaColumns = `id, user_id, kind, storage_key, filename, size, created_at`

func scanProfileMedia(row interface{ Scan(...any) error }) (ProfileMedia, error) {
	var m ProfileMedia
	err := row.Scan(&m.ID, &m.UserID, &m.Kind, &m.StorageKey, &m.Filename, &m.Size, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ProfileMedia{}, ErrNotFound
	}
	if err != nil {
		return ProfileMedia{}, fmt.Errorf("store: scan profile media: %w", err)
	}
	return m, nil
}

func scanAllProfileMedia(rows *sql.Rows) ([]ProfileMedia, error) {
	defer rows.Close()

	var out []ProfileMedia
	for rows.Next() {
		m, err := scanProfileMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read profile media: %w", err)
	}
	return out, nil
}

// CreateProfileMedia records a stored avatar or banner.
func (s *Store) CreateProfileMedia(ctx context.Context, m ProfileMedia) (ProfileMedia, error) {
	m.CreatedAt = now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO profile_media (user_id, kind, storage_key, filename, size, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		m.UserID, m.Kind, m.StorageKey, m.Filename, m.Size, m.CreatedAt)
	if err != nil {
		return ProfileMedia{}, fmt.Errorf("store: create profile media: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return ProfileMedia{}, fmt.Errorf("store: create profile media: %w", err)
	}
	m.ID = id
	return m, nil
}

// ProfileMediaByStorageKey finds the picture a storage key belongs to, which is
// what lets the download handler serve it under the name and type it was
// uploaded with rather than guessing from the URL.
func (s *Store) ProfileMediaByStorageKey(ctx context.Context, key string) (ProfileMedia, error) {
	return scanProfileMedia(s.db.QueryRowContext(ctx,
		`SELECT `+profileMediaColumns+` FROM profile_media WHERE storage_key = ?`, key))
}

// ReplaceProfileMedia stores one picture and returns whatever it displaced, so
// the caller can unlink the old files and give their room back to the quota.
//
// Both halves happen in one transaction: a user holds at most one avatar, and
// a crash between the delete and the insert would either lose the picture they
// still point at or leave two rows claiming the same slot.
func (s *Store) ReplaceProfileMedia(ctx context.Context, m ProfileMedia) (ProfileMedia, []ProfileMedia, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProfileMedia{}, nil, fmt.Errorf("store: replace profile media: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT `+profileMediaColumns+` FROM profile_media WHERE user_id = ? AND kind = ?`,
		m.UserID, m.Kind)
	if err != nil {
		return ProfileMedia{}, nil, fmt.Errorf("store: read previous profile media: %w", err)
	}
	displaced, err := scanAllProfileMedia(rows)
	if err != nil {
		return ProfileMedia{}, nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM profile_media WHERE user_id = ? AND kind = ?`, m.UserID, m.Kind); err != nil {
		return ProfileMedia{}, nil, fmt.Errorf("store: clear previous profile media: %w", err)
	}

	m.CreatedAt = now()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO profile_media (user_id, kind, storage_key, filename, size, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		m.UserID, m.Kind, m.StorageKey, m.Filename, m.Size, m.CreatedAt)
	if err != nil {
		return ProfileMedia{}, nil, fmt.Errorf("store: create profile media: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return ProfileMedia{}, nil, fmt.Errorf("store: create profile media: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ProfileMedia{}, nil, fmt.Errorf("store: replace profile media: %w", err)
	}

	m.ID = id
	return m, displaced, nil
}

// DeleteProfileMedia removes one row by id. The file is the caller's to unlink:
// the row is what makes it reachable, so it goes first.
func (s *Store) DeleteProfileMedia(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM profile_media WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete profile media: %w", err)
	}
	return nil
}

// ClearProfileMedia drops whatever a user holds of one kind and returns it, for
// the case where a picture is removed rather than replaced.
func (s *Store) ClearProfileMedia(ctx context.Context, userID int64, kind string) ([]ProfileMedia, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+profileMediaColumns+` FROM profile_media WHERE user_id = ? AND kind = ?`, userID, kind)
	if err != nil {
		return nil, fmt.Errorf("store: read profile media: %w", err)
	}
	held, err := scanAllProfileMedia(rows)
	if err != nil {
		return nil, err
	}
	if len(held) == 0 {
		return nil, nil
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM profile_media WHERE user_id = ? AND kind = ?`, userID, kind); err != nil {
		return nil, fmt.Errorf("store: clear profile media: %w", err)
	}
	return held, nil
}

// TotalProfileMediaBytes is what every stored avatar and banner adds up to. It
// is read at startup alongside the attachment total: together they are what the
// server-wide ceiling is measured against.
func (s *Store) TotalProfileMediaBytes(ctx context.Context) (int64, error) {
	var total sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT SUM(size) FROM profile_media`).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: total profile media bytes: %w", err)
	}
	return total.Int64, nil
}

// ProfileMediaKeys lists every storage key a picture still points at, which is
// what a sweep of the upload directory has to keep.
func (s *Store) ProfileMediaKeys(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT storage_key FROM profile_media`)
	if err != nil {
		return nil, fmt.Errorf("store: read profile media keys: %w", err)
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("store: scan profile media key: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read profile media keys: %w", err)
	}
	return keys, nil
}
