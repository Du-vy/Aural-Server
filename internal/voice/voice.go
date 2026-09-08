// Package voice is the media plane: the Opus parameters both hosting modes
// agree on, and the WebRTC relay a server-hosted channel runs.
//
// Media never touches the WebSocket. That socket carries signalling — offers,
// answers and ICE candidates — and the media itself travels over RTP, encoded
// by the sender and decoded by the receiver. Nothing here encodes or decodes
// anything: a relay forwards packets it does not look inside, which is what
// lets this server be a single static binary with no cgo and no codec.
//
// The two hosting modes differ only in who does the forwarding. In
// server_host the Relay in this package does it. In client_host one of the
// clients does, and the server's part is limited to electing that client and
// passing signalling between the two ends — no code here is involved at all.
//
// A participant publishes up to three things and they are treated separately
// all the way down: a microphone, a shared screen, and that screen's sound.
// The microphone goes to everybody in the room, because that is what a call
// is. A screen goes only to whoever asked to watch it, because it is two
// orders of magnitude more expensive and nobody wants four of them arriving
// unasked.
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
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// The dynamic payload types the relay offers each codec under. They are the
// numbers Chromium itself uses for the same codecs, which keeps the SDP boring
// and the answers predictable.
const (
	opusPayloadType = 111
	vp9PayloadType  = 98
	vp8PayloadType  = 96
	h264PayloadType = 102
	av1PayloadType  = 45
)

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

// rtcpBuffer is large enough for any RTCP compound packet.
const rtcpBuffer = 1500

// receiveMTU is the size of the buffer one RTP packet is read into. It is
// pion's own default for the same thing, and an Opus packet is a small
// fraction of it.
const receiveMTU = 1500

// keyframeInterval is the shortest gap between two keyframe requests sent to
// one publisher.
//
// A viewer joining a video stream sees nothing until the next keyframe, so one
// is asked for the moment they subscribe. Five viewers arriving together would
// otherwise ask five times and get five keyframes, each of them a burst an
// order of magnitude larger than an ordinary frame — the exact opposite of
// what a stream that is already struggling needs. Asking at most twice a
// second collapses that into one.
const keyframeInterval = 500 * time.Millisecond

// Settings is the audio plane as the relay needs it. It is a plain value so a
// reconfiguration is a comparison and a swap rather than a lock discipline.
//
// It says nothing about video on purpose. The relay does not encode, so the
// resolution and frame rate of a shared screen are between the sender and its
// own encoder; what the server allows is checked when the share is announced,
// which is the only place it can be checked at all.
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

// StreamID and TrackID name a participant's media in the SDP the relay sends.
// A subscriber reads the publisher's identity and what the track carries
// straight off the arriving stream rather than being told separately, which
// removes the window where a client holds media it cannot yet attribute to
// anybody.
func StreamID(userID int64, purpose string) string {
	return streamPrefix(purpose) + strconv.FormatInt(userID, 10)
}

// TrackID is the track name inside that stream.
func TrackID(userID int64, purpose string) string {
	return trackPrefix(purpose) + strconv.FormatInt(userID, 10)
}

// The stream and track name prefixes, one pair per purpose. The microphone
// keeps the names it has always had so that a client older than screen sharing
// still finds the audio exactly where it used to be.
func streamPrefix(purpose string) string {
	switch purpose {
	case protocol.TrackScreen:
		return "sc-"
	case protocol.TrackScreenAudio:
		return "sa-"
	default:
		return "av-"
	}
}

func trackPrefix(purpose string) string {
	switch purpose {
	case protocol.TrackScreen:
		return "vi-"
	case protocol.TrackScreenAudio:
		return "sd-"
	default:
		return "au-"
	}
}

// publishedPurposes is every kind of media one participant can send, in the
// order a room walks them.
var publishedPurposes = []string{protocol.TrackMic, protocol.TrackScreen, protocol.TrackScreenAudio}

// screenPurposes is the subset a screen share consists of.
var screenPurposes = []string{protocol.TrackScreen, protocol.TrackScreenAudio}

