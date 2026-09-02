package gateway_test

import (
	"encoding/json"
	"testing"

	"github.com/pion/webrtc/v4"

	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/protocol"
)

// voiceChannel is the voice channel the seed installs.
func voiceChannel(t *testing.T, ready protocol.Ready) int64 {
	t.Helper()
	for _, ch := range ready.Channels {
		if ch.Type == protocol.ChannelVoice {
			return ch.ID
		}
	}
	t.Fatal("the seed has no voice channel")
	return 0
}

// offer builds a real WebRTC offer with one Opus track on it, so the relay is
// exercised against something a browser could actually have sent rather than
// against a string this test made up.
func offer(t *testing.T) (*webrtc.PeerConnection, string) {
	t.Helper()

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("build a peer connection: %v", err)
	}
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "test",
	)
	if err != nil {
		t.Fatalf("build a track: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("add the track: %v", err)
	}
	sdp, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create an offer: %v", err)
	}
	if err := pc.SetLocalDescription(sdp); err != nil {
		t.Fatalf("set the offer: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc, sdp.SDP
}

func serverHosted(cfg *config.Config)   { cfg.Voice.Mode = protocol.VoiceModeServerHost }
func clientHosted(cfg *config.Config)   { cfg.Voice.Mode = protocol.VoiceModeClientHost }
func voiceSwitchedOff(c *config.Config) { c.Voice.Enabled = false }

func TestVoiceNeedsTheChannelFirst(t *testing.T) {
	h := newHarness(t, clientHosted)
	c := h.dial()
	ready := c.guest("Pablo")
	channelID := voiceChannel(t, ready)

	// Sitting in no channel at all.
	c.fails(protocol.OpVoiceConnect, protocol.VoiceConnectRequest{ChannelID: channelID}, protocol.ErrBadRequest)

	ok[struct{}](c, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID})

	// Sitting in the right channel but naming the wrong one.
	other := channelID + 1000
	c.fails(protocol.OpVoiceConnect, protocol.VoiceConnectRequest{ChannelID: other}, protocol.ErrConflict)
}

func TestVoiceCanBeSwitchedOff(t *testing.T) {
	h := newHarness(t, voiceSwitchedOff)
	c := h.dial()
	ready := c.guest("Pablo")
	channelID := voiceChannel(t, ready)

	if ready.Server.Voice.Enabled {
		t.Fatal("a server with voice off must not advertise it")
	}
	ok[struct{}](c, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID})
	c.fails(protocol.OpVoiceConnect, protocol.VoiceConnectRequest{ChannelID: channelID}, protocol.ErrVoiceDisabled)
}

func TestServerHostedSessionAnswersAnOffer(t *testing.T) {
	h := newHarness(t, serverHosted)
	c := h.dial()
	ready := c.guest("Pablo")
	channelID := voiceChannel(t, ready)

	ok[struct{}](c, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID})

	// A server-hosted session without an offer is refused rather than half-built.
	c.fails(protocol.OpVoiceConnect, protocol.VoiceConnectRequest{ChannelID: channelID}, protocol.ErrBadRequest)

	caller, sdp := offer(t)

	result := ok[protocol.VoiceConnectResult](c, protocol.OpVoiceConnect,
		protocol.VoiceConnectRequest{ChannelID: channelID, SDP: sdp})

	if result.Mode != protocol.VoiceModeServerHost {
		t.Fatalf("mode: got %q", result.Mode)
	}
	if result.SDP == "" {
		t.Fatal("a server-hosted session must be answered")
	}
	if result.HostUserID != nil {
		t.Fatal("a server-hosted session has no client host")
	}
	if result.Voice.SampleRate != config.DefaultVoiceSampleRate {
		t.Fatalf("sample rate: got %d", result.Voice.SampleRate)
	}
	if len(result.Participants) != 1 || result.Participants[0].UserID != ready.User.ID {
		t.Fatalf("participants: got %+v", result.Participants)
	}
	if !result.Participants[0].Connected {
		t.Fatal("the caller should be listed as connected")
	}

	// The answer has to be one the peer that offered actually accepts, which
	// is the whole of what makes this an end-to-end check rather than a check
	// that the relay returned a non-empty string.
	if err := caller.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  result.SDP,
	}); err != nil {
		t.Fatalf("the relay produced an answer the offering peer rejects: %v", err)
	}
}

func TestServerHostedSignallingRejectsAPeerTarget(t *testing.T) {
	h := newHarness(t, serverHosted)
	c := h.dial()
	ready := c.guest("Pablo")
	channelID := voiceChannel(t, ready)
	ok[struct{}](c, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID})

	_, sdp := offer(t)
	ok[protocol.VoiceConnectResult](c, protocol.OpVoiceConnect,
		protocol.VoiceConnectRequest{ChannelID: channelID, SDP: sdp})

	c.fails(protocol.OpVoiceSignal, protocol.VoiceSignalRequest{
		TargetID: ready.User.ID + 1,
		Kind:     protocol.SignalCandidate,
	}, protocol.ErrBadRequest)
}

