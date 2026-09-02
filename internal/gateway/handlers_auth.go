package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/coder/websocket"

	"github.com/aural-chat/aural-server/internal/auth"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// handleAuthGuest hands out a fresh unclaimed identity. The client keeps the
// returned session token; replaying it through auth.token is what makes the
// guest come back as the same person rather than as a new one.
func handleAuthGuest(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.AuthGuestRequest](raw)
	if failure != nil {
		return nil, failure
	}
	if failure := s.beginAuthAttempt(); failure != nil {
		return nil, failure
	}
	if !s.hub.cfg.Registration.AllowGuests {
		return nil, protocol.Errorf(protocol.ErrGuestsDisabled, "this server only accepts registered accounts")
	}
	if failure := checkServerPassword(s.hub.cfg, req.ServerPassword); failure != nil {
		return nil, failure
	}

	nickname := req.Nickname
	if cleanText(nickname) == "" {
		nickname = "Guest"
	}
	nickname, failure = validateNickname(nickname)
	if failure != nil {
		return nil, failure
	}

	user, err := s.hub.st.CreateGuest(ctx, nickname)
	if err != nil {
		return nil, internalError(s, "create guest", err)
	}

	rawToken, tokenHash, err := auth.NewToken()
	if err != nil {
		return nil, internalError(s, "mint token", err)
	}
	if err := s.hub.st.CreateToken(ctx, user.ID, tokenHash, "initial"); err != nil {
		return nil, internalError(s, "store token", err)
	}

	return s.finishAuth(ctx, user, tokenHash, rawToken)
}

// handleAuthToken resumes an identity from a stored session token.
func handleAuthToken(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.AuthTokenRequest](raw)
	if failure != nil {
		return nil, failure
	}
	if failure := s.beginAuthAttempt(); failure != nil {
		return nil, failure
	}
	if req.Token == "" {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "token must not be empty")
	}
	if failure := checkServerPassword(s.hub.cfg, req.ServerPassword); failure != nil {
		return nil, failure
	}

	tokenHash := auth.HashToken(req.Token)
	user, err := s.hub.st.UserByTokenHash(ctx, tokenHash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, protocol.Errorf(protocol.ErrInvalidCredentials, "this session token is no longer valid")
	}
	if err != nil {
		return nil, internalError(s, "resolve token", err)
	}
	// A server that stopped accepting guests keeps its accounts working but
	// turns away the guest identities it handed out earlier.
	if !user.Registered() && !s.hub.cfg.Registration.AllowGuests {
		return nil, protocol.Errorf(protocol.ErrGuestsDisabled, "this server only accepts registered accounts")
	}
	if err := s.hub.st.TouchToken(ctx, tokenHash); err != nil {
		s.log.Warn("touch token", slog.Any("error", err))
	}

	// The token is already on the client, so it is not returned again.
	return s.finishAuth(ctx, user, tokenHash, "")
}

// handleAuthLogin signs in with the credentials an identity was claimed with,
// and mints a token for the device that is signing in. This is the path that
// gets a user back in after losing the client, and the reason claiming an
// identity is worth doing.
func handleAuthLogin(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.AuthLoginRequest](raw)
	if failure != nil {
		return nil, failure
	}
	if failure := s.beginAuthAttempt(); failure != nil {
		return nil, failure
	}
	if failure := checkServerPassword(s.hub.cfg, req.ServerPassword); failure != nil {
		return nil, failure
	}
	if req.Username == "" || req.Password == "" {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "username and password are required")
	}

	user, err := s.hub.st.UserByUsername(ctx, req.Username)
	if errors.Is(err, store.ErrNotFound) {
		// Spend the same work a real verification would, so a wrong username
		// and a wrong password are indistinguishable from the outside.
		auth.BurnVerify(req.Password)
		return nil, protocol.Errorf(protocol.ErrInvalidCredentials, "username or password is wrong")
	}
	if err != nil {
		return nil, internalError(s, "load account", err)
	}
	if user.PasswordHash == nil {
		auth.BurnVerify(req.Password)
		return nil, protocol.Errorf(protocol.ErrInvalidCredentials, "username or password is wrong")
	}

	ok, err := auth.VerifyPassword(*user.PasswordHash, req.Password)
	if err != nil {
		return nil, internalError(s, "verify password", err)
	}
	if !ok {
		return nil, protocol.Errorf(protocol.ErrInvalidCredentials, "username or password is wrong")
	}

	rawToken, tokenHash, err := auth.NewToken()
	if err != nil {
		return nil, internalError(s, "mint token", err)
	}
	if err := s.hub.st.CreateToken(ctx, user.ID, tokenHash, "login"); err != nil {
		return nil, internalError(s, "store token", err)
	}

	return s.finishAuth(ctx, user, tokenHash, rawToken)
}

