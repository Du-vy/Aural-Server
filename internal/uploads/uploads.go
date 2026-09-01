// Package uploads owns the files behind message attachments: where they live
// on disk, how much room they are allowed to take, and what content type they
// are served back as.
//
// The database owns which message a file belongs to; this package owns only
// the bytes. Keeping the two apart is what lets deleting a message be a single
// row delete followed by a best-effort unlink, rather than a transaction
// spanning a filesystem.
package uploads

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrTooLarge means one file exceeded the per-file ceiling.
var ErrTooLarge = errors.New("uploads: file is larger than this server allows")

// ErrQuotaExceeded means the server-wide ceiling has no room left.
var ErrQuotaExceeded = errors.New("uploads: the server has no storage left")

// ErrBadKey means a storage key was not one this package could have minted,
// which is what stops a crafted download URL from escaping the upload
// directory.
var ErrBadKey = errors.New("uploads: malformed storage key")

// keyBytes is the entropy of a storage key. A key is the unguessable part of a
// download URL, so it carries the same weight as a session token: attachments
// are served without a second authentication step, because an <img> or <video>
// tag cannot present one.
const keyBytes = 24

// Store is the upload directory and the accounting that goes with it.
type Store struct {
	root     string
	maxFile  int64
	maxTotal int64

	// mu guards used, which is the running total of every stored file plus the
	// bytes reserved by uploads still in flight. Keeping it in memory is what
	// makes the quota check a comparison rather than a SUM over the table on
	// every upload.
	mu   sync.Mutex
	used int64
}

// Open prepares the upload directory. used is the total size of the files the
// database already knows about, which the caller reads once at startup.
//
// A maxTotal of zero means the only ceiling is the disk itself.
func Open(root string, maxFile, maxTotal, used int64) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("uploads: create %s: %w", root, err)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("uploads: resolve %s: %w", root, err)
	}
	return &Store{root: abs, maxFile: maxFile, maxTotal: maxTotal, used: used}, nil
}

// MaxFileBytes is the per-file ceiling, which clients are told up front.
func (s *Store) MaxFileBytes() int64 { return s.maxFile }

// MaxTotalBytes is the server-wide ceiling, zero meaning none.
func (s *Store) MaxTotalBytes() int64 { return s.maxTotal }

// UsedBytes is how much room the stored files take right now.
func (s *Store) UsedBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used
}

