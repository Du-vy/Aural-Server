package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
	"github.com/aural-chat/aural-server/internal/uploads"
)

// The webhook API is mounted where Discord mounts its own.
//
// That is the whole feature. An operator who already has a service posting
// alerts, build results or alarms into a Discord channel changes one URL and
// nothing else: the path, the payload, the status codes, the error bodies and
// the rate-limit headers are all the ones that service is already written
// against.
const (
	webhookPrefix = "/api/webhooks/"

	// deliveryBurst and deliveriesPerSecond throttle one webhook. The refill
	// is Discord's own ceiling for a channel — thirty messages a minute — and
	// the burst is what a deployment finishing five stages at once sends.
	deliveryBurst       = 5
	deliveriesPerSecond = 0.5

	// maxWebhookJSONBytes bounds a delivery with no files in it. A message is
	// two thousand characters and ten cards; a megabyte is far past anything
	// that is not a mistake.
	maxWebhookJSONBytes = 1 << 20
	// maxWebhookPayloadPart bounds the payload_json part of a multipart
	// delivery, which carries the same thing.
	maxWebhookPayloadPart = maxWebhookJSONBytes
	// maxWebhookParts bounds how many parts one multipart body may hold, so a
	// body of empty parts cannot be used to spin the parser.
	maxWebhookParts = 64
)

// stripAPIVersion rewrites an explicitly versioned path into the bare one.
//
// Discord serves the same API at /api/webhooks/... and at /api/v10/webhooks/...
// and some tools rewrite a pasted URL into the second shape. Normalising here
// rather than registering both is what keeps the routing table unambiguous:
// /api/{version}/webhooks/{id}/{token} and /api/webhooks/{id}/{token}/{format}
// have the same number of segments, and no mux can tell them apart.
func stripAPIVersion(next http.Handler) http.Handler {
	const apiPrefix = "/api/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, found := strings.CutPrefix(r.URL.Path, apiPrefix)
		if !found {
			next.ServeHTTP(w, r)
			return
		}
		segment, tail, found := strings.Cut(rest, "/")
		if !found || len(segment) < 2 || segment[0] != 'v' {
			next.ServeHTTP(w, r)
			return
		}
		if _, err := strconv.Atoi(segment[1:]); err != nil {
			next.ServeHTTP(w, r)
			return
		}
		// The request is rewritten on a copy: the original URL belongs to the
		// caller of ServeHTTP, and anything else reading it — a log line, a
		// later handler — should still see what was asked for.
		trimmed := *r
		url := *r.URL
		url.Path = apiPrefix + tail
		trimmed.URL = &url
		next.ServeHTTP(w, &trimmed)
	})
}

// registerWebhookRoutes mounts the Discord-shaped API.
func (s *Server) registerWebhookRoutes(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("POST "+prefix+"{id}/{token}", s.handleWebhookExecute)
	mux.HandleFunc("GET "+prefix+"{id}/{token}", s.handleWebhookFetch)
	mux.HandleFunc("PATCH "+prefix+"{id}/{token}", s.handleWebhookModify)
	mux.HandleFunc("DELETE "+prefix+"{id}/{token}", s.handleWebhookRevoke)
	mux.HandleFunc("OPTIONS "+prefix+"{id}/{token}", s.handleWebhookPreflight)

	// The two payload dialects Discord also accepts on a webhook URL, so a
	// service that only speaks one of them needs no adapter either.
	mux.HandleFunc("POST "+prefix+"{id}/{token}/{format}", s.handleWebhookExecute)
	mux.HandleFunc("OPTIONS "+prefix+"{id}/{token}/{format}", s.handleWebhookPreflight)

	mux.HandleFunc("GET "+prefix+"{id}/{token}/messages/{messageId}", s.handleWebhookMessageFetch)
	mux.HandleFunc("PATCH "+prefix+"{id}/{token}/messages/{messageId}", s.handleWebhookMessageEdit)
	mux.HandleFunc("DELETE "+prefix+"{id}/{token}/messages/{messageId}", s.handleWebhookMessageDelete)
	mux.HandleFunc("OPTIONS "+prefix+"{id}/{token}/messages/{messageId}", s.handleWebhookPreflight)
}

