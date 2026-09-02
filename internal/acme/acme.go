// Package acme obtains and renews a TLS certificate over the DNS-01 challenge.
//
// The challenge matters more than the protocol here. HTTP-01, which is what
// most automation reaches for, needs port 80 reachable from the internet, and
// a residential connection frequently does not offer one: providers block it,
// routers do not forward it, and the operator of a home server has no way to
// change either. DNS-01 needs nothing inbound at all. It asks the certificate
// authority to look up a TXT record, which this server publishes through the
// same DNS provider it already uses to keep its address current.
//
// The certificate is written to disk as an ordinary PEM pair, which is what
// lets the rest of the server stay unaware of any of this: the gateway serves
// whatever those two files hold and re-reads them when they change, exactly as
// it does for a certificate an operator obtained by hand.
package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/acme"
)

// LetsEncryptStaging is the directory to point at while working out whether a
// deployment is right. Its rate limits are generous and its certificates are
// not trusted by anything, which is the correct combination for a test.
const LetsEncryptStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"

const (
	// renewBefore is how long before expiry a certificate is replaced. Let's
	// Encrypt issues for ninety days and recommends renewing at sixty, which
	// leaves thirty days of failed attempts before anything breaks.
	renewBefore = 30 * 24 * time.Hour
	// propagationWait is how long to let a published TXT record spread before
	// telling the certificate authority to look for it. The record is written
	// with a short TTL, but a provider's own nameservers still need a moment
	// to agree with each other, and a challenge checked too early fails the
	// whole order.
	propagationWait = 30 * time.Second
	// orderTimeout bounds one attempt at obtaining a certificate.
	orderTimeout = 5 * time.Minute
)

// DNSProvider publishes the TXT record that answers a challenge. It is
// satisfied by the dynamic DNS updater, which already holds the credentials
// for the operator's DNS.
type DNSProvider interface {
	// SetChallenge publishes the record and returns a function removing it.
	SetChallenge(ctx context.Context, domain, value string) (remove func(context.Context) error, err error)
}

// Settings is what a manager needs to run.
type Settings struct {
	// Domains are the names the certificate covers. The first is the common
	// name; the rest are subject alternative names.
	Domains []string
	// Email is the contact the certificate authority writes to about expiry.
	// It is optional and worth setting: it is the only warning anybody gets
	// when renewal has been failing.
	Email string
	// DirectoryURL is the ACME directory. Empty means Let's Encrypt
	// production.
	DirectoryURL string
	// CertFile and KeyFile are where the PEM pair is written. They are what
	// the listener serves.
	CertFile string
	KeyFile  string
	// AccountKeyFile holds the account key, which identifies this server to
	// the certificate authority across renewals. Losing it is not fatal — a
	// new account is registered — but keeping it avoids doing that.
	AccountKeyFile string
}

// Manager obtains a certificate and keeps it current.
type Manager struct {
	settings Settings
	dns      DNSProvider
	log      *slog.Logger
}

// New builds a manager, rejecting settings it could not act on.
func New(settings Settings, dns DNSProvider, log *slog.Logger) (*Manager, error) {
	if len(settings.Domains) == 0 {
		return nil, errors.New("acme: no domains to get a certificate for")
	}
	if settings.CertFile == "" || settings.KeyFile == "" {
		return nil, errors.New("acme: nowhere to write the certificate")
	}
	if dns == nil {
		return nil, errors.New("acme: no DNS provider to answer the challenge with")
	}
	return &Manager{settings: settings, dns: dns, log: log}, nil
}

