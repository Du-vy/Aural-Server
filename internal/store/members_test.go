package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestListMembersHoldsAccountsWithTheirRoles pins the two things the member
// list is built on: who counts as a member, and the roles they are painted
// with. Both have to be right for somebody who is not connected, because that
// is the only source there is for them.
func TestListMembersHoldsAccountsWithTheirRoles(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if members, err := s.ListMembers(ctx); err != nil || len(members) != 0 {
		t.Fatalf("fresh database: got %d members, %v", len(members), err)
	}

	guest, err := s.CreateGuest(ctx, "Guest")
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}
	claimed, err := s.CreateGuest(ctx, "Bruno")
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}
	if _, err := s.ClaimIdentity(ctx, claimed.ID, "bruno", "hash"); err != nil {
		t.Fatalf("ClaimIdentity: %v", err)
	}

	role, err := s.CreateRole(ctx, Role{Name: "Moderator"})
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	granted := role.ID
	if err := s.AssignRole(ctx, claimed.ID, granted); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	members, err := s.ListMembers(ctx)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	// The guest is left out: an unclaimed identity exists only while its
	// connection does, so it has no place in a list that outlives one.
	if len(members) != 1 {
		t.Fatalf("members: got %d, want only the claimed identity", len(members))
	}
	if members[0].ID == guest.ID {
		t.Fatal("a guest was listed as a member")
	}
	if members[0].ID != claimed.ID || members[0].Nickname != "Bruno" {
		t.Fatalf("member: got %+v", members[0].User)
	}
	if !members[0].Registered() {
		t.Fatal("a claimed identity was not reported as registered")
	}
	if len(members[0].RoleIDs) != 1 || members[0].RoleIDs[0] != granted {
		t.Fatalf("member roles: got %v, want [%d]", members[0].RoleIDs, granted)
	}
}