func TestClientHostElectsTheFirstArrival(t *testing.T) {
	h := newHarness(t, clientHosted)

	first := h.dial()
	firstReady := first.guest("Pablo")
	channelID := voiceChannel(t, firstReady)
	ok[struct{}](first, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID})

	elected := ok[protocol.VoiceConnectResult](first, protocol.OpVoiceConnect,
		protocol.VoiceConnectRequest{ChannelID: channelID})
	if elected.HostUserID == nil || *elected.HostUserID != firstReady.User.ID {
		t.Fatalf("the first arrival should host: got %+v", elected.HostUserID)
	}
	if elected.SDP != "" {
		t.Fatal("a client-hosted session is not answered by the server")
	}

	second := h.dial()
	secondReady := second.guest("Ana")
	ok[struct{}](second, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID})
	joined := ok[protocol.VoiceConnectResult](second, protocol.OpVoiceConnect,
		protocol.VoiceConnectRequest{ChannelID: channelID})

	if joined.HostUserID == nil || *joined.HostUserID != firstReady.User.ID {
		t.Fatalf("the second arrival should be pointed at the first: got %+v", joined.HostUserID)
	}
	if joined.HostEpoch != elected.HostEpoch {
		t.Fatalf("no election happened, so the epoch should not have moved: %d then %d",
			elected.HostEpoch, joined.HostEpoch)
	}
	if len(joined.Participants) != 2 {
		t.Fatalf("participants: got %d, want 2", len(joined.Participants))
	}

	// The host is told to dial the arrival, and nobody else is.
	var peer protocol.VoicePeerEvent
	decodeEvent(t, first.waitEvent(protocol.EvVoicePeer), &peer)
	if peer.UserID != secondReady.User.ID || peer.Action != protocol.PeerAdd {
		t.Fatalf("peer event: got %+v", peer)
	}
}

func TestClientHostHandsOverWhenTheHostLeaves(t *testing.T) {
	h := newHarness(t, clientHosted)

	host := h.dial()
	hostReady := host.guest("Pablo")
	channelID := voiceChannel(t, hostReady)
	ok[struct{}](host, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID})
	first := ok[protocol.VoiceConnectResult](host, protocol.OpVoiceConnect,
		protocol.VoiceConnectRequest{ChannelID: channelID})

	guest := h.dial()
	guestReady := guest.guest("Ana")
	ok[struct{}](guest, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID})
	ok[protocol.VoiceConnectResult](guest, protocol.OpVoiceConnect,
		protocol.VoiceConnectRequest{ChannelID: channelID})

	// The host leaves the channel, which empties the room.
	ok[struct{}](host, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: nil})

	var reset protocol.VoiceResetEvent
	decodeEvent(t, guest.waitEvent(protocol.EvVoiceReset), &reset)
	if reset.ChannelID != channelID || reset.Reason != protocol.ResetHostChanged {
		t.Fatalf("reset: got %+v", reset)
	}

	// Opening a new session is the whole of the recovery, and it elects the
	// only person left.
	again := ok[protocol.VoiceConnectResult](guest, protocol.OpVoiceConnect,
		protocol.VoiceConnectRequest{ChannelID: channelID})
	if again.HostUserID == nil || *again.HostUserID != guestReady.User.ID {
		t.Fatalf("the remaining participant should host: got %+v", again.HostUserID)
	}
	if again.HostEpoch <= first.HostEpoch {
		t.Fatalf("an election must move the epoch: %d then %d", first.HostEpoch, again.HostEpoch)
	}
}

func TestLeavingTheChannelClosesTheMediaSession(t *testing.T) {
	h := newHarness(t, clientHosted)
	c := h.dial()
	ready := c.guest("Pablo")
	channelID := voiceChannel(t, ready)

	ok[struct{}](c, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID})
	ok[protocol.VoiceConnectResult](c, protocol.OpVoiceConnect,
		protocol.VoiceConnectRequest{ChannelID: channelID})
	ok[struct{}](c, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: nil})

	// Signalling into a session that is gone is refused rather than relayed.
	c.fails(protocol.OpVoiceSignal, protocol.VoiceSignalRequest{
		TargetID: ready.User.ID,
		Kind:     protocol.SignalCandidate,
	}, protocol.ErrConflict)
}

func TestSelfMuteAndDeafenTravelWithTheState(t *testing.T) {
	h := newHarness(t, clientHosted)

	c := h.dial()
	ready := c.guest("Pablo")
	channelID := voiceChannel(t, ready)
	ok[struct{}](c, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID})
	ok[protocol.VoiceConnectResult](c, protocol.OpVoiceConnect,
		protocol.VoiceConnectRequest{ChannelID: channelID})

	deaf := true
	state := ok[protocol.VoiceStateEvent](c, protocol.OpVoiceState,
		protocol.VoiceStateRequest{SelfDeaf: &deaf})

	if !state.State.SelfDeaf {
		t.Fatal("deafening did not take")
	}
	if !state.State.SelfMute {
		t.Fatal("deafening yourself must mute you too")
	}

	// Un-deafening does not un-mute: the two are separate choices once made.
	deaf = false
	state = ok[protocol.VoiceStateEvent](c, protocol.OpVoiceState,
		protocol.VoiceStateRequest{SelfDeaf: &deaf})
	if state.State.SelfDeaf {
		t.Fatal("un-deafening did not take")
	}
	if !state.State.SelfMute {
		t.Fatal("un-deafening should leave the mute where it was")
	}
}

