package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

const (
	// outboundBuffer is how many frames may queue for one client before the
	// connection is considered too slow to keep.
	outboundBuffer = 128
	// readLimit caps one inbound frame. Nothing legitimate in v1 comes close.
	readLimit = 64 * 1024
	// heartbeatInterval is how often the server pings an idle connection.
	heartbeatInterval = 30 * time.Second
	// heartbeatTimeout is how long a pong may take before the peer is dropped.
	//
	// It bounds the read loop as much as the network: a pong is a control
	// frame, and control frames are only processed by a call to Read, so a
	// request being handled on the read loop is time the heartbeat cannot be
	// answered in. The slow reads are dispatched off that loop for exactly
	// this reason, and the margin here covers what is left.
	heartbeatTimeout = 15 * time.Second
	// authDeadline is how long a connection may stay unauthenticated. Such a
	// connection has no identity, so it counts against nothing that max_users
	// bounds, and it will keep answering pings for as long as it is left to.
	authDeadline = 30 * time.Second
	// maxAuthAttempts limits credential guessing on a single connection.
	maxAuthAttempts = 6
	// maxSlowInFlight bounds the reads one session may have running off the
	// read loop at once.
	//
	// It is a ceiling, not a rate: the token buckets are what pace these, and
	// this only exists so that one connection cannot start goroutines without
	// bound. So it is set well above anything a client does on purpose —
	// opening a channel, searching, and jumping into a result all at once is
	// three or four — and a session that reaches it is not one to keep waiting.
	maxSlowInFlight = 16
	// writeTimeout bounds a single frame write to a stalled peer.
	writeTimeout = 10 * time.Second
	// messageBurst and messagesPerSecond throttle posting. The burst is what a
	// person catching up on a conversation actually sends; the refill rate is
	// what stops a script from filling everybody's scrollback.
	messageBurst      = 8
	messagesPerSecond = 1.5
	// searchBurst and searchesPerSecond throttle searching, which is the one
	// read that walks a channel's whole history rather than an index of it.
	// The burst covers refining a query a few times in a row; the refill is
	// what stops a script from turning that into a load generator.
	searchBurst       = 6
	searchesPerSecond = 1
	// signalBurst and signalsPerSecond throttle voice signalling. ICE
	// candidates arrive in a burst at the start of a call and then stop, so the
	// bucket is deep and the refill is modest: it is sized to let a call be set
	// up twice over and to stop the socket being used as a message bus.
	signalBurst      = 96
	signalsPerSecond = 12
	// speakingBurst and speakingPerSecond throttle the speaking indicator,
	// which is the one voice frame a client sends continuously. Two transitions
	// a second is faster than speech alternates; the burst covers a stutter.
	speakingBurst     = 12
	speakingPerSecond = 2
)

// sessionVoice is the audio half of a session's state.
//
// channelID is the channel a media session is open in, and is zero when there
// is none. It is held separately from the session's channel because the two
// genuinely differ: somebody in a voice channel with no microphone sits in the
// channel with no media session at all.
//
// The mute and deafen flags come in pairs because they have different owners.
// selfMute is the participant's own choice and is theirs to undo; mute is
// imposed by a moderator and is not. Folding them into one flag would let
// unmuting yourself undo being muted by somebody else.
type sessionVoice struct {
	channelID int64
	connected bool
	selfMute  bool
	selfDeaf  bool
	mute      bool
	deaf      bool
	speaking  bool
}

// Session is one client connection and the identity behind it.
type Session struct {
	ID  int64
	hub *Hub
	log *slog.Logger

	conn *websocket.Conn
	out  chan protocol.Envelope

	// messages throttles the chat ops, which are the only ones a client sends
	// in bulk during normal use.
	messages *rateLimiter
	// searches throttles searching, which costs far more per request than
	// anything else a client can ask for.
	searches *rateLimiter
	// signals and speaking throttle the two voice ops a client sends without
	// being asked to.
	signals  *rateLimiter
	speaking *rateLimiter

	// slowSlots bounds the reads running off the read loop. It is a channel
	// rather than a counter so that taking a slot is a non-blocking try: a
	// session that has filled it is told so rather than made to queue.
	slowSlots chan struct{}

	closeOnce sync.Once
	closed    chan struct{}

	mu          sync.RWMutex
	authed      bool
	user        store.User
	roleIDs     []int64
	base        permissions.Permission
	channelID   *int64
	tokenHash   string
	authAttempt int
	voice       sessionVoice
}