// handleAuthRegister claims the identity the session is already using. The user
// row is updated in place, so the id, the nickname and everything attached to
// them survive: the guest becomes an account rather than being replaced by one.
func handleAuthRegister(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.AuthRegisterRequest](raw)
	if failure != nil {
		return nil, failure
	}
	cfg := s.hub.cfg
	if !cfg.Registration.Enabled {
		return nil, protocol.Errorf(protocol.ErrRegistrationClosed, "registration is disabled on this server")
	}

	base, _ := s.Permissions()
	if !base.Has(permissions.Register) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to register an account here")
	}
	if s.User().Registered() {
		return nil, protocol.Errorf(protocol.ErrAlreadyRegistered, "this identity already has an account")
	}

	username, failure := validateUsername(cfg, req.Username)
	if failure != nil {
		return nil, failure
	}
	if failure := validatePassword(cfg, req.Password); failure != nil {
		return nil, failure
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		return nil, internalError(s, "hash password", err)
	}

	user, err := s.hub.st.ClaimIdentity(ctx, s.UserID(), username, hash)
	switch {
	case errors.Is(err, store.ErrConflict):
		return nil, protocol.Errorf(protocol.ErrUsernameTaken, "that username is already taken")
	case errors.Is(err, store.ErrAlreadyClaimed):
		return nil, protocol.Errorf(protocol.ErrAlreadyRegistered, "this identity already has an account")
	case err != nil:
		return nil, internalError(s, "claim identity", err)
	}

	// Claiming grants the managed registered role, so the permission cache has
	// to be rebuilt before anyone is told about the change.
	s.setUser(user)
	if err := s.refreshPermissions(ctx); err != nil {
		return nil, internalError(s, "refresh permissions", err)
	}

	view := s.view()
	s.hub.BroadcastUserUpdated(view)
	s.log.Info("identity claimed", slog.Int64("user", user.ID), slog.String("username", username))

	return protocol.AuthRegisterResult{User: view}, nil
}

// handleAuthLogout revokes the session token in use and closes the connection.
func handleAuthLogout(ctx context.Context, s *Session, _ json.RawMessage) (any, *protocol.Error) {
	s.mu.RLock()
	tokenHash := s.tokenHash
	s.mu.RUnlock()

	if tokenHash != "" {
		if err := s.hub.st.DeleteToken(ctx, tokenHash); err != nil {
			return nil, internalError(s, "revoke token", err)
		}
	}
	// Give the reply a chance to reach the client before the socket goes.
	go func() {
		s.Close(websocket.StatusNormalClosure, "logged out")
	}()
	return struct{}{}, nil
}

// --- shared authentication tail ---------------------------------------------

// beginAuthAttempt counts credential attempts on one connection and cuts it off
// once they stop looking like honest mistakes.
func (s *Session) beginAuthAttempt() *protocol.Error {
	if s.Authed() {
		return protocol.Errorf(protocol.ErrConflict, "this connection is already authenticated")
	}

	s.mu.Lock()
	s.authAttempt++
	attempt := s.authAttempt
	s.mu.Unlock()

	if attempt > maxAuthAttempts {
		go s.Close(websocket.StatusPolicyViolation, "too many authentication attempts")
		return protocol.Errorf(protocol.ErrRateLimited, "too many authentication attempts on this connection")
	}
	return nil
}

