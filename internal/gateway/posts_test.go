package gateway_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
)

// postChannel creates one channel of a post-holding type and returns it. The
// seed installs a text and a voice channel and nothing else, so every test
// here makes the channel it needs.
func (h *harness) postChannel(admin *client, channelType, name string) protocol.Channel {
	h.t.Helper()
	return ok[protocol.ChannelEvent](admin, protocol.OpChannelCreate,
		protocol.ChannelCreateRequest{Name: name, Type: channelType}).Channel
}

// eventAt is a start time far enough from now that a test never trips the
// bounds on how far out an event may be.
func eventAt(offset time.Duration) *protocol.PostEventDetails {
	return &protocol.PostEventDetails{StartsAt: time.Now().Add(offset).Unix()}
}

func TestPostsCanBeStartedInEveryPostChannel(t *testing.T) {
	h := newHarness(t, nil)

	admin, _ := h.admin("Admin")
	for _, channelType := range []string{
		protocol.ChannelAnnouncement, protocol.ChannelForum, protocol.ChannelCalendar,
	} {
		channel := h.postChannel(admin, channelType, channelType)
		req := protocol.PostCreateRequest{
			ChannelID: channel.ID,
			Title:     "  A title  ",
			Content:   "  the body  ",
		}
		if channelType == protocol.ChannelCalendar {
			req.Event = eventAt(48 * time.Hour)
		}

		created := ok[protocol.PostEvent](admin, protocol.OpPostCreate, req).Post
		if created.Title != "A title" {
			t.Fatalf("%s: title should be trimmed: %q", channelType, created.Title)
		}
		if created.Body == nil || created.Body.Content != "the body" {
			t.Fatalf("%s: body: %+v", channelType, created.Body)
		}
		if created.Body.PostID == nil || *created.Body.PostID != created.ID {
			t.Fatalf("%s: the body must belong to the post: %+v", channelType, created.Body)
		}
		if created.Comments != 0 {
			t.Fatalf("%s: a new post has no comments: %d", channelType, created.Comments)
		}
		if created.LastCommentAt != created.CreatedAt {
			t.Fatal("a post nobody answered was last active when it was written")
		}
		if (channelType == protocol.ChannelCalendar) != (created.Event != nil) {
			t.Fatalf("%s: event details: %+v", channelType, created.Event)
		}
	}
}

func TestPostsOnlyGoToPostChannels(t *testing.T) {
	h := newHarness(t, nil)

	c := h.dial()
	ready := c.guest("Alice")

	for _, channel := range ready.Channels {
		c.fails(protocol.OpPostCreate,
			protocol.PostCreateRequest{ChannelID: channel.ID, Title: "Nope", Content: "nope"},
			protocol.ErrBadRequest)
	}
	c.fails(protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: 99999, Title: "Nope", Content: "nope"},
		protocol.ErrNotFound)
}

func TestAPostNeedsATitle(t *testing.T) {
	h := newHarness(t, nil)

	admin, _ := h.admin("Admin")
	channel := h.postChannel(admin, protocol.ChannelForum, "topics")

	admin.fails(protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: channel.ID, Title: "   ", Content: "body"},
		protocol.ErrBadRequest)
	// A post with a title and nothing else is as empty as a message with
	// neither words nor files.
	admin.fails(protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: channel.ID, Title: "Title"},
		protocol.ErrBadRequest)
}

