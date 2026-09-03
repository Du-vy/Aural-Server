package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/aural-chat/aural-server/internal/auth"
	"github.com/aural-chat/aural-server/internal/config"
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

// validateDMPrivacy checks who somebody is willing to hear from privately.
func validateDMPrivacy(raw string) (string, *protocol.Error) {
	privacy := strings.ToLower(strings.TrimSpace(raw))
	switch privacy {
	case store.DMEveryone, store.DMRegistered, store.DMNone:
		return privacy, nil
	default:
		return "", protocol.Errorf(protocol.ErrBadRequest,
			"direct message privacy must be everyone, registered or none")
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
	if req.Nickname == nil && req.Status == nil && req.CustomStatus == nil &&
		req.Avatar == nil && req.Banner == nil && req.DMPrivacy == nil {
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
			// The offline-capable check: a member who is not connected is on
			// show in the list like everybody else, and renaming somebody is
			// one of the few things worth doing to them while they are away.
			if failure := s.hub.requireOutranksUser(ctx, s, targetID); failure != nil {
				return nil, failure
			}
		}
		nickname, failure := validateNickname(*req.Nickname)
		if failure != nil {
			return nil, failure
		}
		validatedNickname = &nickname
	}

	if !isSelf && (req.Status != nil || req.CustomStatus != nil || req.Avatar != nil ||
		req.Banner != nil || req.DMPrivacy != nil) {
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

	var validatedDMPrivacy *string
	if req.DMPrivacy != nil {
		privacy, failure := validateDMPrivacy(*req.DMPrivacy)
		if failure != nil {
			return nil, failure
		}
		validatedDMPrivacy = &privacy
	}

	validatedAvatar, failure := s.hub.validateMediaField(req.Avatar, "avatar")
	if failure != nil {
		return nil, failure
	}
	validatedBanner, failure := s.hub.validateMediaField(req.Banner, "banner")
	if failure != nil {
		return nil, failure
	}

	updatedUser, err := s.hub.st.UpdateProfile(ctx, targetID, validatedNickname,
		validatedAvatar, validatedBanner, validatedStatus, validatedCustomStatus, validatedDMPrivacy)
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
		// Only a moderator gets here — the fields a user may set on themselves
		// all need a connection — and what they changed is on show against the
		// member's offline entry, so everybody is told.
		view, err := s.hub.offlineMemberView(ctx, updatedUser)
		if err != nil {
			return nil, internalError(s, "load that user", err)
		}
		s.hub.BroadcastMemberUpdated(view)
		return protocol.UserEvent{User: view}, nil
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
		// Audio goes first, while the channel it belongs to is still set: the
		// voice state that announces the departure is resolved from it.
		s.hub.leaveVoice(target, *from, false)
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

	if from != nil {
		s.hub.leaveVoice(target, *from, false)
	}
	target.setChannel(&destID)
	s.hub.broadcastUserMoved(target.UserID(), from, &destID)
	return struct{}{}, nil
}

// handleUserKick disconnects a user and revokes their membership.
// Supports both online and offline users, logs the kick to the database with
// reason, and optionally purges message history according to DeleteMessages.
func handleUserKick(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
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

	// 1. Check role hierarchy (works for both online and offline users)
	if failure := s.hub.requireOutranksUser(ctx, s, req.UserID); failure != nil {
		return nil, failure
	}

	// 2. Fetch target user info (from live session or database)
	var targetNickname string
	var targetUsername *string
	targetSession, online := s.hub.SessionForUser(req.UserID)
	if online {
		targetUser := targetSession.User()
		targetNickname = targetUser.Nickname
		targetUsername = targetUser.Username
	} else {
		u, err := s.hub.st.UserByID(ctx, req.UserID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "that user does not exist")
		}
		if err != nil {
			return nil, protocol.Errorf(protocol.ErrInternal, "failed to fetch user")
		}
		targetNickname = u.Nickname
		targetUsername = u.Username
	}

	reason := cleanText(req.Reason)
	if reason == "" {
		reason = "kicked"
	}

	actorNickname := s.User().Nickname
	actorID := s.UserID()
	targetID := req.UserID

	// 3. Record kick in database
	deleteMode := req.DeleteMessages
	if deleteMode == "" {
		deleteMode = "none"
	}
	_ = s.hub.st.RecordKick(ctx, store.KickRecord{
		UserID:          &targetID,
		UserNickname:    targetNickname,
		UserUsername:    targetUsername,
		ActorID:         &actorID,
		ActorNickname:   actorNickname,
		Reason:          reason,
		DeletedMessages: deleteMode,
		CreatedAt:       time.Now().Unix(),
	})

	// 4. Optionally purge message history
	var cutoff int64 = -1
	switch deleteMode {
	case "1d", "24h":
		cutoff = time.Now().Unix() - 24*3600
	case "7d":
		cutoff = time.Now().Unix() - 7*24*3600
	case "30d", "1m":
		cutoff = time.Now().Unix() - 30*24*3600
	case "all":
		cutoff = 0
	}

	if cutoff >= 0 {
		deletedTargets, err := s.hub.st.DeleteMessagesByUser(ctx, req.UserID, cutoff)
		if err == nil && len(deletedTargets) > 0 {
			for _, dt := range deletedTargets {
				event := protocol.MessageDeletedEvent{MessageID: dt.ID, ChannelID: dt.ChannelID}
				s.hub.BroadcastChannelEvent(protocol.Event(protocol.EvMessageDeleted, event), dt.ChannelID)
			}
		}
	}

	s.log.Info("user kicked",
		slog.Int64("by", s.UserID()),
		slog.Int64("user", req.UserID),
		slog.String("reason", reason),
		slog.String("delete_messages", deleteMode),
		slog.Bool("online", online))

	// 5. If user is online, disconnect their live session
	if online && targetSession != nil {
		go targetSession.Close(websocket.StatusPolicyViolation, reason)
	}

	// 6. Revoke session tokens and remove user from database
	_ = s.hub.st.DeleteTokensForUser(ctx, req.UserID)
	_ = s.hub.st.DeleteUser(ctx, req.UserID)

	// 7. Broadcast user.removed to all connected clients
	s.hub.Broadcast(protocol.Event(protocol.EvUserRemoved, protocol.UserRemovedEvent{
		UserID: req.UserID,
		Reason: reason,
	}))

	return struct{}{}, nil
}

