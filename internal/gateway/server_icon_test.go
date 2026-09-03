package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/aural-chat/aural-server/internal/gateway"
	"github.com/aural-chat/aural-server/internal/protocol"
)

func (h *harness) uploadServerIcon(token, filename string, content []byte) *http.Response {
	h.t.Helper()

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", filename)
	if err != nil {
		h.t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		h.t.Fatalf("write form part: %v", err)
	}
	if err := form.Close(); err != nil {
		h.t.Fatalf("close form: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, h.http.URL+"/upload/server-icon", &body)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("do upload server icon: %v", err)
	}
	return res
}

func (h *harness) uploadedServerIcon(token, filename string, content []byte) string {
	h.t.Helper()

	res := h.uploadServerIcon(token, filename, content)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		h.t.Fatalf("upload server icon: got status %d", res.StatusCode)
	}
	var result struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		h.t.Fatalf("decode server icon response: %v", err)
	}
	return result.URL
}

func (h *harness) claimAdmin(c *client) {
	h.t.Helper()
	token, err := gateway.EnsureOwnerToken(context.Background(), h.store, h.server.Hub())
	if err != nil {
		h.t.Fatalf("ensure owner token: %v", err)
	}
	ok[protocol.UserEvent](c, protocol.OpServerClaimAdmin, protocol.ClaimAdminRequest{Token: token})
}

func (h *harness) getInfo() protocol.ServerInfo {
	h.t.Helper()
	res, err := http.Get(h.http.URL + "/info")
	if err != nil {
		h.t.Fatalf("GET /info: %v", err)
	}
	defer res.Body.Close()
	var info protocol.ServerInfo
	if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
		h.t.Fatalf("decode /info: %v", err)
	}
	return info
}

func TestServerIconRequiresManageServer(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	alice := h.dial()
	ready := alice.guest("Alice")

	res := h.uploadServerIcon(ready.SessionToken, "icon.png", pngBytes(t, 128, 128))
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("guest uploading server icon: got %d, want 403", res.StatusCode)
	}
}

func TestServerIconUploadAndPreview(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	admin := h.dial()
	ready := admin.guest("Admin")
	h.claimAdmin(admin)

	iconURL := h.uploadedServerIcon(ready.SessionToken, "icon.png", pngBytes(t, 128, 128))
	if iconURL == "" {
		t.Fatal("expected non-empty icon URL")
	}

	// Verify the file is served
	res := h.fetch(iconURL)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("fetch uploaded server icon: got %d, want 200", res.StatusCode)
	}

	// Verify GET /info returns the icon
	info := h.getInfo()
	if info.Icon != iconURL {
		t.Fatalf("GET /info icon = %q, want %q", info.Icon, iconURL)
	}
}

func TestServerIconReplacedAndOldDropped(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	admin := h.dial()
	ready := admin.guest("Admin")
	h.claimAdmin(admin)

	first := h.uploadedServerIcon(ready.SessionToken, "first.png", pngBytes(t, 128, 128))
	second := h.uploadedServerIcon(ready.SessionToken, "second.png", pngBytes(t, 128, 128))

	if first == second {
		t.Fatal("replacement icon reused key")
	}

	// First icon should be gone (404)
	resOld := h.fetch(first)
	resOld.Body.Close()
	if resOld.StatusCode != http.StatusNotFound {
		t.Fatalf("displaced icon still served: got %d, want 404", resOld.StatusCode)
	}

	// Second icon should be served (200)
	resNew := h.fetch(second)
	resNew.Body.Close()
	if resNew.StatusCode != http.StatusOK {
		t.Fatalf("current icon not served: got %d, want 200", resNew.StatusCode)
	}

	info := h.getInfo()
	if info.Icon != second {
		t.Fatalf("GET /info icon = %q, want %q", info.Icon, second)
	}
}

func TestServerIconRemovalViaServerUpdate(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	admin := h.dial()
	ready := admin.guest("Admin")
	h.claimAdmin(admin)

	iconURL := h.uploadedServerIcon(ready.SessionToken, "icon.png", pngBytes(t, 128, 128))

	empty := ""
	updated := ok[protocol.ServerUpdatedEvent](admin, protocol.OpServerUpdate,
		protocol.ServerUpdateRequest{Icon: &empty})

	if updated.Server.Icon != "" {
		t.Fatalf("icon after removal = %q, want empty", updated.Server.Icon)
	}

	// Displaced icon should be gone (404)
	res := h.fetch(iconURL)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("removed icon still served: got %d, want 404", res.StatusCode)
	}

	info := h.getInfo()
	if info.Icon != "" {
		t.Fatalf("GET /info icon after removal = %q, want empty", info.Icon)
	}
}

func TestServerIconRejectsInvalidFormat(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	admin := h.dial()
	ready := admin.guest("Admin")
	h.claimAdmin(admin)

	res := h.uploadServerIcon(ready.SessionToken, "script.sh", []byte("#!/bin/sh\necho hi"))
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("uploading script as icon: got %d, want 400", res.StatusCode)
	}
}

func TestServerIconRejectsArbitraryPathInUpdate(t *testing.T) {
	h := newHarness(t, withUploads(t, nil))

	admin := h.dial()
	admin.guest("Admin")
	h.claimAdmin(admin)

	hostile := "/attachments/fake/icon.png"
	admin.fails(protocol.OpServerUpdate,
		protocol.ServerUpdateRequest{Icon: &hostile}, protocol.ErrBadRequest)
}
