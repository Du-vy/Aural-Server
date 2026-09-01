package gateway_test

import (
	"context"
	"encoding/json"
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

func TestInvisibleUserIsAbsentFromTheSnapshot(t *testing.T) {
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
			t.Fatalf("an invisible user was listed in the snapshot as %+v", u)
		}
	}
	// Listing only connected users is the whole point: an entry for somebody
	// who looks offline could only ever be somebody hiding.
	if len(ready.Users) != 2 {
		t.Fatalf("snapshot users: got %d, want 2 (the visible guest and the arrival)", len(ready.Users))
	}
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