func newSession(id int64, hub *Hub, conn *websocket.Conn, log *slog.Logger) *Session {
	return &Session{
		ID:        id,
		hub:       hub,
		log:       log.With(slog.Int64("session", id)),
		conn:      conn,
		out:       make(chan protocol.Envelope, outboundBuffer),
		slowSlots: make(chan struct{}, maxSlowInFlight),
		closed:    make(chan struct{}),
		messages:  newRateLimiter(messageBurst, messagesPerSecond),
		searches:  newRateLimiter(searchBurst, searchesPerSecond),
		signals:   newRateLimiter(signalBurst, signalsPerSecond),
		speaking:  newRateLimiter(speakingBurst, speakingPerSecond),
	}
}

// --- accessors --------------------------------------------------------------

// UserID is the identity behind the session, or zero before authentication.
func (s *Session) UserID() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.user.ID
}

// User snapshots the stored user record.
func (s *Session) User() store.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.user
}

// Authed reports whether the session has completed authentication.
func (s *Session) Authed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.authed
}

// Permissions returns the server-wide mask and the effective role ids.
func (s *Session) Permissions() (permissions.Permission, []int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.base, s.roleIDs
}

// ChannelID is the channel the session currently sits in, if any.
func (s *Session) ChannelID() *int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.channelID == nil {
		return nil
	}
	id := *s.channelID
	return &id
}

func (s *Session) setChannel(id *int64) {
	s.mu.Lock()
	s.channelID = id
	s.mu.Unlock()
}

// --- voice ------------------------------------------------------------------

// voiceSnapshot copies the audio half of the session.
func (s *Session) voiceSnapshot() sessionVoice {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.voice
}

// openVoiceSession records that media is up in a channel. It returns false when
// a session was already open in a different channel, which cannot happen from a
// well-behaved client and must not be allowed to leave two rooms believing they
// hold the same person.
func (s *Session) openVoiceSession(channelID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.voice.connected && s.voice.channelID != channelID {
		return false
	}
	s.voice.channelID = channelID
	s.voice.connected = true
	return true
}

// clearVoiceSession closes the media session in a channel and reports whether
// there was one. Passing zero closes whichever session is open.
//
// The moderated mute and deafen flags survive it: they belong to the identity
// for as long as it is connected, and leaving a channel is not an appeal.
func (s *Session) clearVoiceSession(channelID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.voice.connected {
		return false
	}
	if channelID != 0 && s.voice.channelID != channelID {
		return false
	}
	s.voice.channelID = 0
	s.voice.connected = false
	s.voice.speaking = false
	return true
}

// voiceChannel is the channel a media session is open in, or zero.
func (s *Session) voiceChannel() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.voice.connected {
		return 0
	}
	return s.voice.channelID
}

// setSelfVoice applies the participant's own mute and deafen. Deafening mutes
// as well: a channel you cannot hear is one there is no point talking into,
// and every other client works that way.
func (s *Session) setSelfVoice(mute, deaf *bool) sessionVoice {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mute != nil {
		s.voice.selfMute = *mute
	}
	if deaf != nil {
		s.voice.selfDeaf = *deaf
		if *deaf {
			s.voice.selfMute = true
		}
	}
	return s.voice
}

// setModeratedVoice applies a moderator's mute and deafen.
func (s *Session) setModeratedVoice(mute, deaf *bool) sessionVoice {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mute != nil {
		s.voice.mute = *mute
	}
	if deaf != nil {
		s.voice.deaf = *deaf
		if *deaf {
			s.voice.mute = true
		}
	}
	return s.voice
}

