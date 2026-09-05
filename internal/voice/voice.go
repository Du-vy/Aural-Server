// Package voice is the audio plane: the Opus parameters both hosting modes
// agree on, and the WebRTC relay a server-hosted channel runs.
//
// Audio never touches the WebSocket. That socket carries signalling — offers,
// answers and ICE candidates — and the media itself travels over RTP, encoded
// as Opus by the sender and decoded by the receiver. Nothing here encodes or
// decodes anything: a relay forwards packets it does not look inside, which is
// what lets this server be a single static binary with no cgo and no codec.
//
// The two hosting modes differ only in who does the forwarding. In
// server_host the Relay in this package does it. In client_host one of the
// clients does, and the server's part is limited to electing that client and
// passing signalling between the two ends — no code here is involved at all.
package voice

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// opusPayloadType is the dynamic payload type the relay offers Opus under. It
// is the number every WebRTC implementation in practice uses for it, which
// keeps the SDP boring.
const opusPayloadType = 111

// negotiationTimeout is how long a renegotiation may go unanswered before the
// peer is given up on. A client that does not answer an offer is a client
// whose media is already broken; dropping it makes it reconnect, which is a
// path that is exercised constantly and therefore works.
const negotiationTimeout = 20 * time.Second

// The ICE timeouts the relay runs with. They are shorter than the defaults
// because a voice call that has stalled for half a minute is not a call any
// more, and reconnecting is both quick and well tested.
const (
	iceDisconnectedTimeout = 6 * time.Second
	iceFailedTimeout       = 20 * time.Second
	iceKeepAliveInterval   = 2 * time.Second
)

// rtcpDrainBuffer is large enough for any RTCP compound packet. The reads exist
// to keep the interceptor chain fed, not to look at what arrives.
const rtcpDrainBuffer = 1500

// receiveMTU is the size of the buffer one RTP packet is read into. It is
// pion's own default for the same thing, and an Opus packet is a small
// fraction of it.
const receiveMTU = 1500

// Settings is the audio plane as the relay needs it. It is a plain value so a
// reconfiguration is a comparison and a swap rather than a lock discipline.
type Settings struct {
	SampleRate int
	Bitrate    int
	MinBitrate int
	MaxBitrate int
	FEC        bool
	DTX        bool
	Stereo     bool

	// PublicIP is substituted into host candidates when set. Without it a
	// server behind a one-to-one NAT advertises the private address of its own
	// interface, which no client outside could ever reach.
	PublicIP string
	// UDPPortMin and UDPPortMax bound the media ports. Both zero lets the
	// operating system choose.
	UDPPortMin int
	UDPPortMax int
}

// FmtpLine renders the Opus parameters as the SDP fmtp attribute both ends
// read. It is exported because the client is told the same numbers and has to
// arrive at the same encoder configuration for them to mean anything.
func (s Settings) FmtpLine() string {
	channels := 0
	if s.Stereo {
		channels = 1
	}
	return "minptime=10" +
		";useinbandfec=" + boolDigit(s.FEC) +
		";usedtx=" + boolDigit(s.DTX) +
		";stereo=" + strconv.Itoa(channels) +
		";sprop-stereo=" + strconv.Itoa(channels) +
		";maxplaybackrate=" + strconv.Itoa(s.SampleRate) +
		";maxaveragebitrate=" + strconv.Itoa(s.MaxBitrate)
}

