package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// handleChannelCreate adds a channel to the tree.
func handleChannelCreate(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.ChannelCreateRequest](raw)
	if failure != nil {
		return nil, failure
	}
	if failure := validateChannelType(req.Type); failure != nil {
		return nil, failure
	}
	name, failure := validateChannelName(req.Name)
	if failure != nil {
		return nil, failure
	}
	topic, failure := validateTopic(req.Topic)
	if failure != nil {
		return nil, failure
	}
	if req.UserLimit < 0 || req.UserLimit > 1000 {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "user limit must be between 0 and 1000")
	}

	// Creating inside a category is governed by that category, so a moderator
	// scoped to one part of the tree cannot create channels elsewhere.
	if failure := s.hub.requireChannelPermission(s, req.ParentID, permissions.ManageChannels); failure != nil {
		return nil, failure
	}
	if failure := s.hub.validateParent(req.Type, req.ParentID); failure != nil {
		return nil, failure
	}

	channel := store.Channel{
		Name:      name,
		Type:      req.Type,
		Topic:     topic,
		ParentID:  req.ParentID,
		UserLimit: req.UserLimit,
	}
	if req.Position != nil {
		channel.Position = *req.Position
	}

	created, err := s.hub.st.CreateChannel(ctx, channel)
	if err != nil {
		return nil, internalError(s, "create the channel", err)
	}
	if err := s.hub.ReloadChannels(ctx); err != nil {
		return nil, internalError(s, "reload the channel tree", err)
	}

	view := channelView(created)
	s.hub.BroadcastChannelEvent(protocol.Event(protocol.EvChannelCreated, protocol.ChannelEvent{Channel: view}), created.ID)
	s.log.Info("channel created", slog.Int64("channel", created.ID), slog.String("name", created.Name))

	return protocol.ChannelEvent{Channel: view}, nil
}

// handleChannelUpdate edits a channel. Permission overwrites are part of this
// op, and editing them additionally needs ManageRoles.
func handleChannelUpdate(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.ChannelUpdateRequest](raw)
	if failure != nil {
		return nil, failure
	}

	current, ok := s.hub.Channel(req.ChannelID)
	if !ok {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such channel")
	}
	if failure := s.hub.requireChannelPermission(s, &req.ChannelID, permissions.ManageChannels); failure != nil {
		return nil, failure
	}

	// A move or an overwrite edit can change who may see the channel, which
	// means the affected clients need a fresh snapshot rather than a patch.
	needsResync := false

	if req.Name != nil {
		name, failure := validateChannelName(*req.Name)
		if failure != nil {
			return nil, failure
		}
		current.Name = name
	}
	if req.Topic != nil {
		topic, failure := validateTopic(*req.Topic)
		if failure != nil {
			return nil, failure
		}
		current.Topic = topic
	}
	if req.Position != nil {
		current.Position = *req.Position
	}
	if req.UserLimit != nil {
		if *req.UserLimit < 0 || *req.UserLimit > 1000 {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "user limit must be between 0 and 1000")
		}
		current.UserLimit = *req.UserLimit
	}
	if req.ParentID != nil {
		parent := *req.ParentID
		if failure := s.hub.validateParent(current.Type, parent); failure != nil {
			return nil, failure
		}
		if failure := s.hub.rejectParentCycle(current.ID, parent); failure != nil {
			return nil, failure
		}
		if failure := s.hub.requireChannelPermission(s, parent, permissions.ManageChannels); failure != nil {
			return nil, failure
		}
		current.ParentID = parent
		needsResync = true
	}

	if req.Overwrites != nil {
		base, _ := s.Permissions()
		if !base.Has(permissions.ManageRoles) {
			return nil, protocol.Errorf(protocol.ErrForbidden, "editing channel permissions needs ManageRoles")
		}
		list, err := overwritesFromView(req.Overwrites, func(id int64) bool {
			_, ok := s.hub.Role(id)
			return ok
		})
		if err != nil {
			var perr *protocol.Error
			if errors.As(err, &perr) {
				return nil, perr
			}
			return nil, protocol.Errorf(protocol.ErrBadRequest, err.Error())
		}
		// You cannot hand out, or take away, authority you do not hold.
		for _, ow := range list {
			if !base.Has(ow.Allow) || !base.Has(ow.Deny) {
				return nil, protocol.Errorf(protocol.ErrForbidden,
					"an overwrite touches a permission you do not hold")
			}
		}
		if err := s.hub.st.SetOverwrites(ctx, current.ID, list); err != nil {
			return nil, internalError(s, "save the channel permissions", err)
		}
		current.Overwrites = list
		needsResync = true
	}

	if err := s.hub.st.UpdateChannel(ctx, current); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such channel")
		}
		return nil, internalError(s, "update the channel", err)
	}
	if err := s.hub.ReloadChannels(ctx); err != nil {
		return nil, internalError(s, "reload the channel tree", err)
	}

	view := channelView(current)
	if needsResync {
		s.hub.resyncAll(ctx)
	} else {
		s.hub.BroadcastChannelEvent(protocol.Event(protocol.EvChannelUpdated, protocol.ChannelEvent{Channel: view}), current.ID)
	}
	// Anybody sitting in a channel they may no longer enter is shown the door.
	s.hub.evictFromUnreachableChannels()

	return protocol.ChannelEvent{Channel: view}, nil
}

