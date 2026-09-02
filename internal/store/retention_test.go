package store

import (
	"context"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()

	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// age backdates a user's last_seen_at, which is what the sweep reads.
func age(t *testing.T, s *Store, userID, seenAt int64) {
	t.Helper()

	if _, err := s.DB().ExecContext(context.Background(),
		`UPDATE users SET last_seen_at = ? WHERE id = ?`, seenAt, userID); err != nil {
		t.Fatalf("age user: %v", err)
	}
}

// ageToken backdates a token's last_used_at.
func ageToken(t *testing.T, s *Store, hash string, usedAt int64) {
	t.Helper()

	if _, err := s.DB().ExecContext(context.Background(),
		`UPDATE tokens SET last_used_at = ? WHERE token_hash = ?`, usedAt, hash); err != nil {
		t.Fatalf("age token: %v", err)
	}
}

func countRows(t *testing.T, s *Store, query string) int {
	t.Helper()

	var n int
	if err := s.DB().QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestPruneStaleTokensRevokesOnlyTheIdleOnes(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	user, err := s.CreateGuest(ctx, "Someone")
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}
	for _, hash := range []string{"old", "fresh"} {
		if err := s.CreateToken(ctx, user.ID, hash, "test"); err != nil {
			t.Fatalf("CreateToken: %v", err)
		}
	}
	ageToken(t, s, "old", 1000)

	revoked, err := s.PruneStaleTokens(ctx, 2000)
	if err != nil {
		t.Fatalf("PruneStaleTokens: %v", err)
	}
	if revoked != 1 {
		t.Errorf("revoked %d tokens, want 1", revoked)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM tokens WHERE token_hash = 'fresh'`); got != 1 {
		t.Error("a token still in use was revoked")
	}
}

// The sweep must never cost anybody an account or a conversation. It is one of
// the few things here that deletes rows, so what it leaves alone matters more
// than what it removes.
func TestPruneStaleGuestsLeavesAccountsAndHistoryAlone(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	// A registered account, long idle and holding no token: still an account.
	account, err := s.CreateGuest(ctx, "Registered")
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}
	if _, err := s.ClaimIdentity(ctx, account.ID, "someone", "hash"); err != nil {
		t.Fatalf("ClaimIdentity: %v", err)
	}
	age(t, s, account.ID, 1000)

	// A guest who visited once, wrote something, and can never come back.
	gone, err := s.CreateGuest(ctx, "Passer By")
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}
	age(t, s, gone.ID, 1000)

	channels, err := s.AllChannels(ctx)
	if err != nil {
		t.Fatalf("AllChannels: %v", err)
	}
	var textChannel int64
	for _, c := range channels {
		if c.Type == "text" {
			textChannel = c.ID
			break
		}
	}
	if textChannel == 0 {
		t.Fatal("the seeded database has no text channel to post in")
	}
	if _, err := s.CreateMessage(ctx, textChannel, gone.ID, "still here"); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	// A guest who still holds a token, however long ago they were seen: they
	// can come back as themselves, so the row is still theirs.
	returning, err := s.CreateGuest(ctx, "Coming Back")
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}
	age(t, s, returning.ID, 1000)
	if err := s.CreateToken(ctx, returning.ID, "still-valid", "test"); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	// A guest who was here a moment ago.
	recent, err := s.CreateGuest(ctx, "Just Arrived")
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}

	removed, err := s.PruneStaleGuests(ctx, 2000)
	if err != nil {
		t.Fatalf("PruneStaleGuests: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed %d identities, want 1", removed)
	}

	for _, kept := range []struct {
		id   int64
		why  string
		name string
	}{
		{account.ID, "a registered account was deleted", "Registered"},
		{returning.ID, "a guest holding a valid token was deleted", "Coming Back"},
		{recent.ID, "a guest seen moments ago was deleted", "Just Arrived"},
	} {
		if _, err := s.UserByID(ctx, kept.id); err != nil {
			t.Errorf("%s (%s): %v", kept.why, kept.name, err)
		}
	}
	if _, err := s.UserByID(ctx, gone.ID); err == nil {
		t.Error("the unreachable guest identity survived the sweep")
	}

	// The conversation is the part that must not move. The row is gone, but
	// the message it wrote stays, attributed to the name captured on it.
	messages, err := s.MessagesBefore(ctx, textChannel, 0, 10)
	if err != nil {
		t.Fatalf("MessagesBefore: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("history holds %d messages, want 1: deleting an author must not delete what they said", len(messages))
	}
	if messages[0].Content != "still here" {
		t.Errorf("the message changed: %q", messages[0].Content)
	}
	if messages[0].Author != "Passer By" {
		t.Errorf("the message lost its author, showing %q", messages[0].Author)
	}
}
