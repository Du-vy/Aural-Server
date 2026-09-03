package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// TestOwnerSurvivesTheAccountItNames pins the one thing a stored id cannot
// promise on its own: the account is still there. An owner whose identity was
// deleted reads as no owner at all rather than as a user id nobody holds.
func TestOwnerSurvivesTheAccountItNames(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if owner, err := s.OwnerUserID(ctx); err != nil || owner != 0 {
		t.Fatalf("fresh database: got owner %d, %v", owner, err)
	}

	user, err := s.CreateGuest(ctx, "Pablo")
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}
	if err := s.SetOwnerUserID(ctx, user.ID); err != nil {
		t.Fatalf("SetOwnerUserID: %v", err)
	}
	if owner, err := s.OwnerUserID(ctx); err != nil || owner != user.ID {
		t.Fatalf("owner: got %d, %v, want %d", owner, err, user.ID)
	}

	if err := s.DeleteUser(ctx, user.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if owner, err := s.OwnerUserID(ctx); err != nil || owner != 0 {
		t.Fatalf("owner after the account went: got %d, %v, want none", owner, err)
	}
}

// TestOwnershipIsBackfilledFromTheAdminRole covers the upgrade path. Ownership
// used to be nothing but a grant of the managed admin role, so a server that
// already had an administrator has to come out of the migration with an owner
// rather than with nobody in charge.
func TestOwnershipIsBackfilledFromTheAdminRole(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	admin, err := s.RoleByManaged(ctx, protocol.ManagedAdmin)
	if err != nil {
		t.Fatalf("RoleByManaged: %v", err)
	}
	first, err := s.CreateGuest(ctx, "First")
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}
	second, err := s.CreateGuest(ctx, "Second")
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}
	for _, id := range []int64{second.ID, first.ID} {
		if err := s.AssignRole(ctx, id, admin.ID); err != nil {
			t.Fatalf("AssignRole: %v", err)
		}
	}

	// Wind the database back to what it looked like before ownership existed:
	// administrators, and no owner recorded anywhere.
	if err := s.DeleteMeta(ctx, MetaOwnerUserID); err != nil {
		t.Fatalf("DeleteMeta: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA user_version = 9`); err != nil {
		t.Fatalf("rewind schema version: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	// The earliest identity holding the role, which on a server whose token was
	// redeemed once is the identity that redeemed it.
	owner, err := reopened.OwnerUserID(ctx)
	if err != nil {
		t.Fatalf("OwnerUserID: %v", err)
	}
	if owner != first.ID {
		t.Fatalf("owner after the migration: got %d, want %d", owner, first.ID)
	}
}