// --- responses --------------------------------------------------------------

// applyWebhookCORS opens these routes to any origin.
//
// Unlike the rest of the HTTP surface, a webhook carries its own credential in
// its path and reads no cookie and no session, so the browser's origin has
// nothing to do with whether a delivery is allowed. Discord answers the same
// way, which is what lets a page-based automation tool post from anywhere.
func applyWebhookCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-GitHub-Event, X-Hub-Signature-256")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Max-Age", "600")
}

func (s *Server) handleWebhookPreflight(w http.ResponseWriter, _ *http.Request) {
	applyWebhookCORS(w)
	w.WriteHeader(http.StatusNoContent)
}

// writeDiscordError answers in the shape every Discord client library parses.
func writeDiscordError(w http.ResponseWriter, status, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(discordError{Message: message, Code: code})
}

// writeRateLimitHeaders publishes the bucket a sender should pace itself by.
func writeRateLimitHeaders(w http.ResponseWriter, webhookID int64, remaining int, retryAfter float64) {
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(deliveryBurst))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Reset-After", strconv.FormatFloat(retryAfter, 'f', 3, 64))
	h.Set("X-RateLimit-Reset",
		strconv.FormatFloat(float64(time.Now().UnixNano())/1e9+retryAfter, 'f', 3, 64))
	// The bucket a client groups its own accounting by. One webhook is one
	// bucket here, which is exactly what it is.
	h.Set("X-RateLimit-Bucket", "webhook-"+snowflake(webhookID))
}

// --- resolution -------------------------------------------------------------

// resolveWebhook reads the id and token out of the path and finds the webhook
// they name.
//
// Both halves are required and both failures answer the same way Discord's do,
// because a sender distinguishes "this webhook was deleted" from "this token is
// wrong" and reacts to them differently.
func (s *Server) resolveWebhook(r *http.Request) (store.Webhook, int, int, string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return store.Webhook{}, http.StatusNotFound, codeUnknownWebhook, "Unknown Webhook"
	}
	token := r.PathValue("token")
	if token == "" {
		return store.Webhook{}, http.StatusUnauthorized, codeInvalidToken, "Invalid Webhook Token Provided"
	}

	wh, err := s.st.WebhookByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Webhook{}, http.StatusNotFound, codeUnknownWebhook, "Unknown Webhook"
	}
	if err != nil {
		s.log.Error("read webhook", slog.Int64("webhook", id), slog.Any("error", err))
		return store.Webhook{}, http.StatusInternalServerError, 0, "500: Internal Server Error"
	}
	if subtle.ConstantTimeCompare([]byte(wh.Token), []byte(token)) != 1 {
		return store.Webhook{}, http.StatusUnauthorized, codeInvalidToken, "Invalid Webhook Token Provided"
	}
	return wh, 0, 0, ""
}

// webhookChannel checks that the channel a webhook points at can still carry
// what is being posted to it.
func (s *Server) webhookChannel(wh store.Webhook) (store.Channel, bool) {
	channel, ok := s.hub.Channel(wh.ChannelID)
	if !ok || channel.Type != protocol.ChannelText {
		return store.Channel{}, false
	}
	return channel, true
}

// --- the webhook object -----------------------------------------------------

// handleWebhookFetch answers with the webhook itself, which is what a client
// library calls first to learn the channel it is posting into.
func (s *Server) handleWebhookFetch(w http.ResponseWriter, r *http.Request) {
	applyWebhookCORS(w)
	wh, status, code, message := s.resolveWebhook(r)
	if status != 0 {
		writeDiscordError(w, status, code, message)
		return
	}
	writeJSON(w, http.StatusOK, s.discordWebhookView(r, wh))
}

