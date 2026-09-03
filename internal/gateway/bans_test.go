package gateway_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
)

// guestWithDevice authenticates as a guest presenting a machine identifier,
// which is what makes a ban able to follow somebody who comes back.
func (c *client) guestWithDevice(nickname, device string) protocol.Ready {
	c.t.Helper()
	return ok[protocol.Ready](c, protocol.OpAuthGuest,
		protocol.AuthGuestRequest{Nickname: nickname, Device: device})
}

// TestHelloCarriesADeviceSalt pins the thing the whole device identifier rests
// on: it is salted per server, so the same machine presents a different value
// everywhere and nothing here can follow somebody between servers.
func TestHelloCarriesADeviceSalt(t *testing.T) {
	h := newHarness(t, nil)

	first := h.dial()
	if len(first.hello.DeviceSalt) < 16 {
		t.Fatalf("device salt is too short to be one: %q", first.hello.DeviceSalt)
	}

	// Stable for the life of the server: a salt that changed would make every
	// identifier new, and a ban against a machine would last as long as the
	// process did.
	second := h.dial()
	if second.hello.DeviceSalt != first.hello.DeviceSalt {
		t.Fatal("two connections to one server were given different salts")
	}
}

// TestBanReachesTheDeviceBehindAFreshGuest is the whole point of the feature.
// Banning the account of somebody who can make another account for free buys
// nothing; banning the machine is what makes coming straight back not work.
func TestBanReachesTheDeviceBehindAFreshGuest(t *testing.T) {
	h := newHarness(t, nil)
	admin, _ := h.admin("Owner")

	troublemaker := h.dial()
	target := troublemaker.guestWithDevice("Nuisance", "aaaa1111")

	banned := ok[protocol.BanEvent](admin, protocol.OpBanCreate, protocol.BanCreateRequest{
		UserID:      target.User.ID,
		Reason:      "spam",
		MatchDevice: boolPtr(true),
		MatchIP:     boolPtr(false),
	})
	if banned.Ban.UserNickname != "Nuisance" {
		t.Fatalf("the ban did not record who it was for: %+v", banned.Ban)
	}
	if !hasMatch(banned.Ban, protocol.MatchDevice) {
		t.Fatalf("the ban did not pick up the device: %+v", banned.Ban.Matches)
	}

	// A brand new identity from the same machine is refused.
	returning := h.dial()
	returning.fails(protocol.OpAuthGuest,
		protocol.AuthGuestRequest{Nickname: "Nuisance again", Device: "aaaa1111"},
		protocol.ErrBanned)

	// Another machine is not.
	stranger := h.dial()
	if ready := stranger.guestWithDevice("Somebody", "bbbb2222"); ready.User.ID == 0 {
		t.Fatal("an unrelated device was caught by the ban")
	}

	// Lifting it lets the machine back.
	ok[protocol.BanDeletedEvent](admin, protocol.OpBanDelete,
		protocol.BanDeleteRequest{BanID: banned.Ban.ID})

	back := h.dial()
	if ready := back.guestWithDevice("Nuisance", "aaaa1111"); ready.User.ID == 0 {
		t.Fatal("lifting the ban did not let the machine back")
	}
}

// TestBanDoesNotCatchTheModeratorsOwnAddress covers the way this feature would
// otherwise go wrong in practice: an address is shared by a household and a
// whole phone network, so banning somebody from your own network must not lock
// you out of your own server.
//
// Every connection in a test comes from the loopback address, which is exactly
// the shape of that problem.
func TestBanDoesNotCatchTheModeratorsOwnAddress(t *testing.T) {
	h := newHarness(t, nil)
	admin, _ := h.admin("Owner")

	troublemaker := h.dial()
	target := troublemaker.guestWithDevice("Nuisance", "aaaa1111")

	banned := ok[protocol.BanEvent](admin, protocol.OpBanCreate, protocol.BanCreateRequest{
		UserID:      target.User.ID,
		MatchIP:     boolPtr(true),
		MatchDevice: boolPtr(true),
	})
	if hasMatch(banned.Ban, protocol.MatchIP) {
		t.Fatal("the ban attached an address the moderator is also sitting behind")
	}
	if !hasMatch(banned.Ban, protocol.MatchDevice) {
		t.Fatal("dropping the address should not have dropped the device with it")
	}

	// The moderator's own connection still works, which a ban that had caught
	// their address would have ended.
	list := ok[protocol.BanListResult](admin, protocol.OpBanList, protocol.BanListRequest{})
	if len(list.Bans) != 1 {
		t.Fatalf("ban list: got %d entries, want 1", len(list.Bans))
	}
}

