package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// ErrNotFound is a webhook, or a message posted through one, that Discord no
// longer has. It is worth telling apart from every other failure: a relay that
// gets it back on an edit should stop trying rather than retry.
var ErrNotFound = errors.New("discord: not found")

// ErrUnauthorized is a webhook URL Discord refuses. Almost always a token that
// was rotated or a webhook somebody deleted in the Discord UI.
var ErrUnauthorized = errors.New("discord: webhook rejected")

// maxRetries bounds how many times one call is repeated after a 429. Discord
// says how long to wait and the relay honours it, but a bucket that never
// frees up must not hold a queue forever.
const maxRetries = 3

// maxRetryAfter caps the delay Discord asks for. A global rate limit can name
// a very long one, and waiting it out inside a request is worse than failing
// the delivery and letting the queue retry.
const maxRetryAfter = 30 * time.Second

// downloadHosts are the only places a relayed attachment is fetched from.
//
// The URL on a Discord attachment is somebody else's string arriving over the
// network, and this server will follow it and store what comes back. Pinning
// it to Discord's own CDN is what stops a crafted gateway payload from turning
// the relay into a fetcher for arbitrary addresses, including ones inside the
// network this server runs in.
var downloadHosts = map[string]bool{
	"cdn.discordapp.com":   true,
	"media.discordapp.net": true,
}

// REST is the HTTP half of a bot identity.
type REST struct {
	token  string
	client *http.Client
	log    *slog.Logger
	// base is the API root, overridable so a test can point it somewhere it
	// controls.
	base string
}

func newREST(token string, log *slog.Logger) *REST {
	return &REST{
		token: token,
		log:   log,
		base:  apiBase,
		client: &http.Client{
			Timeout: requestTimeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost:   4,
				ResponseHeaderTimeout: 20 * time.Second,
				IdleConnTimeout:       60 * time.Second,
			},
		},
	}
}

// httpHeader is what every request to Discord carries, the WebSocket handshake
// included.
func httpHeader() http.Header {
	h := http.Header{}
	h.Set("User-Agent", userAgent)
	return h
}

// --- webhook URLs -----------------------------------------------------------

// ParseWebhookURL pulls the id and token out of a webhook URL.
//
// An administrator pastes this from Discord's own settings screen, so every
// shape Discord has ever put in that box is accepted: with or without an
// explicit API version, on discord.com, discordapp.com or ptb/canary, and with
// whatever query string got copied along with it.
func ParseWebhookURL(raw string) (id, token string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", errors.New("the webhook URL is empty")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", "", errors.New("that is not a URL")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", "", errors.New("a webhook URL must be http or https")
	}
	host := strings.ToLower(parsed.Hostname())
	if !strings.HasSuffix(host, "discord.com") && !strings.HasSuffix(host, "discordapp.com") {
		return "", "", errors.New("that URL does not point at Discord")
	}

	// The path is /api/webhooks/{id}/{token}, optionally with a version
	// segment: /api/v10/webhooks/{id}/{token}.
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i, part := range parts {
		if part != "webhooks" {
			continue
		}
		if i+2 >= len(parts) {
			break
		}
		id, token = parts[i+1], parts[i+2]
		if id == "" || token == "" {
			break
		}
		if _, convErr := strconv.ParseUint(id, 10, 64); convErr != nil {
			return "", "", errors.New("the webhook id in that URL is not a number")
		}
		return id, token, nil
	}
	return "", "", errors.New("that URL is not a Discord webhook URL")
}

// WebhookInfo is what Discord answers with about a webhook, which is how the
// settings screen confirms a pasted URL works and shows which channel it
// posts into.
type WebhookInfo struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Avatar    *string `json:"avatar"`
	ChannelID string  `json:"channel_id"`
	GuildID   string  `json:"guild_id"`
}

// FetchWebhook reads a webhook back through its own URL. It needs no bot
// token: the token in the URL is the whole of its authentication, which is why
// this works even before a bot has been added anywhere.
func (r *REST) FetchWebhook(ctx context.Context, id, token string) (WebhookInfo, error) {
	var info WebhookInfo
	err := r.call(ctx, http.MethodGet, r.webhookPath(id, token), nil, "", &info, false)
	return info, err
}

// --- posting ----------------------------------------------------------------

// OutboundFile is one file uploaded alongside a relayed message.
type OutboundFile struct {
	Filename    string
	ContentType string
	Size        int64
	// Open yields the bytes. It is a function rather than a reader because a
	// delivery may be retried, and a reader that has already been drained
	// cannot be.
	Open func() (io.ReadCloser, error)
}

// OutboundMessage is what the relay posts into a Discord channel.
type OutboundMessage struct {
	Content string
	// Username and AvatarURL are the impersonation. They are what makes a
	// relayed message read as the person who wrote it rather than as the bot,
	// and they are per-message: one webhook carries every Aural member.
	Username  string
	AvatarURL string
	Embeds    []protocol.Embed
	Files     []OutboundFile
}

