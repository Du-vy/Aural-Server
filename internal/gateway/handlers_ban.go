package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/coder/websocket"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// maxBanReason bounds the note a moderator leaves. It is shown back to the
// person refused, so it is a sentence rather than an essay.
const maxBanReason = 512

// maxBanDuration is a year. Anything longer is what permanent is for, and a
// date beyond it is nearly always a unit mistake.
const maxBanDuration int64 = 366 * 24 * 3600

// handleBanList reads the ban list.
func handleBanList(ctx context.Context, s *Session, _ json.RawMessage) (any, *protocol.Error) {
	base, _ := s.Permissions()
	if !base.Has(permissions.BanUsers) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to see the ban list")
	}

	bans, err := s.hub.st.ListBans(ctx, 0)
	if err != nil {
		return nil, internalError(s, "read the ban list", err)
	}

	at := time.Now().Unix()
	views := make([]protocol.Ban, 0, len(bans))
	for _, b := range bans {
		views = append(views, banView(b, at))
	}
	return protocol.BanListResult{Bans: views}, nil
}

// handleBanCreate refuses somebody the server.
//
// A ban is not a stronger kick. A kick ends one connection; this records a
// standing decision and attaches to it every handle the server knows for that
// identity — the account, the addresses it has connected from, the machine it
// declared — so that coming straight back as a fresh guest does not work.
//
// The identity itself is then removed, exactly as a kick removes it. That is
// deliberate on a server that hands out identities to anybody who asks: the
// account is the cheapest of the three handles and the only one the banned
// person can replace for free, so keeping it would buy a name in a member list
// and nothing else.
func handleBanCreate(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.BanCreateRequest](raw)
	if failure != nil {
		return nil, failure
	}

	base, _ := s.Permissions()
	if !base.Has(permissions.BanUsers) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to ban users")
	}
	if req.UserID <= 0 {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "a ban names a user")
	}
	if req.UserID == s.UserID() {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "you cannot ban yourself")
	}
	if s.hub.IsOwner(req.UserID) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "the owner of this server cannot be banned")
	}
	if failure := s.hub.requireOutranksUser(ctx, s, req.UserID); failure != nil {
		return nil, failure
	}

	reason := cleanText(req.Reason)
	if len([]rune(reason)) > maxBanReason {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "that reason is too long")
	}
	if req.Duration < 0 || req.Duration > maxBanDuration {
		return nil, protocol.Errorf(protocol.ErrBadRequest,
			"a ban lasts at most a year; leave the duration out for a permanent one")
	}

	// The target's details are read before anything is written: the row is
	// about to be deleted, and the list has to be readable afterwards.
	target, err := s.hub.st.UserByID(ctx, req.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, protocol.Errorf(protocol.ErrNotFound, "that user does not exist")
	}
	if err != nil {
		return nil, internalError(s, "load that user", err)
	}
	registered := target.Registered()

	matchIP := req.MatchIP == nil || *req.MatchIP
	matchDevice := req.MatchDevice == nil || *req.MatchDevice

	matches := []store.BanMatch{{Kind: store.MatchUser, Value: formatID(req.UserID)}}
	matches = append(matches, s.hub.banHandles(ctx, s, req.UserID, matchIP, matchDevice)...)

	actorID := s.UserID()
	ban := store.Ban{
		UserID:        &req.UserID,
		UserNickname:  target.Nickname,
		UserUsername:  target.Username,
		ActorID:       &actorID,
		ActorNickname: s.User().Nickname,
		Reason:        reason,
		Matches:       matches,
	}
	if req.Duration > 0 {
		expires := time.Now().Unix() + req.Duration
		ban.ExpiresAt = &expires
	}

	created, err := s.hub.st.CreateBan(ctx, ban)
	if err != nil {
		return nil, internalError(s, "record the ban", err)
	}
	created.Matches = matches

	// The history goes before the identity does: the purge is keyed by user id,
	// and deleting the row would take the messages' authorship with it.
	s.purgeUserContent(ctx, req.UserID, req.DeleteMessages)

	if session, online := s.hub.SessionForUser(req.UserID); online {
		closeReason := reason
		if closeReason == "" {
			closeReason = "banned"
		}
		go session.Close(websocket.StatusPolicyViolation, closeReason)
	}
	if err := s.hub.st.DeleteTokensForUser(ctx, req.UserID); err != nil {
		s.log.Warn("revoke tokens of a banned user", slog.Any("error", err))
	}
	if err := s.hub.st.DeleteUser(ctx, req.UserID); err != nil {
		s.log.Warn("remove a banned identity", slog.Any("error", err))
	}
	// Anybody else caught by the same handles goes too: banning the person
	// behind two connections and leaving one of them open is not a ban.
	s.hub.enforceBan(created)

	s.hub.Broadcast(protocol.Event(protocol.EvUserRemoved, protocol.UserRemovedEvent{
		UserID: req.UserID,
		Reason: reason,
	}))

	view := banView(created, time.Now().Unix())
	s.hub.BroadcastTo(protocol.Event(protocol.EvBanCreated, protocol.BanEvent{Ban: view}),
		func(other *Session) bool {
			otherBase, _ := other.Permissions()
			return otherBase.Has(permissions.BanUsers)
		})

	entry := auditTarget(protocol.AuditTargetUser, req.UserID, target.Nickname)
	entry.Action = protocol.AuditUserBan
	entry.Reason = reason
	if ban.ExpiresAt != nil {
		entry.Changes = []store.AuditChange{{
			Key:   "expires",
			After: time.Unix(*ban.ExpiresAt, 0).UTC().Format(time.RFC3339),
		}}
	}
	s.hub.audit(ctx, s, entry)

	s.log.Info("user banned",
		slog.Int64("by", s.UserID()), slog.Int64("user", req.UserID),
		slog.String("reason", reason), slog.Bool("registered", registered),
		slog.Int("handles", len(matches)))

	return protocol.BanEvent{Ban: view}, nil
}