// EnsureCertificate obtains a certificate if the one on disk is missing or
// close to expiring, and does nothing otherwise.
//
// It returns the moment the held certificate should next be looked at, so the
// caller can sleep until then rather than poll.
func (m *Manager) EnsureCertificate(ctx context.Context) (next time.Time, err error) {
	expiry, err := m.heldExpiry()
	switch {
	case err != nil:
		m.log.Info("obtaining a certificate", slog.Any("domains", m.settings.Domains),
			slog.String("reason", err.Error()))
	case time.Until(expiry) > renewBefore:
		// Nothing to do. Look again a day before renewal is due, so a clock
		// that drifts or a process that runs for months still lands inside the
		// window.
		return expiry.Add(-renewBefore - 24*time.Hour), nil
	default:
		m.log.Info("renewing the certificate", slog.Any("domains", m.settings.Domains),
			slog.Time("expires", expiry))
	}

	ctx, cancel := context.WithTimeout(ctx, orderTimeout)
	defer cancel()

	if err := m.obtain(ctx); err != nil {
		return time.Time{}, err
	}

	expiry, err = m.heldExpiry()
	if err != nil {
		return time.Time{}, err
	}
	m.log.Info("obtained a certificate",
		slog.Any("domains", m.settings.Domains), slog.Time("expires", expiry))
	return expiry.Add(-renewBefore - 24*time.Hour), nil
}

// heldExpiry is when the certificate on disk runs out. An error means there is
// nothing usable there, and says why in terms fit for a log line.
func (m *Manager) heldExpiry() (time.Time, error) {
	raw, err := os.ReadFile(m.settings.CertFile)
	if err != nil {
		return time.Time{}, errors.New("no certificate has been obtained yet")
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return time.Time{}, errors.New("the certificate file is not readable as PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, errors.New("the certificate file does not hold a certificate")
	}

	// A certificate that no longer covers what it is being asked to is as good
	// as absent, which is what happens when a domain is added to the
	// configuration.
	for _, domain := range m.settings.Domains {
		if err := cert.VerifyHostname(domain); err != nil {
			return time.Time{}, fmt.Errorf("the held certificate does not cover %s", domain)
		}
	}
	return cert.NotAfter, nil
}

// obtain runs one whole ACME order: register if needed, answer a challenge for
// each domain, and write out the certificate that comes back.
func (m *Manager) obtain(ctx context.Context) error {
	accountKey, err := m.accountKey()
	if err != nil {
		return err
	}

	client := &acme.Client{Key: accountKey, DirectoryURL: m.settings.DirectoryURL}

	var contact []string
	if m.settings.Email != "" {
		contact = []string{"mailto:" + m.settings.Email}
	}
	if _, err := client.Register(ctx, &acme.Account{Contact: contact}, acme.AcceptTOS); err != nil {
		// An account this key already registered is the ordinary case on every
		// run after the first.
		if !errors.Is(err, acme.ErrAccountAlreadyExists) {
			return fmt.Errorf("acme: register: %w", err)
		}
	}

	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(m.settings.Domains...))
	if err != nil {
		return fmt.Errorf("acme: authorize: %w", err)
	}

	for _, url := range order.AuthzURLs {
		if err := m.authorize(ctx, client, url); err != nil {
			return err
		}
	}

	order, err = client.WaitOrder(ctx, order.URI)
	if err != nil {
		return fmt.Errorf("acme: wait for the order: %w", err)
	}

	// A key of its own for the certificate, generated fresh each time: it is
	// unrelated to the account key, and rotating it on renewal costs nothing.
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: m.settings.Domains[0]},
		DNSNames: m.settings.Domains,
	}, certKey)
	if err != nil {
		return fmt.Errorf("acme: build the request: %w", err)
	}

	chain, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return fmt.Errorf("acme: finalize: %w", err)
	}
	return m.write(chain, certKey)
}