func TestOnlyCalendarPostsHappenAtATime(t *testing.T) {
	h := newHarness(t, nil)

	admin, _ := h.admin("Admin")
	forum := h.postChannel(admin, protocol.ChannelForum, "topics")
	calendar := h.postChannel(admin, protocol.ChannelCalendar, "dates")

	// An event in a channel that renders none is refused rather than stored
	// where nothing would show it.
	admin.fails(protocol.OpPostCreate, protocol.PostCreateRequest{
		ChannelID: forum.ID, Title: "Topic", Content: "body", Event: eventAt(time.Hour),
	}, protocol.ErrBadRequest)

	// A calendar post without one is refused for the mirror reason.
	admin.fails(protocol.OpPostCreate, protocol.PostCreateRequest{
		ChannelID: calendar.ID, Title: "When?", Content: "body",
	}, protocol.ErrBadRequest)

	start := time.Now().Add(24 * time.Hour).Unix()
	ends := start - 1
	admin.fails(protocol.OpPostCreate, protocol.PostCreateRequest{
		ChannelID: calendar.ID, Title: "Backwards", Content: "body",
		Event: &protocol.PostEventDetails{StartsAt: start, EndsAt: &ends},
	}, protocol.ErrBadRequest)

	// A timestamp sent in milliseconds by mistake is refused rather than
	// scheduled thousands of years out.
	admin.fails(protocol.OpPostCreate, protocol.PostCreateRequest{
		ChannelID: calendar.ID, Title: "Millis", Content: "body",
		Event: &protocol.PostEventDetails{StartsAt: start * 1000},
	}, protocol.ErrBadRequest)
}

func TestAMediaPostNeedsAFile(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	admin, ready := h.admin("Admin")
	channel := h.postChannel(admin, protocol.ChannelMedia, "gallery")

	admin.fails(protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: channel.ID, Title: "Nothing to see", Content: "words"},
		protocol.ErrBadRequest)

	// Uploading into a channel that holds posts is allowed: the file is going
	// into an entry, which is not a message in the channel timeline.
	attachment := h.uploadOK(ready.SessionToken, channel.ID, "shot.png", pngBytes(t, 32, 16))
	created := ok[protocol.PostEvent](admin, protocol.OpPostCreate, protocol.PostCreateRequest{
		ChannelID:   channel.ID,
		Title:       "A screenshot",
		Attachments: []int64{attachment.ID},
	}).Post

	if created.Body == nil || len(created.Body.Attachments) != 1 {
		t.Fatalf("the file should hang off the body: %+v", created.Body)
	}
	if created.Body.Content != "" {
		t.Fatal("a media post says everything with its file")
	}
}

func TestStartingAPostNeedsCreatePosts(t *testing.T) {
	h := newHarness(t, nil)

	admin, ready := h.admin("Admin")
	channel := h.postChannel(admin, protocol.ChannelAnnouncement, "news")
	everyone := everyoneRole(t, ready)

	// This is how an announcement channel is made: everybody may comment,
	// nobody but a moderator may write an entry.
	ok[protocol.ChannelEvent](admin, protocol.OpChannelUpdate, protocol.ChannelUpdateRequest{
		ChannelID: channel.ID,
		Overwrites: []protocol.Overwrite{{
			RoleID: everyone,
			Deny:   permissions.CreatePosts.String(),
		}},
	})

	guest := h.dial()
	guest.guest("Guest")
	guest.fails(protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: channel.ID, Title: "Mine", Content: "body"},
		protocol.ErrForbidden)

	announcement := ok[protocol.PostEvent](admin, protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: channel.ID, Title: "Downtime", Content: "on friday"}).Post

	// Commenting is SendMessages, which the overwrite left alone.
	ok[protocol.MessageEvent](guest, protocol.OpMessageSend, protocol.MessageSendRequest{
		ChannelID: channel.ID, PostID: announcement.ID, Content: "noted",
	})
}