func boolDigit(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// StreamID and TrackID name a participant's audio in the SDP the relay sends.
// A subscriber reads the publisher's identity straight off the arriving track
// rather than being told separately, which removes the window where a client
// holds audio it cannot yet attribute to anybody.
func StreamID(userID int64) string { return "av-" + strconv.FormatInt(userID, 10) }

// TrackID is the track name inside that stream.
func TrackID(userID int64) string { return "au-" + strconv.FormatInt(userID, 10) }

// Signal is one SDP or ICE frame moving between the relay and a client.
type Signal struct {
	Kind      string
	SDP       string
	Candidate *protocol.ICECandidate
}

// Errors the gateway distinguishes. Everything else is reported as it comes.
var (
	// ErrNoSession means the peer named is not in this relay, which happens
	// whenever signalling arrives just after a teardown. It is ordinary.
	ErrNoSession = errors.New("voice: no media session for that user")
	// ErrClosed means the relay is shutting down.
	ErrClosed = errors.New("voice: relay is closed")
)

// Relay forwards RTP between the participants of server-hosted voice channels.
//
// One channel is one room and one participant is one peer connection. Each
// publisher gets its own local track per subscriber rather than one shared
// between them, which costs a little memory and buys the only thing worth
// having here: the ability to stop one person's audio reaching one other
// person, which is what muting and deafening are.
type Relay struct {
	log *slog.Logger

	mu       sync.Mutex
	settings Settings
	api      *webrtc.API
	rooms    map[int64]*room
	closed   bool

	// onGone is called when the relay itself drops a peer — a failed
	// transport, an unanswered renegotiation — as opposed to being told to.
	// The gateway turns it into the reset event that makes a client try again.
	onGone func(channelID, userID int64)
}

// NewRelay builds a relay for the given settings. It binds nothing until the
// first participant arrives, so constructing one on a server nobody is calling
// on costs a struct.
func NewRelay(settings Settings, log *slog.Logger, onGone func(channelID, userID int64)) (*Relay, error) {
	api, err := buildAPI(settings)
	if err != nil {
		return nil, err
	}
	if onGone == nil {
		onGone = func(int64, int64) {}
	}
	return &Relay{
		log:      log.With(slog.String("component", "voice")),
		settings: settings,
		api:      api,
		rooms:    map[int64]*room{},
		onGone:   onGone,
	}, nil
}

// buildAPI assembles the WebRTC stack. Only Opus is registered: a client that
// offers anything else is answered with an audio section it cannot use, which
// is the correct answer to a client offering video to a voice server.
func buildAPI(s Settings) (*webrtc.API, error) {
	media := &webrtc.MediaEngine{}
	// Opus is always signalled as two channels in the rtpmap regardless of what
	// it actually carries; whether it is mono or stereo is the fmtp's business.
	if err := media.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: s.FmtpLine(),
		},
		PayloadType: opusPayloadType,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("voice: register opus: %w", err)
	}

	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(media, registry); err != nil {
		return nil, fmt.Errorf("voice: interceptors: %w", err)
	}

	setting := webrtc.SettingEngine{}
	// The loopback candidate is offered. It is useless to everybody but a
	// client on this very machine, and that client is the first one any
	// operator has: running the server and talking to it from the same
	// desktop is how a self-hosted server gets tried out, and on a machine
	// with no network at all it is the only address there is.
	setting.SetIncludeLoopbackCandidate(true)
	setting.SetICETimeouts(iceDisconnectedTimeout, iceFailedTimeout, iceKeepAliveInterval)
	if s.PublicIP != "" {
		setting.SetNAT1To1IPs([]string{s.PublicIP}, webrtc.ICECandidateTypeHost)
	}
	if s.UDPPortMin > 0 && s.UDPPortMax >= s.UDPPortMin {
		if err := setting.SetEphemeralUDPPortRange(uint16(s.UDPPortMin), uint16(s.UDPPortMax)); err != nil {
			return nil, fmt.Errorf("voice: udp port range: %w", err)
		}
	}

	return webrtc.NewAPI(
		webrtc.WithMediaEngine(media),
		webrtc.WithInterceptorRegistry(registry),
		webrtc.WithSettingEngine(setting),
	), nil
}

// Settings returns the configuration the relay is running.
func (r *Relay) Settings() Settings {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settings
}

// Reconfigure swaps the audio plane. Existing sessions cannot be migrated —
// the codec parameters live in SDP that was already negotiated — so they are
// torn down and the gateway asks every client to open a new one. Settings that
// have not changed are a no-op, which is what keeps an administrator saving an
// unrelated field from cutting off a call.
func (r *Relay) Reconfigure(settings Settings) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	if r.settings == settings {
		r.mu.Unlock()
		return nil
	}
	api, err := buildAPI(settings)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	rooms := r.rooms
	r.settings, r.api, r.rooms = settings, api, map[int64]*room{}
	r.mu.Unlock()

	for _, rm := range rooms {
		rm.closeAll()
	}
	return nil
}

