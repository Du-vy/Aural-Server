package uploads

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These are the functions that decide what a crafted URL can reach and what a
// browser will do with what comes back. They had no tests, which is the wrong
// way round: they are short precisely because everything they guard is behind
// them.

func TestPathRejectsAnythingThisPackageCouldNotHaveMinted(t *testing.T) {
	store, err := Open(t.TempDir(), 1024, 0, 0)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	for _, key := range []string{
		"",
		"..",
		"../../etc/passwd",
		"..%2f..%2fetc",
		"short",
		strings.Repeat("a", keyLength-1),
		strings.Repeat("a", keyLength+1),
		strings.Repeat("a", keyLength-1) + "/",
		strings.Repeat("a", keyLength-1) + ".",
		strings.Repeat("a", keyLength-1) + "+",
		strings.Repeat("\x00", keyLength),
	} {
		if _, err := store.Path(key); !errors.Is(err, ErrBadKey) {
			t.Fatalf("Path(%q) returned %v, want ErrBadKey", key, err)
		}
	}
}

func TestPathKeepsAMintedKeyInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root, 1024, 0, 0)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	key, err := newKey()
	if err != nil {
		t.Fatalf("new key: %v", err)
	}
	path, err := store.Path(key)
	if err != nil {
		t.Fatalf("Path(%q): %v", key, err)
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs root: %v", err)
	}
	rel, err := filepath.Rel(absRoot, path)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	if strings.HasPrefix(rel, "..") {
		t.Fatalf("a minted key resolved outside the upload directory: %s", rel)
	}
}

func TestSaveEnforcesTheFileCeiling(t *testing.T) {
	store, err := Open(t.TempDir(), 64, 0, 0)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	// A body longer than the ceiling is refused even when it lies about its
	// length, which is the case the declared length cannot catch.
	if _, err := store.Save(bytes.NewReader(make([]byte, 65)), 1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("an oversized body was accepted: %v", err)
	}
	if used := store.UsedBytes(); used != 0 {
		t.Fatalf("a refused upload left %d bytes on the quota", used)
	}

	saved, err := store.Save(bytes.NewReader(make([]byte, 64)), 64)
	if err != nil {
		t.Fatalf("a file exactly at the ceiling was refused: %v", err)
	}
	if saved.Size != 64 {
		t.Fatalf("recorded size %d, want 64", saved.Size)
	}
}

func TestSaveEnforcesTheServerCeiling(t *testing.T) {
	store, err := Open(t.TempDir(), 64, 100, 0)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	if _, err := store.Save(bytes.NewReader(make([]byte, 64)), 64); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	// The reservation is what stops two uploads jointly overshooting, so the
	// second is refused on the declared length rather than after writing.
	if _, err := store.Save(bytes.NewReader(make([]byte, 64)), 64); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("the server ceiling was overshot: %v", err)
	}
}

func TestRemoveGivesTheRoomBack(t *testing.T) {
	store, err := Open(t.TempDir(), 64, 100, 0)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	saved, err := store.Save(bytes.NewReader(make([]byte, 40)), 40)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	store.Remove(saved.Key, saved.Size)
	if used := store.UsedBytes(); used != 0 {
		t.Fatalf("after removing the only file, %d bytes are still counted", used)
	}

	path, err := store.Path(saved.Key)
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the removed file is still on disk")
	}
}

func TestSaveRefusesAnEmptyBody(t *testing.T) {
	store, err := Open(t.TempDir(), 64, 0, 0)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.Save(bytes.NewReader(nil), 0); err == nil {
		t.Fatal("an empty body was stored")
	}
	if used := store.UsedBytes(); used != 0 {
		t.Fatalf("an empty upload left %d bytes on the quota", used)
	}
}

func TestContentTypeIgnoresWhatTheUploaderClaimed(t *testing.T) {
	cases := map[string]string{
		"photo.png":        "image/png",
		"PHOTO.PNG":        "image/png",
		"clip.mp4":         "video/mp4",
		"notes.md":         "text/plain",
		"page.html":        "application/octet-stream",
		"page.htm":         "application/octet-stream",
		"script.svg":       "image/svg+xml",
		"payload.exe":      "application/octet-stream",
		"noextension":      "application/octet-stream",
		"archive.tar.gz":   "application/gzip",
		"trailing.png.exe": "application/octet-stream",
	}
	for filename, want := range cases {
		if got := ContentType(filename); got != want {
			t.Errorf("ContentType(%q) = %q, want %q", filename, got, want)
		}
	}
}

func TestInlineKeepsMarkupOutOfTheBrowser(t *testing.T) {
	// Anything served inline from the server's own origin runs in it, so the
	// list of what may be is the list of what cannot carry script.
	for _, contentType := range []string{"image/png", "video/mp4", "audio/mpeg", "application/pdf", "text/plain"} {
		if !Inline(contentType) {
			t.Errorf("%s should be shown in place", contentType)
		}
	}
	for _, contentType := range []string{
		"image/svg+xml", // markup, and markup can carry script
		"text/html",
		"application/octet-stream",
		"application/zip",
		"application/ogg",
	} {
		if Inline(contentType) {
			t.Errorf("%s must be a download, not shown in place", contentType)
		}
	}
}