// handleWebhookModify renames a webhook or repoints its picture through the URL
// itself, which is how a client library that only holds the URL edits one.
func (s *Server) handleWebhookModify(w http.ResponseWriter, r *http.Request) {
	applyWebhookCORS(w)
	wh, status, code, message := s.resolveWebhook(r)
	if status != 0 {
		writeDiscordError(w, status, code, message)
		return
	}

	var patch struct {
		Name   *string `json:"name"`
		Avatar *string `json:"avatar"`
	}
	body := http.MaxBytesReader(w, r.Body, maxWebhookJSONBytes)
	if err := json.NewDecoder(body).Decode(&patch); err != nil && !errors.Is(err, io.EOF) {
		writeDiscordError(w, http.StatusBadRequest, codeInvalidFormBody, "Invalid Form Body")
		return
	}

	if patch.Name != nil {
		name, failure := validateWebhookName(*patch.Name)
		if failure != nil {
			writeDiscordError(w, http.StatusBadRequest, codeInvalidFormBody, failure.Message)
			return
		}
		wh.Name = name
	}
	if patch.Avatar != nil {
		// The token-authenticated route only accepts a URL, never a data URI:
		// this server hosts no picture for a webhook, and one arriving as
		// base64 has nowhere to be stored that the quota would account for.
		if href := safeURL(*patch.Avatar); href != "" {
			wh.Avatar = &href
		} else {
			wh.Avatar = nil
		}
	}

	if err := s.st.UpdateWebhook(r.Context(), wh); err != nil {
		s.log.Error("update webhook", slog.Int64("webhook", wh.ID), slog.Any("error", err))
		writeDiscordError(w, http.StatusInternalServerError, 0, "500: Internal Server Error")
		return
	}
	writeJSON(w, http.StatusOK, s.discordWebhookView(r, wh))
}

// handleWebhookRevoke deletes a webhook through its own URL, which is how an
// integration tears itself down when it is uninstalled.
func (s *Server) handleWebhookRevoke(w http.ResponseWriter, r *http.Request) {
	applyWebhookCORS(w)
	wh, status, code, message := s.resolveWebhook(r)
	if status != 0 {
		writeDiscordError(w, status, code, message)
		return
	}
	if err := s.st.DeleteWebhook(r.Context(), wh.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Error("delete webhook", slog.Int64("webhook", wh.ID), slog.Any("error", err))
		writeDiscordError(w, http.StatusInternalServerError, 0, "500: Internal Server Error")
		return
	}
	s.log.Info("webhook deleted through its own URL", slog.Int64("webhook", wh.ID))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) discordWebhookView(r *http.Request, wh store.Webhook) discordWebhook {
	return discordWebhook{
		ID:        snowflake(wh.ID),
		Type:      1,
		Name:      wh.Name,
		Avatar:    wh.Avatar,
		ChannelID: snowflake(wh.ChannelID),
		// This server is one guild, so the id is a constant. It is present
		// because client libraries read it, not because it distinguishes
		// anything.
		GuildID:       "1",
		ApplicationID: nil,
		Token:         wh.Token,
		URL:           absoluteURL(r, webhookPath(wh.ID, wh.Token)),
	}
}

// --- executing --------------------------------------------------------------

