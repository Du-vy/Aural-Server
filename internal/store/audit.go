package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// AuditEntry is one line of the record of what moderators did.
//
// Everything about the target is captured as it read at the time. A role that
// is later deleted, a member who is later purged, a channel that is later
// renamed: the log still says what was acted on, because a log that has to
// join against live tables tells a different story every time it is read.
type AuditEntry struct {
	ID         int64
	ActorID    *int64
	ActorName  string
	Action     string
	TargetType string
	TargetID   *int64
	TargetName string
	Reason     string
	Changes    []AuditChange
	CreatedAt  int64
}

// AuditChange is one field an action altered. Before and after are rendered
// strings rather than typed values: the log is read, not replayed.
type AuditChange struct {
	Key    string `json:"key"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// AuditFilter narrows a read of the log. Zero values mean "no narrowing".
type AuditFilter struct {
	ActorID int64
	Action  string
	// Before pages backwards: entries with a smaller id than this one.
	Before int64
	Limit  int
}

const auditColumns = `id, actor_id, actor_name, action, target_type, target_id,
	target_name, reason, changes, created_at`

// RecordAudit appends one entry.
//
// It never returns the row it wrote and its failure is not worth aborting a
// moderation action over: the action has already happened by the time this is
// called, and a log that could veto it would be a second, weaker permission
// check. Callers log the error and carry on.
func (s *Store) RecordAudit(ctx context.Context, e AuditEntry) (AuditEntry, error) {
	ts := now()
	e.CreatedAt = ts

	var changes *string
	if len(e.Changes) > 0 {
		raw, err := json.Marshal(e.Changes)
		if err != nil {
			return AuditEntry{}, fmt.Errorf("store: encode audit changes: %w", err)
		}
		text := string(raw)
		changes = &text
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_entries (actor_id, actor_name, action, target_type, target_id,
		                            target_name, reason, changes, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ActorID, e.ActorName, e.Action, e.TargetType, e.TargetID,
		e.TargetName, e.Reason, changes, ts)
	if err != nil {
		return AuditEntry{}, fmt.Errorf("store: record audit entry: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return AuditEntry{}, fmt.Errorf("store: record audit entry: %w", err)
	}
	e.ID = id
	return e, nil
}

// ListAudit reads one page of the log, newest first.
func (s *Store) ListAudit(ctx context.Context, filter AuditFilter) ([]AuditEntry, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	conditions := []string{"1 = 1"}
	var args []any
	if filter.ActorID > 0 {
		conditions = append(conditions, "actor_id = ?")
		args = append(args, filter.ActorID)
	}
	if filter.Action != "" {
		conditions = append(conditions, "action = ?")
		args = append(args, filter.Action)
	}
	if filter.Before > 0 {
		conditions = append(conditions, "id < ?")
		args = append(args, filter.Before)
	}
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+auditColumns+` FROM audit_entries WHERE `+
			strings.Join(conditions, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read audit log: %w", err)
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var (
			e       AuditEntry
			changes *string
		)
		if err := rows.Scan(&e.ID, &e.ActorID, &e.ActorName, &e.Action, &e.TargetType,
			&e.TargetID, &e.TargetName, &e.Reason, &changes, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: read audit log: %w", err)
		}
		if changes != nil && *changes != "" {
			// A column that will not decode reads as an entry with no field
			// list rather than as a failure: what was done and by whom is the
			// part worth showing, and it is in the other columns.
			_ = json.Unmarshal([]byte(*changes), &e.Changes)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read audit log: %w", err)
	}
	return out, nil
}

// PruneAudit drops entries older than cutoff and reports how many went. A
// server that runs for years should not be holding every role edit ever made.
func (s *Store) PruneAudit(ctx context.Context, cutoff int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM audit_entries WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: prune audit log: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
