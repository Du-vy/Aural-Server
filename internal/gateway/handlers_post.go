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

const (
	// defaultPostLimit is one screenful of entries with room to scroll.
	defaultPostLimit = 25
	maxPostLimit     = 50
	// maxPinnedPosts bounds what travels with the first page of a listing.
	// Pinning everything is not pinning anything, so a channel that tries it
	// gets the newest of them rather than an unbounded page.
	maxPinnedPosts = 25
	// maxCalendarWindow bounds a range query at a little over a year, which is
	// the widest view a calendar offers.
	maxCalendarWindow = 400 * 24 * 3600
	// maxCalendarPosts bounds how many events one window may return.
	maxCalendarPosts = 500
)

// handlePostCreate starts an entry in a channel that holds entries.
func handlePostCreate(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.PostCreateRequest](raw)
	if failure != nil {
		return nil, failure
	}
	attachmentIDs, failure := s.validateAttachmentIDs(req.Attachments)
	if failure != nil {
		return nil, failure
	}
	title, failure := validatePostTitle(req.Title)
	if failure != nil {
		return nil, failure
	}
	// The body of a post is a message, and it is the same message either way:
	// a media entry says everything with its files, an announcement with its
	// words, and both are a message that carries what it carries.
	content, failure := validateMessageContent(req.Content, len(attachmentIDs) > 0)
	if failure != nil {
		return nil, failure
	}

	channel, failure := s.requirePostChannel(req.ChannelID, permissions.CreatePosts)
	if failure != nil {
		return nil, failure
	}
	event, failure := validatePostEvent(channel.Type, req.Event)
	if failure != nil {
		return nil, failure
	}
	// A media entry is its file. Without one there is nothing to show, and the
	// channel would be a text channel with a title on every message.
	if channel.Type == protocol.ChannelMedia && len(attachmentIDs) == 0 {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "a media post needs at least one file")
	}
	if len(attachmentIDs) > 0 {
		if failure := s.hub.requireChannelPermission(s, &req.ChannelID, permissions.AttachFiles); failure != nil {
			return nil, failure
		}
	}
	// Checked after validation so a client is told about a malformed post even
	// while it is being throttled.
	if !s.messages.allow() {
		return nil, protocol.Errorf(protocol.ErrRateLimited, "you are posting too quickly")
	}

	userID := s.UserID()
	post := store.Post{ChannelID: req.ChannelID, UserID: &userID, Title: title}
	if event != nil {
		post.StartsAt = &event.StartsAt
		post.EndsAt = event.EndsAt
		post.AllDay = event.AllDay
		post.Location = event.Location
	}

	// The body of a post is a message like any other, so the same rules apply
	// to it. The title is screened too: it is the part a listing shows, which
	// makes it the part worth writing something unacceptable into.
	verdict, failure := s.screenMessage(ctx, req.ChannelID, content)
	if failure != nil {
		return nil, failure
	}
	content = verdict.Content

	titleVerdict, failure := s.screenText(ctx, req.ChannelID, title)
	if failure != nil {
		return nil, failure
	}
	post.Title = titleVerdict.Content

	created, body, err := s.hub.st.CreatePost(ctx, post, content)
	if err != nil {
		return nil, internalError(s, "store the post", err)
	}

	attachments, err := s.hub.st.ClaimAttachments(ctx, body.ID, userID, created.ChannelID, attachmentIDs)
	if err != nil {
		// A post is only the post that was written once its files are
		// attached, so a claim that fails takes the whole of it — post, body
		// and all — rather than leaving half of what somebody meant to share.
		if delErr := s.hub.st.DeletePost(ctx, created.ID); delErr != nil {
			s.log.Error("roll back a post whose files could not be attached",
				slog.Int64("post", created.ID), slog.Any("error", delErr))
		}
		if errors.Is(err, store.ErrAttachmentUnavailable) {
			return nil, protocol.Errorf(protocol.ErrNotFound,
				"one of those uploads is gone, already posted, or not yours")
		}
		return nil, internalError(s, "attach the files", err)
	}

	bodyView := messageView(body, attachments)
	view := postView(created, &bodyView, store.PostStats{}, store.PostRSVPCounts{}, "")
	// The tallies are empty and nobody has answered, so this frame is the same
	// for everybody and can go out as it stands.
	s.hub.BroadcastChannelEvent(
		protocol.Event(protocol.EvPostCreated, protocol.PostEvent{Post: view}), created.ChannelID)
	s.log.Info("post created",
		slog.Int64("post", created.ID), slog.Int64("channel", created.ChannelID))

	return protocol.PostEvent{Post: view}, nil
}

