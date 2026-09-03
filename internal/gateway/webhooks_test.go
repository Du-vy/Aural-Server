package gateway_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
)

// post makes a plain POST against the harness with the given content type.
func (h *harness) post(path, contentType string, body []byte, headers map[string]string) *http.Response {
	h.t.Helper()

	req, err := http.NewRequest(http.MethodPost, h.http.URL+path, bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("post %s: %v", path, err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (h *harness) postJSON(path string, payload any) *http.Response {
	h.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatalf("encode payload: %v", err)
	}
	return h.post(path, "application/json", raw, nil)
}

// newWebhook redeems the owner token, mints a webhook in the seeded text
// channel and hands back both it and the admin connection.
func (h *harness) newWebhook(name string) (*client, protocol.Ready, protocol.Webhook) {
	h.t.Helper()

	admin, ready := h.admin("Root")
	channel := textChannel(h.t, ready)
	created := ok[protocol.WebhookEvent](admin, protocol.OpWebhookCreate,
		protocol.WebhookCreateRequest{ChannelID: channel.ID, Name: name})
	return admin, ready, created.Webhook
}

func TestWebhookURLIsTheDiscordShape(t *testing.T) {
	h := newHarness(t, nil)
	_, _, wh := h.newWebhook("Deploys")

	want := fmt.Sprintf("/api/webhooks/%d/%s", wh.ID, wh.Token)
	if wh.URL != want {
		t.Fatalf("webhook URL: got %q, want %q", wh.URL, want)
	}
	if wh.Token == "" {
		t.Fatal("a webhook with no token cannot be posted to")
	}
	if wh.LastUsedAt != 0 {
		t.Fatalf("a webhook nobody has used should report zero, got %d", wh.LastUsedAt)
	}
}

func TestWebhookDeliversAMessageToTheChannel(t *testing.T) {
	h := newHarness(t, nil)
	admin, ready, wh := h.newWebhook("CI")
	channel := textChannel(t, ready)

	// Somebody else is watching the channel: a delivery has to reach them, not
	// only the administrator who made the webhook.
	watcher := h.dial()
	watcher.guest("Watcher")

	resp := h.postJSON(wh.URL, map[string]any{
		"content":    "the build is green",
		"username":   "Buildbot",
		"avatar_url": "https://example.com/bot.png",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("execute: got %d, want 204 (%s)", resp.StatusCode, bodyOf(t, resp))
	}

	event := watcher.waitEvent(protocol.EvMessageCreated)
	var payload protocol.MessageEvent
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		t.Fatalf("decode message event: %v", err)
	}
	message := payload.Message
	if message.ChannelID != channel.ID {
		t.Fatalf("message landed in channel %d, want %d", message.ChannelID, channel.ID)
	}
	if message.Content != "the build is green" {
		t.Fatalf("content: got %q", message.Content)
	}
	// The delivery overrode the name, so this message carries the override and
	// the webhook keeps its own.
	if message.Author != "Buildbot" {
		t.Fatalf("author: got %q, want the per-delivery override", message.Author)
	}
	if message.UserID != nil {
		t.Fatal("a webhook message must not be attributed to an identity")
	}
	if message.Webhook == nil || message.Webhook.ID != wh.ID {
		t.Fatalf("message does not name the webhook that posted it: %+v", message.Webhook)
	}
	if message.Webhook.Avatar == nil || *message.Webhook.Avatar != "https://example.com/bot.png" {
		t.Fatalf("avatar override was not kept: %+v", message.Webhook.Avatar)
	}

	// The webhook still reports its own name, not the one this delivery used.
	list := ok[protocol.WebhookListResult](admin, protocol.OpWebhookList, protocol.WebhookListRequest{})
	if len(list.Webhooks) != 1 || list.Webhooks[0].Name != "CI" {
		t.Fatalf("a per-delivery username must not rename the webhook: %+v", list.Webhooks)
	}
	if list.Webhooks[0].LastUsedAt == 0 {
		t.Fatal("a delivery should have recorded that the webhook was used")
	}
}

func TestWebhookWaitReturnsTheDiscordMessage(t *testing.T) {
	h := newHarness(t, nil)
	_, _, wh := h.newWebhook("CI")

	resp := h.postJSON(wh.URL+"?wait=true", map[string]any{"content": "hello"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("execute with wait: got %d, want 200 (%s)", resp.StatusCode, bodyOf(t, resp))
	}

	var message struct {
		ID        string `json:"id"`
		Content   string `json:"content"`
		WebhookID string `json:"webhook_id"`
		Author    struct {
			Username string `json:"username"`
			Bot      bool   `json:"bot"`
		} `json:"author"`
		Embeds      []protocol.Embed `json:"embeds"`
		Attachments []any            `json:"attachments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&message); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if message.ID == "" {
		t.Fatal("a sender that waits needs the id back so it can edit the message later")
	}
	if message.Content != "hello" {
		t.Fatalf("content: got %q", message.Content)
	}
	if !message.Author.Bot || message.Author.Username != "CI" {
		t.Fatalf("author: got %+v", message.Author)
	}
	if message.WebhookID != fmt.Sprint(wh.ID) {
		t.Fatalf("webhook_id: got %q, want %d", message.WebhookID, wh.ID)
	}
	// Both are lists in Discord's shape even when empty, and a client library
	// that iterates them must not meet a null.
	if message.Embeds == nil || message.Attachments == nil {
		t.Fatal("embeds and attachments must be lists rather than null")
	}
}

func TestWebhookEmbedsSurviveAndAreClamped(t *testing.T) {
	h := newHarness(t, nil)
	_, ready, wh := h.newWebhook("Alerts")
	channel := textChannel(t, ready)

	watcher := h.dial()
	watcher.guest("Watcher")

	long := strings.Repeat("x", 300)
	resp := h.postJSON(wh.URL, map[string]any{
		"embeds": []map[string]any{{
			"title":       long,
			"description": "disk is at 91%",
			"color":       0x1FF00FF, // one bit past 24, and must be masked
			"url":         "javascript:alert(1)",
			"author":      map[string]any{"name": "monitor", "icon_url": "https://example.com/i.png"},
			"footer":      map[string]any{"text": "node-1"},
			"fields": []map[string]any{
				{"name": "host", "value": "node-1", "inline": true},
				{"name": "free", "value": "4 GB", "inline": true},
			},
		}},
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("execute: got %d (%s)", resp.StatusCode, bodyOf(t, resp))
	}

	event := watcher.waitEvent(protocol.EvMessageCreated)
	var payload protocol.MessageEvent
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		t.Fatalf("decode message event: %v", err)
	}
	if payload.Message.ChannelID != channel.ID {
		t.Fatalf("wrong channel: %d", payload.Message.ChannelID)
	}
	if len(payload.Message.Embeds) != 1 {
		t.Fatalf("embeds: got %d, want 1", len(payload.Message.Embeds))
	}
	embed := payload.Message.Embeds[0]
	if runes := len([]rune(embed.Title)); runes != 256 {
		t.Fatalf("title should be cut to Discord's limit, got %d runes", runes)
	}
	if embed.Color == nil || *embed.Color != 0xFF00FF {
		t.Fatalf("colour should be masked into 24 bits, got %#x", *embed.Color)
	}
	if embed.URL != "" {
		t.Fatalf("a scheme a client would run must be dropped, got %q", embed.URL)
	}
	if len(embed.Fields) != 2 || !embed.Fields[0].Inline {
		t.Fatalf("fields did not survive: %+v", embed.Fields)
	}
	if embed.Author == nil || embed.Author.Name != "monitor" {
		t.Fatalf("author did not survive: %+v", embed.Author)
	}
}

func TestWebhookRefusesAnEmptyDelivery(t *testing.T) {
	h := newHarness(t, nil)
	_, _, wh := h.newWebhook("CI")

	resp := h.postJSON(wh.URL, map[string]any{"content": "  "})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty delivery: got %d, want 400", resp.StatusCode)
	}
	var failure struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&failure); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if failure.Code != 50006 {
		t.Fatalf("code: got %d, want Discord's 50006", failure.Code)
	}
}

func TestWebhookRejectsAWrongToken(t *testing.T) {
	h := newHarness(t, nil)
	_, _, wh := h.newWebhook("CI")

	resp := h.postJSON(fmt.Sprintf("/api/webhooks/%d/not-the-token", wh.ID), map[string]any{"content": "x"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", resp.StatusCode)
	}

	missing := h.postJSON("/api/webhooks/999999/whatever", map[string]any{"content": "x"})
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown webhook: got %d, want 404", missing.StatusCode)
	}
}

func TestWebhookAnswersUnderAVersionedPath(t *testing.T) {
	h := newHarness(t, nil)
	_, _, wh := h.newWebhook("CI")

	resp := h.postJSON("/api/v10"+strings.TrimPrefix(wh.URL, "/api"), map[string]any{"content": "hi"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("versioned path: got %d, want 204 (%s)", resp.StatusCode, bodyOf(t, resp))
	}
}

func TestWebhookFetchDescribesItself(t *testing.T) {
	h := newHarness(t, nil)
	_, ready, wh := h.newWebhook("CI")
	channel := textChannel(t, ready)

	resp := h.get(wh.URL, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetch: got %d", resp.StatusCode)
	}
	var described struct {
		ID        string `json:"id"`
		Type      int    `json:"type"`
		Name      string `json:"name"`
		ChannelID string `json:"channel_id"`
		Token     string `json:"token"`
		URL       string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&described); err != nil {
		t.Fatalf("decode webhook: %v", err)
	}
	if described.ID != fmt.Sprint(wh.ID) || described.ChannelID != fmt.Sprint(channel.ID) {
		t.Fatalf("ids are not the decimal strings a client library expects: %+v", described)
	}
	if described.Type != 1 {
		t.Fatalf("type: got %d, want 1 (incoming webhook)", described.Type)
	}
	if !strings.HasSuffix(described.URL, wh.URL) {
		t.Fatalf("url should be absolute and end in the path: %q", described.URL)
	}
}

func TestWebhookEditsAndDeletesOnlyItsOwnMessages(t *testing.T) {
	h := newHarness(t, nil)
	admin, ready, wh := h.newWebhook("Status")
	channel := textChannel(t, ready)

	// One message from the webhook, one from a person.
	resp := h.postJSON(wh.URL+"?wait=true", map[string]any{"content": "deploying"})
	var posted struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&posted); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	human := ok[protocol.MessageEvent](admin, protocol.OpMessageSend,
		protocol.MessageSendRequest{ChannelID: channel.ID, Content: "watching"})

	// Editing its own is allowed, and everybody in the channel is told.
	edit, err := http.NewRequest(http.MethodPatch, h.http.URL+wh.URL+"/messages/"+posted.ID,
		strings.NewReader(`{"content":"deployed"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	edit.Header.Set("Content-Type", "application/json")
	edited, err := http.DefaultClient.Do(edit)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	defer edited.Body.Close()
	if edited.StatusCode != http.StatusOK {
		t.Fatalf("edit: got %d, want 200 (%s)", edited.StatusCode, bodyOf(t, edited))
	}
	updated := admin.waitEvent(protocol.EvMessageUpdated)
	var updatedPayload protocol.MessageEvent
	if err := json.Unmarshal(updated.Data, &updatedPayload); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if updatedPayload.Message.Content != "deployed" {
		t.Fatalf("edit did not take: %q", updatedPayload.Message.Content)
	}

	// Somebody else's message is not the webhook's to touch, and reports the
	// same way a message that does not exist does.
	stranger, err := http.NewRequest(http.MethodDelete,
		fmt.Sprintf("%s%s/messages/%d", h.http.URL, wh.URL, human.Message.ID), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	refused, err := http.DefaultClient.Do(stranger)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer refused.Body.Close()
	if refused.StatusCode != http.StatusNotFound {
		t.Fatalf("a webhook must not reach a person's message: got %d", refused.StatusCode)
	}
}

func TestWebhookAcceptsSlackPayloads(t *testing.T) {
	h := newHarness(t, nil)
	_, _, wh := h.newWebhook("Slacky")

	watcher := h.dial()
	watcher.guest("Watcher")

	resp := h.postJSON(wh.URL+"/slack", map[string]any{
		"text":     "see <https://example.com|the dashboard>",
		"username": "Monitor",
		"attachments": []map[string]any{{
			"color": "danger",
			"title": "CPU high",
			"text":  "92% for 5 minutes",
			"fields": []map[string]any{
				{"title": "host", "value": "node-1", "short": true},
			},
		}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("slack delivery: got %d (%s)", resp.StatusCode, bodyOf(t, resp))
	}
	if body := bodyOf(t, resp); body != "ok" {
		t.Fatalf("Slack's own endpoint answers with a bare ok, got %q", body)
	}

	event := watcher.waitEvent(protocol.EvMessageCreated)
	var payload protocol.MessageEvent
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if payload.Message.Author != "Monitor" {
		t.Fatalf("author: got %q", payload.Message.Author)
	}
	if !strings.Contains(payload.Message.Content, "[the dashboard](https://example.com)") {
		t.Fatalf("Slack's link syntax was not rewritten: %q", payload.Message.Content)
	}
	if len(payload.Message.Embeds) != 1 {
		t.Fatalf("the attachment should have become a card: %+v", payload.Message.Embeds)
	}
	embed := payload.Message.Embeds[0]
	if embed.Title != "CPU high" || embed.Color == nil || *embed.Color != 0xa30100 {
		t.Fatalf("attachment did not translate: %+v", embed)
	}
	if len(embed.Fields) != 1 || !embed.Fields[0].Inline {
		t.Fatalf("a short field is an inline one: %+v", embed.Fields)
	}
}

func TestWebhookAcceptsGitHubPayloads(t *testing.T) {
	h := newHarness(t, nil)
	_, _, wh := h.newWebhook("GitHub")

	watcher := h.dial()
	watcher.guest("Watcher")

	body, err := json.Marshal(map[string]any{
		"ref":     "refs/heads/main",
		"compare": "https://github.com/o/r/compare/a...b",
		"commits": []map[string]any{{
			"id":      "0123456789abcdef",
			"message": "fix the thing\n\nlonger body",
			"url":     "https://github.com/o/r/commit/0123456",
			"author":  map[string]any{"name": "Ada", "username": "ada"},
		}},
		"repository": map[string]any{"full_name": "o/r", "html_url": "https://github.com/o/r"},
		"sender":     map[string]any{"login": "ada", "html_url": "https://github.com/ada"},
	})
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}

	resp := h.post(wh.URL+"/github", "application/json", body,
		map[string]string{"X-GitHub-Event": "push"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("github delivery: got %d (%s)", resp.StatusCode, bodyOf(t, resp))
	}

	event := watcher.waitEvent(protocol.EvMessageCreated)
	var payload protocol.MessageEvent
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if len(payload.Message.Embeds) != 1 {
		t.Fatalf("a push should render as one card: %+v", payload.Message.Embeds)
	}
	embed := payload.Message.Embeds[0]
	if embed.Title != "[o/r:main] 1 new commit" {
		t.Fatalf("title: got %q", embed.Title)
	}
	if !strings.Contains(embed.Description, "[`0123456`]") {
		t.Fatalf("the commit line is missing its short hash: %q", embed.Description)
	}
	if embed.Author == nil || embed.Author.Name != "ada" {
		t.Fatalf("the sender should be the card's author: %+v", embed.Author)
	}

	// An event with nothing worth drawing is accepted rather than refused: a
	// 4xx would show in the repository as a failed hook and earn a retry.
	quiet := h.post(wh.URL+"/github", "application/json", []byte(`{"action":"labeled"}`),
		map[string]string{"X-GitHub-Event": "pull_request"})
	if quiet.StatusCode != http.StatusNoContent {
		t.Fatalf("an undrawn event: got %d, want 204", quiet.StatusCode)
	}
}

func TestWebhookCarriesFiles(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) { cfg.Uploads.Path = t.TempDir() })
	_, _, wh := h.newWebhook("Reports")

	watcher := h.dial()
	watcher.guest("Watcher")

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("payload_json", `{"content":"nightly report"}`); err != nil {
		t.Fatalf("write field: %v", err)
	}
	part, err := form.CreateFormFile("files[0]", "report.txt")
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write([]byte("all green\n")); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := form.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}

	resp := h.post(wh.URL, form.FormDataContentType(), body.Bytes(), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("multipart delivery: got %d (%s)", resp.StatusCode, bodyOf(t, resp))
	}

	event := watcher.waitEvent(protocol.EvMessageCreated)
	var payload protocol.MessageEvent
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if payload.Message.Content != "nightly report" {
		t.Fatalf("content: got %q", payload.Message.Content)
	}
	if len(payload.Message.Attachments) != 1 || payload.Message.Attachments[0].Filename != "report.txt" {
		t.Fatalf("the file did not arrive with the message: %+v", payload.Message.Attachments)
	}

	// The file is served exactly as any other attachment is.
	file := h.get(payload.Message.Attachments[0].URL, "")
	if file.StatusCode != http.StatusOK {
		t.Fatalf("fetch attachment: got %d", file.StatusCode)
	}
	if got := bodyOf(t, file); got != "all green\n" {
		t.Fatalf("attachment body: got %q", got)
	}
}

func TestWebhookPublishesItsRateLimit(t *testing.T) {
	h := newHarness(t, nil)
	_, _, wh := h.newWebhook("Chatty")

	var last *http.Response
	for range 8 {
		last = h.postJSON(wh.URL, map[string]any{"content": "spam"})
		if last.StatusCode == http.StatusTooManyRequests {
			break
		}
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a burst should eventually be throttled, got %d", last.StatusCode)
	}
	if last.Header.Get("X-RateLimit-Limit") == "" || last.Header.Get("Retry-After") == "" {
		t.Fatalf("a throttled sender needs the headers to back off by: %v", last.Header)
	}

	var limited struct {
		Message    string  `json:"message"`
		RetryAfter float64 `json:"retry_after"`
		Global     bool    `json:"global"`
	}
	if err := json.NewDecoder(last.Body).Decode(&limited); err != nil {
		t.Fatalf("decode 429: %v", err)
	}
	if limited.RetryAfter <= 0 {
		t.Fatalf("retry_after: got %v, want a positive wait", limited.RetryAfter)
	}
	if limited.Global {
		t.Fatal("one webhook's bucket is not the global one")
	}
}

func TestWebhookManagementNeedsThePermission(t *testing.T) {
	h := newHarness(t, nil)
	admin, ready, wh := h.newWebhook("Private")
	channel := textChannel(t, ready)

	// A guest holds the default everyone mask, which does not carry
	// ManageWebhooks. They see no webhooks and can mint none.
	guest := h.dial()
	guest.guest("Nobody")

	list := ok[protocol.WebhookListResult](guest, protocol.OpWebhookList, protocol.WebhookListRequest{})
	if len(list.Webhooks) != 0 {
		t.Fatalf("a member without the permission must not see webhook tokens: %+v", list.Webhooks)
	}
	guest.fails(protocol.OpWebhookCreate,
		protocol.WebhookCreateRequest{ChannelID: channel.ID, Name: "mine"}, protocol.ErrForbidden)
	// One they cannot manage reports "not found", exactly as the channel would.
	guest.fails(protocol.OpWebhookDelete, protocol.WebhookDeleteRequest{WebhookID: wh.ID}, protocol.ErrNotFound)

	// Granting the bit on the everyone role is enough, and nothing else is.
	everyone := everyoneRole(t, ready)
	mask := permissions.DefaultEveryone | permissions.ManageWebhooks
	granted := mask.String()
	ok[protocol.RoleEvent](admin, protocol.OpRoleUpdate,
		protocol.RoleUpdateRequest{RoleID: everyone, Permissions: &granted})

	after := ok[protocol.WebhookListResult](guest, protocol.OpWebhookList, protocol.WebhookListRequest{})
	if len(after.Webhooks) != 1 || after.Webhooks[0].ID != wh.ID {
		t.Fatalf("the permission should reveal the channel's webhooks: %+v", after.Webhooks)
	}
}

func TestWebhookDeletionRevokesTheURLAndKeepsTheHistory(t *testing.T) {
	h := newHarness(t, nil)
	admin, ready, wh := h.newWebhook("Temporary")
	channel := textChannel(t, ready)

	if resp := h.postJSON(wh.URL, map[string]any{"content": "before"}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first delivery: got %d", resp.StatusCode)
	}
	ok[protocol.WebhookDeleteRequest](admin, protocol.OpWebhookDelete,
		protocol.WebhookDeleteRequest{WebhookID: wh.ID})

	if resp := h.postJSON(wh.URL, map[string]any{"content": "after"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a revoked URL: got %d, want 404", resp.StatusCode)
	}

	// What was said through it is still in the channel, still attributed.
	history := ok[protocol.MessageHistoryResult](admin, protocol.OpMessageHistory,
		protocol.MessageHistoryRequest{ChannelID: channel.ID})
	found := false
	for _, m := range history.Messages {
		if m.Content == "before" {
			found = true
			if m.Author != "Temporary" || m.Webhook == nil {
				t.Fatalf("deleting a webhook must not rewrite what it posted: %+v", m)
			}
		}
	}
	if !found {
		t.Fatal("the message posted before the deletion is gone")
	}
}