// Signal is one SDP or ICE frame moving between the relay and a client.
type Signal struct {
	Kind      string
	SDP       string
	Candidate *protocol.ICECandidate
	// Tracks and Purposes travel with an offer and say what each media section
	// of it is: whose media it carries, and which of that person's media.
	//
	// They are what lets one offer describe both directions at once. A section
	// naming the receiving client itself is a slot the relay has opened for
	// that client to send its own screen on — the relay offers to receive,
	// the client answers by sending — and a section naming anybody else is
	// media arriving. Without the maps a client would have to guess which of
	// several video sections it was meant to fill, and guessing is how media
	// ends up in the wrong window.
	Tracks   map[string]int64
	Purposes map[string]string
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
// having here: the ability to stop one person's media reaching one other
// person, which is what muting, deafening and choosing not to watch a screen
// all are.
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

// videoFeedback is the RTCP a video sender asks to be told about.
//
// The two that matter here are nack, which is how a lost packet is asked for
// again, and pli, which is how a receiver says it has nothing it can decode
// and needs a fresh keyframe. Both are forwarded across the relay rather than
// answered by it: a relay that does not encode cannot make a keyframe, it can
// only pass the request on to whoever can.
var videoFeedback = []webrtc.RTCPFeedback{
	{Type: "goog-remb"},
	{Type: "transport-cc"},
	{Type: "ccm", Parameter: "fir"},
	{Type: "nack"},
	{Type: "nack", Parameter: "pli"},
}

// videoCodecs is what the relay will carry a picture as, in the order it
// offers them.
//
// The order is a recommendation and not a decision: the relay offers the slot
// a client publishes its screen on, so the client answers, and an answerer
// picks from what it was offered. A client with a hardware H.264 encoder and a
// laptop fan it would rather not hear says so through its own codec
// preferences, and this list is what it chooses within.
//
// VP9 leads because a shared screen is the case it is best at: large flat
// areas of one colour and text that does not move are what its screen-content
// tools are for, and it holds legible text at a bitrate where H.264 has
// already given up. H.264 follows because it is the one codec that is encoded
// in hardware on almost every machine, which is what somebody sharing a game
// at sixty frames a second actually needs. AV1 is offered last: it compresses
// better than either and, encoded in software at 1080p in real time, costs
// more CPU than the machine sharing usually has to spare.
func videoCodecs() []webrtc.RTPCodecParameters {
	return []webrtc.RTPCodecParameters{
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeVP9,
				ClockRate:    90000,
				SDPFmtpLine:  "profile-id=0",
				RTCPFeedback: videoFeedback,
			},
			PayloadType: vp9PayloadType,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeH264,
				ClockRate:   90000,
				SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
				// Constrained Baseline at level 3.1 is the profile every
				// decoder in practice has, and packetization-mode=1 is the
				// only one worth carrying: mode 0 cannot fragment a NAL unit,
				// so a keyframe larger than the MTU has nowhere to go.
				RTCPFeedback: videoFeedback,
			},
			PayloadType: h264PayloadType,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeVP8,
				ClockRate:    90000,
				RTCPFeedback: videoFeedback,
			},
			PayloadType: vp8PayloadType,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeAV1,
				ClockRate:    90000,
				RTCPFeedback: videoFeedback,
			},
			PayloadType: av1PayloadType,
		},
	}
}

