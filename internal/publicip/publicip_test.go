package publicip

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

func TestResolveReturnsALiteralUnchanged(t *testing.T) {
	r := New("203.0.113.5", nil)
	if !r.Static() {
		t.Error("a literal address should be static; watching it would be pointless work")
	}

	addr, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if addr.String() != "203.0.113.5" {
		t.Errorf("resolved to %s, want 203.0.113.5", addr)
	}
}

// A hostname is the whole point of the change: it is what a dynamic DNS record
// is, and it must not be mistaken for something fixed, or it would never be
// looked up again after startup.
func TestAHostnameIsNotStatic(t *testing.T) {
	if New("aural.duckdns.org", nil).Static() {
		t.Error("a hostname was treated as static; a dynamic record would never be re-resolved")
	}
	if New("", []string{"stun:stun.example.org:3478"}).Static() {
		t.Error("a STUN-discovered address was treated as static")
	}
}

func TestResolveWithNothingConfiguredReportsNoSource(t *testing.T) {
	_, err := New("", nil).Resolve(context.Background())
	if !errors.Is(err, ErrNoSource) {
		t.Errorf("got %v, want ErrNoSource: an unconfigured server is not a broken one", err)
	}
}

func TestResolveLooksUpAHostname(t *testing.T) {
	// localhost is the one name every machine running these tests resolves
	// without a network.
	addr, err := New("localhost", nil).Resolve(context.Background())
	if err != nil {
		t.Skipf("localhost does not resolve here: %v", err)
	}
	if !addr.IsLoopback() {
		t.Errorf("localhost resolved to %s, which is not loopback", addr)
	}
}

func TestStunAddressReadsTheURLsAnICEServerActuallyCarries(t *testing.T) {
	cases := []struct {
		url  string
		want string
		ok   bool
	}{
		{"stun:stun.example.org:3478", "stun.example.org:3478", true},
		{"stun:stun.example.org", "stun.example.org:3478", true},
		{"STUN:stun.example.org:19302", "stun.example.org:19302", true},
		{"stun:stun.example.org:3478?transport=udp", "stun.example.org:3478", true},
		{"  stun:stun.example.org:3478  ", "stun.example.org:3478", true},
		// A TURN server has credentials and a different job; asking it this
		// question is not what its entry is for.
		{"turn:turn.example.org:3478", "", false},
		{"turns:turn.example.org:5349", "", false},
		// stuns: is TLS over TCP, which the binding request here does not
		// speak.
		{"stuns:stun.example.org:5349", "", false},
		{"stun:", "", false},
		{"nonsense", "", false},
	}

	for _, tc := range cases {
		got, ok := stunAddress(tc.url)
		if ok != tc.ok {
			t.Errorf("stunAddress(%q) ok = %v, want %v", tc.url, ok, tc.ok)
			continue
		}
		if got != tc.want {
			t.Errorf("stunAddress(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// An address that reaches nobody is worse than advertising none at all: it
// becomes a candidate every client wastes time trying.
func TestUsableRejectsAddressesThatReachNobody(t *testing.T) {
	unusable := []string{"127.0.0.1", "::1", "0.0.0.0", "::", "169.254.1.1", "fe80::1", "224.0.0.1"}
	for _, raw := range unusable {
		addr := netip.MustParseAddr(raw)
		if usable(addr) {
			t.Errorf("%s was accepted as something to advertise", raw)
		}
	}

	// Private ranges stay allowed: a server on a LAN whose clients are on it
	// too has exactly the right address, and it is not this code's business to
	// disagree.
	for _, raw := range []string{"192.168.1.10", "10.0.0.5", "203.0.113.5", "2001:db8::1"} {
		addr := netip.MustParseAddr(raw)
		if !usable(addr) {
			t.Errorf("%s was refused, but it is a perfectly good address to advertise", raw)
		}
	}
}

func TestDescribeNamesTheSource(t *testing.T) {
	cases := []struct {
		resolver *Resolver
		want     string
	}{
		{New("203.0.113.5", nil), "configured address"},
		{New("aural.duckdns.org", nil), "hostname aural.duckdns.org"},
		{New("", []string{"stun:stun.example.org"}), "STUN"},
		{New("", nil), "none"},
	}
	for _, tc := range cases {
		if got := tc.resolver.Describe(); got != tc.want {
			t.Errorf("Describe() = %q, want %q", got, tc.want)
		}
	}
}
