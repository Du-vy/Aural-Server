package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The state added for reloading certificates, sweeping rate limiters and
// following a changing public address is all shared between goroutines that no
// other test runs at the same time. The race detector only reports what is
// actually executed concurrently, so these exist to execute it: without them,
// a clean `go test -race` would be saying nothing about any of it.

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// writeKeyPair puts a self-signed certificate and its key on disk.
func writeKeyPair(t *testing.T, certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "aural.test"},
		DNSNames:     []string{"aural.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	// The key first, as a renewal writes it, so a reader that catches the pair
	// mid-write sees the case the reloader has to survive.
	writeBlock(t, keyPath, "EC PRIVATE KEY", keyDER)
	writeBlock(t, certPath, "CERTIFICATE", der)
}

func writeBlock(t *testing.T, path, blockType string, der []byte) {
	t.Helper()

	raw := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// A certificate is served from every handshake at once while a renewal
// rewrites the files underneath. Both happen on a real server the moment a
// renewal lands during a busy minute.
func TestCertReloaderIsSafeUnderConcurrentHandshakes(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeKeyPair(t, certPath, keyPath)

	reloader, err := newCertReloader(certPath, keyPath, quietLogger())
	if err != nil {
		t.Fatalf("new reloader: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers: what the TLS stack does on every connection.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				cert, err := reloader.GetCertificate(&tls.ClientHelloInfo{ServerName: "aural.test"})
				if err != nil {
					t.Errorf("GetCertificate: %v", err)
					return
				}
				if cert == nil {
					t.Error("no certificate was served; a handshake would fail")
					return
				}
			}
		}()
	}

	// A writer standing in for renewal. The check interval means most of these
	// are not picked up, which is the point: the reader must not be disturbed
	// either way.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 20 {
			writeKeyPair(t, certPath, keyPath)
		}
	}()

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// A reloader whose files vanish, or are replaced with rubbish, must keep
// serving what it already holds: a failed reload is not a reason to fail every
// handshake as well.
func TestCertReloaderKeepsServingThroughABadWrite(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeKeyPair(t, certPath, keyPath)

	reloader, err := newCertReloader(certPath, keyPath, quietLogger())
	if err != nil {
		t.Fatalf("new reloader: %v", err)
	}
	held, err := reloader.GetCertificate(nil)
	if err != nil || held == nil {
		t.Fatalf("first read: %v", err)
	}

	// Halfway through a renewal: the certificate has been replaced, the key
	// has not, and the two do not match each other.
	writeBlock(t, certPath, "CERTIFICATE", []byte("nonsense"))
	// Force the next call to look rather than answer from the interval.
	reloader.checked = time.Now().Add(-2 * certCheckInterval)

	after, err := reloader.GetCertificate(nil)
	if err != nil {
		t.Fatalf("a bad write turned into a failed handshake: %v", err)
	}
	if after == nil {
		t.Fatal("no certificate served after a bad write")
	}

	// The file being unreadable is the same story.
	if err := os.Remove(certPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	reloader.checked = time.Now().Add(-2 * certCheckInterval)
	if _, err := reloader.GetCertificate(nil); err != nil {
		t.Errorf("a missing file turned into a failed handshake: %v", err)
	}
}

// sweep walks the map while allow is adding to it and reading the buckets
// inside. The two take their locks in the same order, and this is what says so.
func TestUserLimitersSweepWhileInUse(t *testing.T) {
	limiters := newUserLimiters(4, 2)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for user := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				limiters.allow(int64(user))
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Zero idle so every full bucket is a candidate, which is the
			// hardest version of the walk.
			limiters.sweep(0)
		}
	}()

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// A bucket that is still refusing requests must survive the sweep, or a sweep
// would be a way to reset somebody's rate limit.
func TestSweepSparesABucketThatIsStillEnforcing(t *testing.T) {
	limiters := newUserLimiters(2, 0.001)

	// Spend the bucket.
	for range 2 {
		if !limiters.allow(1) {
			t.Fatal("the bucket refused a request it had tokens for")
		}
	}
	if limiters.allow(1) {
		t.Fatal("the bucket is not actually spent")
	}

	limiters.sweep(0)

	if limiters.allow(1) {
		t.Error("sweeping reset a live rate limit")
	}
}

// The advertised address is written by the watcher and read by anything
// rebuilding the relay.
func TestPublicIPIsSafeToReadWhileItChanges(t *testing.T) {
	h := &Hub{}
	h.storePublicIP("")

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = h.PublicIP()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		addresses := []string{"203.0.113.5", "198.51.100.7", "203.0.113.9"}
		for i := range 500 {
			h.storePublicIP(addresses[i%len(addresses)])
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	if h.PublicIP() == "" {
		t.Error("the address was lost")
	}
}