// buildAPI assembles the WebRTC stack.
//
// Opus is the only audio codec: a client that offers anything else is answered
// with an audio section it cannot use, which is the correct answer to a client
// offering something this server has not agreed to carry. Video is registered
// for the screen shares a voice channel may carry on top of the call, and for
// nothing else — there is no camera here.
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

	for _, codec := range videoCodecs() {
		if err := media.RegisterCodec(codec, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, fmt.Errorf("voice: register %s: %w", codec.MimeType, err)
		}
	}

	registry := &interceptor.Registry{}
	// This registers the negative acknowledgement pair as well as the reports,
	// and it does so for whichever registered codecs asked for them — which is
	// every video codec above and no audio one. That is the right split: a
	// lost Opus packet is concealed by the decoder and asking for it again
	// would arrive too late to play, while a lost video packet leaves a
	// visible hole until it is either resent or coded over.
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
// A shared screen is not part of any of that. The offer this answers carries
// one microphone and nothing else, and the sections a screen needs are opened
// later, by the relay, when somebody says they are about to share one.
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

	p := newPeer(userID, rm, pc, out)

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

	pc.OnTrack(func(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		rm.publish(p, remote, receiver)
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

// SetMuted stops or resumes forwarding what a participant's microphone sends.
// A muted client stops sending too; this is the half that does not depend on
// the client agreeing.
//
// It does not touch a screen share. Muting yourself while showing somebody a
// video is an ordinary thing to do, and a mute that silenced the video too
// would be a surprise nobody asked for.
func (r *Relay) SetMuted(channelID, userID int64, muted bool) {
	if p := r.peer(channelID, userID); p != nil {
		p.muted.Store(muted)
	}
}

// SetDeafened stops or resumes forwarding sound towards a participant. Video
// is unaffected: somebody who has stopped listening has not stopped looking.
func (r *Relay) SetDeafened(channelID, userID int64, deafened bool) {
	if p := r.peer(channelID, userID); p != nil {
		p.deafened.Store(deafened)
	}
}

// OpenScreen makes room in a participant's session for the screen they are
// about to share, and renegotiates if there was not room already.
//
// The relay offers to receive; the client answers by sending. That is the same
// direction of travel as everything else here — the relay is the only side
// that ever offers — and it is what lets a screen share start in the middle of
// a call without the two ends ever both offering at once.
//
// Calling it again for a share that is already open is how the sound of a
// screen is added to one that started without it, and is otherwise a no-op.
func (r *Relay) OpenScreen(channelID, userID int64, audio bool) error {
	p := r.peer(channelID, userID)
	if p == nil {
		return ErrNoSession
	}
	p.streaming.Store(true)
	return p.room.openScreen(p, audio)
}

// CloseScreen ends a participant's screen share: everybody watching stops
// receiving it, and the relay stops forwarding it whatever the client does.
//
// The sections themselves are kept. They cost two lines of SDP each and their
// being there is what makes starting a share again immediate rather than
// another round of negotiation.
func (r *Relay) CloseScreen(channelID, userID int64) {
	p := r.peer(channelID, userID)
	if p == nil {
		return
	}
	p.streaming.Store(false)
	p.room.closeScreen(p)
}

// Watch starts or stops sending one participant's screen to one other.
//
// A screen is the one thing here that is not sent to everybody: it is large
// enough that carrying it to somebody who is not looking would be the single
// most expensive thing this server does, so it is carried only where it was
// asked for.
func (r *Relay) Watch(channelID, viewerID, publisherID int64) error {
	return r.setWatching(channelID, viewerID, publisherID, true)
}

// Unwatch is Watch's other half. It is safe to call for a stream that was
// never being watched.
func (r *Relay) Unwatch(channelID, viewerID, publisherID int64) {
	_ = r.setWatching(channelID, viewerID, publisherID, false)
}

func (r *Relay) setWatching(channelID, viewerID, publisherID int64, watching bool) error {
	r.mu.Lock()
	rm := r.rooms[channelID]
	r.mu.Unlock()
	if rm == nil {
		return ErrNoSession
	}
	return rm.setWatching(viewerID, publisherID, watching)
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
// Its mutex is the outermost in this package. The lock order is room, then a
// peer's sections, then the peer itself, then a publication, and nothing ever
// takes them the other way round: the RTP forwarding loop, which is the only
// hot path, takes the publication's read lock and no other.
//
// The sections mutex exists because renegotiating has to read what a peer is
// sending, and renegotiating is very often the last thing done while the room
// is being rearranged. Guarding those maps with the room's own mutex would
// have that read wait for a lock its caller is already holding, which is a
// deadlock rather than a delay.
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

// admit registers a joining peer and subscribes it to every microphone already
// being published. It is called before the answer is created, so those
// subscriptions travel in that answer and need no renegotiation.
//
// Screens are not among them. An arrival is subscribed to nobody's screen and
// nobody is subscribed to theirs until somebody asks to watch, which is the
// whole of what makes a room with four screens in it affordable.
func (rm *room) admit(p *peer) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	for _, other := range rm.peers {
		if pub := other.publications[protocol.TrackMic]; pub.live() {
			rm.link(pub, p, false)
		}
	}
	rm.peers[p.userID] = p
}

// publish takes a participant's arriving track, works out what it is, hands it
// to everyone entitled to it and starts forwarding. The forwarding loop owns
// the track until it errors, which is how a closed peer connection ends it.
func (rm *room) publish(p *peer, remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	purpose := p.purposeOf(remote, receiver)

	rm.mu.Lock()
	if rm.peers[p.userID] != p {
		// The peer was evicted between its track arriving and this running.
		rm.mu.Unlock()
		return
	}
	pub := p.publications[purpose]
	if pub == nil {
		rm.mu.Unlock()
		return
	}
	generation := pub.arm(remote)

	// A microphone reaches the room the moment it arrives. A screen reaches
	// only whoever had already asked to watch it, which is nobody the first
	// time and can be several people on a share that stopped and started.
	for _, other := range rm.peers {
		if other == p {
			continue
		}
		if purpose == protocol.TrackMic || other.isWatching(p.userID) {
			rm.link(pub, other, true)
		}
	}
	rm.mu.Unlock()

	go pub.forward(remote, generation, rm.relay.log)
}

// openScreen gives a peer the media sections its screen share needs.
//
// It reports nothing about whether a renegotiation happened because there is
// nothing useful the caller could do with it: the offer, if there is one,
// travels on the peer's own signalling channel like every other.
func (rm *room) openScreen(p *peer, audio bool) error {
	wanted := []string{protocol.TrackScreen}
	if audio {
		wanted = append(wanted, protocol.TrackScreenAudio)
	}

	rm.mu.Lock()
	if rm.peers[p.userID] != p {
		rm.mu.Unlock()
		return ErrNoSession
	}
	added := false
	// A share that stopped and started again arrives on the section it already
	// had: the client merely put a track back on a sender it never gave up, so
	// no track "arrives" a second time and nothing here would otherwise notice
	// that the picture is flowing again. Re-arming is what notices.
	for _, purpose := range screenPurposes {
		pub := p.publications[purpose]
		if pub == nil || !pub.rearm() {
			continue
		}
		for _, other := range rm.peers {
			if other != p && other.isWatching(p.userID) {
				rm.link(pub, other, true)
			}
		}
	}
	for _, purpose := range wanted {
		if p.slot(purpose) != nil {
			continue
		}
		kind := webrtc.RTPCodecTypeVideo
		if purpose == protocol.TrackScreenAudio {
			kind = webrtc.RTPCodecTypeAudio
		}
		transceiver, err := p.pc.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		})
		if err != nil {
			rm.mu.Unlock()
			return fmt.Errorf("voice: open a %s section: %w", purpose, err)
		}
		p.setSlot(purpose, transceiver)
		added = true
	}
	rm.mu.Unlock()

	if added {
		p.renegotiate()
	}
	return nil
}