// handlePostList pages through one channel's entries.
//
// A calendar is asked for as a window in time and everything else as a page of
// ids. The first page of a page-wise listing also carries the pinned entries,
// however old they are, because that is the whole point of pinning one.
func handlePostList(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.PostListRequest](raw)
	if failure != nil {
		return nil, failure
	}
	// Reading needs only the right to see the channel: somebody who may not
	// post can still follow along.
	channel, failure := s.requirePostChannel(req.ChannelID, permissions.ViewChannel)
	if failure != nil {
		return nil, failure
	}

	limit := req.Limit
	if limit <= 0 {
		limit = defaultPostLimit
	}
	if limit > maxPostLimit {
		limit = maxPostLimit
	}

	var (
		page    []store.Post
		hasMore bool
	)
	if window := req.From > 0 || req.To > 0; window {
		if channel.Type != protocol.ChannelCalendar {
			return nil, protocol.Errorf(protocol.ErrBadRequest,
				"only a calendar channel is read as a window in time")
		}
		from, to, failure := validatePostWindow(req.From, req.To)
		if failure != nil {
			return nil, failure
		}
		found, err := s.hub.st.PostsInRange(ctx, req.ChannelID, from, to, maxCalendarPosts)
		if err != nil {
			return nil, internalError(s, "read the channel", err)
		}
		page = found
	} else {
		found, err := s.hub.st.PostsBefore(ctx, req.ChannelID, req.Before, limit)
		if err != nil {
			return nil, internalError(s, "read the channel", err)
		}
		page = found
		if len(page) > 0 {
			hasMore, err = s.hub.st.HasPostsBefore(ctx, req.ChannelID, page[len(page)-1].ID)
			if err != nil {
				return nil, internalError(s, "read the channel", err)
			}
		}
		if req.Before == 0 {
			pinned, err := s.hub.st.PinnedPosts(ctx, req.ChannelID, maxPinnedPosts)
			if err != nil {
				return nil, internalError(s, "read the channel", err)
			}
			page = mergePosts(pinned, page)
		}
	}

	views, failure := s.postViews(ctx, page, s.UserID())
	if failure != nil {
		return nil, failure
	}
	return protocol.PostListResult{ChannelID: req.ChannelID, Posts: views, HasMore: hasMore}, nil
}

// handlePostUpdate edits what belongs to a post rather than to its writing.
//
// The title and the event are the author's, for the same reason a message is
// only ever edited by whoever wrote it. Locking and pinning are moderation, so
// they need ManageMessages in the channel — and nothing else in this op does.
func handlePostUpdate(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.PostUpdateRequest](raw)
	if failure != nil {
		return nil, failure
	}
	current, channel, failure := s.loadVisiblePost(ctx, req.PostID)
	if failure != nil {
		return nil, failure
	}

	own := current.UserID != nil && *current.UserID == s.UserID()
	if req.Title != nil || req.Event != nil {
		if !own {
			return nil, protocol.Errorf(protocol.ErrForbidden,
				"only the author can edit a post; a moderator can lock or delete it")
		}
	}
	if req.Locked != nil || req.Pinned != nil {
		if failure := s.hub.requireChannelPermission(s, &current.ChannelID, permissions.ManageMessages); failure != nil {
			return nil, failure
		}
	}

	if req.Title != nil {
		title, failure := validatePostTitle(*req.Title)
		if failure != nil {
			return nil, failure
		}
		current.Title = title
	}
	if req.Event != nil {
		event, failure := validatePostEvent(channel.Type, req.Event)
		if failure != nil {
			return nil, failure
		}
		current.StartsAt = &event.StartsAt
		current.EndsAt = event.EndsAt
		current.AllDay = event.AllDay
		current.Location = event.Location
	}
	if req.Locked != nil {
		current.Locked = *req.Locked
	}
	if req.Pinned != nil {
		current.Pinned = *req.Pinned
	}

	if err := s.hub.st.UpdatePost(ctx, current); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such post")
		}
		return nil, internalError(s, "update the post", err)
	}
	updated, err := s.hub.st.PostByID(ctx, req.PostID)
	if err != nil {
		return nil, internalError(s, "update the post", err)
	}

	// The broadcast carries no answer of anybody's: the tallies are the same
	// for everyone, and Own belongs to whoever is being sent the frame.
	shared, failure := s.postViews(ctx, []store.Post{updated}, 0)
	if failure != nil {
		return nil, failure
	}
	s.hub.BroadcastChannelEvent(
		protocol.Event(protocol.EvPostUpdated, protocol.PostEvent{Post: shared[0]}), updated.ChannelID)

	// The reply is the same post seen by the person who asked for it, so their
	// own answer to an event survives their own edit of it.
	mine, failure := s.postViews(ctx, []store.Post{updated}, s.UserID())
	if failure != nil {
		return nil, failure
	}
	return protocol.PostEvent{Post: mine[0]}, nil
}

