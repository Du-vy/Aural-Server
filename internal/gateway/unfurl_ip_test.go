package gateway

import (
	"net"
	"testing"
)

// isPrivateIP is the whole of the SSRF defence: the dialler resolves a target
// and refuses it before connecting. It had no tests, so a range quietly
// dropping out of it would have gone unnoticed until somebody used it.
func TestIsPrivateIPRefusesWhatIsNotThePublicInternet(t *testing.T) {
	blocked := []string{
		// Loopback and unspecified.
		"127.0.0.1", "127.1.2.3", "0.0.0.0", "::1", "::",
		// RFC 1918.
		"10.0.0.1", "10.255.255.255",
		"172.16.0.1", "172.20.10.5", "172.31.255.255",
		"192.168.0.1", "192.168.255.254",
		// Link-local, which is where a cloud instance keeps its credentials.
		"169.254.169.254", "169.254.0.1", "fe80::1",
		// Carrier-grade NAT: not private, not the public internet, and
		// reachable from a great many hosted servers.
		"100.64.0.1", "100.100.50.1", "100.127.255.255",
		// IPv6 unique local.
		"fc00::1", "fd12:3456::1",
		// An IPv4 address wearing an IPv6 spelling, by either route.
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254",
		"64:ff9b::127.0.0.1", "64:ff9b::10.0.0.1", "64:ff9b::a9fe:a9fe",
	}
	for _, raw := range blocked {
		ip := net.ParseIP(raw)
		if ip == nil {
			t.Fatalf("test case %q is not an IP", raw)
		}
		if !isPrivateIP(ip) {
			t.Errorf("%s was allowed; the server must not be made to reach it", raw)
		}
	}

	allowed := []string{
		"1.1.1.1", "8.8.8.8", "93.184.216.34",
		// The addresses either side of the blocked ranges, which is where an
		// off-by-one in a bound would show up.
		"9.255.255.255", "11.0.0.0",
		"172.15.255.255", "172.32.0.0",
		"192.167.255.255", "192.169.0.0",
		"100.63.255.255", "100.128.0.0",
		"169.253.255.255", "169.255.0.0",
		"2606:4700:4700::1111", "2001:4860:4860::8888",
		// fc00::/7 ends at fdff::; fe00:: is outside it and not link-local.
		"fbff::1",
	}
	for _, raw := range allowed {
		ip := net.ParseIP(raw)
		if ip == nil {
			t.Fatalf("test case %q is not an IP", raw)
		}
		if isPrivateIP(ip) {
			t.Errorf("%s was refused; it is an ordinary public address", raw)
		}
	}

	// A nil address is refused rather than dialled, which is what a failed
	// parse upstream would hand in.
	if !isPrivateIP(nil) {
		t.Error("a nil address must be refused")
	}
}