func TestCommentsBelongToTheirPost(t *testing.T) {
	h := newHarness(t, nil)

	admin, _ := h.admin("Admin")
	channel := h.postChannel(admin, protocol.ChannelForum, "topics")

	first := ok[protocol.PostEvent](admin, protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: channel.ID, Title: "First", Content: "body"}).Post
	second := ok[protocol.PostEvent](admin, protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: channel.ID, Title: "Second", Content: "body"}).Post

	comment := ok[protocol.MessageEvent](admin, protocol.OpMessageSend, protocol.MessageSendRequest{
		ChannelID: channel.ID, PostID: first.ID, Content: "a reply",
	}).Message
	if comment.PostID == nil || *comment.PostID != first.ID {
		t.Fatalf("comment does not name its post: %+v", comment)
	}

	// The thread holds the comment, and not the body it hangs off.
	thread := ok[protocol.MessageHistoryResult](admin, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID, PostID: first.ID})
	if len(thread.Messages) != 1 || thread.Messages[0].ID != comment.ID {
		t.Fatalf("thread: %+v", thread.Messages)
	}
	if thread.PostID != first.ID {
		t.Fatalf("the page should say which thread answered: %d", thread.PostID)
	}

	// The neighbouring thread is untouched, and so is the channel timeline:
	// a comment is not a line of the channel.
	empty := ok[protocol.MessageHistoryResult](admin, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID, PostID: second.ID})
	if len(empty.Messages) != 0 {
		t.Fatalf("second thread: %+v", empty.Messages)
	}

	listed := ok[protocol.PostListResult](admin, protocol.OpPostList,
		protocol.PostListRequest{ChannelID: channel.ID})
	if len(listed.Posts) != 2 {
		t.Fatalf("listing: %+v", listed.Posts)
	}
	for _, post := range listed.Posts {
		want := 0
		if post.ID == first.ID {
			want = 1
		}
		if post.Comments != want {
			t.Fatalf("post %d: comments %d, want %d", post.ID, post.Comments, want)
		}
	}

	// A comment aimed at a post in another channel is refused rather than
	// written into a thread nobody looking at that channel would see.
	other := h.postChannel(admin, protocol.ChannelForum, "elsewhere")
	admin.fails(protocol.OpMessageSend, protocol.MessageSendRequest{
		ChannelID: other.ID, PostID: first.ID, Content: "misfiled",
	}, protocol.ErrBadRequest)
}

func TestTheBodyOfAPostIsNotDeletedOnItsOwn(t *testing.T) {
	h := newHarness(t, nil)

	admin, _ := h.admin("Admin")
	channel := h.postChannel(admin, protocol.ChannelForum, "topics")
	post := ok[protocol.PostEvent](admin, protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: channel.ID, Title: "Topic", Content: "body"}).Post

	admin.fails(protocol.OpMessageDelete,
		protocol.MessageDeleteRequest{MessageID: post.Body.ID}, protocol.ErrBadRequest)

	// Editing it is another matter: that is the author rewriting their own
	// words, which is what message.edit is for.
	edited := ok[protocol.MessageEvent](admin, protocol.OpMessageEdit,
		protocol.MessageEditRequest{MessageID: post.Body.ID, Content: "a better body"}).Message
	if edited.Content != "a better body" {
		t.Fatalf("edited body: %q", edited.Content)
	}
}

func TestLockingAPostClosesIt(t *testing.T) {
	h := newHarness(t, nil)

	admin, _ := h.admin("Admin")
	channel := h.postChannel(admin, protocol.ChannelForum, "topics")
	post := ok[protocol.PostEvent](admin, protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: channel.ID, Title: "Topic", Content: "body"}).Post

	guest := h.dial()
	guest.guest("Guest")

	locked := true
	guest.fails(protocol.OpPostUpdate,
		protocol.PostUpdateRequest{PostID: post.ID, Locked: &locked}, protocol.ErrForbidden)

	// Nor may somebody who is not the author rewrite the title.
	title := "Hijacked"
	guest.fails(protocol.OpPostUpdate,
		protocol.PostUpdateRequest{PostID: post.ID, Title: &title}, protocol.ErrForbidden)

	updated := ok[protocol.PostEvent](admin, protocol.OpPostUpdate,
		protocol.PostUpdateRequest{PostID: post.ID, Locked: &locked}).Post
	if !updated.Locked {
		t.Fatal("the post should be locked")
	}

	guest.fails(protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, PostID: post.ID, Content: "one more"},
		protocol.ErrPostLocked)
}

