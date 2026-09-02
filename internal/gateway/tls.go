package gateway

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certCheckInterval is how often the certificate files are looked at. A
// renewal is a once-in-two-months event, so checking more eagerly than this
// would only stat a pair of files for nothing.
const certCheckInterval = time.Minute

// certReloader serves the TLS certificate and picks up a renewed one without
// a restart.
//
// A certificate handed to ListenAndServeTLS is read exactly once, at startup.
// Anything issued by an ACME certificate authority — which is to say anything
// a self-hosted server is realistically using, whether from Let's Encrypt
// directly or through a DNS provider — is replaced every couple of months, and
// the old one stops being accepted the moment it expires. Without this, a
// server that is otherwise running perfectly goes dark on a schedule.
type certReloader struct {
	certFile string
	keyFile  string
	log      *slog.Logger

	mu sync.Mutex
	// cert is what is currently being served, and is never nil after
	// construction.
	cert *tls.Certificate
	// stamp is the pair of modification times cert was read from.
	stamp [2]time.Time
	// checked is when the files were last looked at, which bounds how often a
	// handshake touches the disk.
	checked time.Time
}

// newCertReloader loads the pair once, so a server with an unreadable or
// mismatched certificate fails at startup rather than at the first handshake.
func newCertReloader(certFile, keyFile string, log *slog.Logger) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, log: log}

	stamp, err := r.stampOf()
	if err != nil {
		return nil, err
	}
	if err := r.load(stamp); err != nil {
		return nil, err
	}
	r.checked = time.Now()
	return r, nil
}

// GetCertificate is the tls.Config hook. It answers from the held certificate,
// re-reading the files first when they have changed and enough time has passed
// to bother looking.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if time.Since(r.checked) < certCheckInterval {
		return r.cert, nil
	}
	r.checked = time.Now()

	stamp, err := r.stampOf()
	if err != nil {
		// The renewal may be mid-write. Serving the certificate already held
		// is right in every case: it is either still valid, in which case
		// nothing is wrong, or it is not, in which case a failed handshake is
		// no worse than the failed handshake refusing here would produce.
		r.log.Warn("could not stat the TLS certificate", slog.Any("error", err))
		return r.cert, nil
	}
	if stamp == r.stamp {
		return r.cert, nil
	}

	if err := r.load(stamp); err != nil {
		// A renewal writes two files, and between the two writes they do not
		// match each other. Keeping the old pair and looking again on the next
		// check is what rides that out.
		r.log.Warn("the TLS certificate changed but could not be loaded", slog.Any("error", err))
		return r.cert, nil
	}
	r.log.Info("reloaded the TLS certificate",
		slog.String("cert", r.certFile), slog.Time("not_after", notAfter(r.cert)))
	return r.cert, nil
}

// load reads the pair and records the stamp it came from. The stamp is stored
// only on success, so a half-written renewal is retried rather than recorded
// as read.
func (r *certReloader) load(stamp [2]time.Time) error {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("gateway: load certificate: %w", err)
	}
	r.cert, r.stamp = &cert, stamp
	return nil
}

// stampOf is the modification time of each file, which is what a renewal
// changes.
func (r *certReloader) stampOf() ([2]time.Time, error) {
	var stamp [2]time.Time
	for i, path := range [2]string{r.certFile, r.keyFile} {
		info, err := os.Stat(path)
		if err != nil {
			return stamp, fmt.Errorf("gateway: stat %s: %w", path, err)
		}
		stamp[i] = info.ModTime()
	}
	return stamp, nil
}

// notAfter is the expiry of a loaded certificate, for the log line that says
// what was picked up. A zero time means the leaf could not be read, which is
// not worth failing a reload over.
func notAfter(cert *tls.Certificate) time.Time {
	if cert == nil || cert.Leaf == nil {
		return time.Time{}
	}
	return cert.Leaf.NotAfter
}