// Close tears every session down. The relay cannot be used afterwards.
func (r *Relay) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	rooms := r.rooms
	r.rooms = map[int64]*room{}
	r.mu.Unlock()

	for _, rm := range rooms {
		rm.closeAll()
	}
}

// Join answers a participant's offer and wires them into the channel's room.
//
// The answer already carries the audio of everyone who was there first, so an
// arrival hears the room without a further round trip. Whether the arrival is
// heard depends on their own track turning up, which happens moments later and
// renegotiates everybody else.
//
// out is called with every signalling frame the relay produces for this
// participant, from any goroutine, and must not block.
func (r *Relay) Join(channelID, userID int64, offer string, out func(Signal)) (string, error) {
	if offer == "" {
		return "", errors.New("voice: an offer is required to open a media session")
	}

	// A second session for the same identity replaces the first. It is what a
	// client reconnecting after a failure looks like from here, and leaving the
	// old peer in place would have everybody hear them twice. It happens before
	// the room is taken, because emptying a room is what discards it.
	r.Leave(channelID, userID)

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return "", ErrClosed
	}
	api := r.api
	rm, ok := r.rooms[channelID]
	if !ok {
		rm = newRoom(channelID, r)
		r.rooms[channelID] = rm
	}
	r.mu.Unlock()

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", fmt.Errorf("voice: peer connection: %w", err)
	}

	p := &peer{
		userID: userID,
		room:   rm,
		pc:     pc,
		out:    out,
		subs:   map[int64]*subscription{},
	}
	p.publication = &publication{owner: p, sinks: map[int64]*sink{}}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			p.emit(Signal{Kind: protocol.SignalEnd})
			return
		}
		init := c.ToJSON()
		p.emit(Signal{Kind: protocol.SignalCandidate, Candidate: &protocol.ICECandidate{
			Candidate:        init.Candidate,
			SDPMid:           init.SDPMid,
			SDPMLineIndex:    init.SDPMLineIndex,
			UsernameFragment: init.UsernameFragment,
		}})
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			// Never inline: this runs on a pion goroutine, and tearing the
			// connection down from inside its own callback deadlocks it.
			go rm.evict(p, "transport "+state.String())
		default:
		}
	})

	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if remote.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		rm.publish(p, remote)
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offer,
	}); err != nil {
		_ = pc.Close()
		return "", fmt.Errorf("voice: that offer could not be read: %w", err)
	}

	// Subscribing before the answer is what puts the room into it.
	rm.admit(p)

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		rm.evict(p, "answer failed")
		return "", fmt.Errorf("voice: create answer: %w", err)
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		rm.evict(p, "answer failed")
		return "", fmt.Errorf("voice: set answer: %w", err)
	}

	local := pc.LocalDescription()
	if local == nil {
		rm.evict(p, "answer missing")
		return "", errors.New("voice: the answer went missing")
	}
	return local.SDP, nil
}

// Accept applies a signalling frame a client sent towards the relay.
func (r *Relay) Accept(channelID, userID int64, sig Signal) error {
	p := r.peer(channelID, userID)
	if p == nil {
		return ErrNoSession
	}
	return p.accept(sig)
}

// Leave closes one participant's session. It is safe to call for somebody who
// never had one.
func (r *Relay) Leave(channelID, userID int64) {
	r.mu.Lock()
	rm := r.rooms[channelID]
	r.mu.Unlock()
	if rm == nil {
		return
	}
	rm.remove(userID)
	r.dropIfEmpty(rm)
}

// LeaveAll closes every session a user holds, wherever it is. Disconnecting is
// the one path that cannot name the channel, because the session is already
// gone by the time anything notices.
func (r *Relay) LeaveAll(userID int64) {
	r.mu.Lock()
	rooms := make([]*room, 0, len(r.rooms))
	for _, rm := range r.rooms {
		rooms = append(rooms, rm)
	}
	r.mu.Unlock()

	for _, rm := range rooms {
		rm.remove(userID)
		r.dropIfEmpty(rm)
	}
}

