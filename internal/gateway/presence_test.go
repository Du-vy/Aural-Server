package gateway_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// quietFor is how long a test waits before deciding a frame is never coming.
// It only ever costs that wait when a test is asserting silence.
const quietFor = 300 * time.Millisecond

// eventWithin reads until an event with the given op arrives or the wait runs
// out. It is what lets a test assert that a frame never came, which is most of
// what hiding presence amounts to.
func (c *client) eventWithin(op string, within time.Duration) (protocol.Envelope, bool) {
	c.t.Helper()

	for i, env := range c.pending {
		if env.Op == op {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			return env, true
		}
	}

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithDeadline(c.ctx, deadline)
		_, raw, err := c.conn.Read(ctx)
		cancel()
		if err != nil {
			return protocol.Envelope{}, false
		}
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			c.t.Fatalf("decode frame: %v", err)
		}
		if env.Op == op {
			return env, true
		}
		c.pending = append(c.pending, env)
	}
	return protocol.Envelope{}, false
}

// setStatus changes the caller's own presence.
func (c *client) setStatus(status string) {
	c.t.Helper()
	ok[protocol.UserEvent](c, protocol.OpUserUpdate, protocol.UserUpdateRequest{Status: &status})
}

// userEvent decodes the user carried by a presence frame.
func userOf(t *testing.T, env protocol.Envelope) protocol.User {
	t.Helper()
	var event protocol.UserEvent
	if err := json.Unmarshal(env.Data, &event); err != nil {
		t.Fatalf("decode user event: %v", err)
	}
	return event.User
}

// member authenticates a fresh connection and claims an account on it, which
// is what makes the identity outlast the connection that made it.
func (h *harness) member(nickname, username string) (*client, protocol.Ready) {
	h.t.Helper()

	c := h.dial()
	ready := c.guest(nickname)
	claimed := ok[protocol.AuthRegisterResult](c, protocol.OpAuthRegister,
		protocol.AuthRegisterRequest{Username: username, Password: "correct-horse"})
	ready.User = claimed.User
	return c, ready
}

// userInList finds one user in a member list.
func userInList(users []protocol.User, id int64) (protocol.User, bool) {
	for _, u := range users {
		if u.ID == id {
			return u, true
		}
	}
	return protocol.User{}, false
}

// requireOffline asserts the shape every entry for somebody who is not here
// takes. It is the shape hiding disappears into, so a field that came through
// it would be a field that tells the two apart.
func requireOffline(t *testing.T, u protocol.User, what string) {
	t.Helper()
	if u.Online {
		t.Fatalf("%s is listed as online", what)
	}
	if u.Status != "offline" {
		t.Fatalf("%s has status %q, want offline", what, u.Status)
	}
	if u.ChannelID != nil {
		t.Fatalf("%s is listed in channel %d", what, *u.ChannelID)
	}
	if u.CustomStatus != "" {
		t.Fatalf("%s carries the custom status %q", what, u.CustomStatus)
	}
}

func TestInvisibleGuestIsAbsentFromTheSnapshot(t *testing.T) {
	h := newHarness(t, nil)

	visible := h.dial()
	visible.guest("Ana")

	hidden := h.dial()
	hiddenReady := hidden.guest("Bruno")
	hidden.setStatus("invisible")

	// A third client arriving afterwards is the plainest reading of the
	// snapshot: it has no history to reconcile it against.
	arriving := h.dial()
	ready := arriving.guest("Carla")

	for _, u := range ready.Users {
		if u.ID == hiddenReady.User.ID {
			t.Fatalf("an invisible guest was listed in the snapshot as %+v", u)
		}
	}
	// A guest has no offline entry to hide in: the identity lasts no longer
	// than the connection, so one who is not shown as connected is not shown
	// at all.
	if len(ready.Users) != 2 {
		t.Fatalf("snapshot users: got %d, want 2 (the visible guest and the arrival)", len(ready.Users))
	}
}

func TestAnAbsentMemberIsStillListed(t *testing.T) {
	h := newHarness(t, nil)

	watcher := h.dial()
	watcher.guest("Ana")

	absent, absentReady := h.member("Bruno", "bruno")
	watcher.waitEvent(protocol.EvUserConnected)
	absent.conn.Close(websocket.StatusNormalClosure, "")
	// The departure is what says the server has finished letting go of the
	// session, which the snapshot below has to be taken after.
	watcher.waitEvent(protocol.EvUserDisconnected)

	arriving := h.dial()
	ready := arriving.guest("Carla")

	listed, found := userInList(ready.Users, absentReady.User.ID)
	if !found {
		t.Fatal("a member who is not connected was left out of the snapshot")
	}
	requireOffline(t, listed, "an absent member")
	if listed.Nickname != "Bruno" {
		t.Fatalf("absent member nickname: got %q, want Bruno", listed.Nickname)
	}
	if !listed.Registered {
		t.Fatal("the absent member is not marked as registered")
	}
}

