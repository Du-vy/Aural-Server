package gateway

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// maxConversations bounds the list one identity is told about at once.
//
// Private threads are not paged: somebody has as many people they have written
// to as they have, and the number is small for the same reason an address book
// is. The bound is here so that a client is never handed a list it cannot
// render rather than because anybody is expected to reach it.
const maxConversations = 200

// handleDMList reads every private conversation the caller is in.
func handleDMList(ctx context.Context, s *Session, _ json.RawMessage) (any, *protocol.Error) {
	if failure := s.hub.requireDirectMessages(); failure != nil {
		return nil, failure
	}
	views, failure := s.hub.conversationViews(ctx, s, s.UserID())
	if failure != nil {
		return nil, failure
	}
	return protocol.DMListResult{Conversations: views}, nil
}

// handleDMHistory reads one page of the conversation with one person.
//
// A conversation nobody has opened yet is not an error: it is what every
// conversation looks like before the first thing is said in it, and a client
// asking for the history of one is exactly how it draws the empty thread it is
// about to write into.
func handleDMHistory(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	if failure := s.hub.requireDirectMessages(); failure != nil {
		return nil, failure
	}
	req, failure := decode[protocol.DMHistoryRequest](raw)
	if failure != nil {
		return nil, failure
	}
	cursors := 0
	for _, cursor := range []int64{req.Before, req.After, req.Around} {
		if cursor > 0 {
			cursors++
		}
	}
	if cursors > 1 {
		return nil, protocol.Errorf(protocol.ErrBadRequest,
			"before, after and around name three different pages; send one")
	}
	if _, failure := s.hub.otherParty(ctx, s, req.UserID); failure != nil {
		return nil, failure
	}

	result := protocol.DMHistoryResult{UserID: req.UserID, Messages: []protocol.DirectMessage{}}
	conversation, err := s.hub.st.ConversationBetween(ctx, s.UserID(), req.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return nil, internalError(s, "read the conversation", err)
	}
	result.ConversationID = conversation.ID

	limit := req.Limit
	if limit <= 0 {
		limit = defaultHistoryLimit
	}
	if limit > maxHistoryLimit {
		limit = maxHistoryLimit
	}

	// Every page leaves here oldest first, the order it is rendered in.
	var page []store.DirectMessage
	switch {
	case req.Around > 0:
		page, err = s.hub.st.DirectMessagesAround(ctx, conversation.ID, req.Around, limit)
	case req.After > 0:
		page, err = s.hub.st.DirectMessagesAfter(ctx, conversation.ID, req.After, limit)
	default:
		page, err = s.hub.st.DirectMessagesBefore(ctx, conversation.ID, req.Before, limit)
		reverseDirect(page)
	}
	var replyIDs []int64
	for _, m := range page {
		if m.ReplyToID != nil {
			replyIDs = append(replyIDs, *m.ReplyToID)
		}
	}
	var replies map[int64]store.DirectMessage
	if len(replyIDs) > 0 {
		replies, _ = s.hub.st.RepliesForDirectMessages(ctx, replyIDs)
	}

	for _, m := range page {
		var replyTo *protocol.ReferencedMessage
		if m.ReplyToID != nil {
			if target, ok := replies[*m.ReplyToID]; ok {
				replyTo = referencedDMView(&target, *m.ReplyToID)
			} else {
				replyTo = referencedDMView(nil, *m.ReplyToID)
			}
		}
		result.Messages = append(result.Messages, directMessageView(m, replyTo))
	}
	if len(page) > 0 {
		result.HasMore, err = s.hub.st.HasDirectMessagesBefore(ctx, conversation.ID, page[0].ID)
		if err != nil {
			return nil, internalError(s, "read the conversation", err)
		}
		result.HasMoreAfter, err = s.hub.st.HasDirectMessagesAfter(ctx, conversation.ID, page[len(page)-1].ID)
		if err != nil {
			return nil, internalError(s, "read the conversation", err)
		}
	}
	return result, nil
}