// authorize answers the DNS-01 challenge of one authorization.
func (m *Manager) authorize(ctx context.Context, client *acme.Client, url string) error {
	authz, err := client.GetAuthorization(ctx, url)
	if err != nil {
		return fmt.Errorf("acme: get authorization: %w", err)
	}
	if authz.Status == acme.StatusValid {
		// Already answered, and still within the window the certificate
		// authority caches it for. This is why a renewal is often quick.
		return nil
	}

	domain := authz.Identifier.Value

	var challenge *acme.Challenge
	for _, candidate := range authz.Challenges {
		if candidate.Type == "dns-01" {
			challenge = candidate
			break
		}
	}
	if challenge == nil {
		return fmt.Errorf("acme: %s offers no dns-01 challenge", domain)
	}

	value, err := client.DNS01ChallengeRecord(challenge.Token)
	if err != nil {
		return fmt.Errorf("acme: build the challenge record: %w", err)
	}

	m.log.Info("answering a DNS challenge",
		slog.String("domain", domain), slog.String("record", ddnsChallengeName(domain)))

	remove, err := m.dns.SetChallenge(ctx, domain, value)
	if err != nil {
		return fmt.Errorf("acme: publish the challenge for %s: %w", domain, err)
	}
	defer func() {
		// Always clean up, including on the paths that failed: a stale
		// challenge record is what makes the next attempt fail too. It gets a
		// context of its own, because the one above may be the reason we are
		// here.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestGrace)
		defer cancel()
		if err := remove(cleanup); err != nil {
			m.log.Warn("could not remove the challenge record",
				slog.String("domain", domain), slog.Any("error", err))
		}
	}()

	// The record has to be visible to the certificate authority before it is
	// asked to look, and there is no way to be told when that is.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(propagationWait):
	}

	if _, err := client.Accept(ctx, challenge); err != nil {
		return fmt.Errorf("acme: accept the challenge for %s: %w", domain, err)
	}
	if _, err := client.WaitAuthorization(ctx, authz.URI); err != nil {
		return fmt.Errorf("acme: %s was not authorized: %w", domain, err)
	}
	return nil
}

// requestGrace is how long the cleanup of a challenge record gets, after the
// context that was running the order has already been given up on.
const requestGrace = 30 * time.Second

// ddnsChallengeName mirrors the name the DNS provider writes to, for the log
// line that says where to look when a challenge fails.
func ddnsChallengeName(domain string) string { return "_acme-challenge." + domain }

// accountKey loads the account key, generating and storing one on first run.
func (m *Manager) accountKey() (crypto.Signer, error) {
	if m.settings.AccountKeyFile != "" {
		if raw, err := os.ReadFile(m.settings.AccountKeyFile); err == nil {
			if block, _ := pem.Decode(raw); block != nil {
				if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
					return key, nil
				}
			}
			m.log.Warn("the ACME account key is unreadable; registering a new account",
				slog.String("path", m.settings.AccountKeyFile))
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	if m.settings.AccountKeyFile == "" {
		return key, nil
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := writePEM(m.settings.AccountKeyFile, "EC PRIVATE KEY", [][]byte{der}, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// write stores the certificate chain and its key.
//
// The key goes first. The gateway reloads on the modification time of either
// file, so writing the certificate last is what makes the pair it picks up a
// matched one rather than a new certificate beside an old key.
func (m *Manager) write(chain [][]byte, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := writePEM(m.settings.KeyFile, "EC PRIVATE KEY", [][]byte{der}, 0o600); err != nil {
		return err
	}
	return writePEM(m.settings.CertFile, "CERTIFICATE", chain, 0o644)
}

// writePEM encodes blocks and replaces path with them, atomically: a renewal
// interrupted halfway must not leave a truncated certificate where a valid one
// used to be.
func writePEM(path, blockType string, blocks [][]byte, perm os.FileMode) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())

	for _, block := range blocks {
		if err := pem.Encode(temp, &pem.Block{Type: blockType, Bytes: block}); err != nil {
			temp.Close()
			return err
		}
	}
	if err := temp.Chmod(perm); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	// Windows will not rename onto an existing file, so the old one goes
	// first. The window this opens is microseconds wide, and the gateway holds
	// the certificate it is serving in memory throughout.
	_ = os.Remove(path)
	return os.Rename(temp.Name(), path)
}
