package gateway

import (
	"context"
	"log/slog"
	"sort"
	"sync"

	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
	"github.com/aural-chat/aural-server/internal/uploads"
)

// maxChannelDepth bounds the ancestor walk so a cycle introduced by a bad write
// can never hang a permission check.
const maxChannelDepth = 32

// Hub owns everything that is true only while the server is running: which
// identities are connected, and which channel each of them sits in. Channel
// membership is deliberately not persisted, exactly as in TeamSpeak: a user is
// in a channel for as long as the connection lasts.
//
// It also caches the role and channel tables, which are read on nearly every
// frame and written very rarely.
type Hub struct {
	cfg     *config.Config
	cfgPath string
	st      *store.Store
	log     *slog.Logger
	// files is nil on a server with uploads switched off, which every path
	// that touches an attachment checks for.
	files *uploads.Store

	// cfgMu guards the only configuration fields that change while the server
	// runs: the server name and description, both editable over the protocol.
	cfgMu sync.RWMutex

	mu       sync.RWMutex
	sessions map[int64]*Session
	byUser   map[int64]*Session

	cacheMu      sync.RWMutex
	roles        map[int64]store.Role
	channels     map[int64]store.Channel
	everyoneID   int64
	registeredID int64
	adminID      int64
}

// NewHub builds a hub and primes its caches from the database. cfgPath is where
// runtime configuration changes are written back.
func NewHub(ctx context.Context, cfg *config.Config, cfgPath string, st *store.Store, log *slog.Logger) (*Hub, error) {
	h := &Hub{
		cfg:      cfg,
		cfgPath:  cfgPath,
		st:       st,
		log:      log,
		sessions: map[int64]*Session{},
		byUser:   map[int64]*Session{},
	}
	if err := h.ReloadRoles(ctx); err != nil {
		return nil, err
	}
	if err := h.ReloadChannels(ctx); err != nil {
		return nil, err
	}
	if cfg.Uploads.Enabled {
		// The quota is measured against what the database already accounts
		// for, so a restart neither forgets stored files nor double-counts them.
		used, err := st.TotalAttachmentBytes(ctx)
		if err != nil {
			return nil, err
		}
		files, err := uploads.Open(cfg.Uploads.Path,
			cfg.Uploads.MaxFileBytes, cfg.Uploads.MaxTotalBytes, used)
		if err != nil {
			return nil, err
		}
		h.files = files
	}
	return h, nil
}

// Files is the upload store, or nil when this server has uploads switched off.
func (h *Hub) Files() *uploads.Store { return h.files }

// UserPermissions resolves what a user may do, from the database rather than
// from a live session. The HTTP upload endpoint needs it because a client may
// legitimately be uploading over a connection the WebSocket has not caught up
// with, and because presence is not what grants a permission: the account is.
func (h *Hub) UserPermissions(ctx context.Context, u store.User) (permissions.Permission, []int64, error) {
	explicit, err := h.st.RoleIDsForUser(ctx, u.ID)
	if err != nil {
		return permissions.None, nil, err
	}
	roleIDs := h.EffectiveRoleIDs(u, explicit)
	return h.BasePermissions(roleIDs), roleIDs, nil
}

// RemoveFiles unlinks stored files and gives their room back to the quota. The
// rows are already gone by the time it is called, so a failure to unlink costs
// disk space and nothing else.
func (h *Hub) RemoveFiles(attachments []store.Attachment) {
	if h.files == nil {
		return
	}
	for _, a := range attachments {
		h.files.Remove(a.StorageKey, a.Size)
	}
}

// ServerIdentity reads the configuration fields that change at runtime.
func (h *Hub) ServerIdentity() (name, description, klipyApiKey string) {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	return h.cfg.Server.Name, h.cfg.Server.Description, h.cfg.Integrations.KlipyAPIKey
}

// updateServerIdentity applies configuration updates and persists them.
func (h *Hub) updateServerIdentity(setName bool, name string, setDescription bool, description string, setKlipy bool, klipyApiKey string) error {
	h.cfgMu.Lock()
	if setName {
		h.cfg.Server.Name = name
	}
	if setDescription {
		h.cfg.Server.Description = description
	}
	if setKlipy {
		h.cfg.Integrations.KlipyAPIKey = klipyApiKey
	}
	snapshot := *h.cfg
	h.cfgMu.Unlock()

	if h.cfgPath == "" {
		return nil
	}
	return config.Save(h.cfgPath, snapshot)
}

// --- caches -----------------------------------------------------------------

