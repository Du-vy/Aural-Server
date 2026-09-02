package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/voice"
)

// The end-to-end check of the server-hosted relay.
//
// Everything else about voice can be tested by looking at frames. This cannot:
// the question it answers is whether audio one client sends comes out of
// another client, over a real peer connection, through a real ICE handshake,
// with the relay forwarding real RTP. It is the one test that would catch the
// media plane being wired up plausibly and not working.
//
// It also covers renegotiation, which is the part of the relay with the most
// moving pieces. The second arrival hears the first through the answer it was
// given; the first hears the second only because the relay offered again and
// the client answered.

// rtcClient is a protocol client that can hold a peer connection at the same
// time, which the synchronous test client cannot: signalling arrives as events
// while requests are in flight.
type rtcClient struct {
	t      *testing.T
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	seq     int
	pending map[string]chan protocol.Envelope

	events chan protocol.Envelope

	pc     *webrtc.PeerConnection
	userID int64
	// arrived carries every track the relay sends down.
	arrived chan *webrtc.TrackRemote
}

func dialRTC(t *testing.T, h *harness, nickname string) *rtcClient {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	url := "ws" + strings.TrimPrefix(h.http.URL, "http") + "/ws"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		cancel()
		t.Fatalf("dial: %v", err)
	}

	c := &rtcClient{
		t:       t,
		conn:    conn,
		ctx:     ctx,
		cancel:  cancel,
		pending: map[string]chan protocol.Envelope{},
		events:  make(chan protocol.Envelope, 256),
		arrived: make(chan *webrtc.TrackRemote, 8),
	}
	t.Cleanup(func() {
		if c.pc != nil {
			_ = c.pc.Close()
		}
		conn.Close(websocket.StatusNormalClosure, "")
		cancel()
	})

	go c.read()

	if hello := c.await(protocol.EvHello); hello.Op != protocol.EvHello {
		t.Fatalf("expected hello first, got %s", hello.Op)
	}
	var ready protocol.Ready
	c.request(t, protocol.OpAuthGuest, protocol.AuthGuestRequest{Nickname: nickname}, &ready)
	c.userID = ready.User.ID
	return c
}

func (c *rtcClient) read() {
	for {
		_, raw, err := c.conn.Read(c.ctx)
		if err != nil {
			close(c.events)
			return
		}
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		if env.ID != "" {
			c.mu.Lock()
			waiter, ok := c.pending[env.ID]
			delete(c.pending, env.ID)
			c.mu.Unlock()
			if ok {
				waiter <- env
			}
			continue
		}
		select {
		case c.events <- env:
		default:
			// A test that fills this buffer has already gone wrong somewhere
			// more interesting than here.
		}
	}
}

// request sends one op and decodes its reply into out, which may be nil.
func (c *rtcClient) request(t *testing.T, op string, payload any, out any) {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode %s: %v", op, err)
	}

	c.mu.Lock()
	c.seq++
	id := fmt.Sprintf("r%d-%d", c.userID, c.seq)
	reply := make(chan protocol.Envelope, 1)
	c.pending[id] = reply
	c.mu.Unlock()

	frame, err := json.Marshal(protocol.Envelope{ID: id, Op: op, Data: body})
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	if err := c.conn.Write(c.ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("write %s: %v", op, err)
	}

	select {
	case env := <-reply:
		if env.Op == protocol.OpError {
			t.Fatalf("%s failed: %s: %s", op, env.Error.Code, env.Error.Message)
		}
		if out != nil && len(env.Data) > 0 {
			if err := json.Unmarshal(env.Data, out); err != nil {
				t.Fatalf("decode %s result: %v", op, err)
			}
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no reply to %s", op)
	}
}

// await returns the next event with the given op.
func (c *rtcClient) await(op string) protocol.Envelope {
	c.t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case env, ok := <-c.events:
			if !ok {
				c.t.Fatalf("connection closed while waiting for %s", op)
			}
			if env.Op == op {
				return env
			}
		case <-deadline:
			c.t.Fatalf("event %s never arrived", op)
		}
	}
}