// handleChannelDelete removes a channel and everything under it.
func handleChannelDelete(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.ChannelDeleteRequest](raw)
	if failure != nil {
		return nil, failure
	}
	if _, ok := s.hub.Channel(req.ChannelID); !ok {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such channel")
	}
	if failure := s.hub.requireChannelPermission(s, &req.ChannelID, permissions.ManageChannels); failure != nil {
		return nil, failure
	}

	// A channel takes its messages with it, and they take their files. The
	// rows go through the cascade, so what was held has to be read first.
	doomed, err := s.hub.st.DescendantIDs(ctx, req.ChannelID)
	if err != nil {
		return nil, internalError(s, "delete the channel", err)
	}
	orphaned, err := s.hub.st.AttachmentsForChannels(ctx, append([]int64{req.ChannelID}, doomed...))
	if err != nil {
		return nil, internalError(s, "delete the channel", err)
	}

	cascaded, err := s.hub.st.DeleteChannel(ctx, req.ChannelID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such channel")
		}
		return nil, internalError(s, "delete the channel", err)
	}
	s.hub.RemoveFiles(orphaned)
	if err := s.hub.ReloadChannels(ctx); err != nil {
		return nil, internalError(s, "reload the channel tree", err)
	}

	removed := append([]int64{req.ChannelID}, cascaded...)
	// The rooms go before the people: evicting somebody from a channel that no
	// longer exists cannot resolve the voice state that would announce it.
	for _, id := range removed {
		s.hub.resetVoiceRoom(id, protocol.ResetDisabled)
		if s.hub.relay != nil {
			s.hub.relay.CloseChannel(id)
		}
		s.hub.forgetVoiceChannel(id)
	}
	s.hub.evictFromChannels(removed)

	event := protocol.ChannelDeletedEvent{ChannelID: req.ChannelID, Cascaded: cascaded}
	// The channel is gone, so visibility can no longer be resolved from it:
	// every client is told, and simply ignores an id it never had.
	s.hub.Broadcast(protocol.Event(protocol.EvChannelDeleted, event))
	s.log.Info("channel deleted", slog.Int64("channel", req.ChannelID), slog.Int("cascaded", len(cascaded)))

	return event, nil
}

// --- helpers ----------------------------------------------------------------

