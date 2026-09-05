package gateway

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// Serving the artwork a game names in its rich presence.
//
// A game does not hand over a picture. It hands over a key — a short name that
// means something only to Discord, where the game's own application keeps its
// uploaded assets — and resolving one means asking Discord what it refers to.
// Somebody has to make that request, and there are only two candidates.
//
// If every member's client made it, Discord would learn who is playing what,
// from how many addresses, every time anybody started a game. Doing it here
// means one request from one address, cached, and a picture that every member
// then loads from the server they were already talking to. That is the whole
// reason this endpoint exists: it is not a convenience, it is the difference
// between a self-hosted server and a room full of clients reporting to a third
// party.
//
// It is a proxy rather than a redirect for the same reason. Handing a client
// a CDN URL would move the request back to where it started.

// activityAssetPrefix is where resolved artwork is served from.
//
// It is a path rather than a whole URL, exactly as an avatar is: the client
// already knows which server it is talking to, and a server that had to know
// its own public address would be one more thing to get wrong behind a proxy.
const activityAssetPrefix = "/activity-assets/"

const (
	// activityAssetBurst and activityAssetsPerSecond throttle one member. A
	// client asks once per picture and then holds it in the page, so the
	// steady rate can be low; the burst covers a member list filling in
	// several games at once when somebody first opens the window.
	activityAssetBurst      = 20
	activityAssetsPerSecond = 2
	// activityAssetMaxBytes caps one picture. Discord is asked for a small
	// size, so anything near this is not the answer that was expected.
	activityAssetMaxBytes = 1 << 20
	// activityAssetTTL is how long a fetched picture is trusted. An app's
	// assets change when the game ships an update, which is not often.
	activityAssetTTL = 24 * time.Hour
	// activityAssetCacheMax is how many pictures are held. Each is a few tens
	// of kilobytes and there are only so many games one community plays.
	activityAssetCacheMax = 256
	// activityAssetEdge is the size asked of the CDN. The largest this is ever
	// drawn is the profile card, at sixty pixels on a screen that may be
	// doubled; anything beyond it is bandwidth spent on nothing.
	activityAssetEdge = 256
)

// Application ids are snowflakes and asset names are a restricted alphabet.
// Both are pasted into a URL aimed at Discord, so both are checked rather than
// escaped: an id that is not digits is not an id, and there is nothing to be
// gained by asking anyway.
var (
	activityAppPattern   = regexp.MustCompile(`^[0-9]{1,25}$`)
	activityAssetPattern = regexp.MustCompile(`^[A-Za-z0-9_\-.]{1,128}$`)
)

// activityAssetClient talks to Discord and to nowhere else.
//
// Every URL it is given is built here out of parts that have been checked
// against the patterns above, so there is no user-named host to defend
// against — but a redirect would be one, which is why none is followed.
var activityAssetClient = &http.Client{
	Timeout: 8 * time.Second,
	Transport: &http.Transport{
		ResponseHeaderTimeout: 6 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       60 * time.Second,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// addressKey folds a client address into the integer the rate limiters are
// keyed by.
//
// The limiters were built for identities and this endpoint has none, but what
// they do — a token bucket per key, swept when it goes idle — is exactly what
// is wanted here. Hashing is only a way of using the same machinery: a
// collision costs two addresses one shared allowance, which is a fair outcome
// for a limit this generous.
func addressKey(address string) int64 {
	sum := fnv.New64a()
	_, _ = sum.Write([]byte(address))
	return int64(sum.Sum64() & 0x7fffffffffffffff)
}

// activityAsset is one picture, held in memory.
type activityAsset struct {
	body        []byte
	contentType string
	fetchedAt   time.Time
}

// activityAssetNames is one application's asset list: the map from the names
// games use to the ids the CDN serves.
type activityAssetNames struct {
	ids       map[string]string
	fetchedAt time.Time
}

// activityAssetCache holds both, because resolving a key needs both and
// neither is worth a round trip twice.
//
// It is memory rather than the database on purpose. These are somebody else's
// pictures, cheap to fetch again, and a server that restarts having forgotten
// them is a server that costs Discord a handful of requests once. Persisting
// them would mean a table, a lifetime and a purge for something that is
// already free to lose.
type activityAssetCache struct {
	mu      sync.Mutex
	images  map[string]activityAsset
	names   map[string]activityAssetNames
	pending map[string]*sync.Mutex
}

func newActivityAssetCache() *activityAssetCache {
	return &activityAssetCache{
		images:  make(map[string]activityAsset),
		names:   make(map[string]activityAssetNames),
		pending: make(map[string]*sync.Mutex),
	}
}

// image returns a cached picture, if there is a fresh one.
func (c *activityAssetCache) image(key string) (activityAsset, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	held, ok := c.images[key]
	if !ok || time.Since(held.fetchedAt) > activityAssetTTL {
		return activityAsset{}, false
	}
	return held, true
}

// put stores a picture, evicting the oldest once there are too many.
func (c *activityAssetCache) put(key string, asset activityAsset) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.images) >= activityAssetCacheMax {
		oldest, at := "", time.Now()
		for held, value := range c.images {
			if value.fetchedAt.Before(at) {
				oldest, at = held, value.fetchedAt
			}
		}
		if oldest != "" {
			delete(c.images, oldest)
		}
	}
	c.images[key] = asset
}