// TestBanListNeverCarriesTheHandleValues pins a privacy property rather than a
// behaviour: a moderator deciding whether to lift a ban needs to know that it
// reaches a machine, not which machine, and an address identifies somebody off
// this server as well as on it.
func TestBanListNeverCarriesTheHandleValues(t *testing.T) {
	h := newHarness(t, nil)
	admin, _ := h.admin("Owner")

	troublemaker := h.dial()
	target := troublemaker.guestWithDevice("Nuisance", "secretdevicehash")

	ok[protocol.BanEvent](admin, protocol.OpBanCreate, protocol.BanCreateRequest{
		UserID:      target.User.ID,
		MatchDevice: boolPtr(true),
	})

	list := ok[protocol.BanListResult](admin, protocol.OpBanList, protocol.BanListRequest{})
	for _, ban := range list.Bans {
		for _, match := range ban.Matches {
			if match.Count < 1 {
				t.Fatalf("a handle was listed with no count: %+v", match)
			}
		}
	}
	if containsAny(list, "secretdevicehash") {
		t.Fatal("the ban list carried the device hash itself")
	}
}

// TestBanningNeedsThePermissionAndTheRank pins that a ban is not something an
// ordinary member can issue, and not something anybody can aim upwards.
func TestBanningNeedsThePermissionAndTheRank(t *testing.T) {
	h := newHarness(t, nil)
	admin, ready := h.admin("Owner")

	ordinary := h.dial()
	ordinary.guestWithDevice("Member", "aaaa1111")

	ordinary.fails(protocol.OpBanCreate,
		protocol.BanCreateRequest{UserID: ready.User.ID}, protocol.ErrForbidden)
	ordinary.fails(protocol.OpBanList, protocol.BanListRequest{}, protocol.ErrForbidden)

	// Nobody bans the owner, whatever they hold.
	admin.fails(protocol.OpBanCreate,
		protocol.BanCreateRequest{UserID: ready.User.ID}, protocol.ErrBadRequest)
}

// TestTheOwnerIsNeverRefused covers the last resort: a server whose owner
// cannot get in has no way back, so the owner is exempt from every ban however
// it was issued.
func TestTheOwnerIsNeverRefused(t *testing.T) {
	h := newHarness(t, nil)
	admin, ready := h.admin("Owner")

	troublemaker := h.dial()
	target := troublemaker.guestWithDevice("Nuisance", "shareddevice")

	ok[protocol.BanEvent](admin, protocol.OpBanCreate, protocol.BanCreateRequest{
		UserID:      target.User.ID,
		MatchDevice: boolPtr(true),
	})

	// The owner comes back from the very machine the ban names.
	returning := h.dial()
	resumed := ok[protocol.Ready](returning, protocol.OpAuthToken, protocol.AuthTokenRequest{
		Token:  ready.SessionToken,
		Device: "shareddevice",
	})
	if resumed.User.ID != ready.User.ID {
		t.Fatalf("the owner was not let back in: got %d, want %d", resumed.User.ID, ready.User.ID)
	}
}