// allowedMentionsNone suppresses every ping a relayed message could raise.
//
// This is not a nicety. Content coming the other way is written by people on a
// server whose moderators are not Discord's, and an unfiltered relay hands any
// one of them @everyone on a server they are not even in. Aural mentions are
// plain text and would not resolve to a Discord id anyway, so nothing that
// should ping is lost by refusing all of it.
var allowedMentionsNone = map[string]any{"parse": []string{}}

// executeBody is the JSON half of a webhook delivery.
type executeBody struct {
	Content         string           `json:"content,omitempty"`
	Username        string           `json:"username,omitempty"`
	AvatarURL       string           `json:"avatar_url,omitempty"`
	Embeds          []protocol.Embed `json:"embeds,omitempty"`
	AllowedMentions any              `json:"allowed_mentions"`
	Attachments     []attachmentSpec `json:"attachments,omitempty"`
}

// attachmentSpec names an uploaded part so Discord binds it to a filename.
type attachmentSpec struct {
	ID       int    `json:"id"`
	Filename string `json:"filename"`
}

// Execute posts a message through a webhook and returns what Discord stored.
//
// The reply matters: its id is what an edit or a delete on the Aural side has
// to name later, so the call always waits for it rather than firing and
// forgetting.
func (r *REST) Execute(ctx context.Context, id, token string, msg OutboundMessage) (Message, error) {
	body := executeBody{
		Content:         msg.Content,
		Username:        msg.Username,
		AvatarURL:       msg.AvatarURL,
		Embeds:          msg.Embeds,
		AllowedMentions: allowedMentionsNone,
	}
	path := r.webhookPath(id, token) + "?wait=true"

	var out Message
	if len(msg.Files) == 0 {
		raw, err := json.Marshal(body)
		if err != nil {
			return Message{}, err
		}
		err = r.call(ctx, http.MethodPost, path, func() (io.Reader, error) {
			return bytes.NewReader(raw), nil
		}, "application/json", &out, false)
		return out, err
	}

	for i, f := range msg.Files {
		body.Attachments = append(body.Attachments, attachmentSpec{ID: i, Filename: f.Filename})
	}
	err := r.callMultipart(ctx, http.MethodPost, path, body, msg.Files, &out)
	return out, err
}

// Edit rewrites a message this relay posted. Discord replaces the whole of it,
// which matches how an edit reaches here.
func (r *REST) Edit(ctx context.Context, id, token, messageID string, msg OutboundMessage) error {
	// A username or avatar cannot be changed on an edit, and sending them is a
	// 400 rather than an ignored field.
	raw, err := json.Marshal(executeBody{
		Content:         msg.Content,
		Embeds:          msg.Embeds,
		AllowedMentions: allowedMentionsNone,
	})
	if err != nil {
		return err
	}
	return r.call(ctx, http.MethodPatch,
		r.webhookPath(id, token)+"/messages/"+url.PathEscape(messageID),
		func() (io.Reader, error) { return bytes.NewReader(raw), nil },
		"application/json", nil, false)
}

// Delete removes a message this relay posted.
func (r *REST) Delete(ctx context.Context, id, token, messageID string) error {
	return r.call(ctx, http.MethodDelete,
		r.webhookPath(id, token)+"/messages/"+url.PathEscape(messageID),
		nil, "", nil, false)
}

func (r *REST) webhookPath(id, token string) string {
	return r.base + "/webhooks/" + url.PathEscape(id) + "/" + url.PathEscape(token)
}

// --- downloading ------------------------------------------------------------

// Download opens a file on Discord's CDN, refusing anything larger than max
// and anything that is not on the CDN at all.
//
// The size is checked twice on purpose: once against the length Discord
// declares, which fails cheaply, and again by the caller as the bytes are
// copied, because a declared length is a claim rather than a fact.
func (r *REST) Download(ctx context.Context, rawURL string, max int64) (io.ReadCloser, int64, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, 0, errors.New("discord: unusable attachment URL")
	}
	if parsed.Scheme != "https" || !downloadHosts[strings.ToLower(parsed.Hostname())] {
		return nil, 0, fmt.Errorf("discord: refusing to fetch an attachment from %q", parsed.Host)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("discord: fetching an attachment answered %s", resp.Status)
	}
	if max > 0 && resp.ContentLength > max {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("discord: attachment is %d bytes, over the limit", resp.ContentLength)
	}
	return resp.Body, resp.ContentLength, nil
}

// --- the call itself --------------------------------------------------------

// bodyFunc yields a fresh request body. A retry after a 429 needs to send the
// same bytes again, and a reader that has been consumed cannot.
type bodyFunc func() (io.Reader, error)

