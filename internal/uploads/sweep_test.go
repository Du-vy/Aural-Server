package uploads

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// stored reports whether a key still has a file behind it. The handle is closed
// straight away: on Windows an open one blocks the temporary directory from
// being cleaned up, which fails the test for a reason that is not the subject.
func stored(t *testing.T, store *Store, key string) bool {
	t.Helper()

	file, _, err := store.Open(key)
	if err != nil {
		return false
	}
	file.Close()
	return true
}

// age backdates a stored file so the sweep's grace period does not spare it,
// which is what lets a test exercise the sweep without waiting a minute.
func age(t *testing.T, store *Store, key string) {
	t.Helper()

	path, err := store.Path(key)
	if err != nil {
		t.Fatalf("path %q: %v", key, err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("backdate %q: %v", key, err)
	}
}

func TestSweepRemovesOnlyWhatNothingNames(t *testing.T) {
	store, err := Open(t.TempDir(), 1024, 0, 0)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	kept, err := store.Save(bytes.NewReader([]byte("referenced")), 10)
	if err != nil {
		t.Fatalf("save kept: %v", err)
	}
	orphan, err := store.Save(bytes.NewReader([]byte("unreferenced")), 12)
	if err != nil {
		t.Fatalf("save orphan: %v", err)
	}
	age(t, store, kept.Key)
	age(t, store, orphan.Key)

	removed, err := store.Sweep(map[string]struct{}{kept.Key: {}}, time.Minute)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed %d files, want 1", removed)
	}

	if !stored(t, store, kept.Key) {
		t.Fatal("the referenced file was swept")
	}
	if stored(t, store, orphan.Key) {
		t.Fatal("the unreferenced file survived the sweep")
	}
}

func TestSweepSparesAFileYoungerThanTheGrace(t *testing.T) {
	store, err := Open(t.TempDir(), 1024, 0, 0)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	// A row is written after the bytes it names, so there is always a moment
	// where a good upload has no row yet. The grace is what stops a sweep in
	// that moment from deleting the file out from under the request.
	fresh, err := store.Save(bytes.NewReader([]byte("just landed")), 11)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	removed, err := store.Sweep(map[string]struct{}{}, time.Minute)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 0 {
		t.Fatalf("the sweep removed %d files that were seconds old", removed)
	}
	if !stored(t, store, fresh.Key) {
		t.Fatal("a file written moments ago was swept")
	}
}

func TestSweepLeavesStrangersAlone(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root, 1024, 0, 0)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	// Anything this package could not have minted is not its business to
	// delete: the upload directory may be somebody's mount point.
	if err := os.MkdirAll(root+"/ab", 0o755); err != nil {
		t.Fatalf("mkdir shard: %v", err)
	}
	stranger := root + "/ab/notes.txt"
	if err := os.WriteFile(stranger, []byte("mine"), 0o644); err != nil {
		t.Fatalf("write stranger: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stranger, old, old); err != nil {
		t.Fatalf("backdate stranger: %v", err)
	}

	removed, err := store.Sweep(map[string]struct{}{}, time.Minute)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 0 {
		t.Fatalf("the sweep removed %d files it did not write", removed)
	}
	if _, err := os.Stat(stranger); err != nil {
		t.Fatalf("a file this package never wrote was deleted: %v", err)
	}
}
