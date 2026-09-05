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

// Bounds on one activity report.
//
// They are deliberately generous for text and tight for pictures. Text is what
// a member list draws and is cut to fit long before any of these; the pictures
// are the part that is broadcast to every connected member each time a track
// changes, so they are what actually decides whether this feature costs the
// server anything. An image ceiling of 24 KiB fits a 128-pixel cover as a data
// URL with room to spare, and the two ceilings together stay well inside the
// 64 KiB frame limit with the rest of the payload alongside them.
const (
	activityTextMax  = 128
	activityURLMax   = 512
	activityImageMax = 24 * 1024
	activityIconMax  = 8 * 1024
	// activityPartyMax bounds the party counter. It is a display number, not a
	// membership: nothing here looks anybody up.
	activityPartyMax = 1_000_000
)

// activityTypes is the set of verbs a report may carry.
//
// It is closed on the way in and open on the way out: a server only accepts
// what it knows, while a client is expected to render a type it does not
// recognise as "playing" rather than drop the activity. That is what lets a
// later revision add one without every older client losing the feature.
var activityTypes = map[string]bool{
	"playing":   true,
	"listening": true,
}

// handleUserActivity reports what the caller is doing outside Aural.
//
// Nothing here is written down. The report lives on the session and is gone
// when the connection is, which is the only honest lifetime for it: it
// describes a machine that this socket is the last evidence of. That is also
// why there is no permission to check and no way to set it for somebody else —
// it is not a claim about a person, it is a reading from their computer, and
// only their computer can take it.
func handleUserActivity(_ context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.UserActivityRequest](raw)
	if failure != nil {
		return nil, failure
	}

	// Clearing is exempt from the limiter on purpose. The report that matters
	// most is the one saying the music stopped: dropping it would leave a
	// member listening to a track that ended, which is worse than anything
	// letting it through can cost.
	if req.Activity != nil && !s.activities.allow() {
		return nil, protocol.Errorf(protocol.ErrRateLimited,
			"you are reporting activity too quickly")
	}

	activity, failure := validateActivity(req.Activity, s.hub.cfg.Activity.Assets)
	if failure != nil {
		return nil, failure
	}

	// Unchanged reports are the common case: a source that polls sends the
	// same answer every time nothing happened, and forwarding those would
	// redraw every member list on the server for nothing.
	if sameActivity(s.Activity(), activity) {
		return protocol.UserEvent{User: s.hub.MaskUser(s, s.view())}, nil
	}

	s.setActivity(activity)
	view := s.view()
	// The status has not changed, so this is an ordinary update — and for a
	// hidden user it is no update at all, which is the point: an activity
	// changes on its own and would otherwise keep announcing somebody who has
	// chosen to look offline.
	s.hub.BroadcastUserUpdated(view)
	return protocol.UserEvent{User: view}, nil
}

// validateActivity bounds one report, or passes nil through as the clear it is.
//
// assets says whether this server will fetch game artwork on its members'
// behalf. It is threaded down here rather than checked at the edge because the
// answer decides what a picture reference turns into, and a reference this
// server will not resolve has to be dropped on the way in — not broadcast for
// every client to fail to load.
func validateActivity(a *protocol.Activity, assets bool) (*protocol.Activity, *protocol.Error) {
	if a == nil {
		return nil, nil
	}

	clean := protocol.Activity{
		Type:      strings.TrimSpace(a.Type),
		Name:      strings.TrimSpace(a.Name),
		Details:   strings.TrimSpace(a.Details),
		State:     strings.TrimSpace(a.State),
		StartedAt: a.StartedAt,
		EndsAt:    a.EndsAt,
		Image:     strings.TrimSpace(a.Image),
		Icon:      strings.TrimSpace(a.Icon),
		ImageText: strings.TrimSpace(a.ImageText),
		IconText:  strings.TrimSpace(a.IconText),
	}

	if !activityTypes[clean.Type] {
		return nil, protocol.Errorf(protocol.ErrBadRequest,
			"activity type must be one of: listening, playing")
	}
	if clean.Name == "" {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "activity name is required")
	}
	for field, value := range map[string]string{
		"name":      clean.Name,
		"details":   clean.Details,
		"state":     clean.State,
		"imageText": clean.ImageText,
		"iconText":  clean.IconText,
	} {
		if len([]rune(value)) > activityTextMax {
			return nil, protocol.Errorf(protocol.ErrBadRequest,
				fmt.Sprintf("activity %s must be at most %d characters", field, activityTextMax))
		}
	}

	// A timestamp is a counter a client runs, so all that has to be true of it
	// is that it is a time and that the pair reads forwards.
	if clean.StartedAt < 0 || clean.EndsAt < 0 {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "activity timestamps must not be negative")
	}
	if clean.StartedAt > 0 && clean.EndsAt > 0 && clean.EndsAt < clean.StartedAt {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "activity ends before it starts")
	}

	image, failure := validateActivityImage(clean.Image, "image", activityImageMax, assets)
	if failure != nil {
		return nil, failure
	}
	clean.Image = image
	icon, failure := validateActivityImage(clean.Icon, "icon", activityIconMax, assets)
	if failure != nil {
		return nil, failure
	}
	clean.Icon = icon

	if a.Party != nil {
		party := *a.Party
		if party.Size < 0 || party.Max < 0 {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "activity party counts must not be negative")
		}
		if party.Size > activityPartyMax || party.Max > activityPartyMax {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "activity party counts are out of range")
		}
		// A party of nobody is what an absent party already says.
		if party.Size > 0 || party.Max > 0 {
			clean.Party = &party
		}
	}

	return &clean, nil
}