// openMedia brings a full media session up: a microphone, an offer, the
// relay's answer, and a goroutine that keeps answering it afterwards.
func (c *rtcClient) openMedia(t *testing.T, channelID int64) *webrtc.TrackLocalStaticSample {
	t.Helper()

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("peer connection: %v", err)
	}
	c.pc = pc

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", fmt.Sprintf("client-%d", c.userID),
	)
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("add track: %v", err)
	}

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		init := candidate.ToJSON()
		c.send(protocol.OpVoiceSignal, protocol.VoiceSignalRequest{
			TargetID: protocol.ServerPeer,
			Kind:     protocol.SignalCandidate,
			Candidate: &protocol.ICECandidate{
				Candidate:        init.Candidate,
				SDPMid:           init.SDPMid,
				SDPMLineIndex:    init.SDPMLineIndex,
				UsernameFragment: init.UsernameFragment,
			},
		})
	})
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		select {
		case c.arrived <- remote:
		default:
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set offer: %v", err)
	}

	var result protocol.VoiceConnectResult
	c.request(t, protocol.OpVoiceConnect, protocol.VoiceConnectRequest{
		ChannelID: channelID,
		SDP:       pc.LocalDescription().SDP,
	}, &result)

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  result.SDP,
	}); err != nil {
		t.Fatalf("set answer: %v", err)
	}

	go c.pumpSignals()
	return track
}

// pumpSignals answers whatever the relay sends for the rest of the test. The
// relay is the only side that offers after the first exchange, so this never
// has to resolve a collision.
func (c *rtcClient) pumpSignals() {
	for env := range c.events {
		if env.Op != protocol.EvVoiceSignal {
			continue
		}
		var event protocol.VoiceSignalEvent
		if err := json.Unmarshal(env.Data, &event); err != nil {
			continue
		}
		switch event.Kind {
		case protocol.SignalCandidate:
			if event.Candidate == nil {
				continue
			}
			_ = c.pc.AddICECandidate(webrtc.ICECandidateInit{
				Candidate:        event.Candidate.Candidate,
				SDPMid:           event.Candidate.SDPMid,
				SDPMLineIndex:    event.Candidate.SDPMLineIndex,
				UsernameFragment: event.Candidate.UsernameFragment,
			})
		case protocol.SignalOffer:
			if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer,
				SDP:  event.SDP,
			}); err != nil {
				continue
			}
			answer, err := c.pc.CreateAnswer(nil)
			if err != nil {
				continue
			}
			if err := c.pc.SetLocalDescription(answer); err != nil {
				continue
			}
			c.send(protocol.OpVoiceSignal, protocol.VoiceSignalRequest{
				TargetID: protocol.ServerPeer,
				Kind:     protocol.SignalAnswer,
				SDP:      c.pc.LocalDescription().SDP,
			})
		}
	}
}

// send fires a request and does not wait for its reply, which is what a
// callback on a pion goroutine needs.
func (c *rtcClient) send(op string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	frame, err := json.Marshal(protocol.Envelope{Op: op, Data: body})
	if err != nil {
		return
	}
	_ = c.conn.Write(c.ctx, websocket.MessageText, frame)
}

// speak writes audio until the test stops it. The payload is not real Opus,
// and does not need to be: nothing between the two ends decodes it.
func speak(track *webrtc.TrackLocalStaticSample, done <-chan struct{}) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	payload := make([]byte, 80)
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			_ = track.WriteSample(media.Sample{Data: payload, Duration: 20 * time.Millisecond})
		}
	}
}

