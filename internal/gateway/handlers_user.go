package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/coder/websocket"

	"github.com/aural-chat/aural-server/internal/auth"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

func validateStatus(status string) (string, *protocol.Error) {
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	// "offline" is not offered: a connected client asking to look offline means
	// invisible, and letting both spellings through leaves two names for one
	// state to keep in step everywhere presence is decided.
	case "online", "idle", "dnd", "invisible":
		return status, nil
	default:
		return "", protocol.Errorf(protocol.ErrBadRequest, "invalid status: must be online, idle, dnd, or invisible")
	}
}

// validateMediaURL accepts only a reference to a file this server stores.
//
// An avatar is fetched by every client that renders the member list, so a URL
// pointing anywhere else would have every member's browser call on a host of
// the setter's choosing — an address book of the whole server, collected by
// whoever asked for it, from a chat that is self-hosted precisely so that it
// answers to nobody outside. The upload endpoints are how a picture gets here.
func (h *Hub) validateMediaURL(raw string, name string) (string, *protocol.Error) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) > 2048 {
		return "", protocol.Errorf(protocol.ErrBadRequest, fmt.Sprintf("%s URL is too long", name))
	}
	rest, found := strings.CutPrefix(trimmed, uploadPrefix)
	if !found {
		return "", protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("a %s must be a file uploaded to this server", name))
	}
	key, _, found := strings.Cut(rest, "/")
	if !found || key == "" {
		return "", protocol.Errorf(protocol.ErrBadRequest, fmt.Sprintf("that %s URL is malformed", name))
	}
	// Path is what rejects a key this server could not have minted, which is
	// the same check that stops a crafted download URL escaping the directory.
	if files := h.Files(); files != nil {
		if _, err := files.Path(key); err != nil {
			return "", protocol.Errorf(protocol.ErrBadRequest, fmt.Sprintf("that %s URL is malformed", name))
		}
	}
	return trimmed, nil
}

// validateMediaField turns an avatar or banner field into the column update it
// stands for: nil to leave it alone, a pointer to nil to clear it, and a
// pointer to a checked URL to set it.
func (h *Hub) validateMediaField(field *string, name string) (**string, *protocol.Error) {
	if field == nil {
		return nil, nil
	}
	if strings.TrimSpace(*field) == "" {
		var cleared *string
		return &cleared, nil
	}
	value, failure := h.validateMediaURL(*field, name)
	if failure != nil {
		return nil, failure
	}
	set := &value
	return &set, nil
}

// handleUserUpdate changes profile attributes (nickname, status, customStatus, avatar, banner).
func handleUserUpdate(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.UserUpdateRequest](raw)
	if failure != nil {
		return nil, failure
	}
	if req.Nickname == nil && req.Status == nil && req.CustomStatus == nil && req.Avatar == nil && req.Banner == nil {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "nothing to update")
	}

	targetID := s.UserID()
	if req.UserID != nil {
		targetID = *req.UserID
	}

	isSelf := targetID == s.UserID()
	base, _ := s.Permissions()

	var validatedNickname *string
	if req.Nickname != nil {
		if isSelf {
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
		validatedNickname = &nickname
	}

	if !isSelf && (req.Status != nil || req.CustomStatus != nil || req.Avatar != nil || req.Banner != nil) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you may only update your own profile details")
	}

	var validatedStatus *string
	if req.Status != nil {
		st, failure := validateStatus(*req.Status)
		if failure != nil {
			return nil, failure
		}
		validatedStatus = &st
	}

	var validatedCustomStatus *string
	if req.CustomStatus != nil {
		cs := strings.TrimSpace(*req.CustomStatus)
		if len([]rune(cs)) > 128 {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "custom status must be at most 128 characters")
		}
		validatedCustomStatus = &cs
	}

	validatedAvatar, failure := s.hub.validateMediaField(req.Avatar, "avatar")
	if failure != nil {
		return nil, failure
	}
	validatedBanner, failure := s.hub.validateMediaField(req.Banner, "banner")
	if failure != nil {
		return nil, failure
	}

	updatedUser, err := s.hub.st.UpdateProfile(ctx, targetID, validatedNickname, validatedAvatar, validatedBanner, validatedStatus, validatedCustomStatus)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such user")
		}
		return nil, internalError(s, "update user profile", err)
	}

	// Removing a picture has to take its file with it. Nothing points at it
	// once the column is empty, so leaving it on disk spends the server's quota
	// on something no client can ever ask for.
	for kind, cleared := range map[string]bool{
		store.KindAvatar: validatedAvatar != nil && *validatedAvatar == nil,
		store.KindBanner: validatedBanner != nil && *validatedBanner == nil,
	} {
		if !cleared {
			continue
		}
		orphaned, err := s.hub.st.ClearProfileMedia(ctx, targetID, kind)
		if err != nil {
			s.log.Warn("clear profile media", slog.String("kind", kind), slog.Any("error", err))
			continue
		}
		s.hub.RemoveProfileMedia(orphaned)
	}

	target, ok := s.hub.SessionForUser(targetID)
	if !ok {
		return struct{}{}, nil
	}

	// The status the rest of the server last saw, read before the session is
	// updated: it is what decides whether this change reads as a departure, an
	// arrival, or an ordinary update to everybody else.
	was := target.User().Status
	target.setUser(updatedUser)
	view := target.view()
	s.hub.BroadcastUserPresence(was, view)
	// The caller is not always the subject: a moderator renaming somebody must
	// not learn from the reply what the broadcast just took care to hide.
	return protocol.UserEvent{User: s.hub.MaskUser(s, view)}, nil
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
	s.hub.BroadcastUserUpdated(view)
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
	if req.Name == nil && req.Description == nil && req.KlipyAPIKey == nil {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "nothing to update")
	}

	var name, description, klipyApiKey string
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
	if req.KlipyAPIKey != nil {
		klipyApiKey = strings.TrimSpace(*req.KlipyAPIKey)
		if len(klipyApiKey) > 256 {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "klipy api key is too long")
		}
	}

	if err := s.hub.updateServerIdentity(req.Name != nil, name, req.Description != nil, description, req.KlipyAPIKey != nil, klipyApiKey); err != nil {
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
	// A hidden user carries no channel in anybody else's view, so a move
	// between channels is theirs alone to hear about.
	hidden := false
	if mover, ok := h.SessionForUser(userID); ok {
		hidden = HidesPresence(mover.User().Status)
	}
	for _, s := range h.Sessions() {
		if hidden && s.UserID() != userID {
			continue
		}
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