// handleWebhookExecute posts one message through a webhook.
//
//	POST /api/webhooks/{id}/{token}[/github|/slack][?wait=true]
//
// The body is either JSON, or multipart/form-data with a payload_json part and
// one part per file. Both are what Discord accepts, in the same fields.
func (s *Server) handleWebhookExecute(w http.ResponseWriter, r *http.Request) {
	applyWebhookCORS(w)

	wh, status, code, message := s.resolveWebhook(r)
	if status != 0 {
		writeDiscordError(w, status, code, message)
		return
	}

	format := r.PathValue("format")
	switch format {
	case "", "github", "slack":
	default:
		writeDiscordError(w, http.StatusNotFound, codeUnknownWebhook, "404: Not Found")
		return
	}

	if _, ok := s.webhookChannel(wh); !ok {
		// The channel is gone or is no longer one that carries messages. The
		// webhook is a dangling URL rather than a working one, and saying so
		// is what stops a sender retrying forever.
		writeDiscordError(w, http.StatusNotFound, codeUnknownWebhook, "Unknown Channel")
		return
	}

	allowed, remaining, retryAfter := s.deliveries.take(wh.ID)
	writeRateLimitHeaders(w, wh.ID, remaining, retryAfter)
	if !allowed {
		w.Header().Set("Retry-After", strconv.FormatFloat(retryAfter, 'f', 3, 64))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(discordRateLimited{
			Message:    "You are being rate limited.",
			RetryAfter: retryAfter,
			Global:     false,
		})
		return
	}

	payload, files, failure := s.readDelivery(w, r, wh, format)
	if failure != nil {
		s.discardUploads(files)
		if failure == errDeliveryIgnored {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeDiscordError(w, failure.status, failure.code, failure.message)
		return
	}

	view, err := s.postWebhookMessage(r.Context(), wh, payload, files)
	if err != nil {
		s.discardUploads(files)
		var failed *deliveryFailure
		if errors.As(err, &failed) {
			writeDiscordError(w, failed.status, failed.code, failed.message)
			return
		}
		s.log.Error("deliver webhook message",
			slog.Int64("webhook", wh.ID), slog.Any("error", err))
		writeDiscordError(w, http.StatusInternalServerError, 0, "500: Internal Server Error")
		return
	}

	// Slack's own endpoint answers with a bare "ok", and the tools written
	// against it check for exactly that.
	if format == "slack" && r.URL.Query().Get("wait") != "true" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}
	if r.URL.Query().Get("wait") == "true" {
		writeJSON(w, http.StatusOK, discordMessageView(r, view, wh))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deliveryFailure is a refusal that already knows how it should read to the
// sender, so the layers between the parser and the writer do not have to.
type deliveryFailure struct {
	status  int
	code    int
	message string
}

func (d *deliveryFailure) Error() string { return d.message }

func deliveryError(status, code int, message string) *deliveryFailure {
	return &deliveryFailure{status: status, code: code, message: message}
}

// errDeliveryIgnored is a delivery that was understood and deliberately not
// drawn: a GitHub event this server renders nothing for. It is not a failure —
// answering with one would mark the hook red in the sender's settings and earn
// a retry — so it is answered exactly as a delivered message with no body is.
var errDeliveryIgnored = deliveryError(http.StatusNoContent, 0, "")

// savedUpload is a file that reached the disk but has no row yet. Holding the
// pair is what lets a delivery that fails afterwards give the bytes back.
type savedUpload struct {
	key      string
	size     int64
	filename string
	width    *int
	height   *int
}

// discardUploads returns the bytes of a delivery that did not become a message.
func (s *Server) discardUploads(files []savedUpload) {
	saved := s.hub.Files()
	if saved == nil {
		return
	}
	for _, f := range files {
		saved.Remove(f.key, f.size)
	}
}

// readDelivery parses a body in whichever of the three dialects it arrived in.
func (s *Server) readDelivery(w http.ResponseWriter, r *http.Request, wh store.Webhook, format string) (executePayload, []savedUpload, *deliveryFailure) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		// A sender that names no content type is taken at the word of its
		// body, which for every one of these is JSON.
		mediaType = "application/json"
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		if format != "" {
			return executePayload{}, nil, deliveryError(http.StatusBadRequest, codeInvalidFormBody,
				"Invalid Form Body")
		}
		return s.readMultipartDelivery(r, wh)
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookJSONBytes))
	if err != nil {
		return executePayload{}, nil, deliveryError(http.StatusRequestEntityTooLarge,
			codeRequestTooLarge, "Request entity too large")
	}

	switch format {
	case "github":
		payload, failure := githubPayload(r, body)
		return payload, nil, failure
	case "slack":
		payload, failure := slackPayload(body)
		return payload, nil, failure
	}

	var payload executePayload
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			return executePayload{}, nil, deliveryError(http.StatusBadRequest,
				codeInvalidFormBody, "Invalid Form Body")
		}
	}
	return payload, nil, nil
}

