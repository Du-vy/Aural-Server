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

	// defaultSearchLimit is one page of results in the side panel.
	defaultSearchLimit = 25
	maxSearchLimit     = 50
	// maxSearchOffset bounds how deep paging may go. Past this, refining the
	// search finds what scrolling will not.
	maxSearchOffset = 5000
	// maxSearchTerms bounds how many substrings one query is split into, so a
	// pasted paragraph cannot turn into a hundred scans of the history.
	maxSearchTerms = 8
	// maxSearchAuthors bounds the from: filter for the same reason. Nobody
	// narrows a search to a dozen people; a script would name thousands.
	maxSearchAuthors = 16
)

// handleMessageSend posts a message to a text channel, or a comment to a post.
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
	// A comment is a message in the channel its post is in, so the permission
	// checks are the channel's either way. What a post adds is that it can be
	// closed, and that the channel is not a text channel at all.
	var postID *int64
	if req.PostID > 0 {
		post, _, failure := s.loadVisiblePost(ctx, req.PostID)
		if failure != nil {
			return nil, failure
		}
		if post.ChannelID != req.ChannelID {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "that post is in another channel")
		}
		if post.Locked {
			return nil, protocol.Errorf(protocol.ErrPostLocked, "that post is closed to comments")
		}
		if failure := s.hub.requireChannelPermission(s, &req.ChannelID, permissions.SendMessages); failure != nil {
			return nil, failure
		}
		postID = &post.ID
	} else if failure := s.requireTextChannel(req.ChannelID, permissions.SendMessages); failure != nil {
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
	// Automatic moderation runs last, on the way in: a message a rule refuses
	// is never written, and one a rule censors is never written uncensored.
	verdict, failure := s.screenMessage(ctx, req.ChannelID, content)
	if failure != nil {
		return nil, failure
	}
	content = verdict.Content

	created, err := s.hub.st.CreateMessage(ctx, req.ChannelID, postID, s.UserID(), content)
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
	// After the broadcast, and never before it: a bridge is a courtesy to the
	// people who have not moved off Discord yet, and a slow one must not be
	// able to delay the message for the people who have.
	s.hub.relayMessage(created, attachments)

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

// handleMessageHistory reads one page of a channel.
//
// The page is anchored by at most one of the three cursors: backwards from
// before, forwards from after, or centred on around. Reading with none of them
// gives the newest page, which is what opening a channel asks for.
func handleMessageHistory(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.MessageHistoryRequest](raw)
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
	if req.PostID > 0 {
		return s.postHistory(ctx, req)
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

	// Every page leaves here oldest first, the order it is rendered in, whether
	// the query that produced it walked the index that way or the other.
	var (
		page []store.Message
		err  error
	)
	switch {
	case req.Around > 0:
		page, err = s.hub.st.MessagesAround(ctx, req.ChannelID, req.Around, limit)
	case req.After > 0:
		page, err = s.hub.st.MessagesAfter(ctx, req.ChannelID, req.After, limit)
	default:
		page, err = s.hub.st.MessagesBefore(ctx, req.ChannelID, req.Before, limit)
		reverse(page)
	}
	if err != nil {
		return nil, internalError(s, "read the channel history", err)
	}

	views, failure := s.messageViews(ctx, page, "read the channel history")
	if failure != nil {
		return nil, failure
	}

	result := protocol.MessageHistoryResult{ChannelID: req.ChannelID, Messages: views}
	if len(page) > 0 {
		result.HasMore, err = s.hub.st.HasMessagesBefore(ctx, req.ChannelID, page[0].ID)
		if err != nil {
			return nil, internalError(s, "read the channel history", err)
		}
		result.HasMoreAfter, err = s.hub.st.HasMessagesAfter(ctx, req.ChannelID, page[len(page)-1].ID)
		if err != nil {
			return nil, internalError(s, "read the channel history", err)
		}
	}
	return result, nil
}

// postHistory reads one page of a post's comments.
//
// Only the backwards cursor is accepted: a thread is opened at its start and
// read forwards, and the paging a client does in one is scrolling back through
// a long conversation, never jumping into the middle of it. The body is not a
// page of the thread — it arrives with the post.
func (s *Session) postHistory(ctx context.Context, req protocol.MessageHistoryRequest) (any, *protocol.Error) {
	if req.After > 0 || req.Around > 0 {
		return nil, protocol.Errorf(protocol.ErrBadRequest,
			"a post's comments are paged backwards from before")
	}
	post, _, failure := s.loadVisiblePost(ctx, req.PostID)
	if failure != nil {
		return nil, failure
	}
	if req.ChannelID != 0 && post.ChannelID != req.ChannelID {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "that post is in another channel")
	}

	limit := req.Limit
	if limit <= 0 {
		limit = defaultHistoryLimit
	}
	if limit > maxHistoryLimit {
		limit = maxHistoryLimit
	}

	// A post whose body was purged has no root to exclude, and every message
	// left in the thread is a comment.
	var root int64
	if post.RootMessageID != nil {
		root = *post.RootMessageID
	}

	page, err := s.hub.st.PostMessagesBefore(ctx, post.ID, root, req.Before, limit)
	if err != nil {
		return nil, internalError(s, "read the post", err)
	}
	reverse(page)

	views, failure := s.messageViews(ctx, page, "read the post")
	if failure != nil {
		return nil, failure
	}

	result := protocol.MessageHistoryResult{
		ChannelID: post.ChannelID,
		PostID:    post.ID,
		Messages:  views,
	}
	if len(page) > 0 {
		result.HasMore, err = s.hub.st.HasPostMessagesBefore(ctx, post.ID, root, page[0].ID)
		if err != nil {
			return nil, internalError(s, "read the post", err)
		}
	}
	return result, nil
}

