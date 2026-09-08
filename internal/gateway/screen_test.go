package gateway_test

import (
	"encoding/json"
	"testing"

	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
)

// The tests below are about the decisions the server makes around a screen
// share rather than about the picture itself: who may start one, how large it
// is allowed to be, who is told, and what happens to all of that when somebody
// leaves. The media plane has its own tests; none of these carry a frame.

func screenOff(cfg *config.Config) {
	cfg.Voice.Mode = protocol.VoiceModeServerHost
	cfg.Voice.Screen.Enabled = false
}

func cappedScreens(cfg *config.Config) {
	cfg.Voice.Mode = protocol.VoiceModeServerHost
	cfg.Voice.Screen.Enabled = true
	cfg.Voice.Screen.Audio = true
	cfg.Voice.Screen.MaxHeight = 720
	cfg.Voice.Screen.MaxFramerate = 30
	cfg.Voice.Screen.MaxBitrate = 1_500_000
}

// peerToPeer is the mode where the ceilings are the operator's opinion about
// somebody else's bandwidth, and are not applied.
func peerToPeerScreens(cfg *config.Config) {
	cfg.Voice.Mode = protocol.VoiceModeClientHost
	cfg.Voice.Screen.Enabled = true
	cfg.Voice.Screen.MaxHeight = 720
	cfg.Voice.Screen.MaxFramerate = 30
	cfg.Voice.Screen.MaxBitrate = 1_500_000
}

// joinVoice puts a client in the voice channel and opens a media session, which
// is what every screen op requires to have happened first.
func joinVoice(t *testing.T, c *client, channelID int64) {
	t.Helper()
	ok[struct{}](c, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID})
	_, sdp := offer(t)
	ok[protocol.VoiceConnectResult](c, protocol.OpVoiceConnect, protocol.VoiceConnectRequest{
		ChannelID: channelID,
		SDP:       sdp,
	})
}

func TestScreenShareNeedsAMediaSession(t *testing.T) {
	h := newHarness(t, serverHosted)
	c := h.dial()
	ready := c.guest("Pablo")
	channelID := voiceChannel(t, ready)

	// Sitting in the channel is not the same as carrying media in it, and a
	// screen rides on the media session rather than on the channel.
	ok[struct{}](c, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID})
	c.fails(protocol.OpVoiceStream, protocol.VoiceStreamRequest{Active: true}, protocol.ErrConflict)
}

func TestScreenShareCanBeSwitchedOff(t *testing.T) {
	h := newHarness(t, screenOff)
	c := h.dial()
	ready := c.guest("Pablo")
	channelID := voiceChannel(t, ready)
	joinVoice(t, c, channelID)

	c.fails(protocol.OpVoiceStream, protocol.VoiceStreamRequest{Active: true}, protocol.ErrStreamDisabled)

	// Stopping is deliberately still reachable: somebody sharing when an
	// administrator switches this off must be able to stop, and a client that
	// is told no when it tries would be stuck sharing.
	ok[protocol.VoiceStreamResult](c, protocol.OpVoiceStream, protocol.VoiceStreamRequest{Active: false})
}

func TestScreenShareNeedsThePermission(t *testing.T) {
	h := newHarness(t, serverHosted)
	admin, ready := h.admin("Admin")

	// Sharing a screen is its own permission rather than part of Speak,
	// because the two are different acts on the same room and cost two orders
	// of magnitude apart. Taking it away must leave talking alone.
	stripped := permissions.DefaultEveryone &^ permissions.Stream
	ok[protocol.RoleEvent](admin, protocol.OpRoleUpdate, protocol.RoleUpdateRequest{
		RoleID:      everyoneRole(t, ready),
		Permissions: ptr(stripped.String()),
	})

	c := h.dial()
	c.guest("Pablo")
	channelID := voiceChannel(t, ready)
	joinVoice(t, c, channelID)

	c.fails(protocol.OpVoiceStream, protocol.VoiceStreamRequest{Active: true}, protocol.ErrForbidden)
}

