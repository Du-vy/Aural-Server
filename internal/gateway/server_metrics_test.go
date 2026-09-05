package gateway_test

import (
	"context"
	"testing"

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