func TestPinnedPostsTravelWithTheFirstPage(t *testing.T) {
	h := newHarness(t, nil)

	admin, _ := h.admin("Admin")
	channel := h.postChannel(admin, protocol.ChannelForum, "topics")

	oldest := ok[protocol.PostEvent](admin, protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: channel.ID, Title: "Rules", Content: "read me"}).Post
	for i := range 3 {
		ok[protocol.PostEvent](admin, protocol.OpPostCreate, protocol.PostCreateRequest{
			ChannelID: channel.ID, Title: "Topic", Content: string(rune('a' + i)),
		})
	}

	pinned := true
	ok[protocol.PostEvent](admin, protocol.OpPostUpdate,
		protocol.PostUpdateRequest{PostID: oldest.ID, Pinned: &pinned})

	// One entry per page, so the pinned one would otherwise be three pages
	// down. It arrives at the top of the first page instead.
	listed := ok[protocol.PostListResult](admin, protocol.OpPostList,
		protocol.PostListRequest{ChannelID: channel.ID, Limit: 1})
	if len(listed.Posts) != 2 {
		t.Fatalf("first page should carry the pinned entry too: %+v", listed.Posts)
	}
	if listed.Posts[0].ID != oldest.ID || !listed.Posts[0].Pinned {
		t.Fatalf("pinned entry should come first: %+v", listed.Posts[0])
	}
	if !listed.HasMore {
		t.Fatal("three more entries remain")
	}

	// Paging back does not repeat it: the cursor is the page's, and the pinned
	// entry was never part of that order.
	next := ok[protocol.PostListResult](admin, protocol.OpPostList,
		protocol.PostListRequest{ChannelID: channel.ID, Limit: 2, Before: listed.Posts[1].ID})
	for _, post := range next.Posts {
		if post.ID == oldest.ID {
			t.Fatal("the pinned entry should not come back on a later page")
		}
	}
}

func TestDeletingAPostTakesItsThread(t *testing.T) {
	h := newHarness(t, nil)

	admin, _ := h.admin("Admin")
	channel := h.postChannel(admin, protocol.ChannelForum, "topics")
	post := ok[protocol.PostEvent](admin, protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: channel.ID, Title: "Topic", Content: "body"}).Post

	guest := h.dial()
	guest.guest("Guest")
	comment := ok[protocol.MessageEvent](guest, protocol.OpMessageSend, protocol.MessageSendRequest{
		ChannelID: channel.ID, PostID: post.ID, Content: "a reply",
	}).Message

	// Somebody else's post is moderation, and needs ManageMessages.
	guest.fails(protocol.OpPostDelete, protocol.PostDeleteRequest{PostID: post.ID}, protocol.ErrForbidden)

	deleted := ok[protocol.PostDeletedEvent](admin, protocol.OpPostDelete,
		protocol.PostDeleteRequest{PostID: post.ID})
	if deleted.PostID != post.ID || deleted.ChannelID != channel.ID {
		t.Fatalf("deleted event: %+v", deleted)
	}

	// The thread went with it, comment and all.
	admin.fails(protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID, PostID: post.ID}, protocol.ErrNotFound)
	admin.fails(protocol.OpMessageEdit,
		protocol.MessageEditRequest{MessageID: comment.ID, Content: "still here?"}, protocol.ErrNotFound)

	listed := ok[protocol.PostListResult](admin, protocol.OpPostList,
		protocol.PostListRequest{ChannelID: channel.ID})
	if len(listed.Posts) != 0 {
		t.Fatalf("listing after the delete: %+v", listed.Posts)
	}
}