// closeScreen stops a screen share reaching anybody.
func (rm *room) closeScreen(p *peer) {
	rm.mu.Lock()
	for _, purpose := range screenPurposes {
		pub := p.publications[purpose]
		if pub == nil {
			continue
		}
		pub.disarm()
		for subscriberID := range pub.detachAll() {
			rm.unlink(rm.peers[subscriberID], p.userID, purpose, true)
		}
	}
	// Nobody is watching a stream that has ended. Leaving the intent behind
	// would have a share that started again reach people who stopped looking
	// several minutes ago.
	for _, other := range rm.peers {
		other.setWatching(p.userID, false)
	}
	rm.mu.Unlock()
}

// setWatching adds or removes one viewer's subscription to one screen.
func (rm *room) setWatching(viewerID, publisherID int64, watching bool) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	viewer := rm.peers[viewerID]
	publisher := rm.peers[publisherID]
	if viewer == nil || publisher == nil {
		return ErrNoSession
	}

	if !watching {
		viewer.setWatching(publisherID, false)
		for _, purpose := range screenPurposes {
			if pub := publisher.publications[purpose]; pub != nil {
				pub.detach(viewerID)
			}
			rm.unlink(viewer, publisherID, purpose, true)
		}
		return nil
	}

	viewer.setWatching(publisherID, true)
	for _, purpose := range screenPurposes {
		pub := publisher.publications[purpose]
		if pub != nil && pub.live() {
			rm.link(pub, viewer, true)
		}
	}
	return nil
}

// link gives one subscriber a track carrying one publisher's media. rm.mu is
// held by every caller.
func (rm *room) link(pub *publication, sub *peer, renegotiate bool) {
	key := subKey{publisher: pub.owner.userID, purpose: pub.purpose}
	if sub.subscription(key) != nil {
		return
	}
	track, err := webrtc.NewTrackLocalStaticRTP(
		pub.capability(),
		TrackID(pub.owner.userID, pub.purpose),
		StreamID(pub.owner.userID, pub.purpose),
	)
	if err != nil {
		rm.relay.log.Error("build relay track",
			slog.Int64("channel", rm.channelID),
			slog.Int64("from", pub.owner.userID),
			slog.Int64("to", sub.userID),
			slog.String("carries", pub.purpose),
			slog.Any("error", err))
		return
	}
	sender, err := sub.pc.AddTrack(track)
	if err != nil {
		rm.relay.log.Warn("subscribe to relay track",
			slog.Int64("channel", rm.channelID),
			slog.Int64("from", pub.owner.userID),
			slog.Int64("to", sub.userID),
			slog.String("carries", pub.purpose),
			slog.Any("error", err))
		return
	}

	sub.addSubscription(key, &subscription{sender: sender, purpose: pub.purpose, publisher: pub.owner.userID})
	pub.attach(sub, track)
	go readRTCP(sender, pub.requestKeyframe)

	if pub.video {
		// A subscriber that arrives mid-stream has nothing it can decode until
		// the next keyframe, which on a still picture may be a very long time
		// away. Asking for one now is the difference between a stream that
		// appears at once and one that appears eventually.
		pub.requestKeyframe()
	}
	if renegotiate {
		sub.renegotiate()
	}
}