// setSpeaking records a speaking transition and reports whether it was one.
// A client that repeats itself is answered without a broadcast.
func (s *Session) setSpeaking(speaking bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.voice.speaking == speaking {
		return false
	}
	s.voice.speaking = speaking
	return true
}

func (s *Session) setUser(u store.User) {
	s.mu.Lock()
	s.user = u
	s.mu.Unlock()
}

// applyIdentity records who the session is and what it may do.
func (s *Session) applyIdentity(u store.User, roleIDs []int64, base permissions.Permission, tokenHash string) {
	s.mu.Lock()
	s.authed = true
	s.user = u
	s.roleIDs = roleIDs
	s.base = base
	if tokenHash != "" {
		s.tokenHash = tokenHash
	}
	s.mu.Unlock()
}

// clearIdentity withdraws an identity that was applied but could not be
// registered, which happens when the server turns out to be full. The session
// goes back to being unauthenticated and may try again — as a different
// account, or once somebody leaves.
func (s *Session) clearIdentity() {
	s.mu.Lock()
	s.authed = false
	s.user = store.User{}
	s.roleIDs = nil
	s.base = permissions.None
	s.tokenHash = ""
	s.mu.Unlock()
}

// refreshPermissions recomputes the cached role set and mask from the database.
// It is called after anything that could change them: a role edit, a grant, a
// registration.
func (s *Session) refreshPermissions(ctx context.Context) error {
	explicit, err := s.hub.st.RoleIDsForUser(ctx, s.UserID())
	if err != nil {
		return err
	}
	roleIDs := s.hub.EffectiveRoleIDs(s.User(), explicit)
	base := s.hub.PermissionsFor(s.UserID(), roleIDs)

	s.mu.Lock()
	s.roleIDs, s.base = roleIDs, base
	s.mu.Unlock()
	return nil
}

// view renders the session as a protocol user.
func (s *Session) view() protocol.User {
	owner := s.hub.IsOwner(s.UserID())

	s.mu.RLock()
	defer s.mu.RUnlock()
	return userView(s.user, s.roleIDs, s.channelID, true, owner)
}

// --- transport --------------------------------------------------------------

// Send queues a frame. A client too slow to drain its queue is disconnected
// rather than allowed to grow it without bound.
func (s *Session) Send(env protocol.Envelope) {
	select {
	case <-s.closed:
	case s.out <- env:
	default:
		s.log.Warn("client fell behind, dropping connection", slog.Int64("user", s.UserID()))
		s.Close(websocket.StatusPolicyViolation, "outbound queue overflow")
	}
}

// Close shuts the session down once, whichever goroutine gets there first.
func (s *Session) Close(code websocket.StatusCode, reason string) {
	s.closeOnce.Do(func() {
		close(s.closed)
		// Best effort: the peer may already be gone.
		_ = s.conn.Close(code, truncateReason(reason))
	})
}

// truncateReason keeps a close reason inside the 123 byte protocol limit.
func truncateReason(reason string) string {
	if len(reason) <= 123 {
		return reason
	}
	return reason[:120] + "..."
}

// serve runs the connection until either side closes it.
func (s *Session) serve(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.writePump(ctx)
	}()
	go func() {
		defer wg.Done()
		s.watchAuthDeadline(ctx)
	}()

	s.readPump(ctx)
	cancel()
	wg.Wait()
}

// watchAuthDeadline drops a connection that never says who it is.
//
// Until a session authenticates it holds no identity, so it is counted by
// nothing max_users bounds while still holding a buffer, four rate limiters
// and a goroutine. Left alone it would keep all of that for as long as it kept
// answering pings, which is indefinitely.
func (s *Session) watchAuthDeadline(ctx context.Context) {
	timer := time.NewTimer(authDeadline)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-s.closed:
	case <-timer.C:
		if !s.Authed() {
			s.log.Debug("closing a connection that never authenticated")
			s.Close(websocket.StatusPolicyViolation, "authentication timed out")
		}
	}
}

