package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

const (
	// defaultHistoryLimit is one screenful with room to scroll.
	defaultHistoryLimit = 50
	maxHistoryLimit     = 100
)

// handleMessageSend posts a message to a text channel.
func handleMessageSend(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.MessageSendRequest](raw)
	if failure != nil {
		return nil, failure
	}
	attachmentIDs, failure := s.validateAttachmentIDs(req.Attachments)
	if failure != nil {
		return nil, failure
	}
	// A message carrying files may have nothing to say beyond them, which is
	// the one case where empty content is a message rather than a mistake.
	content, failure := validateMessageContent(req.Content, len(attachmentIDs) > 0)
	if failure != nil {
		return nil, failure
	}
	if failure := s.requireTextChannel(req.ChannelID, permissions.SendMessages); failure != nil {
		return nil, failure
	}
	if len(attachmentIDs) > 0 {
		if failure := s.hub.requireChannelPermission(s, &req.ChannelID, permissions.AttachFiles); failure != nil {
			return nil, failure
		}
	}
	// Checked after validation so a client is told about a malformed message
	// even while it is being throttled.
	if !s.messages.allow() {
		return nil, protocol.Errorf(protocol.ErrRateLimited, "you are sending messages too quickly")
	}

	created, err := s.hub.st.CreateMessage(ctx, req.ChannelID, s.UserID(), content)
	if err != nil {
		return nil, internalError(s, "store the message", err)
	}

	attachments, err := s.hub.st.ClaimAttachments(ctx, created.ID, s.UserID(), created.ChannelID, attachmentIDs)
	if err != nil {
		// The message is only the message that was written once its files are
		// attached, so a claim that fails takes it with it rather than posting
		// half of what somebody meant to send.
		if delErr := s.hub.st.DeleteMessage(ctx, created.ID); delErr != nil {
			s.log.Error("roll back a message whose files could not be attached",
				slog.Int64("message", created.ID), slog.Any("error", delErr))
		}
		if errors.Is(err, store.ErrAttachmentUnavailable) {
			return nil, protocol.Errorf(protocol.ErrNotFound,
				"one of those uploads is gone, already posted, or not yours")
		}
		return nil, internalError(s, "attach the files", err)
	}

	view := messageView(created, attachments)
	s.hub.BroadcastChannelEvent(
		protocol.Event(protocol.EvMessageCreated, protocol.MessageEvent{Message: view}),
		created.ChannelID)

	return protocol.MessageEvent{Message: view}, nil
}

// validateAttachmentIDs checks the shape of an attachment list before anything
// is written: the count against what the server allows, and the ids against
// each other, since claiming the same upload twice would otherwise report a
// missing one rather than the duplicate that caused it.
func (s *Session) validateAttachmentIDs(ids []int64) ([]int64, *protocol.Error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if s.hub.Files() == nil {
		return nil, protocol.Errorf(protocol.ErrUploadsDisabled, "this server does not accept file uploads")
	}
	if limit := s.hub.cfg.Uploads.MaxPerMessage; len(ids) > limit {
		return nil, protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("a message may carry at most %d files", limit))
	}

	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "attachment ids must be positive")
		}
		if _, dup := seen[id]; dup {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "the same upload is listed twice")
		}
		seen[id] = struct{}{}
	}
	return ids, nil
}

// handleMessageHistory pages backwards through a channel.
func handleMessageHistory(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.MessageHistoryRequest](raw)
	if failure != nil {
		return nil, failure
	}
	// Reading needs only the right to see the channel: a member who may not
	// post can still follow along.
	if failure := s.requireTextChannel(req.ChannelID, permissions.ViewChannel); failure != nil {
		return nil, failure
	}

	limit := req.Limit
	if limit <= 0 {
		limit = defaultHistoryLimit
	}
	if limit > maxHistoryLimit {
		limit = maxHistoryLimit
	}

	page, err := s.hub.st.MessagesBefore(ctx, req.ChannelID, req.Before, limit)
	if err != nil {
		return nil, internalError(s, "read the channel history", err)
	}

	// One query serves the whole page, rather than one per message.
	ids := make([]int64, 0, len(page))
	for _, m := range page {
		ids = append(ids, m.ID)
	}
	attachments, err := s.hub.st.AttachmentsForMessages(ctx, ids)
	if err != nil {
		return nil, internalError(s, "read the channel history", err)
	}

	// The store walks the index newest first; clients render oldest first.
	views := make([]protocol.Message, 0, len(page))
	for i := len(page) - 1; i >= 0; i-- {
		views = append(views, messageView(page[i], attachments[page[i].ID]))
	}

	hasMore := false
	if len(page) > 0 {
		// page is newest first, so its last entry is the oldest one read.
		oldest := page[len(page)-1].ID
		hasMore, err = s.hub.st.HasMessagesBefore(ctx, req.ChannelID, oldest)
		if err != nil {
			return nil, internalError(s, "read the channel history", err)
		}
	}

	return protocol.MessageHistoryResult{
		ChannelID: req.ChannelID,
		Messages:  views,
		HasMore:   hasMore,
	}, nil
}

