package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aural-chat/aural-server/internal/permissions"
)

// Channel is a node of the channel tree. Categories may hold other channels;
// text and voice channels are always leaves.
type Channel struct {
	ID         int64
	ParentID   *int64
	Name       string
	Type       string
	Topic      string
	Position   int
	UserLimit  int
	CreatedAt  int64
	Overwrites []permissions.Overwrite
}

const channelColumns = `id, parent_id, name, type, topic, position, user_limit, created_at`

func scanChannel(row interface{ Scan(...any) error }) (Channel, error) {
	var c Channel
	err := row.Scan(&c.ID, &c.ParentID, &c.Name, &c.Type, &c.Topic, &c.Position, &c.UserLimit, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, ErrNotFound
	}
	if err != nil {
		return Channel{}, fmt.Errorf("store: scan channel: %w", err)
	}
	return c, nil
}

// AllChannels lists the whole tree with every channel overwrite attached, in
// the order a client should render it.
func (s *Store) AllChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+channelColumns+` FROM channels ORDER BY position ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list channels: %w", err)
	}
	defer rows.Close()

	var out []Channel
	index := map[int64]int{}
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		index[c.ID] = len(out)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list channels: %w", err)
	}

	overwrites, err := s.allOverwrites(ctx)
	if err != nil {
		return nil, err
	}
	for channelID, list := range overwrites {
		if i, ok := index[channelID]; ok {
			out[i].Overwrites = list
		}
	}
	return out, nil
}

// ChannelByID loads one channel together with its overwrites.
func (s *Store) ChannelByID(ctx context.Context, id int64) (Channel, error) {
	c, err := scanChannel(s.db.QueryRowContext(ctx,
		`SELECT `+channelColumns+` FROM channels WHERE id = ?`, id))
	if err != nil {
		return Channel{}, err
	}
	c.Overwrites, err = s.OverwritesFor(ctx, id)
	if err != nil {
		return Channel{}, err
	}
	return c, nil
}

// CreateChannel inserts a channel. A zero Position places it last among its
// siblings.
func (s *Store) CreateChannel(ctx context.Context, c Channel) (Channel, error) {
	ts := now()
	if c.Position == 0 {
		var highest sql.NullInt64
		var err error
		if c.ParentID == nil {
			err = s.db.QueryRowContext(ctx,
				`SELECT MAX(position) FROM channels WHERE parent_id IS NULL`).Scan(&highest)
		} else {
			err = s.db.QueryRowContext(ctx,
				`SELECT MAX(position) FROM channels WHERE parent_id = ?`, *c.ParentID).Scan(&highest)
		}
		if err != nil {
			return Channel{}, fmt.Errorf("store: pick channel position: %w", err)
		}
		if highest.Valid {
			c.Position = int(highest.Int64) + 1
		}
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO channels (parent_id, name, type, topic, position, user_limit, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.ParentID, c.Name, c.Type, c.Topic, c.Position, c.UserLimit, ts)
	if err != nil {
		return Channel{}, fmt.Errorf("store: create channel: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Channel{}, fmt.Errorf("store: create channel: %w", err)
	}
	c.ID = id
	c.CreatedAt = ts
	return c, nil
}

// UpdateChannel writes back a channel the caller has already patched.
func (s *Store) UpdateChannel(ctx context.Context, c Channel) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE channels SET parent_id = ?, name = ?, topic = ?, position = ?, user_limit = ? WHERE id = ?`,
		c.ParentID, c.Name, c.Topic, c.Position, c.UserLimit, c.ID)
	if err != nil {
		return fmt.Errorf("store: update channel: %w", err)
	}
	return requireOneRow(res, "channel")
}

// DeleteChannel removes a channel and every descendant, returning the ids of
// the descendants that went with it so the gateway can tell clients precisely
// what disappeared.
func (s *Store) DeleteChannel(ctx context.Context, id int64) ([]int64, error) {
	descendants, err := s.DescendantIDs(ctx, id)
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("store: delete channel: %w", err)
	}
	if err := requireOneRow(res, "channel"); err != nil {
		return nil, err
	}
	return descendants, nil
}

// DescendantIDs lists every channel below id, depth first.
func (s *Store) DescendantIDs(ctx context.Context, id int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`WITH RECURSIVE subtree(id) AS (
			SELECT id FROM channels WHERE parent_id = ?
			UNION ALL
			SELECT c.id FROM channels c JOIN subtree s ON c.parent_id = s.id
		)
		SELECT id FROM subtree`, id)
	if err != nil {
		return nil, fmt.Errorf("store: list descendants: %w", err)
	}
	defer rows.Close()

	out := make([]int64, 0)
	for rows.Next() {
		var child int64
		if err := rows.Scan(&child); err != nil {
			return nil, fmt.Errorf("store: list descendants: %w", err)
		}
		out = append(out, child)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list descendants: %w", err)
	}
	return out, nil
}

// OverwritesFor lists the permission overwrites attached to one channel.
func (s *Store) OverwritesFor(ctx context.Context, channelID int64) ([]permissions.Overwrite, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT role_id, allow, deny FROM channel_overwrites WHERE channel_id = ? ORDER BY role_id`, channelID)
	if err != nil {
		return nil, fmt.Errorf("store: list overwrites: %w", err)
	}
	defer rows.Close()

	var out []permissions.Overwrite
	for rows.Next() {
		var (
			ow          permissions.Overwrite
			allow, deny int64
		)
		if err := rows.Scan(&ow.RoleID, &allow, &deny); err != nil {
			return nil, fmt.Errorf("store: list overwrites: %w", err)
		}
		ow.Allow = permissions.Permission(allow) & permissions.All
		ow.Deny = permissions.Permission(deny) & permissions.All
		out = append(out, ow)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list overwrites: %w", err)
	}
	return out, nil
}

// SetOverwrites replaces the whole overwrite set of a channel.
func (s *Store) SetOverwrites(ctx context.Context, channelID int64, list []permissions.Overwrite) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM channel_overwrites WHERE channel_id = ?`, channelID); err != nil {
			return fmt.Errorf("store: clear overwrites: %w", err)
		}
		for _, ow := range list {
			// An overwrite that neither allows nor denies anything is noise.
			if ow.Allow == 0 && ow.Deny == 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO channel_overwrites (channel_id, role_id, allow, deny) VALUES (?, ?, ?, ?)`,
				channelID, ow.RoleID, int64(ow.Allow), int64(ow.Deny)); err != nil {
				return fmt.Errorf("store: write overwrite: %w", err)
			}
		}
		return nil
	})
}

// allOverwrites loads every overwrite in the database, grouped by channel.
func (s *Store) allOverwrites(ctx context.Context) (map[int64][]permissions.Overwrite, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT channel_id, role_id, allow, deny FROM channel_overwrites ORDER BY channel_id, role_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list overwrites: %w", err)
	}
	defer rows.Close()

	out := map[int64][]permissions.Overwrite{}
	for rows.Next() {
		var (
			channelID   int64
			ow          permissions.Overwrite
			allow, deny int64
		)
		if err := rows.Scan(&channelID, &ow.RoleID, &allow, &deny); err != nil {
			return nil, fmt.Errorf("store: list overwrites: %w", err)
		}
		ow.Allow = permissions.Permission(allow) & permissions.All
		ow.Deny = permissions.Permission(deny) & permissions.All
		out[channelID] = append(out[channelID], ow)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list overwrites: %w", err)
	}
	return out, nil
}
