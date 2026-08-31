package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Message is one post in a text channel.
type Message struct {
	ID        int64
	ChannelID int64
	// UserID is nil once the author's account has been removed. Author holds
	// the name to show in that case.
	UserID    *int64
	Author    string
	Content   string
	CreatedAt int64
	EditedAt  *int64
}

// messageColumns resolves the author live from the users table, falling back to
// the name captured when the message was sent. Reading the current nickname is
// what makes a rename show up throughout the history instead of only on new
// messages, which matches the identity model: the row is the person.
const messageColumns = `m.id, m.channel_id, m.user_id,
	COALESCE(u.nickname, m.author), m.content, m.created_at, m.edited_at`

const messageFrom = ` FROM messages m LEFT JOIN users u ON u.id = m.user_id`

func scanMessage(row interface{ Scan(...any) error }) (Message, error) {
	var m Message
	err := row.Scan(&m.ID, &m.ChannelID, &m.UserID, &m.Author, &m.Content, &m.CreatedAt, &m.EditedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, fmt.Errorf("store: scan message: %w", err)
	}
	return m, nil
}

// CreateMessage stores a post and returns it as it will be rendered.
func (s *Store) CreateMessage(ctx context.Context, channelID, userID int64, content string) (Message, error) {
	ts := now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (channel_id, user_id, author, content, created_at)
		 VALUES (?, ?, (SELECT nickname FROM users WHERE id = ?), ?, ?)`,
		channelID, userID, userID, content, ts)
	if err != nil {
		return Message{}, fmt.Errorf("store: create message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, fmt.Errorf("store: create message: %w", err)
	}
	return s.MessageByID(ctx, id)
}

// MessageByID loads one message.
func (s *Store) MessageByID(ctx context.Context, id int64) (Message, error) {
	return scanMessage(s.db.QueryRowContext(ctx,
		`SELECT `+messageColumns+messageFrom+` WHERE m.id = ?`, id))
}

// MessagesBefore reads one page of a channel's history, newest first.
//
// A zero before starts at the newest message. The page is returned newest
// first because that is the order the query walks the index in; callers that
// render oldest first reverse it.
func (s *Store) MessagesBefore(ctx context.Context, channelID, before int64, limit int) ([]Message, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if before > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+messageColumns+messageFrom+
				` WHERE m.channel_id = ? AND m.id < ? ORDER BY m.id DESC LIMIT ?`,
			channelID, before, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+messageColumns+messageFrom+
				` WHERE m.channel_id = ? ORDER BY m.id DESC LIMIT ?`,
			channelID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read history: %w", err)
	}
	defer rows.Close()

	out := make([]Message, 0, limit)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read history: %w", err)
	}
	return out, nil
}

// HasMessagesBefore reports whether anything older than id remains, which is
// what tells a client another page is worth asking for.
func (s *Store) HasMessagesBefore(ctx context.Context, channelID, id int64) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM messages WHERE channel_id = ? AND id < ?)`,
		channelID, id).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: probe history: %w", err)
	}
	return exists == 1, nil
}

// UpdateMessageContent rewrites a message and stamps it as edited.
func (s *Store) UpdateMessageContent(ctx context.Context, id int64, content string) (Message, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET content = ?, edited_at = ? WHERE id = ?`, content, now(), id)
	if err != nil {
		return Message{}, fmt.Errorf("store: edit message: %w", err)
	}
	if err := requireOneRow(res, "message"); err != nil {
		return Message{}, err
	}
	return s.MessageByID(ctx, id)
}

// DeleteMessage removes one message.
func (s *Store) DeleteMessage(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete message: %w", err)
	}
	return requireOneRow(res, "message")
}
