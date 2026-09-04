package gateway_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aural-chat/aural-server/internal/gateway"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
)

// textChannel finds the seeded text channel in a ready snapshot.
func textChannel(t *testing.T, ready protocol.Ready) protocol.Channel {
	t.Helper()
	for _, c := range ready.Channels {
		if c.Type == protocol.ChannelText {
			return c
		}
	}
	t.Fatal("the seed should install a text channel")
	return protocol.Channel{}
}

// admin dials a connection, authenticates it and redeems the owner token on
// it. The snapshot is returned because it is the only place a caller can read
// the channel and role ids from.
func (h *harness) admin(nickname string) (*client, protocol.Ready) {
	h.t.Helper()

	token, err := gateway.EnsureOwnerToken(context.Background(), h.store, h.server.Hub())
	if err != nil {
		h.t.Fatalf("ensure owner token: %v", err)
	}
	c := h.dial()
	ready := c.guest(nickname)
	claimed := ok[protocol.UserEvent](c, protocol.OpServerClaimAdmin, protocol.ClaimAdminRequest{Token: token})
	ready.User = claimed.User
	return c, ready
}

// everyoneRole finds the managed everyone role in a ready snapshot.
func everyoneRole(t *testing.T, ready protocol.Ready) int64 {
	t.Helper()
	for _, role := range ready.Roles {
		if role.Managed == protocol.ManagedEveryone {
			return role.ID
		}
	}
	t.Fatal("no everyone role in the snapshot")
	return 0
}

func TestSendingAMessageReachesEveryone(t *testing.T) {
	h := newHarness(t, nil)

	alice := h.dial()
	ready := alice.guest("Alice")
	channel := textChannel(t, ready)

	bob := h.dial()
	bob.guest("Bob")

	sent := ok[protocol.MessageEvent](alice, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: "  hello there  "})

	if sent.Message.Content != "hello there" {
		t.Fatalf("content should be trimmed: %q", sent.Message.Content)
	}
	if sent.Message.Author != "Alice" {
		t.Fatalf("author: got %q, want Alice", sent.Message.Author)
	}
	if sent.Message.UserID == nil || *sent.Message.UserID != ready.User.ID {
		t.Fatalf("message is not attributed to the sender: %+v", sent.Message)
	}
	if sent.Message.EditedAt != nil {
		t.Fatal("a new message must not look edited")
	}

	// Bob was never told which request produced it; he learns from the event.
	event := bob.waitEvent(protocol.EvMessageCreated)
	var announced protocol.MessageEvent
	if err := json.Unmarshal(event.Data, &announced); err != nil {
		t.Fatalf("decode message event: %v", err)
	}
	if announced.Message.ID != sent.Message.ID {
		t.Fatalf("announced the wrong message: %+v", announced.Message)
	}
	if announced.Message.Content != "hello there" {
		t.Fatalf("announced content: %q", announced.Message.Content)
	}
}

func TestMessagesOnlyGoToTextChannels(t *testing.T) {
	h := newHarness(t, nil)

	c := h.dial()
	ready := c.guest("Alice")

	for _, channel := range ready.Channels {
		if channel.Type == protocol.ChannelText {
			continue
		}
		c.fails(protocol.OpMessageSend,
			protocol.MessageSendRequest{ChannelID: channel.ID, Content: "nope"},
			protocol.ErrBadRequest)
	}

	// A channel that does not exist is not found, whatever its type.
	c.fails(protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: 99999, Content: "nope"}, protocol.ErrNotFound)
}

func TestEmptyAndOversizedMessagesAreRejected(t *testing.T) {
	h := newHarness(t, nil)

	c := h.dial()
	channel := textChannel(t, c.guest("Alice"))

	for _, content := range []string{"", "   ", "\n\n\n", strings.Repeat("x", 2001)} {
		c.fails(protocol.OpMessageSend,
			protocol.MessageSendRequest{ChannelID: channel.ID, Content: content},
			protocol.ErrBadRequest)
	}

	// Exactly at the limit is fine.
	ok[protocol.MessageEvent](c, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: strings.Repeat("x", 2000)})
}

func TestMessagesKeepLineBreaksButNotControlCharacters(t *testing.T) {
	h := newHarness(t, nil)

	c := h.dial()
	channel := textChannel(t, c.guest("Alice"))

	sent := ok[protocol.MessageEvent](c, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: "one\ntwo\x07\tthree"})

	want := "one\ntwo three"
	if sent.Message.Content != want {
		t.Fatalf("content: got %q, want %q", sent.Message.Content, want)
	}
}

