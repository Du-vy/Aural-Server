package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Webhook is a URL that posts into one text channel.
//
// It holds no identity. A message that arrives through one is attributed to
// the name and picture the webhook carries — or to the ones the delivery
// itself overrode — rather than to any user, which is why every message row it
// produces has a NULL user_id and a webhook_id instead.
type Webhook struct {
	ID        int64
	ChannelID int64
	Name      string
	// Avatar is the picture shown for messages this webhook posts, unless a
	// delivery overrides it. It is an absolute URL: the sender is an outside
	// service, and nothing about it is hosted here.
	Avatar *string
	// Token is the secret half of the URL, stored as it was minted. See the
	// schema comment on the table for why it is not hashed.
	Token string
	// CreatorID is nil once the account that made it is gone. The webhook
	// itself survives: an integration must not stop working because the
	// administrator who wired it up left.
	CreatorID  *int64
	CreatedAt  int64
	LastUsedAt int64
}

const webhookColumns = `id, channel_id, name, avatar, token, creator_id, created_at, last_used_at`

func scanWebhook(row interface{ Scan(...any) error }) (Webhook, error) {
	var wh Webhook
	err := row.Scan(&wh.ID, &wh.ChannelID, &wh.Name, &wh.Avatar, &wh.Token,
		&wh.CreatorID, &wh.CreatedAt, &wh.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Webhook{}, ErrNotFound
	}
	if err != nil {
		return Webhook{}, fmt.Errorf("store: scan webhook: %w", err)
	}
	return wh, nil
}

func scanWebhooks(rows *sql.Rows) ([]Webhook, error) {
	defer rows.Close()

	out := []Webhook{}
	for rows.Next() {
		wh, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, wh)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read webhooks: %w", err)
	}
	return out, nil
}

// CreateWebhook records a new webhook. Token must already be minted.
func (s *Store) CreateWebhook(ctx context.Context, wh Webhook) (Webhook, error) {
	ts := now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO webhooks (channel_id, name, avatar, token, creator_id, created_at, last_used_at)
		 VALUES (?, ?, ?, ?, ?, ?, 0)`,
		wh.ChannelID, wh.Name, wh.Avatar, wh.Token, wh.CreatorID, ts)
	if err != nil {
		if isUniqueViolation(err) {
			return Webhook{}, ErrConflict
		}
		return Webhook{}, fmt.Errorf("store: create webhook: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Webhook{}, fmt.Errorf("store: create webhook: %w", err)
	}
	return s.WebhookByID(ctx, id)
}

// WebhookByID loads one webhook.
func (s *Store) WebhookByID(ctx context.Context, id int64) (Webhook, error) {
	return scanWebhook(s.db.QueryRowContext(ctx,
		`SELECT `+webhookColumns+` FROM webhooks WHERE id = ?`, id))
}

// WebhooksForChannels lists the webhooks of a set of channels, oldest first.
// Passing no channels lists none, which is what a caller who may manage
// nothing should see.
func (s *Store) WebhooksForChannels(ctx context.Context, channelIDs []int64) ([]Webhook, error) {
	if len(channelIDs) == 0 {
		return []Webhook{}, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+webhookColumns+` FROM webhooks WHERE channel_id IN (`+
			placeholders(len(channelIDs))+`) ORDER BY id`, idArgs(channelIDs)...)
	if err != nil {
		return nil, fmt.Errorf("store: list webhooks: %w", err)
	}
	return scanWebhooks(rows)
}

// CountWebhooksInChannel is what a per-channel ceiling is measured against.
func (s *Store) CountWebhooksInChannel(ctx context.Context, channelID int64) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM webhooks WHERE channel_id = ?`, channelID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count webhooks: %w", err)
	}
	return n, nil
}

// UpdateWebhook rewrites the fields an administrator may change: the name, the
// default avatar, and which channel the URL posts into.
func (s *Store) UpdateWebhook(ctx context.Context, wh Webhook) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE webhooks SET name = ?, avatar = ?, channel_id = ? WHERE id = ?`,
		wh.Name, wh.Avatar, wh.ChannelID, wh.ID)
	if err != nil {
		return fmt.Errorf("store: update webhook: %w", err)
	}
	return requireOneRow(res, "webhook")
}

// TouchWebhook records a delivery. It is a write on the hot path of every
// message a webhook posts, so it is deliberately its own statement rather than
// part of the insert: a failure here costs a timestamp, not the message.
func (s *Store) TouchWebhook(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE webhooks SET last_used_at = ? WHERE id = ?`, now(), id)
	if err != nil {
		return fmt.Errorf("store: touch webhook: %w", err)
	}
	return nil
}

// DeleteWebhook revokes a URL. What was posted through it stays: the messages
// keep the name and picture they were posted under, because the history is a
// record of what was said rather than of who may still say it.
func (s *Store) DeleteWebhook(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete webhook: %w", err)
	}
	return requireOneRow(res, "webhook")
}
