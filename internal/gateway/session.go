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
	heartbeatTimeout = 10 * time.Second
	// maxAuthAttempts limits credential guessing on a single connection.
	maxAuthAttempts = 6
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
)

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
}

func newSession(id int64, hub *Hub, conn *websocket.Conn, log *slog.Logger) *Session {
	return &Session{
		ID:       id,
		hub:      hub,
		log:      log.With(slog.Int64("session", id)),
		conn:     conn,
		out:      make(chan protocol.Envelope, outboundBuffer),
		closed:   make(chan struct{}),
		messages: newRateLimiter(messageBurst, messagesPerSecond),
		searches: newRateLimiter(searchBurst, searchesPerSecond),
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

// refreshPermissions recomputes the cached role set and mask from the database.
// It is called after anything that could change them: a role edit, a grant, a
// registration.
func (s *Session) refreshPermissions(ctx context.Context) error {
	explicit, err := s.hub.st.RoleIDsForUser(ctx, s.UserID())
	if err != nil {
		return err
	}
	roleIDs := s.hub.EffectiveRoleIDs(s.User(), explicit)
	base := s.hub.BasePermissions(roleIDs)

	s.mu.Lock()
	s.roleIDs, s.base = roleIDs, base
	s.mu.Unlock()
	return nil
}

// view renders the session as a protocol user.
func (s *Session) view() protocol.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return userView(s.user, s.roleIDs, s.channelID, true)
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
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.writePump(ctx)
	}()

	s.readPump(ctx)
	cancel()
	wg.Wait()
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
	fn        func(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error)
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

	protocol.OpMessageSend:    {needsAuth: true, fn: handleMessageSend},
	protocol.OpMessageHistory: {needsAuth: true, fn: handleMessageHistory},
	protocol.OpMessageSearch:  {needsAuth: true, fn: handleMessageSearch},
	protocol.OpMessageEdit:    {needsAuth: true, fn: handleMessageEdit},
	protocol.OpMessageDelete:  {needsAuth: true, fn: handleMessageDelete},

	protocol.OpRoleCreate:   {needsAuth: true, fn: handleRoleCreate},
	protocol.OpRoleUpdate:   {needsAuth: true, fn: handleRoleUpdate},
	protocol.OpRoleDelete:   {needsAuth: true, fn: handleRoleDelete},
	protocol.OpRoleAssign:   {needsAuth: true, fn: handleRoleAssign},
	protocol.OpRoleUnassign: {needsAuth: true, fn: handleRoleUnassign},
}