// readMultipartDelivery streams a body that carries files alongside its JSON.
//
// The parts are handled in the order they arrive rather than buffered, and the
// JSON may come before or after the files: libraries disagree about which, and
// a delivery must not depend on the one this server happened to expect.
func (s *Server) readMultipartDelivery(r *http.Request, wh store.Webhook) (executePayload, []savedUpload, *deliveryFailure) {
	reader, err := r.MultipartReader()
	if err != nil {
		return executePayload{}, nil, deliveryError(http.StatusBadRequest, codeInvalidFormBody,
			"Invalid Form Body")
	}

	var (
		payload executePayload
		saved   []savedUpload
	)
	files := s.hub.Files()
	maxFiles := s.cfg.Uploads.MaxPerMessage

	for parts := 0; parts < maxWebhookParts; parts++ {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return payload, saved, nil
		}
		if err != nil {
			return payload, saved, deliveryError(http.StatusBadRequest, codeInvalidFormBody,
				"Invalid Form Body")
		}

		if part.FileName() == "" {
			// The JSON half. Discord names it payload_json; a few senders use
			// the bare "payload" Slack once did.
			name := part.FormName()
			if name != "payload_json" && name != "payload" {
				part.Close()
				continue
			}
			raw, err := io.ReadAll(io.LimitReader(part, maxWebhookPayloadPart))
			part.Close()
			if err != nil {
				return payload, saved, deliveryError(http.StatusBadRequest, codeInvalidFormBody,
					"Invalid Form Body")
			}
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &payload); err != nil {
					return payload, saved, deliveryError(http.StatusBadRequest, codeInvalidFormBody,
						"Invalid Form Body")
				}
			}
			continue
		}

		if files == nil {
			part.Close()
			return payload, saved, deliveryError(http.StatusForbidden, codeMissingAccess,
				"This server does not accept file uploads")
		}
		if len(saved) >= maxFiles {
			part.Close()
			return payload, saved, deliveryError(http.StatusBadRequest, codeInvalidFormBody,
				fmt.Sprintf("A message may carry at most %d files", maxFiles))
		}

		filename := cleanFilename(part.FileName())
		if filename == "" {
			part.Close()
			continue
		}
		written, err := files.Save(part, -1)
		part.Close()
		switch {
		case errors.Is(err, uploads.ErrTooLarge):
			return payload, saved, deliveryError(http.StatusRequestEntityTooLarge, codeRequestTooLarge,
				fmt.Sprintf("Files on this server may be at most %s", humanBytes(files.MaxFileBytes())))
		case errors.Is(err, uploads.ErrQuotaExceeded):
			return payload, saved, deliveryError(http.StatusInsufficientStorage, codeRequestTooLarge,
				"This server has no storage left for new files")
		case err != nil:
			s.log.Error("store webhook upload", slog.Int64("webhook", wh.ID), slog.Any("error", err))
			return payload, saved, deliveryError(http.StatusInternalServerError, 0,
				"500: Internal Server Error")
		}

		upload := savedUpload{key: written.Key, size: written.Size, filename: filename}
		if path, err := files.Path(written.Key); err == nil {
			if width, height, ok := uploads.Dimensions(path); ok {
				upload.width, upload.height = &width, &height
			}
		}
		saved = append(saved, upload)
	}
	return payload, saved, deliveryError(http.StatusBadRequest, codeInvalidFormBody,
		"Invalid Form Body")
}

