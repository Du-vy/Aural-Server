package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aural-chat/aural-server/internal/auth"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
	"github.com/aural-chat/aural-server/internal/uploads"
)

// Attachments travel over HTTP rather than over the WebSocket. A file does not
// fit the frame budget the socket is tuned for, and going over HTTP is what
// gives uploads a progress bar and downloads range requests, resumable seeking
// and ordinary browser caching — none of which a JSON frame could offer.
const (
	// uploadPrefix is where a stored file is served from. The unguessable
	// storage key is the whole of its access control: a client cannot attach an
	// Authorization header to an <img> or <video> tag, so the URL is the
	// capability. It is exactly the model a CDN-backed chat uses.
	uploadPrefix = "/attachments/"
	// maxFilenameRunes bounds the name kept for a file. It is only ever shown
	// and offered as a download name, never used as a path.
	maxFilenameRunes = 200
	// uploadBurst and uploadsPerSecond throttle uploading. It is far tighter
	// than the message limiter because each one costs disk rather than a row.
	uploadBurst      = 5
	uploadsPerSecond = 0.5
	// orphanGrace is how recently a file may have been written and still be
	// spared by the startup sweep. Nothing should be in flight at that point,
	// so this is a second line rather than the first: it is what keeps a sweep
	// from ever being the thing that deletes a live upload.
	orphanGrace = time.Minute
)

// userLimiters throttles per identity rather than per connection: an HTTP
// request arrives on its own connection, so there is no session to hang a
// bucket off. Every endpoint outside the WebSocket needs one, at its own rate.
type userLimiters struct {
	burst  int
	perSec float64

	mu sync.Mutex
	by map[int64]*rateLimiter
}

func newUserLimiters(burst int, perSec float64) *userLimiters {
	return &userLimiters{burst: burst, perSec: perSec, by: map[int64]*rateLimiter{}}
}

func (u *userLimiters) allow(userID int64) bool {
	u.mu.Lock()
	limiter, ok := u.by[userID]
	if !ok {
		limiter = newRateLimiter(u.burst, u.perSec)
		u.by[userID] = limiter
	}
	u.mu.Unlock()
	return limiter.allow()
}

// handleUpload accepts one file and records it as pending: it belongs to
// nobody's message until the client sends message.send naming its id.
//
//	POST /upload?channel=<id>
//	Authorization: Bearer <session token>
//	Content-Type: multipart/form-data, one part named "file"
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
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

	channelID, err := strconv.ParseInt(r.URL.Query().Get("channel"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, protocol.ErrBadRequest,
			"the channel query parameter is required")
		return
	}

	// The same two checks message.send makes, made here so a file that could
	// never be posted is never written: the channel must be a text channel the
	// uploader may both see and attach to.
	base, roleIDs, err := s.hub.UserPermissions(r.Context(), user)
	if err != nil {
		s.log.Error("resolve uploader permissions", slog.Any("error", err))
		writeAPIError(w, http.StatusInternalServerError, protocol.ErrInternal, "could not check permissions")
		return
	}
	channel, known := s.hub.Channel(channelID)
	perms := s.hub.ChannelPermissions(base, roleIDs, channelID)
	if !known || !perms.Has(permissions.ViewChannel) {
		writeAPIError(w, http.StatusNotFound, protocol.ErrNotFound, "no such channel")
		return
	}
	if channel.Type != protocol.ChannelText {
		writeAPIError(w, http.StatusBadRequest, protocol.ErrBadRequest, "that channel does not carry messages")
		return
	}
	if !perms.Has(permissions.SendMessages) || !perms.Has(permissions.AttachFiles) {
		writeAPIError(w, http.StatusForbidden, protocol.ErrForbidden, "you may not attach files here")
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

	// The part carries no length of its own, so the request's is the only hint
	// available for reserving quota. Save treats it as exactly that: a hint.
	saved, err := files.Save(part, r.ContentLength)
	switch {
	case errors.Is(err, uploads.ErrTooLarge):
		writeAPIError(w, http.StatusRequestEntityTooLarge, protocol.ErrTooLarge,
			fmt.Sprintf("files on this server may be at most %s", humanBytes(files.MaxFileBytes())))
		return
	case errors.Is(err, uploads.ErrQuotaExceeded):
		writeAPIError(w, http.StatusInsufficientStorage, protocol.ErrStorageFull,
			"this server has no storage left for new files")
		return
	case err != nil:
		s.log.Error("store upload", slog.Any("error", err))
		writeAPIError(w, http.StatusInternalServerError, protocol.ErrInternal, "the file could not be stored")
		return
	}

	record := store.Attachment{
		UserID:      &user.ID,
		ChannelID:   channelID,
		StorageKey:  saved.Key,
		Filename:    filename,
		ContentType: uploads.ContentType(filename),
		Size:        saved.Size,
	}
	if path, err := files.Path(saved.Key); err == nil {
		if width, height, ok := uploads.Dimensions(path); ok {
			record.Width, record.Height = &width, &height
		}
	}

	created, err := s.st.CreateAttachment(r.Context(), record)
	if err != nil {
		// The row is what makes the file reachable; without it the bytes are
		// unreferenced and must go back to the quota now rather than at the
		// next sweep.
		files.Remove(saved.Key, saved.Size)
		s.log.Error("record upload", slog.Any("error", err))
		writeAPIError(w, http.StatusInternalServerError, protocol.ErrInternal, "the file could not be recorded")
		return
	}

	s.log.Info("file uploaded",
		slog.Int64("user", user.ID), slog.Int64("channel", channelID),
		slog.String("file", filename), slog.Int64("bytes", saved.Size))

	writeJSON(w, http.StatusCreated, attachmentView(created))
}

