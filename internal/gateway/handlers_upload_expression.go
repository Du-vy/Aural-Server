package gateway

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
	"github.com/aural-chat/aural-server/internal/uploads"
)

// The soundboard accepts one format, and that is what makes its length limit
// enforceable rather than merely declared: the client re-encodes whatever it
// was handed, and a RIFF header can be read exactly. Accepting an MP3 would
// mean either shipping a decoder or believing a number the uploader chose.
const soundExtension = ".wav"

// handleEmojiUpload adds a custom emoji.
//
//	POST /upload/emoji?name=<name>
//	Authorization: Bearer <session token>
//	Content-Type: multipart/form-data, one part named "file"
func (s *Server) handleEmojiUpload(w http.ResponseWriter, r *http.Request) {
	s.uploadExpression(w, r, store.KindEmoji)
}

// handleStickerUpload adds a custom sticker.
func (s *Server) handleStickerUpload(w http.ResponseWriter, r *http.Request) {
	s.uploadExpression(w, r, store.KindSticker)
}

// uploadExpression is both of the above. They differ in one ceiling, one row
// and one word in a message; everything else — the permission, the format
// check, the quota, the broadcast — is the same sequence, and writing it twice
// is how the two drift apart.
func (s *Server) uploadExpression(w http.ResponseWriter, r *http.Request, kind string) {
	s.applyCORS(w, r)

	files := s.hub.Files()
	if files == nil {
		writeAPIError(w, http.StatusForbidden, protocol.ErrUploadsDisabled,
			"this server does not accept file uploads")
		return
	}

	user, failure := s.authenticateRequest(r)
	if failure != nil {
		writeProtocolError(w, failure)
		return
	}
	base, _, err := s.hub.UserPermissions(r.Context(), user)
	if err != nil {
		s.log.Error("resolve uploader permissions", slog.Any("error", err))
		writeAPIError(w, http.StatusInternalServerError, protocol.ErrInternal, "could not check permissions")
		return
	}
	if !base.Has(permissions.ManageExpressions) {
		writeAPIError(w, http.StatusForbidden, protocol.ErrForbidden,
			"you are not allowed to manage this server's emoji")
		return
	}

	name, failure := validateExpressionName(r.URL.Query().Get("name"))
	if failure != nil {
		writeProtocolError(w, failure)
		return
	}

	limit, maxBytes := s.hub.cfg.Expressions.MaxEmojis, s.hub.cfg.Expressions.MaxEmojiBytes
	if kind == store.KindSticker {
		limit, maxBytes = s.hub.cfg.Expressions.MaxStickers, s.hub.cfg.Expressions.MaxStickerBytes
	}
	held, err := s.st.CountExpressions(r.Context(), kind)
	if err != nil {
		s.log.Error("count expressions", slog.Any("error", err))
		writeAPIError(w, http.StatusInternalServerError, protocol.ErrInternal, "could not check the limit")
		return
	}
	if held >= limit {
		writeAPIError(w, http.StatusConflict, protocol.ErrExpressionLimit,
			fmt.Sprintf("this server already holds its limit of %d %ss", limit, kind))
		return
	}
	if !s.uploads.allow(user.ID) {
		writeAPIError(w, http.StatusTooManyRequests, protocol.ErrRateLimited,
			"you are uploading files too quickly")
		return
	}

	part, filename, failure := openUploadPart(r)
	if failure != nil {
		writeProtocolError(w, failure)
		return
	}
	defer part.Close()

	if !isAllowedExpressionFormat(filename) {
		writeAPIError(w, http.StatusBadRequest, protocol.ErrBadRequest,
			"unsupported image format; allowed formats are PNG, GIF, WebP and JPG")
		return
	}

	saved, err := files.SaveWithLimit(part, r.ContentLength, maxBytes)
	if failure := storageFailure(err, maxBytes, kind); failure != nil {
		if err != nil && !errors.Is(err, uploads.ErrTooLarge) && !errors.Is(err, uploads.ErrQuotaExceeded) {
			s.log.Error("store expression", slog.String("kind", kind), slog.Any("error", err))
		}
		writeProtocolError(w, failure)
		return
	}

	record := store.Expression{
		Kind:        kind,
		Name:        name,
		StorageKey:  saved.Key,
		Filename:    filename,
		ContentType: uploads.ContentType(filename),
		Size:        saved.Size,
		Animated:    strings.EqualFold(filepath.Ext(filename), ".gif"),
		CreatorID:   &user.ID,
	}
	created, err := s.st.CreateExpression(r.Context(), record)
	if err != nil {
		// The row is what makes the file reachable; without it the bytes are
		// unreferenced and must go back to the quota now rather than at the
		// next sweep.
		files.Remove(saved.Key, saved.Size)
		if errors.Is(err, store.ErrConflict) {
			writeAPIError(w, http.StatusConflict, protocol.ErrConflict,
				"something here already has that name")
			return
		}
		s.log.Error("record expression", slog.String("kind", kind), slog.Any("error", err))
		writeAPIError(w, http.StatusInternalServerError, protocol.ErrInternal, "the file could not be recorded")
		return
	}

	if err := s.hub.ReloadExpressions(r.Context()); err != nil {
		s.log.Warn("reload expressions", slog.Any("error", err))
	}
	view := expressionView(created)
	s.hub.Broadcast(protocol.Event(protocol.EvExpressionCreated, protocol.ExpressionEvent{Expression: view}))

	entry := auditTarget(protocol.AuditTargetExpression, created.ID, created.Name)
	entry.Action = protocol.AuditExpressionAdd
	entry.ActorID = &user.ID
	entry.ActorName = user.Nickname
	s.hub.audit(r.Context(), nil, entry)

	s.log.Info("expression uploaded",
		slog.Int64("user", user.ID), slog.String("kind", kind),
		slog.String("name", created.Name), slog.Int64("bytes", saved.Size))

	writeJSON(w, http.StatusCreated, view)
}

