package gateway_test

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
)

// withUploads points a harness at a throwaway upload directory. Every test
// that touches a file needs one, and none of them may share it.
func withUploads(t *testing.T, tune func(*config.Config)) func(*config.Config) {
	dir := t.TempDir()
	return func(cfg *config.Config) {
		cfg.Uploads.Path = filepath.Join(dir, "uploads")
		if tune != nil {
			tune(cfg)
		}
	}
}

// pngBytes renders a solid image of a known size, so a test can assert on the
// dimensions the server read back out of it.
func pngBytes(t *testing.T, width, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := range width {
		for y := range height {
			img.Set(x, y, color.RGBA{R: 20, G: 120, B: 220, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// upload posts one file the way a client does and returns the raw response.
func (h *harness) upload(token string, channelID int64, filename string, content []byte) *http.Response {
	h.t.Helper()

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", filename)
	if err != nil {
		h.t.Fatalf("build form: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		h.t.Fatalf("write form: %v", err)
	}
	if err := form.Close(); err != nil {
		h.t.Fatalf("close form: %v", err)
	}

	url := h.http.URL + "/upload?channel=" + strconv.FormatInt(channelID, 10)
	req, err := http.NewRequest(http.MethodPost, url, &body)
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("upload: %v", err)
	}
	return res
}

// uploadOK posts a file and requires it to have been accepted.
func (h *harness) uploadOK(token string, channelID int64, filename string, content []byte) protocol.Attachment {
	h.t.Helper()

	res := h.upload(token, channelID, filename, content)
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(res.Body)
		h.t.Fatalf("upload %s: got %d, want 201 (%s)", filename, res.StatusCode, strings.TrimSpace(string(raw)))
	}

	var attachment protocol.Attachment
	if err := json.NewDecoder(res.Body).Decode(&attachment); err != nil {
		h.t.Fatalf("decode attachment: %v", err)
	}
	return attachment
}

// uploadFails posts a file and requires a specific protocol error back.
func (h *harness) uploadFails(token string, channelID int64, filename string, content []byte, wantCode string) {
	h.t.Helper()

	res := h.upload(token, channelID, filename, content)
	defer res.Body.Close()
	if res.StatusCode < 400 {
		h.t.Fatalf("upload %s unexpectedly succeeded with %d", filename, res.StatusCode)
	}

	var body struct {
		Error protocol.Error `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		h.t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Code != wantCode {
		h.t.Fatalf("upload %s: got error %q, want %q (%s)",
			filename, body.Error.Code, wantCode, body.Error.Message)
	}
}

// fetch reads an attachment back through the URL the server advertised.
func (h *harness) fetch(attachmentURL string) *http.Response {
	h.t.Helper()

	res, err := http.Get(h.http.URL + attachmentURL)
	if err != nil {
		h.t.Fatalf("fetch attachment: %v", err)
	}
	return res
}

func TestUploadedFileTravelsWithItsMessage(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	alice := h.dial()
	ready := alice.guest("Alice")
	channel := textChannel(t, ready)

	content := pngBytes(t, 64, 32)
	attachment := h.uploadOK(ready.SessionToken, channel.ID, "shot.png", content)

	if attachment.ContentType != "image/png" {
		t.Fatalf("content type: got %q, want image/png", attachment.ContentType)
	}
	if attachment.Size != strconv.Itoa(len(content)) {
		t.Fatalf("size: got %s, want %d", attachment.Size, len(content))
	}
	if attachment.Width == nil || *attachment.Width != 64 || attachment.Height == nil || *attachment.Height != 32 {
		t.Fatalf("the server should have read the image dimensions, got %v x %v",
			attachment.Width, attachment.Height)
	}

	// A message may carry a file and nothing else: the picture is the message.
	sent := ok[protocol.MessageEvent](alice, protocol.OpMessageSend, protocol.MessageSendRequest{
		ChannelID:   channel.ID,
		Attachments: []int64{attachment.ID},
	})
	if len(sent.Message.Attachments) != 1 {
		t.Fatalf("the message should carry one file, got %d", len(sent.Message.Attachments))
	}
	if sent.Message.Attachments[0].ID != attachment.ID {
		t.Fatal("the message carries a different file than the one uploaded")
	}

	// The file is readable at the URL the server advertised, and comes back
	// byte for byte.
	res := h.fetch(attachment.URL)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("fetch: got %d, want 200", res.StatusCode)
	}
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("the file that came back is not the file that went up")
	}
	if ct := res.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("served content type: got %q, want image/png", ct)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("a stored file must be served with nosniff")
	}

	// History carries the file too, so a client that joins later sees it.
	page := ok[protocol.MessageHistoryResult](alice, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID})
	if len(page.Messages) != 1 || len(page.Messages[0].Attachments) != 1 {
		t.Fatal("history should carry the message and its file")
	}
}

func TestDeletingAMessageDeletesItsFiles(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	alice := h.dial()
	ready := alice.guest("Alice")
	channel := textChannel(t, ready)

	attachment := h.uploadOK(ready.SessionToken, channel.ID, "doc.txt", []byte("hello from a file"))
	sent := ok[protocol.MessageEvent](alice, protocol.OpMessageSend, protocol.MessageSendRequest{
		ChannelID:   channel.ID,
		Content:     "here you go",
		Attachments: []int64{attachment.ID},
	})

	// Locate the file on disk, so its absence afterwards is provable rather
	// than merely reported.
	path := h.storagePath(t, attachment.URL)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the uploaded file should be on disk: %v", err)
	}

	ok[protocol.MessageDeletedEvent](alice, protocol.OpMessageDelete,
		protocol.MessageDeleteRequest{MessageID: sent.Message.ID})

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("deleting the message should have deleted the file it carried")
	}
	res := h.fetch(attachment.URL)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("the file should be gone, got %d", res.StatusCode)
	}
}

// storagePath turns an advertised URL back into the file behind it.
func (h *harness) storagePath(t *testing.T, attachmentURL string) string {
	t.Helper()

	trimmed := strings.TrimPrefix(attachmentURL, "/attachments/")
	key, _, _ := strings.Cut(trimmed, "/")
	path, err := h.server.Hub().Files().Path(key)
	if err != nil {
		t.Fatalf("resolve storage path: %v", err)
	}
	return path
}

func TestUploadsRespectTheFileCeiling(t *testing.T) {
	h := newHarness(t, withUploads(t, func(cfg *config.Config) {
		cfg.Uploads.MaxFileBytes = 1024
		cfg.Uploads.MaxTotalBytes = 8192
	}))

	alice := h.dial()
	ready := alice.guest("Alice")
	channel := textChannel(t, ready)

	h.uploadOK(ready.SessionToken, channel.ID, "small.txt", bytes.Repeat([]byte("a"), 512))
	h.uploadFails(ready.SessionToken, channel.ID, "big.txt", bytes.Repeat([]byte("a"), 2048), protocol.ErrTooLarge)
}

func TestUploadsRespectTheServerCeiling(t *testing.T) {
	h := newHarness(t, withUploads(t, func(cfg *config.Config) {
		cfg.Uploads.MaxFileBytes = 1024
		cfg.Uploads.MaxTotalBytes = 2048
		// The burst has to cover the run, or the throttle would be what stops
		// it rather than the quota under test.
		cfg.Uploads.MaxPerMessage = 10
	}))

	alice := h.dial()
	ready := alice.guest("Alice")
	channel := textChannel(t, ready)

	h.uploadOK(ready.SessionToken, channel.ID, "one.txt", bytes.Repeat([]byte("a"), 1024))
	h.uploadOK(ready.SessionToken, channel.ID, "two.txt", bytes.Repeat([]byte("a"), 1024))
	h.uploadFails(ready.SessionToken, channel.ID, "three.txt", []byte("no room left"), protocol.ErrStorageFull)
}

func TestUploadNeedsAttachFiles(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	admin, adminReady := h.admin("Root")
	channel := textChannel(t, adminReady)
	everyone := everyoneRole(t, adminReady)

	// Take the permission away from everybody, which is how a server that
	// wants a text-only channel is configured.
	withoutFiles := (permissions.DefaultEveryone &^ permissions.AttachFiles).String()
	ok[protocol.RoleEvent](admin, protocol.OpRoleUpdate, protocol.RoleUpdateRequest{
		RoleID:      everyone,
		Permissions: &withoutFiles,
	})

	bob := h.dial()
	bobReady := bob.guest("Bob")
	h.uploadFails(bobReady.SessionToken, channel.ID, "nope.txt", []byte("denied"), protocol.ErrForbidden)
}

func TestAnUploadCanOnlyBePostedByItsOwnerAndOnlyOnce(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	alice := h.dial()
	aliceReady := alice.guest("Alice")
	channel := textChannel(t, aliceReady)

	bob := h.dial()
	bob.guest("Bob")

	attachment := h.uploadOK(aliceReady.SessionToken, channel.ID, "mine.txt", []byte("alice wrote this"))

	// Somebody else naming the id learns nothing about whether it exists.
	bob.fails(protocol.OpMessageSend, protocol.MessageSendRequest{
		ChannelID:   channel.ID,
		Content:     "not mine",
		Attachments: []int64{attachment.ID},
	}, protocol.ErrNotFound)

	ok[protocol.MessageEvent](alice, protocol.OpMessageSend, protocol.MessageSendRequest{
		ChannelID:   channel.ID,
		Content:     "mine",
		Attachments: []int64{attachment.ID},
	})

	// And it cannot be posted a second time, which is what stops one upload
	// from being spread across a whole channel.
	alice.fails(protocol.OpMessageSend, protocol.MessageSendRequest{
		ChannelID:   channel.ID,
		Content:     "again",
		Attachments: []int64{attachment.ID},
	}, protocol.ErrNotFound)
}

func TestUploadRejectsABadToken(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	alice := h.dial()
	channel := textChannel(t, alice.guest("Alice"))

	h.uploadFails("not-a-real-token", channel.ID, "x.txt", []byte("nope"), protocol.ErrInvalidCredentials)
	h.uploadFails("", channel.ID, "x.txt", []byte("nope"), protocol.ErrUnauthorized)
}

func TestUploadsCanBeSwitchedOff(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) { cfg.Uploads.Enabled = false })

	alice := h.dial()
	ready := alice.guest("Alice")
	channel := textChannel(t, ready)

	if ready.Server.Uploads.Enabled {
		t.Fatal("a server with uploads off should say so in its server info")
	}
	h.uploadFails(ready.SessionToken, channel.ID, "x.txt", []byte("nope"), protocol.ErrUploadsDisabled)
}

func TestServerInfoAdvertisesTheUploadLimits(t *testing.T) {
	h := newHarness(t, withUploads(t, func(cfg *config.Config) {
		cfg.Uploads.MaxFileBytes = 12345
		cfg.Uploads.MaxTotalBytes = 678900
		cfg.Uploads.MaxPerMessage = 4
	}))

	res, err := http.Get(h.http.URL + "/info")
	if err != nil {
		t.Fatalf("get info: %v", err)
	}
	defer res.Body.Close()

	var info protocol.ServerInfo
	if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
		t.Fatalf("decode info: %v", err)
	}
	if !info.Uploads.Enabled {
		t.Fatal("uploads should be advertised as enabled")
	}
	if info.Uploads.MaxFileBytes != "12345" || info.Uploads.MaxTotalBytes != "678900" {
		t.Fatalf("limits: got %s / %s", info.Uploads.MaxFileBytes, info.Uploads.MaxTotalBytes)
	}
	if info.Uploads.MaxPerMessage != 4 {
		t.Fatalf("per-message limit: got %d, want 4", info.Uploads.MaxPerMessage)
	}
}

func TestAFileIsNamedButNeverPathed(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	alice := h.dial()
	ready := alice.guest("Alice")
	channel := textChannel(t, ready)

	// A name that is a traversal attempt keeps only its last segment, so it
	// can never decide where the file lands.
	attachment := h.uploadOK(ready.SessionToken, channel.ID, "../../../etc/passwd", []byte("root:x:0:0"))
	if attachment.Filename != "passwd" {
		t.Fatalf("filename: got %q, want %q", attachment.Filename, "passwd")
	}

	// An unknown extension is served as something no browser will interpret.
	page := h.uploadOK(ready.SessionToken, channel.ID, "trap.html", []byte("<script>alert(1)</script>"))
	if page.ContentType != "application/octet-stream" {
		t.Fatalf("content type: got %q, want application/octet-stream", page.ContentType)
	}
	res := h.fetch(page.URL)
	defer res.Body.Close()
	if disposition := res.Header.Get("Content-Disposition"); !strings.HasPrefix(disposition, "attachment") {
		t.Fatalf("an uninterpretable file must be a download, got %q", disposition)
	}
}