func isAllowedImageFormat(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".avif", ".bmp":
		return true
	default:
		return false
	}
}

type MediaUploadResult struct {
	URL  string        `json:"url"`
	User protocol.User `json:"user"`
}

// handleAvatarUpload replaces the caller's avatar.
//
//	POST /upload/avatar
//	Authorization: Bearer <session token>
//	Content-Type: multipart/form-data, one part named "file"
func (s *Server) handleAvatarUpload(w http.ResponseWriter, r *http.Request) {
	s.uploadProfileMedia(w, r, store.KindAvatar)
}

// handleBannerUpload replaces the caller's profile banner.
//
//	POST /upload/banner
func (s *Server) handleBannerUpload(w http.ResponseWriter, r *http.Request) {
	s.uploadProfileMedia(w, r, store.KindBanner)
}

// uploadProfileMedia is both of the above. An avatar and a banner differ in one
// size limit, one column and one word in an error message; everything else —
// the auth, the format check, the quota, the row, the broadcast — is the same
// sequence, and writing it twice is how the two drift apart.
func (s *Server) uploadProfileMedia(w http.ResponseWriter, r *http.Request, kind string) {
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

	if !isAllowedImageFormat(filename) {
		writeAPIError(w, http.StatusBadRequest, protocol.ErrBadRequest,
			"unsupported image format; allowed formats are JPG, PNG, WebP, GIF, AVIF, BMP")
		return
	}

	maxBytes := s.cfg.Uploads.MaxAvatarBytes
	if kind == store.KindBanner {
		maxBytes = s.cfg.Uploads.MaxBannerBytes
	}
	if maxBytes <= 0 {
		maxBytes = files.MaxFileBytes()
	}

	saved, err := files.SaveWithLimit(part, r.ContentLength, maxBytes)
	switch {
	case errors.Is(err, uploads.ErrTooLarge):
		writeAPIError(w, http.StatusRequestEntityTooLarge, protocol.ErrTooLarge,
			fmt.Sprintf("%ss on this server may be at most %s", kind, humanBytes(maxBytes)))
		return
	case errors.Is(err, uploads.ErrQuotaExceeded):
		writeAPIError(w, http.StatusInsufficientStorage, protocol.ErrStorageFull,
			"this server has no storage left for new files")
		return
	case err != nil:
		s.log.Error("store profile media", slog.String("kind", kind), slog.Any("error", err))
		writeAPIError(w, http.StatusInternalServerError, protocol.ErrInternal, "the file could not be stored")
		return
	}

	// The row goes in before the user points at it: it is what accounts for the
	// bytes, so a failure after this point can give them back, and a restart
	// still knows the file is there.
	_, displaced, err := s.st.ReplaceProfileMedia(r.Context(), store.ProfileMedia{
		UserID:     user.ID,
		Kind:       kind,
		StorageKey: saved.Key,
		Filename:   filename,
		Size:       saved.Size,
	})
	if err != nil {
		files.Remove(saved.Key, saved.Size)
		s.log.Error("record profile media", slog.String("kind", kind), slog.Any("error", err))
		writeAPIError(w, http.StatusInternalServerError, protocol.ErrInternal, "the file could not be recorded")
		return
	}

	mediaURL := uploadPrefix + saved.Key + "/" + url.PathEscape(filename)
	set := s.st.SetAvatar
	if kind == store.KindBanner {
		set = s.st.SetBanner
	}
	if err := set(r.Context(), user.ID, &mediaURL); err != nil {
		// Nothing points at the picture, so the row and the bytes both go back.
		if replaced, clearErr := s.st.ClearProfileMedia(r.Context(), user.ID, kind); clearErr == nil {
			s.hub.RemoveProfileMedia(replaced)
		}
		s.log.Error("record profile media url", slog.String("kind", kind), slog.Any("error", err))
		writeAPIError(w, http.StatusInternalServerError, protocol.ErrInternal, "the picture could not be recorded")
		return
	}

	// Only once the new picture is the one in use does the old one go.
	s.hub.RemoveProfileMedia(displaced)

	updatedUser, err := s.st.UserByID(r.Context(), user.ID)
	if err != nil {
		updatedUser = user
		if kind == store.KindBanner {
			updatedUser.Banner = &mediaURL
		} else {
			updatedUser.Avatar = &mediaURL
		}
	}

	if sess, ok := s.hub.SessionForUser(user.ID); ok {
		sess.setUser(updatedUser)
		view := sess.view()
		s.hub.BroadcastUserUpdated(view)
		writeJSON(w, http.StatusOK, MediaUploadResult{URL: mediaURL, User: view})
		return
	}

	_, roleIDs, _ := s.hub.UserPermissions(r.Context(), updatedUser)
	writeJSON(w, http.StatusOK, MediaUploadResult{URL: mediaURL, User: userView(updatedUser, roleIDs, nil, false)})
}