func TestServerHostedScreenSharesAreCapped(t *testing.T) {
	h := newHarness(t, cappedScreens)
	c := h.dial()
	ready := c.guest("Pablo")
	channelID := voiceChannel(t, ready)
	joinVoice(t, c, channelID)

	result := ok[protocol.VoiceStreamResult](c, protocol.OpVoiceStream, protocol.VoiceStreamRequest{
		Active:  true,
		Quality: &protocol.VideoQuality{Height: 2160, Framerate: 60, Bitrate: 20_000_000},
	})
	if result.Quality.Height != 720 || result.Quality.Framerate != 30 {
		t.Fatalf("quality was not held to the ceiling: %+v", result.Quality)
	}
	if result.Quality.Bitrate != 1_500_000 {
		t.Fatalf("bitrate was not held to the ceiling: %d", result.Quality.Bitrate)
	}

	// Asking for less than the ceiling is a choice and is honoured: the
	// operator sets a maximum, not a target.
	result = ok[protocol.VoiceStreamResult](c, protocol.OpVoiceStream, protocol.VoiceStreamRequest{
		Active:  true,
		Quality: &protocol.VideoQuality{Height: 480, Framerate: 15, Bitrate: 600_000},
	})
	if result.Quality.Height != 480 || result.Quality.Framerate != 15 || result.Quality.Bitrate != 600_000 {
		t.Fatalf("a modest request was not honoured: %+v", result.Quality)
	}
}

func TestPeerToPeerScreenSharesAreNotCapped(t *testing.T) {
	h := newHarness(t, peerToPeerScreens)
	c := h.dial()
	ready := c.guest("Pablo")
	channelID := voiceChannel(t, ready)

	ok[struct{}](c, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID})
	connect := ok[protocol.VoiceConnectResult](c, protocol.OpVoiceConnect,
		protocol.VoiceConnectRequest{ChannelID: channelID})

	// The server says so itself rather than leaving the client to work it out
	// from the mode, which is what keeps the two ends from disagreeing.
	if connect.Voice.Screen.Enforced {
		t.Fatal("the ceilings must not be enforced when the server relays nothing")
	}

	result := ok[protocol.VoiceStreamResult](c, protocol.OpVoiceStream, protocol.VoiceStreamRequest{
		Active:  true,
		Quality: &protocol.VideoQuality{Height: 1440, Framerate: 60, Bitrate: 8_000_000},
	})
	if result.Quality.Height != 1440 || result.Quality.Framerate != 60 || result.Quality.Bitrate != 8_000_000 {
		t.Fatalf("a peer-to-peer share was capped: %+v", result.Quality)
	}
}

func TestScreenShareIsAnnouncedAndShowsInTheVoiceState(t *testing.T) {
	h := newHarness(t, serverHosted)
	alice, bob := h.dial(), h.dial()
	ready := alice.guest("Alice")
	bob.guest("Bob")
	channelID := voiceChannel(t, ready)

	joinVoice(t, alice, channelID)
	joinVoice(t, bob, channelID)

	ok[protocol.VoiceStreamResult](alice, protocol.OpVoiceStream, protocol.VoiceStreamRequest{
		Active:  true,
		Quality: &protocol.VideoQuality{Height: 1080, Framerate: 30, Bitrate: 2_500_000},
		Source:  protocol.ScreenSourceWindow,
	})

	var announced protocol.VoiceStreamEvent
	decode(t, bob.waitEvent(protocol.EvVoiceStream).Data, &announced)
	if !announced.Active || announced.UserID != ready.User.ID {
		t.Fatalf("bob was told the wrong thing: %+v", announced)
	}
	if announced.Source != protocol.ScreenSourceWindow {
		t.Fatalf("the source did not travel: %q", announced.Source)
	}

	// The badge beside a name comes from the voice state, so that somebody who
	// arrives after a share started still sees it.
	for range 8 {
		var state protocol.VoiceStateEvent
		decode(t, bob.waitEvent(protocol.EvVoiceState).Data, &state)
		if state.State.UserID == ready.User.ID && state.State.Streaming {
			return
		}
	}
	t.Fatal("the voice state never reported a live screen share")
}

