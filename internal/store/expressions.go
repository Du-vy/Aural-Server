package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The two kinds of expression. They share a table because they are the same
// object — a name, a picture, and one namespace everybody on the server writes
// into — and differ only in where a client draws them: an emoji goes inline in
// a line of text, a sticker is the whole of a message.
const (
	KindEmoji   = "emoji"
	KindSticker = "sticker"
)

// Expression is one custom emoji or sticker.
type Expression struct {
	ID          int64
	Kind        string
	Name        string
	StorageKey  string
	Filename    string
	ContentType string
	Size        int64
	Animated    bool
	CreatorID   *int64
	CreatedAt   int64
}

const expressionColumns = `id, kind, name, storage_key, filename, content_type, size,
	animated, creator_id, created_at`

func scanExpression(row interface{ Scan(...any) error }) (Expression, error) {
	var (
		e        Expression
		animated int
	)
	err := row.Scan(&e.ID, &e.Kind, &e.Name, &e.StorageKey, &e.Filename, &e.ContentType,
		&e.Size, &animated, &e.CreatorID, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Expression{}, ErrNotFound
	}
	if err != nil {
		return Expression{}, fmt.Errorf("store: scan expression: %w", err)
	}
	e.Animated = animated != 0
	return e, nil
}

// AllExpressions lists every custom emoji and sticker, oldest first. It is read
// once at startup into the hub's cache and again after every write, exactly as
// the role and channel tables are: a message is rendered against it.
func (s *Store) AllExpressions(ctx context.Context) ([]Expression, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+expressionColumns+` FROM expressions ORDER BY kind ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list expressions: %w", err)
	}
	defer rows.Close()

	var out []Expression
	for rows.Next() {
		e, err := scanExpression(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list expressions: %w", err)
	}
	return out, nil
}

// ExpressionByID reads one.
func (s *Store) ExpressionByID(ctx context.Context, id int64) (Expression, error) {
	return scanExpression(s.db.QueryRowContext(ctx,
		`SELECT `+expressionColumns+` FROM expressions WHERE id = ?`, id))
}

// CountExpressions is how many of one kind the server holds, which is what the
// per-kind ceiling is checked against.
func (s *Store) CountExpressions(ctx context.Context, kind string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM expressions WHERE kind = ?`, kind).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count expressions: %w", err)
	}
	return n, nil
}

// CreateExpression records an uploaded emoji or sticker. A name already taken
// within the same kind is ErrConflict.
func (s *Store) CreateExpression(ctx context.Context, e Expression) (Expression, error) {
	ts := now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO expressions (kind, name, storage_key, filename, content_type, size,
		                          animated, creator_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Kind, e.Name, e.StorageKey, e.Filename, e.ContentType, e.Size,
		boolToInt(e.Animated), e.CreatorID, ts)
	if err != nil {
		if isUniqueViolation(err) {
			return Expression{}, ErrConflict
		}
		return Expression{}, fmt.Errorf("store: create expression: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Expression{}, fmt.Errorf("store: create expression: %w", err)
	}
	e.ID = id
	e.CreatedAt = ts
	return e, nil
}

// RenameExpression changes what writers type to reach one.
func (s *Store) RenameExpression(ctx context.Context, id int64, name string) (Expression, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE expressions SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		if isUniqueViolation(err) {
			return Expression{}, ErrConflict
		}
		return Expression{}, fmt.Errorf("store: rename expression: %w", err)
	}
	if err := requireOneRow(res, "expression"); err != nil {
		return Expression{}, err
	}
	return s.ExpressionByID(ctx, id)
}

// DeleteExpression removes one and returns it, so the caller can unlink the
// file the row was the only reference to.
func (s *Store) DeleteExpression(ctx context.Context, id int64) (Expression, error) {
	existing, err := s.ExpressionByID(ctx, id)
	if err != nil {
		return Expression{}, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM expressions WHERE id = ?`, id)
	if err != nil {
		return Expression{}, fmt.Errorf("store: delete expression: %w", err)
	}
	if err := requireOneRow(res, "expression"); err != nil {
		return Expression{}, err
	}
	return existing, nil
}

// --- the soundboard ---------------------------------------------------------

// Sound is one soundboard clip.
type Sound struct {
	ID          int64
	Name        string
	Emoji       string
	StorageKey  string
	Filename    string
	ContentType string
	Size        int64
	DurationMs  int
	Volume      int
	CreatorID   *int64
	CreatedAt   int64
}

const soundColumns = `id, name, emoji, storage_key, filename, content_type, size,
	duration_ms, volume, creator_id, created_at`

func scanSound(row interface{ Scan(...any) error }) (Sound, error) {
	var s Sound
	err := row.Scan(&s.ID, &s.Name, &s.Emoji, &s.StorageKey, &s.Filename, &s.ContentType,
		&s.Size, &s.DurationMs, &s.Volume, &s.CreatorID, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Sound{}, ErrNotFound
	}
	if err != nil {
		return Sound{}, fmt.Errorf("store: scan sound: %w", err)
	}
	return s, nil
}

// AllSounds lists the soundboard, oldest first.
func (s *Store) AllSounds(ctx context.Context) ([]Sound, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+soundColumns+` FROM sounds ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list sounds: %w", err)
	}
	defer rows.Close()

	var out []Sound
	for rows.Next() {
		sound, err := scanSound(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sound)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sounds: %w", err)
	}
	return out, nil
}

