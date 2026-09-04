package gateway_test

import (
	"encoding/json"
	"testing"

	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// registered claims the connection's identity, which is what the "registered
// only" half of the privacy setting turns on.
func (c *client) registered(username string) protocol.User {
	c.t.Helper()
	claimed := ok[protocol.AuthRegisterResult](c, protocol.OpAuthRegister,
		protocol.AuthRegisterRequest{Username: username, Password: "correct horse battery"})
	return claimed.User
}

// setDMPrivacy changes who this identity will hear from privately.
func (c *client) setDMPrivacy(privacy string) {
	c.t.Helper()
	ok[protocol.UserEvent](c, protocol.OpUserUpdate, protocol.UserUpdateRequest{DMPrivacy: &privacy})
}

func dmEvent[T any](t *testing.T, c *client, op string) T {
	t.Helper()
	env := c.waitEvent(op)
	var out T
	if err := json.Unmarshal(env.Data, &out); err != nil {
		t.Fatalf("decode %s: %v", op, err)
	}
	return out
}

func TestDirectMessageReachesBothSidesUnderTheOtherName(t *testing.T) {
	h := newHarness(t, nil)

	alice := h.dial()
	aliceReady := alice.guest("Alice")
	bob := h.dial()
	bobReady := bob.guest("Bob")

	sent := ok[protocol.DMCreatedEvent](alice, protocol.OpDMSend,
		protocol.DMSendRequest{UserID: bobReady.User.ID, Content: "  are you there  "})

	if sent.Message.Content != "are you there" {
		t.Fatalf("content should be trimmed: %q", sent.Message.Content)
	}
	// A conversation is named by the other person, so the same thread reaches
	// the two of them under two different names.
	if sent.Conversation.UserID != bobReady.User.ID {
		t.Fatalf("the sender's copy should name Bob: %+v", sent.Conversation)
	}
	if sent.Conversation.Unread != 0 {
		t.Fatalf("your own message must never be unread: %+v", sent.Conversation)
	}

	arrived := dmEvent[protocol.DMCreatedEvent](t, bob, protocol.EvDMCreated)
	if arrived.Message.ID != sent.Message.ID {
		t.Fatalf("the wrong message arrived: %+v", arrived.Message)
	}
	if arrived.Conversation.ID != sent.Conversation.ID {
		t.Fatalf("the two sides disagree about the thread: %d and %d",
			arrived.Conversation.ID, sent.Conversation.ID)
	}
	if arrived.Conversation.UserID != aliceReady.User.ID {
		t.Fatalf("the recipient's copy should name Alice: %+v", arrived.Conversation)
	}
	if arrived.Conversation.Unread != 1 {
		t.Fatalf("unread: got %d, want 1", arrived.Conversation.Unread)
	}
}

func TestConversationHistoryAndReadMarker(t *testing.T) {
	h := newHarness(t, nil)

	alice := h.dial()
	aliceReady := alice.guest("Alice")
	bob := h.dial()
	bobReady := bob.guest("Bob")

	// Nothing has been said yet: an empty thread rather than an error, which is
	// what a client draws when somebody clicks a name in the member list.
	empty := ok[protocol.DMHistoryResult](bob, protocol.OpDMHistory,
		protocol.DMHistoryRequest{UserID: aliceReady.User.ID})
	if empty.ConversationID != 0 || len(empty.Messages) != 0 {
		t.Fatalf("an unopened conversation should be empty: %+v", empty)
	}

	var last int64
	for _, line := range []string{"one", "two", "three"} {
		sent := ok[protocol.DMCreatedEvent](alice, protocol.OpDMSend,
			protocol.DMSendRequest{UserID: bobReady.User.ID, Content: line})
		last = sent.Message.ID
	}

	page := ok[protocol.DMHistoryResult](bob, protocol.OpDMHistory,
		protocol.DMHistoryRequest{UserID: aliceReady.User.ID})
	if len(page.Messages) != 3 {
		t.Fatalf("history: got %d messages, want 3", len(page.Messages))
	}
	// Oldest first, the order it is rendered in.
	if page.Messages[0].Content != "one" || page.Messages[2].Content != "three" {
		t.Fatalf("history is out of order: %+v", page.Messages)
	}
	if page.HasMore || page.HasMoreAfter {
		t.Fatalf("a three line conversation has no further pages: %+v", page)
	}

	list := ok[protocol.DMListResult](bob, protocol.OpDMList, protocol.DMListRequest{})
	if len(list.Conversations) != 1 {
		t.Fatalf("conversations: got %d, want 1", len(list.Conversations))
	}
	if list.Conversations[0].Unread != 3 {
		t.Fatalf("unread: got %d, want 3", list.Conversations[0].Unread)
	}
	if list.Conversations[0].LastMessage == nil || list.Conversations[0].LastMessage.Content != "three" {
		t.Fatalf("the preview should be the last line: %+v", list.Conversations[0])
	}

	read := ok[protocol.Conversation](bob, protocol.OpDMRead,
		protocol.DMReadRequest{UserID: aliceReady.User.ID, MessageID: last})
	if read.Unread != 0 {
		t.Fatalf("reading should clear the badge: %+v", read)
	}

	// The marker never moves backwards, so paging through old lines cannot
	// bring back a badge that reading has already cleared.
	back := ok[protocol.Conversation](bob, protocol.OpDMRead,
		protocol.DMReadRequest{UserID: aliceReady.User.ID, MessageID: 1})
	if back.Unread != 0 {
		t.Fatalf("a read marker must not move backwards: %+v", back)
	}
}

func TestPrivacyRefusesFromBothSides(t *testing.T) {
	h := newHarness(t, nil)

	alice := h.dial()
	aliceReady := alice.guest("Alice")
	bob := h.dial()
	bobReady := bob.guest("Bob")

	bob.setDMPrivacy(store.DMNone)
	alice.fails(protocol.OpDMSend,
		protocol.DMSendRequest{UserID: bobReady.User.ID, Content: "hello"}, protocol.ErrDMBlocked)

	// The same setting stops the person who set it writing out, which is what
	// keeps somebody from opening a thread nobody may answer.
	bob.fails(protocol.OpDMSend,
		protocol.DMSendRequest{UserID: aliceReady.User.ID, Content: "hello"}, protocol.ErrDMBlocked)

	bob.setDMPrivacy(store.DMRegistered)
	alice.fails(protocol.OpDMSend,
		protocol.DMSendRequest{UserID: bobReady.User.ID, Content: "hello"}, protocol.ErrDMBlocked)

	// Claiming an account is what gets Alice through that door.
	alice.registered("alice")
	ok[protocol.DMCreatedEvent](alice, protocol.OpDMSend,
		protocol.DMSendRequest{UserID: bobReady.User.ID, Content: "hello"})
}

func TestPrivacySettingStaysWithItsOwner(t *testing.T) {
	h := newHarness(t, nil)

	alice := h.dial()
	alice.guest("Alice")
	bob := h.dial()
	bobReady := bob.guest("Bob")
	bob.setDMPrivacy(store.DMNone)

	// Alice reconnects so her snapshot is built after Bob's change.
	fresh := h.dial()
	ready := fresh.guest("Alice again")

	if ready.User.DMPrivacy != store.DMEveryone {
		t.Fatalf("your own setting should reach you: %q", ready.User.DMPrivacy)
	}
	for _, u := range ready.Users {
		if u.ID == bobReady.User.ID && u.DMPrivacy != "" {
			t.Fatalf("somebody else's privacy setting leaked: %q", u.DMPrivacy)
		}
	}

	// Nor may it be set for somebody else, whatever the caller holds.
	admin, _ := h.admin("Root")
	privacy := store.DMNone
	admin.fails(protocol.OpUserUpdate,
		protocol.UserUpdateRequest{UserID: &bobReady.User.ID, DMPrivacy: &privacy}, protocol.ErrForbidden)
}

func TestEditingAndDeletingAPrivateLine(t *testing.T) {
	h := newHarness(t, nil)

	alice := h.dial()
	aliceReady := alice.guest("Alice")
	bob := h.dial()
	bobReady := bob.guest("Bob")

	sent := ok[protocol.DMCreatedEvent](alice, protocol.OpDMSend,
		protocol.DMSendRequest{UserID: bobReady.User.ID, Content: "frst"})
	dmEvent[protocol.DMCreatedEvent](t, bob, protocol.EvDMCreated)

	ok[protocol.DMUpdatedEvent](alice, protocol.OpDMEdit,
		protocol.DMEditRequest{MessageID: sent.Message.ID, Content: "first"})
	edited := dmEvent[protocol.DMUpdatedEvent](t, bob, protocol.EvDMUpdated)
	if edited.Message.Content != "first" {
		t.Fatalf("the edit did not reach the other side: %+v", edited.Message)
	}
	if edited.UserID != aliceReady.User.ID {
		t.Fatalf("the event should name Alice to Bob: %+v", edited)
	}
	if edited.Message.EditedAt == nil {
		t.Fatal("an edited message must say so")
	}

	// There is no moderator in a private conversation: the recipient may not
	// rewrite or remove what was said to them.
	bob.fails(protocol.OpDMEdit,
		protocol.DMEditRequest{MessageID: sent.Message.ID, Content: "no"}, protocol.ErrForbidden)
	bob.fails(protocol.OpDMDelete,
		protocol.DMDeleteRequest{MessageID: sent.Message.ID}, protocol.ErrForbidden)

	ok[protocol.DMDeletedEvent](alice, protocol.OpDMDelete,
		protocol.DMDeleteRequest{MessageID: sent.Message.ID})
	gone := dmEvent[protocol.DMDeletedEvent](t, bob, protocol.EvDMDeleted)
	if gone.MessageID != sent.Message.ID {
		t.Fatalf("the wrong message was removed: %+v", gone)
	}
}

func TestAConversationIsNotOverheard(t *testing.T) {
	h := newHarness(t, nil)

	alice := h.dial()
	alice.guest("Alice")
	bob := h.dial()
	bobReady := bob.guest("Bob")
	eve := h.dial()
	eve.guest("Eve")

	sent := ok[protocol.DMCreatedEvent](alice, protocol.OpDMSend,
		protocol.DMSendRequest{UserID: bobReady.User.ID, Content: "between us"})

	// A message somebody is not a party to reports "not found": that it exists
	// at all is not theirs to learn.
	eve.fails(protocol.OpDMEdit,
		protocol.DMEditRequest{MessageID: sent.Message.ID, Content: "mine now"}, protocol.ErrNotFound)
	eve.fails(protocol.OpDMDelete,
		protocol.DMDeleteRequest{MessageID: sent.Message.ID}, protocol.ErrNotFound)

	list := ok[protocol.DMListResult](eve, protocol.OpDMList, protocol.DMListRequest{})
	if len(list.Conversations) != 0 {
		t.Fatalf("a bystander holds no conversations: %+v", list.Conversations)
	}
}

func TestWritingToYourselfIsRefused(t *testing.T) {
	h := newHarness(t, nil)

	alice := h.dial()
	ready := alice.guest("Alice")

	alice.fails(protocol.OpDMSend,
		protocol.DMSendRequest{UserID: ready.User.ID, Content: "note to self"}, protocol.ErrBadRequest)
	alice.fails(protocol.OpDMSend,
		protocol.DMSendRequest{UserID: 9999, Content: "hello?"}, protocol.ErrNotFound)
}

func TestPermissionGatesPrivateMessages(t *testing.T) {
	h := newHarness(t, nil)

	admin, ready := h.admin("Root")
	bob := h.dial()
	bob.guest("Bob")

	stripped := permissions.DefaultEveryone &^ permissions.SendDirectMessages
	ok[protocol.RoleEvent](admin, protocol.OpRoleUpdate, protocol.RoleUpdateRequest{
		RoleID:      everyoneRole(t, ready),
		Permissions: ptr(stripped.String()),
	})
	bob.waitEvent(protocol.EvRoleUpdated)

	bob.fails(protocol.OpDMSend,
		protocol.DMSendRequest{UserID: ready.User.ID, Content: "hello"}, protocol.ErrForbidden)
}

func TestServerCanCarryNoPrivateMessages(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) { cfg.Server.AllowDirectMessages = false })

	alice := h.dial()
	ready := alice.guest("Alice")
	bob := h.dial()
	bobReady := bob.guest("Bob")

	if ready.Server.DirectMessages {
		t.Fatal("the preview should say this server carries none")
	}
	if len(ready.Conversations) != 0 {
		t.Fatalf("no conversations should be listed: %+v", ready.Conversations)
	}
	alice.fails(protocol.OpDMSend,
		protocol.DMSendRequest{UserID: bobReady.User.ID, Content: "hello"}, protocol.ErrDMDisabled)
	alice.fails(protocol.OpDMList, protocol.DMListRequest{}, protocol.ErrDMDisabled)
}