// hear waits for a track and reads packets off it, reporting who it belongs to.
// The track is returned as well, so a caller that wants to check what stops
// arriving does not have to find it again.
func hear(t *testing.T, c *rtcClient, want int) (remote *webrtc.TrackRemote, streamID string, packets int) {
	t.Helper()

	select {
	case remote = <-c.arrived:
	case <-time.After(20 * time.Second):
		t.Fatal("no audio arrived")
	}

	deadline := time.Now().Add(20 * time.Second)
	for packets < want {
		if err := remote.SetReadDeadline(deadline); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		if _, _, err := remote.ReadRTP(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) || time.Now().After(deadline) {
				break
			}
			break
		}
		packets++
	}
	return remote, remote.StreamID(), packets
}

func TestServerHostedRelayCarriesAudioBothWays(t *testing.T) {
	if testing.Short() {
		t.Skip("the relay test brings up two real peer connections")
	}

	h := newHarness(t, serverHosted)

	// The seeded voice channel, found the same way every other test finds it.
	var channelID int64
	for _, ch := range h.server.Hub().SortedChannels() {
		if ch.Type == protocol.ChannelVoice {
			channelID = ch.ID
			break
		}
	}
	if channelID == 0 {
		t.Fatal("the seed has no voice channel")
	}

	alice := dialRTC(t, h, "Alice")
	alice.request(t, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID}, nil)
	aliceTrack := alice.openMedia(t, channelID)

	bob := dialRTC(t, h, "Bob")
	bob.request(t, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID}, nil)
	bobTrack := bob.openMedia(t, channelID)

	done := make(chan struct{})
	defer close(done)
	go speak(aliceTrack, done)
	go speak(bobTrack, done)

	// Bob hears Alice through the answer he was given: she was already
	// publishing when he arrived, so her track travelled in it.
	_, stream, packets := hear(t, bob, 20)
	if want := voice.StreamID(alice.userID); stream != want {
		t.Fatalf("bob heard stream %q, want %q", stream, want)
	}
	if packets < 20 {
		t.Fatalf("bob heard %d packets, want at least 20", packets)
	}

	// Alice hears Bob only because the relay offered again once his audio
	// turned up, and her client answered. That is the renegotiation path.
	_, stream, packets = hear(t, alice, 20)
	if want := voice.StreamID(bob.userID); stream != want {
		t.Fatalf("alice heard stream %q, want %q", stream, want)
	}
	if packets < 20 {
		t.Fatalf("alice heard %d packets, want at least 20", packets)
	}
}

func TestServerHostedRelayStopsAMutedParticipant(t *testing.T) {
	if testing.Short() {
		t.Skip("the relay test brings up two real peer connections")
	}

	h := newHarness(t, serverHosted)

	admin, ready := h.admin("Owner")
	var channelID int64
	for _, ch := range ready.Channels {
		if ch.Type == protocol.ChannelVoice {
			channelID = ch.ID
			break
		}
	}

	talker := dialRTC(t, h, "Talker")
	talker.request(t, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID}, nil)
	track := talker.openMedia(t, channelID)

	listener := dialRTC(t, h, "Listener")
	listener.request(t, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &channelID}, nil)
	listener.openMedia(t, channelID)

	done := make(chan struct{})
	defer close(done)
	go speak(track, done)

	remote, _, packets := hear(t, listener, 10)
	if packets < 10 {
		t.Fatalf("the listener heard %d packets before the mute, want at least 10", packets)
	}

	// A moderator's mute is enforced by the relay, not by the client agreeing
	// to stop: the talker below keeps sending throughout.
	mute := true
	state := ok[protocol.VoiceStateEvent](admin, protocol.OpVoiceModerate,
		protocol.VoiceModerateRequest{UserID: talker.userID, Mute: &mute})
	if !state.State.Mute {
		t.Fatal("the mute did not take")
	}

	// Whatever was already in flight when the mute landed is read off first,
	// so that what is measured afterwards is what the relay is forwarding now
	// rather than what it forwarded a moment ago.
	settle := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(settle) {
		if err := remote.SetReadDeadline(settle); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		if _, _, err := remote.ReadRTP(); err != nil {
			break
		}
	}

	if err := remote.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, _, err := remote.ReadRTP(); err == nil {
		t.Fatal("the relay forwarded audio from a muted participant")
	}
}
