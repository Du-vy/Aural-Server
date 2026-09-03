package gateway_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aural-chat/aural-server/internal/protocol"
)

func TestKickOnlineUserRevokesAndBroadcastsRemoved(t *testing.T) {
	h := newHarness(t, nil)

	adminClient, _ := h.admin("Admin")

	target := h.dial()
	targetReady := target.guest("BadActor")
	adminClient.waitEvent(protocol.EvUserConnected)

	// Admin kicks the online user
	ok[struct{}](adminClient, protocol.OpUserKick, protocol.UserKickRequest{
		UserID:         targetReady.User.ID,
		Reason:         "Violation of rules",
		DeleteMessages: "none",
	})

	// Admin should receive EvUserRemoved
	env := adminClient.waitEvent(protocol.EvUserRemoved)
	var removed protocol.UserRemovedEvent
	if err := json.Unmarshal(env.Data, &removed); err != nil {
		t.Fatalf("decode user removed event: %v", err)
	}
	if removed.UserID != targetReady.User.ID {
		t.Fatalf("removed user id = %d, want %d", removed.UserID, targetReady.User.ID)
	}
	if removed.Reason != "Violation of rules" {
		t.Fatalf("removed reason = %q, want %q", removed.Reason, "Violation of rules")
	}

	// Target socket should be closed
	time.Sleep(50 * time.Millisecond)

	// Reconnecting with target's token should fail
	reconnect := h.dial()
	reconnect.fails(protocol.OpAuthToken, protocol.AuthTokenRequest{
		Token: targetReady.SessionToken,
	}, protocol.ErrInvalidCredentials)
}

func TestKickOfflineUserAndPurgeMessages(t *testing.T) {
	h := newHarness(t, nil)

	adminClient, adminReady := h.admin("Admin")
	tc := textChannel(t, adminReady)

	// Member registers an account, posts a message, then disconnects
	member := h.dial()
	memberReady := member.guest("Carlos")
	ok[protocol.UserEvent](member, protocol.OpAuthRegister, protocol.AuthRegisterRequest{
		Username: "carlos",
		Password: "password123",
	})

	// Post message
	msg := ok[protocol.MessageEvent](member, protocol.OpMessageSend, protocol.MessageSendRequest{
		ChannelID: tc.ID,
		Content:   "Spam message from Carlos",
	})

	// Member disconnects (goes offline)
	member.conn.Close(websocket.StatusNormalClosure, "")
	adminClient.waitEvent(protocol.EvUserDisconnected)

	// Admin kicks the offline user with 1d message purge
	ok[struct{}](adminClient, protocol.OpUserKick, protocol.UserKickRequest{
		UserID:         memberReady.User.ID,
		Reason:         "Spamming channels",
		DeleteMessages: "1d",
	})

	// Admin should receive message deleted event
	msgDeletedEnv := adminClient.waitEvent(protocol.EvMessageDeleted)
	var deletedEv protocol.MessageDeletedEvent
	if err := json.Unmarshal(msgDeletedEnv.Data, &deletedEv); err != nil {
		t.Fatalf("decode message deleted event: %v", err)
	}
	if deletedEv.MessageID != msg.Message.ID {
		t.Fatalf("deleted message id = %d, want %d", deletedEv.MessageID, msg.Message.ID)
	}

	// Admin should receive user removed event
	userRemovedEnv := adminClient.waitEvent(protocol.EvUserRemoved)
	var removedEv protocol.UserRemovedEvent
	if err := json.Unmarshal(userRemovedEnv.Data, &removedEv); err != nil {
		t.Fatalf("decode user removed event: %v", err)
	}
	if removedEv.UserID != memberReady.User.ID {
		t.Fatalf("removed user id = %d, want %d", removedEv.UserID, memberReady.User.ID)
	}

	// Verify kick record was saved in DB
	records, err := h.store.KicksByUserID(context.Background(), memberReady.User.ID)
	if err != nil {
		t.Fatalf("failed to query kicks table: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 kick row, got %d", len(records))
	}
	if records[0].Reason != "Spamming channels" {
		t.Fatalf("expected reason 'Spamming channels', got %q", records[0].Reason)
	}
	if records[0].DeletedMessages != "1d" {
		t.Fatalf("expected deleteMessages '1d', got %q", records[0].DeletedMessages)
	}
}