// handleDMSend writes to one person, opening the conversation if this is the
// first thing either of them has said to the other.
func handleDMSend(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	if failure := s.hub.requireDirectMessages(); failure != nil {
		return nil, failure
	}
	req, failure := decode[protocol.DMSendRequest](raw)
	if failure != nil {
		return nil, failure
	}
	content, failure := validateMessageContent(req.Content, false)
	if failure != nil {
		return nil, failure
	}
	recipient, failure := s.hub.otherParty(ctx, s, req.UserID)
	if failure != nil {
		return nil, failure
	}
	if base, _ := s.Permissions(); !base.Has(permissions.SendDirectMessages) {
		return nil, protocol.Errorf(protocol.ErrForbidden,
			"you are not allowed to send private messages on this server")
	}
	if failure := s.hub.requireMutualConsent(ctx, s, recipient); failure != nil {
		return nil, failure
	}
	// Checked after validation so a malformed or refused message is reported as
	// such even while the connection is being throttled.
	if !s.messages.allow() {
		return nil, protocol.Errorf(protocol.ErrRateLimited, "you are sending messages too quickly")
	}

	conversation, err := s.hub.st.EnsureConversation(ctx, s.UserID(), recipient.ID)
	if err != nil {
		return nil, internalError(s, "open the conversation", err)
	}

	var replyToID *int64
	var replyTo *protocol.ReferencedMessage
	if req.ReplyToID != nil && *req.ReplyToID > 0 {
		target, err := s.hub.st.DirectMessageByID(ctx, *req.ReplyToID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, protocol.Errorf(protocol.ErrNotFound, "the message you are replying to does not exist")
			}
			return nil, internalError(s, "load reply target", err)
		}
		if target.ConversationID != conversation.ID {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "cannot reply to a message from another conversation")
		}
		replyToID = req.ReplyToID
		replyTo = referencedDMView(&target, *req.ReplyToID)
	}

	created, err := s.hub.st.CreateDirectMessage(ctx, conversation.ID, s.UserID(), content, replyToID)
	if err != nil {
		return nil, internalError(s, "store the message", err)
	}

	// The row is read back after the write, because sending moved the sender's
	// own read marker and stamped the thread with the time of this message.
	conversation, err = s.hub.st.ConversationByID(ctx, conversation.ID)
	if err != nil {
		return nil, internalError(s, "store the message", err)
	}

	view := directMessageView(created, replyTo)
	own, failure := s.hub.deliverDirectMessage(ctx, s, conversation, view)
	if failure != nil {
		return nil, failure
	}
	return own, nil
}

// deliverDirectMessage hands one line to both people in the thread and returns
// what the author is told.
//
// Each side is sent its own frame: a conversation is named by the other
// participant, so the same message travels to the two of them under two
// different names.
func (h *Hub) deliverDirectMessage(
	ctx context.Context,
	author *Session,
	conversation store.Conversation,
	message protocol.DirectMessage,
) (protocol.DMCreatedEvent, *protocol.Error) {
	authorID := author.UserID()
	peerID := conversation.PeerOf(authorID)

	ownView, failure := h.conversationView(ctx, author, conversation, authorID, &message)
	if failure != nil {
		return protocol.DMCreatedEvent{}, failure
	}
	own := protocol.DMCreatedEvent{Conversation: ownView, Message: message}
	author.Send(protocol.Event(protocol.EvDMCreated, own))

	if peer, ok := h.SessionForUser(peerID); ok {
		peerView, failure := h.conversationView(ctx, peer, conversation, peerID, &message)
		if failure != nil {
			return protocol.DMCreatedEvent{}, failure
		}
		peer.Send(protocol.Event(protocol.EvDMCreated,
			protocol.DMCreatedEvent{Conversation: peerView, Message: message}))
	}
	return own, nil
}

// handleDMEdit rewrites one private line.
//
// Only the author may edit, exactly as in a channel, and here there is not even
// a moderator to argue about it with: a private conversation has two people in
// it and no third party who could be entitled to rewrite either of them.
func handleDMEdit(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	if failure := s.hub.requireDirectMessages(); failure != nil {
		return nil, failure
	}
	req, failure := decode[protocol.DMEditRequest](raw)
	if failure != nil {
		return nil, failure
	}
	content, failure := validateMessageContent(req.Content, false)
	if failure != nil {
		return nil, failure
	}
	existing, conversation, failure := s.loadOwnDirectMessage(ctx, req.MessageID)
	if failure != nil {
		return nil, failure
	}
	if existing.UserID == nil || *existing.UserID != s.UserID() {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you can only edit your own messages")
	}
	if !s.messages.allow() {
		return nil, protocol.Errorf(protocol.ErrRateLimited, "you are editing messages too quickly")
	}

	updated, err := s.hub.st.UpdateDirectMessageContent(ctx, req.MessageID, content)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such message")
		}
		return nil, internalError(s, "edit the message", err)
	}

	var replyTo *protocol.ReferencedMessage
	if updated.ReplyToID != nil {
		if target, err := s.hub.st.DirectMessageByID(ctx, *updated.ReplyToID); err == nil {
			replyTo = referencedDMView(&target, *updated.ReplyToID)
		} else {
			replyTo = referencedDMView(nil, *updated.ReplyToID)
		}
	}

	view := directMessageView(updated, replyTo)
	s.hub.notifyBothSides(conversation, func(userID int64) protocol.Envelope {
		return protocol.Event(protocol.EvDMUpdated, protocol.DMUpdatedEvent{
			UserID:  conversation.PeerOf(userID),
			Message: view,
		})
	})
	return protocol.DMUpdatedEvent{UserID: conversation.PeerOf(s.UserID()), Message: view}, nil
}

