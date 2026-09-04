package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Which way a link carries messages.
//
// A one-way link is not a half-broken two-way one. A community that announces
// releases into a Discord it no longer reads wants RelayToDiscord, and one
// letting the members who have not moved yet be heard here without giving them
// a way to post wants RelayToAural.
const (
	RelayBoth      = "both"
	RelayToAural   = "to_aural"
	RelayToDiscord = "to_discord"
)

// ValidRelayDirection reports whether a direction is one of the three.
func ValidRelayDirection(d string) bool {
	switch d {
	case RelayBoth, RelayToAural, RelayToDiscord:
		return true
	default:
		return false
	}
}

// Which side a relayed message was written on. An edit travels away from its
// origin and never back towards it, which is what keeps two servers from
// editing each other in a circle.
const (
	RelayOriginDiscord = "discord"
	RelayOriginAural   = "aural"
)

// RelayLink pairs one Aural text channel with one Discord channel.
type RelayLink struct {
	ID        int64
	ChannelID int64
	// DiscordGuildID is the server the channel is in. It is stored rather than
	// derived so a link keeps meaning something while the bot is disconnected
	// and nothing can be looked up.
	DiscordGuildID   string
	DiscordChannelID string
	// WebhookID and WebhookToken are the two halves of the Discord webhook URL
	// messages go out through. The id is also the loop guard: see the schema
	// comment in migrations.go.
	WebhookID    string
	WebhookToken string
	Direction    string
	Enabled      bool
	// RelayAttachments and RelayEdits are what crosses beyond the words.
	RelayAttachments bool
	RelayEdits       bool
	// SourceWebhookID is the webhooks row messages arriving from Discord are
	// written under. It is nil until the first inbound message provisions one.
	SourceWebhookID *int64
	CreatedAt       int64
	LastRelayedAt   int64
	LastError       string
}

// Bidirectional reports whether messages written here reach Discord.
func (l RelayLink) ToDiscord() bool {
	return l.Enabled && (l.Direction == RelayBoth || l.Direction == RelayToDiscord)
}

// ToAural reports whether messages written in Discord reach this server.
func (l RelayLink) ToAural() bool {
	return l.Enabled && (l.Direction == RelayBoth || l.Direction == RelayToAural)
}

const relayLinkColumns = `id, channel_id, discord_guild_id, discord_channel_id,
	webhook_id, webhook_token, direction, enabled, relay_attachments, relay_edits,
	source_webhook_id, created_at, last_relayed_at, last_error`

func scanRelayLink(row interface{ Scan(...any) error }) (RelayLink, error) {
	var l RelayLink
	err := row.Scan(&l.ID, &l.ChannelID, &l.DiscordGuildID, &l.DiscordChannelID,
		&l.WebhookID, &l.WebhookToken, &l.Direction, &l.Enabled,
		&l.RelayAttachments, &l.RelayEdits, &l.SourceWebhookID,
		&l.CreatedAt, &l.LastRelayedAt, &l.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return RelayLink{}, ErrNotFound
	}
	if err != nil {
		return RelayLink{}, fmt.Errorf("store: scan relay link: %w", err)
	}
	return l, nil
}

func scanRelayLinks(rows *sql.Rows) ([]RelayLink, error) {
	defer rows.Close()

	out := []RelayLink{}
	for rows.Next() {
		l, err := scanRelayLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read relay links: %w", err)
	}
	return out, nil
}

// CreateRelayLink records a new pairing.
//
// A channel already linked on either side is a conflict rather than a second
// link: two links onto the same Discord channel would relay each other's
// output, and the webhook guard cannot see that loop because both halves of it
// are legitimately this server's.
func (s *Store) CreateRelayLink(ctx context.Context, l RelayLink) (RelayLink, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO relay_links (channel_id, discord_guild_id, discord_channel_id,
			webhook_id, webhook_token, direction, enabled, relay_attachments,
			relay_edits, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ChannelID, l.DiscordGuildID, l.DiscordChannelID, l.WebhookID, l.WebhookToken,
		l.Direction, boolToInt(l.Enabled), boolToInt(l.RelayAttachments),
		boolToInt(l.RelayEdits), now())
	if err != nil {
		if isUniqueViolation(err) {
			return RelayLink{}, ErrConflict
		}
		return RelayLink{}, fmt.Errorf("store: create relay link: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return RelayLink{}, fmt.Errorf("store: create relay link: %w", err)
	}
	return s.RelayLinkByID(ctx, id)
}

// UpdateRelayLink rewrites the fields an administrator may change. The channel
// pairing is one of them: repointing a link is how a channel is renamed on
// either side without losing its settings.
func (s *Store) UpdateRelayLink(ctx context.Context, l RelayLink) (RelayLink, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE relay_links SET channel_id = ?, discord_guild_id = ?, discord_channel_id = ?,
			webhook_id = ?, webhook_token = ?, direction = ?, enabled = ?,
			relay_attachments = ?, relay_edits = ?
		 WHERE id = ?`,
		l.ChannelID, l.DiscordGuildID, l.DiscordChannelID, l.WebhookID, l.WebhookToken,
		l.Direction, boolToInt(l.Enabled), boolToInt(l.RelayAttachments),
		boolToInt(l.RelayEdits), l.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return RelayLink{}, ErrConflict
		}
		return RelayLink{}, fmt.Errorf("store: update relay link: %w", err)
	}
	if err := requireOneRow(res, "relay link"); err != nil {
		return RelayLink{}, err
	}
	return s.RelayLinkByID(ctx, l.ID)
}

// DeleteRelayLink unpairs two channels. What was already relayed stays on both
// sides: the history is a record of what was said, not of how it arrived.
func (s *Store) DeleteRelayLink(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM relay_links WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete relay link: %w", err)
	}
	return requireOneRow(res, "relay link")
}

// RelayLinkByID loads one link.
func (s *Store) RelayLinkByID(ctx context.Context, id int64) (RelayLink, error) {
	return scanRelayLink(s.db.QueryRowContext(ctx,
		`SELECT `+relayLinkColumns+` FROM relay_links WHERE id = ?`, id))
}

// RelayLinks lists every link, oldest first. There are a handful at most, so
// the relay holds the whole set in memory and this is only read on a change.
func (s *Store) RelayLinks(ctx context.Context) ([]RelayLink, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+relayLinkColumns+` FROM relay_links ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list relay links: %w", err)
	}
	return scanRelayLinks(rows)
}