// Emoji are ordinary text as far as the protocol is concerned, but the
// sanitiser drops control characters, and the sequences that build a family or
// a professional emoji are held together by joiners that a careless filter
// would eat. This pins that they survive a full round trip.
func TestEmojiSurviveIntact(t *testing.T) {
	h := newHarness(t, nil)

	c := h.dial()
	channel := textChannel(t, c.guest("Alice"))

	cases := map[string]string{
		"a plain emoji":              "hello 👋",
		"a variation selector":       "❤️", // red heart, emoji presentation
		"a zero width joiner family": "\U0001F468‍\U0001F469‍\U0001F467",
		"a skin tone modifier":       "\U0001F44D\U0001F3FD",
		"a flag, two regionals":      "\U0001F1E6\U0001F1F7",
		"emoji around text":          "🎉 shipped 🎉",
	}

	for name, content := range cases {
		sent := ok[protocol.MessageEvent](c, protocol.OpMessageSend,
			protocol.MessageSendRequest{ChannelID: channel.ID, Content: content})
		if sent.Message.Content != content {
			t.Errorf("%s: got %q (% x), want %q (% x)",
				name, sent.Message.Content, sent.Message.Content, content, content)
		}
	}

	// And they survive the database, not just the request.
	page := ok[protocol.MessageHistoryResult](c, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID})
	if len(page.Messages) != len(cases) {
		t.Fatalf("history holds %d messages, want %d", len(page.Messages), len(cases))
	}
	stored := map[string]bool{}
	for _, m := range page.Messages {
		stored[m.Content] = true
	}
	for name, content := range cases {
		if !stored[content] {
			t.Errorf("%s did not survive storage: %q", name, content)
		}
	}
}

// An emoji is several runes, so the limit counts it as several characters.
// That is conservative rather than wrong, but it should be deliberate.
func TestEmojiCountTowardsTheLengthLimit(t *testing.T) {
	h := newHarness(t, nil)

	c := h.dial()
	channel := textChannel(t, c.guest("Alice"))

	// 2000 of a single-rune emoji is exactly the limit.
	ok[protocol.MessageEvent](c, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: strings.Repeat("🎉", 2000)})

	c.fails(protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: strings.Repeat("🎉", 2001)},
		protocol.ErrBadRequest)
}

func TestHistoryPagesBackwards(t *testing.T) {
	h := newHarness(t, nil)

	c := h.dial()
	channel := textChannel(t, c.guest("Alice"))

	// More than one page, and more than the burst the limiter allows, which is
	// why this writes through the store rather than the wire.
	ctx := context.Background()
	for i := range 12 {
		if _, err := h.store.CreateMessage(ctx, channel.ID, nil, 1, string(rune('a'+i)), nil); err != nil {
			t.Fatalf("seed message %d: %v", i, err)
		}
	}

	first := ok[protocol.MessageHistoryResult](c, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID, Limit: 5})

	if len(first.Messages) != 5 {
		t.Fatalf("first page: got %d messages, want 5", len(first.Messages))
	}
	if !first.HasMore {
		t.Fatal("first page should report more to come")
	}
	// Oldest first, and the newest five of twelve.
	if first.Messages[0].Content != "h" || first.Messages[4].Content != "l" {
		t.Fatalf("first page contents: %q..%q", first.Messages[0].Content, first.Messages[4].Content)
	}

	second := ok[protocol.MessageHistoryResult](c, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID, Limit: 5, Before: first.Messages[0].ID})

	if len(second.Messages) != 5 {
		t.Fatalf("second page: got %d messages, want 5", len(second.Messages))
	}
	if second.Messages[4].Content != "g" {
		t.Fatalf("second page should end just before the first: %q", second.Messages[4].Content)
	}

	third := ok[protocol.MessageHistoryResult](c, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID, Limit: 5, Before: second.Messages[0].ID})

	if len(third.Messages) != 2 {
		t.Fatalf("third page: got %d messages, want 2", len(third.Messages))
	}
	if third.HasMore {
		t.Fatal("the last page must not claim more remains")
	}
}

