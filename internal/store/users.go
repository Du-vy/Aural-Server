package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// User is a member of the server. A guest is a user whose Username is still
// nil: the row exists, holds an id and a nickname, and can later be claimed by
// setting a username and a password on it. Claiming never creates a second row,
// which is what keeps the guest identity and the account the same identity.
type User struct {
	ID           int64
	Nickname     string
	Username     *string
	PasswordHash *string
	Avatar       *string
	Banner       *string
	Status       string
	CustomStatus string
	RegisteredAt *int64
	CreatedAt    int64
	LastSeenAt   int64
}

// Registered reports whether the identity has been claimed with credentials.
func (u User) Registered() bool { return u.Username != nil }

const (
	userColumns    = `id, nickname, username, password_hash, avatar, banner, status, custom_status, registered_at, created_at, last_seen_at`
	userColumnsAsU = `u.id, u.nickname, u.username, u.password_hash, u.avatar, u.banner, u.status, u.custom_status, u.registered_at, u.created_at, u.last_seen_at`
)

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Nickname, &u.Username, &u.PasswordHash, &u.Avatar, &u.Banner, &u.Status, &u.CustomStatus, &u.RegisteredAt, &u.CreatedAt, &u.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: scan user: %w", err)
	}
	if u.Status == "" {
		u.Status = "online"
	}
	return u, nil
}

// CreateGuest inserts a fresh unclaimed identity.
func (s *Store) CreateGuest(ctx context.Context, nickname string) (User, error) {
	ts := now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (nickname, status, custom_status, created_at, last_seen_at) VALUES (?, 'online', '', ?, ?)`,
		nickname, ts, ts)
	if err != nil {
		return User{}, fmt.Errorf("store: create guest: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("store: create guest: %w", err)
	}
	return User{ID: id, Nickname: nickname, Status: "online", CustomStatus: "", CreatedAt: ts, LastSeenAt: ts}, nil
}

// UserByID looks up one user.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// UserByUsername looks up an account by name. The column collates NOCASE, so
// the match is case-insensitive.
func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE username = ?`, username))
}

// UserByTokenHash resolves a session token to its owner.
func (s *Store) UserByTokenHash(ctx context.Context, tokenHash string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumnsAsU+`
		 FROM users u JOIN tokens t ON t.user_id = u.id
		 WHERE t.token_hash = ?`, tokenHash))
}

// ClaimIdentity turns the guest identity id into an account. It fails with
// ErrConflict when the username is taken, and with ErrAlreadyClaimed when the
// identity already has credentials.
func (s *Store) ClaimIdentity(ctx context.Context, id int64, username, passwordHash string) (User, error) {
	ts := now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET username = ?, password_hash = ?, registered_at = ?
		 WHERE id = ? AND username IS NULL`,
		username, passwordHash, ts, id)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrConflict
		}
		return User{}, fmt.Errorf("store: claim identity: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return User{}, fmt.Errorf("store: claim identity: %w", err)
	}
	if affected == 0 {
		// Either the row is gone or it already carries a username.
		if _, err := s.UserByID(ctx, id); err != nil {
			return User{}, err
		}
		return User{}, ErrAlreadyClaimed
	}
	return s.UserByID(ctx, id)
}

// ErrAlreadyClaimed is returned when an identity that already has credentials
// tries to register again.
var ErrAlreadyClaimed = errors.New("store: identity already claimed")

// SetNickname changes the display name of a user.
func (s *Store) SetNickname(ctx context.Context, id int64, nickname string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET nickname = ? WHERE id = ?`, nickname, id)
	if err != nil {
		return fmt.Errorf("store: set nickname: %w", err)
	}
	return requireOneRow(res, "user")
}

// SetAvatar changes the avatar URL of a user.
func (s *Store) SetAvatar(ctx context.Context, id int64, avatar *string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET avatar = ? WHERE id = ?`, avatar, id)
	if err != nil {
		return fmt.Errorf("store: set avatar: %w", err)
	}
	return requireOneRow(res, "user")
}

// SetBanner changes the banner URL of a user.
func (s *Store) SetBanner(ctx context.Context, id int64, banner *string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET banner = ? WHERE id = ?`, banner, id)
	if err != nil {
		return fmt.Errorf("store: set banner: %w", err)
	}
	return requireOneRow(res, "user")
}