// handleSoundUploadHTTP adds a soundboard clip.
//
//	POST /upload/sound?name=<name>&emoji=<emoji>
//	Authorization: Bearer <session token>
//	Content-Type: multipart/form-data, one part named "file"
//
// The clip arrives as WAV, whatever the uploader picked: the client decodes it,
// cuts the range that was chosen and re-encodes. That is what puts the length
// limit within reach of the server, which reads it out of the header rather
// than taking the client's word for it.
func (s *Server) handleSoundUploadHTTP(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)

	files := s.hub.Files()
	if files == nil {
		writeAPIError(w, http.StatusForbidden, protocol.ErrUploadsDisabled,
			"this server does not accept file uploads")
		return
	}

	user, failure := s.authenticateRequest(r)
	if failure != nil {
		writeProtocolError(w, failure)
		return
	}
	base, _, err := s.hub.UserPermissions(r.Context(), user)
	if err != nil {
		s.log.Error("resolve uploader permissions", slog.Any("error", err))
		writeAPIError(w, http.StatusInternalServerError, protocol.ErrInternal, "could not check permissions")
		return
	}
	if !base.Has(permissions.ManageExpressions) {
		writeAPIError(w, http.StatusForbidden, protocol.ErrForbidden,
			"you are not allowed to manage this server's sounds")
		return
	}

	name, failure := validateSoundName(r.URL.Query().Get("name"))
	if failure != nil {
		writeProtocolError(w, failure)
		return
	}
	emoji, failure := validateSoundEmoji(r.URL.Query().Get("emoji"))
	if failure != nil {
		writeProtocolError(w, failure)
		return
	}

	limits := s.hub.cfg.Expressions
	held, err := s.st.CountSounds(r.Context())
	if err != nil {
		s.log.Error("count sounds", slog.Any("error", err))
		writeAPIError(w, http.StatusInternalServerError, protocol.ErrInternal, "could not check the limit")
		return
	}
	if held >= limits.MaxSounds {
		writeAPIError(w, http.StatusConflict, protocol.ErrExpressionLimit,
			fmt.Sprintf("this server already holds its limit of %d sounds", limits.MaxSounds))
		return
	}
	if !s.uploads.allow(user.ID) {
		writeAPIError(w, http.StatusTooManyRequests, protocol.ErrRateLimited,
			"you are uploading files too quickly")
		return
	}

	part, filename, failure := openUploadPart(r)
	if failure != nil {
		writeProtocolError(w, failure)
		return
	}
	defer part.Close()

	if !strings.EqualFold(filepath.Ext(filename), soundExtension) {
		writeAPIError(w, http.StatusBadRequest, protocol.ErrBadRequest,
			"a soundboard clip must be uploaded as WAV")
		return
	}

	saved, err := files.SaveWithLimit(part, r.ContentLength, limits.MaxSoundBytes)
	if failure := storageFailure(err, limits.MaxSoundBytes, "sound"); failure != nil {
		if err != nil && !errors.Is(err, uploads.ErrTooLarge) && !errors.Is(err, uploads.ErrQuotaExceeded) {
			s.log.Error("store sound", slog.Any("error", err))
		}
		writeProtocolError(w, failure)
		return
	}

	// Read from the file rather than believed from the request. A clip is
	// played at everybody in a channel at once, and its length is the whole of
	// how much that can be made to hurt.
	path, err := files.Path(saved.Key)
	if err != nil {
		files.Remove(saved.Key, saved.Size)
		writeAPIError(w, http.StatusInternalServerError, protocol.ErrInternal, "the file could not be stored")
		return
	}
	durationMs, readable := uploads.WAVDuration(path)
	if !readable {
		files.Remove(saved.Key, saved.Size)
		writeAPIError(w, http.StatusBadRequest, protocol.ErrBadRequest,
			"that file is not readable as WAV audio")
		return
	}
	if durationMs > limits.MaxSoundSeconds*1000 {
		files.Remove(saved.Key, saved.Size)
		writeAPIError(w, http.StatusBadRequest, protocol.ErrBadRequest,
			fmt.Sprintf("a sound may be at most %d seconds long", limits.MaxSoundSeconds))
		return
	}

	created, err := s.st.CreateSound(r.Context(), store.Sound{
		Name:        name,
		Emoji:       emoji,
		StorageKey:  saved.Key,
		Filename:    filename,
		ContentType: uploads.ContentType(filename),
		Size:        saved.Size,
		DurationMs:  durationMs,
		Volume:      100,
		CreatorID:   &user.ID,
	})
	if err != nil {
		files.Remove(saved.Key, saved.Size)
		s.log.Error("record sound", slog.Any("error", err))
		writeAPIError(w, http.StatusInternalServerError, protocol.ErrInternal, "the file could not be recorded")
		return
	}

	if err := s.hub.ReloadSounds(r.Context()); err != nil {
		s.log.Warn("reload sounds", slog.Any("error", err))
	}
	view := soundView(created)
	s.hub.Broadcast(protocol.Event(protocol.EvSoundCreated, protocol.SoundEvent{Sound: view}))

	entry := auditTarget(protocol.AuditTargetSound, created.ID, created.Name)
	entry.Action = protocol.AuditSoundAdd
	entry.ActorID = &user.ID
	entry.ActorName = user.Nickname
	s.hub.audit(r.Context(), nil, entry)

	s.log.Info("sound uploaded",
		slog.Int64("user", user.ID), slog.String("name", created.Name),
		slog.Int("ms", durationMs), slog.Int64("bytes", saved.Size))

	writeJSON(w, http.StatusCreated, view)
}

// isAllowedExpressionFormat is the narrow list an emoji or sticker may arrive
// in. SVG is deliberately absent: it is markup, and markup fetched by every
// client that renders a message is not a thing to accept from an upload.
func isAllowedExpressionFormat(filename string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".png", ".gif", ".webp", ".jpg", ".jpeg":
		return true
	default:
		return false
	}
}

// storageFailure turns the errors Save reports into the protocol error the
// caller should send, or nil when the save worked.
func storageFailure(err error, maxBytes int64, what string) *protocol.Error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, uploads.ErrTooLarge):
		return protocol.Errorf(protocol.ErrTooLarge,
			fmt.Sprintf("a %s on this server may be at most %s", what, humanBytes(maxBytes)))
	case errors.Is(err, uploads.ErrQuotaExceeded):
		return protocol.Errorf(protocol.ErrStorageFull,
			"this server has no storage left for new files")
	default:
		return protocol.Errorf(protocol.ErrInternal, "the file could not be stored")
	}
}
