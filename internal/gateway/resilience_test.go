package gateway_test

import (
	"sync"
	"testing"

	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/protocol"
)

// Capacity used to be read and then taken as two separate moments, so several
// authentications landing together could each see room and all take it. The
// symptom is a server quietly running over the limit its operator set — which
// on a home connection is a limit about bandwidth, not about taste.
func TestCapacityHoldsUnderConcurrentArrivals(t *testing.T) {
	const limit = 3
	const arrivals = 12

	h := newHarness(t, func(cfg *config.Config) { cfg.Server.MaxUsers = limit })

	// Every client is connected and waiting before any of them authenticates,
	// so the requests land as close together as the harness can arrange.
	clients := make([]*client, arrivals)
	for i := range clients {
		clients[i] = h.dial()
	}

	var (
		mu       sync.Mutex
		admitted int
		refused  int
		start    sync.WaitGroup
		done     sync.WaitGroup
	)
	start.Add(1)
	for i, c := range clients {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()

			reply := c.do(protocol.OpAuthGuest, protocol.AuthGuestRequest{
				Nickname: nicknames[i%len(nicknames)],
			})

			mu.Lock()
			defer mu.Unlock()
			switch {
			case reply.Op == protocol.OpResult:
				admitted++
			case reply.Error != nil && reply.Error.Code == protocol.ErrServerFull:
				refused++
			default:
				t.Errorf("unexpected reply: op=%s error=%v", reply.Op, reply.Error)
			}
		}()
	}
	start.Done()
	done.Wait()

	if admitted > limit {
		t.Errorf("%d identities were admitted to a server limited to %d", admitted, limit)
	}
	if admitted+refused != arrivals {
		t.Errorf("%d admitted plus %d refused is not %d arrivals", admitted, refused, arrivals)
	}
	if admitted == 0 {
		t.Error("nobody got in at all, which is a different bug")
	}
}

// nicknames keeps the concurrent arrivals distinguishable in a failure
// message. They are not otherwise meaningful.
var nicknames = []string{"Ana", "Ben", "Cleo", "Dai", "Eve", "Fen", "Gil", "Hana"}

// awaitResult returns the next reply to any outstanding request, stashing
// events as it goes. It is what reads back a batch of requests that were sent
// without waiting for each other, where the order of the replies is precisely
// the thing not to assume.
func (c *client) awaitResult() protocol.Envelope {
	c.t.Helper()

	for i, env := range c.pending {
		if env.ID != "" {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			return env
		}
	}
	for range 64 {
		env := c.next()
		if env.ID != "" {
			return env
		}
		c.pending = append(c.pending, env)
	}
	c.t.Fatal("no reply arrived")
	return protocol.Envelope{}
}

// Somebody whose connection dropped and who is coming straight back already
// holds a place. Counting them as a new arrival would make a full server
// permanently unreachable to the very people who were just on it — which is
// what happens to everybody at once when a home server restarts.
func TestReconnectingDoesNotNeedAFreePlace(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) { cfg.Server.MaxUsers = 1 })

	first := h.dial()
	ready := first.guest("Alice")
	if ready.SessionToken == "" {
		t.Fatal("no session token to come back with")
	}

	// The server is now full. The same identity must still get in.
	second := h.dial()
	resumed := ok[protocol.Ready](second, protocol.OpAuthToken,
		protocol.AuthTokenRequest{Token: ready.SessionToken})

	if resumed.User.ID != ready.User.ID {
		t.Errorf("came back as user %d, want %d", resumed.User.ID, ready.User.ID)
	}

	// And a genuinely new arrival is still refused.
	third := h.dial()
	third.fails(protocol.OpAuthGuest, protocol.AuthGuestRequest{Nickname: "Bob"}, protocol.ErrServerFull)
}

// History and search are now handled off the read loop, so that a slow one
// cannot stop the connection answering a heartbeat. The risk of that change is
// the reply going astray, so it is worth pinning that an ordinary page still
// arrives, and that the id it is correlated by still matches.
func TestHistoryStillAnswersWhenDispatchedOffTheReadLoop(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	channel := textChannel(t, c.guest("Alice"))

	sent := ok[protocol.MessageEvent](c, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: "hello"})

	page := ok[protocol.MessageHistoryResult](c, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID})

	if len(page.Messages) != 1 {
		t.Fatalf("history holds %d messages, want 1", len(page.Messages))
	}
	if page.Messages[0].ID != sent.Message.ID {
		t.Errorf("history returned message %d, want %d", page.Messages[0].ID, sent.Message.ID)
	}

	// A write sent straight after still takes effect, which is what says the
	// two dispatch paths have not been left racing each other.
	ok[protocol.MessageEvent](c, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: "and again"})

	page = ok[protocol.MessageHistoryResult](c, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID})
	if len(page.Messages) != 2 {
		t.Errorf("history holds %d messages, want 2", len(page.Messages))
	}
}

// Several reads in flight at once is what a client does when it opens a
// channel and searches in the same breath. They must all be answered, and each
// to the request that asked.
func TestConcurrentReadsAreEachAnswered(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	channel := textChannel(t, c.guest("Alice"))

	for range 3 {
		ok[protocol.MessageEvent](c, protocol.OpMessageSend,
			protocol.MessageSendRequest{ChannelID: channel.ID, Content: "something"})
	}

	// Sent without waiting for a reply, so they overlap on the server.
	ids := make([]string, 4)
	for i := range ids {
		ids[i] = c.send(protocol.OpMessageHistory,
			protocol.MessageHistoryRequest{ChannelID: channel.ID, Limit: 3})
	}

	seen := map[string]bool{}
	for range ids {
		reply := c.awaitResult()
		if reply.Error != nil {
			t.Fatalf("a concurrent read failed: %v", reply.Error)
		}
		seen[reply.ID] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("request %s was never answered", id)
		}
	}
}