// postWebhookMessage turns a parsed delivery into a message everybody in the
// channel is told about.
func (s *Server) postWebhookMessage(ctx context.Context, wh store.Webhook, payload executePayload, files []savedUpload) (protocol.Message, error) {
	content := cleanMessage(payload.Content)
	if utf8.RuneCountInString(content) > maxMessageRunes {
		return protocol.Message{}, deliveryError(http.StatusBadRequest, codeInvalidFormBody,
			fmt.Sprintf("Must be %d or fewer in length.", maxMessageRunes))
	}
	embeds := sanitiseEmbeds(payload.Embeds)
	if content == "" && len(embeds) == 0 && len(files) == 0 {
		return protocol.Message{}, deliveryError(http.StatusBadRequest, codeEmptyMessage,
			"Cannot send an empty message")
	}

	// The name and picture of one delivery. A payload that overrides them
	// overrides them for this message only: the webhook keeps its own, which
	// is what lets one URL post as several different senders.
	author := wh.Name
	if override := cleanText(payload.Username); override != "" {
		author = truncateRunes(override, maxWebhookUsername)
	}
	avatar := wh.Avatar
	if override := safeURL(payload.AvatarURL); override != "" {
		avatar = &override
	}

	encoded, err := embedsJSON(embeds)
	if err != nil {
		return protocol.Message{}, err
	}

	created, err := s.st.CreateWebhookMessage(ctx, store.Message{
		ChannelID:     wh.ChannelID,
		Author:        author,
		Content:       content,
		WebhookID:     &wh.ID,
		WebhookAvatar: avatar,
		Embeds:        encoded,
	})
	if err != nil {
		return protocol.Message{}, err
	}

	attachments, err := s.recordWebhookFiles(ctx, wh, created.ID, files)
	if err != nil {
		// The message is only the message that was sent once its files are on
		// it, so a half-written one is rolled back rather than posted.
		if delErr := s.st.DeleteMessage(ctx, created.ID); delErr != nil {
			s.log.Error("roll back a webhook message whose files could not be attached",
				slog.Int64("message", created.ID), slog.Any("error", delErr))
		}
		return protocol.Message{}, err
	}

	if err := s.st.TouchWebhook(ctx, wh.ID); err != nil {
		// A timestamp, not the message. It is logged and let go.
		s.log.Debug("record webhook use", slog.Int64("webhook", wh.ID), slog.Any("error", err))
	}

	var replyTo *protocol.ReferencedMessage
	if created.ReplyToID != nil {
		if target, err := s.st.MessageByID(ctx, *created.ReplyToID); err == nil {
			replyTo = referencedMessageView(&target, *created.ReplyToID)
		} else {
			replyTo = referencedMessageView(nil, *created.ReplyToID)
		}
	}

	view := messageView(created, attachments, replyTo)
	s.hub.BroadcastChannelEvent(
		protocol.Event(protocol.EvMessageCreated, protocol.MessageEvent{Message: view}),
		created.ChannelID)
	// A webhook delivery into a bridged channel crosses like anything else: an
	// alert worth posting here is worth posting to the people still reading
	// the other side. The relay drops the one case that would loop — its own
	// inbound webhook — by the tag on the row rather than by a check here.
	s.hub.relayMessage(created, attachments)

	s.log.Info("webhook message delivered",
		slog.Int64("webhook", wh.ID), slog.Int64("channel", wh.ChannelID),
		slog.Int("embeds", len(embeds)), slog.Int("files", len(files)))

	return view, nil
}

// recordWebhookFiles binds the files of a delivery to the message that carries
// them. They are written already attached: a webhook has no session to hold a
// pending upload in between.
func (s *Server) recordWebhookFiles(ctx context.Context, wh store.Webhook, messageID int64, files []savedUpload) ([]store.Attachment, error) {
	if len(files) == 0 {
		return nil, nil
	}
	out := make([]store.Attachment, 0, len(files))
	for _, f := range files {
		created, err := s.st.CreatePostedAttachment(ctx, store.Attachment{
			MessageID:   &messageID,
			ChannelID:   wh.ChannelID,
			StorageKey:  f.key,
			Filename:    f.filename,
			ContentType: uploads.ContentType(f.filename),
			Size:        f.size,
			Width:       f.width,
			Height:      f.height,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, created)
	}
	return out, nil
}

// --- messages a webhook posted ---------------------------------------------

// loadWebhookMessage reads a message this webhook posted, and only one.
//
// A webhook may edit and delete what it said and nothing else: the URL is not
// a moderation credential, and a message somebody typed is not its to touch.
func (s *Server) loadWebhookMessage(r *http.Request, wh store.Webhook) (store.Message, *deliveryFailure) {
	id, err := strconv.ParseInt(r.PathValue("messageId"), 10, 64)
	if err != nil || id <= 0 {
		return store.Message{}, deliveryError(http.StatusNotFound, codeUnknownMessage, "Unknown Message")
	}
	message, err := s.st.MessageByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Message{}, deliveryError(http.StatusNotFound, codeUnknownMessage, "Unknown Message")
	}
	if err != nil {
		s.log.Error("read webhook message", slog.Int64("message", id), slog.Any("error", err))
		return store.Message{}, deliveryError(http.StatusInternalServerError, 0, "500: Internal Server Error")
	}
	if message.WebhookID == nil || *message.WebhookID != wh.ID {
		return store.Message{}, deliveryError(http.StatusNotFound, codeUnknownMessage, "Unknown Message")
	}
	return message, nil
}

