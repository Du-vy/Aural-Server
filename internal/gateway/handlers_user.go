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

// handleUserUpdate changes a nickname, either the caller's own or, with
// ManageNicknames, somebody else's.
func handleUserUpdate(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.UserUpdateRequest](raw)
	if failure != nil {
		return nil, failure
	}
	if req.Nickname == nil {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "nothing to update")
	}

	targetID := s.UserID()
	if req.UserID != nil {
		targetID = *req.UserID
	}

	base, _ := s.Permissions()
	if targetID == s.UserID() {
		if !base.Has(permissions.ChangeNickname) {
			return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to change your nickname")
		}
	} else {
		if !base.Has(permissions.ManageNicknames) {
			return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to change other nicknames")
		}
		if failure := s.hub.requireOutranks(s, targetID); failure != nil {
			return nil, failure
		}
	}

	nickname, failure := validateNickname(*req.Nickname)
	if failure != nil {
		return nil, failure
	}
	if err := s.hub.st.SetNickname(ctx, targetID, nickname); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such user")
		}
		return nil, internalError(s, "set nickname", err)
	}

	target, ok := s.hub.SessionForUser(targetID)
	if !ok {
		// The user is offline; the new nickname is already persisted and will
		// show up the next time they connect.
		return struct{}{}, nil
	}

	user := target.User()
	user.Nickname = nickname
	target.setUser(user)

	view := target.view()
	s.hub.Broadcast(protocol.Event(protocol.EvUserUpdated, protocol.UserEvent{User: view}))
	return protocol.UserEvent{User: view}, nil
}

// handleUserMove joins, leaves or switches a voice channel. Presence lives only
// in a voice channel: text channels are chosen client side and carry no state.
func handleUserMove(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.UserMoveRequest](raw)
	if failure != nil {
		return nil, failure
	}

	target := s
	moveOther := req.UserID != nil && *req.UserID != s.UserID()
	if moveOther {
		found, ok := s.hub.SessionForUser(*req.UserID)
		if !ok {
			return nil, protocol.Errorf(protocol.ErrNotFound, "that user is not connected")
		}
		target = found
	}

	base, _ := s.Permissions()
	if moveOther {
		if !base.Has(permissions.MoveUsers) {
			return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to move other users")
		}
		if failure := s.hub.requireOutranks(s, target.UserID()); failure != nil {
			return nil, failure
		}
	}

	from := target.ChannelID()

	if req.ChannelID == nil {
		if from == nil {
			return struct{}{}, nil
		}
		target.setChannel(nil)
		s.hub.broadcastUserMoved(target.UserID(), from, nil)
		return struct{}{}, nil
	}

	destID := *req.ChannelID
	dest, ok := s.hub.Channel(destID)
	if !ok {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such channel")
	}
	if dest.Type != protocol.ChannelVoice {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "only voice channels can be joined")
	}
	if from != nil && *from == destID {
		return struct{}{}, nil
	}

	// The permission that matters is the one the person being moved holds:
	// a moderator cannot drop somebody into a channel they may not enter.
	targetBase, targetRoles := target.Permissions()
	destPerms := s.hub.ChannelPermissions(targetBase, targetRoles, destID)
	if !destPerms.Has(permissions.ViewChannel) {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such channel")
	}
	if !destPerms.Has(permissions.Connect) {
		if moveOther {
			return nil, protocol.Errorf(protocol.ErrForbidden, "that user is not allowed into that channel")
		}
		return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed into that channel")
	}

	// A full channel still admits anyone holding MoveUsers, which is what lets
	// a moderator follow a conversation they need to be in.
	if dest.UserLimit > 0 && !base.Has(permissions.MoveUsers) {
		if s.hub.channelPopulation(destID) >= dest.UserLimit {
			return nil, protocol.Errorf(protocol.ErrConflict, "that channel is full")
		}
	}

	target.setChannel(&destID)
	s.hub.broadcastUserMoved(target.UserID(), from, &destID)
	return struct{}{}, nil
}

// handleUserKick disconnects a user. Without bans in v0.1 this is a boot, not a
// block: the user may reconnect immediately.
func handleUserKick(_ context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.UserKickRequest](raw)
	if failure != nil {
		return nil, failure
	}

	base, _ := s.Permissions()
	if !base.Has(permissions.KickUsers) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to kick users")
	}
	if req.UserID == s.UserID() {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "you cannot kick yourself")
	}

	target, ok := s.hub.SessionForUser(req.UserID)
	if !ok {
		return nil, protocol.Errorf(protocol.ErrNotFound, "that user is not connected")
	}
	if failure := s.hub.requireOutranks(s, req.UserID); failure != nil {
		return nil, failure
	}

	reason := cleanText(req.Reason)
	if reason == "" {
		reason = "kicked"
	}
	s.log.Info("user kicked",
		slog.Int64("by", s.UserID()),
		slog.Int64("user", req.UserID),
		slog.String("reason", reason))

	go target.Close(websocket.StatusPolicyViolation, reason)
	return struct{}{}, nil
}

