package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// handleRoleCreate adds a role below the caller in the hierarchy.
func handleRoleCreate(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.RoleCreateRequest](raw)
	if failure != nil {
		return nil, failure
	}

	base, roleIDs := s.Permissions()
	if !base.Has(permissions.ManageRoles) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to manage roles")
	}

	name, failure := validateRoleName(req.Name)
	if failure != nil {
		return nil, failure
	}
	color, failure := validateColor(req.Color)
	if failure != nil {
		return nil, failure
	}
	perms, err := permissions.Parse(req.Permissions)
	if err != nil {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "permissions must be a decimal bitmask string")
	}
	if !base.Has(perms) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "a role cannot be given permissions you do not hold")
	}

	// A new role always lands strictly below its author, so creating one can
	// never be a route to more authority than the author already has.
	ceiling := s.hub.HighestRolePosition(roleIDs)
	position := s.hub.nextRolePosition()
	if position >= ceiling {
		position = ceiling - 1
	}
	if position < 1 {
		return nil, protocol.Errorf(protocol.ErrForbidden, "your own role is too low to create a role below it")
	}

	created, err := s.hub.st.CreateRole(ctx, store.Role{
		Name:        name,
		Color:       color,
		Permissions: perms,
		Position:    position,
		Hoist:       req.Hoist,
	})
	if err != nil {
		return nil, internalError(s, "create the role", err)
	}
	if err := s.hub.ReloadRoles(ctx); err != nil {
		return nil, internalError(s, "reload roles", err)
	}

	view := roleView(created)
	s.hub.Broadcast(protocol.Event(protocol.EvRoleCreated, protocol.RoleEvent{Role: view}))
	s.log.Info("role created", slog.Int64("role", created.ID), slog.String("name", created.Name))

	return protocol.RoleEvent{Role: view}, nil
}

// handleRoleUpdate edits a role the caller outranks.
func handleRoleUpdate(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.RoleUpdateRequest](raw)
	if failure != nil {
		return nil, failure
	}

	base, roleIDs := s.Permissions()
	if !base.Has(permissions.ManageRoles) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to manage roles")
	}

	role, ok := s.hub.Role(req.RoleID)
	if !ok {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such role")
	}
	ceiling := s.hub.HighestRolePosition(roleIDs)
	if role.Position >= ceiling {
		return nil, protocol.Errorf(protocol.ErrForbidden, "that role sits at or above your own")
	}

	permissionsChanged := false

	if req.Name != nil {
		if role.Managed == protocol.ManagedEveryone {
			return nil, protocol.Errorf(protocol.ErrForbidden, "the everyone role cannot be renamed")
		}
		name, failure := validateRoleName(*req.Name)
		if failure != nil {
			return nil, failure
		}
		role.Name = name
	}
	if req.Color != nil {
		color, failure := validateColor(*req.Color)
		if failure != nil {
			return nil, failure
		}
		role.Color = color
	}
	if req.Hoist != nil {
		role.Hoist = *req.Hoist
	}
	if req.Permissions != nil {
		next, err := permissions.Parse(*req.Permissions)
		if err != nil {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "permissions must be a decimal bitmask string")
		}
		// Only the bits actually being flipped need to be yours to flip.
		if changed := role.Permissions ^ next; !base.Has(changed) {
			return nil, protocol.Errorf(protocol.ErrForbidden, "that change touches a permission you do not hold")
		}
		permissionsChanged = role.Permissions != next
		role.Permissions = next
	}
	if req.Position != nil {
		if role.Managed == protocol.ManagedEveryone {
			return nil, protocol.Errorf(protocol.ErrForbidden, "the everyone role is always at the bottom")
		}
		if *req.Position < 1 {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "position must be at least 1")
		}
		if *req.Position >= ceiling {
			return nil, protocol.Errorf(protocol.ErrForbidden, "you cannot move a role to or above your own")
		}
		role.Position = *req.Position
		permissionsChanged = true
	}

	if err := s.hub.st.UpdateRole(ctx, role); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such role")
		}
		return nil, internalError(s, "update the role", err)
	}
	if err := s.hub.ReloadRoles(ctx); err != nil {
		return nil, internalError(s, "reload roles", err)
	}

	view := roleView(role)
	s.hub.Broadcast(protocol.Event(protocol.EvRoleUpdated, protocol.RoleEvent{Role: view}))
	if permissionsChanged {
		s.hub.resyncAll(ctx)
		s.hub.evictFromUnreachableChannels()
	}

	return protocol.RoleEvent{Role: view}, nil
}