func (s *Server) handleWebhookMessageFetch(w http.ResponseWriter, r *http.Request) {
	applyWebhookCORS(w)
	wh, status, code, message := s.resolveWebhook(r)
	if status != 0 {
		writeDiscordError(w, status, code, message)
		return
	}
	existing, failure := s.loadWebhookMessage(r, wh)
	if failure != nil {
		writeDiscordError(w, failure.status, failure.code, failure.message)
		return
	}
	attachments, err := s.st.AttachmentsForMessage(r.Context(), existing.ID)
	if err != nil {
		writeDiscordError(w, http.StatusInternalServerError, 0, "500: Internal Server Error")
		return
	}
	var replyTo *protocol.ReferencedMessage
	if existing.ReplyToID != nil {
		if target, err := s.st.MessageByID(r.Context(), *existing.ReplyToID); err == nil {
			replyTo = referencedMessageView(&target, *existing.ReplyToID)
		} else {
			replyTo = referencedMessageView(nil, *existing.ReplyToID)
		}
	}
	writeJSON(w, http.StatusOK, discordMessageView(r, messageView(existing, attachments, replyTo), wh))
}

// handleWebhookMessageEdit rewrites what a webhook posted.
//
// It is what a status page or a long-running job uses to keep one message
// current instead of posting a hundred, and it is the reason the execute
// endpoint bothers to answer with an id.
func (s *Server) handleWebhookMessageEdit(w http.ResponseWriter, r *http.Request) {
	applyWebhookCORS(w)
	wh, status, code, message := s.resolveWebhook(r)
	if status != 0 {
		writeDiscordError(w, status, code, message)
		return
	}
	existing, failure := s.loadWebhookMessage(r, wh)
	if failure != nil {
		writeDiscordError(w, failure.status, failure.code, failure.message)
		return
	}

	allowed, remaining, retryAfter := s.deliveries.take(wh.ID)
	writeRateLimitHeaders(w, wh.ID, remaining, retryAfter)
	if !allowed {
		w.Header().Set("Retry-After", strconv.FormatFloat(retryAfter, 'f', 3, 64))
		writeJSON(w, http.StatusTooManyRequests, discordRateLimited{
			Message: "You are being rate limited.", RetryAfter: retryAfter,
		})
		return
	}

	// An edit through this endpoint is a patch: a field that is absent is left
	// as it was, which is how a sender updates only the embed of a message it
	// posted with words as well.
	var patch struct {
		Content *string           `json:"content"`
		Embeds  *[]protocol.Embed `json:"embeds"`
	}
	body := http.MaxBytesReader(w, r.Body, maxWebhookJSONBytes)
	if err := json.NewDecoder(body).Decode(&patch); err != nil && !errors.Is(err, io.EOF) {
		writeDiscordError(w, http.StatusBadRequest, codeInvalidFormBody, "Invalid Form Body")
		return
	}

	content := existing.Content
	if patch.Content != nil {
		content = cleanMessage(*patch.Content)
		if utf8.RuneCountInString(content) > maxMessageRunes {
			writeDiscordError(w, http.StatusBadRequest, codeInvalidFormBody,
				fmt.Sprintf("Must be %d or fewer in length.", maxMessageRunes))
			return
		}
	}
	encoded := existing.Embeds
	embedCount := len(decodeEmbeds(existing.Embeds))
	if patch.Embeds != nil {
		embeds := sanitiseEmbeds(*patch.Embeds)
		next, err := embedsJSON(embeds)
		if err != nil {
			writeDiscordError(w, http.StatusInternalServerError, 0, "500: Internal Server Error")
			return
		}
		encoded, embedCount = next, len(embeds)
	}

	attachments, err := s.st.AttachmentsForMessage(r.Context(), existing.ID)
	if err != nil {
		writeDiscordError(w, http.StatusInternalServerError, 0, "500: Internal Server Error")
		return
	}
	if content == "" && embedCount == 0 && len(attachments) == 0 {
		writeDiscordError(w, http.StatusBadRequest, codeEmptyMessage, "Cannot send an empty message")
		return
	}

	updated, err := s.st.UpdateWebhookMessage(r.Context(), existing.ID, content, encoded)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeDiscordError(w, http.StatusNotFound, codeUnknownMessage, "Unknown Message")
			return
		}
		s.log.Error("edit webhook message", slog.Int64("message", existing.ID), slog.Any("error", err))
		writeDiscordError(w, http.StatusInternalServerError, 0, "500: Internal Server Error")
		return
	}

	var replyTo *protocol.ReferencedMessage
	if updated.ReplyToID != nil {
		if target, err := s.st.MessageByID(r.Context(), *updated.ReplyToID); err == nil {
			replyTo = referencedMessageView(&target, *updated.ReplyToID)
		} else {
			replyTo = referencedMessageView(nil, *updated.ReplyToID)
		}
	}

	view := messageView(updated, attachments, replyTo)
	s.hub.BroadcastChannelEvent(
		protocol.Event(protocol.EvMessageUpdated, protocol.MessageEvent{Message: view}),
		updated.ChannelID)

	writeJSON(w, http.StatusOK, discordMessageView(r, view, wh))
}