// writePump owns the connection for writing. Every outbound frame goes through
// it, which is what makes concurrent Send calls safe.
func (s *Session) writePump(ctx context.Context) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case env := <-s.out:
			raw, err := json.Marshal(env)
			if err != nil {
				s.log.Error("encode frame", slog.String("op", env.Op), slog.Any("error", err))
				continue
			}
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err = s.conn.Write(writeCtx, websocket.MessageText, raw)
			cancel()
			if err != nil {
				s.Close(websocket.StatusInternalError, "write failed")
				return
			}
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
			err := s.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				s.Close(websocket.StatusGoingAway, "heartbeat timeout")
				return
			}
		}
	}
}

// readPump decodes inbound frames and dispatches them.
func (s *Session) readPump(ctx context.Context) {
	s.conn.SetReadLimit(readLimit)

	for {
		kind, raw, err := s.conn.Read(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) && websocket.CloseStatus(err) == -1 {
				s.log.Debug("read ended", slog.Any("error", err))
			}
			s.Close(websocket.StatusNormalClosure, "")
			return
		}
		if kind != websocket.MessageText {
			s.Close(websocket.StatusUnsupportedData, "expected text frames")
			return
		}

		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			s.Send(protocol.Failure("", protocol.Errorf(protocol.ErrBadRequest, "frame is not valid JSON")))
			continue
		}
		s.dispatch(ctx, env)
	}
}

// dispatch routes one request to its handler and replies with the result.
//
// A slow route runs in a goroutine of its own. The reason is the heartbeat: a
// pong is a control frame, and this library only processes control frames
// inside a call to Read, so anything handled on the read loop is time in which
// the connection cannot answer a ping. A history walk that outlasts
// heartbeatTimeout would be dropped as an unresponsive peer, which is the
// worst possible answer to a server that is merely busy.
//
// Only side-effect-free reads qualify. Everything that writes stays on the
// read loop, in the order the client sent it.
func (s *Session) dispatch(ctx context.Context, env protocol.Envelope) {
	route, known := routes[env.Op]
	if !known {
		s.Send(protocol.Failure(env.ID, protocol.Errorf(protocol.ErrBadRequest, "unknown op "+env.Op)))
		return
	}
	if route.needsAuth && !s.Authed() {
		s.Send(protocol.Failure(env.ID, protocol.Errorf(protocol.ErrUnauthorized, "authenticate first")))
		return
	}

	if !route.slow {
		s.reply(ctx, route, env)
		return
	}

	select {
	case s.slowSlots <- struct{}{}:
	default:
		s.Send(protocol.Failure(env.ID, protocol.Errorf(protocol.ErrRateLimited,
			"too many reads are already running on this connection")))
		return
	}
	go func() {
		defer func() { <-s.slowSlots }()
		s.reply(ctx, route, env)
	}()
}

// reply runs one handler and sends whatever it produced.
func (s *Session) reply(ctx context.Context, route route, env protocol.Envelope) {
	payload, failure := route.fn(ctx, s, env.Data)
	if failure != nil {
		s.Send(protocol.Failure(env.ID, failure))
		return
	}
	s.Send(protocol.Result(env.ID, payload))
}

// decode unmarshals a request payload, turning a malformed body into the
// protocol error the client expects. An absent payload decodes as the zero
// value, which is what makes every field of a request optional by default.
func decode[T any](raw json.RawMessage) (T, *protocol.Error) {
	var out T
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, protocol.Errorf(protocol.ErrBadRequest, "payload does not match the shape this op expects")
	}
	return out, nil
}

// route is one entry of the dispatch table.
type route struct {
	needsAuth bool
	// slow marks a read that may take longer than the heartbeat allows and is
	// therefore run off the read loop. Only ops with no side effects may set
	// it: running them out of order with the rest is what it costs.
	slow bool
	fn   func(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error)
}