// handleMessageSearch looks through every channel the caller may read.
//
// Searching is a read, so it needs no more than the right to see a channel; the
// permission model does the narrowing, by deciding which channels the query is
// allowed to run over at all.
func handleMessageSearch(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.MessageSearchRequest](raw)
	if failure != nil {
		return nil, failure
	}

	filter, failure := s.searchFilter(req)
	if failure != nil {
		return nil, failure
	}
	// Checked after validation so a malformed search is reported as malformed
	// even while the connection is being throttled.
	if !s.searches.allow() {
		return nil, protocol.Errorf(protocol.ErrRateLimited, "you are searching too quickly")
	}

	hits, total, err := s.hub.st.SearchMessages(ctx, filter)
	if err != nil {
		return nil, internalError(s, "search the history", err)
	}

	views, failure := s.searchHits(ctx, hits)
	if failure != nil {
		return nil, failure
	}
	return protocol.MessageSearchResult{
		Hits:   views,
		Total:  total,
		Offset: filter.Offset,
		Limit:  filter.Limit,
	}, nil
}

// searchFilter turns a request into the query the store runs, refusing the ones
// that ask for nothing and quietly dropping the channels the caller may not see.
func (s *Session) searchFilter(req protocol.MessageSearchRequest) (store.SearchFilter, *protocol.Error) {
	filter := store.SearchFilter{
		Terms:  store.SearchTerms(req.Query, maxSearchTerms),
		After:  req.After,
		Before: req.Before,
		Sort:   protocol.SortNewest,
		Limit:  defaultSearchLimit,
		Offset: req.Offset,
	}

	if len(req.AuthorIDs) > maxSearchAuthors {
		return filter, protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("a search may name at most %d authors", maxSearchAuthors))
	}
	filter.AuthorIDs = dedupe(req.AuthorIDs)

	// Repeating a requirement asks for the same thing twice, so it is collapsed
	// rather than turned into a second identical clause.
	seen := map[string]bool{}
	for _, has := range req.Has {
		switch has {
		case protocol.HasLink, protocol.HasFile, protocol.HasImage, protocol.HasVideo, protocol.HasSound:
			if !seen[has] {
				seen[has] = true
				filter.Has = append(filter.Has, has)
			}
		default:
			return filter, protocol.Errorf(protocol.ErrBadRequest,
				"a message can be required to have a link, file, image, video or sound")
		}
	}
	switch req.Sort {
	case "", protocol.SortNewest:
	case protocol.SortOldest, protocol.SortRelevance:
		filter.Sort = req.Sort
	default:
		return filter, protocol.Errorf(protocol.ErrBadRequest,
			"results can be sorted newest, oldest or by relevance")
	}
	if req.Limit > 0 {
		filter.Limit = min(req.Limit, maxSearchLimit)
	}
	if filter.Offset < 0 || filter.Offset > maxSearchOffset {
		return filter, protocol.Errorf(protocol.ErrBadRequest,
			"a search cannot be paged that far; narrow it instead")
	}
	if len(filter.Terms) == 0 && len(filter.AuthorIDs) == 0 && len(filter.Has) == 0 &&
		filter.After == 0 && filter.Before == 0 && len(req.ChannelIDs) == 0 {
		return filter, protocol.Errorf(protocol.ErrBadRequest, "a search needs something to look for")
	}

	// A channel the caller may not read is dropped rather than refused: it is
	// absent from their channel tree, and a search must not be the one place
	// that admits it exists.
	wanted := map[int64]bool{}
	for _, id := range req.ChannelIDs {
		wanted[id] = true
	}
	for _, channel := range s.hub.VisibleChannels(s) {
		if channel.Type != protocol.ChannelText {
			continue
		}
		if len(wanted) == 0 || wanted[channel.ID] {
			filter.ChannelIDs = append(filter.ChannelIDs, channel.ID)
		}
	}
	return filter, nil
}

