package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, ctx
}

// TestFindBanMatchesAnyHandle is the whole point of splitting a ban from the
// things it catches: which handle somebody comes back on is exactly what cannot
// be predicted, so all of them have to hit the same ban.
func TestFindBanMatchesAnyHandle(t *testing.T) {
	s, ctx := openStore(t)

	created, err := s.CreateBan(ctx, Ban{
		UserNickname: "Bruno",
		Reason:       "spam",
		Matches: []BanMatch{
			{Kind: MatchUser, Value: "7"},
			{Kind: MatchIP, Value: "203.0.113.4"},
			{Kind: MatchDevice, Value: "abc123"},
		},
	})
	if err != nil {
		t.Fatalf("CreateBan: %v", err)
	}

	now := time.Now().Unix()
	for _, match := range created.Matches {
		found, err := s.FindBan(ctx, now, []BanMatch{match})
		if err != nil {
			t.Fatalf("FindBan on %s: %v", match.Kind, err)
		}
		if found.ID != created.ID {
			t.Fatalf("FindBan on %s found ban %d, want %d", match.Kind, found.ID, created.ID)
		}
	}

	// A connection presenting none of them is not caught.
	_, err = s.FindBan(ctx, now, []BanMatch{
		{Kind: MatchUser, Value: "8"},
		{Kind: MatchIP, Value: "198.51.100.1"},
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindBan on unrelated handles: %v, want ErrNotFound", err)
	}

	// An empty handle is one the client did not send, and must match nothing:
	// otherwise every connection with no device identifier would be caught by
	// every ban that had none either.
	_, err = s.FindBan(ctx, now, []BanMatch{{Kind: MatchDevice, Value: ""}})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindBan on an empty handle: %v, want ErrNotFound", err)
	}
}

// TestFindBanIgnoresOneThatHasRunOut pins that a temporary ban stops being
// enforced the moment it expires, rather than when a housekeeping tick notices.
func TestFindBanIgnoresOneThatHasRunOut(t *testing.T) {
	s, ctx := openStore(t)

	expired := time.Now().Add(-time.Hour).Unix()
	if _, err := s.CreateBan(ctx, Ban{
		UserNickname: "Bruno",
		ExpiresAt:    &expired,
		Matches:      []BanMatch{{Kind: MatchIP, Value: "203.0.113.4"}},
	}); err != nil {
		t.Fatalf("CreateBan: %v", err)
	}

	_, err := s.FindBan(ctx, time.Now().Unix(), []BanMatch{{Kind: MatchIP, Value: "203.0.113.4"}})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindBan on an expired ban: %v, want ErrNotFound", err)
	}

	// It is still in the list, because the list is a record of what was done.
	bans, err := s.ListBans(ctx, 0)
	if err != nil || len(bans) != 1 {
		t.Fatalf("ListBans: got %d bans, %v", len(bans), err)
	}
	if bans[0].Active(time.Now().Unix()) {
		t.Fatal("an expired ban reported itself as active")
	}
}

// TestCreateBanTakesOverAHandleAnotherBanHeld pins the tie-break: two bans
// matching the same address is not a state the check could resolve, so the
// newer decision — the one a moderator just made — wins.
func TestCreateBanTakesOverAHandleAnotherBanHeld(t *testing.T) {
	s, ctx := openStore(t)

	first, err := s.CreateBan(ctx, Ban{
		UserNickname: "Bruno",
		Matches:      []BanMatch{{Kind: MatchIP, Value: "203.0.113.4"}},
	})
	if err != nil {
		t.Fatalf("CreateBan: %v", err)
	}
	second, err := s.CreateBan(ctx, Ban{
		UserNickname: "Ana",
		Matches:      []BanMatch{{Kind: MatchIP, Value: "203.0.113.4"}},
	})
	if err != nil {
		t.Fatalf("CreateBan: %v", err)
	}

	found, err := s.FindBan(ctx, time.Now().Unix(), []BanMatch{{Kind: MatchIP, Value: "203.0.113.4"}})
	if err != nil {
		t.Fatalf("FindBan: %v", err)
	}
	if found.ID != second.ID {
		t.Fatalf("the address resolved to ban %d, want the newer one %d", found.ID, second.ID)
	}

	// Lifting the older ban must not take the address with it: it belongs to
	// the newer one now.
	if err := s.DeleteBan(ctx, first.ID); err != nil {
		t.Fatalf("DeleteBan: %v", err)
	}
	if _, err := s.FindBan(ctx, time.Now().Unix(),
		[]BanMatch{{Kind: MatchIP, Value: "203.0.113.4"}}); err != nil {
		t.Fatalf("FindBan after lifting the older ban: %v", err)
	}
}

// TestIdentityMarksRememberWhereSomebodyConnectedFrom covers what makes banning
// a guest mean anything: the identity is gone the moment it is banned, so the
// addresses and devices behind it have to have been written down beforehand.
func TestIdentityMarksRememberWhereSomebodyConnectedFrom(t *testing.T) {
	s, ctx := openStore(t)

	user, err := s.CreateGuest(ctx, "Bruno")
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}

	for range 3 {
		if err := s.RecordIdentityMark(ctx, user.ID,
			IdentityMark{IP: "203.0.113.4", Device: "abc"}); err != nil {
			t.Fatalf("RecordIdentityMark: %v", err)
		}
	}
	if err := s.RecordIdentityMark(ctx, user.ID,
		IdentityMark{IP: "198.51.100.9", Device: "abc"}); err != nil {
		t.Fatalf("RecordIdentityMark: %v", err)
	}
	// A connection that presented neither is not a place worth remembering.
	if err := s.RecordIdentityMark(ctx, user.ID, IdentityMark{}); err != nil {
		t.Fatalf("RecordIdentityMark on an empty mark: %v", err)
	}

	marks, err := s.IdentityMarks(ctx, user.ID, 8)
	if err != nil {
		t.Fatalf("IdentityMarks: %v", err)
	}
	if len(marks) != 2 {
		t.Fatalf("got %d marks, want the two distinct ones: %v", len(marks), marks)
	}
}
