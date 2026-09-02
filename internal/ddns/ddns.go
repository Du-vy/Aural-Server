// Package ddns keeps a dynamic DNS record pointing at this server.
//
// A home connection has an address its provider may change at any time, and a
// server on one is reachable only for as long as some name resolves to the
// current one. That is usually a separate program — ddclient, a router's own
// updater, a cron job — which is one more thing to install, configure and
// notice the failure of. Since the server already has to know its own public
// address to advertise it for voice, it may as well publish it too.
//
// Two providers are supported, chosen because they are what a self-hosted
// server actually sits behind: DuckDNS, which is free and needs a token and
// nothing else, and Cloudflare, which is where a person who owns a domain
// already keeps it.
package ddns

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Provider names, as they appear in the configuration file.
const (
	ProviderDuckDNS    = "duckdns"
	ProviderCloudflare = "cloudflare"
)

// requestTimeout bounds one call to a provider's API.
const requestTimeout = 20 * time.Second

// Settings is what an operator configures. It mirrors the ddns block of the
// configuration file.
type Settings struct {
	Provider string
	// Domain is the record to keep current: a DuckDNS subdomain, with or
	// without the suffix, or a fully qualified name on Cloudflare.
	Domain string
	// Token authenticates to the provider. For Cloudflare it is an API token
	// — not a global key — with permission to edit DNS on the zone.
	Token string
	// ZoneID is the Cloudflare zone. It may be left empty, in which case the
	// zone is looked up by name, which needs the token to be able to read
	// zones as well as edit them.
	ZoneID string
	// Proxied asks Cloudflare to serve the record through its proxy. It is off
	// by default, and should stay off for any record voice media travels to:
	// the proxy does not carry UDP at all, and only carries WebSocket traffic
	// on the handful of ports it accepts.
	Proxied bool
}

// Updater publishes an address to one provider, and remembers what it last
// published so an unchanged address costs nothing.
type Updater struct {
	settings Settings
	client   *http.Client

	// published is the address the record is believed to hold. It is a cache
	// of this server's own writes, not a reading of the zone, so it is only
	// ever used to skip work.
	published netip.Addr
	// recordID is the Cloudflare record being edited, looked up once and kept.
	recordID string
	zoneID   string

	// The API roots. They are fields rather than constants so a test can point
	// them at a server it controls: every path here is an HTTP call, and one
	// that is never exercised is one whose parameter names are guesses.
	duckDNS    string
	cloudflare string
}

// New builds an updater, rejecting settings it could not act on.
func New(settings Settings) (*Updater, error) {
	settings.Domain = strings.TrimSpace(settings.Domain)
	settings.Token = strings.TrimSpace(settings.Token)
	settings.ZoneID = strings.TrimSpace(settings.ZoneID)

	switch settings.Provider {
	case ProviderDuckDNS, ProviderCloudflare:
	default:
		return nil, fmt.Errorf("ddns: provider %q is not one of %q or %q",
			settings.Provider, ProviderDuckDNS, ProviderCloudflare)
	}
	if settings.Domain == "" {
		return nil, fmt.Errorf("ddns: %s needs a domain", settings.Provider)
	}
	if settings.Token == "" {
		return nil, fmt.Errorf("ddns: %s needs a token", settings.Provider)
	}

	return &Updater{
		settings:   settings,
		client:     &http.Client{Timeout: requestTimeout},
		zoneID:     settings.ZoneID,
		duckDNS:    duckDNSEndpoint,
		cloudflare: cloudflareAPI,
	}, nil
}

// Describe names what is being kept current, for the startup log.
func (u *Updater) Describe() string {
	return u.settings.Provider + ":" + u.settings.Domain
}

// Published is the address last written, which is the zero value until one has
// been.
func (u *Updater) Published() netip.Addr { return u.published }