// ReloadRoles refreshes the cached role table. Call it after any role write.
func (h *Hub) ReloadRoles(ctx context.Context) error {
	roles, err := h.st.AllRoles(ctx)
	if err != nil {
		return err
	}
	byID := make(map[int64]store.Role, len(roles))
	var everyone, registered, admin int64
	for _, r := range roles {
		byID[r.ID] = r
		switch r.Managed {
		case protocol.ManagedEveryone:
			everyone = r.ID
		case protocol.ManagedRegistered:
			registered = r.ID
		case protocol.ManagedAdmin:
			admin = r.ID
		}
	}

	h.cacheMu.Lock()
	h.roles, h.everyoneID, h.registeredID, h.adminID = byID, everyone, registered, admin
	h.cacheMu.Unlock()
	return nil
}

// ReloadChannels refreshes the cached channel tree. Call it after any channel
// write.
func (h *Hub) ReloadChannels(ctx context.Context) error {
	channels, err := h.st.AllChannels(ctx)
	if err != nil {
		return err
	}
	byID := make(map[int64]store.Channel, len(channels))
	for _, c := range channels {
		byID[c.ID] = c
	}

	h.cacheMu.Lock()
	h.channels = byID
	h.cacheMu.Unlock()
	return nil
}

// Role returns a cached role.
func (h *Hub) Role(id int64) (store.Role, bool) {
	h.cacheMu.RLock()
	defer h.cacheMu.RUnlock()
	r, ok := h.roles[id]
	return r, ok
}

// Channel returns a cached channel.
func (h *Hub) Channel(id int64) (store.Channel, bool) {
	h.cacheMu.RLock()
	defer h.cacheMu.RUnlock()
	c, ok := h.channels[id]
	return c, ok
}

// EveryoneRoleID is the id of the managed role every user holds.
func (h *Hub) EveryoneRoleID() int64 {
	h.cacheMu.RLock()
	defer h.cacheMu.RUnlock()
	return h.everyoneID
}

// AdminRoleID is the id of the managed role the owner token grants.
func (h *Hub) AdminRoleID() int64 {
	h.cacheMu.RLock()
	defer h.cacheMu.RUnlock()
	return h.adminID
}

// SortedRoles lists cached roles lowest position first.
func (h *Hub) SortedRoles() []store.Role {
	h.cacheMu.RLock()
	out := make([]store.Role, 0, len(h.roles))
	for _, r := range h.roles {
		out = append(out, r)
	}
	h.cacheMu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// SortedChannels lists cached channels in render order.
func (h *Hub) SortedChannels() []store.Channel {
	h.cacheMu.RLock()
	out := make([]store.Channel, 0, len(h.channels))
	for _, c := range h.channels {
		out = append(out, c)
	}
	h.cacheMu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// --- permissions ------------------------------------------------------------

// EffectiveRoleIDs merges the roles a user holds implicitly, by being connected
// and by having claimed an account, with the ones granted explicitly.
func (h *Hub) EffectiveRoleIDs(u store.User, explicit []int64) []int64 {
	h.cacheMu.RLock()
	everyone, registered := h.everyoneID, h.registeredID
	h.cacheMu.RUnlock()

	seen := make(map[int64]struct{}, len(explicit)+2)
	out := make([]int64, 0, len(explicit)+2)
	add := func(id int64) {
		if id == 0 {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}

	add(everyone)
	if u.Registered() {
		add(registered)
	}
	for _, id := range explicit {
		add(id)
	}
	return out
}

// BasePermissions is the server-wide mask produced by a set of roles.
func (h *Hub) BasePermissions(roleIDs []int64) permissions.Permission {
	h.cacheMu.RLock()
	defer h.cacheMu.RUnlock()

	masks := make([]permissions.Permission, 0, len(roleIDs))
	for _, id := range roleIDs {
		if r, ok := h.roles[id]; ok {
			masks = append(masks, r.Permissions)
		}
	}
	return permissions.Resolve(masks)
}

// ChannelPermissions applies the overwrites of a channel and of every category
// above it, outermost first, on top of a server-wide mask. Overwrites therefore
// inherit down the tree: denying ViewChannel on a category hides everything
// inside it.
func (h *Hub) ChannelPermissions(base permissions.Permission, roleIDs []int64, channelID int64) permissions.Permission {
	chain := h.ancestorChain(channelID)
	everyone := h.EveryoneRoleID()

	perms := base
	for _, ch := range chain {
		perms = permissions.ResolveInChannel(perms, everyone, roleIDs, ch.Overwrites)
		if perms == permissions.None {
			return permissions.None
		}
	}
	return perms
}

// ancestorChain returns the channel and its ancestors, outermost first.
func (h *Hub) ancestorChain(channelID int64) []store.Channel {
	h.cacheMu.RLock()
	defer h.cacheMu.RUnlock()

	var reversed []store.Channel
	id := channelID
	for range maxChannelDepth {
		ch, ok := h.channels[id]
		if !ok {
			break
		}
		reversed = append(reversed, ch)
		if ch.ParentID == nil {
			break
		}
		id = *ch.ParentID
	}

	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed
}

// HighestRolePosition is the top of a user's role stack, which is what decides
// who may act on whom.
func (h *Hub) HighestRolePosition(roleIDs []int64) int {
	h.cacheMu.RLock()
	defer h.cacheMu.RUnlock()

	highest := 0
	for _, id := range roleIDs {
		if r, ok := h.roles[id]; ok && r.Position > highest {
			highest = r.Position
		}
	}
	return highest
}

// --- session registry -------------------------------------------------------

// Add registers an authenticated session. When the same identity is already
// connected the previous session is returned so the caller can close it: one
// connection per identity keeps presence unambiguous.
func (h *Hub) Add(s *Session) (displaced *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if prev, ok := h.byUser[s.UserID()]; ok && prev != s {
		delete(h.sessions, prev.ID)
		displaced = prev
	}
	h.sessions[s.ID] = s
	h.byUser[s.UserID()] = s
	return displaced
}

// Remove deregisters a session. It is safe to call for a session that was never
// added, and it will not evict a newer session that displaced this one.
func (h *Hub) Remove(s *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.sessions, s.ID)
	if current, ok := h.byUser[s.UserID()]; ok && current == s {
		delete(h.byUser, s.UserID())
	}
}

// Sessions snapshots the connected sessions.
func (h *Hub) Sessions() []*Session {
	h.mu.RLock()
	defer h.mu.RUnlock()

	out := make([]*Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, s)
	}
	return out
}

// SessionForUser finds the live session of one identity.
func (h *Hub) SessionForUser(userID int64) (*Session, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s, ok := h.byUser[userID]
	return s, ok
}

// OnlineCount is how many identities are connected.
func (h *Hub) OnlineCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.sessions)
}

