package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/coder/websocket"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// deviceSaltBytes is the entropy of the per-install salt a client folds into
// the machine identifier it presents.
const deviceSaltBytes = 24

// maxDeviceLength bounds what a client may send as a device identifier. It is
// a hash, so anything longer is either a mistake or an attempt to use the
// column as storage.
const maxDeviceLength = 128

// maxBanMarks is how many addresses and devices one ban picks up from where the
// identity has been seen. A ban should follow somebody's machine and the couple
// of places they connect from, not every network they have ever touched: the
// wider it reaches the more likely it is to catch a stranger who was handed the
// same address by the same provider six weeks later.
const maxBanMarks = 5

// loadDeviceSalt reads the salt this server hands out, minting one the first
// time. It is stored rather than generated per run because a value that changed
// on restart would make every device identifier new, and a ban against a
// machine would last exactly as long as the process did.
func loadDeviceSalt(ctx context.Context, st *store.Store) (string, error) {
	salt, err := st.Meta(ctx, store.MetaDeviceSalt)
	if err == nil && salt != "" {
		return salt, nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}

	buf := make([]byte, deviceSaltBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("gateway: read device salt entropy: %w", err)
	}
	salt = base64.RawURLEncoding.EncodeToString(buf)
	if err := st.SetMeta(ctx, store.MetaDeviceSalt, salt); err != nil {
		return "", err
	}
	return salt, nil
}

// DeviceSalt is the value handed to clients in Hello.
func (h *Hub) DeviceSalt() string { return h.deviceSalt }

// cleanDevice reduces what a client sent to something worth storing. Anything
// unexpected becomes the empty string, which matches no ban: a malformed
// identifier is the same as a client that sent none.
func cleanDevice(raw string) string {
	if len(raw) == 0 || len(raw) > maxDeviceLength {
		return ""
	}
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return ""
		}
	}
	return raw
}

// FindActiveBan looks for a ban in force against any of the handles a
// connection presents. Zero and empty values are simply not asked about.
func (h *Hub) FindActiveBan(ctx context.Context, userID int64, ip, device string) (store.Ban, bool) {
	matches := make([]store.BanMatch, 0, 3)
	if userID > 0 {
		matches = append(matches, store.BanMatch{Kind: store.MatchUser, Value: formatID(userID)})
	}
	if ip != "" {
		matches = append(matches, store.BanMatch{Kind: store.MatchIP, Value: ip})
	}
	if device != "" {
		matches = append(matches, store.BanMatch{Kind: store.MatchDevice, Value: device})
	}
	if len(matches) == 0 {
		return store.Ban{}, false
	}

	ban, err := h.st.FindBan(ctx, time.Now().Unix(), matches)
	if errors.Is(err, store.ErrNotFound) {
		return store.Ban{}, false
	}
	if err != nil {
		// A database that cannot be read is not a reason to let somebody in,
		// but it is not a reason to lock everybody out either: the log is what
		// an operator has to go on, and the connection is allowed.
		h.log.Error("check bans", slog.Any("error", err))
		return store.Ban{}, false
	}
	return ban, true
}

// banError is what a refused connection is told. The reason travels with it,
// because a refusal nobody can explain is the one thing a moderator will be
// asked about, and so does the date it lifts on.
func banError(ban store.Ban) *protocol.Error {
	message := "you are banned from this server"
	if ban.Reason != "" {
		message += ": " + ban.Reason
	}
	if ban.ExpiresAt != nil {
		message += " (until " + time.Unix(*ban.ExpiresAt, 0).UTC().Format(time.RFC3339) + ")"
	}
	return protocol.Errorf(protocol.ErrBanned, message)
}

// enforceBan closes every connection the ban now catches.
//
// It walks the sessions rather than querying, because the handles a live
// connection presents are held on the session: the address it came from and
// the device it declared are not in any table until the identity behind them
// is recorded, and a guest's never will be.
//
// The owner is never closed. The handles a ban carries already exclude the ones
// the owner and the moderator are sitting behind, so this is the second line
// rather than the first — but a ban issued while the owner was away and
// enforced when they came back would otherwise reach them.
func (h *Hub) enforceBan(ban store.Ban) {
	byKind := map[string]map[string]bool{}
	for _, m := range ban.Matches {
		if byKind[m.Kind] == nil {
			byKind[m.Kind] = map[string]bool{}
		}
		byKind[m.Kind][m.Value] = true
	}

	reason := ban.Reason
	if reason == "" {
		reason = "banned"
	}
	for _, s := range h.Sessions() {
		if h.IsOwner(s.UserID()) {
			continue
		}
		caught := byKind[store.MatchUser][formatID(s.UserID())] ||
			byKind[store.MatchIP][s.Peer()] ||
			(s.Device() != "" && byKind[store.MatchDevice][s.Device()])
		if caught {
			go s.Close(websocket.StatusPolicyViolation, reason)
		}
	}
}

// --- the audit log ----------------------------------------------------------

// audit records one moderation action and tells the sessions allowed to read
// the log about it.
//
// The action has already happened by the time this is called, so a failure to
// write is logged and nothing more: a log that could veto a moderation would
// be a second, weaker permission check.
func (h *Hub) audit(ctx context.Context, actor *Session, entry store.AuditEntry) {
	if actor != nil {
		id := actor.UserID()
		entry.ActorID = &id
		entry.ActorName = actor.User().Nickname
	}
	if entry.ActorName == "" {
		entry.ActorName = "the server"
	}

	written, err := h.st.RecordAudit(ctx, entry)
	if err != nil {
		h.log.Error("record audit entry", slog.String("action", entry.Action), slog.Any("error", err))
		return
	}

	view := auditView(written)
	event := protocol.Event(protocol.EvAuditEntry, protocol.AuditEntryEvent{Entry: view})
	h.BroadcastTo(event, func(s *Session) bool {
		base, _ := s.Permissions()
		return base.Has(permissions.ViewAuditLog)
	})
}

// auditTarget fills in the three fields that describe what was acted on. It is
// a helper rather than three assignments because every call site would
// otherwise have to remember that the id is a pointer.
func auditTarget(kind string, id int64, name string) store.AuditEntry {
	entry := store.AuditEntry{TargetType: kind, TargetName: name}
	if id > 0 {
		entry.TargetID = &id
	}
	return entry
}

// changed records one field an action altered, and only when it actually did.
func changed(list []store.AuditChange, key, before, after string) []store.AuditChange {
	if before == after {
		return list
	}
	return append(list, store.AuditChange{Key: key, Before: before, After: after})
}
