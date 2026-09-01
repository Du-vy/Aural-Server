package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// Klipy is reached through the server rather than from the client, for two
// reasons that point the same way.
//
// The credential is the operator's. Klipy puts it in the request path, so a
// client holding it would leak it into every proxy log it passes through, and
// handing it out at all makes it public the moment one member looks at their
// network tab. It stays here.
//
// And a Klipy key is rated by the hour, not by the member. One cache in front
// of one key serves the whole server: a room full of people opening the GIF
// picker on the same trending list costs one upstream call rather than one
// each, which is the difference between a development key working and not.
const (
	klipyBaseURL = "https://api.klipy.com/api/v1"
	// klipyCacheTTL is how long a listing is reused. Trending moves slowly and
	// a search for the same word does not move at all; a quarter of an hour is
	// far below what anybody notices and far above what the rate limit costs.
	klipyCacheTTL = 15 * time.Minute
	// maxKlipyBodyBytes bounds what an upstream answer may be. A listing of
	// thirty items is a few tens of kilobytes.
	maxKlipyBodyBytes = 2 * 1024 * 1024
	// klipyBurst and klipyPerSecond throttle one member. The burst covers
	// opening the picker and refining a search a few times; the refill is what
	// stops one member from spending the whole server's hourly allowance.
	klipyBurst     = 12
	klipyPerSecond = 0.5
	// maxKlipyQueryRunes bounds a search term. Klipy rejects longer ones and
	// there is no reason to spend a request finding that out.
	maxKlipyQueryRunes = 100
	// klipyDefaultLimit and klipyMaxLimit bound how many items are asked for.
	klipyDefaultLimit = 30
	klipyMaxLimit     = 50
)

// klipyCache holds upstream answers by request, shared across every member of
// the server.
type klipyCache struct {
	mu sync.Mutex
	by map[string]klipyEntry
}

type klipyEntry struct {
	body    []byte
	fetched time.Time
}

var klipyResponses = &klipyCache{by: map[string]klipyEntry{}}

// get returns a cached body that is still fresh.
func (c *klipyCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.by[key]
	if !ok || time.Since(entry.fetched) > klipyCacheTTL {
		return nil, false
	}
	return entry.body, true
}

// put stores a body, dropping everything stale on the way. The map is bounded
// by the number of distinct searches made within one TTL, and a sweep on write
// is what keeps it from growing past that.
func (c *klipyCache) put(key string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, entry := range c.by {
		if time.Since(entry.fetched) > klipyCacheTTL {
			delete(c.by, k)
		}
	}
	c.by[key] = klipyEntry{body: body, fetched: time.Now()}
}

// klipyClient is shared so that connections to Klipy are reused across
// requests rather than dialled fresh for each one.
var klipyClient = &http.Client{Timeout: 8 * time.Second}

// handleKlipy proxies one GIF or sticker lookup to Klipy under the server's own
// credential.
//
//	GET /klipy/{kind}/{action}?q=<term>&limit=<n>
//	Authorization: Bearer <session token>
//
// kind is gifs or stickers; action is categories, trending or search.
func (s *Server) handleKlipy(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)

	apiKey := s.hub.KlipyAPIKey()
	if apiKey == "" {
		writeAPIError(w, http.StatusForbidden, "klipy_disabled",
			"this server has no Klipy integration configured")
		return
	}

	// Proxying under the server's credential is a favour to members, not to the
	// internet: without this the endpoint would spend the operator's hourly
	// allowance for anybody who found it.
	user, failure := s.authenticateRequest(r)
	if failure != nil {
		writeProtocolError(w, failure)
		return
	}

	kind := r.PathValue("kind")
	if kind != "gifs" && kind != "stickers" {
		writeAPIError(w, http.StatusNotFound, protocol.ErrNotFound, "no such Klipy collection")
		return
	}
	action := r.PathValue("action")
	switch action {
	case "categories", "trending", "search":
	default:
		writeAPIError(w, http.StatusNotFound, protocol.ErrNotFound, "no such Klipy lookup")
		return
	}
	if action == "categories" && kind != "gifs" {
		writeAPIError(w, http.StatusNotFound, protocol.ErrNotFound, "only gifs have categories")
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(query)) > maxKlipyQueryRunes {
		writeAPIError(w, http.StatusBadRequest, protocol.ErrBadRequest,
			fmt.Sprintf("a search term may be at most %d characters", maxKlipyQueryRunes))
		return
	}
	if action == "search" && query == "" {
		writeAPIError(w, http.StatusBadRequest, protocol.ErrBadRequest, "a search needs a term")
		return
	}
	// Lowercased so that two people searching the same word in different case
	// share one cache entry and one upstream call.
	query = strings.ToLower(query)

	limit := klipyDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			writeAPIError(w, http.StatusBadRequest, protocol.ErrBadRequest, "limit must be a positive number")
			return
		}
		limit = min(parsed, klipyMaxLimit)
	}

	cacheKey := kind + "/" + action + "?q=" + query + "&limit=" + strconv.Itoa(limit)
	if body, fresh := klipyResponses.get(cacheKey); fresh {
		writeKlipyBody(w, body)
		return
	}

	// The limiter is checked only once the cache has missed: a member scrolling
	// a list everybody else has already loaded costs Klipy nothing, so it
	// should cost them nothing either.
	if !s.klipy.allow(user.ID) {
		writeAPIError(w, http.StatusTooManyRequests, protocol.ErrRateLimited,
			"you are browsing GIFs too quickly")
		return
	}

	// The key travels in the path, which is Klipy's design rather than a choice
	// here; escaping it is what keeps a malformed one from reshaping the URL.
	target := fmt.Sprintf("%s/%s/%s/%s", klipyBaseURL, url.PathEscape(apiKey), kind, action)
	params := url.Values{}
	if action != "categories" {
		params.Set("limit", strconv.Itoa(limit))
	}
	if action == "search" {
		params.Set("q", query)
	}
	if len(params) > 0 {
		target += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, protocol.ErrInternal, "could not build the Klipy request")
		return
	}
	req.Header.Set("Accept", "application/json")

	resp, err := klipyClient.Do(req)
	if err != nil {
		// The URL carries the credential, so the error is logged without it.
		s.log.Warn("klipy request failed",
			slog.String("kind", kind), slog.String("action", action), slog.Any("error", err))
		writeAPIError(w, http.StatusBadGateway, "klipy_failed", "could not reach Klipy")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Upstream bodies are not passed through: a rejected key is answered
		// with a body that quotes it back, and that is the one thing this
		// endpoint exists to keep in.
		s.log.Warn("klipy answered an error",
			slog.String("kind", kind), slog.String("action", action), slog.Int("status", resp.StatusCode))
		status := http.StatusBadGateway
		if resp.StatusCode == http.StatusTooManyRequests {
			status = http.StatusTooManyRequests
		}
		writeAPIError(w, status, "klipy_failed",
			fmt.Sprintf("Klipy answered %d", resp.StatusCode))
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxKlipyBodyBytes))
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "klipy_failed", "could not read the Klipy answer")
		return
	}
	if !json.Valid(body) {
		writeAPIError(w, http.StatusBadGateway, "klipy_failed", "Klipy answered with something that is not JSON")
		return
	}

	klipyResponses.put(cacheKey, body)
	writeKlipyBody(w, body)
}

// writeKlipyBody hands an upstream answer back untouched. The client reads the
// same shape it would have read from Klipy directly, so the proxy is invisible
// to it beyond the address.
func writeKlipyBody(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Private: the answer is the same for everybody, but it was fetched under a
	// credential and there is no reason for a shared proxy to hold it.
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = w.Write(body)
}