// handleRoleDelete removes an unmanaged role the caller outranks.
func handleRoleDelete(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.RoleDeleteRequest](raw)
	if failure != nil {
		return nil, failure
	}

	base, roleIDs := s.Permissions()
	if !base.Has(permissions.ManageRoles) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to manage roles")
	}

	role, ok := s.hub.Role(req.RoleID)
	if !ok {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such role")
	}
	if role.Managed != protocol.ManagedNone {
		return nil, protocol.Errorf(protocol.ErrForbidden, "built-in roles cannot be deleted")
	}
	if role.Position >= s.hub.HighestRolePosition(roleIDs) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "that role sits at or above your own")
	}

	if err := s.hub.st.DeleteRole(ctx, req.RoleID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such role")
		}
		return nil, internalError(s, "delete the role", err)
	}
	if err := s.hub.ReloadRoles(ctx); err != nil {
		return nil, internalError(s, "reload roles", err)
	}

	s.hub.Broadcast(protocol.Event(protocol.EvRoleDeleted, protocol.RoleDeletedEvent{RoleID: req.RoleID}))
	s.hub.resyncAll(ctx)
	s.hub.evictFromUnreachableChannels()
	s.log.Info("role deleted", slog.Int64("role", req.RoleID))

	return protocol.RoleDeletedEvent{RoleID: req.RoleID}, nil
}

// handleRoleAssign grants a role to a user.
func handleRoleAssign(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	return s.changeRoleMembership(ctx, raw, true)
}

// handleRoleUnassign revokes a role from a user.
func handleRoleUnassign(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	return s.changeRoleMembership(ctx, raw, false)
}

// changeRoleMembership is the shared body of assign and unassign.
func (s *Session) changeRoleMembership(ctx context.Context, raw json.RawMessage, grant bool) (any, *protocol.Error) {
	req, failure := decode[protocol.RoleMembershipRequest](raw)
	if failure != nil {
		return nil, failure
	}

	base, roleIDs := s.Permissions()
	if !base.Has(permissions.ManageRoles) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to manage roles")
	}

	role, ok := s.hub.Role(req.RoleID)
	if !ok {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such role")
	}
	if role.AutoAssigned() {
		return nil, protocol.Errorf(protocol.ErrForbidden, "that role is granted automatically and cannot be assigned by hand")
	}
	if role.Position >= s.hub.HighestRolePosition(roleIDs) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "that role sits at or above your own")
	}
	if failure := s.hub.requireOutranksUser(ctx, s, req.UserID); failure != nil {
		return nil, failure
	}

	var err error
	if grant {
		err = s.hub.st.AssignRole(ctx, req.UserID, req.RoleID)
	} else {
		err = s.hub.st.UnassignRole(ctx, req.UserID, req.RoleID)
	}
	if err != nil {
		return nil, internalError(s, "change role membership", err)
	}

	target, online := s.hub.SessionForUser(req.UserID)
	if !online {
		// The grant is stored and takes effect on the next connection.
		return struct{}{}, nil
	}
	if err := target.refreshPermissions(ctx); err != nil {
		return nil, internalError(s, "refresh permissions", err)
	}

	view := target.view()
	s.hub.BroadcastUserUpdated(view)
	// The target may now see, or stop seeing, whole parts of the tree.
	target.Send(protocol.Event(protocol.EvReady, s.hub.buildReady(target, "")))
	s.hub.evictFromUnreachableChannels()

	s.log.Info("role membership changed",
		slog.Int64("role", req.RoleID),
		slog.Int64("user", req.UserID),
		slog.Bool("granted", grant))

	return protocol.UserEvent{User: view}, nil
}

// --- helpers ----------------------------------------------------------------

// nextRolePosition is one above the highest unmanaged role, leaving the admin
// role at the top of the stack where it belongs.
func (h *Hub) nextRolePosition() int {
	highest := 0
	for _, r := range h.SortedRoles() {
		if r.Managed == protocol.ManagedAdmin {
			continue
		}
		if r.Position > highest {
			highest = r.Position
		}
	}
	return highest + 1
}

// requireOutranksUser is the hierarchy check for a target that may be offline,
// which is what role grants need: the roles of an absent user still matter.
func (h *Hub) requireOutranksUser(ctx context.Context, actor *Session, targetID int64) *protocol.Error {
	if actor.UserID() == targetID {
		return nil
	}

	_, actorRoles := actor.Permissions()
	actorHighest := h.HighestRolePosition(actorRoles)

	var targetRoles []int64
	if target, online := h.SessionForUser(targetID); online {
		_, targetRoles = target.Permissions()
	} else {
		user, err := h.st.UserByID(ctx, targetID)
		if errors.Is(err, store.ErrNotFound) {
			return protocol.Errorf(protocol.ErrNotFound, "no such user")
		}
		if err != nil {
			return protocol.Errorf(protocol.ErrInternal, "the server failed to load that user")
		}
		explicit, err := h.st.RoleIDsForUser(ctx, targetID)
		if err != nil {
			return protocol.Errorf(protocol.ErrInternal, "the server failed to load that user")
		}
		targetRoles = h.EffectiveRoleIDs(user, explicit)
	}

	if actorHighest <= h.HighestRolePosition(targetRoles) {
		return protocol.Errorf(protocol.ErrForbidden, "that user holds a role at or above your own")
	}
	return nil
}
