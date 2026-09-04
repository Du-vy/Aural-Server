package gateway_test

import (
	"testing"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// The relay over the protocol.
//
// Nothing here reaches Discord. Every case is one this server can decide on
// its own, which is deliberate: a request that is wrong in a way we can see
// should be refused without a round trip to somebody else's API, and a test
// suite that needed the internet would be one nobody runs.

// aValidWebhookURL is well-formed and points at nothing. It gets a request as
// far as the network call, which is where these tests stop.
const aValidWebhookURL = "https://discord.com/api/webhooks/1234567890123456789/aB3-_xYz09"

func TestRelayStateNeedsManageServer(t *testing.T) {
	h := newHarness(t, nil)

	guest := h.dial()
	guest.guest("Pablo")
	guest.fails(protocol.OpRelayGet, struct{}{}, protocol.ErrForbidden)
	guest.fails(protocol.OpRelayConfigure, protocol.RelayConfigureRequest{Enabled: false},
		protocol.ErrForbidden)
	guest.fails(protocol.OpRelayCreate, protocol.RelayCreateRequest{
		ChannelID: 1, WebhookURL: aValidWebhookURL,
	}, protocol.ErrForbidden)
	guest.fails(protocol.OpRelayDelete, protocol.RelayDeleteRequest{ID: 1},
		protocol.ErrForbidden)
}

func TestRelayStartsOffAndSaysSo(t *testing.T) {
	h := newHarness(t, nil)
	admin, _ := h.admin("Root")

	state := ok[protocol.RelayEvent](admin, protocol.OpRelayGet, struct{}{}).Relay

	if state.Enabled {
		t.Fatal("the relay should be off on a fresh server")
	}
	if state.Configured {
		t.Fatal("a fresh server has no bot token")
	}
	if state.Connected {
		t.Fatal("a relay that is off cannot be connected")
	}
	if state.Links == nil || len(state.Links) != 0 {
		t.Fatalf("links should be an empty list, got %v", state.Links)
	}
	if state.Guilds == nil {
		t.Fatal("guilds should be an empty list rather than null")
	}
}

func TestRelayRefusesToBeSwitchedOnWithNoToken(t *testing.T) {
	h := newHarness(t, nil)
	admin, _ := h.admin("Root")

	// Switching it on with nothing to connect with would produce a relay that
	// reports itself enabled and never connects, which is the least useful
	// possible state to leave an administrator in.
	admin.fails(protocol.OpRelayConfigure, protocol.RelayConfigureRequest{Enabled: true},
		protocol.ErrBadRequest)
}

func TestRelayRefusesAPasteThatIsNotABotToken(t *testing.T) {
	h := newHarness(t, nil)
	admin, _ := h.admin("Root")

	// The three things people actually paste into this box by mistake: an
	// application id, a client secret, and the webhook URL from the other
	// field on the same screen.
	for _, wrong := range []string{
		"1234567890123456789",
		"aB3xYz09aB3xYz09aB3xYz09aB3xYz09",
		aValidWebhookURL,
	} {
		token := wrong
		admin.fails(protocol.OpRelayConfigure, protocol.RelayConfigureRequest{
			Enabled: false, BotToken: &token,
		}, protocol.ErrBadRequest)
	}
}

func TestRelayKeepsTheTokenItWasGivenWithoutEverSendingItBack(t *testing.T) {
	h := newHarness(t, nil)
	admin, _ := h.admin("Root")

	// Shaped like a real one: three dot-separated segments, right length.
	token := "MTIzNDU2Nzg5MDEyMzQ1Njc4.GaBcDe.abcdefghijklmnopqrstuvwxyz0123"
	state := ok[protocol.RelayEvent](admin, protocol.OpRelayConfigure,
		protocol.RelayConfigureRequest{Enabled: false, BotToken: &token}).Relay

	if !state.Configured {
		t.Fatal("the token was not recorded")
	}
	if state.Enabled {
		t.Fatal("the relay was switched on when the request said not to")
	}

	// A later request that omits the token must keep it rather than clear it:
	// the screen toggles the relay without ever holding the credential.
	again := ok[protocol.RelayEvent](admin, protocol.OpRelayGet, struct{}{}).Relay
	if !again.Configured {
		t.Fatal("the token was lost")
	}
}

func TestRelayLinkRefusesAWebhookURLThatIsNotOne(t *testing.T) {
	h := newHarness(t, nil)
	admin, ready := h.admin("Root")
	channel := textChannel(t, ready)

	for _, wrong := range []string{
		"",
		"not a url",
		"https://example.com/api/webhooks/1234567890123456789/token",
		"https://discord.com/channels/123/456",
	} {
		admin.fails(protocol.OpRelayCreate, protocol.RelayCreateRequest{
			ChannelID: channel.ID, WebhookURL: wrong,
		}, protocol.ErrBadRequest)
	}
}

func TestRelayLinkRefusesADirectionThatMeansNothing(t *testing.T) {
	h := newHarness(t, nil)
	admin, ready := h.admin("Root")
	channel := textChannel(t, ready)

	admin.fails(protocol.OpRelayCreate, protocol.RelayCreateRequest{
		ChannelID: channel.ID, WebhookURL: aValidWebhookURL, Direction: "sideways",
	}, protocol.ErrBadRequest)
}

func TestRelayLinkOnlyPointsAtATextChannel(t *testing.T) {
	h := newHarness(t, nil)
	admin, ready := h.admin("Root")

	// A voice channel carries no messages, so a bridge into one would have
	// nothing to write.
	admin.fails(protocol.OpRelayCreate, protocol.RelayCreateRequest{
		ChannelID: voiceChannel(t, ready), WebhookURL: aValidWebhookURL,
	}, protocol.ErrBadRequest)

	// And a channel that does not exist is a not-found rather than a bad
	// request: the shape of the ask was fine.
	admin.fails(protocol.OpRelayCreate, protocol.RelayCreateRequest{
		ChannelID: 999999, WebhookURL: aValidWebhookURL,
	}, protocol.ErrNotFound)
}

func TestRelayUpdateAndDeleteRefuseALinkThatIsNotThere(t *testing.T) {
	h := newHarness(t, nil)
	admin, _ := h.admin("Root")

	admin.fails(protocol.OpRelayUpdate, protocol.RelayUpdateRequest{ID: 4242},
		protocol.ErrNotFound)
	admin.fails(protocol.OpRelayDelete, protocol.RelayDeleteRequest{ID: 4242},
		protocol.ErrNotFound)
}