func TestDirectMessageReplies(t *testing.T) {
	h := newHarness(t, nil)

	alice := h.dial()
	aliceReady := alice.guest("Alice")
	bob := h.dial()
	bobReady := bob.guest("Bob")

	first := ok[protocol.DMCreatedEvent](alice, protocol.OpDMSend,
		protocol.DMSendRequest{UserID: bobReady.User.ID, Content: "Private message"})

	reply := ok[protocol.DMCreatedEvent](bob, protocol.OpDMSend,
		protocol.DMSendRequest{
			UserID:    aliceReady.User.ID,
			Content:   "Private reply",
			ReplyToID: &first.Message.ID,
		})

	if reply.Message.ReplyToID == nil || *reply.Message.ReplyToID != first.Message.ID {
		t.Fatalf("expected replyToId=%d, got %v", first.Message.ID, reply.Message.ReplyToID)
	}
	if reply.Message.ReplyTo == nil || reply.Message.ReplyTo.Content != "Private message" {
		t.Fatalf("unexpected DM replyTo snapshot: %+v", reply.Message.ReplyTo)
	}

	// History returns DM with snapshot
	hist := ok[protocol.DMHistoryResult](alice, protocol.OpDMHistory,
		protocol.DMHistoryRequest{UserID: bobReady.User.ID})
	if len(hist.Messages) != 2 {
		t.Fatalf("expected 2 DMs, got %d", len(hist.Messages))
	}
	if hist.Messages[1].ReplyTo == nil || hist.Messages[1].ReplyTo.Content != "Private message" {
		t.Fatalf("expected DM reply snapshot in history, got %+v", hist.Messages[1].ReplyTo)
	}
}