// finishAuth is the tail every successful authentication shares: resolve the
// permissions, take the identity slot, announce the arrival, and answer with a
// full state snapshot.
func (s *Session) finishAuth(ctx context.Context, user store.User, tokenHash, rawToken string) (any, *protocol.Error) {
	h := s.hub

	explicit, err := h.st.RoleIDsForUser(ctx, user.ID)
	if err != nil {
		return nil, internalError(s, "load roles", err)
	}
	roleIDs := h.EffectiveRoleIDs(user, explicit)
	s.applyIdentity(user, roleIDs, h.BasePermissions(roleIDs), tokenHash)

	// Capacity is checked against the identities already connected, inside the
	// hub's own lock: somebody reconnecting displaces their own session rather
	// than being turned away, and nobody slips past a full server by arriving
	// at the same moment as somebody else. The identity is applied first
	// because taking a place needs one, and withdrawn again if there is none.
	displaced, full := h.Add(s)
	if full {
		s.clearIdentity()
		return nil, protocol.Errorf(protocol.ErrServerFull, "the server is full")
	}
	if displaced != nil {
		displaced.Close(websocket.StatusPolicyViolation, "signed in from another connection")
	}
	if err := h.st.TouchUser(ctx, user.ID); err != nil {
		s.log.Warn("touch user", slog.Any("error", err))
	}

	ready := h.buildReady(s, rawToken)
	view := s.view()
	// Connecting while hidden is announced to nobody: an arrival the rest of
	// the server cannot see is one it must not hear about either.
	if !HidesPresence(view.Status) {
		for _, other := range h.Sessions() {
			if other != s {
				masked := h.MaskUser(other, view)
				other.Send(protocol.Event(protocol.EvUserConnected, protocol.UserEvent{User: masked}))
			}
		}
	}

	s.log.Info("authenticated",
		slog.Int64("user", user.ID),
		slog.String("nickname", user.Nickname),
		slog.Bool("registered", user.Registered()))

	return ready, nil
}

// buildReady assembles the state snapshot for one session. Channels are already
// filtered to what the session may see, and the channel a user sits in is
// hidden from viewers who cannot see that channel.
func (h *Hub) buildReady(s *Session, sessionToken string) protocol.Ready {
	base, _ := s.Permissions()

	visible := h.VisibleChannels(s)
	channels := make([]protocol.Channel, 0, len(visible))
	for _, c := range visible {
		channels = append(channels, channelView(c))
	}

	roles := h.SortedRoles()
	roleViews := make([]protocol.Role, 0, len(roles))
	for _, r := range roles {
		roleViews = append(roleViews, roleView(r))
	}

	sessions := h.Sessions()
	users := make([]protocol.User, 0, len(sessions))
	for _, other := range sessions {
		view := other.view()
		// A hidden user is left out rather than listed as offline: this list
		// holds only connected users, so an entry for one who looks offline
		// could only ever mean somebody hiding.
		if other.UserID() != s.UserID() && HidesPresence(view.Status) {
			continue
		}
		users = append(users, h.MaskUser(s, view))
	}

	return protocol.Ready{
		SessionToken: sessionToken,
		User:         s.view(),
		Users:        users,
		Channels:     channels,
		Roles:        roleViews,
		Permissions:  base.String(),
		Server:       h.serverInfo(),
		ICEServers:   h.iceServers(),
		VoiceStates:  h.voiceStatesFor(s),
	}
}

// internalError logs the cause and returns the opaque error the client sees.
func internalError(s *Session, what string, err error) *protocol.Error {
	s.log.Error(what, slog.Any("error", err))
	return protocol.Errorf(protocol.ErrInternal, "the server failed to "+what)
}