// lockFor serialises the work for one key.
//
// A member list drawing eight people in the same game asks for the same
// picture eight times at once. Without this the server would make eight
// identical requests to Discord and keep the last one, which is the shape of
// a stampede: the cache is only useful if the first miss stops the others.
func (c *activityAssetCache) lockFor(key string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	held, ok := c.pending[key]
	if !ok {
		held = &sync.Mutex{}
		c.pending[key] = held
	}
	return held
}

// handleActivityAsset serves the artwork behind one activity.
//
//	GET /activity-assets/{app}/{key}
func (s *Server) handleActivityAsset(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)

	if !s.cfg.Activity.Assets {
		writeAPIError(w, http.StatusForbidden, "activity_assets_disabled",
			"this server does not fetch game artwork")
		return
	}

	app := r.PathValue("app")
	key := r.PathValue("key")
	if !activityAppPattern.MatchString(app) || !activityAssetPattern.MatchString(key) {
		writeAPIError(w, http.StatusBadRequest, protocol.ErrBadRequest,
			"malformed application or asset reference")
		return
	}
	// A trailing extension is what a browser adds when it is guessing; the
	// reference itself never has one.
	key = strings.TrimSuffix(strings.TrimSuffix(key, ".png"), ".jpg")

	cacheKey := app + "/" + key
	// Answered from memory before anything else is considered. A picture that
	// is already here was fetched when somebody was legitimately playing the
	// game, and it costs nothing to keep serving after they stopped.
	if held, ok := s.activityAssets.image(cacheKey); ok {
		writeActivityAsset(w, held)
		return
	}

	// Only what somebody is actually playing may be fetched.
	//
	// This is the access control, and it is deliberately not a token. The
	// picture is loaded by an `<img>` tag, which cannot carry an Authorization
	// header — the same reason an attachment's unguessable key is its own
	// capability. But an activity asset has no unguessable key: the path is
	// two public Discord identifiers, so the URL protects nothing and does not
	// need to, because the artwork behind it is public too.
	//
	// What did need protecting is the fetch, not the picture: an endpoint that
	// would fetch anything asked of it is an open proxy pointed at a CDN. So
	// the answer is the set of things members are reporting right now, which
	// is both narrower than any token would be and exactly the set this
	// feature needs.
	if !s.hub.ReportsActivityAsset(app, key) {
		writeAPIError(w, http.StatusNotFound, protocol.ErrNotFound,
			"nobody here is reporting that artwork")
		return
	}

	// Keyed by address rather than by identity, for the same reason there is
	// no token: there is nobody to name. It bounds the cost of an address that
	// asks for a great many different pictures in a row.
	if !s.activityAssetLimit.allow(addressKey(clientIP(r, s.trustedProxies))) {
		writeAPIError(w, http.StatusTooManyRequests, protocol.ErrRateLimited,
			"you are asking for artwork too quickly")
		return
	}

	// One fetch per picture, however many members asked at once.
	gate := s.activityAssets.lockFor(cacheKey)
	gate.Lock()
	defer gate.Unlock()
	if held, ok := s.activityAssets.image(cacheKey); ok {
		writeActivityAsset(w, held)
		return
	}

	asset, err := s.fetchActivityAsset(r, app, key)
	if err != nil {
		s.log.Debug("activity asset fetch failed",
			slog.String("app", app), slog.String("asset", key), slog.Any("error", err))
		writeAPIError(w, http.StatusBadGateway, "activity_asset_failed",
			"could not fetch that artwork")
		return
	}

	s.activityAssets.put(cacheKey, asset)
	writeActivityAsset(w, asset)
}

