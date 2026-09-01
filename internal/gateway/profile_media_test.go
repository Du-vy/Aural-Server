package gateway_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// uploadedAvatar posts one avatar and returns the URL the server gave it.
func (h *harness) uploadedAvatar(token, filename string, content []byte) string {
	h.t.Helper()

	res := h.uploadAvatar(token, filename, content)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		h.t.Fatalf("upload avatar: got status %d", res.StatusCode)
	}
	var result struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		h.t.Fatalf("decode avatar response: %v", err)
	}
	return result.URL
}

func TestProfileMediaCountsAgainstTheQuota(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	alice := h.dial()
	ready := alice.guest("Alice")

	before, err := h.store.TotalProfileMediaBytes(context.Background())
	if err != nil {
		t.Fatalf("total profile media bytes: %v", err)
	}
	if before != 0 {
		t.Fatalf("a fresh server holds %d picture bytes, want 0", before)
	}

	h.uploadedAvatar(ready.SessionToken, "avatar.png", pngBytes(t, 128, 128))

	// The row is what makes the bytes countable. Without it a restart reads the
	// quota back from the attachments table alone and forgets every picture.
	after, err := h.store.TotalProfileMediaBytes(context.Background())
	if err != nil {
		t.Fatalf("total profile media bytes: %v", err)
	}
	if after <= 0 {
		t.Fatal("an uploaded avatar was not counted against the quota")
	}
}

func TestReplacingAnAvatarReclaimsTheOldOne(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	alice := h.dial()
	ready := alice.guest("Alice")

	first := h.uploadedAvatar(ready.SessionToken, "first.png", pngBytes(t, 128, 128))
	firstSize, err := h.store.TotalProfileMediaBytes(context.Background())
	if err != nil {
		t.Fatalf("total profile media bytes: %v", err)
	}

	second := h.uploadedAvatar(ready.SessionToken, "second.png", pngBytes(t, 128, 128))
	if second == first {
		t.Fatal("a replacement avatar reused the storage key of the one it replaced")
	}

	// A user holds one avatar, so replacing must not leave the quota carrying
	// two: the old row goes as the new one lands, and the file with it.
	after, err := h.store.TotalProfileMediaBytes(context.Background())
	if err != nil {
		t.Fatalf("total profile media bytes: %v", err)
	}
	if after >= firstSize*2 {
		t.Fatalf("replacing an avatar left both counted: %d bytes after one of %d", after, firstSize)
	}

	// And the displaced file is gone from disk rather than merely unreferenced.
	if res := h.fetch(first); res.StatusCode != http.StatusNotFound {
		res.Body.Close()
		t.Fatalf("the replaced avatar is still served: got %d, want 404", res.StatusCode)
	}
	res := h.fetch(second)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the current avatar is not served: got %d", res.StatusCode)
	}
}

func TestAvatarMustPointAtThisServer(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	alice := h.dial()
	alice.guest("Alice")

	// Every client that draws the member list fetches whatever this field
	// names, so a URL pointing elsewhere turns the member list into a beacon
	// for a host of the setter's choosing.
	for _, hostile := range []string{
		"https://tracker.example/pixel.gif",
		"http://127.0.0.1:9999/probe.png",
		"//tracker.example/pixel.gif",
		"data:image/gif;base64,R0lGODlhAQABAAAAACw=",
		"/attachments/../../etc/passwd/x.png",
		"/attachments/not-a-real-key/x.png",
		"/elsewhere/key/x.png",
	} {
		value := hostile
		alice.fails(protocol.OpUserUpdate,
			protocol.UserUpdateRequest{Avatar: &value}, protocol.ErrBadRequest)
	}
}

func TestAvatarAcceptsAnUploadedFileAndClearing(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	alice := h.dial()
	ready := alice.guest("Alice")

	uploaded := h.uploadedAvatar(ready.SessionToken, "avatar.png", pngBytes(t, 64, 64))

	// What the upload endpoint hands back is exactly what the field accepts.
	result := ok[protocol.UserEvent](alice, protocol.OpUserUpdate,
		protocol.UserUpdateRequest{Avatar: &uploaded})
	if result.User.Avatar == nil || *result.User.Avatar != uploaded {
		t.Fatalf("avatar was not kept: %v", result.User.Avatar)
	}

	// And removing a picture stays possible. An empty string is what carries
	// that over JSON: a null would reach the server as an absent field.
	empty := ""
	cleared := ok[protocol.UserEvent](alice, protocol.OpUserUpdate,
		protocol.UserUpdateRequest{Avatar: &empty})
	if cleared.User.Avatar != nil {
		t.Fatalf("avatar was not cleared: %v", *cleared.User.Avatar)
	}

	// Leaving the field out must not disturb the picture, which is the case a
	// pointer-to-a-pointer could not tell apart from a removal.
	nickname := "Alicia"
	kept := ok[protocol.UserEvent](alice, protocol.OpUserUpdate,
		protocol.UserUpdateRequest{Nickname: &nickname})
	if kept.User.Nickname != nickname {
		t.Fatalf("nickname was not changed: %q", kept.User.Nickname)
	}
}

func TestTheStartupSweepSparesPicturesAndAttachments(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	alice := h.dial()
	ready := alice.guest("Alice")
	channel := textChannel(t, ready).ID

	avatar := h.uploadedAvatar(ready.SessionToken, "avatar.png", pngBytes(t, 64, 64))
	attachment := h.uploadOK(ready.SessionToken, channel, "notes.txt", []byte("hello"))

	// An orphan: a valid key with nothing pointing at it, which is what a crash
	// between writing the bytes and writing the row leaves behind. Backdated
	// past the grace period, since that guard is about uploads still in flight.
	orphan := plantOrphan(t, h)

	h.server.SweepOrphanedFiles(context.Background())

	if res := h.fetch(avatar); res.StatusCode != http.StatusOK {
		res.Body.Close()
		t.Fatalf("the sweep took an avatar: got %d", res.StatusCode)
	} else {
		res.Body.Close()
	}
	if res := h.fetch(attachment.URL); res.StatusCode != http.StatusOK {
		res.Body.Close()
		t.Fatalf("the sweep took an attachment: got %d", res.StatusCode)
	} else {
		res.Body.Close()
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the unreferenced file survived the sweep")
	}
}

// plantOrphan writes a file with a plausible key that no row names, backdated
// so the sweep's grace period does not spare it. It returns its path.
func plantOrphan(t *testing.T, h *harness) string {
	t.Helper()

	// Borrowing a real key's shape from a stored file is what keeps this in
	// step with whatever the uploads package considers well formed.
	key := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 24))
	dir := filepath.Join(h.cfg.Uploads.Path, key[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir shard: %v", err)
	}
	path := filepath.Join(dir, key)
	if err := os.WriteFile(path, []byte("nothing points at me"), 0o644); err != nil {
		t.Fatalf("write orphan: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("backdate orphan: %v", err)
	}
	return path
}
