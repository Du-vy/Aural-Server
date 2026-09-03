package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// The three things a ban can be matched against.
//
// They are deliberately of different strengths and are meant to be used
// together. An account is exact and trivially replaced; an address is shared by
// a household and changed by a reboot; a device survives both of those and is
// the only one that follows somebody who signs up again from the same machine.
// No one of them is a wall, which is why a ban carries as many as are known.
const (
	MatchUser   = "user"
	MatchIP     = "ip"
	MatchDevice = "device"
)

// Ban is one moderation decision, with the handles that enforce it attached.
type Ban struct {
	ID            int64
	UserID        *int64
	UserNickname  string
	UserUsername  *string
	ActorID       *int64
	ActorNickname string
	Reason        string
	CreatedAt     int64
	// ExpiresAt is nil for a permanent ban. A ban that has passed its date is
	// kept rather than deleted: the list is a record of what was done, not
	// only of what is in force.
	ExpiresAt *int64
	// Matches are the identifiers a connection is compared against. A ban with
	// none of them is a record and nothing more, which is what a ban against a
	// guest whose address could not be read amounts to.
	Matches []BanMatch
}

// BanMatch is one identifier a ban catches.
type BanMatch struct {
	Kind  string
	Value string
}

// Active reports whether a ban is still in force at the given time.
func (b Ban) Active(at int64) bool {
	return b.ExpiresAt == nil || *b.ExpiresAt > at
}

const banColumns = `id, user_id, user_nickname, user_username, actor_id, actor_nickname,
	reason, created_at, expires_at`

func scanBan(row interface{ Scan(...any) error }) (Ban, error) {
	var b Ban
	err := row.Scan(&b.ID, &b.UserID, &b.UserNickname, &b.UserUsername,
		&b.ActorID, &b.ActorNickname, &b.Reason, &b.CreatedAt, &b.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Ban{}, ErrNotFound
	}
	if err != nil {
		return Ban{}, fmt.Errorf("store: scan ban: %w", err)
	}
	return b, nil
}