// call makes one authenticated request, retrying a rate limit the number of
// times Discord asks to be retried.
//
// authOptional is for the webhook endpoints, which authenticate through the
// token in their own path. Sending a bot token alongside is harmless but means
// a webhook URL cannot be verified before a token is configured, which is
// exactly what the settings screen needs to do.
func (r *REST) call(ctx context.Context, method, endpoint string, body bodyFunc,
	contentType string, out any, authOptional bool) error {

	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
			var err error
			reader, err = body()
			if err != nil {
				return err
			}
		}

		req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", userAgent)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if r.token != "" && !authOptional {
			req.Header.Set("Authorization", "Bot "+r.token)
		}

		resp, err := r.client.Do(req)
		if err != nil {
			return err
		}

		retry, err := r.finish(resp, out, attempt)
		if retry > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retry):
			}
			continue
		}
		return err
	}
}

// callMultipart posts a body that carries files, in the shape Discord expects:
// a payload_json part and one files[n] part per file.
func (r *REST) callMultipart(ctx context.Context, method, endpoint string,
	payload executeBody, files []OutboundFile, out any) error {

	// The body is built into memory rather than streamed. A relayed message
	// carries at most a handful of files, each bounded by the configured
	// ceiling, and buffering is what makes a retry after a 429 possible at all.
	build := func() (io.Reader, string, error) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)

		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, "", err
		}
		if err := w.WriteField("payload_json", string(raw)); err != nil {
			return nil, "", err
		}

		for i, f := range files {
			src, err := f.Open()
			if err != nil {
				return nil, "", err
			}
			part, err := w.CreatePart(filePartHeader(i, f))
			if err != nil {
				src.Close()
				return nil, "", err
			}
			_, err = io.Copy(part, src)
			src.Close()
			if err != nil {
				return nil, "", err
			}
		}
		if err := w.Close(); err != nil {
			return nil, "", err
		}
		return &buf, w.FormDataContentType(), nil
	}

	reader, contentType, err := build()
	if err != nil {
		return err
	}
	buffered, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	return r.call(ctx, method, endpoint, func() (io.Reader, error) {
		return bytes.NewReader(buffered), nil
	}, contentType, out, false)
}

// filePartHeader names one uploaded file the way Discord binds it to the
// attachment entry of the same index.
func filePartHeader(index int, f OutboundFile) textproto.MIMEHeader {
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(
		`form-data; name="files[%d]"; filename=%q`, index, f.Filename))
	if f.ContentType != "" {
		h.Set("Content-Type", f.ContentType)
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	return h
}

// finish reads a response and reports how long to wait before repeating the
// request, or zero to stop.
func (r *REST) finish(resp *http.Response, out any, attempt int) (time.Duration, error) {
	defer resp.Body.Close()
	// Bounded: an error body from Discord is small, and one that is not is not
	// worth reading.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		if attempt >= maxRetries {
			return 0, fmt.Errorf("discord: rate limited after %d attempts", attempt+1)
		}
		wait := retryAfter(resp, raw)
		if wait > maxRetryAfter {
			return 0, fmt.Errorf("discord: rate limited for %s, giving up on this delivery", wait)
		}
		r.log.Debug("discord rate limited", slog.Duration("retry_after", wait))
		return wait, nil

	case resp.StatusCode == http.StatusNotFound:
		return 0, ErrNotFound

	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return 0, fmt.Errorf("%w: %s", ErrUnauthorized, describeError(raw))

	case resp.StatusCode >= 500:
		if attempt >= maxRetries {
			return 0, fmt.Errorf("discord: %s", resp.Status)
		}
		// Discord having a bad moment. A short flat wait, not the rate-limit
		// one: there is no bucket to respect here.
		return time.Duration(attempt+1) * time.Second, nil

	case resp.StatusCode >= 400:
		return 0, fmt.Errorf("discord: %s: %s", resp.Status, describeError(raw))
	}

	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return 0, fmt.Errorf("discord: decode reply: %w", err)
		}
	}
	return 0, nil
}

// retryAfter reads how long Discord wants to be left alone, preferring the
// JSON body's fractional seconds over the header's whole ones.
func retryAfter(resp *http.Response, raw []byte) time.Duration {
	var body struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && body.RetryAfter > 0 {
		return time.Duration(body.RetryAfter * float64(time.Second))
	}
	if header := resp.Header.Get("Retry-After"); header != "" {
		if seconds, err := strconv.ParseFloat(header, 64); err == nil && seconds > 0 {
			return time.Duration(seconds * float64(time.Second))
		}
	}
	return time.Second
}

// describeError pulls the human half out of a Discord error body, falling back
// to the body itself when it is not the shape this expects.
func describeError(raw []byte) string {
	var body struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && body.Message != "" {
		return body.Message
	}
	text := strings.TrimSpace(string(raw))
	if len(text) > 200 {
		text = text[:200]
	}
	if text == "" {
		return "no detail"
	}
	return text
}