// RelayLinkByDiscordChannel finds the link a Discord channel belongs to.
func (s *Store) RelayLinkByDiscordChannel(ctx context.Context, discordChannelID string) (RelayLink, error) {
	return scanRelayLink(s.db.QueryRowContext(ctx,
		`SELECT `+relayLinkColumns+` FROM relay_links WHERE discord_channel_id = ?`,
		discordChannelID))
}

// SetRelayLinkSourceWebhook records which webhooks row inbound messages are
// written under, once one has been provisioned for the link.
func (s *Store) SetRelayLinkSourceWebhook(ctx context.Context, id, webhookID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE relay_links SET source_webhook_id = ? WHERE id = ?`, webhookID, id)
	if err != nil {
		return fmt.Errorf("store: set relay source webhook: %w", err)
	}
	return nil
}

// TouchRelayLink records the outcome of a delivery: when it happened, and what
// went wrong if anything did.
//
// It is on the hot path of every relayed message and is deliberately its own
// statement, so a failure here costs a timestamp rather than the message.
func (s *Store) TouchRelayLink(ctx context.Context, id int64, failure string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE relay_links SET last_relayed_at = ?, last_error = ? WHERE id = ?`,
		now(), failure, id)
	if err != nil {
		return fmt.Errorf("store: touch relay link: %w", err)
	}
	return nil
}

// --- what one message is called on the other side ---------------------------

// RelayMessage pairs the two ids one message has.
type RelayMessage struct {
	AuralID   int64
	LinkID    int64
	DiscordID string
	Origin    string
	CreatedAt int64
}

// MapRelayMessage records the pairing. A message relayed twice — which happens
// when Discord retries a delivery this server already handled — replaces the
// earlier row rather than failing, so the mapping always names the message that
// actually exists.
func (s *Store) MapRelayMessage(ctx context.Context, m RelayMessage) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO relay_messages (aural_id, link_id, discord_id, origin, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(aural_id) DO UPDATE SET
			link_id = excluded.link_id,
			discord_id = excluded.discord_id,
			origin = excluded.origin`,
		m.AuralID, m.LinkID, m.DiscordID, m.Origin, now())
	if err != nil {
		return fmt.Errorf("store: map relay message: %w", err)
	}
	return nil
}

func scanRelayMessage(row interface{ Scan(...any) error }) (RelayMessage, error) {
	var m RelayMessage
	err := row.Scan(&m.AuralID, &m.LinkID, &m.DiscordID, &m.Origin, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RelayMessage{}, ErrNotFound
	}
	if err != nil {
		return RelayMessage{}, fmt.Errorf("store: scan relay message: %w", err)
	}
	return m, nil
}

// RelayMessageByAural finds what an Aural message is called on Discord.
func (s *Store) RelayMessageByAural(ctx context.Context, auralID int64) (RelayMessage, error) {
	return scanRelayMessage(s.db.QueryRowContext(ctx,
		`SELECT aural_id, link_id, discord_id, origin, created_at
		 FROM relay_messages WHERE aural_id = ?`, auralID))
}

// RelayMessageByDiscord finds what a Discord message is called here.
func (s *Store) RelayMessageByDiscord(ctx context.Context, linkID int64, discordID string) (RelayMessage, error) {
	return scanRelayMessage(s.db.QueryRowContext(ctx,
		`SELECT aural_id, link_id, discord_id, origin, created_at
		 FROM relay_messages WHERE link_id = ? AND discord_id = ?`, linkID, discordID))
}

// ForgetRelayMessage drops one pairing, which is what a delete on either side
// leaves behind.
func (s *Store) ForgetRelayMessage(ctx context.Context, auralID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM relay_messages WHERE aural_id = ?`, auralID)
	if err != nil {
		return fmt.Errorf("store: forget relay message: %w", err)
	}
	return nil
}

// SweepRelayMessages drops pairings whose Aural message no longer exists.
//
// The table has no foreign key onto messages on purpose — the row has to
// outlive the delete that reads it — so the rows a bulk deletion leaves behind
// are collected here instead, in bounded batches, on the housekeeping tick.
func (s *Store) SweepRelayMessages(ctx context.Context, limit int) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM relay_messages WHERE aural_id IN (
			SELECT r.aural_id FROM relay_messages r
			LEFT JOIN messages m ON m.id = r.aural_id
			WHERE m.id IS NULL
			LIMIT ?)`, limit)
	if err != nil {
		return 0, fmt.Errorf("store: sweep relay messages: %w", err)
	}
	removed, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return removed, nil
}