// handleBanDelete lifts a ban.
func handleBanDelete(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.BanDeleteRequest](raw)
	if failure != nil {
		return nil, failure
	}

	base, _ := s.Permissions()
	if !base.Has(permissions.BanUsers) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to lift bans")
	}

	existing, err := s.hub.st.BanByID(ctx, req.BanID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such ban")
	}
	if err != nil {
		return nil, internalError(s, "read that ban", err)
	}
	if err := s.hub.st.DeleteBan(ctx, req.BanID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such ban")
		}
		return nil, internalError(s, "lift the ban", err)
	}

	event := protocol.BanDeletedEvent{BanID: req.BanID}
	s.hub.BroadcastTo(protocol.Event(protocol.EvBanDeleted, event), func(other *Session) bool {
		otherBase, _ := other.Permissions()
		return otherBase.Has(permissions.BanUsers)
	})

	entry := auditTarget(protocol.AuditTargetUser, valueOrZero(existing.UserID), existing.UserNickname)
	entry.Action = protocol.AuditUserUnban
	s.hub.audit(ctx, s, entry)

	s.log.Info("ban lifted", slog.Int64("by", s.UserID()), slog.Int64("ban", req.BanID))
	return event, nil
}

// banHandles collects the addresses and devices a ban should reach.
//
// Both the live connection and the recorded history are asked, because they
// answer different questions: the session knows where the person being banned
// is right now, including a guest whose identity was never written down, and
// the table knows where they were the last few times.
//
// A handle the moderator issuing the ban, or the owner, is also sitting behind
// is dropped. An address is shared by a household, a university and a phone
// network, so the day somebody bans a troublemaker from their own network is
// the day that handle would lock out the person who issued the ban. Losing it
// costs the ban some reach; keeping it costs the server its staff.
func (h *Hub) banHandles(ctx context.Context, actor *Session, userID int64, matchIP, matchDevice bool) []store.BanMatch {
	if !matchIP && !matchDevice {
		return nil
	}

	protected := map[string]bool{}
	shield := func(s *Session) {
		if s == nil {
			return
		}
		protected[store.MatchIP+"\x00"+s.Peer()] = true
		if device := s.Device(); device != "" {
			protected[store.MatchDevice+"\x00"+device] = true
		}
	}
	shield(actor)
	if owner, online := h.SessionForUser(h.OwnerUserID()); online {
		shield(owner)
	}

	seen := map[string]bool{}
	out := make([]store.BanMatch, 0, maxBanMarks*2)
	add := func(kind, value string) {
		if value == "" || len(out) >= maxBanMarks*2 {
			return
		}
		key := kind + "\x00" + value
		if seen[key] || protected[key] {
			return
		}
		seen[key] = true
		out = append(out, store.BanMatch{Kind: kind, Value: value})
	}

	if session, online := h.SessionForUser(userID); online {
		if matchIP {
			add(store.MatchIP, session.Peer())
		}
		if matchDevice {
			add(store.MatchDevice, session.Device())
		}
	}

	marks, err := h.st.IdentityMarks(ctx, userID, maxBanMarks)
	if err != nil {
		// A ban that reaches fewer handles is still a ban. Refusing to issue
		// one because the history could not be read would be worse.
		h.log.Warn("read where a banned identity has connected from", slog.Any("error", err))
		return out
	}
	for _, mark := range marks {
		if matchIP {
			add(store.MatchIP, mark.IP)
		}
		if matchDevice {
			add(store.MatchDevice, mark.Device)
		}
	}
	return out
}