// SoundByID reads one.
func (s *Store) SoundByID(ctx context.Context, id int64) (Sound, error) {
	return scanSound(s.db.QueryRowContext(ctx, `SELECT `+soundColumns+` FROM sounds WHERE id = ?`, id))
}

// CountSounds is how many the server holds, checked against the configured
// ceiling before an upload is accepted.
func (s *Store) CountSounds(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sounds`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count sounds: %w", err)
	}
	return n, nil
}

// CreateSound records an uploaded clip.
func (s *Store) CreateSound(ctx context.Context, sound Sound) (Sound, error) {
	ts := now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sounds (name, emoji, storage_key, filename, content_type, size,
		                     duration_ms, volume, creator_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sound.Name, sound.Emoji, sound.StorageKey, sound.Filename, sound.ContentType,
		sound.Size, sound.DurationMs, sound.Volume, sound.CreatorID, ts)
	if err != nil {
		return Sound{}, fmt.Errorf("store: create sound: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Sound{}, fmt.Errorf("store: create sound: %w", err)
	}
	sound.ID = id
	sound.CreatedAt = ts
	return sound, nil
}

// UpdateSound rewrites the label and level of a clip. The file itself is never
// edited: replacing the audio is a delete and an upload.
func (s *Store) UpdateSound(ctx context.Context, id int64, name, emoji string, volume int) (Sound, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sounds SET name = ?, emoji = ?, volume = ? WHERE id = ?`, name, emoji, volume, id)
	if err != nil {
		return Sound{}, fmt.Errorf("store: update sound: %w", err)
	}
	if err := requireOneRow(res, "sound"); err != nil {
		return Sound{}, err
	}
	return s.SoundByID(ctx, id)
}

// DeleteSound removes one and returns it, so its file can be unlinked.
func (s *Store) DeleteSound(ctx context.Context, id int64) (Sound, error) {
	existing, err := s.SoundByID(ctx, id)
	if err != nil {
		return Sound{}, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM sounds WHERE id = ?`, id)
	if err != nil {
		return Sound{}, fmt.Errorf("store: delete sound: %w", err)
	}
	if err := requireOneRow(res, "sound"); err != nil {
		return Sound{}, err
	}
	return existing, nil
}

// --- storage accounting -----------------------------------------------------

// TotalExpressionBytes is the disk the emoji, stickers and sounds take. The
// quota counts them like everything else: a sticker occupies exactly the room
// an attachment of the same size does.
func (s *Store) TotalExpressionBytes(ctx context.Context) (int64, error) {
	var total sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT SUM(size) FROM expressions), 0)
		      + COALESCE((SELECT SUM(size) FROM sounds), 0)`).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: total expression bytes: %w", err)
	}
	return total.Int64, nil
}

// ExpressionKeys is every storage key the two tables name. The orphan sweep
// needs a complete set, so a caller that cannot read this must not sweep.
func (s *Store) ExpressionKeys(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT storage_key FROM expressions UNION ALL SELECT storage_key FROM sounds`)
	if err != nil {
		return nil, fmt.Errorf("store: read expression keys: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("store: read expression keys: %w", err)
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read expression keys: %w", err)
	}
	return out, nil
}

// FileByStorageKey resolves a key to the name and type an expression or sound
// is served under, which is what the download handler needs and what a URL
// cannot be trusted for.
func (s *Store) FileByStorageKey(ctx context.Context, key string) (filename, contentType string, createdAt int64, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT filename, content_type, created_at FROM expressions WHERE storage_key = ?
		 UNION ALL
		 SELECT filename, content_type, created_at FROM sounds WHERE storage_key = ?
		 LIMIT 1`, key, key)
	err = row.Scan(&filename, &contentType, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", 0, ErrNotFound
	}
	if err != nil {
		return "", "", 0, fmt.Errorf("store: read stored file: %w", err)
	}
	return filename, contentType, createdAt, nil
}
