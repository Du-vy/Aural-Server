package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The forwarding header is written by whoever spoke to the proxy, so the whole
// value of clientIP is in what it refuses to believe. A bug here does not
// break anything visibly; it quietly lets anybody put whatever they like in
// the log, and in whatever is later keyed off it.
func TestClientIPIgnoresForwardingHeadersFromUntrustedPeers(t *testing.T) {
	trusted, err := parseTrustedProxies([]string{"10.0.0.0/8", "127.0.0.1"})
	if err != nil {
		t.Fatalf("parse trusted proxies: %v", err)
	}

	cases := []struct {
		name    string
		peer    string
		headers map[string][]string
		trusted []string
		want    string
	}{{
		name:    "a header from a stranger is not believed",
		peer:    "203.0.113.9:41000",
		headers: map[string][]string{"X-Forwarded-For": {"1.2.3.4"}},
		want:    "203.0.113.9",
	}, {
		name:    "a header from a trusted proxy is believed",
		peer:    "127.0.0.1:41000",
		headers: map[string][]string{"X-Forwarded-For": {"198.51.100.7"}},
		want:    "198.51.100.7",
	}, {
		name:    "with no proxies configured, nothing is believed",
		peer:    "127.0.0.1:41000",
		headers: map[string][]string{"X-Forwarded-For": {"198.51.100.7"}},
		trusted: []string{},
		want:    "127.0.0.1",
	}, {
		// The client half of the chain is written by the client. A proxy that
		// appends rather than replaces hands us both, and only the part our
		// own proxies vouched for may be trusted.
		name:    "a chain is walked back only as far as the trusted hops",
		peer:    "10.1.2.3:41000",
		headers: map[string][]string{"X-Forwarded-For": {"9.9.9.9, 198.51.100.7, 10.4.5.6"}},
		want:    "198.51.100.7",
	}, {
		name:    "a spoofed chain of trusted-looking hops cannot reach past the client",
		peer:    "10.1.2.3:41000",
		headers: map[string][]string{"X-Forwarded-For": {"203.0.113.1, 10.9.9.9, 10.8.8.8"}},
		want:    "203.0.113.1",
	}, {
		name:    "a header split across several lines still reads in order",
		peer:    "10.1.2.3:41000",
		headers: map[string][]string{"X-Forwarded-For": {"198.51.100.7", "10.4.5.6"}},
		want:    "198.51.100.7",
	}, {
		name:    "rubbish in the chain stops the walk rather than being taken",
		peer:    "10.1.2.3:41000",
		headers: map[string][]string{"X-Forwarded-For": {"not-an-address, 10.4.5.6"}},
		want:    "10.4.5.6",
	}, {
		name:    "X-Real-IP is read when there is no chain",
		peer:    "127.0.0.1:41000",
		headers: map[string][]string{"X-Real-IP": {"198.51.100.7"}},
		want:    "198.51.100.7",
	}, {
		name: "a peer with no header at all is itself",
		peer: "203.0.113.9:41000",
		want: "203.0.113.9",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ws", nil)
			req.RemoteAddr = tc.peer
			for name, values := range tc.headers {
				for _, value := range values {
					req.Header.Add(name, value)
				}
			}

			prefixes := trusted
			if tc.trusted != nil {
				prefixes, err = parseTrustedProxies(tc.trusted)
				if err != nil {
					t.Fatalf("parse trusted proxies: %v", err)
				}
			}

			if got := clientIP(req, prefixes); got != tc.want {
				t.Errorf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseTrustedProxiesAcceptsBareAddressesAndRanges(t *testing.T) {
	prefixes, err := parseTrustedProxies([]string{"127.0.0.1", "10.0.0.0/8", "  ", "::1"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(prefixes) != 3 {
		t.Fatalf("got %d prefixes, want 3 (the blank entry is skipped)", len(prefixes))
	}
	// A bare address must cover itself and nothing else.
	if prefixes[0].Bits() != 32 {
		t.Errorf("a bare IPv4 address became a /%d, want /32", prefixes[0].Bits())
	}
}

func TestParseTrustedProxiesRejectsNonsense(t *testing.T) {
	if _, err := parseTrustedProxies([]string{"example.com"}); err == nil {
		t.Error("a hostname was accepted; only addresses and ranges can be matched against a peer")
	}
}