// handleWebhookMessageDelete removes what a webhook posted, and the files it
// carried with it.
func (s *Server) handleWebhookMessageDelete(w http.ResponseWriter, r *http.Request) {
	applyWebhookCORS(w)
	wh, status, code, message := s.resolveWebhook(r)
	if status != 0 {
		writeDiscordError(w, status, code, message)
		return
	}
	existing, failure := s.loadWebhookMessage(r, wh)
	if failure != nil {
		writeDiscordError(w, failure.status, failure.code, failure.message)
		return
	}

	// Read before the delete: the rows go with the message through the
	// cascade, so what it held has to be known while it still exists.
	attachments, err := s.st.AttachmentsForMessage(r.Context(), existing.ID)
	if err != nil {
		writeDiscordError(w, http.StatusInternalServerError, 0, "500: Internal Server Error")
		return
	}
	if err := s.st.DeleteMessage(r.Context(), existing.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeDiscordError(w, http.StatusNotFound, codeUnknownMessage, "Unknown Message")
			return
		}
		s.log.Error("delete webhook message", slog.Int64("message", existing.ID), slog.Any("error", err))
		writeDiscordError(w, http.StatusInternalServerError, 0, "500: Internal Server Error")
		return
	}
	s.hub.RemoveFiles(attachments)

	s.hub.BroadcastChannelEvent(protocol.Event(protocol.EvMessageDeleted,
		protocol.MessageDeletedEvent{MessageID: existing.ID, ChannelID: existing.ChannelID}),
		existing.ChannelID)

	w.WriteHeader(http.StatusNoContent)
}

// --- rendering back to the sender -------------------------------------------

// absoluteURL turns a path this server serves into the address the caller
// reached it by, which is the only address known to work from where they are.
func absoluteURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}

// discordMessageView renders a stored message the way Discord reports one.
func discordMessageView(r *http.Request, m protocol.Message, wh store.Webhook) discordMessage {
	out := discordMessage{
		ID:        snowflake(m.ID),
		Type:      0,
		ChannelID: snowflake(m.ChannelID),
		Content:   m.Content,
		Author: discordUser{
			ID:            snowflake(wh.ID),
			Username:      m.Author,
			Discriminator: "0000",
			Bot:           true,
		},
		Attachments:  []discordAttachment{},
		Embeds:       m.Embeds,
		Timestamp:    time.Unix(m.CreatedAt, 0).UTC().Format(time.RFC3339),
		Mentions:     []discordUser{},
		MentionRoles: []string{},
		WebhookID:    snowflake(wh.ID),
	}
	if out.Embeds == nil {
		out.Embeds = []protocol.Embed{}
	}
	if m.Webhook != nil {
		out.Author.Avatar = m.Webhook.Avatar
	}
	if m.EditedAt != nil {
		edited := time.Unix(*m.EditedAt, 0).UTC().Format(time.RFC3339)
		out.EditedTimestamp = &edited
	}
	for _, a := range m.Attachments {
		href := absoluteURL(r, a.URL)
		size, _ := strconv.ParseInt(a.Size, 10, 64)
		out.Attachments = append(out.Attachments, discordAttachment{
			ID:          snowflake(a.ID),
			Filename:    a.Filename,
			Size:        size,
			URL:         href,
			ProxyURL:    href,
			ContentType: a.ContentType,
			Width:       a.Width,
			Height:      a.Height,
		})
	}
	return out
}