// handleDMDelete removes one private line. Only the author may: there is
// nobody else in a private conversation to hold ManageMessages over it.
func handleDMDelete(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	if failure := s.hub.requireDirectMessages(); failure != nil {
		return nil, failure
	}
	req, failure := decode[protocol.DMDeleteRequest](raw)
	if failure != nil {
		return nil, failure
	}
	existing, conversation, failure := s.loadOwnDirectMessage(ctx, req.MessageID)
	if failure != nil {
		return nil, failure
	}
	if existing.UserID == nil || *existing.UserID != s.UserID() {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you can only delete your own messages")
	}

	if err := s.hub.st.DeleteDirectMessage(ctx, req.MessageID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such message")
		}
		return nil, internalError(s, "delete the message", err)
	}

	s.hub.notifyBothSides(conversation, func(userID int64) protocol.Envelope {
		return protocol.Event(protocol.EvDMDeleted, protocol.DMDeletedEvent{
			UserID:         conversation.PeerOf(userID),
			ConversationID: conversation.ID,
			MessageID:      existing.ID,
		})
	})
	return protocol.DMDeletedEvent{
		UserID:         conversation.PeerOf(s.UserID()),
		ConversationID: conversation.ID,
		MessageID:      existing.ID,
	}, nil
}

// handleDMRead moves the caller's own read marker up to a message they have
// seen. Nobody else is told: a read marker is a badge, not a receipt.
func handleDMRead(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	if failure := s.hub.requireDirectMessages(); failure != nil {
		return nil, failure
	}
	req, failure := decode[protocol.DMReadRequest](raw)
	if failure != nil {
		return nil, failure
	}
	conversation, err := s.hub.st.ConversationBetween(ctx, s.UserID(), req.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such conversation")
	}
	if err != nil {
		return nil, internalError(s, "read the conversation", err)
	}
	if err := s.hub.st.MarkConversationRead(ctx, conversation.ID, s.UserID(), req.MessageID); err != nil {
		return nil, internalError(s, "mark the conversation read", err)
	}

	conversation, err = s.hub.st.ConversationByID(ctx, conversation.ID)
	if err != nil {
		return nil, internalError(s, "read the conversation", err)
	}
	view, failure := s.hub.conversationView(ctx, s, conversation, s.UserID(), nil)
	if failure != nil {
		return nil, failure
	}
	return view, nil
}

// --- helpers ----------------------------------------------------------------

// requireDirectMessages refuses everything here on a server that carries no
// private conversations.
func (h *Hub) requireDirectMessages() *protocol.Error {
	if h.DirectMessagesEnabled() {
		return nil
	}
	return protocol.Errorf(protocol.ErrDMDisabled, "this server does not carry private messages")
}

// otherParty resolves who a request is about, refusing the two ids that can
// never name a conversation: somebody who does not exist, and yourself.
func (h *Hub) otherParty(ctx context.Context, s *Session, userID int64) (store.User, *protocol.Error) {
	if userID == s.UserID() {
		return store.User{}, protocol.Errorf(protocol.ErrBadRequest,
			"a private conversation needs somebody else in it")
	}
	other, err := h.st.UserByID(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, protocol.Errorf(protocol.ErrNotFound, "no such user")
	}
	if err != nil {
		return store.User{}, internalError(s, "load that user", err)
	}
	return other, nil
}

// requireMutualConsent applies the privacy setting from both ends.
//
// It is read both ways round on purpose. A setting that only stopped people
// writing to you would leave somebody who wants no private messages able to
// open a thread nobody may answer, which is a worse place to be than either
// answer on its own.
func (h *Hub) requireMutualConsent(ctx context.Context, s *Session, recipient store.User) *protocol.Error {
	sender, err := h.st.UserByID(ctx, s.UserID())
	if err != nil {
		return internalError(s, "read your privacy settings", err)
	}
	if !sender.AcceptsDMFrom(recipient) {
		return protocol.Errorf(protocol.ErrDMBlocked,
			"your privacy settings do not allow a private conversation with that member")
	}
	if !recipient.AcceptsDMFrom(sender) {
		return protocol.Errorf(protocol.ErrDMBlocked,
			"that member does not accept private messages from you")
	}
	return nil
}