// unlink undoes one subscription. rm.mu is held by every caller, and sub may
// be nil for a peer that has already gone.
func (rm *room) unlink(sub *peer, publisherID int64, purpose string, renegotiate bool) {
	if sub == nil {
		return
	}
	entry := sub.removeSubscription(subKey{publisher: publisherID, purpose: purpose})
	if entry == nil {
		return
	}
	if err := sub.pc.RemoveTrack(entry.sender); err != nil {
		rm.relay.log.Debug("remove relay track", slog.Any("error", err))
	}
	if renegotiate {
		sub.renegotiate()
	}
}

// remove takes a participant out of the room, out of everybody's ears and off
// everybody's screen.
func (rm *room) remove(userID int64) {
	rm.mu.Lock()
	p := rm.peers[userID]
	if p == nil {
		rm.mu.Unlock()
		return
	}
	delete(rm.peers, userID)

	// Stop everybody receiving them.
	for _, purpose := range publishedPurposes {
		pub := p.publications[purpose]
		if pub == nil {
			continue
		}
		for subscriberID := range pub.detachAll() {
			rm.unlink(rm.peers[subscriberID], userID, purpose, true)
		}
	}

	// Stop them receiving everybody.
	for _, key := range p.subscriptions() {
		if other := rm.peers[key.publisher]; other != nil {
			if pub := other.publications[key.purpose]; pub != nil {
				pub.detach(userID)
			}
		}
	}
	// And stop anybody remembering that they were watching this person.
	for _, other := range rm.peers {
		other.setWatching(userID, false)
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
		for _, pub := range p.publications {
			pub.detachAll()
		}
		p.close()
	}
}

// --- peers ------------------------------------------------------------------

// subKey names one subscription: whose media, and which of their media. Both
// halves are needed because one publisher can be the source of three different
// things and a viewer may hold any subset of them.
type subKey struct {
	publisher int64
	purpose   string
}

// subscription is one publisher's media as it is sent to one subscriber. The
// track itself is held by the publication, which is what writes to it.
type subscription struct {
	sender    *webrtc.RTPSender
	purpose   string
	publisher int64
}

// peer is one participant's connection to the relay.
type peer struct {
	userID int64
	room   *room
	pc     *webrtc.PeerConnection
	out    func(Signal)

	// muted stops what this peer's microphone sends; deafened stops the sound
	// it receives; streaming is whether its screen share is meant to be
	// running at all. All three are read on the forwarding path, once per
	// packet per subscriber, which is why they are atomics rather than
	// anything the mutex guards.
	muted     atomic.Bool
	deafened  atomic.Bool
	streaming atomic.Bool

	// publications is everything this peer sends, by purpose. All three exist
	// from the moment the peer does and start carrying packets when a track
	// arrives for them.
	publications map[string]*publication

	// sectionsMu guards the three maps below. They are only ever changed while
	// the room mutex is held — every change to them is a structural change to
	// the room — but they are read while renegotiating, which happens with the
	// room mutex already held, so the room's own mutex cannot be what protects
	// them.
	sectionsMu sync.Mutex
	// slots are the media sections opened for this peer to publish a screen
	// on, by purpose. They are how an arriving track is known to be a screen
	// rather than a microphone, which the kind alone cannot say: a screen's
	// sound and a voice are both audio.
	slots map[string]*webrtc.RTPTransceiver
	// subs is what this peer receives, by publisher and purpose.
	subs map[subKey]*subscription
	// watching is whose screens this peer has asked for. It outlives the
	// tracks: somebody who asked to watch a stream that had not started yet is
	// sent it the moment it does.
	watching map[int64]bool

	mu          sync.Mutex
	closed      bool
	negotiating bool
	pending     bool
	timer       *time.Timer
}