// reserve claims room for an upload before a byte is written, so two large
// uploads racing each other cannot both pass a check and jointly overshoot.
func (s *Store) reserve(n int64) error {
	if n > s.maxFile {
		return ErrTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxTotal > 0 && s.used+n > s.maxTotal {
		return ErrQuotaExceeded
	}
	s.used += n
	return nil
}

// settle turns a reservation into the space actually used, which is smaller
// whenever the upload was shorter than its declared length.
func (s *Store) settle(reserved, actual int64) {
	s.mu.Lock()
	s.used += actual - reserved
	s.mu.Unlock()
}

// Saved describes a file that reached the disk.
type Saved struct {
	Key  string
	Size int64
}

// Save streams r into the upload directory.
func (s *Store) Save(r io.Reader, contentLength int64) (Saved, error) {
	return s.SaveWithLimit(r, contentLength, s.maxFile)
}

// SaveWithLimit streams r into the upload directory, capped at maxBytes.
func (s *Store) SaveWithLimit(r io.Reader, contentLength, maxBytes int64) (Saved, error) {
	if maxBytes <= 0 || (s.maxFile > 0 && maxBytes > s.maxFile) {
		maxBytes = s.maxFile
	}
	reserved := contentLength
	if reserved <= 0 || reserved > maxBytes {
		reserved = maxBytes
	}
	if err := s.reserve(reserved); err != nil {
		return Saved{}, err
	}

	key, err := newKey()
	if err != nil {
		s.settle(reserved, 0)
		return Saved{}, err
	}

	path := s.pathFor(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.settle(reserved, 0)
		return Saved{}, fmt.Errorf("uploads: create shard: %w", err)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		s.settle(reserved, 0)
		return Saved{}, fmt.Errorf("uploads: create %s: %w", key, err)
	}

	// One byte past the ceiling is read on purpose: a body that produces it is
	// over the limit, and stopping exactly at the limit could not tell the
	// difference between a file that fits and one that was truncated.
	written, err := io.Copy(file, io.LimitReader(r, maxBytes+1))
	closeErr := file.Close()

	switch {
	case err != nil:
		s.discard(path, reserved)
		return Saved{}, fmt.Errorf("uploads: write %s: %w", key, err)
	case closeErr != nil:
		s.discard(path, reserved)
		return Saved{}, fmt.Errorf("uploads: write %s: %w", key, closeErr)
	case written > maxBytes:
		s.discard(path, reserved)
		return Saved{}, ErrTooLarge
	case written == 0:
		s.discard(path, reserved)
		return Saved{}, errors.New("uploads: the file was empty")
	}

	// The reservation was an upper bound; only what was written is kept, and a
	// short upload gives the rest back.
	s.settle(reserved, written)
	if s.maxTotal > 0 && s.UsedBytes() > s.maxTotal {
		// Only reachable when the declared length undershot the real one, so
		// the reservation was too small to have caught this earlier.
		s.Remove(key, written)
		return Saved{}, ErrQuotaExceeded
	}
	return Saved{Key: key, Size: written}, nil
}

// discard removes a half-written file and gives its whole reservation back.
func (s *Store) discard(path string, reserved int64) {
	_ = os.Remove(path)
	s.settle(reserved, 0)
}

// Open reads a stored file back.
func (s *Store) Open(key string) (*os.File, os.FileInfo, error) {
	path, err := s.Path(key)
	if err != nil {
		return nil, nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

// Remove deletes a stored file and returns its room to the quota. A file that
// is already gone is not an error: the row it belonged to is what matters, and
// it has been deleted by the time this is called.
func (s *Store) Remove(key string, size int64) {
	path, err := s.Path(key)
	if err != nil {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
	s.mu.Lock()
	s.used -= size
	if s.used < 0 {
		s.used = 0
	}
	s.mu.Unlock()
}

// Path is where a key lives on disk. It rejects anything this package could not
// have minted, so a key arriving from a URL can never walk out of the upload
// directory.
func (s *Store) Path(key string) (string, error) {
	if !validKey(key) {
		return "", ErrBadKey
	}
	return filepath.Join(s.root, key[:2], key), nil
}

// pathFor is Path for a key this package just minted.
func (s *Store) pathFor(key string) string {
	return filepath.Join(s.root, key[:2], key)
}

// newKey mints a storage key: the file's name on disk and the unguessable part
// of its download URL.
func newKey() (string, error) {
	buf := make([]byte, keyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("uploads: read key entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// keyLength is how many characters keyBytes encodes to.
var keyLength = base64.RawURLEncoding.EncodedLen(keyBytes)

func validKey(key string) bool {
	if len(key) != keyLength {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// Sweep walks the upload directory and removes files no key in keep names. It
// is what reclaims bytes left behind by a crash between writing a file and
// recording its row.
func (s *Store) Sweep(keep map[string]struct{}) (removed int, err error) {
	shards, err := os.ReadDir(s.root)
	if err != nil {
		return 0, fmt.Errorf("uploads: read %s: %w", s.root, err)
	}
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(s.root, shard.Name()))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !validKey(name) {
				continue
			}
			if _, held := keep[name]; held {
				continue
			}
			if os.Remove(filepath.Join(s.root, shard.Name(), name)) == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// --- content types ----------------------------------------------------------

// byExtension is the curated map from file extension to the type a file is
// served as. It is deliberately a list rather than a call to mime.TypeByExtension:
// what a server hands back decides what a browser will render and therefore
// what it will execute, and that is not a decision to delegate to the host's
// registry.
var byExtension = map[string]string{
	// Images.
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".avif": "image/avif",
	".bmp": "image/bmp", ".ico": "image/x-icon", ".svg": "image/svg+xml",
	".heic": "image/heic", ".tif": "image/tiff", ".tiff": "image/tiff",

	// Audio.
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac",
	".wav": "audio/wav", ".flac": "audio/flac", ".opus": "audio/ogg",
	".oga": "audio/ogg", ".weba": "audio/webm",

	// Video.
	".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm",
	".mov": "video/quicktime", ".mkv": "video/x-matroska",
	".ogv": "video/ogg", ".avi": "video/x-msvideo",

	// Documents a client can show inline.
	".pdf": "application/pdf",

	// Text and source, all served as plain text so that nothing in them is
	// ever interpreted as markup by a browser that navigates to one directly.
	".txt": "text/plain", ".md": "text/plain", ".markdown": "text/plain",
	".log": "text/plain", ".csv": "text/plain", ".tsv": "text/plain",
	".json": "text/plain", ".yaml": "text/plain", ".yml": "text/plain",
	".toml": "text/plain", ".ini": "text/plain", ".conf": "text/plain",
	".xml": "text/plain", ".sql": "text/plain", ".diff": "text/plain",
	".patch": "text/plain", ".go": "text/plain", ".rs": "text/plain",
	".py": "text/plain", ".js": "text/plain", ".ts": "text/plain",
	".jsx": "text/plain", ".tsx": "text/plain", ".c": "text/plain",
	".h": "text/plain", ".cpp": "text/plain", ".hpp": "text/plain",
	".cs": "text/plain", ".java": "text/plain", ".kt": "text/plain",
	".rb": "text/plain", ".php": "text/plain", ".sh": "text/plain",
	".bat": "text/plain", ".ps1": "text/plain", ".css": "text/plain",
	".lua": "text/plain", ".swift": "text/plain", ".zig": "text/plain",

	// Archives, which are only ever downloaded.
	".zip": "application/zip", ".gz": "application/gzip",
	".tar": "application/x-tar", ".7z": "application/x-7z-compressed",
	".rar": "application/vnd.rar", ".xz": "application/x-xz",
	".zst": "application/zstd",

	// .ogg is ambiguous by extension alone: the container carries audio as
	// often as video. The generic type is right, and every player probes it.
	".ogg": "application/ogg",
}

// ContentType decides what a stored file is served as, from its name alone.
//
// The type a client declared at upload is deliberately ignored. Anything not
// recognised becomes application/octet-stream, which is inert everywhere.
func ContentType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	if known, ok := byExtension[ext]; ok {
		return known
	}
	return "application/octet-stream"
}

// Inline reports whether a type is one a browser may render in place rather
// than download.
//
// SVG is excluded on purpose: it is markup, and markup served inline from the
// server's own origin can carry script. It still renders inside an <img> tag,
// which is where a client shows it, because a subresource load ignores the
// disposition entirely.
func Inline(contentType string) bool {
	if contentType == "image/svg+xml" {
		return false
	}
	switch {
	case strings.HasPrefix(contentType, "image/"),
		strings.HasPrefix(contentType, "audio/"),
		strings.HasPrefix(contentType, "video/"):
		return true
	}
	return contentType == "application/pdf" || contentType == "text/plain"
}
