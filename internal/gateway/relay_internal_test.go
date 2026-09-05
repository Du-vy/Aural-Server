package gateway

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/discord"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// The loop guard is the one piece of this feature that has to be exactly right.
// Everything else degrades — a picture that does not load, a file that did not
// cross — but a relay that echoes itself fills two servers with the same
// message until somebody notices, and it does it fastest on the busiest
// channel.
//
// So both halves are pinned here, at the level they actually decide anything.

func relayForTest() *discordRelay {
	r := &discordRelay{
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		byChannel:       map[int64]store.RelayLink{},
		byDiscord:       map[string]store.RelayLink{},
		ownWebhooks:     map[string]struct{}{},
		inboundWebhooks: map[int64]struct{}{},
		queues:          map[int64]*relayQueue{},
		stopped:         make(chan struct{}),
	}
	inbound := int64(7)
	link := store.RelayLink{
		ID: 1, ChannelID: 42,
		DiscordChannelID: "900", WebhookID: "800", WebhookToken: "tok",
		Direction: store.RelayBoth, Enabled: true,
		SourceWebhookID: &inbound,
	}
	r.byChannel[link.ChannelID] = link
	r.byDiscord[link.DiscordChannelID] = link
	r.ownWebhooks[link.WebhookID] = struct{}{}
	r.inboundWebhooks[inbound] = struct{}{}
	return r
}

func TestOwnEchoIsRecognisedByTheWebhookItWasPostedThrough(t *testing.T) {
	r := relayForTest()

	// The message this server posted, coming back over the gateway. This is
	// the frame that would loop.
	if !r.isOwnEcho(discord.Message{ChannelID: "900", WebhookID: "800"}) {
		t.Fatal("a message posted through our own webhook was not recognised as an echo")
	}
	// Somebody else's webhook in the same channel is not ours: a GitHub
	// integration posting into a bridged channel should still cross.
	if r.isOwnEcho(discord.Message{ChannelID: "900", WebhookID: "801"}) {
		t.Fatal("another service's webhook was mistaken for ours")
	}
	// A person typing has no webhook id at all.
	if r.isOwnEcho(discord.Message{ChannelID: "900"}) {
		t.Fatal("a message somebody typed was mistaken for an echo")
	}
}

func TestOwnEchoSurvivesALinkBeingSwitchedOff(t *testing.T) {
	r := relayForTest()

	// A link that has been disabled stops relaying, but a message already in
	// flight through it still has to be recognised as ours — otherwise
	// switching a bridge off at the wrong moment is what starts the loop.
	link := r.byDiscord["900"]
	link.Enabled = false
	r.byDiscord["900"] = link
	r.byChannel[42] = link

	if !r.isOwnEcho(discord.Message{ChannelID: "900", WebhookID: "800"}) {
		t.Fatal("disabling a link stopped its echoes being recognised")
	}
}

func TestInboundMessagesAreNotSentBackToDiscord(t *testing.T) {
	r := relayForTest()

	relayed := int64(7)  // the webhooks row this relay writes under
	ordinary := int64(9) // somebody's own webhook in the same channel

	if !r.isInboundMessage(store.Message{ChannelID: 42, WebhookID: &relayed}) {
		t.Fatal("a message this relay wrote was not recognised as its own")
	}
	if r.isInboundMessage(store.Message{ChannelID: 42, WebhookID: &ordinary}) {
		t.Fatal("an unrelated webhook message was mistaken for a relayed one")
	}
	if r.isInboundMessage(store.Message{ChannelID: 42}) {
		t.Fatal("a message somebody typed was mistaken for a relayed one")
	}
}

func TestOutboundLinkRefusesWhatMustNotCross(t *testing.T) {
	r := relayForTest()
	postID := int64(3)
	relayed := int64(7)

	cases := []struct {
		name string
		in   store.Message
	}{
		{"a message this relay wrote", store.Message{ChannelID: 42, WebhookID: &relayed}},
		{"a comment on a post", store.Message{ChannelID: 42, PostID: &postID}},
		{"a message in a channel nobody bridged", store.Message{ChannelID: 99}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := r.outboundLink(tc.in); ok {
				t.Fatal("this message was accepted for relaying")
			}
		})
	}
}

