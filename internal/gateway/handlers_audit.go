package gateway

import (
	"context"
	"encoding/json"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

const (
	defaultAuditLimit = 50
	maxAuditLimit     = 100
)

// handleAuditList reads one page of the moderation log, newest first.
//
// There is no op to write one. Entries are produced by the actions they
// describe, which is the only thing that makes the log worth reading: a record
// a client could append to would be a record of what somebody claimed to have
// done.
func handleAuditList(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.AuditListRequest](raw)
	if failure != nil {
		return nil, failure
	}

	base, _ := s.Permissions()
	if !base.Has(permissions.ViewAuditLog) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to read the audit log")
	}

	limit := req.Limit
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	if limit > maxAuditLimit {
		limit = maxAuditLimit
	}

	// One more than asked for, so "is there another page" is answered by the
	// query that produced this one rather than by a second count over a table
	// that only grows.
	entries, err := s.hub.st.ListAudit(ctx, store.AuditFilter{
		ActorID: req.ActorID,
		Action:  req.Action,
		Before:  req.Before,
		Limit:   limit + 1,
	})
	if err != nil {
		return nil, internalError(s, "read the audit log", err)
	}

	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}

	views := make([]protocol.AuditEntry, 0, len(entries))
	for _, e := range entries {
		views = append(views, auditView(e))
	}
	return protocol.AuditListResult{Entries: views, HasMore: hasMore}, nil
}