// handlePostDelete removes a post and its whole thread. Anybody may delete
// their own; deleting somebody else's needs ManageMessages in that channel.
func handlePostDelete(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.PostDeleteRequest](raw)
	if failure != nil {
		return nil, failure
	}
	existing, _, failure := s.loadVisiblePost(ctx, req.PostID)
	if failure != nil {
		return nil, failure
	}

	own := existing.UserID != nil && *existing.UserID == s.UserID()
	if !own {
		if failure := s.hub.requireChannelPermission(s, &existing.ChannelID, permissions.ManageMessages); failure != nil {
			return nil, failure
		}
	}

	// The thread goes with the post through the cascade, so what it held on
	// disk has to be read while those rows still exist.
	attachments, err := s.hub.st.AttachmentsForPost(ctx, existing.ID)
	if err != nil {
		return nil, internalError(s, "read the post", err)
	}
	if err := s.hub.st.DeletePost(ctx, existing.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such post")
		}
		return nil, internalError(s, "delete the post", err)
	}
	s.hub.RemoveFiles(attachments)

	event := protocol.PostDeletedEvent{PostID: existing.ID, ChannelID: existing.ChannelID}
	s.hub.BroadcastChannelEvent(protocol.Event(protocol.EvPostDeleted, event), existing.ChannelID)
	if !own {
		s.log.Info("post deleted by a moderator",
			slog.Int64("post", existing.ID), slog.Int64("channel", existing.ChannelID))
	}

	return event, nil
}

// handlePostRSVP answers a calendar post.
//
// Answering is not writing, so it needs no more than the right to see the
// channel: an event everybody can read is an event everybody can say they are
// coming to.
func handlePostRSVP(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.PostRSVPRequest](raw)
	if failure != nil {
		return nil, failure
	}
	switch req.Response {
	case protocol.RSVPGoing, protocol.RSVPMaybe, protocol.RSVPDeclined, protocol.RSVPNone:
	default:
		return nil, protocol.Errorf(protocol.ErrBadRequest,
			"an answer is going, maybe, declined, or empty to withdraw one")
	}

	post, _, failure := s.loadVisiblePost(ctx, req.PostID)
	if failure != nil {
		return nil, failure
	}
	if !post.Event() {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "that post is not an event")
	}
	// Locking closes a post: no more comments, and no more answers either.
	if post.Locked {
		return nil, protocol.Errorf(protocol.ErrPostLocked, "that event is closed")
	}
	if !s.messages.allow() {
		return nil, protocol.Errorf(protocol.ErrRateLimited, "you are answering too quickly")
	}

	userID := s.UserID()
	var err error
	if req.Response == protocol.RSVPNone {
		err = s.hub.st.DeleteRSVP(ctx, post.ID, userID)
	} else {
		err = s.hub.st.SetRSVP(ctx, post.ID, userID, req.Response)
	}
	if err != nil {
		return nil, internalError(s, "record your answer", err)
	}

	counts, err := s.hub.st.RSVPCountsFor(ctx, []int64{post.ID})
	if err != nil {
		return nil, internalError(s, "record your answer", err)
	}
	event := protocol.PostRSVPEvent{
		PostID:    post.ID,
		ChannelID: post.ChannelID,
		UserID:    userID,
		Response:  req.Response,
		// Own is deliberately empty: the counts are everybody's and the answer
		// is one person's, which UserID above is what names.
		RSVP: rsvpView(counts[post.ID], ""),
	}
	s.hub.BroadcastChannelEvent(protocol.Event(protocol.EvPostRSVP, event), post.ChannelID)

	// The caller gets their own answer back in the summary, which is what a
	// client renders the button state from.
	reply := event
	reply.RSVP.Own = req.Response
	return reply, nil
}

// --- helpers ----------------------------------------------------------------

