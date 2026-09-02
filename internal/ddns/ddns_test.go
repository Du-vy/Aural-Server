package ddns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// newTestUpdater builds an updater pointed at a server the test controls.
func newTestUpdater(t *testing.T, settings Settings, handler http.Handler) (*Updater, *httptest.Server) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	updater, err := New(settings)
	if err != nil {
		t.Fatalf("new updater: %v", err)
	}
	updater.duckDNS = server.URL + "/update"
	updater.cloudflare = server.URL
	return updater, server
}

func TestDuckDNSPublishesTheAddress(t *testing.T) {
	var got struct {
		domains, token, ip string
	}
	updater, _ := newTestUpdater(t,
		Settings{Provider: ProviderDuckDNS, Domain: "aural.duckdns.org", Token: "secret"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			got.domains, got.token, got.ip = q.Get("domains"), q.Get("token"), q.Get("ip")
			_, _ = w.Write([]byte("OK"))
		}))

	changed, err := updater.Update(context.Background(), netip.MustParseAddr("203.0.113.5"))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !changed {
		t.Error("the first update reported no change")
	}
	// DuckDNS takes the subdomain, not the whole name, whichever of the two
	// the operator wrote in the file.
	if got.domains != "aural" {
		t.Errorf("sent domains=%q, want %q", got.domains, "aural")
	}
	if got.token != "secret" {
		t.Errorf("sent token=%q, want %q", got.token, "secret")
	}
	if got.ip != "203.0.113.5" {
		t.Errorf("sent ip=%q, want %q", got.ip, "203.0.113.5")
	}
}

// The API answers 200 whether it worked or not, so the body is the only thing
// that says so. A version of this that only checked the status would report
// success forever on a bad token.
func TestDuckDNSTreatsKOAsAFailure(t *testing.T) {
	updater, _ := newTestUpdater(t,
		Settings{Provider: ProviderDuckDNS, Domain: "aural", Token: "wrong"},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("KO"))
		}))

	_, err := updater.Update(context.Background(), netip.MustParseAddr("203.0.113.5"))
	if err == nil {
		t.Fatal("a KO answer was taken as success")
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Errorf("the token leaked into an error that reaches a log file: %v", err)
	}
	if updater.Published().IsValid() {
		t.Error("a failed update was recorded as published, so the retry would be skipped")
	}
}

func TestUpdateSkipsAnAddressThatHasNotMoved(t *testing.T) {
	calls := 0
	updater, _ := newTestUpdater(t,
		Settings{Provider: ProviderDuckDNS, Domain: "aural", Token: "secret"},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			_, _ = w.Write([]byte("OK"))
		}))

	addr := netip.MustParseAddr("203.0.113.5")
	for range 5 {
		if _, err := updater.Update(context.Background(), addr); err != nil {
			t.Fatalf("update: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("called the provider %d times for one address; an unchanged address must cost nothing", calls)
	}

	changed, err := updater.Update(context.Background(), netip.MustParseAddr("198.51.100.9"))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !changed || calls != 2 {
		t.Errorf("a genuinely new address was not published (changed=%v, calls=%d)", changed, calls)
	}
}

func TestDuckDNSChallengeSetsAndClearsTheTXTRecord(t *testing.T) {
	var seen []string
	updater, _ := newTestUpdater(t,
		Settings{Provider: ProviderDuckDNS, Domain: "aural", Token: "secret"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			seen = append(seen, q.Get("txt")+"|"+q.Get("clear"))
			_, _ = w.Write([]byte("OK"))
		}))

	remove, err := updater.SetChallenge(context.Background(), "aural.duckdns.org", "challenge-value")
	if err != nil {
		t.Fatalf("set challenge: %v", err)
	}
	if err := remove(context.Background()); err != nil {
		t.Fatalf("remove challenge: %v", err)
	}

	if len(seen) != 2 {
		t.Fatalf("made %d calls, want 2 (one to set, one to clear)", len(seen))
	}
	if seen[0] != "challenge-value|" {
		t.Errorf("set sent %q, want the challenge value", seen[0])
	}
	// DuckDNS holds one TXT per subdomain, so a spent challenge left behind
	// would collide with the next renewal.
	if seen[1] != "|true" {
		t.Errorf("clear sent %q, want an empty value with clear=true", seen[1])
	}
}

// cloudflareOK writes the envelope every Cloudflare endpoint answers with.
func cloudflareOK(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
}

