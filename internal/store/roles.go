package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
)

// Role is a named bundle of permissions.
//
// Managed roles are maintained by the server. The everyone and registered roles
// are held implicitly by whoever qualifies and are never rows in user_roles;
// the admin role is granted explicitly, by redeeming the owner token or by
// another administrator.
type Role struct {
	ID          int64
	Name        string
	Color       string
	Permissions permissions.Permission
	Position    int
	Hoist       bool
	Managed     string
	CreatedAt   int64
}

// AutoAssigned reports whether membership of the role follows from what a user
// is rather than from an explicit grant.
func (r Role) AutoAssigned() bool {
	return r.Managed == protocol.ManagedEveryone || r.Managed == protocol.ManagedRegistered
}

const roleColumns = `id, name, color, permissions, position, hoist, managed, created_at`

func scanRole(row interface{ Scan(...any) error }) (Role, error) {
	var (
		r     Role
		perms int64
		hoist int
	)
	err := row.Scan(&r.ID, &r.Name, &r.Color, &perms, &r.Position, &hoist, &r.Managed, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Role{}, ErrNotFound
	}
	if err != nil {
		return Role{}, fmt.Errorf("store: scan role: %w", err)
	}
	r.Permissions = permissions.Permission(perms) & permissions.All
	r.Hoist = hoist != 0
	return r, nil
}

// AllRoles lists every role, lowest position first.
func (s *Store) AllRoles(ctx context.Context) ([]Role, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+roleColumns+` FROM roles ORDER BY position ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list roles: %w", err)
	}
	defer rows.Close()

	var out []Role
	for rows.Next() {
		r, err := scanRole(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list roles: %w", err)
	}
	return out, nil
}

// RoleByID looks up one role.
func (s *Store) RoleByID(ctx context.Context, id int64) (Role, error) {
	return scanRole(s.db.QueryRowContext(ctx, `SELECT `+roleColumns+` FROM roles WHERE id = ?`, id))
}

// RoleByManaged looks up one of the managed roles by its kind.
func (s *Store) RoleByManaged(ctx context.Context, managed string) (Role, error) {
	return scanRole(s.db.QueryRowContext(ctx, `SELECT `+roleColumns+` FROM roles WHERE managed = ?`, managed))
}

// CreateRole inserts a role. Position, when left at zero, is placed just above
// the highest unmanaged role so a new role starts out without authority over
// the existing hierarchy.
func (s *Store) CreateRole(ctx context.Context, r Role) (Role, error) {
	ts := now()
	if r.Position == 0 {
		var highest sql.NullInt64
		if err := s.db.QueryRowContext(ctx,
			`SELECT MAX(position) FROM roles WHERE managed <> ?`, protocol.ManagedAdmin).Scan(&highest); err != nil {
			return Role{}, fmt.Errorf("store: pick role position: %w", err)
		}
		r.Position = int(highest.Int64) + 1
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO roles (name, color, permissions, position, hoist, managed, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.Name, r.Color, int64(r.Permissions), r.Position, boolToInt(r.Hoist), r.Managed, ts)
	if err != nil {
		if isUniqueViolation(err) {
			return Role{}, ErrConflict
		}
		return Role{}, fmt.Errorf("store: create role: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Role{}, fmt.Errorf("store: create role: %w", err)
	}
	r.ID = id
	r.CreatedAt = ts
	return r, nil
}

// UpdateRole writes back a role the caller has already patched. Managed is not
// writable: a role never changes what kind of managed role it is.
func (s *Store) UpdateRole(ctx context.Context, r Role) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE roles SET name = ?, color = ?, permissions = ?, position = ?, hoist = ? WHERE id = ?`,
		r.Name, r.Color, int64(r.Permissions), r.Position, boolToInt(r.Hoist), r.ID)
	if err != nil {
		return fmt.Errorf("store: update role: %w", err)
	}
	return requireOneRow(res, "role")
}

// DeleteRole removes a role and, by cascade, every grant of it.
func (s *Store) DeleteRole(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM roles WHERE id = ? AND managed = ''`, id)
	if err != nil {
		return fmt.Errorf("store: delete role: %w", err)
	}
	return requireOneRow(res, "role")
}

// RoleIDsForUser lists the roles explicitly granted to a user. The implicit
// everyone and registered roles are not included; resolve them from the user
// record instead.
func (s *Store) RoleIDsForUser(ctx context.Context, userID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.id FROM user_roles ur JOIN roles r ON r.id = ur.role_id
		 WHERE ur.user_id = ? ORDER BY r.position ASC, r.id ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list user roles: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: list user roles: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list user roles: %w", err)
	}
	return out, nil
}

// AssignRole grants a role. Granting a role the user already holds is not an
// error, which keeps the operation idempotent.
func (s *Store) AssignRole(ctx context.Context, userID, roleID int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO user_roles (user_id, role_id) VALUES (?, ?)`, userID, roleID)
	if err != nil {
		return fmt.Errorf("store: assign role: %w", err)
	}
	return nil
}

// UnassignRole revokes a role, idempotently.
func (s *Store) UnassignRole(ctx context.Context, userID, roleID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM user_roles WHERE user_id = ? AND role_id = ?`, userID, roleID)
	if err != nil {
		return fmt.Errorf("store: unassign role: %w", err)
	}
	return nil
}