// Update points the record at addr, unless it already does.
//
// It reports whether it wrote anything, so a caller can log a change and stay
// quiet the rest of the time — which is nearly always, since an address that
// changes twice a month is checked several thousand times in between.
func (u *Updater) Update(ctx context.Context, addr netip.Addr) (changed bool, err error) {
	if !addr.IsValid() {
		return false, fmt.Errorf("ddns: refusing to publish an invalid address")
	}
	if addr == u.published {
		return false, nil
	}

	switch u.settings.Provider {
	case ProviderDuckDNS:
		err = u.updateDuckDNS(ctx, addr)
	case ProviderCloudflare:
		err = u.updateCloudflare(ctx, addr)
	}
	if err != nil {
		return false, err
	}
	u.published = addr
	return true, nil
}

// --- DNS-01 challenge records -----------------------------------------------

// ChallengeName is the record an ACME DNS-01 challenge is answered at.
func ChallengeName(domain string) string {
	return "_acme-challenge." + strings.TrimSuffix(domain, ".")
}

// SetChallenge publishes the TXT record that answers a DNS-01 challenge for
// domain, and returns a function that removes it again.
//
// This is here rather than in the ACME code because answering the challenge is
// a DNS operation, and this package is what already knows how to talk to the
// operator's DNS. It is also why DNS-01 is the challenge worth supporting at
// all for this kind of deployment: HTTP-01 needs port 80 reachable from the
// internet, which a residential connection frequently does not offer, while
// this needs nothing inbound whatsoever.
func (u *Updater) SetChallenge(ctx context.Context, domain, value string) (remove func(context.Context) error, err error) {
	switch u.settings.Provider {
	case ProviderDuckDNS:
		if err := u.setDuckDNSTXT(ctx, value); err != nil {
			return nil, err
		}
		return func(ctx context.Context) error { return u.clearDuckDNSTXT(ctx) }, nil

	case ProviderCloudflare:
		id, err := u.setCloudflareTXT(ctx, domain, value)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) error { return u.deleteCloudflareRecord(ctx, id) }, nil
	}
	return nil, fmt.Errorf("ddns: %s cannot answer a DNS-01 challenge", u.settings.Provider)
}

// setDuckDNSTXT sets the single TXT record DuckDNS keeps for a subdomain. It
// serves it at _acme-challenge.<subdomain>.duckdns.org, which is exactly where
// the challenge is looked for.
func (u *Updater) setDuckDNSTXT(ctx context.Context, value string) error {
	query := url.Values{}
	query.Set("domains", strings.TrimSuffix(u.settings.Domain, ".duckdns.org"))
	query.Set("token", u.settings.Token)
	query.Set("txt", value)
	return u.callDuckDNS(ctx, query)
}

// clearDuckDNSTXT removes it again. DuckDNS holds one TXT record per
// subdomain, so leaving a spent challenge behind would collide with the next
// renewal.
func (u *Updater) clearDuckDNSTXT(ctx context.Context) error {
	query := url.Values{}
	query.Set("domains", strings.TrimSuffix(u.settings.Domain, ".duckdns.org"))
	query.Set("token", u.settings.Token)
	query.Set("txt", "")
	query.Set("clear", "true")
	return u.callDuckDNS(ctx, query)
}

// setCloudflareTXT creates the challenge record and returns its id, so it can
// be deleted once the challenge has been answered.
func (u *Updater) setCloudflareTXT(ctx context.Context, domain, value string) (string, error) {
	if u.zoneID == "" {
		zone, err := u.findCloudflareZone(ctx)
		if err != nil {
			return "", err
		}
		u.zoneID = zone
	}

	name := ChallengeName(domain)
	// A challenge record left behind by an interrupted renewal would be
	// served alongside the new one, and a validator that reads the stale value
	// fails the order. They are cleared before the new one is written.
	if err := u.clearCloudflareTXT(ctx, name); err != nil {
		return "", err
	}

	body, err := json.Marshal(map[string]any{
		"type":    "TXT",
		"name":    name,
		"content": value,
		// A challenge lives for seconds. Nothing should cache it.
		"ttl": 60,
	})
	if err != nil {
		return "", err
	}
	raw, err := u.callCloudflare(ctx, http.MethodPost,
		fmt.Sprintf("/zones/%s/dns_records", u.zoneID), body)
	if err != nil {
		return "", err
	}
	var record struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return "", fmt.Errorf("ddns: cloudflare: unreadable challenge record: %w", err)
	}
	return record.ID, nil
}