func TestAnInvisibleMemberIsListedAsOffline(t *testing.T) {
	h := newHarness(t, nil)

	hidden, hiddenReady := h.member("Bruno", "bruno")
	custom := "in a meeting"
	ok[protocol.UserEvent](hidden, protocol.OpUserUpdate,
		protocol.UserUpdateRequest{CustomStatus: &custom})
	hidden.setStatus("invisible")

	// A second member who really is away is what the hidden one has to be
	// indistinguishable from.
	away, awayReady := h.member("Dora", "dora")
	away.conn.Close(websocket.StatusNormalClosure, "")

	arriving := h.dial()
	ready := arriving.guest("Carla")

	listed, found := userInList(ready.Users, hiddenReady.User.ID)
	if !found {
		t.Fatal("an invisible member was left out of the snapshot rather than shown as offline")
	}
	requireOffline(t, listed, "an invisible member")

	// The hidden entry and the genuinely absent one have to agree on every
	// field that is not the person: anything else is what would tell a watcher
	// which of the two they are looking at.
	gone, found := userInList(ready.Users, awayReady.User.ID)
	if !found {
		t.Fatal("a member who is not connected was left out of the snapshot")
	}
	if listed.Online != gone.Online || listed.Status != gone.Status ||
		listed.CustomStatus != gone.CustomStatus || (listed.ChannelID == nil) != (gone.ChannelID == nil) {
		t.Fatalf("hiding is visible: hidden %+v, absent %+v", listed, gone)
	}
}

func TestRenamingAnAbsentMemberReachesEverybody(t *testing.T) {
	h := newHarness(t, nil)

	admin, _ := h.admin("Ana")

	absent, absentReady := h.member("Bruno", "bruno")
	admin.waitEvent(protocol.EvUserConnected)
	absent.conn.Close(websocket.StatusNormalClosure, "")
	admin.waitEvent(protocol.EvUserDisconnected)

	watcher := h.dial()
	watcher.guest("Carla")

	// A rename is one of the few things that happens to somebody who is not
	// here, and their entry is on show, so it has to be announced.
	renamed := "Bruno the Absent"
	ok[protocol.UserEvent](admin, protocol.OpUserUpdate,
		protocol.UserUpdateRequest{UserID: &absentReady.User.ID, Nickname: &renamed})

	env, arrived := watcher.eventWithin(protocol.EvUserUpdated, quietFor)
	if !arrived {
		t.Fatal("renaming an absent member was announced to nobody")
	}
	updated := userOf(t, env)
	if updated.ID != absentReady.User.ID {
		t.Fatalf("update was for user %d, want %d", updated.ID, absentReady.User.ID)
	}
	if updated.Nickname != renamed {
		t.Fatalf("nickname: got %q, want %q", updated.Nickname, renamed)
	}
	requireOffline(t, updated, "a renamed absent member")
}

func TestGrantingARoleToAHiddenMemberReadsAsAnAbsentOne(t *testing.T) {
	h := newHarness(t, nil)

	admin, _ := h.admin("Ana")

	hidden, hiddenReady := h.member("Bruno", "bruno")
	admin.waitEvent(protocol.EvUserConnected)
	hidden.setStatus("invisible")
	admin.waitEvent(protocol.EvUserDisconnected)

	watcher := h.dial()
	watcher.guest("Carla")

	created := ok[protocol.RoleEvent](admin, protocol.OpRoleCreate, protocol.RoleCreateRequest{Name: "Moderator"})
	granted := ok[protocol.UserEvent](admin, protocol.OpRoleAssign,
		protocol.RoleMembershipRequest{UserID: hiddenReady.User.ID, RoleID: created.Role.ID})

	// Somebody else's doing, on a member every list holds an entry for: it goes
	// out, but as the update an absent member's grant would make. The reply to
	// the moderator who asked is masked the same way, because a role grant must
	// not report back where a hidden member is sitting.
	requireOffline(t, granted.User, "the reply about a hidden member")

	env, arrived := watcher.eventWithin(protocol.EvUserUpdated, quietFor)
	if !arrived {
		t.Fatal("granting a role to a hidden member was announced to nobody")
	}
	updated := userOf(t, env)
	if updated.ID != hiddenReady.User.ID {
		t.Fatalf("update was for user %d, want %d", updated.ID, hiddenReady.User.ID)
	}
	if !slices.Contains(updated.Roles, created.Role.ID) {
		t.Fatalf("roles after the grant: got %v, want one of them to be %d", updated.Roles, created.Role.ID)
	}
	requireOffline(t, updated, "a hidden member given a role")
}