// requireChannelPermission checks a permission at a point in the tree. A nil
// channel means the server-wide mask, which is what governs the tree root.
func (h *Hub) requireChannelPermission(s *Session, channelID *int64, want permissions.Permission) *protocol.Error {
	base, roleIDs := s.Permissions()
	perms := base
	if channelID != nil {
		perms = h.ChannelPermissions(base, roleIDs, *channelID)
		if !perms.Has(permissions.ViewChannel) {
			// Reporting "forbidden" here would confirm the channel exists.
			return protocol.Errorf(protocol.ErrNotFound, "no such channel")
		}
	}
	if !perms.Has(want) {
		return protocol.Errorf(protocol.ErrForbidden, "you are not allowed to do that here")
	}
	return nil
}

// validateParent enforces the shape of the tree: categories live at the root,
// and everything else may sit at the root or inside exactly one category.
func (h *Hub) validateParent(channelType string, parentID *int64) *protocol.Error {
	if channelType == protocol.ChannelCategory {
		if parentID != nil {
			return protocol.Errorf(protocol.ErrBadRequest, "categories cannot be nested")
		}
		return nil
	}
	if parentID == nil {
		return nil
	}
	parent, ok := h.Channel(*parentID)
	if !ok {
		return protocol.Errorf(protocol.ErrNotFound, "no such parent channel")
	}
	if parent.Type != protocol.ChannelCategory {
		return protocol.Errorf(protocol.ErrBadRequest, "a channel can only be placed inside a category")
	}
	return nil
}

// rejectParentCycle refuses a move that would make a channel its own ancestor.
func (h *Hub) rejectParentCycle(channelID int64, parentID *int64) *protocol.Error {
	if parentID == nil {
		return nil
	}
	id := *parentID
	for range maxChannelDepth {
		if id == channelID {
			return protocol.Errorf(protocol.ErrBadRequest, "that move would put the channel inside itself")
		}
		parent, ok := h.Channel(id)
		if !ok || parent.ParentID == nil {
			return nil
		}
		id = *parent.ParentID
	}
	return protocol.Errorf(protocol.ErrBadRequest, "the channel tree is nested too deeply")
}

// evictFromChannels pulls everyone out of channels that no longer exist.
func (h *Hub) evictFromChannels(removed []int64) {
	gone := make(map[int64]struct{}, len(removed))
	for _, id := range removed {
		gone[id] = struct{}{}
	}
	for _, s := range h.Sessions() {
		current := s.ChannelID()
		if current == nil {
			continue
		}
		if _, hit := gone[*current]; !hit {
			continue
		}
		h.leaveVoice(s, *current, false)
		s.setChannel(nil)
		h.broadcastUserMoved(s.UserID(), current, nil)
	}
}

// evictFromUnreachableChannels pulls out anybody whose permission to be where
// they are was just taken away.
func (h *Hub) evictFromUnreachableChannels() {
	for _, s := range h.Sessions() {
		current := s.ChannelID()
		if current == nil {
			continue
		}
		base, roleIDs := s.Permissions()
		if h.ChannelPermissions(base, roleIDs, *current).Has(permissions.Connect) {
			continue
		}
		h.leaveVoice(s, *current, false)
		s.setChannel(nil)
		h.broadcastUserMoved(s.UserID(), current, nil)
	}
}

// resyncAll rebuilds every session's permissions and hands each client a fresh
// snapshot. It is the honest answer to a change that can add or remove channels
// from what somebody is allowed to see: patching that incrementally would take
// more bookkeeping than a rare full refresh is worth.
func (h *Hub) resyncAll(ctx context.Context) {
	for _, s := range h.Sessions() {
		if err := s.refreshPermissions(ctx); err != nil {
			s.log.Error("refresh permissions", slog.Any("error", err))
			continue
		}
		ready, err := h.buildReady(ctx, s, "")
		if err != nil {
			s.log.Error("build state snapshot", slog.Any("error", err))
			continue
		}
		s.Send(protocol.Event(protocol.EvReady, ready))
	}
}