// RemoveProfileMedia unlinks pictures whose rows are already gone and returns
// their room to the quota. The recorded size is used rather than the one on
// disk: it is what the quota was charged, so it is what has to be refunded.
func (h *Hub) RemoveProfileMedia(media []store.ProfileMedia) {
	if h.files == nil {
		return
	}
	for _, m := range media {
		h.files.Remove(m.StorageKey, m.Size)
	}
}

// openUploadPart finds the file part of a multipart body and returns it
// unbuffered, so the bytes go straight from the socket to the disk.
func openUploadPart(r *http.Request) (io.ReadCloser, string, *protocol.Error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, "", protocol.Errorf(protocol.ErrBadRequest,
			"the body must be multipart/form-data with one file part")
	}

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, "", protocol.Errorf(protocol.ErrBadRequest, "the body carried no file")
		}
		if err != nil {
			return nil, "", protocol.Errorf(protocol.ErrBadRequest, "the body could not be read")
		}
		if part.FormName() != "file" {
			part.Close()
			continue
		}
		name := cleanFilename(part.FileName())
		if name == "" {
			part.Close()
			return nil, "", protocol.Errorf(protocol.ErrBadRequest, "the file part is missing a filename")
		}
		return part, name, nil
	}
}

// cleanFilename reduces a client-supplied name to something safe to store,
// show, and hand back as a download name.
//
// Only the base name survives: a name is a label here, never a path, so
// anything that looks like one is discarded rather than sanitised.
func cleanFilename(raw string) string {
	// Both separators are stripped regardless of host, because the client that
	// sent the name may not run the same operating system as the server.
	raw = strings.ReplaceAll(raw, "\\", "/")
	name := filepath.Base(strings.TrimSpace(raw))

	var b strings.Builder
	for _, r := range name {
		switch {
		case r == utf8.RuneError, unicode.IsControl(r):
			continue
		case r == '/', r == ':', r == '*', r == '?', r == '"', r == '<', r == '>', r == '|':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}

	name = strings.TrimSpace(b.String())
	name = strings.Trim(name, ".")
	if name == "" {
		return ""
	}
	if utf8.RuneCountInString(name) > maxFilenameRunes {
		// The extension decides how the file is served, so it is what survives
		// the trim rather than the leading characters of a very long name.
		ext := filepath.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		keep := maxFilenameRunes - utf8.RuneCountInString(ext)
		if keep < 1 {
			return string([]rune(name)[:maxFilenameRunes])
		}
		name = string([]rune(stem)[:keep]) + ext
	}
	return name
}

// handleAttachment serves a stored file.
//
//	GET /attachments/<key>/<filename>
//
// The key is the authorisation. Adding ?download forces the browser to save
// the file rather than display it, which is what the client's own download
// action asks for.
func (s *Server) handleAttachment(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)

	files := s.hub.Files()
	if files == nil {
		http.NotFound(w, r)
		return
	}

	key := r.PathValue("key")
	rawFilename := r.PathValue("filename")
	filename, err := url.PathUnescape(rawFilename)
	if err != nil || filename == "" {
		filename = rawFilename
	}

	var contentType string
	var modTime time.Time

	attachment, err := s.st.AttachmentByStorageKey(r.Context(), key)
	switch {
	case err == nil:
		filename = attachment.Filename
		contentType = attachment.ContentType
		modTime = time.Unix(attachment.CreatedAt, 0)
	case errors.Is(err, store.ErrNotFound):
		// Avatars and banners are recorded elsewhere, so the name and type come
		// from that table rather than from the URL, which anybody may write.
		if picture, pErr := s.st.ProfileMediaByStorageKey(r.Context(), key); pErr == nil {
			filename = picture.Filename
			contentType = uploads.ContentType(picture.Filename)
			modTime = time.Unix(picture.CreatedAt, 0)
		} else {
			contentType = uploads.ContentType(filename)
		}
	default:
		s.log.Error("read attachment", slog.Any("error", err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	file, info, err := files.Open(key)
	if err != nil {
		s.log.Warn("attachment file is missing", slog.String("key", key))
		http.NotFound(w, r)
		return
	}
	defer file.Close()

	if modTime.IsZero() && info != nil {
		modTime = info.ModTime()
	}

	inline := uploads.Inline(contentType) && !r.URL.Query().Has("download")
	disposition := "attachment"
	if inline {
		disposition = "inline"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType(disposition, map[string]string{"filename": filename}))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Accept-Ranges", "bytes")

	http.ServeContent(w, r, filename, modTime, file)
}

// --- HTTP plumbing ----------------------------------------------------------

// authenticateRequest resolves the bearer token an HTTP request carries into
// the identity behind it. It is the same token the WebSocket resumes with, so
// a client that is connected already holds one.
func (s *Server) authenticateRequest(r *http.Request) (store.User, *protocol.Error) {
	header := r.Header.Get("Authorization")
	raw, found := strings.CutPrefix(header, "Bearer ")
	if !found || strings.TrimSpace(raw) == "" {
		return store.User{}, protocol.Errorf(protocol.ErrUnauthorized, "a bearer session token is required")
	}

	user, err := s.st.UserByTokenHash(r.Context(), auth.HashToken(raw))
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, protocol.Errorf(protocol.ErrInvalidCredentials, "this session token is no longer valid")
	}
	if err != nil {
		s.log.Error("resolve upload token", slog.Any("error", err))
		return store.User{}, protocol.Errorf(protocol.ErrInternal, "could not check the session token")
	}
	if !user.Registered() && !s.cfg.Registration.AllowGuests {
		return store.User{}, protocol.Errorf(protocol.ErrGuestsDisabled, "this server only accepts registered accounts")
	}
	return user, nil
}

// applyCORS mirrors the origin policy the WebSocket upgrade already enforces,
// so a browser client served from somewhere else can upload exactly where it
// can connect.
func (s *Server) applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	if !s.cfg.OriginAllowed(origin) {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	// Range is here for the client's text previews, which ask for only the head
	// of a file rather than the whole of it.
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Range")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Max-Age", "600")
}

// handlePreflight answers the OPTIONS request a browser sends before an upload,
// which it does because the request carries an Authorization header.
func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// apiError is the JSON body of a failed HTTP request. It carries the same
// codes the WebSocket uses, so a client has one table of errors rather than two.
type apiError struct {
	Error protocol.Error `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiError{Error: protocol.Error{Code: code, Message: message}})
}

// writeProtocolError sends an error raised by shared code, mapping its code to
// the HTTP status that means the same thing.
func writeProtocolError(w http.ResponseWriter, failure *protocol.Error) {
	writeAPIError(w, statusForCode(failure.Code), failure.Code, failure.Message)
}

func statusForCode(code string) int {
	switch code {
	case protocol.ErrBadRequest:
		return http.StatusBadRequest
	case protocol.ErrUnauthorized, protocol.ErrInvalidCredentials:
		return http.StatusUnauthorized
	case protocol.ErrForbidden, protocol.ErrGuestsDisabled, protocol.ErrUploadsDisabled:
		return http.StatusForbidden
	case protocol.ErrNotFound:
		return http.StatusNotFound
	case protocol.ErrConflict:
		return http.StatusConflict
	case protocol.ErrTooLarge:
		return http.StatusRequestEntityTooLarge
	case protocol.ErrStorageFull:
		return http.StatusInsufficientStorage
	case protocol.ErrRateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

// humanBytes renders a limit the way the message announcing it should read.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.4g %sB", float64(n)/float64(div), []string{"K", "M", "G", "T"}[exp])
}

// --- housekeeping -----------------------------------------------------------

// sweepOrphanedFiles deletes files in the upload directory that no row names.
//
// A row is written after the bytes it names, so a crash in between leaves a
// file nothing can reach: not served, not deletable, and not counted against
// the quota that a restart recomputes from the tables. Only a walk of the
// directory finds those, and startup is when it is safe to do — nothing is
// serving yet, so nothing is half-written.
//
// The keep set has to be every key in both tables. A partial set does not sweep
// less, it deletes more, so a query that fails cancels the sweep rather than
// narrowing it.
func (s *Server) sweepOrphanedFiles(ctx context.Context) {
	files := s.hub.Files()
	if files == nil {
		return
	}

	attachments, err := s.st.AttachmentKeys(ctx)
	if err != nil {
		s.log.Warn("skipping orphan sweep: could not read attachment keys", slog.Any("error", err))
		return
	}
	pictures, err := s.st.ProfileMediaKeys(ctx)
	if err != nil {
		s.log.Warn("skipping orphan sweep: could not read profile media keys", slog.Any("error", err))
		return
	}

	keep := make(map[string]struct{}, len(attachments)+len(pictures))
	for _, key := range attachments {
		keep[key] = struct{}{}
	}
	for _, key := range pictures {
		keep[key] = struct{}{}
	}

	removed, err := files.Sweep(keep, orphanGrace)
	if err != nil {
		s.log.Warn("orphan sweep", slog.Any("error", err))
		return
	}
	if removed > 0 {
		s.log.Info("removed unreferenced files", slog.Int("files", removed), slog.Int("kept", len(keep)))
	}
}

// sweepPending runs until ctx ends, deleting uploads that were never posted.
//
// A writer who picks a file and then closes the client leaves one behind. The
// row is what makes it reachable, so the row goes first and the file after it:
// the reverse order would leave a message able to name a file that is gone.
func (s *Server) sweepPending(ctx context.Context) {
	ttl := time.Duration(s.cfg.Uploads.PendingTTLMinutes) * time.Minute
	ticker := time.NewTicker(min(ttl, 15*time.Minute))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepPendingOnce(ctx, time.Now().Add(-ttl).Unix())
		}
	}
}

// sweepPendingOnce removes one batch of abandoned uploads.
func (s *Server) sweepPendingOnce(ctx context.Context, cutoff int64) {
	const batch = 200

	stale, err := s.st.PendingAttachmentsBefore(ctx, cutoff, batch)
	if err != nil {
		s.log.Warn("sweep pending uploads", slog.Any("error", err))
		return
	}
	removed := 0
	for _, a := range stale {
		if err := s.st.DeleteAttachment(ctx, a.ID); err != nil {
			continue
		}
		s.hub.RemoveFiles([]store.Attachment{a})
		removed++
	}
	if removed > 0 {
		s.log.Info("removed abandoned uploads", slog.Int("files", removed))
	}

	ttlDays := s.cfg.Unfurl.CacheTTLDays
	if ttlDays <= 0 {
		ttlDays = 7
	}
	pruneCutoff := time.Now().Add(-time.Duration(ttlDays) * 24 * time.Hour).Unix()
	if pruned, err := s.st.PruneLinkPreviews(ctx, pruneCutoff); err == nil && pruned > 0 {
		s.log.Info("pruned expired link previews", slog.Int64("count", pruned))
	}
}
