package gateway_test

import (
	"testing"

	"github.com/coder/websocket"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// unreadFor picks one channel out of a snapshot's unread list. A channel with
// nothing waiting is absent from it rather than present as zero, so "not
// found" is the answer for a channel that is fully read.
func unreadFor(ready protocol.Ready, channelID int64) (protocol.ChannelUnread, bool) {
	for _, entry := range ready.Unread {
		if entry.ChannelID == channelID {
			return entry, true
		}
	}
	return protocol.ChannelUnread{}, false
}

func TestUnreadChannelSurvivesReconnecting(t *testing.T) {
	h := newHarness(t, nil)

	alice := h.dial()
	aliceReady := alice.guest("Alice")
	channel := textChannel(t, aliceReady)

	bob := h.dial()
	bobReady := bob.guest("Bob")

	// Nothing has been said since Bob arrived, so his first snapshot is empty
	// however much history the server carries.
	if len(bobReady.Unread) != 0 {
		t.Fatalf("a member who has just arrived has nothing waiting: %+v", bobReady.Unread)
	}

	for _, content := range []string{"one", "two", "three"} {
		ok[protocol.MessageEvent](alice, protocol.OpMessageSend,
			protocol.MessageSendRequest{ChannelID: channel.ID, Content: content})
	}

	// The whole point: Bob goes away and comes back, and what arrived while he
	// was gone is still waiting for him.
	bob.conn.Close(websocket.StatusNormalClosure, "")
	returning := h.dial()
	resumed := ok[protocol.Ready](returning, protocol.OpAuthToken,
		protocol.AuthTokenRequest{Token: bobReady.SessionToken})

	entry, found := unreadFor(resumed, channel.ID)
	if !found {
		t.Fatalf("three messages arrived while Bob was away: %+v", resumed.Unread)
	}
	if entry.Count != 3 {
		t.Fatalf("unread: got %d, want 3", entry.Count)
	}

	read := ok[protocol.ChannelUnread](returning, protocol.OpChannelRead,
		protocol.ChannelReadRequest{ChannelID: channel.ID})
	if read.Count != 0 {
		t.Fatalf("reading should clear the badge: %+v", read)
	}

	// And it stays cleared across the next reconnection, which is the half a
	// client holding the count in memory could never do.
	returning.conn.Close(websocket.StatusNormalClosure, "")
	again := h.dial()
	afterReading := ok[protocol.Ready](again, protocol.OpAuthToken,
		protocol.AuthTokenRequest{Token: bobReady.SessionToken})
	if _, found := unreadFor(afterReading, channel.ID); found {
		t.Fatalf("a channel read to the end must stay read: %+v", afterReading.Unread)
	}
}

func TestUnreadMentionsCarryTheWordsAndNotTheVerdict(t *testing.T) {
	h := newHarness(t, nil)

	alice := h.dial()
	aliceReady := alice.guest("Alice")
	channel := textChannel(t, aliceReady)

	bob := h.dial()
	bobReady := bob.guest("Bob")
	bob.conn.Close(websocket.StatusNormalClosure, "")

	ok[protocol.MessageEvent](alice, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: "nothing to see"})
	named := ok[protocol.MessageEvent](alice, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: "@Bob are you there"})

	returning := h.dial()
	resumed := ok[protocol.Ready](returning, protocol.OpAuthToken,
		protocol.AuthTokenRequest{Token: bobReady.SessionToken})

	if len(resumed.UnreadMentions) != 2 {
		t.Fatalf("both unread messages should travel: %+v", resumed.UnreadMentions)
	}
	// Newest first, so the sample a cap cuts short keeps the recent end.
	first := resumed.UnreadMentions[0]
	if first.Content != named.Message.Content {
		t.Fatalf("newest first: %+v", resumed.UnreadMentions)
	}
	if first.ChannelID != channel.ID || first.UserID != aliceReady.User.ID {
		t.Fatalf("an unread message should name where it landed and who wrote it: %+v", first)
	}
	// The server ships the words and says nothing about whom they name: that
	// is the client's to decide, against the member list it holds.
	if first.ReplyToUserID != 0 {
		t.Fatalf("nothing was being answered: %+v", first)
	}
}

func TestWritingMarksTheChannelRead(t *testing.T) {
	h := newHarness(t, nil)

	alice := h.dial()
	aliceReady := alice.guest("Alice")
	channel := textChannel(t, aliceReady)

	bob := h.dial()
	bobReady := bob.guest("Bob")

	ok[protocol.MessageEvent](alice, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: "anybody about"})
	// Answering is reading: somebody who says something has read what is above
	// it, and a second client of theirs must not light up for their own words.
	ok[protocol.MessageEvent](bob, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: "here"})

	bob.conn.Close(websocket.StatusNormalClosure, "")
	returning := h.dial()
	resumed := ok[protocol.Ready](returning, protocol.OpAuthToken,
		protocol.AuthTokenRequest{Token: bobReady.SessionToken})

	if entry, found := unreadFor(resumed, channel.ID); found {
		t.Fatalf("writing in a channel reads it: %+v", entry)
	}
}

func TestReadMarkerNeverMovesBackwards(t *testing.T) {
	h := newHarness(t, nil)

	alice := h.dial()
	aliceReady := alice.guest("Alice")
	channel := textChannel(t, aliceReady)

	bob := h.dial()
	bob.guest("Bob")

	var first int64
	for _, content := range []string{"one", "two", "three"} {
		sent := ok[protocol.MessageEvent](alice, protocol.OpMessageSend,
			protocol.MessageSendRequest{ChannelID: channel.ID, Content: content})
		if first == 0 {
			first = sent.Message.ID
		}
	}

	read := ok[protocol.ChannelUnread](bob, protocol.OpChannelRead,
		protocol.ChannelReadRequest{ChannelID: channel.ID})
	if read.Count != 0 {
		t.Fatalf("reading should clear the badge: %+v", read)
	}

	// Paging back through old lines must not bring back a badge that reading
	// has already cleared.
	back := ok[protocol.ChannelUnread](bob, protocol.OpChannelRead,
		protocol.ChannelReadRequest{ChannelID: channel.ID, MessageID: first})
	if back.Count != 0 {
		t.Fatalf("a read marker must not move backwards: %+v", back)
	}
}

func TestChannelReadRefusesWhatCarriesNoMessages(t *testing.T) {
	h := newHarness(t, nil)

	c := h.dial()
	ready := c.guest("Alice")

	var voice int64
	for _, channel := range ready.Channels {
		if channel.Type == protocol.ChannelVoice {
			voice = channel.ID
		}
	}
	if voice == 0 {
		t.Fatal("the seeded tree should hold a voice channel")
	}

	c.fails(protocol.OpChannelRead,
		protocol.ChannelReadRequest{ChannelID: voice}, protocol.ErrBadRequest)
	// An invisible channel reports "not found" rather than leaking its type.
	c.fails(protocol.OpChannelRead,
		protocol.ChannelReadRequest{ChannelID: 99999}, protocol.ErrNotFound)
}
