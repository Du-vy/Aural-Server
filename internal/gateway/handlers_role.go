package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"

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
	ceiling := s.hub.RankOf(s.UserID(), roleIDs)
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

	entry := auditTarget(protocol.AuditTargetRole, created.ID, created.Name)
	entry.Action = protocol.AuditRoleCreate
	s.hub.audit(ctx, s, entry)

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
	ceiling := s.hub.RankOf(s.UserID(), roleIDs)
	if role.Position >= ceiling {
		return nil, protocol.Errorf(protocol.ErrForbidden, "that role sits at or above your own")
	}

	// Captured before anything is patched, so the log can say what actually
	// changed rather than only what it was set to.
	before := role
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
		// The owner has no role above them for the ceiling to bound, so the
		// stack itself bounds them: a position is a place in the hierarchy
		// rather than an arbitrary number, and the top of it is as far as a
		// role goes.
		if *req.Position > s.hub.topRolePosition() {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "position must not be above the top of the role stack")
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

	entry := auditTarget(protocol.AuditTargetRole, role.ID, role.Name)
	entry.Action = protocol.AuditRoleUpdate
	entry.Changes = changed(nil, "name", before.Name, role.Name)
	entry.Changes = changed(entry.Changes, "color", before.Color, role.Color)
	entry.Changes = changed(entry.Changes, "permissions",
		before.Permissions.String(), role.Permissions.String())
	entry.Changes = changed(entry.Changes, "position",
		strconv.Itoa(before.Position), strconv.Itoa(role.Position))
	s.hub.audit(ctx, s, entry)

	if permissionsChanged {
		s.hub.resyncAll(ctx)
		s.hub.evictFromUnreachableChannels()
	}

	return protocol.RoleEvent{Role: view}, nil
}

// handleRoleReorder restacks the hierarchy in one move.
//
// A reorder is one decision about the whole stack, so it is validated and
// written as one: positions mean nothing on their own, and applying half of a
// requested order would leave a hierarchy nobody asked for.
//
// The check is made on where each role sits in the order rather than on the
// number recorded against it, because the write renumbers everything to sit
// contiguously — a role that has not moved relative to its neighbours has not
// moved, whatever integer it ends up with.
func handleRoleReorder(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.RoleReorderRequest](raw)
	if failure != nil {
		return nil, failure
	}

	base, roleIDs := s.Permissions()
	if !base.Has(permissions.ManageRoles) {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you are not allowed to manage roles")
	}

	// The everyone role is beneath everything by definition and is not part of
	// the stack being ordered.
	current := make([]store.Role, 0)
	for _, r := range s.hub.SortedRoles() {
		if r.Managed != protocol.ManagedEveryone {
			current = append(current, r)
		}
	}
	if len(req.RoleIDs) != len(current) {
		return nil, protocol.Errorf(protocol.ErrBadRequest,
			"a reorder must name every role exactly once")
	}

	wasAt := make(map[int64]int, len(current))
	for i, r := range current {
		wasAt[r.ID] = i
	}
	nowAt := make(map[int64]int, len(req.RoleIDs))
	for i, id := range req.RoleIDs {
		if _, ok := wasAt[id]; !ok {
			return nil, protocol.Errorf(protocol.ErrBadRequest,
				"a reorder must name every role exactly once")
		}
		if _, seen := nowAt[id]; seen {
			return nil, protocol.Errorf(protocol.ErrBadRequest,
				"a reorder must name every role exactly once")
		}
		nowAt[id] = i
	}

	// Where the caller's own authority stops, as an index into the same order.
	// Everything from there up is at or above them and is not theirs to move,
	// nor a gap to move anything else into.
	ceiling := len(current)
	if rank := s.hub.RankOf(s.UserID(), roleIDs); !s.hub.IsOwner(s.UserID()) {
		ceiling = 0
		for i, r := range current {
			if r.Position < rank {
				ceiling = i + 1
			}
		}
	}
	for id, to := range nowAt {
		from := wasAt[id]
		if from == to {
			continue
		}
		if from >= ceiling || to >= ceiling {
			return nil, protocol.Errorf(protocol.ErrForbidden,
				"you cannot move a role to or above your own")
		}
	}

	if err := s.hub.st.ReorderRoles(ctx, req.RoleIDs); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such role")
		}
		return nil, internalError(s, "reorder the roles", err)
	}
	if err := s.hub.ReloadRoles(ctx); err != nil {
		return nil, internalError(s, "reload roles", err)
	}

	// Every position has been rewritten, so every role is news to a client.
	views := make([]protocol.Role, 0, len(req.RoleIDs))
	for _, id := range req.RoleIDs {
		role, ok := s.hub.Role(id)
		if !ok {
			continue
		}
		view := roleView(role)
		views = append(views, view)
		s.hub.Broadcast(protocol.Event(protocol.EvRoleUpdated, protocol.RoleEvent{Role: view}))
	}

	entry := auditTarget(protocol.AuditTargetRole, 0, "")
	entry.Action = protocol.AuditRoleUpdate
	entry.Changes = changed(nil, "order", orderOf(current), orderOf(rolesInOrder(s.hub, req.RoleIDs)))
	s.hub.audit(ctx, s, entry)

	// The hierarchy decides what everybody may do, so everybody's view of it
	// has to be rebuilt — the same resync a permission edit causes.
	s.hub.resyncAll(ctx)
	s.hub.evictFromUnreachableChannels()

	s.log.Info("roles reordered", slog.Int("roles", len(req.RoleIDs)))

	return protocol.RoleReorderResult{Roles: views}, nil
}