// handleServerClaimAdmin redeems the one-time owner token printed on the first
// start. It is how a freshly installed server gets its first administrator
// without shipping a default password.
func handleServerClaimAdmin(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.ClaimAdminRequest](raw)
	if failure != nil {
		return nil, failure
	}
	if req.Token == "" {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "token must not be empty")
	}

	wantHash, err := s.hub.st.Meta(ctx, store.MetaOwnerTokenHash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "the owner token has already been used")
	}
	if err != nil {
		return nil, internalError(s, "read owner token", err)
	}
	if auth.HashToken(req.Token) != wantHash {
		return nil, protocol.Errorf(protocol.ErrInvalidCredentials, "that owner token is not valid")
	}

	adminRoleID := s.hub.AdminRoleID()
	if adminRoleID == 0 {
		return nil, internalError(s, "find the admin role", errors.New("managed admin role is missing"))
	}
	if err := s.hub.st.AssignRole(ctx, s.UserID(), adminRoleID); err != nil {
		return nil, internalError(s, "grant the admin role", err)
	}
	// Burning the token is what makes it one-time.
	if err := s.hub.st.DeleteMeta(ctx, store.MetaOwnerTokenHash); err != nil {
		return nil, internalError(s, "burn the owner token", err)
	}
	if err := s.refreshPermissions(ctx); err != nil {
		return nil, internalError(s, "refresh permissions", err)
	}

	view := s.view()
	s.hub.Broadcast(protocol.Event(protocol.EvUserUpdated, protocol.UserEvent{User: view}))
	s.log.Info("server ownership claimed", slog.Int64("user", s.UserID()))

	return protocol.UserEvent{User: view}, nil
}

// handleServerUpdate renames or re-describes the server and writes the change
// back to the configuration file, so it survives a restart.
func handleServerUpdate(_ context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.ServerUpdateRequest](raw)
	if failure != nil {
		return nil, failure
	}

	base, _ := s.Permissions()
	if !base.Has(permissions.ManageServer) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to manage this server")
	}
	if req.Name == nil && req.Description == nil {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "nothing to update")
	}

	var name, description string
	if req.Name != nil {
		name = cleanText(*req.Name)
		if name == "" {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "server name must not be empty")
		}
		if len([]rune(name)) > 64 {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "server name must be at most 64 characters")
		}
	}
	if req.Description != nil {
		description = cleanText(*req.Description)
		if len([]rune(description)) > 512 {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "server description must be at most 512 characters")
		}
	}

	if err := s.hub.updateServerIdentity(req.Name != nil, name, req.Description != nil, description); err != nil {
		return nil, internalError(s, "save the configuration", err)
	}

	info := s.hub.serverInfo()
	s.hub.Broadcast(protocol.Event(protocol.EvServerUpdated, protocol.ServerUpdatedEvent{Server: info}))
	return protocol.ServerUpdatedEvent{Server: info}, nil
}

// --- helpers ----------------------------------------------------------------

// requireOutranks enforces the role hierarchy: you may only act on somebody
// whose highest role sits below your own. Without it, anyone holding
// ManageRoles could promote themselves past the people who granted it.
func (h *Hub) requireOutranks(actor *Session, targetID int64) *protocol.Error {
	if actor.UserID() == targetID {
		return nil
	}

	target, ok := h.SessionForUser(targetID)
	if !ok {
		return protocol.Errorf(protocol.ErrNotFound, "that user is not connected")
	}

	_, actorRoles := actor.Permissions()
	_, targetRoles := target.Permissions()
	if h.HighestRolePosition(actorRoles) <= h.HighestRolePosition(targetRoles) {
		return protocol.Errorf(protocol.ErrForbidden, "that user holds a role at or above your own")
	}
	return nil
}

// channelPopulation counts the sessions currently sitting in a channel.
func (h *Hub) channelPopulation(channelID int64) int {
	n := 0
	for _, s := range h.Sessions() {
		if id := s.ChannelID(); id != nil && *id == channelID {
			n++
		}
	}
	return n
}

// broadcastUserMoved reports a channel change, hiding either end of the move
// from viewers who may not see that channel.
func (h *Hub) broadcastUserMoved(userID int64, from, to *int64) {
	for _, s := range h.Sessions() {
		visibleFrom := h.maskChannel(s, from)
		visibleTo := h.maskChannel(s, to)
		if visibleFrom == nil && visibleTo == nil && s.UserID() != userID {
			continue
		}
		s.Send(protocol.Event(protocol.EvUserMoved, protocol.UserMovedEvent{
			UserID: userID,
			From:   visibleFrom,
			To:     visibleTo,
		}))
	}
}

// maskChannel drops a channel reference the session is not allowed to see.
func (h *Hub) maskChannel(s *Session, channelID *int64) *int64 {
	if channelID == nil || !h.SessionCanView(s, *channelID) {
		return nil
	}
	return channelID
}