func writeActivityAsset(w http.ResponseWriter, asset activityAsset) {
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("Content-Length", fmt.Sprint(len(asset.body)))
	// A game's artwork changes when the game ships an update. A day in the
	// browser's cache saves this server from being asked again by every window
	// somebody opens.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(asset.body)
}

// fetchActivityAsset turns an application and a key into a picture.
//
// The key is either the id of an asset, which the CDN serves directly, or the
// name a game gave one, which has to be looked up first. Games send both, and
// which one is not something the reporting client can tell.
func (s *Server) fetchActivityAsset(r *http.Request, app, key string) (activityAsset, error) {
	id := key
	if !activityAppPattern.MatchString(key) {
		resolved, err := s.resolveActivityAssetName(r, app, key)
		if err != nil {
			return activityAsset{}, err
		}
		id = resolved
	}

	url := fmt.Sprintf("https://cdn.discordapp.com/app-assets/%s/%s.png?size=%d",
		app, id, activityAssetEdge)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		return activityAsset{}, err
	}
	req.Header.Set("User-Agent", activityAssetUserAgent)

	resp, err := activityAssetClient.Do(req)
	if err != nil {
		return activityAsset{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return activityAsset{}, fmt.Errorf("cdn returned status %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	// Whatever comes back is served to every member's browser, so it has to be
	// a picture rather than merely have been asked for as one.
	if !strings.HasPrefix(contentType, "image/") {
		return activityAsset{}, fmt.Errorf("cdn returned %q", contentType)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, activityAssetMaxBytes+1))
	if err != nil {
		return activityAsset{}, err
	}
	if len(body) == 0 || len(body) > activityAssetMaxBytes {
		return activityAsset{}, fmt.Errorf("cdn returned %d bytes", len(body))
	}

	return activityAsset{
		body:        body,
		contentType: contentType,
		fetchedAt:   time.Now(),
	}, nil
}

// activityAssetUserAgent identifies this server. Discord rejects a request
// without one, which is a failure worth naming rather than discovering.
const activityAssetUserAgent = "Aural (https://github.com/aural-chat/aural-server, 1.0)"

// resolveActivityAssetName looks up which asset a name refers to.
//
// The list belongs to the application rather than to the asset, so it is
// cached per application: one request covers every picture a game will ever
// name, which for a game with a large-image-per-map is the difference between
// one round trip and dozens.
func (s *Server) resolveActivityAssetName(r *http.Request, app, key string) (string, error) {
	s.activityAssets.mu.Lock()
	held, ok := s.activityAssets.names[app]
	s.activityAssets.mu.Unlock()
	if ok && time.Since(held.fetchedAt) <= activityAssetTTL {
		if id, found := held.ids[strings.ToLower(key)]; found {
			return id, nil
		}
		// A fresh list that does not name it. Asking again would get the same
		// answer, so this is a miss rather than a reason to fetch.
		return "", fmt.Errorf("no asset named %q", key)
	}

	url := fmt.Sprintf("https://discord.com/api/v10/oauth2/applications/%s/assets", app)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", activityAssetUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := activityAssetClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("assets returned status %d", resp.StatusCode)
	}

	var listed []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	body := io.LimitReader(resp.Body, activityAssetMaxBytes)
	if err := json.NewDecoder(body).Decode(&listed); err != nil {
		return "", err
	}

	ids := make(map[string]string, len(listed))
	for _, entry := range listed {
		if activityAppPattern.MatchString(entry.ID) {
			ids[strings.ToLower(entry.Name)] = entry.ID
		}
	}

	s.activityAssets.mu.Lock()
	s.activityAssets.names[app] = activityAssetNames{ids: ids, fetchedAt: time.Now()}
	s.activityAssets.mu.Unlock()

	id, found := ids[strings.ToLower(key)]
	if !found {
		return "", fmt.Errorf("no asset named %q", key)
	}
	return id, nil
}
