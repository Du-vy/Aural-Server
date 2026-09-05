package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aural-chat/aural-server/internal/protocol"
)

const (
	// maxUnfurlBodyBytes is the max HTML read to find OpenGraph meta tags.
	maxUnfurlBodyBytes = 512 * 1024
	// unfurlBurst and unfurlsPerSecond throttle one member. Unfurling is an
	// outbound fetch made in this server's name, so the rate is what decides
	// how much of somebody else's bandwidth one member can spend.
	unfurlBurst      = 10
	unfurlsPerSecond = 1
)

// UnfurlResult is the OpenGraph / meta data returned to the client.
type UnfurlResult struct {
	URL         string `json:"url"`
	SiteName    string `json:"siteName,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Image       string `json:"image,omitempty"`
	Video       string `json:"video,omitempty"`
	VideoType   string `json:"videoType,omitempty"`
	Favicon     string `json:"favicon,omitempty"`
	Color       string `json:"color,omitempty"`
	Author      string `json:"author,omitempty"`
}

var (
	metaTagRegex  = regexp.MustCompile(`(?i)<meta\s+([^>]*?)>`)
	linkTagRegex  = regexp.MustCompile(`(?i)<link\s+([^>]*?)>`)
	titleTagRegex = regexp.MustCompile(`(?i)<title[^>]*>(.*?)</title>`)
	attrRegex     = regexp.MustCompile(`(?i)(name|property|content|rel|href)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
)

// isPrivateIP blocks SSRF attacks by refusing loopback and private subnets.
func isPrivateIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	// Check IPv4 private ranges (RFC 1918) and IPv6 unique local (fc00::/7)
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 10:
			return true
		case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
			return true
		case ip4[0] == 192 && ip4[1] == 168:
			return true
		case ip4[0] == 169 && ip4[1] == 254:
			return true
		case ip4[0] == 127:
			return true
		case ip4[0] == 0:
			return true
		case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127:
			// Carrier-grade NAT: not private, but not the public internet
			// either, and reachable from a great many hosted servers.
			return true
		}
	} else if len(ip) == 16 {
		if ip[0]&0xfe == 0xfc { // fc00::/7
			return true
		}
		// NAT64 (64:ff9b::/96) carries an IPv4 address in its low 32 bits, so
		// it is a way to reach a blocked v4 range through a v6 address.
		if ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b {
			return isPrivateIP(net.IPv4(ip[12], ip[13], ip[14], ip[15]))
		}
	}
	return false
}

// safeDialContext creates a Dialer that resolves DNS and validates target IPs.
func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no IP found for %s", host)
	}

	for _, ip := range ips {
		if isPrivateIP(ip) {
			return nil, fmt.Errorf("refusing connection to private or loopback IP: %s", ip)
		}
	}

	dialer := &net.Dialer{
		Timeout:   3 * time.Second,
		KeepAlive: 10 * time.Second,
	}
	return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}

// unfurlClient is shared across requests, like the Klipy proxy's.
//
// Building one per request meant the pool settings below never applied: every
// unfurl dialled and shook hands afresh, and the transport it was left with
// held its idle connection — and the goroutines reading it — for a further
// IdleConnTimeout with nothing able to reach it again.
//
// Its dialler is what keeps this endpoint from being a way to reach the inside
// of the operator's network, so it is the one field that must not be shared
// with any other client here.
var unfurlClient = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		DialContext:           safeDialContext,
		ResponseHeaderTimeout: 4 * time.Second,
		MaxIdleConns:          20,
		IdleConnTimeout:       30 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	},
}