func TestVoiceModerationNeedsThePermission(t *testing.T) {
	h := newHarness(t, clientHosted)

	admin, adminReady := h.admin("Owner")
	channelID := voiceChannel(t, adminReady)

	member := h.dial()
	memberReady := member.guest("Ana")
	ok[struct{}](member, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID})

	mute := true
	member.fails(protocol.OpVoiceModerate, protocol.VoiceModerateRequest{
		UserID: adminReady.User.ID,
		Mute:   &mute,
	}, protocol.ErrForbidden)

	moderated := ok[protocol.VoiceStateEvent](admin, protocol.OpVoiceModerate,
		protocol.VoiceModerateRequest{UserID: memberReady.User.ID, Mute: &mute})
	if !moderated.State.Mute {
		t.Fatal("the moderator's mute did not take")
	}
	if moderated.State.SelfMute {
		t.Fatal("a moderator's mute is not the member's own")
	}

	// Unmuting yourself must not undo somebody else's mute.
	no := false
	own := ok[protocol.VoiceStateEvent](member, protocol.OpVoiceState,
		protocol.VoiceStateRequest{SelfMute: &no})
	if !own.State.Mute {
		t.Fatal("a member unmuted themselves out of a moderated mute")
	}
}

func TestAdminCanReconfigureTheAudioPlane(t *testing.T) {
	h := newHarness(t, serverHosted)

	admin, ready := h.admin("Owner")
	channelID := voiceChannel(t, ready)
	ok[struct{}](admin, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID})

	_, sdp := offer(t)
	ok[protocol.VoiceConnectResult](admin, protocol.OpVoiceConnect,
		protocol.VoiceConnectRequest{ChannelID: channelID, SDP: sdp})

	settings := protocol.VoiceSettings{
		Enabled:    true,
		Mode:       protocol.VoiceModeServerHost,
		SampleRate: 24000,
		Bitrate:    32000,
		MinBitrate: 16000,
		MaxBitrate: 64000,
		FEC:        true,
	}
	updated := ok[protocol.ServerUpdatedEvent](admin, protocol.OpServerUpdate,
		protocol.ServerUpdateRequest{Voice: &settings})

	if updated.Server.Voice.SampleRate != 24000 || updated.Server.Voice.MaxBitrate != 64000 {
		t.Fatalf("the audio plane was not applied: %+v", updated.Server.Voice)
	}

	// A live session cannot carry parameters negotiated before the change, so
	// everybody is asked to start over.
	var reset protocol.VoiceResetEvent
	decodeEvent(t, admin.waitEvent(protocol.EvVoiceReset), &reset)
	if reset.Reason != protocol.ResetConfigChanged {
		t.Fatalf("reset reason: got %q", reset.Reason)
	}

	// A rate Opus does not encode at is refused, whoever asks.
	settings.SampleRate = 44100
	admin.fails(protocol.OpServerUpdate, protocol.ServerUpdateRequest{Voice: &settings}, protocol.ErrBadRequest)

	// So is a range that is the wrong way round.
	settings.SampleRate = 48000
	settings.MinBitrate = 96000
	settings.MaxBitrate = 32000
	admin.fails(protocol.OpServerUpdate, protocol.ServerUpdateRequest{Voice: &settings}, protocol.ErrBadRequest)
}

func TestVoiceStatesTravelInTheSnapshot(t *testing.T) {
	h := newHarness(t, clientHosted)

	first := h.dial()
	firstReady := first.guest("Pablo")
	channelID := voiceChannel(t, firstReady)
	ok[struct{}](first, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID})
	ok[protocol.VoiceConnectResult](first, protocol.OpVoiceConnect,
		protocol.VoiceConnectRequest{ChannelID: channelID})

	second := h.dial()
	ready := second.guest("Ana")

	if len(ready.VoiceStates) != 1 {
		t.Fatalf("voice states: got %d, want 1", len(ready.VoiceStates))
	}
	state := ready.VoiceStates[0]
	if state.UserID != firstReady.User.ID || state.ChannelID != channelID {
		t.Fatalf("voice state: got %+v", state)
	}
	if !state.Connected || !state.Host {
		t.Fatalf("the only participant should be a connected host: got %+v", state)
	}
}

// decodeEvent unpacks an event payload into out.
func decodeEvent(t *testing.T, env protocol.Envelope, out any) {
	t.Helper()
	if err := json.Unmarshal(env.Data, out); err != nil {
		t.Fatalf("decode %s: %v", env.Op, err)
	}
}