// clearCloudflareTXT removes every TXT record at name.
func (u *Updater) clearCloudflareTXT(ctx context.Context, name string) error {
	raw, err := u.callCloudflare(ctx, http.MethodGet,
		fmt.Sprintf("/zones/%s/dns_records?type=TXT&name=%s", u.zoneID, url.QueryEscape(name)), nil)
	if err != nil {
		return err
	}
	var records []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &records); err != nil {
		return fmt.Errorf("ddns: cloudflare: unreadable record list: %w", err)
	}
	for _, record := range records {
		if err := u.deleteCloudflareRecord(ctx, record.ID); err != nil {
			return err
		}
	}
	return nil
}

func (u *Updater) deleteCloudflareRecord(ctx context.Context, id string) error {
	_, err := u.callCloudflare(ctx, http.MethodDelete,
		fmt.Sprintf("/zones/%s/dns_records/%s", u.zoneID, id), nil)
	return err
}

// --- DuckDNS ----------------------------------------------------------------

// duckDNSEndpoint is the whole of the DuckDNS API.
const duckDNSEndpoint = "https://www.duckdns.org/update"

// updateDuckDNS points the address record at addr.
func (u *Updater) updateDuckDNS(ctx context.Context, addr netip.Addr) error {
	// The API takes the subdomain, not the whole name, and an operator will
	// write whichever of the two they think of first.
	domain := strings.TrimSuffix(u.settings.Domain, ".duckdns.org")

	query := url.Values{}
	query.Set("domains", domain)
	query.Set("token", u.settings.Token)
	// DuckDNS keeps separate A and AAAA records and picks by parameter name.
	if addr.Is6() {
		query.Set("ipv6", addr.String())
	} else {
		query.Set("ip", addr.String())
	}
	return u.callDuckDNS(ctx, query)
}

// callDuckDNS performs one API call. The whole API is a GET that answers with
// the word OK or the word KO and nothing else, so the body is the only thing
// that says whether it worked: the status is 200 either way.
func (u *Updater) callDuckDNS(ctx context.Context, query url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.duckDNS+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	res, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("ddns: duckdns: %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 1024))
	if err != nil {
		return fmt.Errorf("ddns: duckdns: read reply: %w", err)
	}
	if answer := strings.TrimSpace(string(body)); !strings.HasPrefix(answer, "OK") {
		// The token is in the query, never in the error: this string reaches a
		// log file.
		return fmt.Errorf("ddns: duckdns refused the request for %s (answered %q)",
			query.Get("domains"), answer)
	}
	return nil
}

// --- Cloudflare -------------------------------------------------------------

// cloudflareAPI is the base of the v4 API.
const cloudflareAPI = "https://api.cloudflare.com/client/v4"

// cloudflareReply is the envelope every endpoint answers with. Cloudflare
// reports failure in the body as well as in the status, and the messages there
// are the ones worth showing an operator.
type cloudflareReply struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

// updateCloudflare points an A or AAAA record at addr, discovering the zone
// and the record the first time it is asked.
func (u *Updater) updateCloudflare(ctx context.Context, addr netip.Addr) error {
	if u.zoneID == "" {
		zone, err := u.findCloudflareZone(ctx)
		if err != nil {
			return err
		}
		u.zoneID = zone
	}
	if u.recordID == "" {
		record, err := u.findCloudflareRecord(ctx, addr)
		if err != nil {
			return err
		}
		u.recordID = record
	}

	body, err := json.Marshal(map[string]any{
		"type":    recordType(addr),
		"name":    u.settings.Domain,
		"content": addr.String(),
		"proxied": u.settings.Proxied,
	})
	if err != nil {
		return err
	}

	path := fmt.Sprintf("/zones/%s/dns_records/%s", u.zoneID, u.recordID)
	if _, err := u.callCloudflare(ctx, http.MethodPatch, path, body); err != nil {
		// A record deleted or recreated behind our back invalidates the id we
		// cached. Forgetting it means the next attempt looks it up again
		// rather than failing the same way forever.
		u.recordID = ""
		return err
	}
	return nil
}

// findCloudflareZone resolves the zone holding the configured name, by trying
// the name's parent domains from the most specific down. A record on
// "aural.example.com" lives in the zone "example.com", and an operator should
// not have to know which of the two to write where.
func (u *Updater) findCloudflareZone(ctx context.Context) (string, error) {
	labels := strings.Split(u.settings.Domain, ".")
	for i := 0; i+1 < len(labels); i++ {
		candidate := strings.Join(labels[i:], ".")

		raw, err := u.callCloudflare(ctx, http.MethodGet,
			"/zones?name="+url.QueryEscape(candidate), nil)
		if err != nil {
			return "", err
		}
		var zones []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &zones); err != nil {
			return "", fmt.Errorf("ddns: cloudflare: unreadable zone list: %w", err)
		}
		if len(zones) > 0 {
			return zones[0].ID, nil
		}
	}
	return "", fmt.Errorf("ddns: cloudflare holds no zone for %s; set ddns.zone_id if the token cannot list zones",
		u.settings.Domain)
}

