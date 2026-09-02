// Package publicip works out the address a server-hosted voice relay should
// advertise to the outside world.
//
// The relay substitutes one address into its ICE host candidates, and getting
// it wrong is invisible until somebody tries to speak: signalling travels over
// the WebSocket and works regardless, while the media never finds a path. On a
// machine with a fixed address this is a one-line setting nobody thinks about
// again. On a home connection it is not, because the address changes whenever
// the provider decides it does, and a value written into a file months ago is
// then quietly wrong.
//
// So the address is resolved rather than configured, from whichever of three
// sources the operator has given: a literal IP, a hostname to look up — the
// one a dynamic DNS record points at — or, with neither, a STUN server to ask
// what the internet sees.
package publicip

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/pion/stun/v3"
)

// defaultSTUNPort is the port a stun: URL means when it names none.
const defaultSTUNPort = "3478"

// stunTimeout bounds one binding request. A STUN server that has not answered
// in this long is one to give up on rather than wait for; there is usually
// another in the list.
const stunTimeout = 5 * time.Second

// ErrNoSource means nothing was configured to resolve from, which is the
// ordinary state of a server whose own interface holds the address clients
// reach it on. It is not a failure.
var ErrNoSource = errors.New("publicip: no public address configured")

// Resolver answers what the relay should advertise. It holds no state: the
// answer is looked up each time, because the whole point is that it changes.
type Resolver struct {
	// configured is voice.public_ip exactly as the operator wrote it: an IP
	// literal, a hostname, or empty.
	configured string
	// stun is the list of STUN servers to ask when configured is empty. They
	// are taken from voice.ice_servers, which a server hosting its own voice
	// behind NAT will already have.
	stun []string
}

// New builds a resolver. iceURLs is every URL from voice.ice_servers; the
// stun: ones are kept and the rest ignored, since a TURN server is not
// something to ask this question of.
func New(configured string, iceURLs []string) *Resolver {
	r := &Resolver{configured: strings.TrimSpace(configured)}
	for _, raw := range iceURLs {
		if addr, ok := stunAddress(raw); ok {
			r.stun = append(r.stun, addr)
		}
	}
	return r
}

// Static reports whether the answer can never change, which is true only of a
// literal address. A static resolver needs no watching.
func (r *Resolver) Static() bool {
	_, err := netip.ParseAddr(r.configured)
	return err == nil
}

// Describe says where the answer comes from, for the line the server logs at
// startup.
func (r *Resolver) Describe() string {
	switch {
	case r.Static():
		return "configured address"
	case r.configured != "":
		return "hostname " + r.configured
	case len(r.stun) > 0:
		return "STUN"
	default:
		return "none"
	}
}

// Resolve returns the address to advertise.
//
// A literal is returned as it stands. A hostname is looked up, which is what
// makes a dynamic DNS record — a DuckDNS subdomain, a Cloudflare A record —
// usable here: the record is already being kept current by something, and this
// reads it. With neither, a STUN server is asked, which needs no configuration
// at all beyond the one most such servers already list.
func (r *Resolver) Resolve(ctx context.Context) (netip.Addr, error) {
	if addr, err := netip.ParseAddr(r.configured); err == nil {
		return addr.Unmap(), nil
	}
	if r.configured != "" {
		return r.lookup(ctx, r.configured)
	}
	if len(r.stun) > 0 {
		return r.askSTUN(ctx)
	}
	return netip.Addr{}, ErrNoSource
}

// lookup resolves a hostname, preferring IPv4.
//
// The preference is not arbitrary: this address is substituted into host
// candidates for a relay whose media is carried over UDP, and a home
// deployment reached through port forwarding is reached over v4. A record that
// resolves to both is a record whose v4 half is the one that was forwarded.
func (r *Resolver) lookup(ctx context.Context, host string) (netip.Addr, error) {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("publicip: look up %s: %w", host, err)
	}

	var fallback netip.Addr
	for _, addr := range addrs {
		addr = addr.Unmap()
		if !usable(addr) {
			continue
		}
		if addr.Is4() {
			return addr, nil
		}
		if !fallback.IsValid() {
			fallback = addr
		}
	}
	if fallback.IsValid() {
		return fallback, nil
	}
	return netip.Addr{}, fmt.Errorf("publicip: %s resolves to no usable address", host)
}

// askSTUN asks each configured STUN server in turn for the address it sees,
// and takes the first answer. They are asked in order rather than in parallel
// because the first one nearly always answers, and a server that is minutes
// into its own startup has better things to be doing.
func (r *Resolver) askSTUN(ctx context.Context) (netip.Addr, error) {
	var last error
	for _, server := range r.stun {
		addr, err := querySTUN(ctx, server)
		if err == nil {
			return addr, nil
		}
		last = err
	}
	return netip.Addr{}, fmt.Errorf("publicip: no STUN server answered: %w", last)
}

// querySTUN performs one binding request and reads the mapped address out of
// the reply, which is the address the far end saw the request arrive from.
func querySTUN(ctx context.Context, server string) (netip.Addr, error) {
	deadline := time.Now().Add(stunTimeout)
	if fromCtx, ok := ctx.Deadline(); ok && fromCtx.Before(deadline) {
		deadline = fromCtx
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp4", server)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("publicip: dial %s: %w", server, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return netip.Addr{}, err
	}

	client, err := stun.NewClient(conn)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("publicip: stun client: %w", err)
	}
	defer client.Close()

	var (
		mapped stun.XORMappedAddress
		inner  error
	)
	err = client.Do(stun.MustBuild(stun.TransactionID, stun.BindingRequest), func(event stun.Event) {
		if event.Error != nil {
			inner = event.Error
			return
		}
		inner = mapped.GetFrom(event.Message)
	})
	if err != nil {
		return netip.Addr{}, fmt.Errorf("publicip: ask %s: %w", server, err)
	}
	if inner != nil {
		return netip.Addr{}, fmt.Errorf("publicip: ask %s: %w", server, inner)
	}

	addr, ok := netip.AddrFromSlice(mapped.IP)
	if !ok {
		return netip.Addr{}, fmt.Errorf("publicip: %s returned an unreadable address", server)
	}
	addr = addr.Unmap()
	if !usable(addr) {
		return netip.Addr{}, fmt.Errorf("publicip: %s reports %s, which is not routable", server, addr)
	}
	return addr, nil
}

// usable rejects the addresses that would be worse than advertising nothing:
// a loopback or link-local candidate reaches nobody, and an unspecified one is
// not an address at all.
//
// Private ranges are deliberately allowed. A server on a LAN whose clients are
// also on it has a private address that is exactly right for them, and it is
// not this function's place to decide that a deployment is wrong.
func usable(addr netip.Addr) bool {
	return addr.IsValid() &&
		!addr.IsUnspecified() &&
		!addr.IsLoopback() &&
		!addr.IsLinkLocalUnicast() &&
		!addr.IsMulticast()
}

// stunAddress turns an ICE server URL into a host:port to dial, reporting
// whether it was a STUN URL at all.
func stunAddress(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	scheme, rest, ok := strings.Cut(raw, ":")
	if !ok || !strings.EqualFold(scheme, "stun") {
		// stuns: is TLS over TCP, which this does not speak, and turn: is not
		// a question to ask a relay credential for.
		return "", false
	}
	// A query string carries the transport, which is not part of the address.
	if q := strings.IndexByte(rest, '?'); q >= 0 {
		rest = rest[:q]
	}
	if rest == "" {
		return "", false
	}
	if _, _, err := net.SplitHostPort(rest); err != nil {
		rest = net.JoinHostPort(rest, defaultSTUNPort)
	}
	return rest, true
}