func TestHistoryOfAnEmptyChannelIsEmpty(t *testing.T) {
	h := newHarness(t, nil)

	c := h.dial()
	channel := textChannel(t, c.guest("Alice"))

	page := ok[protocol.MessageHistoryResult](c, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID})

	if len(page.Messages) != 0 {
		t.Fatalf("a fresh channel should have no history, got %d", len(page.Messages))
	}
	if page.HasMore {
		t.Fatal("an empty channel cannot have more")
	}
}

func TestOnlyTheAuthorMayEdit(t *testing.T) {
	h := newHarness(t, nil)

	alice := h.dial()
	channel := textChannel(t, alice.guest("Alice"))
	sent := ok[protocol.MessageEvent](alice, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: "original"})

	edited := ok[protocol.MessageEvent](alice, protocol.OpMessageEdit,
		protocol.MessageEditRequest{MessageID: sent.Message.ID, Content: "corrected"})

	if edited.Message.Content != "corrected" {
		t.Fatalf("content after edit: %q", edited.Message.Content)
	}
	if edited.Message.EditedAt == nil {
		t.Fatal("an edited message must be stamped as edited")
	}
	if edited.Message.ID != sent.Message.ID {
		t.Fatal("editing must not mint a new message")
	}

	// Not even an administrator may rewrite what somebody else wrote.
	admin, _ := h.admin("Admin")
	admin.fails(protocol.OpMessageEdit,
		protocol.MessageEditRequest{MessageID: sent.Message.ID, Content: "hijacked"},
		protocol.ErrForbidden)
}

func TestDeletingOwnAndOthersMessages(t *testing.T) {
	h := newHarness(t, nil)

	alice := h.dial()
	channel := textChannel(t, alice.guest("Alice"))

	own := ok[protocol.MessageEvent](alice, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: "mine to remove"})
	target := ok[protocol.MessageEvent](alice, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: "moderated away"})

	// Bob watches from the start, so both deletions reach him as events.
	bob := h.dial()
	bob.guest("Bob")

	// Anybody may remove their own, with no permission at all.
	removed := ok[protocol.MessageDeletedEvent](alice, protocol.OpMessageDelete,
		protocol.MessageDeleteRequest{MessageID: own.Message.ID})
	if removed.MessageID != own.Message.ID || removed.ChannelID != channel.ID {
		t.Fatalf("delete event: %+v", removed)
	}
	alice.fails(protocol.OpMessageDelete,
		protocol.MessageDeleteRequest{MessageID: own.Message.ID}, protocol.ErrNotFound)

	// Somebody else's needs ManageMessages, which a plain guest lacks.
	bob.fails(protocol.OpMessageDelete,
		protocol.MessageDeleteRequest{MessageID: target.Message.ID}, protocol.ErrForbidden)

	admin, _ := h.admin("Admin")
	ok[protocol.MessageDeletedEvent](admin, protocol.OpMessageDelete,
		protocol.MessageDeleteRequest{MessageID: target.Message.ID})

	// Bob watched both deletions happen without asking for either. His own
	// connection did nothing, so the events are the only way he could know.
	if got := bob.waitDeletion(); got != own.Message.ID {
		t.Fatalf("first deletion announced: got %d, want %d", got, own.Message.ID)
	}
	if got := bob.waitDeletion(); got != target.Message.ID {
		t.Fatalf("second deletion announced: got %d, want %d", got, target.Message.ID)
	}
}

// waitDeletion reads the next message.deleted event and returns its id.
func (c *client) waitDeletion() int64 {
	c.t.Helper()

	event := c.waitEvent(protocol.EvMessageDeleted)
	var announced protocol.MessageDeletedEvent
	if err := json.Unmarshal(event.Data, &announced); err != nil {
		c.t.Fatalf("decode delete event: %v", err)
	}
	return announced.MessageID
}

func TestMessagingNeedsSendMessages(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	admin, ready := h.admin("Admin")
	channel := textChannel(t, ready)

	// Take SendMessages away from everyone, server-wide.
	everyone := everyoneRole(t, ready)
	stripped := permissions.DefaultEveryone &^ permissions.SendMessages
	ok[protocol.RoleEvent](admin, protocol.OpRoleUpdate, protocol.RoleUpdateRequest{
		RoleID:      everyone,
		Permissions: ptr(stripped.String()),
	})

	guest := h.dial()
	guest.guest("Guest")
	guest.fails(protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: "silenced"}, protocol.ErrForbidden)

	// Reading is governed by ViewChannel, not by SendMessages, so a muted
	// member can still follow the conversation.
	if _, err := h.store.CreateMessage(ctx, channel.ID, nil, ready.User.ID, "still readable", nil); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	page := ok[protocol.MessageHistoryResult](guest, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID})
	if len(page.Messages) != 1 || page.Messages[0].Content != "still readable" {
		t.Fatalf("a muted member should still read history, got %+v", page.Messages)
	}
}