// CloseChannel tears down a whole room, which is what a deleted channel and a
// reconfigured audio plane both amount to.
func (r *Relay) CloseChannel(channelID int64) {
	r.mu.Lock()
	rm := r.rooms[channelID]
	delete(r.rooms, channelID)
	r.mu.Unlock()
	if rm != nil {
		rm.closeAll()
	}
}

// SetMuted stops or resumes forwarding what a participant sends. A muted
// client stops sending too; this is the half that does not depend on the
// client agreeing.
func (r *Relay) SetMuted(channelID, userID int64, muted bool) {
	if p := r.peer(channelID, userID); p != nil {
		p.muted.Store(muted)
	}
}

// SetDeafened stops or resumes forwarding everything towards a participant.
func (r *Relay) SetDeafened(channelID, userID int64, deafened bool) {
	if p := r.peer(channelID, userID); p != nil {
		p.deafened.Store(deafened)
	}
}

// Connected reports whether a participant holds a live session here.
func (r *Relay) Connected(channelID, userID int64) bool {
	return r.peer(channelID, userID) != nil
}

func (r *Relay) peer(channelID, userID int64) *peer {
	r.mu.Lock()
	rm := r.rooms[channelID]
	r.mu.Unlock()
	if rm == nil {
		return nil
	}
	return rm.peer(userID)
}

// dropIfEmpty forgets a room nobody is in. Keeping it would leak a map entry
// per voice channel ever used, which on a long-lived server is a slow leak
// rather than a bounded cost.
func (r *Relay) dropIfEmpty(rm *room) {
	rm.mu.Lock()
	empty := len(rm.peers) == 0
	rm.mu.Unlock()
	if !empty {
		return
	}

	r.mu.Lock()
	if current, ok := r.rooms[rm.channelID]; ok && current == rm {
		rm.mu.Lock()
		stillEmpty := len(rm.peers) == 0
		rm.mu.Unlock()
		if stillEmpty {
			delete(r.rooms, rm.channelID)
		}
	}
	r.mu.Unlock()
}

// --- rooms ------------------------------------------------------------------

// room is one voice channel's worth of peers.
//
// Its mutex is the outermost of the three in this package. The lock order is
// room, then peer, then publication, and nothing ever takes them the other way
// round: the RTP forwarding loop, which is the only hot path, takes the
// publication's read lock and no other.
type room struct {
	channelID int64
	relay     *Relay

	mu    sync.Mutex
	peers map[int64]*peer
}

func newRoom(channelID int64, relay *Relay) *room {
	return &room{channelID: channelID, relay: relay, peers: map[int64]*peer{}}
}

func (rm *room) peer(userID int64) *peer {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return rm.peers[userID]
}

// admit registers a joining peer and subscribes it to everything already being
// published. It is called before the answer is created, so the subscriptions
// travel in that answer and need no renegotiation.
func (rm *room) admit(p *peer) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	for _, other := range rm.peers {
		if other.publication.live() {
			rm.link(other.publication, p, false)
		}
	}
	rm.peers[p.userID] = p
}

// publish takes a participant's arriving track, hands it to everyone else, and
// starts forwarding it. The forwarding loop owns the track until it errors,
// which is how a closed peer connection ends it.
func (rm *room) publish(p *peer, remote *webrtc.TrackRemote) {
	rm.mu.Lock()
	if rm.peers[p.userID] != p {
		// The peer was evicted between its track arriving and this running.
		rm.mu.Unlock()
		return
	}
	pub := p.publication
	pub.arm(remote.Codec().RTPCodecCapability)
	for _, other := range rm.peers {
		if other != p {
			rm.link(pub, other, true)
		}
	}
	rm.mu.Unlock()

	go pub.forward(remote, rm.relay.log)
}