// routes is the whole client-facing surface of the protocol.
var routes = map[string]route{
	protocol.OpAuthGuest:    {needsAuth: false, fn: handleAuthGuest},
	protocol.OpAuthToken:    {needsAuth: false, fn: handleAuthToken},
	protocol.OpAuthLogin:    {needsAuth: false, fn: handleAuthLogin},
	protocol.OpAuthRegister: {needsAuth: true, fn: handleAuthRegister},
	protocol.OpAuthLogout:   {needsAuth: true, fn: handleAuthLogout},

	protocol.OpServerClaimAdmin: {needsAuth: true, fn: handleServerClaimAdmin},
	protocol.OpServerUpdate:     {needsAuth: true, fn: handleServerUpdate},

	protocol.OpUserUpdate: {needsAuth: true, fn: handleUserUpdate},
	protocol.OpUserMove:   {needsAuth: true, fn: handleUserMove},
	protocol.OpUserKick:   {needsAuth: true, fn: handleUserKick},

	protocol.OpChannelCreate: {needsAuth: true, fn: handleChannelCreate},
	protocol.OpChannelUpdate: {needsAuth: true, fn: handleChannelUpdate},
	protocol.OpChannelDelete: {needsAuth: true, fn: handleChannelDelete},

	protocol.OpMessageSend: {needsAuth: true, fn: handleMessageSend},
	// The two reads that walk history rather than an index of it, and the only
	// requests here that can outlast a heartbeat.
	protocol.OpMessageHistory: {needsAuth: true, slow: true, fn: handleMessageHistory},
	protocol.OpMessageSearch:  {needsAuth: true, slow: true, fn: handleMessageSearch},
	protocol.OpMessageEdit:    {needsAuth: true, fn: handleMessageEdit},
	protocol.OpMessageDelete:  {needsAuth: true, fn: handleMessageDelete},

	// Private conversations. Reading one walks its history rather than an
	// index of it, exactly as a channel does, so it is dispatched off the read
	// loop for the same reason.
	protocol.OpDMList:    {needsAuth: true, slow: true, fn: handleDMList},
	protocol.OpDMHistory: {needsAuth: true, slow: true, fn: handleDMHistory},
	protocol.OpDMSend:    {needsAuth: true, fn: handleDMSend},
	protocol.OpDMEdit:    {needsAuth: true, fn: handleDMEdit},
	protocol.OpDMDelete:  {needsAuth: true, fn: handleDMDelete},
	protocol.OpDMRead:    {needsAuth: true, fn: handleDMRead},

	// Webhook management. Listing walks the channel tree the caller may see
	// and then one query, so it stays on the read loop with the rest.
	protocol.OpWebhookList:   {needsAuth: true, fn: handleWebhookList},
	protocol.OpWebhookCreate: {needsAuth: true, fn: handleWebhookCreate},
	protocol.OpWebhookUpdate: {needsAuth: true, fn: handleWebhookUpdate},
	protocol.OpWebhookDelete: {needsAuth: true, fn: handleWebhookDelete},

	protocol.OpRoleCreate:   {needsAuth: true, fn: handleRoleCreate},
	protocol.OpRoleUpdate:   {needsAuth: true, fn: handleRoleUpdate},
	protocol.OpRoleDelete:   {needsAuth: true, fn: handleRoleDelete},
	protocol.OpRoleAssign:   {needsAuth: true, fn: handleRoleAssign},
	protocol.OpRoleUnassign: {needsAuth: true, fn: handleRoleUnassign},

	protocol.OpVoiceConnect:  {needsAuth: true, fn: handleVoiceConnect},
	protocol.OpVoiceLeave:    {needsAuth: true, fn: handleVoiceLeave},
	protocol.OpVoiceSignal:   {needsAuth: true, fn: handleVoiceSignal},
	protocol.OpVoiceState:    {needsAuth: true, fn: handleVoiceState},
	protocol.OpVoiceModerate: {needsAuth: true, fn: handleVoiceModerate},
	protocol.OpVoiceSpeaking: {needsAuth: true, fn: handleVoiceSpeaking},
}
