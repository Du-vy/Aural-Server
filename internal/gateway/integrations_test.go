package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/protocol"
)

const testKlipyKey = "klipy-secret-not-for-clients"

// withKlipy gives a harness a Klipy credential to keep.
func withKlipy(cfg *config.Config) {
	cfg.Integrations.KlipyAPIKey = testKlipyKey
}

// get makes a plain GET against the harness, with a bearer token when one is
// given.
func (h *harness) get(path, token string) *http.Response {
	h.t.Helper()

	req, err := http.NewRequest(http.MethodGet, h.http.URL+path, nil)
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("get %s: %v", path, err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// bodyOf reads a response whole, so a test can assert on what is not in it.
func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw)
}

func TestServerPreviewDoesNotCarryTheKlipyKey(t *testing.T) {
	h := newHarness(t, withKlipy)

	// GET /info is unauthenticated by design, so everything it carries is
	// public. A credential in it is a credential anyone on the internet holds.
	body := bodyOf(t, h.get("/info", ""))
	if strings.Contains(body, testKlipyKey) {
		t.Fatalf("the public server preview carried the Klipy key: %s", body)
	}

	var info protocol.ServerInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		t.Fatalf("decode info: %v", err)
	}
	if !info.KlipyEnabled {
		t.Fatal("a server holding a key should report the integration as available")
	}
}

func TestHelloDoesNotCarryTheKlipyKey(t *testing.T) {
	h := newHarness(t, withKlipy)

	// The greeting reaches a connection before it has authenticated at all, so
	// it is as public as the preview.
	c := h.dial()
	ready := c.guest("Ana")

	if ready.Server.KlipyEnabled != true {
		t.Fatal("the ready snapshot should report the integration as available")
	}
	raw, err := json.Marshal(ready.Server)
	if err != nil {
		t.Fatalf("encode server info: %v", err)
	}
	if strings.Contains(string(raw), testKlipyKey) {
		t.Fatalf("the server info handed to a client carried the Klipy key: %s", raw)
	}
}

func TestKlipyProxyRefusesAnonymousCallers(t *testing.T) {
	h := newHarness(t, withKlipy)

	// The proxy spends the operator's hourly Klipy allowance, so it is for
	// members rather than for whoever finds the address.
	for _, path := range []string{
		"/klipy/gifs/trending",
		"/klipy/gifs/categories",
		"/klipy/stickers/search?q=cat",
	} {
		resp := h.get(path, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: got %d, want 401", path, resp.StatusCode)
		}
		if body := bodyOf(t, resp); strings.Contains(body, testKlipyKey) {
			t.Fatalf("%s: the refusal carried the key: %s", path, body)
		}
	}
}

func TestKlipyProxyRejectsUnknownLookups(t *testing.T) {
	h := newHarness(t, withKlipy)

	c := h.dial()
	ready := c.guest("Ana")

	// Only the collections and lookups this server proxies are reachable: the
	// path is built into an upstream URL, so nothing arbitrary may reach it.
	for _, path := range []string{
		"/klipy/evil/trending",
		"/klipy/gifs/delete",
		"/klipy/stickers/categories",
	} {
		resp := h.get(path, ready.SessionToken)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: got %d, want 404", path, resp.StatusCode)
		}
	}

	// A search with no term never reaches Klipy either.
	if resp := h.get("/klipy/gifs/search", ready.SessionToken); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an empty search: got %d, want 400", resp.StatusCode)
	}
}

func TestKlipyProxyIsOffWithoutAKey(t *testing.T) {
	h := newHarness(t, nil)

	c := h.dial()
	ready := c.guest("Ana")

	resp := h.get("/klipy/gifs/trending", ready.SessionToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d, want 403 on a server with no key", resp.StatusCode)
	}

	info := bodyOf(t, h.get("/info", ""))
	if strings.Contains(info, `"klipyEnabled":true`) {
		t.Fatal("a server with no key should not advertise the integration")
	}
}

func TestUnfurlRefusesAnonymousCallers(t *testing.T) {
	h := newHarness(t, nil)

	// Unfurling makes the server fetch a URL somebody names, from its own
	// address. Open, that is an anonymous fetcher pointed at the internet.
	resp := h.get("/unfurl?url=https://example.com", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", resp.StatusCode)
	}

	resp = h.get("/unfurl?url=https://example.com", "not-a-real-token")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("with a bad token: got %d, want 401", resp.StatusCode)
	}
}

func TestUnfurlRefusesPrivateAndDisallowedTargets(t *testing.T) {
	h := newHarness(t, nil)

	c := h.dial()
	ready := c.guest("Ana")

	// A member may unfurl, but not use the server to reach what only the server
	// can reach, and not through a scheme that was never the point.
	for _, target := range []string{
		"http://127.0.0.1:9999/",
		"http://[::1]:9999/",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/",
		"file:///etc/passwd",
		"gopher://example.com/",
	} {
		resp := h.get("/unfurl?url="+target, ready.SessionToken)
		if resp.StatusCode < 400 {
			t.Fatalf("%s: got %d, want a refusal", target, resp.StatusCode)
		}
	}
}
