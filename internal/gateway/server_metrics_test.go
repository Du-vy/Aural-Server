package gateway_test

import (
	"context"
	"testing"
	"time"

	"github.com/aural-chat/aural-server/internal/gateway"
	"github.com/aural-chat/aural-server/internal/protocol"
)

func TestServerMetricsRequiresManageServer(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	c.guest("GuestUser")

	// Regular guest without ManageServer cannot access metrics
	c.fails(protocol.OpServerMetrics, protocol.ServerMetricsRequest{}, protocol.ErrForbidden)

	// Claim admin token to become owner/admin
	token, err := gateway.EnsureOwnerToken(context.Background(), h.store, h.server.Hub())
	if err != nil {
		t.Fatalf("ensure owner token: %v", err)
	}
	ok[protocol.UserEvent](c, protocol.OpServerClaimAdmin, protocol.ClaimAdminRequest{Token: token})

	// Now admin can fetch server metrics successfully
	res := ok[protocol.ServerMetrics](c, protocol.OpServerMetrics, protocol.ServerMetricsRequest{Force: true})

	if res.CPU.Cores <= 0 {
		t.Errorf("expected CPU cores > 0, got %d", res.CPU.Cores)
	}
	if res.Memory.ProcessHeapSys <= 0 {
		t.Errorf("expected Memory.ProcessHeapSys > 0, got %d", res.Memory.ProcessHeapSys)
	}
	if res.Activity.ActiveConnections <= 0 {
		t.Errorf("expected ActiveConnections > 0, got %d", res.Activity.ActiveConnections)
	}
	if res.System.ServerVersion == "" {
		t.Errorf("expected ServerVersion, got empty")
	}
}

// The whole contract of ping is that it answers, and answers to anybody who is
// signed in: a client cannot time its round trip to a server that will only
// reply to an administrator.
func TestPingAnswersAnyAuthenticatedSession(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	c.guest("GuestUser")

	before := time.Now().Add(-time.Minute).UnixMilli()
	res := ok[protocol.PongResponse](c, protocol.OpPing, nil)
	after := time.Now().Add(time.Minute).UnixMilli()

	if res.ServerTime < before || res.ServerTime > after {
		t.Fatalf("pong carried %d, which is not a plausible clock right now", res.ServerTime)
	}
}

func TestPingNeedsAuthentication(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()

	c.fails(protocol.OpPing, nil, protocol.ErrUnauthorized)
}