func TestDirectionDecidesWhichWayAMessageMoves(t *testing.T) {
	cases := []struct {
		direction              string
		enabled                bool
		wantDiscord, wantAural bool
	}{
		{store.RelayBoth, true, true, true},
		{store.RelayToDiscord, true, true, false},
		{store.RelayToAural, true, false, true},
		{store.RelayBoth, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.direction, func(t *testing.T) {
			link := store.RelayLink{Direction: tc.direction, Enabled: tc.enabled}
			if got := link.ToDiscord(); got != tc.wantDiscord {
				t.Fatalf("ToDiscord: got %v, want %v", got, tc.wantDiscord)
			}
			if got := link.ToAural(); got != tc.wantAural {
				t.Fatalf("ToAural: got %v, want %v", got, tc.wantAural)
			}
		})
	}
}

func TestOutboundNameNudgesTheNamesDiscordRefuses(t *testing.T) {
	r := relayForTest()

	cases := []struct {
		name   string
		author string
	}{
		{"a name containing discord", "Discordia"},
		{"the reserved everyone", "everyone"},
		{"the reserved here", "here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.outboundName(store.Message{Author: tc.author})
			if got == tc.author {
				t.Fatalf("%q was passed through unchanged, Discord would refuse it", tc.author)
			}
			// The name still has to read as itself: this breaks the token, it
			// does not rename anybody.
			if len([]rune(got)) != len([]rune(tc.author))+1 {
				t.Fatalf("outboundName(%q) = %q, expected exactly one character inserted",
					tc.author, got)
			}
		})
	}

	// An ordinary name is left completely alone.
	if got := r.outboundName(store.Message{Author: "Pablo"}); got != "Pablo" {
		t.Fatalf("an ordinary name was altered: %q", got)
	}
	// A message with no author at all still has to post.
	if got := r.outboundName(store.Message{}); got == "" {
		t.Fatal("a message with no author produced an empty username")
	}
}

func TestPublicBaseURLPrefersWhatTheOperatorActuallySaid(t *testing.T) {
	// Discord fetches a relayed avatar from its own network, so this address
	// has to be the one the outside world can reach. Guessing it wrong costs
	// only the pictures, which is why an empty answer is a valid one.
	cases := []struct {
		name     string
		tune     func(*config.Config)
		publicIP string
		want     string
	}{
		{"an explicit address wins over everything", func(c *config.Config) {
			c.Relay.PublicURL = "https://aural.example.com"
			c.TLS.Enabled = true
			c.TLS.ACME.Enabled = true
			c.TLS.ACME.Domains = []string{"ignored.example.com"}
		}, "", "https://aural.example.com"},

		{"then the name on the certificate", func(c *config.Config) {
			c.TLS.Enabled = true
			c.TLS.ACME.Enabled = true
			c.TLS.ACME.Domains = []string{"chat.example.com"}
			c.Server.Port = 443
		}, "", "https://chat.example.com"},

		{"then the dynamic DNS record", func(c *config.Config) {
			c.DDNS.Enabled = true
			c.DDNS.Domain = "home.duckdns.org"
			c.Server.Port = 9871
		}, "", "http://home.duckdns.org:9871"},

		{"then whatever address the server resolved for itself", func(c *config.Config) {
			c.Server.Port = 9871
		}, "203.0.113.7", "http://203.0.113.7:9871"},

		{"and nothing at all when there is nothing to guess from", func(c *config.Config) {
			c.Server.Port = 9871
		}, "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			tc.tune(&cfg)

			h := &Hub{cfg: &cfg}
			h.storePublicIP(tc.publicIP)

			if got := h.publicBaseURL(); got != tc.want {
				t.Fatalf("publicBaseURL: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRelayCacheAndQueuesSurviveConcurrentUse(t *testing.T) {
	// The relay's shared state is reached from three directions at once: the
	// Discord read loop asking whether a message is an echo, an administrator's
	// request swapping the whole link set, and a per-link worker draining
	// deliveries. Nothing else in the test suite drives them together, so this
	// is what `go test -race` actually has to look at.
	r := relayForTest()
	defer close(r.stopped)

	const rounds = 200
	var wg sync.WaitGroup

	// An administrator editing links while everything else runs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range rounds {
			inbound := int64(7)
			r.setLinks([]store.RelayLink{{
				ID: 1, ChannelID: 42,
				DiscordChannelID: "900",
				WebhookID:        "800",
				Direction:        store.RelayBoth,
				Enabled:          i%2 == 0,
				SourceWebhookID:  &inbound,
			}})
		}
	}()

	// The read loop deciding what to drop.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			relayed := int64(7)
			for range rounds {
				r.isOwnEcho(discord.Message{ChannelID: "900", WebhookID: "800"})
				r.isInboundMessage(store.Message{ChannelID: 42, WebhookID: &relayed})
				r.linkForChannel(42)
				r.linkForDiscord("900")
			}
		}()
	}

	// Deliveries queueing up behind each other.
	var delivered atomic.Int64
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				r.enqueue(1, func(context.Context) { delivered.Add(1) })
			}
		}()
	}

	wg.Wait()

	// The queue drops rather than blocks when it is full, which is the whole
	// point of it, so the count is not fixed. What has to hold is that the
	// workers ran at all and that nothing deadlocked getting here.
	deadline := time.Now().Add(2 * time.Second)
	for delivered.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if delivered.Load() == 0 {
		t.Fatal("no queued delivery ever ran")
	}
}

