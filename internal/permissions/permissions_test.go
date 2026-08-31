package permissions_test

import (
	"testing"

	"github.com/aural-chat/aural-server/internal/permissions"
)

func TestAdministratorSatisfiesEverything(t *testing.T) {
	admin := permissions.Administrator
	if !admin.Has(permissions.ManageChannels | permissions.KickUsers) {
		t.Fatal("Administrator should satisfy any request")
	}

	resolved := permissions.Resolve([]permissions.Permission{permissions.Administrator})
	if resolved != permissions.All {
		t.Fatalf("Administrator should resolve to every bit, got %v", resolved.Names())
	}
}

func TestResolveUnionsRoles(t *testing.T) {
	got := permissions.Resolve([]permissions.Permission{
		permissions.ViewChannel | permissions.Connect,
		permissions.Speak,
	})
	want := permissions.ViewChannel | permissions.Connect | permissions.Speak
	if got != want {
		t.Fatalf("union: got %v, want %v", got.Names(), want.Names())
	}
}

func TestParseRoundTrip(t *testing.T) {
	original := permissions.ViewChannel | permissions.ManageRoles
	parsed, err := permissions.Parse(original.String())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed != original {
		t.Fatalf("round trip: got %v, want %v", parsed.Names(), original.Names())
	}

	empty, err := permissions.Parse("")
	if err != nil || empty != permissions.None {
		t.Fatalf("an empty mask should parse as None, got %v (%v)", empty, err)
	}
	if _, err := permissions.Parse("not-a-number"); err == nil {
		t.Fatal("a non-numeric mask should be rejected")
	}
}

func TestParseDropsUndefinedBits(t *testing.T) {
	// A client that invents a bit must not be able to store it.
	parsed, err := permissions.Parse("18446744073709551615")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed != permissions.All {
		t.Fatalf("undefined bits should be dropped, got %v", uint64(parsed))
	}
}

func TestChannelOverwritesApplyDenyThenAllow(t *testing.T) {
	const everyoneID, modID int64 = 1, 2

	base := permissions.ViewChannel | permissions.Connect | permissions.Speak
	overwrites := []permissions.Overwrite{
		{RoleID: everyoneID, Deny: permissions.Speak},
		{RoleID: modID, Allow: permissions.Speak},
	}

	// A plain member loses Speak to the everyone overwrite.
	member := permissions.ResolveInChannel(base, everyoneID, []int64{everyoneID}, overwrites)
	if member.Has(permissions.Speak) {
		t.Fatal("the everyone deny should have taken Speak away")
	}
	if !member.Has(permissions.Connect) {
		t.Fatal("Connect was not touched and should survive")
	}

	// A moderator gets it back from the role overwrite.
	mod := permissions.ResolveInChannel(base, everyoneID, []int64{everyoneID, modID}, overwrites)
	if !mod.Has(permissions.Speak) {
		t.Fatal("the role allow should have restored Speak")
	}
}

func TestLosingViewChannelLosesEverything(t *testing.T) {
	const everyoneID int64 = 1

	base := permissions.ViewChannel | permissions.Connect | permissions.Speak
	got := permissions.ResolveInChannel(base, everyoneID, []int64{everyoneID},
		[]permissions.Overwrite{{RoleID: everyoneID, Deny: permissions.ViewChannel}})

	if got != permissions.None {
		t.Fatalf("a channel you cannot see grants nothing, got %v", got.Names())
	}
}

func TestOverwritesCannotDemoteAnAdministrator(t *testing.T) {
	const everyoneID int64 = 1

	got := permissions.ResolveInChannel(permissions.Administrator, everyoneID, []int64{everyoneID},
		[]permissions.Overwrite{{RoleID: everyoneID, Deny: permissions.ViewChannel | permissions.Connect}})

	if got != permissions.All {
		t.Fatalf("an administrator keeps everything, got %v", got.Names())
	}
}

func TestDefaultEveryoneIsUsableButNotPowerful(t *testing.T) {
	d := permissions.DefaultEveryone

	for _, want := range []permissions.Permission{
		permissions.ViewChannel, permissions.Connect, permissions.Speak, permissions.Register,
	} {
		if !d.Has(want) {
			t.Fatalf("a default guest should hold %v", want.Names())
		}
	}
	for _, deny := range []permissions.Permission{
		permissions.ManageChannels, permissions.ManageRoles, permissions.ManageServer,
		permissions.KickUsers, permissions.Administrator,
	} {
		if d.Has(deny) {
			t.Fatalf("a default guest must not hold %v", deny.Names())
		}
	}
}