// loadOwnDirectMessage reads a private line and the thread it is in, refusing
// one the caller is not a party to. A conversation somebody is not in reports
// "not found": that it exists at all is not theirs to learn.
func (s *Session) loadOwnDirectMessage(ctx context.Context, id int64) (store.DirectMessage, store.Conversation, *protocol.Error) {
	message, err := s.hub.st.DirectMessageByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.DirectMessage{}, store.Conversation{}, protocol.Errorf(protocol.ErrNotFound, "no such message")
	}
	if err != nil {
		return store.DirectMessage{}, store.Conversation{}, internalError(s, "read the message", err)
	}
	conversation, err := s.hub.st.ConversationByID(ctx, message.ConversationID)
	if err != nil {
		return store.DirectMessage{}, store.Conversation{}, internalError(s, "read the conversation", err)
	}
	if !conversation.Involves(s.UserID()) {
		return store.DirectMessage{}, store.Conversation{}, protocol.Errorf(protocol.ErrNotFound, "no such message")
	}
	return message, conversation, nil
}

// notifyBothSides sends whichever frame each participant should see. build is
// called with a participant's own id, so it can name the other one.
func (h *Hub) notifyBothSides(conversation store.Conversation, build func(userID int64) protocol.Envelope) {
	for _, userID := range [2]int64{conversation.UserLow, conversation.UserHigh} {
		if session, ok := h.SessionForUser(userID); ok {
			session.Send(build(userID))
		}
	}
}

// conversationView renders one thread as it looks to one of the two people in
// it. last is the line to show under the name when the caller already has it;
// otherwise it is read.
func (h *Hub) conversationView(
	ctx context.Context,
	s *Session,
	conversation store.Conversation,
	viewerID int64,
	last *protocol.DirectMessage,
) (protocol.Conversation, *protocol.Error) {
	view := protocol.Conversation{
		ID:            conversation.ID,
		UserID:        conversation.PeerOf(viewerID),
		LastMessageAt: conversation.LastMessageAt,
		LastMessage:   last,
	}
	if last == nil {
		previews, err := h.st.LastDirectMessages(ctx, []int64{conversation.ID})
		if err != nil {
			return protocol.Conversation{}, internalError(s, "read the conversation", err)
		}
		if message, ok := previews[conversation.ID]; ok {
			rendered := directMessageView(message, nil)
			view.LastMessage = &rendered
		}
	}
	unread, err := h.st.UnreadDirectCount(ctx, conversation.ID, viewerID)
	if err != nil {
		return protocol.Conversation{}, internalError(s, "read the conversation", err)
	}
	view.Unread = unread
	return view, nil
}

// conversationViews renders every thread one identity is in, newest first. The
// previews and the badges are read as two queries for the whole list rather
// than two per thread.
func (h *Hub) conversationViews(ctx context.Context, s *Session, userID int64) ([]protocol.Conversation, *protocol.Error) {
	conversations, err := h.st.ConversationsFor(ctx, userID, maxConversations)
	if err != nil {
		return nil, internalError(s, "list your conversations", err)
	}
	if len(conversations) == 0 {
		return []protocol.Conversation{}, nil
	}

	ids := make([]int64, 0, len(conversations))
	for _, c := range conversations {
		ids = append(ids, c.ID)
	}
	previews, err := h.st.LastDirectMessages(ctx, ids)
	if err != nil {
		return nil, internalError(s, "list your conversations", err)
	}
	unread, err := h.st.UnreadDirectCounts(ctx, userID)
	if err != nil {
		return nil, internalError(s, "list your conversations", err)
	}

	out := make([]protocol.Conversation, 0, len(conversations))
	for _, c := range conversations {
		view := protocol.Conversation{
			ID:            c.ID,
			UserID:        c.PeerOf(userID),
			LastMessageAt: c.LastMessageAt,
			Unread:        unread[c.ID],
		}
		if message, ok := previews[c.ID]; ok {
			rendered := directMessageView(message, nil)
			view.LastMessage = &rendered
		}
		out = append(out, view)
	}
	return out, nil
}

// reverseDirect flips a page in place, turning the order the index was walked
// in into the oldest-first order every page is rendered in.
func reverseDirect(page []store.DirectMessage) {
	for i, j := 0, len(page)-1; i < j; i, j = i+1, j-1 {
		page[i], page[j] = page[j], page[i]
	}
}