// requirePostChannel checks that a channel exists, holds posts, and that the
// caller holds want inside it.
func (s *Session) requirePostChannel(channelID int64, want permissions.Permission) (store.Channel, *protocol.Error) {
	channel, ok := s.hub.Channel(channelID)
	if !ok {
		return store.Channel{}, protocol.Errorf(protocol.ErrNotFound, "no such channel")
	}
	if failure := s.hub.requireChannelPermission(s, &channelID, want); failure != nil {
		return store.Channel{}, failure
	}
	// Checked after the permission so an invisible channel reports "not found"
	// rather than leaking its type.
	if !protocol.PostChannel(channel.Type) {
		return store.Channel{}, protocol.Errorf(protocol.ErrBadRequest, "that channel does not hold posts")
	}
	return channel, nil
}

// loadVisiblePost reads a post the caller is allowed to see, along with the
// channel it is in. A post in a channel they cannot see reports "not found",
// exactly as the channel would.
func (s *Session) loadVisiblePost(ctx context.Context, id int64) (store.Post, store.Channel, *protocol.Error) {
	post, err := s.hub.st.PostByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Post{}, store.Channel{}, protocol.Errorf(protocol.ErrNotFound, "no such post")
		}
		return store.Post{}, store.Channel{}, internalError(s, "read the post", err)
	}
	channel, failure := s.requirePostChannel(post.ChannelID, permissions.ViewChannel)
	if failure != nil {
		// The post is only reachable through its channel, so a channel that
		// reports "not found" is what the post reports too.
		return store.Post{}, store.Channel{}, protocol.Errorf(protocol.ErrNotFound, "no such post")
	}
	return post, channel, nil
}

// postViews renders a run of posts: their bodies, the files those carry, the
// shape of each thread, and the answers to the events among them.
//
// viewerID is whose own answer the frames should carry. Zero renders a frame
// for everybody at once, which is what a broadcast needs: the tallies are
// shared, so only Own has to be left out of one.
func (s *Session) postViews(ctx context.Context, posts []store.Post, viewerID int64) ([]protocol.Post, *protocol.Error) {
	if len(posts) == 0 {
		return []protocol.Post{}, nil
	}

	ids := make([]int64, 0, len(posts))
	bodyIDs := make([]int64, 0, len(posts))
	eventIDs := make([]int64, 0, len(posts))
	for _, p := range posts {
		ids = append(ids, p.ID)
		if p.RootMessageID != nil {
			bodyIDs = append(bodyIDs, *p.RootMessageID)
		}
		if p.Event() {
			eventIDs = append(eventIDs, p.ID)
		}
	}

	// The bodies of a whole page are read and rendered as one set, so a
	// listing costs the same handful of queries however long it is.
	bodies, err := s.hub.st.MessagesByID(ctx, bodyIDs)
	if err != nil {
		return nil, internalError(s, "read the posts", err)
	}
	bodyViews, failure := s.messageViews(ctx, bodies, "read the posts")
	if failure != nil {
		return nil, failure
	}
	byID := make(map[int64]protocol.Message, len(bodyViews))
	for _, view := range bodyViews {
		byID[view.ID] = view
	}

	stats, err := s.hub.st.PostStatsFor(ctx, ids)
	if err != nil {
		return nil, internalError(s, "read the posts", err)
	}
	counts, err := s.hub.st.RSVPCountsFor(ctx, eventIDs)
	if err != nil {
		return nil, internalError(s, "read the posts", err)
	}
	var own map[int64]string
	if viewerID != 0 {
		own, err = s.hub.st.RSVPsOf(ctx, viewerID, eventIDs)
		if err != nil {
			return nil, internalError(s, "read the posts", err)
		}
	}

	out := make([]protocol.Post, 0, len(posts))
	for _, p := range posts {
		var body *protocol.Message
		if p.RootMessageID != nil {
			if view, ok := byID[*p.RootMessageID]; ok {
				body = &view
			}
		}
		out = append(out, postView(p, body, stats[p.ID], counts[p.ID], own[p.ID]))
	}
	return out, nil
}

// mergePosts puts the pinned entries of a channel in front of a page of it,
// dropping the ones the page already holds. The page is otherwise untouched:
// its order is the cursor's, and a client sorts what it has.
func mergePosts(pinned, page []store.Post) []store.Post {
	if len(pinned) == 0 {
		return page
	}
	seen := make(map[int64]bool, len(pinned))
	out := make([]store.Post, 0, len(pinned)+len(page))
	for _, p := range pinned {
		seen[p.ID] = true
		out = append(out, p)
	}
	for _, p := range page {
		if seen[p.ID] {
			continue
		}
		out = append(out, p)
	}
	return out
}