// handleUnfurl fetches a webpage and extracts OpenGraph metadata without CORS restrictions.
//
//	GET /unfurl?url=<target_url>
func (s *Server) handleUnfurl(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)

	if !s.cfg.Unfurl.Enabled {
		writeAPIError(w, http.StatusForbidden, "unfurl_disabled", "link unfurling is disabled on this server")
		return
	}

	// Fetching a URL somebody names, from this server's address, is a
	// capability that belongs to members. Left open it is an anonymous fetcher
	// pointed at the public internet from an address that is not the caller's.
	user, failure := s.authenticateRequest(r)
	if failure != nil {
		writeProtocolError(w, failure)
		return
	}

	target := strings.TrimSpace(r.URL.Query().Get("url"))
	if target == "" {
		writeAPIError(w, http.StatusBadRequest, protocol.ErrBadRequest, "the url parameter is required")
		return
	}

	parsedTarget, err := url.Parse(target)
	if err != nil || (parsedTarget.Scheme != "http" && parsedTarget.Scheme != "https") || parsedTarget.Host == "" {
		writeAPIError(w, http.StatusBadRequest, protocol.ErrBadRequest, "invalid url: only http and https are allowed")
		return
	}

	// Calculate cache expiration based on configured TTL days.
	ttlDays := s.cfg.Unfurl.CacheTTLDays
	if ttlDays <= 0 {
		ttlDays = 7
	}
	validAfter := time.Now().Add(-time.Duration(ttlDays) * 24 * time.Hour).Unix()
	hash := sha256.Sum256([]byte(target))
	urlHash := hex.EncodeToString(hash[:])

	// 1. Check SQLite cache
	if cachedJSON, err := s.st.GetLinkPreview(r.Context(), urlHash, validAfter); err == nil && cachedJSON != "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write([]byte(cachedJSON))
		return
	}

	if !s.unfurls.allow(user.ID) {
		writeAPIError(w, http.StatusTooManyRequests, protocol.ErrRateLimited,
			"you are unfurling links too quickly")
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, protocol.ErrBadRequest, "failed to build request")
		return
	}

	// Use Discordbot User-Agent because social platforms and web servers optimize OpenGraph for it.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Discordbot/2.0; +https://aural.chat)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,es;q=0.8")

	resp, err := unfurlClient.Do(req)
	if err != nil {
		s.log.Debug("unfurl request failed", slog.String("url", target), slog.Any("error", err))
		writeAPIError(w, http.StatusBadGateway, "unfurl_failed", "could not fetch target url")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		writeAPIError(w, http.StatusBadGateway, "unfurl_failed", fmt.Sprintf("target returned status %d", resp.StatusCode))
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUnfurlBodyBytes))
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "unfurl_failed", "failed to read target response")
		return
	}

	meta := extractHTMLMetadata(string(body), resp.Request.URL)
	if meta.Title == "" && meta.Description == "" && meta.Image == "" && meta.Video == "" {
		writeAPIError(w, http.StatusNotFound, protocol.ErrNotFound, "no opengraph metadata found")
		return
	}

	encoded, err := json.Marshal(meta)
	if err == nil {
		_ = s.st.SaveLinkPreview(r.Context(), urlHash, target, string(encoded), time.Now().Unix())
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if len(encoded) > 0 {
		_, _ = w.Write(encoded)
	} else {
		_ = json.NewEncoder(w).Encode(meta)
	}
}

func parseAttributes(raw string) map[string]string {
	attrs := make(map[string]string)
	matches := attrRegex.FindAllStringSubmatch(raw, -1)
	for _, m := range matches {
		key := strings.ToLower(m[1])
		val := m[2]
		if val == "" {
			val = m[3]
		}
		if val == "" {
			val = m[4]
		}
		attrs[key] = strings.TrimSpace(html.UnescapeString(val))
	}
	return attrs
}

func resolveURL(base *url.URL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return base.ResolveReference(parsed).String()
}

func extractHTMLMetadata(doc string, baseURL *url.URL) UnfurlResult {
	res := UnfurlResult{
		URL: baseURL.String(),
	}

	metas := metaTagRegex.FindAllStringSubmatch(doc, -1)
	for _, m := range metas {
		if len(m) < 2 {
			continue
		}
		attrs := parseAttributes(m[1])
		prop := attrs["property"]
		if prop == "" {
			prop = attrs["name"]
		}
		prop = strings.ToLower(prop)
		content := attrs["content"]
		if content == "" {
			continue
		}

		switch prop {
		case "og:title", "twitter:title":
			if res.Title == "" || prop == "og:title" {
				res.Title = content
			}
		case "og:description", "twitter:description", "description":
			if res.Description == "" || prop == "og:description" {
				res.Description = content
			}
		case "og:image", "og:image:url", "og:image:secure_url", "twitter:image", "twitter:image:src":
			if res.Image == "" || strings.HasPrefix(prop, "og:image") {
				res.Image = resolveURL(baseURL, content)
			}
		case "og:video", "og:video:url", "og:video:secure_url", "twitter:player:stream":
			if res.Video == "" || strings.HasPrefix(prop, "og:video") {
				res.Video = resolveURL(baseURL, content)
			}
		case "og:video:type":
			res.VideoType = content
		case "og:site_name", "application-name":
			if res.SiteName == "" {
				res.SiteName = content
			}
		case "theme-color":
			if res.Color == "" {
				res.Color = content
			}
		case "author", "article:author", "og:article:author", "twitter:creator":
			if res.Author == "" {
				res.Author = content
			}
		}
	}

	if res.Title == "" {
		if tMatch := titleTagRegex.FindStringSubmatch(doc); len(tMatch) > 1 {
			res.Title = strings.TrimSpace(html.UnescapeString(tMatch[1]))
		}
	}

	links := linkTagRegex.FindAllStringSubmatch(doc, -1)
	for _, l := range links {
		if len(l) < 2 {
			continue
		}
		attrs := parseAttributes(l[1])
		rel := strings.ToLower(attrs["rel"])
		href := attrs["href"]
		if href == "" {
			continue
		}

		if strings.Contains(rel, "icon") || strings.Contains(rel, "apple-touch-icon") {
			if res.Favicon == "" || strings.Contains(rel, "apple-touch-icon") {
				res.Favicon = resolveURL(baseURL, href)
			}
		}
	}

	return res
}