// handleMessageEdit rewrites a message.
//
// Only the author may edit, and no permission overrides that: putting words in
// somebody's mouth is not moderation. A moderator who objects to a message can
// delete it, which is visible, rather than change it, which is not.
func handleMessageEdit(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.MessageEditRequest](raw)
	if failure != nil {
		return nil, failure
	}
	existing, failure := s.loadVisibleMessage(ctx, req.MessageID)
	if failure != nil {
		return nil, failure
	}
	if existing.UserID == nil || *existing.UserID != s.UserID() {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you can only edit your own messages")
	}

	// An edit rewrites what was said and never touches what was attached, so a
	// message whose whole content was a file may be edited down to no text at
	// all without becoming an empty message.
	attachments, err := s.hub.st.AttachmentsForMessage(ctx, existing.ID)
	if err != nil {
		return nil, internalError(s, "read the message", err)
	}
	content, failure := validateMessageContent(req.Content, len(attachments) > 0)
	if failure != nil {
		return nil, failure
	}
	if !s.messages.allow() {
		return nil, protocol.Errorf(protocol.ErrRateLimited, "you are editing messages too quickly")
	}

	updated, err := s.hub.st.UpdateMessageContent(ctx, req.MessageID, content)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such message")
		}
		return nil, internalError(s, "edit the message", err)
	}

	view := messageView(updated, attachments)
	s.hub.BroadcastChannelEvent(
		protocol.Event(protocol.EvMessageUpdated, protocol.MessageEvent{Message: view}),
		updated.ChannelID)

	return protocol.MessageEvent{Message: view}, nil
}

// handleMessageDelete removes a message. Anybody may delete their own;
// deleting somebody else's needs ManageMessages in that channel.
func handleMessageDelete(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.MessageDeleteRequest](raw)
	if failure != nil {
		return nil, failure
	}

	existing, failure := s.loadVisibleMessage(ctx, req.MessageID)
	if failure != nil {
		return nil, failure
	}

	own := existing.UserID != nil && *existing.UserID == s.UserID()
	if !own {
		if failure := s.hub.requireChannelPermission(s, &existing.ChannelID, permissions.ManageMessages); failure != nil {
			return nil, failure
		}
	}

	// The rows go with the message through the cascade, so which files it held
	// has to be read while they still exist.
	attachments, err := s.hub.st.AttachmentsForMessage(ctx, existing.ID)
	if err != nil {
		return nil, internalError(s, "read the message", err)
	}

	if err := s.hub.st.DeleteMessage(ctx, req.MessageID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such message")
		}
		return nil, internalError(s, "delete the message", err)
	}
	// Deleting a message deletes what it carried. Moderating a file is
	// therefore the same act as moderating the message it arrived in, which is
	// the only handle anybody is given on it.
	s.hub.RemoveFiles(attachments)

	event := protocol.MessageDeletedEvent{MessageID: existing.ID, ChannelID: existing.ChannelID}
	s.hub.BroadcastChannelEvent(protocol.Event(protocol.EvMessageDeleted, event), existing.ChannelID)
	if !own {
		s.log.Info("message deleted by a moderator",
			slog.Int64("message", existing.ID), slog.Int64("channel", existing.ChannelID))
	}

	return event, nil
}

// --- helpers ----------------------------------------------------------------

// requireTextChannel checks that a channel exists, is a text channel, and that
// the caller holds want inside it.
func (s *Session) requireTextChannel(channelID int64, want permissions.Permission) *protocol.Error {
	channel, ok := s.hub.Channel(channelID)
	if !ok {
		return protocol.Errorf(protocol.ErrNotFound, "no such channel")
	}
	if failure := s.hub.requireChannelPermission(s, &channelID, want); failure != nil {
		return failure
	}
	// Checked after the permission so an invisible channel reports "not found"
	// rather than leaking its type.
	if channel.Type != protocol.ChannelText {
		return protocol.Errorf(protocol.ErrBadRequest, "that channel does not carry messages")
	}
	return nil
}

// loadVisibleMessage reads a message the caller is allowed to see. A message in
// a channel they cannot see reports "not found", exactly as the channel would.
func (s *Session) loadVisibleMessage(ctx context.Context, id int64) (store.Message, *protocol.Error) {
	message, err := s.hub.st.MessageByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Message{}, protocol.Errorf(protocol.ErrNotFound, "no such message")
		}
		return store.Message{}, internalError(s, "read the message", err)
	}
	if !s.hub.SessionCanView(s, message.ChannelID) {
		return store.Message{}, protocol.Errorf(protocol.ErrNotFound, "no such message")
	}
	return message, nil
}