func TestCloudflarePatchesTheExistingRecord(t *testing.T) {
	var (
		auth    string
		patched map[string]any
	)
	updater, _ := newTestUpdater(t,
		Settings{Provider: ProviderCloudflare, Domain: "aural.example.com", Token: "cf-token", ZoneID: "zone1"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth = r.Header.Get("Authorization")
			switch {
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/dns_records"):
				cloudflareOK(w, []map[string]any{{"id": "rec1"}})
			case r.Method == http.MethodPatch:
				_ = json.NewDecoder(r.Body).Decode(&patched)
				cloudflareOK(w, map[string]any{"id": "rec1"})
			default:
				t.Errorf("unexpected %s %s", r.Method, r.URL)
				cloudflareOK(w, nil)
			}
		}))

	if _, err := updater.Update(context.Background(), netip.MustParseAddr("203.0.113.5")); err != nil {
		t.Fatalf("update: %v", err)
	}

	if auth != "Bearer cf-token" {
		t.Errorf("sent Authorization %q, want a bearer token", auth)
	}
	if patched["content"] != "203.0.113.5" {
		t.Errorf("patched content=%v, want the new address", patched["content"])
	}
	if patched["type"] != "A" {
		t.Errorf("patched type=%v, want A for an IPv4 address", patched["type"])
	}
	// The proxy carries no UDP and only some ports, so a record voice arrives
	// on must not be put behind it by accident.
	if patched["proxied"] != false {
		t.Errorf("patched proxied=%v, want false unless the operator asked", patched["proxied"])
	}
}

func TestCloudflareCreatesARecordThatDoesNotExistYet(t *testing.T) {
	var created map[string]any
	updater, _ := newTestUpdater(t,
		Settings{Provider: ProviderCloudflare, Domain: "aural.example.com", Token: "cf-token", ZoneID: "zone1"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				cloudflareOK(w, []map[string]any{})
			case http.MethodPost:
				_ = json.NewDecoder(r.Body).Decode(&created)
				cloudflareOK(w, map[string]any{"id": "rec-new"})
			case http.MethodPatch:
				cloudflareOK(w, map[string]any{"id": "rec-new"})
			}
		}))

	if _, err := updater.Update(context.Background(), netip.MustParseAddr("203.0.113.5")); err != nil {
		t.Fatalf("update: %v", err)
	}
	if created["name"] != "aural.example.com" {
		t.Errorf("created name=%v, want the configured domain", created["name"])
	}
}

// Cloudflare reports failure in the body as well as in the status, and the
// message there is the only thing that distinguishes a token missing one
// permission from a token that does not exist.
func TestCloudflareReportsWhatTheAPISaid(t *testing.T) {
	updater, _ := newTestUpdater(t,
		Settings{Provider: ProviderCloudflare, Domain: "aural.example.com", Token: "cf-token", ZoneID: "zone1"},
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"errors":  []map[string]any{{"code": 10000, "message": "Authentication error"}},
			})
		}))

	_, err := updater.Update(context.Background(), netip.MustParseAddr("203.0.113.5"))
	if err == nil {
		t.Fatal("a refused request was taken as success")
	}
	if !strings.Contains(err.Error(), "Authentication error") {
		t.Errorf("the reason was dropped: %v", err)
	}
}

func TestCloudflareFindsTheZoneFromTheRecordName(t *testing.T) {
	var asked []string
	updater, _ := newTestUpdater(t,
		Settings{Provider: ProviderCloudflare, Domain: "aural.example.com", Token: "cf-token"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/zones") && r.URL.Query().Get("name") != "" {
				name := r.URL.Query().Get("name")
				asked = append(asked, name)
				// Only the registrable domain is a zone; the record's own name
				// is not.
				if name == "example.com" {
					cloudflareOK(w, []map[string]any{{"id": "zone1", "name": name}})
					return
				}
				cloudflareOK(w, []map[string]any{})
				return
			}
			switch r.Method {
			case http.MethodGet:
				cloudflareOK(w, []map[string]any{{"id": "rec1"}})
			default:
				cloudflareOK(w, map[string]any{"id": "rec1"})
			}
		}))

	if _, err := updater.Update(context.Background(), netip.MustParseAddr("203.0.113.5")); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(asked) == 0 || asked[0] != "aural.example.com" {
		t.Errorf("zone lookup asked %v, want the most specific name first", asked)
	}
}

func TestNewRejectsSettingsItCouldNotActOn(t *testing.T) {
	cases := []Settings{
		{Provider: "route53", Domain: "aural.example.com", Token: "t"},
		{Provider: ProviderDuckDNS, Token: "t"},
		{Provider: ProviderDuckDNS, Domain: "aural"},
	}
	for _, settings := range cases {
		if _, err := New(settings); err == nil {
			t.Errorf("%+v was accepted", settings)
		}
	}
}

func TestChallengeNameIsWhereACMELooks(t *testing.T) {
	if got := ChallengeName("aural.example.com"); got != "_acme-challenge.aural.example.com" {
		t.Errorf("ChallengeName = %q", got)
	}
	if got := ChallengeName("aural.example.com."); got != "_acme-challenge.aural.example.com" {
		t.Errorf("a fully qualified name kept its trailing dot: %q", got)
	}
}