// link gives one subscriber a track carrying one publisher's audio. rm.mu is
// held by every caller.
func (rm *room) link(pub *publication, sub *peer, renegotiate bool) {
	if sub.subs[pub.owner.userID] != nil {
		return
	}
	track, err := webrtc.NewTrackLocalStaticRTP(
		pub.capability(),
		TrackID(pub.owner.userID),
		StreamID(pub.owner.userID),
	)
	if err != nil {
		rm.relay.log.Error("build relay track",
			slog.Int64("channel", rm.channelID),
			slog.Int64("from", pub.owner.userID),
			slog.Int64("to", sub.userID),
			slog.Any("error", err))
		return
	}
	sender, err := sub.pc.AddTrack(track)
	if err != nil {
		rm.relay.log.Warn("subscribe to relay track",
			slog.Int64("channel", rm.channelID),
			slog.Int64("from", pub.owner.userID),
			slog.Int64("to", sub.userID),
			slog.Any("error", err))
		return
	}

	sub.subs[pub.owner.userID] = &subscription{sender: sender}
	pub.attach(sub, track)
	go drainRTCP(sender)

	if renegotiate {
		sub.renegotiate()
	}
}

// remove takes a participant out of the room and out of everybody's ears.
func (rm *room) remove(userID int64) {
	rm.mu.Lock()
	p := rm.peers[userID]
	if p == nil {
		rm.mu.Unlock()
		return
	}
	delete(rm.peers, userID)

	// Stop everybody receiving them.
	for subID := range p.publication.detachAll() {
		other := rm.peers[subID]
		if other == nil {
			continue
		}
		if s := other.subs[userID]; s != nil {
			delete(other.subs, userID)
			if err := other.pc.RemoveTrack(s.sender); err != nil {
				rm.relay.log.Debug("remove relay track", slog.Any("error", err))
			}
			other.renegotiate()
		}
	}

	// Stop them receiving everybody.
	for pubID := range p.subs {
		if other := rm.peers[pubID]; other != nil {
			other.publication.detach(userID)
		}
	}
	rm.mu.Unlock()

	p.close()
}

// evict is remove for a peer the relay itself gave up on, and it tells the
// gateway so the client can be asked to try again.
func (rm *room) evict(p *peer, reason string) {
	rm.mu.Lock()
	current := rm.peers[p.userID] == p
	rm.mu.Unlock()

	if !current {
		// Already gone, or replaced by a newer session that must not be cut.
		p.close()
		return
	}
	rm.relay.log.Info("voice session dropped",
		slog.Int64("channel", rm.channelID),
		slog.Int64("user", p.userID),
		slog.String("reason", reason))

	rm.remove(p.userID)
	rm.relay.dropIfEmpty(rm)
	rm.relay.onGone(rm.channelID, p.userID)
}

// closeAll empties the room without notifying anybody: the caller is either
// shutting the server down or has already told everyone to reconnect.
func (rm *room) closeAll() {
	rm.mu.Lock()
	peers := rm.peers
	rm.peers = map[int64]*peer{}
	rm.mu.Unlock()

	for _, p := range peers {
		p.publication.detachAll()
		p.close()
	}
}

// --- peers ------------------------------------------------------------------

// subscription is one publisher's audio as it is sent to one subscriber. The
// track itself is held by the publication, which is what writes to it.
type subscription struct {
	sender *webrtc.RTPSender
}

// peer is one participant's connection to the relay.
type peer struct {
	userID int64
	room   *room
	pc     *webrtc.PeerConnection
	out    func(Signal)

	// muted stops what this peer sends; deafened stops what it receives. Both
	// are read on the forwarding path, once per packet per subscriber, which is
	// why they are atomics rather than anything the mutex guards.
	muted    atomic.Bool
	deafened atomic.Bool

	// publication is this peer's own audio. It exists from the moment the peer
	// does, and starts carrying packets when the track arrives.
	publication *publication

	// subs is what this peer receives, keyed by publisher. It is guarded by the
	// room mutex, not by mu: every change to it is a structural change to the
	// room.
	subs map[int64]*subscription

	mu          sync.Mutex
	closed      bool
	negotiating bool
	pending     bool
	timer       *time.Timer
}