// SetStatus changes the status and optional custom status of a user.
func (s *Store) SetStatus(ctx context.Context, id int64, status string, customStatus *string) error {
	var res sql.Result
	var err error
	if customStatus != nil {
		res, err = s.db.ExecContext(ctx, `UPDATE users SET status = ?, custom_status = ? WHERE id = ?`, status, *customStatus, id)
	} else {
		res, err = s.db.ExecContext(ctx, `UPDATE users SET status = ? WHERE id = ?`, status, id)
	}
	if err != nil {
		return fmt.Errorf("store: set status: %w", err)
	}
	return requireOneRow(res, "user")
}

// UpdateProfile updates nickname, avatar, banner, status, or custom status for a user.
func (s *Store) UpdateProfile(ctx context.Context, id int64, nickname *string, avatar **string, banner **string, status *string, customStatus *string) (User, error) {
	var sets []string
	var args []any

	if nickname != nil {
		sets = append(sets, "nickname = ?")
		args = append(args, *nickname)
	}
	if avatar != nil {
		sets = append(sets, "avatar = ?")
		args = append(args, *avatar)
	}
	if banner != nil {
		sets = append(sets, "banner = ?")
		args = append(args, *banner)
	}
	if status != nil {
		sets = append(sets, "status = ?")
		args = append(args, *status)
	}
	if customStatus != nil {
		sets = append(sets, "custom_status = ?")
		args = append(args, *customStatus)
	}

	if len(sets) == 0 {
		return s.UserByID(ctx, id)
	}

	args = append(args, id)
	query := fmt.Sprintf("UPDATE users SET %s WHERE id = ?", strings.Join(sets, ", "))
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return User{}, fmt.Errorf("store: update profile: %w", err)
	}
	if err := requireOneRow(res, "user"); err != nil {
		return User{}, err
	}
	return s.UserByID(ctx, id)
}

// TouchUser records that the user was seen just now.
func (s *Store) TouchUser(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET last_seen_at = ? WHERE id = ?`, now(), id)
	if err != nil {
		return fmt.Errorf("store: touch user: %w", err)
	}
	return nil
}

// CreateToken stores the hash of a freshly minted session token.
func (s *Store) CreateToken(ctx context.Context, userID int64, tokenHash, label string) error {
	ts := now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (user_id, token_hash, label, created_at, last_used_at) VALUES (?, ?, ?, ?, ?)`,
		userID, tokenHash, label, ts, ts)
	if err != nil {
		return fmt.Errorf("store: create token: %w", err)
	}
	return nil
}

// TouchToken records a token being used to resume a session.
func (s *Store) TouchToken(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET last_used_at = ? WHERE token_hash = ?`, now(), tokenHash)
	if err != nil {
		return fmt.Errorf("store: touch token: %w", err)
	}
	return nil
}

// DeleteToken revokes one session token.
func (s *Store) DeleteToken(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE token_hash = ?`, tokenHash)
	if err != nil {
		return fmt.Errorf("store: delete token: %w", err)
	}
	return nil
}

// DeleteTokensForUser revokes every session of one user, which is what a
// password change or a forced sign-out needs.
func (s *Store) DeleteTokensForUser(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("store: delete tokens: %w", err)
	}
	return nil
}

// UsersByID loads a batch of users, in the order the ids were given. Missing
// ids are skipped rather than reported.
func (s *Store) UsersByID(ctx context.Context, ids []int64) ([]User, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	query := `SELECT ` + userColumns + ` FROM users WHERE id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: load users: %w", err)
	}
	defer rows.Close()

	byID := make(map[int64]User, len(ids))
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		byID[u.ID] = u
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: load users: %w", err)
	}

	out := make([]User, 0, len(ids))
	for _, id := range ids {
		if u, ok := byID[id]; ok {
			out = append(out, u)
		}
	}
	return out, nil
}

// requireOneRow turns an UPDATE that matched nothing into ErrNotFound.
func requireOneRow(res sql.Result, what string) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update %s: %w", what, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// placeholders builds "?, ?, ?" for an IN clause of n values.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, 0, n*3-2)
	for i := range n {
		if i > 0 {
			buf = append(buf, ',', ' ')
		}
		buf = append(buf, '?')
	}
	return string(buf)
}