// validateActivityImage bounds one picture reference, and resolves the one
// form that is not a picture at all.
//
// Three forms are allowed, because the sources genuinely differ:
//
//	data:image/  a media session, which hands over decoded bytes that exist
//	             nowhere but the machine they came from
//	https://     a game naming art that is already hosted somewhere
//	asset:       a game naming art by a key that means something only to
//	             Discord, where the game's own application keeps it
//
// The third is the interesting one. It is not loadable by anybody, and turning
// it into something that is means asking Discord what the key refers to. That
// request is made by this server rather than by every member's client — see
// handlers_activity.go for why — so what goes out on the wire is a path back
// here, and the fetch happens once, when somebody's client asks for it.
//
// Plain http is not among the forms. This is content every member's client
// will load, and a plaintext fetch turns a member list into a way of telling
// somebody's network what they are listening to.
func validateActivityImage(value, field string, max int, assets bool) (string, *protocol.Error) {
	if value == "" {
		return "", nil
	}
	switch {
	case strings.HasPrefix(value, "https://"):
		if len(value) > activityURLMax {
			return "", protocol.Errorf(protocol.ErrBadRequest,
				fmt.Sprintf("activity %s url must be at most %d characters", field, activityURLMax))
		}
	case strings.HasPrefix(value, "data:image/"):
		if len(value) > max {
			return "", protocol.Errorf(protocol.ErrTooLarge,
				fmt.Sprintf("activity %s must be at most %d bytes", field, max))
		}
	case strings.HasPrefix(value, "asset:"):
		// An operator who has switched artwork off has said this server does
		// not call Discord. The reference is dropped rather than refused: the
		// activity itself is fine, and losing the text over a picture nobody
		// asked for would be the wrong trade.
		if !assets {
			return "", nil
		}
		app, key, found := strings.Cut(strings.TrimPrefix(value, "asset:"), "/")
		if !found || !activityAppPattern.MatchString(app) || !activityAssetPattern.MatchString(key) {
			return "", protocol.Errorf(protocol.ErrBadRequest,
				fmt.Sprintf("activity %s names a malformed asset", field))
		}
		return activityAssetPrefix + app + "/" + key, nil
	default:
		return "", protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("activity %s must be an https, data:image or asset reference", field))
	}
	return value, nil
}

// sameActivity reports whether two reports say the same thing, so that a source
// which polls does not redraw every member list on the server for no change.
func sameActivity(a, b *protocol.Activity) bool {
	if a == nil || b == nil {
		return a == b
	}
	switch {
	case a.Party == nil && b.Party == nil:
	case a.Party == nil || b.Party == nil:
		return false
	case *a.Party != *b.Party:
		return false
	}
	x, y := *a, *b
	x.Party, y.Party = nil, nil
	return x == y
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

	// 4. Optionally purge message history. Shared with ban, which differs in
	// what it does to the identity and not at all in what it does to what the
	// identity wrote.
	s.purgeUserContent(ctx, req.UserID, deleteMode)

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

	entry := auditTarget(protocol.AuditTargetUser, targetID, targetNickname)
	entry.Action = protocol.AuditUserKick
	entry.Reason = reason
	if deleteMode != "none" {
		entry.Changes = []store.AuditChange{{Key: "deletedMessages", After: deleteMode}}
	}
	s.hub.audit(ctx, s, entry)

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

	entry := auditTarget(protocol.AuditTargetServer, s.UserID(), s.User().Nickname)
	entry.Action = protocol.AuditOwnerClaim
	s.hub.audit(ctx, s, entry)

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
	if req.Name == nil && req.Description == nil && req.Icon == nil && req.KlipyAPIKey == nil && req.Voice == nil {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "nothing to update")
	}
	// The only icon this op accepts is no icon. A path the client chose would
	// name a file it does not own, so setting one is an upload and this field
	// is the way back to having none.
	if req.Icon != nil && strings.TrimSpace(*req.Icon) != "" {
		return nil, protocol.Errorf(protocol.ErrBadRequest,
			"the server icon is set by uploading one, not by naming a path")
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

	if req.Icon != nil {
		oldKey, oldSize, err := s.hub.SetServerIcon("", "", 0)
		if err != nil {
			return nil, internalError(s, "save the configuration", err)
		}
		s.hub.DropServerIcon(oldKey, oldSize)
	}

	var voiceChanged bool
	if req.Name != nil || req.Description != nil || req.KlipyAPIKey != nil || newVoice != nil {
		var err error
		voiceChanged, err = s.hub.updateServerIdentity(
			req.Name != nil, name,
			req.Description != nil, description,
			req.KlipyAPIKey != nil, klipyApiKey,
			newVoice,
		)
		if err != nil {
			return nil, internalError(s, "save the configuration", err)
		}
	}

	info := s.hub.serverInfo()
	s.hub.Broadcast(protocol.Event(protocol.EvServerUpdated, protocol.ServerUpdatedEvent{Server: info}))

	entry := auditTarget(protocol.AuditTargetServer, 0, info.Name)
	entry.Action = protocol.AuditServerUpdate
	if req.Name != nil {
		entry.Changes = changed(entry.Changes, "name", "", name)
	}
	if req.Description != nil {
		entry.Changes = append(entry.Changes, store.AuditChange{Key: "description", After: "changed"})
	}
	if req.Icon != nil {
		entry.Changes = append(entry.Changes, store.AuditChange{Key: "icon", After: "removed"})
	}
	if req.KlipyAPIKey != nil {
		// The credential itself never reaches the log. That it was replaced is
		// the part a moderator reading this needs to know.
		entry.Changes = append(entry.Changes, store.AuditChange{Key: "klipyApiKey", After: "changed"})
	}
	if voiceChanged {
		entry.Changes = append(entry.Changes, store.AuditChange{Key: "voice", After: "reconfigured"})
	}
	s.hub.audit(context.Background(), s, entry)

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
