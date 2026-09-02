package gateway

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/aural-chat/aural-server/internal/acme"
	"github.com/aural-chat/aural-server/internal/ddns"
)

// acmeRetry is how soon a failed renewal is tried again. Renewal starts thirty
// days before expiry, so there is room for a great many of these before
// anything stops working — which is the point of starting that early.
const acmeRetry = time.Hour

// startACME obtains a certificate, blocking until there is one, and then keeps
// it renewed in the background.
//
// The first order is deliberately not backgrounded. A server told to serve
// wss:// cannot serve anything at all without a certificate, so failing here
// with the reason is far more use than starting a listener that refuses every
// handshake while the reason scrolls past in a log.
func (s *Server) startACME(ctx context.Context) error {
	manager, err := s.acmeManager()
	if err != nil {
		return err
	}

	next, err := manager.EnsureCertificate(ctx)
	if err != nil {
		return err
	}

	go s.renewCertificate(ctx, manager, next)
	return nil
}

// acmeManager builds the certificate manager over the operator's DNS
// credentials, which are the ddns block whether or not that block is publishing
// anything.
func (s *Server) acmeManager() (*acme.Manager, error) {
	provider, err := ddns.New(ddns.Settings{
		Provider: s.cfg.DDNS.Provider,
		Domain:   s.cfg.DDNS.Domain,
		Token:    s.cfg.DDNS.Token,
		ZoneID:   s.cfg.DDNS.ZoneID,
	})
	if err != nil {
		return nil, err
	}

	directory := s.cfg.TLS.ACME.DirectoryURL
	if directory == "" && s.cfg.TLS.ACME.Staging {
		directory = acme.LetsEncryptStaging
	}

	return acme.New(acme.Settings{
		Domains:        s.cfg.TLS.ACME.Domains,
		Email:          s.cfg.TLS.ACME.Email,
		DirectoryURL:   directory,
		CertFile:       s.cfg.TLS.CertFile,
		KeyFile:        s.cfg.TLS.KeyFile,
		AccountKeyFile: s.cfg.ACMEAccountKeyFile(),
	}, provider, s.log)
}

// renewCertificate sleeps until the certificate is due and replaces it, until
// ctx ends.
//
// Renewal is the half of this that matters. Getting a certificate once is
// something an operator will happily do by hand; getting one every sixty days,
// for years, without being reminded, is not, and it is the reason a home
// server that worked in February stops working in May.
func (s *Server) renewCertificate(ctx context.Context, manager *acme.Manager, next time.Time) {
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		wait := acmeRetry
		following, err := manager.EnsureCertificate(ctx)
		switch {
		case errors.Is(err, context.Canceled):
			return
		case err != nil:
			s.log.Error("could not renew the certificate", slog.Any("error", err))
		default:
			wait = time.Until(following)
		}

		// A due date already past, or one that arrives while a renewal keeps
		// failing, must not turn into a spin.
		if wait < acmeRetry {
			wait = acmeRetry
		}
		timer.Reset(wait)
	}
}
