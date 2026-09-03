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
	// WebhookID names the webhook that posted the message, and is nil for
	// everything a person wrote. It is not a foreign key: deleting a webhook
	// revokes its URL and leaves the history it produced exactly as it was.
	WebhookID *int64
	// WebhookAvatar is the picture this one message was posted under. Only a
	// webhook message ever carries one.
	WebhookAvatar *string
	// Embeds is the rich cards the message carries, held as the JSON array
	// they arrived in. This layer keeps it opaque; the gateway is what knows
	// its shape.
	Embeds *string
}

// messageColumns resolves the author live from the users table, falling back to
// the name captured when the message was sent. Reading the current nickname is
// what makes a rename show up throughout the history instead of only on new
// messages, which matches the identity model: the row is the person.
const messageColumns = `m.id, m.channel_id, m.user_id,
	COALESCE(u.nickname, m.author), m.content, m.created_at, m.edited_at,
	m.webhook_id, m.webhook_avatar, m.embeds`

const messageFrom = ` FROM messages m LEFT JOIN users u ON u.id = m.user_id`

func scanMessage(row interface{ Scan(...any) error }) (Message, error) {
	var m Message
	err := row.Scan(&m.ID, &m.ChannelID, &m.UserID, &m.Author, &m.Content, &m.CreatedAt, &m.EditedAt,
		&m.WebhookID, &m.WebhookAvatar, &m.Embeds)
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
		`INSERT INTO messages (channel_id, user_id, author, content, search_text, created_at)
		 VALUES (?, ?, (SELECT nickname FROM users WHERE id = ?), ?, ?, ?)`,
		channelID, userID, userID, content, foldForSearch(content), ts)
	if err != nil {
		return Message{}, fmt.Errorf("store: create message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, fmt.Errorf("store: create message: %w", err)
	}
	return s.MessageByID(ctx, id)
}

// CreateWebhookMessage stores a post that arrived through a webhook.
//
// It is not CreateMessage with a nil user: the author is captured rather than
// resolved, because there is no row in users for it to be resolved from. The
// name and the picture belong to the delivery, so a webhook renamed tomorrow
// leaves today's messages reading as they did.
func (s *Store) CreateWebhookMessage(ctx context.Context, m Message) (Message, error) {
	ts := now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (channel_id, user_id, author, content, search_text,
			created_at, webhook_id, webhook_avatar, embeds)
		 VALUES (?, NULL, ?, ?, ?, ?, ?, ?, ?)`,
		m.ChannelID, m.Author, m.Content, foldForSearch(m.Content), ts,
		m.WebhookID, m.WebhookAvatar, m.Embeds)
	if err != nil {
		return Message{}, fmt.Errorf("store: create webhook message: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, fmt.Errorf("store: create webhook message: %w", err)
	}
	return s.MessageByID(ctx, id)
}

// UpdateWebhookMessage rewrites what a webhook posted, content and embeds
// together: a delivery that edits a message replaces the whole of it, which is
// what the endpoint behind it means by an edit.
func (s *Store) UpdateWebhookMessage(ctx context.Context, id int64, content string, embeds *string) (Message, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET content = ?, search_text = ?, embeds = ?, edited_at = ? WHERE id = ?`,
		content, foldForSearch(content), embeds, now(), id)
	if err != nil {
		return Message{}, fmt.Errorf("store: edit webhook message: %w", err)
	}
	if err := requireOneRow(res, "message"); err != nil {
		return Message{}, err
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
	return scanMessages(rows, limit)
}

// scanMessages drains a message query. size is only a hint for the slice.
func scanMessages(rows *sql.Rows, size int) ([]Message, error) {
	defer rows.Close()

	out := make([]Message, 0, size)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read messages: %w", err)
	}
	return out, nil
}

// MessagesAfter reads one page forwards from after, oldest first.
//
// It is what a client walks back to the present with after jumping into the
// middle of a channel, and the mirror of MessagesBefore in every other way.
func (s *Store) MessagesAfter(ctx context.Context, channelID, after int64, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageColumns+messageFrom+
			` WHERE m.channel_id = ? AND m.id > ? ORDER BY m.id ASC LIMIT ?`,
		channelID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("store: read history: %w", err)
	}
	return scanMessages(rows, limit)
}