// TestRetiredLinksReleaseTheirWorkers covers what happens to a link's delivery
// queue when the link itself goes.
//
// A queue is built on the first delivery and used to be kept for the life of
// the process, worker and buffer alike. Link ids are never reused, so every
// bridge an administrator removed left one behind — a slow leak, but one with
// no ceiling on a server whose owner keeps repointing it.
//
// What must not happen in fixing that is losing a message somebody already
// sent, so a retired queue delivers what it is holding before it goes.
func TestRetiredLinksReleaseTheirWorkers(t *testing.T) {
	r := relayForTest()
	defer close(r.stopped)

	// Block the worker on its first job so the rest queue up behind it, which
	// is what makes "delivers what it is holding" a real question rather than
	// a race the test happens to win.
	release := make(chan struct{})
	var delivered atomic.Int64
	r.enqueue(1, func(context.Context) { <-release })
	for range 5 {
		r.enqueue(1, func(context.Context) { delivered.Add(1) })
	}

	r.mu.Lock()
	_, held := r.queues[1]
	r.mu.Unlock()
	if !held {
		t.Fatal("enqueueing did not build a queue for the link")
	}

	// The link is gone from the table. Everything else stays.
	r.setLinks(nil)

	r.mu.Lock()
	_, stillHeld := r.queues[1]
	r.mu.Unlock()
	if stillHeld {
		t.Error("a link that no longer exists kept its queue")
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for delivered.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := delivered.Load(); got != 5 {
		t.Errorf("delivered %d of the 5 messages already queued", got)
	}

	// A delivery for a link that came back gets a fresh queue rather than the
	// retired one, whose worker has gone.
	r.enqueue(1, func(context.Context) { delivered.Add(1) })
	deadline = time.Now().Add(2 * time.Second)
	for delivered.Load() < 6 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := delivered.Load(); got != 6 {
		t.Errorf("a queue rebuilt after retirement did not run: delivered %d", got)
	}
}

// TestSanitiseEmbedsKeepsTheKindDiscordUnfurled checks that what Discord made
// of a link survives into what a client is told.
//
// The kind is the whole of the difference between a picture and a card with a
// picture in the corner of it, and between a video that plays here and a still
// that does not. A sender's own word for it is not taken beyond the three
// kinds a client draws differently.
func TestSanitiseEmbedsKeepsTheKindDiscordUnfurled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"a bare picture", "image", "image"},
		{"a looping clip", "gifv", "gifv"},
		{"a video page", "video", "video"},
		{"a card an application composed", "rich", "rich"},
		{"an article", "article", "rich"},
		{"a kind nobody has heard of", "interstellar", "rich"},
		{"none given at all", "", "rich"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := sanitiseEmbeds([]protocol.Embed{{
				Type:      tc.in,
				URL:       "https://example.com/a",
				Thumbnail: &protocol.EmbedMedia{URL: "https://example.com/a.jpg"},
			}})
			if len(out) != 1 {
				t.Fatalf("embeds: got %d, want 1", len(out))
			}
			if out[0].Type != tc.want {
				t.Fatalf("type: got %q, want %q", out[0].Type, tc.want)
			}
		})
	}
}