// findCloudflareRecord finds the record to edit, creating it when the zone
// does not have one yet. A first run on a fresh name is otherwise a manual
// step for no reason: the whole point is that the operator does not maintain
// this record by hand.
func (u *Updater) findCloudflareRecord(ctx context.Context, addr netip.Addr) (string, error) {
	query := url.Values{}
	query.Set("type", recordType(addr))
	query.Set("name", u.settings.Domain)

	raw, err := u.callCloudflare(ctx, http.MethodGet,
		fmt.Sprintf("/zones/%s/dns_records?%s", u.zoneID, query.Encode()), nil)
	if err != nil {
		return "", err
	}
	var records []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &records); err != nil {
		return "", fmt.Errorf("ddns: cloudflare: unreadable record list: %w", err)
	}
	if len(records) > 0 {
		return records[0].ID, nil
	}

	body, err := json.Marshal(map[string]any{
		"type":    recordType(addr),
		"name":    u.settings.Domain,
		"content": addr.String(),
		"proxied": u.settings.Proxied,
		// One minute, which is the shortest Cloudflare accepts. A record whose
		// address changes without warning is one nobody should be caching.
		"ttl": 60,
	})
	if err != nil {
		return "", err
	}
	created, err := u.callCloudflare(ctx, http.MethodPost,
		fmt.Sprintf("/zones/%s/dns_records", u.zoneID), body)
	if err != nil {
		return "", err
	}
	var record struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &record); err != nil {
		return "", fmt.Errorf("ddns: cloudflare: unreadable created record: %w", err)
	}
	return record.ID, nil
}

// callCloudflare performs one API call and returns the result field, turning
// the errors reported in the body into ordinary Go errors.
func (u *Updater) callCloudflare(ctx context.Context, method, path string, body []byte) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}

	req, err := http.NewRequestWithContext(ctx, method, u.cloudflare+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+u.settings.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ddns: cloudflare: %w", err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("ddns: cloudflare: read reply: %w", err)
	}

	var reply cloudflareReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("ddns: cloudflare answered %s with something unreadable", res.Status)
	}
	if !reply.Success {
		return nil, fmt.Errorf("ddns: cloudflare refused the request: %s", describeErrors(reply))
	}
	return reply.Result, nil
}

// describeErrors renders whatever Cloudflare said went wrong. The status alone
// is rarely enough — a token missing one permission and a token that does not
// exist are both a 403.
func describeErrors(reply cloudflareReply) string {
	if len(reply.Errors) == 0 {
		return "no reason given"
	}
	parts := make([]string, 0, len(reply.Errors))
	for _, e := range reply.Errors {
		parts = append(parts, fmt.Sprintf("%d %s", e.Code, e.Message))
	}
	return strings.Join(parts, "; ")
}

// recordType is the DNS record an address belongs in.
func recordType(addr netip.Addr) string {
	if addr.Is6() {
		return "AAAA"
	}
	return "A"
}