func TestOwnInvisibleStatusSurvivesTheSnapshot(t *testing.T) {
	h := newHarness(t, nil)

	hidden := h.dial()
	ready := hidden.guest("Bruno")
	hidden.setStatus("invisible")
	// Closed before resuming: displacing a live session costs the teardown a
	// long drain, and this test is about the stored status rather than that.
	hidden.conn.Close(websocket.StatusNormalClosure, "")

	// Hiding is from everybody else, never from oneself: a client that could
	// not read its own status back could not show which one is selected.
	resumed := h.dial()
	after := ok[protocol.Ready](resumed, protocol.OpAuthToken,
		protocol.AuthTokenRequest{Token: ready.SessionToken})

	if after.User.Status != "invisible" {
		t.Fatalf("own status: got %q, want invisible", after.User.Status)
	}
}

func TestGoingInvisibleReadsAsADeparture(t *testing.T) {
	h := newHarness(t, nil)

	watcher := h.dial()
	watcher.guest("Ana")

	hidden := h.dial()
	hiddenReady := hidden.guest("Bruno")
	watcher.waitEvent(protocol.EvUserConnected)

	hidden.setStatus("invisible")

	// The change has to reach the watcher as a departure. An update saying
	// "offline" would leave the user in a list that holds only connected
	// people, which is the tell hiding exists to remove.
	env, arrived := watcher.eventWithin(protocol.EvUserDisconnected, quietFor)
	if !arrived {
		t.Fatal("going invisible was not announced as a departure")
	}
	var gone protocol.UserDisconnectedEvent
	if err := json.Unmarshal(env.Data, &gone); err != nil {
		t.Fatalf("decode departure: %v", err)
	}
	if gone.UserID != hiddenReady.User.ID {
		t.Fatalf("departure was for user %d, want %d", gone.UserID, hiddenReady.User.ID)
	}
	if _, leaked := watcher.eventWithin(protocol.EvUserUpdated, quietFor); leaked {
		t.Fatal("an update about the hidden user reached another client")
	}
}

func TestAnAlreadyInvisibleUserMakesNoNoise(t *testing.T) {
	h := newHarness(t, nil)

	watcher := h.dial()
	watcher.guest("Ana")

	hidden := h.dial()
	hidden.guest("Bruno")
	watcher.waitEvent(protocol.EvUserConnected)

	hidden.setStatus("invisible")
	watcher.waitEvent(protocol.EvUserDisconnected)

	// A user the rest of the server believes is offline generates no events at
	// all, so neither may a hidden one: any frame here says they are still here.
	custom := "in a meeting"
	ok[protocol.UserEvent](hidden, protocol.OpUserUpdate,
		protocol.UserUpdateRequest{CustomStatus: &custom})

	if env, leaked := watcher.eventWithin(protocol.EvUserUpdated, quietFor); leaked {
		t.Fatalf("a hidden user's profile change reached another client: %+v", userOf(t, env))
	}
}

func TestComingBackReadsAsAnArrival(t *testing.T) {
	h := newHarness(t, nil)

	watcher := h.dial()
	watcher.guest("Ana")

	hidden := h.dial()
	hiddenReady := hidden.guest("Bruno")
	watcher.waitEvent(protocol.EvUserConnected)

	hidden.setStatus("invisible")
	watcher.waitEvent(protocol.EvUserDisconnected)

	hidden.setStatus("online")

	// The client treats an update and an arrival the same way, so becoming
	// visible again needs no event of its own: it just has to carry the user
	// whole.
	env, arrived := watcher.eventWithin(protocol.EvUserUpdated, quietFor)
	if !arrived {
		t.Fatal("becoming visible again was never announced")
	}
	back := userOf(t, env)
	if back.ID != hiddenReady.User.ID {
		t.Fatalf("update was for user %d, want %d", back.ID, hiddenReady.User.ID)
	}
	if !back.Online || back.Status != "online" {
		t.Fatalf("user came back as online=%v status=%q", back.Online, back.Status)
	}
}

func TestConnectingAndLeavingWhileInvisibleTellsNobody(t *testing.T) {
	h := newHarness(t, nil)

	watcher := h.dial()
	watcher.guest("Ana")

	hidden := h.dial()
	ready := hidden.guest("Bruno")
	watcher.waitEvent(protocol.EvUserConnected)

	hidden.setStatus("invisible")
	watcher.waitEvent(protocol.EvUserDisconnected)

	// Dropping the connection must stay silent: a departure for somebody the
	// watcher already believes is gone is what says they never really were.
	hidden.conn.Close(websocket.StatusNormalClosure, "")
	if _, leaked := watcher.eventWithin(protocol.EvUserDisconnected, quietFor); leaked {
		t.Fatal("a hidden user's disconnect was announced")
	}

	// And coming back is just as quiet: the status is stored, so the new
	// connection is hidden from the first frame.
	returning := h.dial()
	after := ok[protocol.Ready](returning, protocol.OpAuthToken,
		protocol.AuthTokenRequest{Token: ready.SessionToken})
	if after.User.Status != "invisible" {
		t.Fatalf("status did not survive the reconnect: got %q", after.User.Status)
	}
	if _, leaked := watcher.eventWithin(protocol.EvUserConnected, quietFor); leaked {
		t.Fatal("a hidden user's arrival was announced")
	}
}