// TestModerationIsWrittenToTheAuditLog pins that the log is produced by the
// actions themselves. A record a client could append to would be a record of
// what somebody claimed to have done.
func TestModerationIsWrittenToTheAuditLog(t *testing.T) {
	h := newHarness(t, nil)
	admin, _ := h.admin("Owner")

	troublemaker := h.dial()
	target := troublemaker.guestWithDevice("Nuisance", "aaaa1111")

	ok[protocol.BanEvent](admin, protocol.OpBanCreate, protocol.BanCreateRequest{
		UserID:      target.User.ID,
		Reason:      "spam",
		MatchDevice: boolPtr(true),
	})

	log := ok[protocol.AuditListResult](admin, protocol.OpAuditList, protocol.AuditListRequest{})
	var found *protocol.AuditEntry
	for i, entry := range log.Entries {
		if entry.Action == protocol.AuditUserBan {
			found = &log.Entries[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("the ban is not in the log: %+v", log.Entries)
	}
	if found.TargetName != "Nuisance" || found.Reason != "spam" {
		t.Fatalf("the entry does not describe what happened: %+v", found)
	}
	if found.ActorName != "Owner" {
		t.Fatalf("the entry does not name who did it: %+v", found)
	}

	// Reading it is its own permission, and an ordinary member does not have it.
	ordinary := h.dial()
	ordinary.guest("Member")
	ordinary.fails(protocol.OpAuditList, protocol.AuditListRequest{}, protocol.ErrForbidden)
}

// TestAutoModCensorsAndBlocks drives the engine through the gateway rather than
// in isolation, which is the only way to check the one thing that matters most
// about it: a blocked message is never written, and a censored one is never
// written uncensored.
func TestAutoModCensorsAndBlocks(t *testing.T) {
	h := newHarness(t, nil)
	admin, ready := h.admin("Owner")
	channel := textChannel(t, ready).ID

	config := protocol.DefaultAutoMod()
	config.Enabled = true
	config.Words.Enabled = true
	config.Words.Action = protocol.AutoModCensor
	config.Words.Words = []string{"tonto"}
	config.Links.Enabled = true
	config.Links.Action = protocol.AutoModBlock
	config.Links.AllowedDomains = []string{"github.com"}

	ok[protocol.AutoModResult](admin, protocol.OpAutoModUpdate,
		protocol.AutoModUpdateRequest{Config: config})

	// The owner holds Administrator, which is exemption in itself, so the rules
	// are exercised from an ordinary guest.
	member := h.dial()
	member.guest("Member")

	censored := ok[protocol.MessageEvent](member, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel, Content: "eres un TÓNTO total"})
	if contains(censored.Message.Content, "tonto") || contains(censored.Message.Content, "TÓNTO") {
		t.Fatalf("the listed word survived: %q", censored.Message.Content)
	}
	if !contains(censored.Message.Content, "eres un ") {
		t.Fatalf("censoring disturbed the rest of the sentence: %q", censored.Message.Content)
	}

	member.fails(protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel, Content: "see https://evil.example/x"},
		protocol.ErrAutoModBlocked)

	allowed := ok[protocol.MessageEvent](member, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel, Content: "see https://github.com/a/b"})
	if !contains(allowed.Message.Content, "github.com") {
		t.Fatalf("an allowed domain was masked: %q", allowed.Message.Content)
	}

	// An administrator is outside the rules, which is the same rule the
	// permission mask follows everywhere else on this server.
	said := ok[protocol.MessageEvent](admin, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel, Content: "tonto https://evil.example"})
	if !contains(said.Message.Content, "tonto") {
		t.Fatalf("an administrator was censored: %q", said.Message.Content)
	}
}

// TestAutoModExemptsARole covers the half of the feature every server actually
// configures: staff write what they like, everybody else does not.
func TestAutoModExemptsARole(t *testing.T) {
	h := newHarness(t, nil)
	admin, ready := h.admin("Owner")
	channel := textChannel(t, ready).ID

	role := ok[protocol.RoleEvent](admin, protocol.OpRoleCreate,
		protocol.RoleCreateRequest{Name: "Staff", Permissions: permissions.None.String()})

	member := h.dial()
	memberReady := member.guest("Member")

	config := protocol.DefaultAutoMod()
	config.Enabled = true
	config.Words.Enabled = true
	config.Words.Action = protocol.AutoModBlock
	config.Words.Words = []string{"tonto"}
	config.Words.ExemptRoles = []int64{role.Role.ID}

	ok[protocol.AutoModResult](admin, protocol.OpAutoModUpdate,
		protocol.AutoModUpdateRequest{Config: config})

	member.fails(protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel, Content: "tonto"},
		protocol.ErrAutoModBlocked)

	ok[protocol.UserEvent](admin, protocol.OpRoleAssign,
		protocol.RoleMembershipRequest{UserID: memberReady.User.ID, RoleID: role.Role.ID})
	// Granting a role hands the member a fresh snapshot, which has to be read
	// off the wire before the next reply can be matched.
	member.waitEvent(protocol.EvReady)

	said := ok[protocol.MessageEvent](member, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel, Content: "tonto"})
	if !contains(said.Message.Content, "tonto") {
		t.Fatalf("an exempt role was still moderated: %q", said.Message.Content)
	}
}

// --- helpers ----------------------------------------------------------------

func boolPtr(v bool) *bool { return &v }

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// containsAny renders the whole result and looks for a string in it, which is
// how a test asks "did any of this leak" without knowing where it would sit.
func containsAny(value any, needle string) bool {
	raw, err := json.Marshal(value)
	if err != nil {
		return false
	}
	return strings.Contains(string(raw), needle)
}

func hasMatch(ban protocol.Ban, kind string) bool {
	for _, match := range ban.Matches {
		if match.Kind == kind {
			return true
		}
	}
	return false
}