func TestAnsweringACalendarPost(t *testing.T) {
	h := newHarness(t, nil)

	admin, _ := h.admin("Admin")
	channel := h.postChannel(admin, protocol.ChannelCalendar, "dates")
	post := ok[protocol.PostEvent](admin, protocol.OpPostCreate, protocol.PostCreateRequest{
		ChannelID: channel.ID, Title: "Game night", Content: "bring dice",
		Event: eventAt(72 * time.Hour),
	}).Post

	if post.RSVP == nil || post.RSVP.Going != 0 || post.RSVP.Own != "" {
		t.Fatalf("a new event has no answers: %+v", post.RSVP)
	}

	guest := h.dial()
	guestReady := guest.guest("Guest")

	answered := ok[protocol.PostRSVPEvent](guest, protocol.OpPostRSVP,
		protocol.PostRSVPRequest{PostID: post.ID, Response: protocol.RSVPGoing})
	if answered.RSVP.Going != 1 || answered.RSVP.Own != protocol.RSVPGoing {
		t.Fatalf("own answer: %+v", answered.RSVP)
	}
	if answered.UserID != guestReady.User.ID {
		t.Fatalf("the event should name who answered: %+v", answered)
	}

	// Everybody else is told the tally, and whose answer it was, but never
	// gets somebody else's answer as their own.
	event := admin.waitEvent(protocol.EvPostRSVP)
	var announced protocol.PostRSVPEvent
	if err := json.Unmarshal(event.Data, &announced); err != nil {
		t.Fatalf("decode rsvp event: %v", err)
	}
	if announced.RSVP.Going != 1 || announced.RSVP.Own != "" {
		t.Fatalf("announced summary: %+v", announced.RSVP)
	}

	// Changing an answer moves it rather than adding another.
	ok[protocol.PostRSVPEvent](guest, protocol.OpPostRSVP,
		protocol.PostRSVPRequest{PostID: post.ID, Response: protocol.RSVPMaybe})
	withdrawn := ok[protocol.PostRSVPEvent](guest, protocol.OpPostRSVP,
		protocol.PostRSVPRequest{PostID: post.ID, Response: protocol.RSVPNone})
	if withdrawn.RSVP.Going != 0 || withdrawn.RSVP.Maybe != 0 {
		t.Fatalf("after withdrawing: %+v", withdrawn.RSVP)
	}

	ok[protocol.PostRSVPEvent](admin, protocol.OpPostRSVP,
		protocol.PostRSVPRequest{PostID: post.ID, Response: protocol.RSVPDeclined})

	// A listing carries the tallies to everybody and each answer to its own
	// author.
	listed := ok[protocol.PostListResult](admin, protocol.OpPostList,
		protocol.PostListRequest{ChannelID: channel.ID})
	if len(listed.Posts) != 1 {
		t.Fatalf("listing: %+v", listed.Posts)
	}
	mine := listed.Posts[0].RSVP
	if mine.Declined != 1 || mine.Own != protocol.RSVPDeclined {
		t.Fatalf("admin's own view: %+v", mine)
	}
	theirs := ok[protocol.PostListResult](guest, protocol.OpPostList,
		protocol.PostListRequest{ChannelID: channel.ID}).Posts[0].RSVP
	if theirs.Declined != 1 || theirs.Own != "" {
		t.Fatalf("the guest should see the tally and no answer of their own: %+v", theirs)
	}

	guest.fails(protocol.OpPostRSVP,
		protocol.PostRSVPRequest{PostID: post.ID, Response: "later"}, protocol.ErrBadRequest)

	// Answering something that does not happen at a time is meaningless.
	forum := h.postChannel(admin, protocol.ChannelForum, "topics")
	topic := ok[protocol.PostEvent](admin, protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: forum.ID, Title: "Topic", Content: "body"}).Post
	guest.fails(protocol.OpPostRSVP,
		protocol.PostRSVPRequest{PostID: topic.ID, Response: protocol.RSVPGoing}, protocol.ErrBadRequest)
}