// CreateBan records a ban and the identifiers it catches.
//
// An identifier already claimed by another ban is moved onto this one rather
// than refusing the whole write. Two bans matching the same address is not a
// state worth being able to reach — the check would have to pick one of them —
// and the newer decision is the one a moderator just made.
func (s *Store) CreateBan(ctx context.Context, b Ban) (Ban, error) {
	ts := now()
	b.CreatedAt = ts

	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO bans (user_id, user_nickname, user_username, actor_id, actor_nickname,
			                   reason, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			b.UserID, b.UserNickname, b.UserUsername, b.ActorID, b.ActorNickname,
			b.Reason, ts, b.ExpiresAt)
		if err != nil {
			return fmt.Errorf("store: create ban: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: create ban: %w", err)
		}
		b.ID = id

		for _, m := range b.Matches {
			if m.Value == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO ban_matches (ban_id, kind, value) VALUES (?, ?, ?)
				 ON CONFLICT(kind, value) DO UPDATE SET ban_id = excluded.ban_id`,
				id, m.Kind, m.Value); err != nil {
				return fmt.Errorf("store: record ban match: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return Ban{}, err
	}
	return b, nil
}

// DeleteBan lifts a ban and, by cascade, every identifier it caught.
func (s *Store) DeleteBan(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM bans WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete ban: %w", err)
	}
	return requireOneRow(res, "ban")
}

// BanByID reads one ban with its identifiers.
func (s *Store) BanByID(ctx context.Context, id int64) (Ban, error) {
	b, err := scanBan(s.db.QueryRowContext(ctx, `SELECT `+banColumns+` FROM bans WHERE id = ?`, id))
	if err != nil {
		return Ban{}, err
	}
	matches, err := s.banMatches(ctx, []int64{b.ID})
	if err != nil {
		return Ban{}, err
	}
	b.Matches = matches[b.ID]
	return b, nil
}

// ListBans reads the ban list, newest first.
func (s *Store) ListBans(ctx context.Context, limit int) ([]Ban, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+banColumns+` FROM bans ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list bans: %w", err)
	}
	defer rows.Close()

	var out []Ban
	var ids []int64
	for rows.Next() {
		b, err := scanBan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
		ids = append(ids, b.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list bans: %w", err)
	}

	matches, err := s.banMatches(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Matches = matches[out[i].ID]
	}
	return out, nil
}

// banMatches reads the identifiers of a set of bans in one query.
func (s *Store) banMatches(ctx context.Context, banIDs []int64) (map[int64][]BanMatch, error) {
	out := map[int64][]BanMatch{}
	if len(banIDs) == 0 {
		return out, nil
	}

	args := make([]any, 0, len(banIDs))
	for _, id := range banIDs {
		args = append(args, id)
	}
	query := `SELECT ban_id, kind, value FROM ban_matches WHERE ban_id IN (` +
		placeholders(len(banIDs)) + `) ORDER BY kind ASC, value ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read ban matches: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var banID int64
		var m BanMatch
		if err := rows.Scan(&banID, &m.Kind, &m.Value); err != nil {
			return nil, fmt.Errorf("store: read ban matches: %w", err)
		}
		out[banID] = append(out[banID], m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read ban matches: %w", err)
	}
	return out, nil
}

// FindBan looks for a ban in force against any of the identifiers a connection
// presents. It returns ErrNotFound when nothing matches.
//
// Every identifier is asked about in one query rather than three, because this
// runs on the authentication path of every single connection and the answer is
// almost always "no". Expired bans are excluded here rather than swept: a ban
// that has run out stops being enforced the second it does, without waiting for
// a housekeeping tick to notice.
func (s *Store) FindBan(ctx context.Context, at int64, matches []BanMatch) (Ban, error) {
	if len(matches) == 0 {
		return Ban{}, ErrNotFound
	}

	var conditions []string
	args := []any{at}
	for _, m := range matches {
		if m.Value == "" {
			continue
		}
		conditions = append(conditions, `(m.kind = ? AND m.value = ?)`)
		args = append(args, m.Kind, m.Value)
	}
	if len(conditions) == 0 {
		return Ban{}, ErrNotFound
	}

	query := `SELECT ` + prefixColumns(banColumns, "b") + `
		FROM bans b JOIN ban_matches m ON m.ban_id = b.id
		WHERE (b.expires_at IS NULL OR b.expires_at > ?) AND (` + strings.Join(conditions, " OR ") + `)
		ORDER BY b.id DESC LIMIT 1`

	return scanBan(s.db.QueryRowContext(ctx, query, args...))
}

// --- where an identity has been ---------------------------------------------

// IdentityMark is one address and device an identity has connected from.
type IdentityMark struct {
	IP     string
	Device string
}

// RecordIdentityMark notes where an identity just connected from, so that a ban
// against it later can reach the same place.
//
// Writing it on every authentication rather than only the first is what keeps
// the last_seen_at column meaningful, which is what the sweep prunes on.
func (s *Store) RecordIdentityMark(ctx context.Context, userID int64, mark IdentityMark) error {
	if mark.IP == "" && mark.Device == "" {
		return nil
	}
	ts := now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO identity_marks (user_id, ip, device, first_seen_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(user_id, ip, device) DO UPDATE SET last_seen_at = excluded.last_seen_at`,
		userID, mark.IP, mark.Device, ts, ts)
	if err != nil {
		return fmt.Errorf("store: record identity mark: %w", err)
	}
	return nil
}

// IdentityMarks lists where an identity has connected from, most recent first.
// limit bounds it, because a ban should reach the addresses somebody actually
// uses rather than every one they have ever had.
func (s *Store) IdentityMarks(ctx context.Context, userID int64, limit int) ([]IdentityMark, error) {
	if limit <= 0 {
		limit = 8
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT ip, device FROM identity_marks WHERE user_id = ?
		 ORDER BY last_seen_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: read identity marks: %w", err)
	}
	defer rows.Close()

	var out []IdentityMark
	for rows.Next() {
		var m IdentityMark
		if err := rows.Scan(&m.IP, &m.Device); err != nil {
			return nil, fmt.Errorf("store: read identity marks: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read identity marks: %w", err)
	}
	return out, nil
}

// PruneIdentityMarks drops the places nobody has connected from since cutoff.
// They exist to make a ban land, and one nobody has used in months would not.
func (s *Store) PruneIdentityMarks(ctx context.Context, cutoff int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM identity_marks WHERE last_seen_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune identity marks: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// --- small SQL helpers ------------------------------------------------------

// prefixColumns qualifies a column list with a table alias, so one constant can
// serve both a plain select and a join.
func prefixColumns(columns, alias string) string {
	parts := strings.Split(columns, ",")
	for i, part := range parts {
		parts[i] = " " + alias + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ",")
}