// rolesInOrder resolves ids to the roles they name, skipping any that have
// gone. Only the audit log reads it.
func rolesInOrder(h *Hub, ids []int64) []store.Role {
	out := make([]store.Role, 0, len(ids))
	for _, id := range ids {
		if role, ok := h.Role(id); ok {
			out = append(out, role)
		}
	}
	return out
}

// orderOf names a stack bottom-up, which is how a reorder reads in the log.
func orderOf(roles []store.Role) string {
	names := make([]string, 0, len(roles))
	for _, r := range roles {
		names = append(names, r.Name)
	}
	return strings.Join(names, " < ")
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
	if role.Position >= s.hub.RankOf(s.UserID(), roleIDs) {
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

	entry := auditTarget(protocol.AuditTargetRole, role.ID, role.Name)
	entry.Action = protocol.AuditRoleDelete
	s.hub.audit(ctx, s, entry)

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
	if role.Position >= s.hub.RankOf(s.UserID(), roleIDs) {
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

	// Logged against the member rather than the role: reading the log to find
	// out what happened to somebody is what it is for, and a grant is a thing
	// done to a person.
	entry := auditTarget(protocol.AuditTargetUser, req.UserID, s.hub.nicknameOf(ctx, req.UserID))
	entry.Action = protocol.AuditRoleUnassign
	if grant {
		entry.Action = protocol.AuditRoleAssign
	}
	entry.Changes = []store.AuditChange{{Key: "role", After: role.Name}}
	s.hub.audit(ctx, s, entry)

	target, online := s.hub.SessionForUser(req.UserID)
	if !online {
		// The grant takes effect on the next connection, but the roles it
		// paints the member with are on show in everybody's list right now, so
		// the change still has to be announced.
		user, err := s.hub.st.UserByID(ctx, req.UserID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such user")
		}
		if err != nil {
			return nil, internalError(s, "load that user", err)
		}
		view, err := s.hub.offlineMemberView(ctx, user)
		if err != nil {
			return nil, internalError(s, "load that user", err)
		}
		s.hub.BroadcastMemberUpdated(view)

		s.log.Info("role membership changed",
			slog.Int64("role", req.RoleID),
			slog.Int64("user", req.UserID),
			slog.Bool("granted", grant))

		return protocol.UserEvent{User: view}, nil
	}
	if err := target.refreshPermissions(ctx); err != nil {
		return nil, internalError(s, "refresh permissions", err)
	}

	view := target.view()
	// A role change is somebody else's doing, so it goes out even when the
	// target is hiding: masking turns it into an update to the offline entry
	// they already sit in, which is the frame an absent member's grant makes.
	s.hub.BroadcastMemberUpdated(view)
	// The target may now see, or stop seeing, whole parts of the tree.
	ready, err := s.hub.buildReady(ctx, target, "")
	if err != nil {
		return nil, internalError(s, "build the state snapshot", err)
	}
	target.Send(protocol.Event(protocol.EvReady, ready))
	s.hub.evictFromUnreachableChannels()

	s.log.Info("role membership changed",
		slog.Int64("role", req.RoleID),
		slog.Int64("user", req.UserID),
		slog.Bool("granted", grant))

	// The caller is not the subject, so their copy is masked like anybody
	// else's: granting a role must not report back where a hidden member is
	// sitting.
	return protocol.UserEvent{User: s.hub.MaskUser(s, view)}, nil
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

// topRolePosition is the highest position any role sits at, which is the
// ceiling for whoever has no role above them.
func (h *Hub) topRolePosition() int {
	highest := 0
	for _, r := range h.SortedRoles() {
		if r.Position > highest {
			highest = r.Position
		}
	}
	return highest
}

// requireOutranksUser is the hierarchy check for a target that may be offline,
// which is what role grants need: the roles of an absent user still matter.
func (h *Hub) requireOutranksUser(ctx context.Context, actor *Session, targetID int64) *protocol.Error {
	if actor.UserID() == targetID {
		return nil
	}

	_, actorRoles := actor.Permissions()
	actorHighest := h.RankOf(actor.UserID(), actorRoles)

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

	if actorHighest <= h.RankOf(targetID, targetRoles) {
		return protocol.Errorf(protocol.ErrForbidden, "that user stands at or above you")
	}
	return nil
}

// nicknameOf is the name to record for somebody an action was taken against.
// It reads the live session first and falls back to the row, because the
// nickname is what makes a log entry readable and the id is what makes it
// precise: an entry needs both, and a member who has just been removed still
// has to appear under the name everybody knew them by.
func (h *Hub) nicknameOf(ctx context.Context, userID int64) string {
	if session, online := h.SessionForUser(userID); online {
		return session.User().Nickname
	}
	if user, err := h.st.UserByID(ctx, userID); err == nil {
		return user.Nickname
	}
	return ""
}