func TestPostingIsRateLimited(t *testing.T) {
	h := newHarness(t, nil)

	c := h.dial()
	channel := textChannel(t, c.guest("Flooder"))

	// The bucket holds messageBurst tokens and refills far slower than this
	// loop runs, so a flood must be refused before it gets far.
	limited := false
	for i := range 40 {
		env := c.do(protocol.OpMessageSend,
			protocol.MessageSendRequest{ChannelID: channel.ID, Content: "flood"})
		if env.Op == protocol.OpError {
			if env.Error.Code != protocol.ErrRateLimited {
				t.Fatalf("message %d failed with %s: %s", i, env.Error.Code, env.Error.Message)
			}
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("a flood of 40 messages should have been throttled")
	}
}

func TestMessagesAreHiddenWithTheirChannel(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	admin, ready := h.admin("Admin")
	channel := textChannel(t, ready)

	sent, err := h.store.CreateMessage(ctx, channel.ID, nil, ready.User.ID, "secret", nil)
	if err != nil {
		t.Fatalf("seed message: %v", err)
	}
	everyone := everyoneRole(t, ready)

	// Hide the channel from everyone.
	ok[protocol.ChannelEvent](admin, protocol.OpChannelUpdate, protocol.ChannelUpdateRequest{
		ChannelID: channel.ID,
		Overwrites: []protocol.Overwrite{{
			RoleID: everyone,
			Allow:  permissions.None.String(),
			Deny:   permissions.ViewChannel.String(),
		}},
	})

	guest := h.dial()
	guest.guest("Guest")

	// The channel is not in the snapshot, and neither its history nor a
	// message inside it may be reached: both report not found rather than
	// forbidden, which would confirm they exist.
	guest.fails(protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID}, protocol.ErrNotFound)
	guest.fails(protocol.OpMessageDelete,
		protocol.MessageDeleteRequest{MessageID: sent.ID}, protocol.ErrNotFound)
	guest.fails(protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: "hello?"}, protocol.ErrNotFound)
}

func TestDeletingAChannelTakesItsMessages(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	admin, ready := h.admin("Admin")
	channel := textChannel(t, ready)

	sent, err := h.store.CreateMessage(ctx, channel.ID, nil, ready.User.ID, "doomed", nil)
	if err != nil {
		t.Fatalf("seed message: %v", err)
	}

	ok[protocol.ChannelDeletedEvent](admin, protocol.OpChannelDelete,
		protocol.ChannelDeleteRequest{ChannelID: channel.ID})

	if _, err := h.store.MessageByID(ctx, sent.ID); err == nil {
		t.Fatal("deleting a channel must take its messages with it")
	}
}

func TestMessageReplies(t *testing.T) {
	h := newHarness(t, nil)

	alice := h.dial()
	aliceReady := alice.guest("Alice")
	channel := textChannel(t, aliceReady)

	bob := h.dial()
	bob.guest("Bob")

	first := ok[protocol.MessageEvent](alice, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: "Hello world"})
	if first.Message.ID == 0 {
		t.Fatal("expected message ID")
	}

	// Bob replies to Alice's message.
	reply := ok[protocol.MessageEvent](bob, protocol.OpMessageSend,
		protocol.MessageSendRequest{
			ChannelID: channel.ID,
			Content:   "Hello Alice!",
			ReplyToID: &first.Message.ID,
		})

	if reply.Message.ReplyToID == nil || *reply.Message.ReplyToID != first.Message.ID {
		t.Fatalf("expected replyToId=%d, got %v", first.Message.ID, reply.Message.ReplyToID)
	}
	if reply.Message.ReplyTo == nil {
		t.Fatal("expected replyTo snapshot")
	}
	if reply.Message.ReplyTo.Author != "Alice" || reply.Message.ReplyTo.Content != "Hello world" {
		t.Fatalf("unexpected replyTo: %+v", reply.Message.ReplyTo)
	}

	// Cannot reply to non-existent message
	bob.fails(protocol.OpMessageSend,
		protocol.MessageSendRequest{
			ChannelID: channel.ID,
			Content:   "Ghost reply",
			ReplyToID: ptr[int64](999999),
		}, protocol.ErrNotFound)

	// History returns the reply with snapshot
	hist := ok[protocol.MessageHistoryResult](alice, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID})
	if len(hist.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(hist.Messages))
	}
	if hist.Messages[1].ReplyTo == nil || hist.Messages[1].ReplyTo.Author != "Alice" {
		t.Fatalf("expected history reply snapshot, got %+v", hist.Messages[1].ReplyTo)
	}

	// When original is deleted, replyTo reports Deleted: true
	ok[protocol.MessageDeletedEvent](alice, protocol.OpMessageDelete,
		protocol.MessageDeleteRequest{MessageID: first.Message.ID})

	histAfter := ok[protocol.MessageHistoryResult](bob, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID})
	if len(histAfter.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(histAfter.Messages))
	}
	if histAfter.Messages[0].ReplyTo == nil || !histAfter.Messages[0].ReplyTo.Deleted {
		t.Fatalf("expected replyTo.Deleted=true, got %+v", histAfter.Messages[0].ReplyTo)
	}
}

