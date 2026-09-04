package store

import (
	"context"
	"testing"
)

func TestWebhookSourceRoundtrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	channels, err := s.AllChannels(ctx)
	if err != nil || len(channels) == 0 {
		t.Fatalf("Channels: %v", err)
	}
	channelID := channels[0].ID

	webhookID := int64(100)
	source := "discord"

	// 1. Relayed Discord webhook message
	relayed, err := s.CreateWebhookMessage(ctx, Message{
		ChannelID:     channelID,
		Author:        "DiscordUser",
		Content:       "Hello from Discord",
		WebhookID:     &webhookID,
		WebhookSource: &source,
	})
	if err != nil {
		t.Fatalf("CreateWebhookMessage (relayed): %v", err)
	}
	if relayed.WebhookSource == nil || *relayed.WebhookSource != "discord" {
		t.Fatalf("expected WebhookSource 'discord', got %v", relayed.WebhookSource)
	}

	loaded, err := s.MessageByID(ctx, relayed.ID)
	if err != nil {
		t.Fatalf("MessageByID: %v", err)
	}
	if loaded.WebhookSource == nil || *loaded.WebhookSource != "discord" {
		t.Fatalf("loaded expected WebhookSource 'discord', got %v", loaded.WebhookSource)
	}

	// Read via timeline MessagesBefore
	history, err := s.MessagesBefore(ctx, channelID, 0, 10)
	if err != nil {
		t.Fatalf("MessagesBefore: %v", err)
	}
	if len(history) == 0 || history[0].WebhookSource == nil || *history[0].WebhookSource != "discord" {
		t.Fatalf("history expected WebhookSource 'discord', got %v", history[0].WebhookSource)
	}

	// 2. Regular webhook message without source
	normalWhID := int64(200)
	normal, err := s.CreateWebhookMessage(ctx, Message{
		ChannelID: channelID,
		Author:    "NormalWebhook",
		Content:   "Hello from normal webhook",
		WebhookID: &normalWhID,
	})
	if err != nil {
		t.Fatalf("CreateWebhookMessage (normal): %v", err)
	}
	if normal.WebhookSource != nil {
		t.Fatalf("expected nil WebhookSource for normal webhook, got %v", *normal.WebhookSource)
	}

	loadedNormal, err := s.MessageByID(ctx, normal.ID)
	if err != nil {
		t.Fatalf("MessageByID normal: %v", err)
	}
	if loadedNormal.WebhookSource != nil {
		t.Fatalf("loaded expected nil WebhookSource, got %v", *loadedNormal.WebhookSource)
	}
}

func TestMigration15Backfill(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	channels, err := s.AllChannels(ctx)
	if err != nil || len(channels) == 0 {
		t.Fatalf("AllChannels: %v", err)
	}
	channelID := channels[0].ID

	whID := int64(300)
	// Create a message without webhook_source (simulating a message created before migration 15)
	msg, err := s.CreateWebhookMessage(ctx, Message{
		ChannelID: channelID,
		Author:    "OldRelayedUser",
		Content:   "Relayed before migration 15",
		WebhookID: &whID,
	})
	if err != nil {
		t.Fatalf("CreateWebhookMessage: %v", err)
	}

	// Link it via relay_messages with origin = 'discord'
	link, err := s.CreateRelayLink(ctx, RelayLink{
		ChannelID:        channelID,
		DiscordGuildID:   "guild-1",
		DiscordChannelID: "chan-1",
		WebhookID:        "wh-1",
		WebhookToken:     "tok-1",
		SourceWebhookID:  &whID,
		Direction:        RelayBoth,
		Enabled:          true,
	})
	if err != nil {
		t.Fatalf("CreateRelayLink: %v", err)
	}

	if err := s.MapRelayMessage(ctx, RelayMessage{
		AuralID:   msg.ID,
		LinkID:    link.ID,
		DiscordID: "discord-msg-1",
		Origin:    RelayOriginDiscord,
	}); err != nil {
		t.Fatalf("MapRelayMessage: %v", err)
	}

	// Clear webhook_source if set
	if _, err := s.DB().ExecContext(ctx, `UPDATE messages SET webhook_source = NULL WHERE id = ?`, msg.ID); err != nil {
		t.Fatalf("clear webhook_source: %v", err)
	}

	// Run migration 15 query
	if _, err := s.DB().ExecContext(ctx, `
		UPDATE messages
		SET webhook_source = 'discord'
		WHERE id IN (SELECT aural_id FROM relay_messages WHERE origin = 'discord')
		   OR (webhook_id IS NOT NULL AND webhook_id IN (SELECT source_webhook_id FROM relay_links WHERE source_webhook_id IS NOT NULL));
	`); err != nil {
		t.Fatalf("run migration 15 backfill query: %v", err)
	}

	// Verify it now has WebhookSource = 'discord'
	reloaded, err := s.MessageByID(ctx, msg.ID)
	if err != nil {
		t.Fatalf("MessageByID: %v", err)
	}
	if reloaded.WebhookSource == nil || *reloaded.WebhookSource != "discord" {
		t.Fatalf("expected backfilled WebhookSource 'discord', got %v", reloaded.WebhookSource)
	}
}
