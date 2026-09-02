package gateway

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/aural-chat/aural-server/internal/ddns"
	"github.com/aural-chat/aural-server/internal/publicip"
)

// ddnsRetry is how soon a failed update is tried again, whatever the
// configured interval is. A provider that refused once — a rate limit, an API
// having a bad minute, a network that is not up yet — is usually fine shortly
// afterwards, and waiting out a five minute interval to find that out leaves
// the record wrong for no reason.
const ddnsRetry = time.Minute

// ddnsTimeout bounds one round: discovering the address and publishing it.
const ddnsTimeout = 45 * time.Second

// watchDDNS keeps the configured dynamic DNS record pointing at this server
// until ctx ends.
//
// The address is discovered by STUN rather than read from an interface,
// because the interface of a machine behind a router holds a private address
// and the record has to carry the public one. It is also not read from
// voice.public_ip: on a server configured the way this feature expects, that
// field names the very record being updated here, and resolving it to decide
// what to publish to it would be circular.
func (s *Server) watchDDNS(ctx context.Context) {
	cfg := s.cfg.DDNS
	if !cfg.Enabled {
		return
	}

	updater, err := ddns.New(ddns.Settings{
		Provider: cfg.Provider,
		Domain:   cfg.Domain,
		Token:    cfg.Token,
		ZoneID:   cfg.ZoneID,
		Proxied:  cfg.Proxied,
	})
	if err != nil {
		// A misconfigured updater is not a reason to refuse to serve. The
		// record simply stays wherever it was pointing.
		s.log.Error("dynamic DNS is configured but could not be started", slog.Any("error", err))
		return
	}

	// Its own resolver, with no configured address: this one has to find out
	// what the world sees, which is exactly the question STUN answers.
	resolver := publicip.New("", cfg.STUNServers)
	interval := time.Duration(cfg.IntervalMinutes) * time.Minute

	s.log.Info("keeping a dynamic DNS record current",
		slog.String("record", updater.Describe()),
		slog.Duration("interval", interval))

	// The first round runs immediately: a server that has just started after
	// an address change is the case this exists for.
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		next := interval
		if err := s.updateDDNS(ctx, updater, resolver); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			s.log.Warn("could not update the dynamic DNS record",
				slog.String("record", updater.Describe()), slog.Any("error", err))
			next = ddnsRetry
		}
		timer.Reset(next)
	}
}

// updateDDNS runs one round: ask where we are, and publish it if it moved.
func (s *Server) updateDDNS(ctx context.Context, updater *ddns.Updater, resolver *publicip.Resolver) error {
	ctx, cancel := context.WithTimeout(ctx, ddnsTimeout)
	defer cancel()

	addr, err := resolver.Resolve(ctx)
	if err != nil {
		return err
	}

	changed, err := updater.Update(ctx, addr)
	if err != nil {
		return err
	}
	if changed {
		s.log.Info("published a new address",
			slog.String("record", updater.Describe()), slog.String("address", addr.String()))
	}
	return nil
}
