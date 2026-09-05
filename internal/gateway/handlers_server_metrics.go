package gateway

import (
	"context"
	"encoding/json"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
)

// handleServerMetrics returns CPU, RAM, disk storage breakdown, and system activity metrics.
// It requires the ManageServer or Administrator permission.
func handleServerMetrics(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.ServerMetricsRequest](raw)
	if failure != nil {
		return nil, failure
	}

	base, _ := s.Permissions()
	if !base.Has(permissions.ManageServer) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to view server metrics")
	}

	metrics, err := s.hub.CollectMetrics(ctx, req.Force)
	if err != nil {
		return nil, internalError(s, "collect server metrics", err)
	}

	return metrics, nil
}
