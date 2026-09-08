package gateway_test

import (
	"testing"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// TestProfileCosmeticsSyncAndBroadcast verifies that profile theme colors and avatar frames
// are validated, stored in the database, and broadcast to other connected sessions.
func TestProfileCosmeticsSyncAndBroadcast(t *testing.T) {
	h := newHarness(t, nil)
	alice, _ := h.member("Alice", "alice")
	bob, _ := h.member("Bob", "bob")

	// 1. Invalid theme color rejected
	invalidColor := "not-a-color"
	alice.fails(protocol.OpUserUpdate, protocol.UserUpdateRequest{
		ThemeColor: &invalidColor,
	}, protocol.ErrBadRequest)

	// 2. Invalid frame style rejected
	invalidFrame := &protocol.CustomAvatarFrame{
		Style:     "spinning-triangle",
		ColorMode: "custom",
		Animation: "spin",
	}
	alice.fails(protocol.OpUserUpdate, protocol.UserUpdateRequest{
		CustomFrame: invalidFrame,
	}, protocol.ErrBadRequest)

	// 3. Set valid theme color and gradient spin frame
	themeColor := "#5865f2"
	validFrame := &protocol.CustomAvatarFrame{
		Style:        "glow",
		ColorMode:    "gradient",
		CustomColor:  "#06b6d4",
		CustomColor2: "#8b5cf6",
		Animation:    "spin",
	}
	updated := ok[protocol.UserEvent](alice, protocol.OpUserUpdate, protocol.UserUpdateRequest{
		ThemeColor:  &themeColor,
		CustomFrame: validFrame,
	})

	if updated.User.ThemeColor != "#5865f2" {
		t.Fatalf("alice returned theme color %q, want #5865f2", updated.User.ThemeColor)
	}
	if updated.User.CustomFrame == nil {
		t.Fatal("alice returned nil CustomFrame")
	}
	if updated.User.CustomFrame.Style != "glow" || updated.User.CustomFrame.Animation != "spin" {
		t.Fatalf("alice CustomFrame mismatch: %+v", updated.User.CustomFrame)
	}

	// 4. Bob receives user.updated broadcast with Alice's cosmetics
	var bobSeen protocol.User
	for range 10 {
		env := bob.waitEvent(protocol.EvUserUpdated)
		u := userOf(t, env)
		if u.ID == updated.User.ID {
			bobSeen = u
			break
		}
	}
	if bobSeen.ID != updated.User.ID {
		t.Fatalf("bob never received user update for alice (%d)", updated.User.ID)
	}
	if bobSeen.ThemeColor != "#5865f2" {
		t.Fatalf("bob sees alice theme color %q, want #5865f2", bobSeen.ThemeColor)
	}
	if bobSeen.CustomFrame == nil || bobSeen.CustomFrame.Style != "glow" || bobSeen.CustomFrame.ColorMode != "gradient" {
		t.Fatalf("bob sees unexpected CustomFrame: %+v", bobSeen.CustomFrame)
	}
	if bobSeen.CustomFrame.CustomColor != "#06b6d4" || bobSeen.CustomFrame.CustomColor2 != "#8b5cf6" {
		t.Fatalf("bob sees unexpected frame colors: %+v", bobSeen.CustomFrame)
	}

	// 5. Connecting client receives Alice's cosmetics in Ready.Users
	charlie := h.dial()
	ready := charlie.guest("Charlie")
	foundAlice, okFound := userInList(ready.Users, updated.User.ID)
	if !okFound {
		t.Fatalf("charlie ready snapshot missing alice")
	}
	if foundAlice.ThemeColor != "#5865f2" {
		t.Fatalf("charlie sees alice theme color %q in snapshot, want #5865f2", foundAlice.ThemeColor)
	}
	if foundAlice.CustomFrame == nil || foundAlice.CustomFrame.Style != "glow" {
		t.Fatalf("charlie sees invalid custom frame in snapshot: %+v", foundAlice.CustomFrame)
	}

	// 6. Resetting / clearing theme color and frame
	emptyColor := ""
	noneFrame := &protocol.CustomAvatarFrame{Style: "none"}
	cleared := ok[protocol.UserEvent](alice, protocol.OpUserUpdate, protocol.UserUpdateRequest{
		ThemeColor:  &emptyColor,
		CustomFrame: noneFrame,
	})
	if cleared.User.ThemeColor != "" {
		t.Fatalf("expected cleared theme color, got %q", cleared.User.ThemeColor)
	}
	if cleared.User.CustomFrame != nil {
		t.Fatalf("expected cleared CustomFrame (nil), got %+v", cleared.User.CustomFrame)
	}
}
