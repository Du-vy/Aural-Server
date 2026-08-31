package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestStoreStats(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	stats, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	// Default seeded database should have 3 roles and 3 channels (1 category, 1 text, 1 voice)
	if stats.TotalRoles != 3 {
		t.Errorf("expected 3 roles, got %d", stats.TotalRoles)
	}
	if stats.TotalChannels != 3 {
		t.Errorf("expected 3 channels, got %d", stats.TotalChannels)
	}
	if stats.Categories != 1 || stats.TextChannels != 1 || stats.VoiceChannels != 1 {
		t.Errorf("expected 1 category, 1 text, 1 voice; got %d cat, %d text, %d voice",
			stats.Categories, stats.TextChannels, stats.VoiceChannels)
	}
	if stats.TotalUsers != 0 || stats.RegisteredUsers != 0 {
		t.Errorf("expected 0 users, got %d total, %d registered", stats.TotalUsers, stats.RegisteredUsers)
	}

	// Create a guest and register a user
	guest, err := s.CreateGuest(ctx, "Guest1")
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}
	_, err = s.ClaimIdentity(ctx, guest.ID, "user1", "hashed_pw")
	if err != nil {
		t.Fatalf("ClaimIdentity: %v", err)
	}

	stats, err = s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.TotalUsers != 1 || stats.RegisteredUsers != 1 {
		t.Errorf("expected 1 registered user, got total=%d registered=%d", stats.TotalUsers, stats.RegisteredUsers)
	}
}