// A reply may only point into the run it is written in. The preview a client
// draws is a link into the list it is already reading, so a reference out of
// that list is one it could never follow.
func TestReplyStaysInsideItsOwnRun(t *testing.T) {
	h := newHarness(t, nil)

	admin, ready := h.admin("Admin")
	channel := textChannel(t, ready)
	other := ok[protocol.ChannelEvent](admin, protocol.OpChannelCreate,
		protocol.ChannelCreateRequest{Name: "elsewhere", Type: protocol.ChannelText}).Channel

	elsewhere := ok[protocol.MessageEvent](admin, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: other.ID, Content: "over here"})

	admin.fails(protocol.OpMessageSend, protocol.MessageSendRequest{
		ChannelID: channel.ID,
		Content:   "answering another channel",
		ReplyToID: &elsewhere.Message.ID,
	}, protocol.ErrBadRequest)

	// A post's comments are messages in the post's channel, so the channel
	// alone does not say whether two of them are in the same list.
	forum := h.postChannel(admin, protocol.ChannelForum, "forum")
	first := ok[protocol.PostEvent](admin, protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: forum.ID, Title: "First", Content: "body"}).Post
	second := ok[protocol.PostEvent](admin, protocol.OpPostCreate,
		protocol.PostCreateRequest{ChannelID: forum.ID, Title: "Second", Content: "body"}).Post

	comment := ok[protocol.MessageEvent](admin, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: forum.ID, PostID: first.ID, Content: "a comment"})

	admin.fails(protocol.OpMessageSend, protocol.MessageSendRequest{
		ChannelID: forum.ID,
		PostID:    second.ID,
		Content:   "answering another post",
		ReplyToID: &comment.Message.ID,
	}, protocol.ErrBadRequest)

	// Inside the one post it is an ordinary reply.
	reply := ok[protocol.MessageEvent](admin, protocol.OpMessageSend,
		protocol.MessageSendRequest{
			ChannelID: forum.ID,
			PostID:    first.ID,
			Content:   "answering in place",
			ReplyToID: &comment.Message.ID,
		})
	if reply.Message.ReplyTo == nil || reply.Message.ReplyTo.Content != "a comment" {
		t.Fatalf("unexpected replyTo: %+v", reply.Message.ReplyTo)
	}
}

// A reference carries a preview's worth of the message it points at, not the
// whole of it: the reply is drawn as one line, and the rest would be paid for
// on every frame it appears in.
func TestReplyReferenceIsCutToAPreview(t *testing.T) {
	h := newHarness(t, nil)

	admin, ready := h.admin("Admin")
	channel := textChannel(t, ready)

	long := strings.Repeat("x", 2000)
	first := ok[protocol.MessageEvent](admin, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: long})

	reply := ok[protocol.MessageEvent](admin, protocol.OpMessageSend,
		protocol.MessageSendRequest{
			ChannelID: channel.ID,
			Content:   "short",
			ReplyToID: &first.Message.ID,
		})
	if reply.Message.ReplyTo == nil {
		t.Fatal("expected replyTo snapshot")
	}
	if n := len([]rune(reply.Message.ReplyTo.Content)); n >= len(long) {
		t.Fatalf("the reference should be cut to a preview, got %d runes", n)
	}
}

func ptr[T any](v T) *T { return &v }