func newPeer(userID int64, rm *room, pc *webrtc.PeerConnection, out func(Signal)) *peer {
	p := &peer{
		userID:       userID,
		room:         rm,
		pc:           pc,
		out:          out,
		publications: make(map[string]*publication, len(publishedPurposes)),
		slots:        map[string]*webrtc.RTPTransceiver{},
		subs:         map[subKey]*subscription{},
		watching:     map[int64]bool{},
	}
	for _, purpose := range publishedPurposes {
		p.publications[purpose] = &publication{
			owner:   p,
			purpose: purpose,
			video:   purpose == protocol.TrackScreen,
			sinks:   map[int64]*sink{},
		}
	}
	return p
}

// The sections a peer holds, each behind the one mutex that guards them.

func (p *peer) subscription(key subKey) *subscription {
	p.sectionsMu.Lock()
	defer p.sectionsMu.Unlock()
	return p.subs[key]
}

func (p *peer) addSubscription(key subKey, entry *subscription) {
	p.sectionsMu.Lock()
	p.subs[key] = entry
	p.sectionsMu.Unlock()
}

func (p *peer) removeSubscription(key subKey) *subscription {
	p.sectionsMu.Lock()
	defer p.sectionsMu.Unlock()
	entry := p.subs[key]
	delete(p.subs, key)
	return entry
}

func (p *peer) subscriptions() []subKey {
	p.sectionsMu.Lock()
	defer p.sectionsMu.Unlock()
	keys := make([]subKey, 0, len(p.subs))
	for key := range p.subs {
		keys = append(keys, key)
	}
	return keys
}

func (p *peer) slot(purpose string) *webrtc.RTPTransceiver {
	p.sectionsMu.Lock()
	defer p.sectionsMu.Unlock()
	return p.slots[purpose]
}

func (p *peer) setSlot(purpose string, transceiver *webrtc.RTPTransceiver) {
	p.sectionsMu.Lock()
	p.slots[purpose] = transceiver
	p.sectionsMu.Unlock()
}

func (p *peer) isWatching(publisherID int64) bool {
	p.sectionsMu.Lock()
	defer p.sectionsMu.Unlock()
	return p.watching[publisherID]
}

func (p *peer) setWatching(publisherID int64, watching bool) {
	p.sectionsMu.Lock()
	if watching {
		p.watching[publisherID] = true
	} else {
		delete(p.watching, publisherID)
	}
	p.sectionsMu.Unlock()
}

// purposeOf works out what an arriving track carries.
//
// A section the relay opened for a screen is known by the transceiver it was
// opened on, which is the only reliable way to tell a screen's sound from a
// voice: both are Opus, both arrive on a sendonly section of the client's, and
// nothing in the RTP says which is which. Anything arriving on a section the
// relay did not open is the microphone, because the only other thing in the
// client's original offer was the microphone.
func (p *peer) purposeOf(remote *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) string {
	p.sectionsMu.Lock()
	slots := make(map[string]*webrtc.RTPTransceiver, len(p.slots))
	for purpose, transceiver := range p.slots {
		slots[purpose] = transceiver
	}
	p.sectionsMu.Unlock()

	if len(slots) > 0 && receiver != nil {
		for _, transceiver := range p.pc.GetTransceivers() {
			if transceiver.Receiver() != receiver {
				continue
			}
			for purpose, slot := range slots {
				if slot == transceiver {
					return purpose
				}
			}
			break
		}
	}
	if remote.Kind() == webrtc.RTPCodecTypeVideo {
		// A picture arriving on a section the relay did not open is still a
		// picture, and calling it a microphone would be worse than carrying it
		// where a screen goes.
		return protocol.TrackScreen
	}
	return protocol.TrackMic
}

func (p *peer) emit(sig Signal) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if !closed {
		p.out(sig)
	}
}

// describeSections says what each media section of this peer's session is for,
// by media id. It is read after a local description is set, which is the first
// moment the media ids exist.
func (p *peer) describeSections() (map[string]int64, map[string]string) {
	p.sectionsMu.Lock()
	senders := make(map[*webrtc.RTPSender]subKey, len(p.subs))
	for key, entry := range p.subs {
		senders[entry.sender] = key
	}
	slots := make(map[*webrtc.RTPTransceiver]string, len(p.slots))
	for purpose, transceiver := range p.slots {
		slots[transceiver] = purpose
	}
	p.sectionsMu.Unlock()

	tracks := map[string]int64{}
	purposes := map[string]string{}
	for _, transceiver := range p.pc.GetTransceivers() {
		mid := transceiver.Mid()
		if mid == "" {
			continue
		}
		if purpose, ok := slots[transceiver]; ok {
			// A section naming the receiving client itself is the slot it
			// publishes its own screen on.
			tracks[mid] = p.userID
			purposes[mid] = purpose
			continue
		}
		if key, ok := senders[transceiver.Sender()]; ok {
			tracks[mid] = key.publisher
			purposes[mid] = key.purpose
		}
	}
	if len(tracks) == 0 {
		return nil, nil
	}
	return tracks, purposes
}