// searchHits renders a page of matches together with the line either side of
// each of them, which is what makes a result recognisable at a glance.
func (s *Session) searchHits(ctx context.Context, hits []store.Message) ([]protocol.MessageSearchHit, *protocol.Error) {
	if len(hits) == 0 {
		return []protocol.MessageSearchHit{}, nil
	}

	ids := make([]int64, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ID)
	}
	neighbours, err := s.hub.st.NeighbourIDs(ctx, ids)
	if err != nil {
		return nil, internalError(s, "search the history", err)
	}

	// The hits and everything around them are read and rendered as one set, so
	// a page of results costs the same handful of queries however long it is.
	around := ids
	for _, n := range neighbours {
		if n.Before != nil {
			around = append(around, *n.Before)
		}
		if n.After != nil {
			around = append(around, *n.After)
		}
	}
	surrounding, err := s.hub.st.MessagesByID(ctx, around)
	if err != nil {
		return nil, internalError(s, "search the history", err)
	}
	views, failure := s.messageViews(ctx, surrounding, "search the history")
	if failure != nil {
		return nil, failure
	}
	byID := make(map[int64]protocol.Message, len(views))
	for _, view := range views {
		byID[view.ID] = view
	}

	out := make([]protocol.MessageSearchHit, 0, len(hits))
	for _, hit := range hits {
		entry := protocol.MessageSearchHit{Message: byID[hit.ID]}
		n := neighbours[hit.ID]
		if n.Before != nil {
			if view, ok := byID[*n.Before]; ok {
				entry.Before = &view
			}
		}
		if n.After != nil {
			if view, ok := byID[*n.After]; ok {
				entry.After = &view
			}
		}
		out = append(out, entry)
	}
	return out, nil
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
	// Editing is screened too. A rule that only looked at sends would be
	// worked around by posting a full stop and then editing it.
	verdict, failure := s.screenText(ctx, existing.ChannelID, content)
	if failure != nil {
		return nil, failure
	}
	content = verdict.Content

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
	s.hub.relayEdit(updated)

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
	// The body of a post is not a message anybody deletes on its own: doing so
	// would leave the post standing with nothing in it. Deleting the post is
	// the act that was meant, and it takes the whole thread with it.
	if existing.PostID != nil {
		post, err := s.hub.st.PostByID(ctx, *existing.PostID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, internalError(s, "read the message", err)
		}
		if post.RootMessageID != nil && *post.RootMessageID == existing.ID {
			return nil, protocol.Errorf(protocol.ErrBadRequest,
				"that message is the body of a post; delete the post instead")
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
	s.hub.relayDelete(existing)
	if !own {
		s.log.Info("message deleted by a moderator",
			slog.Int64("message", existing.ID), slog.Int64("channel", existing.ChannelID))
		// Only somebody else's message is logged. Deleting your own needs no
		// permission and is nobody's business but yours, so recording it would
		// make the log a record of what everybody had second thoughts about.
		entry := auditTarget(protocol.AuditTargetMessage, existing.ID, existing.Author)
		entry.Action = protocol.AuditMessageDelete
		if channel, ok := s.hub.Channel(existing.ChannelID); ok {
			entry.Changes = []store.AuditChange{{Key: "channel", After: channel.Name}}
		}
		s.hub.audit(ctx, s, entry)
	}

	return event, nil
}

// --- helpers ----------------------------------------------------------------

// dedupe keeps the first of each id, which is what a repeated filter means.
func dedupe(ids []int64) []int64 {
	if len(ids) < 2 {
		return ids
	}
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// reverse flips a page in place, which is how a query that walked the index
// newest first becomes the oldest-first order every page is rendered in.
func reverse(page []store.Message) {
	for i, j := 0, len(page)-1; i < j; i, j = i+1, j-1 {
		page[i], page[j] = page[j], page[i]
	}
}

// messageViews renders a run of messages along with the files they carry. One
// query serves the whole run, rather than one per message.
func (s *Session) messageViews(ctx context.Context, page []store.Message, doing string) ([]protocol.Message, *protocol.Error) {
	ids := make([]int64, 0, len(page))
	for _, m := range page {
		ids = append(ids, m.ID)
	}
	attachments, err := s.hub.st.AttachmentsForMessages(ctx, ids)
	if err != nil {
		return nil, internalError(s, doing, err)
	}

	views := make([]protocol.Message, 0, len(page))
	for _, m := range page {
		views = append(views, messageView(m, attachments[m.ID]))
	}
	return views, nil
}

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