func TestCalendarPostsAreReadAsAWindow(t *testing.T) {
	h := newHarness(t, nil)

	admin, _ := h.admin("Admin")
	channel := h.postChannel(admin, protocol.ChannelCalendar, "dates")

	soon := ok[protocol.PostEvent](admin, protocol.OpPostCreate, protocol.PostCreateRequest{
		ChannelID: channel.ID, Title: "This week", Content: "soon", Event: eventAt(24 * time.Hour),
	}).Post
	ok[protocol.PostEvent](admin, protocol.OpPostCreate, protocol.PostCreateRequest{
		ChannelID: channel.ID, Title: "Next year", Content: "later",
		Event: eventAt(300 * 24 * time.Hour),
	})

	from := time.Now().Add(-time.Hour).Unix()
	window := ok[protocol.PostListResult](admin, protocol.OpPostList, protocol.PostListRequest{
		ChannelID: channel.ID, From: from, To: from + 7*24*3600,
	})
	if len(window.Posts) != 1 || window.Posts[0].ID != soon.ID {
		t.Fatalf("window: %+v", window.Posts)
	}
	if window.HasMore {
		t.Fatal("a window is bounded by dates, not by what fits in a page")
	}

	// A window wider than a calendar ever renders is refused, and so is one
	// asked of a channel that is not a calendar.
	admin.fails(protocol.OpPostList,
		protocol.PostListRequest{ChannelID: channel.ID, From: from, To: from + 500*24*3600},
		protocol.ErrBadRequest)
	forum := h.postChannel(admin, protocol.ChannelForum, "topics")
	admin.fails(protocol.OpPostList,
		protocol.PostListRequest{ChannelID: forum.ID, From: from, To: from + 24*3600},
		protocol.ErrBadRequest)
}

func TestPostsAreInvisibleWithoutTheChannel(t *testing.T) {
	h := newHarness(t, nil)

	admin, ready := h.admin("Admin")
	channel := h.postChannel(admin, protocol.ChannelForum, "private")
	post := ok[protocol.PostEvent](admin, protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: channel.ID, Title: "Topic", Content: "body"}).Post

	ok[protocol.ChannelEvent](admin, protocol.OpChannelUpdate, protocol.ChannelUpdateRequest{
		ChannelID: channel.ID,
		Overwrites: []protocol.Overwrite{{
			RoleID: everyoneRole(t, ready),
			Deny:   permissions.ViewChannel.String(),
		}},
	})

	guest := h.dial()
	guest.guest("Guest")

	// Not "forbidden": that would confirm the post, and the channel it is in,
	// exist at all.
	guest.fails(protocol.OpPostList,
		protocol.PostListRequest{ChannelID: channel.ID}, protocol.ErrNotFound)
	guest.fails(protocol.OpPostUpdate,
		protocol.PostUpdateRequest{PostID: post.ID}, protocol.ErrNotFound)
	guest.fails(protocol.OpPostDelete,
		protocol.PostDeleteRequest{PostID: post.ID}, protocol.ErrNotFound)
	guest.fails(protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID, PostID: post.ID}, protocol.ErrNotFound)
}

func TestChannelTimelinesAndThreadsStaySeparate(t *testing.T) {
	h := newHarness(t, nil)

	admin, ready := h.admin("Admin")
	text := textChannel(t, ready)
	forum := h.postChannel(admin, protocol.ChannelForum, "topics")

	ok[protocol.MessageEvent](admin, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: text.ID, Content: "in the channel"})
	post := ok[protocol.PostEvent](admin, protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: forum.ID, Title: "Topic", Content: "in a thread"}).Post
	ok[protocol.MessageEvent](admin, protocol.OpMessageSend, protocol.MessageSendRequest{
		ChannelID: forum.ID, PostID: post.ID, Content: "a comment",
	})

	timeline := ok[protocol.MessageHistoryResult](admin, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: text.ID})
	if len(timeline.Messages) != 1 || timeline.Messages[0].Content != "in the channel" {
		t.Fatalf("text channel history: %+v", timeline.Messages)
	}
	if timeline.Messages[0].PostID != nil {
		t.Fatal("a message written into a channel belongs to no post")
	}

	// The channel timeline of a forum is not a thing a client asks for: the
	// channel does not carry messages of its own.
	admin.fails(protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: forum.ID}, protocol.ErrBadRequest)

	// A thread is read forwards from its start; jumping into the middle of one
	// is not something a client does.
	admin.fails(protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: forum.ID, PostID: post.ID, Around: post.Body.ID},
		protocol.ErrBadRequest)
}