func TestWatchingNeedsALiveStream(t *testing.T) {
	h := newHarness(t, serverHosted)
	alice, bob := h.dial(), h.dial()
	ready := alice.guest("Alice")
	bobReady := bob.guest("Bob")
	channelID := voiceChannel(t, ready)

	joinVoice(t, alice, channelID)
	joinVoice(t, bob, channelID)

	// Nobody is sharing yet.
	bob.fails(protocol.OpVoiceWatch,
		protocol.VoiceWatchRequest{UserID: ready.User.ID, Watching: true}, protocol.ErrNotFound)

	// Nor is watching yourself a thing that means anything.
	bob.fails(protocol.OpVoiceWatch,
		protocol.VoiceWatchRequest{UserID: bobReady.User.ID, Watching: true}, protocol.ErrBadRequest)
}

func TestWatchingIsAnnouncedAndEndsWithTheStream(t *testing.T) {
	h := newHarness(t, serverHosted)
	alice, bob := h.dial(), h.dial()
	ready := alice.guest("Alice")
	bob.guest("Bob")
	channelID := voiceChannel(t, ready)

	joinVoice(t, alice, channelID)
	joinVoice(t, bob, channelID)
	ok[protocol.VoiceStreamResult](alice, protocol.OpVoiceStream, protocol.VoiceStreamRequest{Active: true})

	ok[struct{}](bob, protocol.OpVoiceWatch,
		protocol.VoiceWatchRequest{UserID: ready.User.ID, Watching: true})

	// The whole channel is told, because two different readers need it: the
	// host of a client-hosted channel, and everybody drawing a viewer count.
	var watch protocol.VoiceWatchEvent
	decode(t, alice.waitEvent(protocol.EvVoiceWatch).Data, &watch)
	if !watch.Watching || watch.PublisherID != ready.User.ID {
		t.Fatalf("the wrong thing was announced: %+v", watch)
	}

	// Ending the share ends everybody's subscription to it. Leaving the intent
	// behind would have a share that started again reach people who stopped
	// looking several minutes ago.
	ok[protocol.VoiceStreamResult](alice, protocol.OpVoiceStream, protocol.VoiceStreamRequest{Active: false})

	for range 8 {
		var event protocol.VoiceWatchEvent
		decode(t, bob.waitEvent(protocol.EvVoiceWatch).Data, &event)
		if !event.Watching && event.PublisherID == ready.User.ID {
			return
		}
	}
	t.Fatal("watching was never ended along with the stream")
}

func TestLeavingVoiceEndsTheScreenShare(t *testing.T) {
	h := newHarness(t, serverHosted)
	alice, bob := h.dial(), h.dial()
	ready := alice.guest("Alice")
	bob.guest("Bob")
	channelID := voiceChannel(t, ready)

	joinVoice(t, alice, channelID)
	joinVoice(t, bob, channelID)
	ok[protocol.VoiceStreamResult](alice, protocol.OpVoiceStream, protocol.VoiceStreamRequest{Active: true})

	ok[struct{}](alice, protocol.OpVoiceLeave, struct{}{})

	for range 8 {
		var event protocol.VoiceStreamEvent
		decode(t, bob.waitEvent(protocol.EvVoiceStream).Data, &event)
		if !event.Active && event.UserID == ready.User.ID {
			return
		}
	}
	t.Fatal("the share outlived the media session that carried it")
}

func TestOneScreenPerChannelWhenTheOperatorSaysSo(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		serverHosted(cfg)
		cfg.Voice.Screen.MaxStreams = 1
	})
	alice, bob := h.dial(), h.dial()
	ready := alice.guest("Alice")
	bob.guest("Bob")
	channelID := voiceChannel(t, ready)

	joinVoice(t, alice, channelID)
	joinVoice(t, bob, channelID)

	ok[protocol.VoiceStreamResult](alice, protocol.OpVoiceStream, protocol.VoiceStreamRequest{Active: true})
	bob.fails(protocol.OpVoiceStream, protocol.VoiceStreamRequest{Active: true}, protocol.ErrConflict)

	// Alice changing her own quality is not a second stream, and must not be
	// refused by the limit she is already inside.
	ok[protocol.VoiceStreamResult](alice, protocol.OpVoiceStream, protocol.VoiceStreamRequest{
		Active:  true,
		Quality: &protocol.VideoQuality{Height: 720, Framerate: 30},
	})
}

func decode(t *testing.T, raw json.RawMessage, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode event: %v", err)
	}
}