// renegotiate offers the peer its current set of sections.
//
// The relay is the only side that ever offers after the first exchange: a
// client adds its microphone before its opening offer and never adds anything
// again — even a screen share goes onto a section the relay offered — so there
// is exactly one offerer at any moment and no glare to resolve. A second
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
	tracks, purposes := p.describeSections()
	p.emit(Signal{Kind: protocol.SignalOffer, SDP: local.SDP, Tracks: tracks, Purposes: purposes})
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

// sink is one subscriber's copy of one publisher's media.
type sink struct {
	track *webrtc.TrackLocalStaticRTP
	peer  *peer
}

// publication is one thing one participant sends, and everybody receiving it.
type publication struct {
	owner   *peer
	purpose string
	// video says whether this carries a picture, which decides whether
	// keyframes are something that can be asked for and whether a deafened
	// subscriber still gets it.
	video bool

	mu    sync.RWMutex
	armed bool
	codec webrtc.RTPCodecCapability
	// remote is the arriving track, kept so a keyframe can be asked of the
	// SSRC actually carrying the picture.
	remote *webrtc.TrackRemote
	// generation distinguishes one arriving track from the next on the same
	// section, so a forwarding loop left over from a share that stopped and
	// started again exits rather than writing alongside its replacement.
	generation  uint64
	sinks       map[int64]*sink
	lastRequest time.Time
}

// live reports whether a track has arrived for this publication yet. A peer
// that has connected but is not sending — no microphone, or permission to
// listen and not to speak — never becomes live, and nobody subscribes to it.
func (pub *publication) live() bool {
	pub.mu.RLock()
	defer pub.mu.RUnlock()
	return pub.armed
}

// arm records the arriving track and returns the generation the forwarding
// loop for it should run under.
func (pub *publication) arm(remote *webrtc.TrackRemote) uint64 {
	pub.mu.Lock()
	defer pub.mu.Unlock()
	pub.armed = true
	pub.codec = remote.Codec().RTPCodecCapability
	pub.remote = remote
	pub.generation++
	return pub.generation
}

// disarm marks the publication as carrying nothing, which is what a screen
// share that has stopped is. The section stays; only the claim that something
// is arriving on it goes away.
func (pub *publication) disarm() {
	pub.mu.Lock()
	pub.armed = false
	pub.mu.Unlock()
}

// rearm puts a stopped publication back into service on the track it already
// had, and reports whether there was one to put back.
//
// It exists because stopping a screen share is not the end of a track: the
// client takes its picture off a sender and later puts one back, on the same
// section with the same SSRC, so nothing arrives that could announce itself.
// The announcement is this instead.
func (pub *publication) rearm() bool {
	pub.mu.Lock()
	defer pub.mu.Unlock()
	if pub.armed || pub.remote == nil {
		return false
	}
	pub.armed = true
	return true
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

// requestKeyframe asks the publisher for a frame that can be decoded on its
// own, which is the only thing that lets a new viewer see anything.
//
// The relay cannot make one: it does not decode, so it has nothing to encode
// from. All it can do is pass the request back to the machine that does have
// the picture, which is what a picture loss indication is. Requests are
// collapsed to at most one every keyframeInterval, because several viewers
// arriving at once is exactly when a burst of keyframes would hurt most.
func (pub *publication) requestKeyframe() {
	if !pub.video {
		return
	}
	pub.mu.Lock()
	remote := pub.remote
	now := time.Now()
	if !pub.armed || remote == nil || now.Sub(pub.lastRequest) < keyframeInterval {
		pub.mu.Unlock()
		return
	}
	pub.lastRequest = now
	ssrc := remote.SSRC()
	pub.mu.Unlock()

	if err := pub.owner.pc.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: uint32(ssrc)},
	}); err != nil {
		// A request that could not be sent costs one viewer a moment longer
		// before the picture appears. The next one, from the next arrival or
		// the next report, is a fraction of a second away.
		pub.owner.room.relay.log.Debug("ask for a keyframe",
			slog.Int64("from", pub.owner.userID), slog.Any("error", err))
	}
}