func (p *peer) emit(sig Signal) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if !closed {
		p.out(sig)
	}
}

// renegotiate offers the peer its current set of tracks.
//
// The relay is the only side that ever offers after the first exchange: a
// client adds its microphone before its opening offer and never changes it, so
// there is exactly one offerer at any moment and no glare to resolve. A second
// renegotiation arriving while one is outstanding is remembered rather than
// sent, and runs when the answer lands.
func (p *peer) renegotiate() {
	p.mu.Lock()
	switch {
	case p.closed:
		p.mu.Unlock()
		return
	case p.negotiating:
		p.pending = true
		p.mu.Unlock()
		return
	}
	p.negotiating = true
	p.armTimeoutLocked()
	p.mu.Unlock()

	offer, err := p.pc.CreateOffer(nil)
	if err == nil {
		err = p.pc.SetLocalDescription(offer)
	}
	if err != nil {
		p.room.relay.log.Warn("renegotiate voice session",
			slog.Int64("channel", p.room.channelID),
			slog.Int64("user", p.userID),
			slog.Any("error", err))
		go p.room.evict(p, "renegotiation failed")
		return
	}

	local := p.pc.LocalDescription()
	if local == nil {
		go p.room.evict(p, "offer went missing")
		return
	}
	p.emit(Signal{Kind: protocol.SignalOffer, SDP: local.SDP})
}

// armTimeoutLocked starts the clock on an outstanding offer. p.mu is held.
func (p *peer) armTimeoutLocked() {
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(negotiationTimeout, func() {
		p.room.evict(p, "renegotiation went unanswered")
	})
}

// accept applies one signalling frame from the client.
func (p *peer) accept(sig Signal) error {
	switch sig.Kind {
	case protocol.SignalAnswer:
		if sig.SDP == "" {
			return errors.New("voice: that answer carries no sdp")
		}
		if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer,
			SDP:  sig.SDP,
		}); err != nil {
			return fmt.Errorf("voice: that answer could not be applied: %w", err)
		}

		p.mu.Lock()
		p.negotiating = false
		again := p.pending
		p.pending = false
		if p.timer != nil {
			p.timer.Stop()
			p.timer = nil
		}
		p.mu.Unlock()

		if again {
			p.renegotiate()
		}
		return nil

	case protocol.SignalCandidate:
		if sig.Candidate == nil || sig.Candidate.Candidate == "" {
			// An empty candidate is how a browser says it has finished
			// gathering. There is nothing to add and nothing wrong.
			return nil
		}
		return p.pc.AddICECandidate(webrtc.ICECandidateInit{
			Candidate:        sig.Candidate.Candidate,
			SDPMid:           sig.Candidate.SDPMid,
			SDPMLineIndex:    sig.Candidate.SDPMLineIndex,
			UsernameFragment: sig.Candidate.UsernameFragment,
		})

	case protocol.SignalEnd:
		return nil

	case protocol.SignalOffer:
		// The relay offers and the client answers, always. An offer from a
		// client mid-session means the two ends disagree about that, and
		// answering it would leave the session in a state neither expects.
		return errors.New("voice: the relay is the only side that offers")

	default:
		return fmt.Errorf("voice: unknown signalling frame %q", sig.Kind)
	}
}

func (p *peer) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.mu.Unlock()

	if err := p.pc.Close(); err != nil {
		p.room.relay.log.Debug("close voice peer",
			slog.Int64("user", p.userID), slog.Any("error", err))
	}
}

// --- forwarding -------------------------------------------------------------

// sink is one subscriber's copy of one publisher's audio.
type sink struct {
	track *webrtc.TrackLocalStaticRTP
	peer  *peer
}

// publication is one participant's outgoing audio and everybody listening to it.
type publication struct {
	owner *peer

	mu    sync.RWMutex
	armed bool
	codec webrtc.RTPCodecCapability
	sinks map[int64]*sink
}