// Full reports whether the server has reached its configured capacity.
func (h *Hub) Full() bool {
	return h.OnlineCount() >= h.cfg.Server.MaxUsers
}

// --- fan-out ----------------------------------------------------------------

// Broadcast delivers an event to every connected session.
func (h *Hub) Broadcast(env protocol.Envelope) {
	h.BroadcastTo(env, nil)
}

// BroadcastTo delivers an event to the sessions that satisfy want. A nil
// predicate means everybody.
func (h *Hub) BroadcastTo(env protocol.Envelope, want func(*Session) bool) {
	for _, s := range h.Sessions() {
		if want != nil && !want(s) {
			continue
		}
		s.Send(env)
	}
}

// BroadcastChannelEvent delivers an event only to the sessions allowed to see
// the channel it concerns, so a restricted channel never leaks its existence.
func (h *Hub) BroadcastChannelEvent(env protocol.Envelope, channelID int64) {
	h.BroadcastTo(env, func(s *Session) bool {
		return h.SessionCanView(s, channelID)
	})
}

// SessionCanView reports whether a session may see a channel.
func (h *Hub) SessionCanView(s *Session, channelID int64) bool {
	base, roleIDs := s.Permissions()
	return h.ChannelPermissions(base, roleIDs, channelID).Has(permissions.ViewChannel)
}

// VisibleChannels filters the channel tree down to what a session may see.
func (h *Hub) VisibleChannels(s *Session) []store.Channel {
	all := h.SortedChannels()
	out := make([]store.Channel, 0, len(all))
	for _, c := range all {
		if h.SessionCanView(s, c.ID) {
			out = append(out, c)
		}
	}
	return out
}

// MaskUser masks sensitive presence for other viewers (e.g. invisible status or hidden voice channels).
func (h *Hub) MaskUser(viewer *Session, view protocol.User) protocol.User {
	if viewer.UserID() != view.ID && (view.Status == "invisible" || view.Status == "offline") {
		view.Online = false
		view.Status = "offline"
		view.ChannelID = nil
	}
	if view.ChannelID != nil && !h.SessionCanView(viewer, *view.ChannelID) {
		view.ChannelID = nil
	}
	return view
}

// BroadcastUserUpdated announces a user update, properly masked per viewer.
func (h *Hub) BroadcastUserUpdated(user protocol.User) {
	for _, s := range h.Sessions() {
		masked := h.MaskUser(s, user)
		s.Send(protocol.Event(protocol.EvUserUpdated, protocol.UserEvent{User: masked}))
	}
}