// blocked reports whether this publication is currently allowed to reach
// anybody at all.
//
// The two reasons are different and deliberately not folded together. A
// microphone is stopped by its owner being muted, which is a decision about a
// voice. A screen and its sound are stopped by the share not running, which is
// a decision about a screen — so muting yourself while sharing a video leaves
// the video playing, and stopping the share does not silence you.
func (pub *publication) blocked() bool {
	if pub.purpose == protocol.TrackMic {
		return pub.owner.muted.Load()
	}
	return !pub.owner.streaming.Load()
}

// forward copies RTP from one publisher to everyone receiving it, until the
// track ends. It is the only hot path in the server, so it allocates nothing
// per packet and takes exactly one read lock.
//
// Reading into a buffer of its own is what makes the first half of that true.
// TrackRemote.ReadRTP, the obvious call, allocates a receive buffer and a
// packet on every call — at fifty packets a second per publisher, a room of
// ten people is a thousand allocations a second and most of a megabyte, all of
// it garbage, all of it in the one loop that must not be interrupted by a
// collection, and a shared screen is an order of magnitude more packets again.
// Reading and unmarshalling into the same two values instead costs nothing per
// packet.
//
// It is only safe because every write below is synchronous: Unmarshal points
// packet.Payload straight into buf, and pion's SRTP session marshals header
// and payload into a pooled buffer of its own before encrypting, so nothing
// downstream is still holding either by the time the next packet overwrites
// them. rtp.Header.Unmarshal reuses its CSRC and extension slices for the same
// reason, so the packet is meant to be filled in over and over.
//
// A blocked publication and a deafened subscriber are both handled by not
// writing the packet. The gap that leaves in the sequence numbers is what the
// far end's concealment is for, and it is the same gap a lost packet leaves.
func (pub *publication) forward(remote *webrtc.TrackRemote, generation uint64, log *slog.Logger) {
	owner := pub.owner
	buf := make([]byte, receiveMTU)
	var packet rtp.Packet

	for {
		n, _, err := remote.Read(buf)
		if err != nil {
			// The peer connection closed, which is the ordinary way out.
			return
		}
		pub.mu.RLock()
		stale := pub.generation != generation
		pub.mu.RUnlock()
		if stale {
			// A newer track arrived on this section and has a loop of its own.
			return
		}
		if pub.blocked() {
			continue
		}
		if err := packet.Unmarshal(buf[:n]); err != nil {
			// A packet that will not parse is one packet, not a reason to stop
			// carrying the rest of somebody's media.
			log.Debug("read voice packet",
				slog.Int64("from", owner.userID),
				slog.String("carries", pub.purpose),
				slog.Any("error", err))
			continue
		}

		pub.mu.RLock()
		for _, s := range pub.sinks {
			// Deafening stops sound and nothing else. A shared screen keeps
			// arriving, because somebody who has stopped listening to a
			// meeting is very often still reading the slides.
			if !pub.video && s.peer.deafened.Load() {
				continue
			}
			if err := s.track.WriteRTP(&packet); err != nil {
				// One subscriber's transport going away must not stop the
				// others receiving anything; the peer's own state change is
				// what removes it.
				log.Debug("forward voice packet",
					slog.Int64("from", owner.userID),
					slog.Int64("to", s.peer.userID),
					slog.String("carries", pub.purpose),
					slog.Any("error", err))
			}
		}
		pub.mu.RUnlock()
	}
}

// readRTCP reads what a subscriber sends back about a track it is receiving.
//
// Most of it is receiver reports, which are not acted on but must be read:
// leaving them unread stalls the interceptor chain that produced them. The two
// that are acted on both mean the same thing — the far end has nothing it can
// decode — and are passed back to whoever can do something about it, which is
// never this server.
func readRTCP(sender *webrtc.RTPSender, onKeyframeWanted func()) {
	buf := make([]byte, rtcpBuffer)
	for {
		n, _, err := sender.Read(buf)
		if err != nil {
			return
		}
		if onKeyframeWanted == nil {
			continue
		}
		packets, err := rtcp.Unmarshal(buf[:n])
		if err != nil {
			continue
		}
		for _, packet := range packets {
			switch packet.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				onKeyframeWanted()
			}
		}
	}
}