// MessagesAround reads a page centred on one message, oldest first.
//
// The anchor itself is included when it still exists; when it does not — it was
// deleted between a search and the jump into it — the window either side of
// where it used to be is returned, which is the conversation the reader was
// looking for anyway.
func (s *Store) MessagesAround(ctx context.Context, channelID, id int64, limit int) ([]Message, error) {
	// The anchor is read as part of the older half, by asking for everything
	// before the id after it.
	older, err := s.MessagesBefore(ctx, channelID, id+1, limit/2+1)
	if err != nil {
		return nil, err
	}
	newer, err := s.MessagesAfter(ctx, channelID, id, limit-len(older))
	if err != nil {
		return nil, err
	}

	out := make([]Message, 0, len(older)+len(newer))
	// MessagesBefore walks the index newest first; this page is rendered
	// oldest first, like every other.
	for i := len(older) - 1; i >= 0; i-- {
		out = append(out, older[i])
	}
	return append(out, newer...), nil
}

// HasMessagesBefore reports whether anything older than id remains, which is
// what tells a client another page is worth asking for.
func (s *Store) HasMessagesBefore(ctx context.Context, channelID, id int64) (bool, error) {
	return s.probeHistory(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE channel_id = ? AND id < ?)`, channelID, id)
}

// HasMessagesAfter reports whether anything newer than id remains, which is
// what tells a client whether it is holding the present or a window behind it.
func (s *Store) HasMessagesAfter(ctx context.Context, channelID, id int64) (bool, error) {
	return s.probeHistory(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE channel_id = ? AND id > ?)`, channelID, id)
}

func (s *Store) probeHistory(ctx context.Context, query string, args ...any) (bool, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: probe history: %w", err)
	}
	return exists == 1, nil
}

// MessagesByID loads a scattered set of messages in one query. Search reads its
// hits by matching and then reads the conversation around them by id.
func (s *Store) MessagesByID(ctx context.Context, ids []int64) ([]Message, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageColumns+messageFrom+
			` WHERE m.id IN (`+placeholders(len(ids))+`) ORDER BY m.id`, idArgs(ids)...)
	if err != nil {
		return nil, fmt.Errorf("store: read messages: %w", err)
	}
	return scanMessages(rows, len(ids))
}

// UpdateMessageContent rewrites a message and stamps it as edited.
func (s *Store) UpdateMessageContent(ctx context.Context, id int64, content string) (Message, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET content = ?, search_text = ?, edited_at = ? WHERE id = ?`,
		content, foldForSearch(content), now(), id)
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

type DeletedMessageTarget struct {
	ID        int64
	ChannelID int64
}

// DeleteMessagesByUser removes messages written by a specific user, optionally restricted
// by a cutoff timestamp (seconds). If cutoff <= 0, all messages are removed.
func (s *Store) DeleteMessagesByUser(ctx context.Context, userID int64, cutoff int64) ([]DeletedMessageTarget, error) {
	var query string
	var args []any
	if cutoff > 0 {
		query = `SELECT id, channel_id FROM messages WHERE user_id = ? AND created_at >= ?`
		args = []any{userID, cutoff}
	} else {
		query = `SELECT id, channel_id FROM messages WHERE user_id = ?`
		args = []any{userID}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query messages to delete: %w", err)
	}
	defer rows.Close()

	var targets []DeletedMessageTarget
	for rows.Next() {
		var t DeletedMessageTarget
		if err := rows.Scan(&t.ID, &t.ChannelID); err != nil {
			return nil, fmt.Errorf("store: scan message target: %w", err)
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate messages to delete: %w", err)
	}

	if len(targets) == 0 {
		return nil, nil
	}

	if cutoff > 0 {
		_, err = s.db.ExecContext(ctx, `DELETE FROM messages WHERE user_id = ? AND created_at >= ?`, userID, cutoff)
	} else {
		_, err = s.db.ExecContext(ctx, `DELETE FROM messages WHERE user_id = ?`, userID)
	}
	if err != nil {
		return nil, fmt.Errorf("store: delete messages: %w", err)
	}

	return targets, nil
}