// purgeUserContent deletes what somebody wrote, as far back as mode asks for.
// It is shared by kick and ban, which differ in what they do to the identity
// and not at all in what they do to its history.
//
// Failures are logged rather than reported: the moderation has happened, and a
// history that could not be swept is not a reason to tell the moderator their
// action failed.
func (s *Session) purgeUserContent(ctx context.Context, userID int64, mode string) {
	cutoff := purgeCutoff(mode)
	if cutoff < 0 {
		return
	}

	// Posts go first. A post is a title in front of a thread, so deleting only
	// the messages would leave entries standing with nothing in them; deleting
	// the post takes its whole thread, including other people's comments.
	purgedPosts, err := s.hub.st.DeletePostsByUser(ctx, userID, cutoff)
	if err != nil {
		s.log.Warn("purge posts", slog.Int64("user", userID), slog.Any("error", err))
	}
	for _, pt := range purgedPosts {
		event := protocol.PostDeletedEvent{PostID: pt.ID, ChannelID: pt.ChannelID}
		s.hub.BroadcastChannelEvent(protocol.Event(protocol.EvPostDeleted, event), pt.ChannelID)
	}

	deleted, err := s.hub.st.DeleteMessagesByUser(ctx, userID, cutoff)
	if err != nil {
		s.log.Warn("purge messages", slog.Int64("user", userID), slog.Any("error", err))
		return
	}
	for _, dt := range deleted {
		event := protocol.MessageDeletedEvent{MessageID: dt.ID, ChannelID: dt.ChannelID}
		s.hub.BroadcastChannelEvent(protocol.Event(protocol.EvMessageDeleted, event), dt.ChannelID)
	}
}

// purgeCutoff turns the window a moderator picked into the timestamp before
// which nothing is deleted. A negative result means "delete nothing".
func purgeCutoff(mode string) int64 {
	now := time.Now().Unix()
	switch mode {
	case "1d", "24h":
		return now - 24*3600
	case "7d":
		return now - 7*24*3600
	case "30d", "1m":
		return now - 30*24*3600
	case "all":
		return 0
	default:
		return -1
	}
}

// valueOrZero reads through an optional id, which is what an audit target that
// may no longer exist amounts to.
func valueOrZero(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
}
