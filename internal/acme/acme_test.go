package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// stubDNS stands in for a provider. Nothing here reaches the network.
type stubDNS struct{}

func (stubDNS) SetChallenge(context.Context, string, string) (func(context.Context) error, error) {
	return func(context.Context) error { return nil }, nil
}

// writeSelfSigned puts a certificate on disk with the given lifetime and
// names, so the expiry logic can be exercised without an ACME server.
func writeSelfSigned(t *testing.T, path string, notAfter time.Time, domains ...string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domains[0]},
		DNSNames:     domains,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}
	if err := writePEM(path, "CERTIFICATE", [][]byte{der}, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func newTestManager(t *testing.T, dir string, domains ...string) *Manager {
	t.Helper()

	m, err := New(Settings{
		Domains:  domains,
		CertFile: filepath.Join(dir, "cert.pem"),
		KeyFile:  filepath.Join(dir, "key.pem"),
	}, stubDNS{}, quietLog())
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return m
}

// A certificate with months left must not be replaced. Getting this wrong
// means ordering a new certificate on every restart, which is how a server
// meets a rate limit and ends up with no certificate at all.
func TestEnsureLeavesAHealthyCertificateAlone(t *testing.T) {
	dir := t.TempDir()
	m := newTestManager(t, dir, "aural.example.com")
	expiry := time.Now().Add(80 * 24 * time.Hour)
	writeSelfSigned(t, m.settings.CertFile, expiry, "aural.example.com")

	next, err := m.EnsureCertificate(context.Background())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if next.After(expiry) {
		t.Error("the next check was scheduled after the certificate expires")
	}
	if time.Until(next) < 24*time.Hour {
		t.Error("the next check is almost immediate for a certificate with months left")
	}
}

// A certificate inside the renewal window, one that has expired, and one that
// does not cover the configured names all have to be replaced. Each is
// recognised by heldExpiry refusing it, so that is what is pinned here: the
// order itself needs a certificate authority.
func TestHeldExpiryRefusesWhatMustBeReplaced(t *testing.T) {
	t.Run("a certificate that does not cover the domain", func(t *testing.T) {
		dir := t.TempDir()
		m := newTestManager(t, dir, "aural.example.com", "voice.example.com")
		// Covers only the first of the two names it is being asked for, which
		// is what adding a domain to the configuration looks like.
		writeSelfSigned(t, m.settings.CertFile, time.Now().Add(80*24*time.Hour), "aural.example.com")

		_, err := m.heldExpiry()
		if err == nil {
			t.Fatal("a certificate missing one of its names was accepted")
		}
		if !strings.Contains(err.Error(), "voice.example.com") {
			t.Errorf("the reason does not name the missing domain: %v", err)
		}
	})

	t.Run("no certificate at all", func(t *testing.T) {
		m := newTestManager(t, t.TempDir(), "aural.example.com")
		if _, err := m.heldExpiry(); err == nil {
			t.Error("a missing certificate was reported as fine")
		}
	})

	t.Run("a file that is not a certificate", func(t *testing.T) {
		dir := t.TempDir()
		m := newTestManager(t, dir, "aural.example.com")
		if err := os.WriteFile(m.settings.CertFile, []byte("not pem"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := m.heldExpiry(); err == nil {
			t.Error("a rubbish file was accepted as a certificate")
		}
	})

	t.Run("a certificate inside the renewal window", func(t *testing.T) {
		dir := t.TempDir()
		m := newTestManager(t, dir, "aural.example.com")
		expiry := time.Now().Add(10 * 24 * time.Hour)
		writeSelfSigned(t, m.settings.CertFile, expiry, "aural.example.com")

		held, err := m.heldExpiry()
		if err != nil {
			t.Fatalf("heldExpiry: %v", err)
		}
		if time.Until(held) > renewBefore {
			t.Fatal("the test certificate is not actually inside the renewal window")
		}
	})
}

// A renewal interrupted halfway must not leave a truncated certificate where a
// working one used to be, and it must be able to overwrite one on Windows,
// where renaming onto an existing file fails.
func TestWritePEMReplacesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cert.pem")

	if err := writePEM(path, "CERTIFICATE", [][]byte{[]byte("first")}, 0o644); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writePEM(path, "CERTIFICATE", [][]byte{[]byte("second")}, 0o644); err != nil {
		t.Fatalf("second write: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "BEGIN CERTIFICATE") {
		t.Error("the file does not hold PEM")
	}
	if strings.Count(string(raw), "BEGIN CERTIFICATE") != 1 {
		t.Error("the second write appended rather than replaced")
	}

	// The directory must not be left holding the temporary files.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("%d files left behind, want just the certificate", len(entries))
	}
}

func TestWritePEMCreatesTheDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "cert.pem")
	if err := writePEM(path, "CERTIFICATE", [][]byte{[]byte("x")}, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the file was not created: %v", err)
	}
}

func TestNewRejectsSettingsItCouldNotActOn(t *testing.T) {
	cases := []struct {
		name     string
		settings Settings
		dns      DNSProvider
	}{
		{"no domains", Settings{CertFile: "c", KeyFile: "k"}, stubDNS{}},
		{"nowhere to write", Settings{Domains: []string{"a.example.com"}}, stubDNS{}},
		{"no DNS provider", Settings{Domains: []string{"a.example.com"}, CertFile: "c", KeyFile: "k"}, nil},
	}
	for _, tc := range cases {
		if _, err := New(tc.settings, tc.dns, quietLog()); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}
}