// handleServerClaimAdmin redeems the one-time owner token printed on the first
// start. It is how a freshly installed server gets its owner without shipping a
// default password.
//
// It grants no role. Ownership is a property of the identity, as in Discord:
// the owner holds every permission and outranks every role without appearing
// in the role editor at all, which is what keeps it from being edited away.
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

	previous := s.hub.OwnerUserID()
	if err := s.hub.st.SetOwnerUserID(ctx, s.UserID()); err != nil {
		return nil, internalError(s, "record the owner", err)
	}
	// Burning the token is what makes it one-time.
	if err := s.hub.st.DeleteMeta(ctx, store.MetaOwnerTokenHash); err != nil {
		return nil, internalError(s, "burn the owner token", err)
	}
	if err := s.hub.ReloadOwner(ctx); err != nil {
		return nil, internalError(s, "reload the owner", err)
	}
	if err := s.refreshPermissions(ctx); err != nil {
		return nil, internalError(s, "refresh permissions", err)
	}

	view := s.view()
	s.hub.BroadcastUserUpdated(view)
	s.log.Info("server ownership claimed", slog.Int64("user", s.UserID()))

	// Ownership uncovers every restricted channel at once, so the claimer is
	// sent a fresh snapshot rather than left with an interface that has not
	// caught up with what they may now see.
	ready, err := s.hub.buildReady(ctx, s, "")
	if err != nil {
		return nil, internalError(s, "build the state snapshot", err)
	}
	s.Send(protocol.Event(protocol.EvReady, ready))

	// A replacement token issued with -new-owner-token hands the server over,
	// which is how an operator recovers from a lost owner account. The former
	// owner keeps whatever roles they hold and nothing more, so both what they
	// may do and everybody's copy of them have to be rebuilt.
	if previous != 0 && previous != s.UserID() {
		s.log.Info("server ownership transferred",
			slog.Int64("from", previous), slog.Int64("to", s.UserID()))
		s.hub.resyncAll(ctx)
		s.hub.evictFromUnreachableChannels()
		if former, online := s.hub.SessionForUser(previous); online {
			s.hub.BroadcastMemberUpdated(former.view())
		} else if user, err := s.hub.st.UserByID(ctx, previous); err == nil {
			if formerView, err := s.hub.offlineMemberView(ctx, user); err == nil {
				s.hub.BroadcastMemberUpdated(formerView)
			}
		}
	}

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
	if req.Name == nil && req.Description == nil && req.KlipyAPIKey == nil && req.Voice == nil {
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

	var newVoice *config.Voice
	if req.Voice != nil {
		current := s.hub.voiceConfig()
		current.Enabled = req.Voice.Enabled
		current.Mode = req.Voice.Mode
		current.SampleRate = req.Voice.SampleRate
		current.Bitrate = req.Voice.Bitrate
		current.MinBitrate = req.Voice.MinBitrate
		current.MaxBitrate = req.Voice.MaxBitrate
		current.FEC = req.Voice.FEC
		current.DTX = req.Voice.DTX
		current.Stereo = req.Voice.Stereo
		current.MaxParticipants = req.Voice.MaxParticipants
		// The same rules the configuration file goes through, so a setting that
		// would not survive a restart cannot be reached from a client either.
		if err := current.Validate(); err != nil {
			return nil, protocol.Errorf(protocol.ErrBadRequest, err.Error())
		}
		newVoice = &current
	}

	voiceChanged, err := s.hub.updateServerIdentity(
		req.Name != nil, name,
		req.Description != nil, description,
		req.KlipyAPIKey != nil, klipyApiKey,
		newVoice,
	)
	if err != nil {
		return nil, internalError(s, "save the configuration", err)
	}

	info := s.hub.serverInfo()
	s.hub.Broadcast(protocol.Event(protocol.EvServerUpdated, protocol.ServerUpdatedEvent{Server: info}))

	if voiceChanged {
		// The Opus parameters live in SDP that was negotiated before this, and
		// there is no way to edit that in place. Everybody starts over, which
		// is a second of silence and the same path a host handover takes.
		s.hub.resetAllVoice(protocol.ResetConfigChanged)
		s.log.Info("audio plane reconfigured", slog.Int64("by", s.UserID()))
	}
	return protocol.ServerUpdatedEvent{Server: info}, nil
}

// --- helpers ----------------------------------------------------------------

// requireOutranks enforces the hierarchy: you may only act on somebody who
// stands below you. Without it, anyone holding ManageRoles could promote
// themselves past the people who granted it. The owner stands above every
// role, so nobody may act on them and they may act on anybody.
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
	if h.RankOf(actor.UserID(), actorRoles) <= h.RankOf(targetID, targetRoles) {
		return protocol.Errorf(protocol.ErrForbidden, "that user stands at or above you")
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
