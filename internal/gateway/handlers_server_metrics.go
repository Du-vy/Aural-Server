package gateway

import (
	"context"
	"encoding/json"
	"time"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
)

// handlePing answers immediately, which is the entire point of it: the reply
// carries no work, so what the caller times is the connection and the read
// loop and nothing else.
//
// It reads nothing and takes no payload, so it is not decoded. An op that
// refused a body would only give a future client a reason to be careful about
// sending one.
func handlePing(_ context.Context, _ *Session, _ json.RawMessage) (any, *protocol.Error) {
	return protocol.PongResponse{ServerTime: time.Now().UnixMilli()}, nil
}

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