// live reports whether a track has arrived for this publication yet. A peer
// that has connected but is not sending — no microphone, or permission to
// listen and not to speak — never becomes live, and nobody subscribes to it.
func (pub *publication) live() bool {
	pub.mu.RLock()
	defer pub.mu.RUnlock()
	return pub.armed
}

func (pub *publication) arm(codec webrtc.RTPCodecCapability) {
	pub.mu.Lock()
	pub.armed, pub.codec = true, codec
	pub.mu.Unlock()
}

func (pub *publication) capability() webrtc.RTPCodecCapability {
	pub.mu.RLock()
	defer pub.mu.RUnlock()
	return pub.codec
}

func (pub *publication) attach(sub *peer, track *webrtc.TrackLocalStaticRTP) {
	pub.mu.Lock()
	pub.sinks[sub.userID] = &sink{track: track, peer: sub}
	pub.mu.Unlock()
}

func (pub *publication) detach(subscriberID int64) {
	pub.mu.Lock()
	delete(pub.sinks, subscriberID)
	pub.mu.Unlock()
}

// detachAll empties the publication and returns what was in it, so the caller
// can undo the other half of each subscription.
func (pub *publication) detachAll() map[int64]*sink {
	pub.mu.Lock()
	sinks := pub.sinks
	pub.sinks = map[int64]*sink{}
	pub.mu.Unlock()
	return sinks
}

// forward copies RTP from one publisher to everyone listening, until the track
// ends. It is the only hot path in the server, so it allocates nothing per
// packet and takes exactly one read lock.
//
// Reading into a buffer of its own is what makes the first half of that true.
// TrackRemote.ReadRTP, the obvious call, allocates a receive buffer and a
// packet on every call — at fifty packets a second per publisher, a room of
// ten people is a thousand allocations a second and most of a megabyte, all of
// it garbage, all of it in the one loop that must not be interrupted by a
// collection. Reading and unmarshalling into the same two values instead costs
// nothing per packet.
//
// It is only safe because every write below is synchronous: Unmarshal points
// packet.Payload straight into buf, and pion's SRTP session marshals header
// and payload into a pooled buffer of its own before encrypting, so nothing
// downstream is still holding either by the time the next packet overwrites
// them. rtp.Header.Unmarshal reuses its CSRC and extension slices for the same
// reason, so the packet is meant to be filled in over and over.
//
// A muted publisher and a deafened subscriber are both handled by not writing
// the packet. The gap that leaves in the sequence numbers is what the far end's
// concealment is for, and it is the same gap a lost packet leaves.
func (pub *publication) forward(remote *webrtc.TrackRemote, log *slog.Logger) {
	owner := pub.owner
	buf := make([]byte, receiveMTU)
	var packet rtp.Packet

	for {
		n, _, err := remote.Read(buf)
		if err != nil {
			// The peer connection closed, which is the ordinary way out.
			return
		}
		if owner.muted.Load() {
			continue
		}
		if err := packet.Unmarshal(buf[:n]); err != nil {
			// A packet that will not parse is one packet, not a reason to stop
			// carrying the rest of somebody's audio.
			log.Debug("read voice packet",
				slog.Int64("from", owner.userID), slog.Any("error", err))
			continue
		}

		pub.mu.RLock()
		for _, s := range pub.sinks {
			if s.peer.deafened.Load() {
				continue
			}
			if err := s.track.WriteRTP(&packet); err != nil {
				// One subscriber's transport going away must not stop the
				// others hearing anything; the peer's own state change is what
				// removes it.
				log.Debug("forward voice packet",
					slog.Int64("from", owner.userID),
					slog.Int64("to", s.peer.userID),
					slog.Any("error", err))
			}
		}
		pub.mu.RUnlock()
	}
}

// drainRTCP reads and discards the receiver reports a subscriber sends back.
// They are not acted on, but leaving them unread stalls the interceptor chain
// that produced them.
func drainRTCP(sender *webrtc.RTPSender) {
	buf := make([]byte, rtcpDrainBuffer)
	for {
		if _, _, err := sender.Read(buf); err != nil {
			return
		}
	}
}
