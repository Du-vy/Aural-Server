package gateway

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// clientIP is the address a request came from, as far as this server can
// honestly tell.
//
// Behind a reverse proxy — which is how a self-hosted server gets a
// certificate, a real domain, or a port that a residential connection does not
// block — every request arrives from the proxy, and the address of the person
// who made it survives only in a header. That header is written by whoever
// spoke to the proxy, so believing it from an arbitrary peer means believing
// anybody about anything.
//
// So it is only read when the immediate peer is a proxy the operator named,
// and the walk stops at the first hop that is not one. Nobody who is not
// already trusted can move the answer.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	peer := remoteAddr(r)
	if len(trusted) == 0 || !peer.IsValid() || !trustedBy(peer, trusted) {
		return addrString(peer, r.RemoteAddr)
	}

	// X-Forwarded-For reads left to right as client, then each proxy that
	// added itself. Walking from the right skips the hops we trust and stops
	// at the first entry no trusted proxy vouched for, which is the furthest
	// point still worth believing.
	hops := forwardedFor(r)
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			// An unparseable entry means the chain cannot be trusted past this
			// point. The last hop we did believe is the answer.
			break
		}
		hop = hop.Unmap()
		if !trustedBy(hop, trusted) {
			return hop.String()
		}
		peer = hop
	}
	return addrString(peer, r.RemoteAddr)
}

// forwardedFor is the X-Forwarded-For chain, flattened. The header may appear
// more than once, and the entries then join in order.
func forwardedFor(r *http.Request) []string {
	var hops []string
	for _, header := range r.Header.Values("X-Forwarded-For") {
		for _, hop := range strings.Split(header, ",") {
			if hop = strings.TrimSpace(hop); hop != "" {
				hops = append(hops, hop)
			}
		}
	}
	if len(hops) > 0 {
		return hops
	}
	// Some proxies write only X-Real-IP, which carries the client and nothing
	// else.
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return []string{real}
	}
	return nil
}

// remoteAddr is the immediate peer, parsed. It is invalid when the request did
// not come over a network, which is what an in-process test looks like.
func remoteAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

func trustedBy(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, prefix := range trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// addrString renders an address, falling back to whatever the request carried
// so that a log line is never empty.
func addrString(addr netip.Addr, fallback string) string {
	if addr.IsValid() {
		return addr.String()
	}
	return fallback
}

// parseTrustedProxies turns the configured list into prefixes. A bare address
// becomes a prefix covering just itself, so an operator may write either.
func parseTrustedProxies(entries []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			out = append(out, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, err
		}
		addr = addr.Unmap()
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}
