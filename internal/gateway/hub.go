package gateway

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/publicip"
	"github.com/aural-chat/aural-server/internal/store"
	"github.com/aural-chat/aural-server/internal/uploads"
	"github.com/aural-chat/aural-server/internal/voice"
)

// The window in which the relay's advertised address is re-resolved.
//
// publicIPInterval is the ordinary cadence: a provider-forced address change
// is a thing that happens every few days at most, so checking every few
// minutes is already generous. publicIPRetry is used after a failure, because
// a lookup that failed at startup — a resolver not up yet, a network not up
// yet — is one worth trying again shortly rather than in five minutes.
const (
	publicIPInterval = 5 * time.Minute
	publicIPRetry    = 30 * time.Second
	publicIPTimeout  = 10 * time.Second
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
	// ownerID is the identity that owns the server, which is not a role and is
	// therefore cached beside the role table rather than in it.
	ownerID int64

	// The audio plane. voiceRooms holds who has media up in each voice
	// channel, voiceEpochs counts the elections that channel has seen, and
	// relay is the server-hosted forwarder — nil on a server that could not
	// build one, which is the one failure that has to leave the rest working.
	voiceMu     sync.Mutex
	voiceRooms  map[int64]*voiceRoom
	voiceEpochs map[int64]int64
	relay       *voice.Relay

	// publicIP is the address the relay currently advertises, which is not
	// necessarily the one in the configuration file: an operator on a home
	// connection names a hostname, or nothing at all, and this is what that
	// resolved to the last time it was looked up.
	publicIP   atomic.Pointer[string]
	publicAddr *publicip.Resolver
}

// NewHub builds a hub and primes its caches from the database. cfgPath is where
// runtime configuration changes are written back.
func NewHub(ctx context.Context, cfg *config.Config, cfgPath string, st *store.Store, log *slog.Logger) (*Hub, error) {
	h := &Hub{
		cfg:         cfg,
		cfgPath:     cfgPath,
		st:          st,
		log:         log,
		sessions:    map[int64]*Session{},
		byUser:      map[int64]*Session{},
		voiceRooms:  map[int64]*voiceRoom{},
		voiceEpochs: map[int64]int64{},
	}

	// The address the relay advertises is resolved before it is built, so a
	// server whose voice.public_ip is a hostname starts with the right one
	// rather than with none until the first watch tick.
	h.publicAddr = publicip.New(cfg.Voice.PublicIP, iceURLs(cfg.Voice.ICEServers))
	h.storePublicIP("")
	if resolved, err := h.resolvePublicIP(ctx); err != nil {
		if !errors.Is(err, publicip.ErrNoSource) {
			// Not fatal. The relay advertises the addresses of its own
			// interfaces, which is what an unconfigured server does anyway,
			// and the watcher will correct it as soon as the lookup works.
			log.Warn("could not resolve the address to advertise for voice",
				slog.String("source", h.publicAddr.Describe()), slog.Any("error", err))
		}
	} else {
		h.storePublicIP(resolved)
		// Said out loud at startup, because it is the first thing to check
		// when voice does not work and the only thing that is otherwise
		// invisible: everything else about a call is negotiated over a socket
		// that is plainly working.
		log.Info("advertising an address for voice",
			slog.String("address", resolved),
			slog.String("source", h.publicAddr.Describe()))
	}

	// The relay is built whatever the mode, so that switching to server_host
	// at runtime needs nothing but a reconfiguration. It binds no port and
	// starts no goroutine until somebody actually calls.
	relay, err := voice.NewRelay(relaySettings(cfg.Voice, h.PublicIP()), log, h.onRelayGone)
	if err != nil {
		// A server that cannot relay audio is still a server. Voice reports
		// itself as unavailable and everything else carries on.
		log.Error("the audio plane could not be started", slog.Any("error", err))
	} else {
		h.relay = relay
	}
	if err := h.ReloadRoles(ctx); err != nil {
		return nil, err
	}
	if err := h.ReloadOwner(ctx); err != nil {
		return nil, err
	}
	if err := h.ReloadChannels(ctx); err != nil {
		return nil, err
	}
	if cfg.Uploads.Enabled {
		// The quota is measured against what the database already accounts
		// for, so a restart neither forgets stored files nor double-counts them.
		// Both tables count: an avatar takes the same disk an attachment does,
		// and leaving it out drifts the ceiling further from the truth with
		// every picture uploaded.
		used, err := st.TotalAttachmentBytes(ctx)
		if err != nil {
			return nil, err
		}
		pictures, err := st.TotalProfileMediaBytes(ctx)
		if err != nil {
			return nil, err
		}
		used += pictures
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
	return h.PermissionsFor(u.ID, roleIDs), roleIDs, nil
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
func (h *Hub) ServerIdentity() (name, description string) {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	return h.cfg.Server.Name, h.cfg.Server.Description
}

// DirectMessagesEnabled reports whether this server carries private
// conversations at all. It is read on every op behind them as well as
// advertised in the server preview, so a client that ignores the preview is
// still refused rather than half served.
func (h *Hub) DirectMessagesEnabled() bool {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	return h.cfg.Server.AllowDirectMessages
}

// KlipyAPIKey is the operator's Klipy credential, which only the proxy handler
// is meant to read. It is deliberately not part of ServerIdentity: that feeds
// the public server preview, and a secret has no business travelling with it.
func (h *Hub) KlipyAPIKey() string {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	return h.cfg.Integrations.KlipyAPIKey
}

// updateServerIdentity applies configuration updates and persists them.
//
// A voice change is the one that reaches further than the file: the codec
// parameters of a live session were negotiated before it, so voiceChanged tells
// the caller to start every room over.
func (h *Hub) updateServerIdentity(
	setName bool, name string,
	setDescription bool, description string,
	setKlipy bool, klipyApiKey string,
	newVoice *config.Voice,
) (voiceChanged bool, err error) {
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
	if newVoice != nil {
		// The deployment half of the configuration is the machine's, not the
		// administrator's: it is carried across rather than taken from the
		// request, which is why the request has no fields for it.
		newVoice.PublicIP = h.cfg.Voice.PublicIP
		newVoice.UDPPortMin = h.cfg.Voice.UDPPortMin
		newVoice.UDPPortMax = h.cfg.Voice.UDPPortMax
		newVoice.ICEServers = h.cfg.Voice.ICEServers
		voiceChanged = !h.cfg.Voice.SameAs(*newVoice)
		h.cfg.Voice = *newVoice
	}
	snapshot := *h.cfg
	h.cfgMu.Unlock()

	if voiceChanged && h.relay != nil {
		if err := h.relay.Reconfigure(relaySettings(snapshot.Voice, h.PublicIP())); err != nil {
			h.log.Error("reconfigure the audio plane", slog.Any("error", err))
		}
	}
	if h.cfgPath == "" {
		return voiceChanged, nil
	}
	return voiceChanged, config.Save(h.cfgPath, snapshot)
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

// AdminRoleID is the id of the managed role that carries Administrator.
func (h *Hub) AdminRoleID() int64 {
	h.cacheMu.RLock()
	defer h.cacheMu.RUnlock()
	return h.adminID
}

// ReloadOwner refreshes the cached owner. Call it after ownership changes.
func (h *Hub) ReloadOwner(ctx context.Context) error {
	id, err := h.st.OwnerUserID(ctx)
	if err != nil {
		return err
	}
	h.cacheMu.Lock()
	h.ownerID = id
	h.cacheMu.Unlock()
	return nil
}

// OwnerUserID is the identity that owns the server, or zero when nobody has
// claimed it yet.
func (h *Hub) OwnerUserID() int64 {
	h.cacheMu.RLock()
	defer h.cacheMu.RUnlock()
	return h.ownerID
}

// IsOwner reports whether an identity owns the server.
//
// Ownership is a property of the identity rather than a role it holds, exactly
// as in Discord: it grants every permission and outranks every role, it cannot
// be edited away in the role editor, and taking every role off the owner
// changes none of it.
func (h *Hub) IsOwner(userID int64) bool {
	if userID == 0 {
		return false
	}
	h.cacheMu.RLock()
	defer h.cacheMu.RUnlock()
	return h.ownerID == userID
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

// PermissionsFor is the server-wide mask of one identity: what its roles grant,
// or everything at all when it owns the server.
func (h *Hub) PermissionsFor(userID int64, roleIDs []int64) permissions.Permission {
	if h.IsOwner(userID) {
		return permissions.All
	}
	return h.BasePermissions(roleIDs)
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

// ownerRank sits above every role position. It is what puts the owner out of
// everybody else's reach: no role can be moved high enough to match it, so no
// administrator can act on the owner or edit a role the owner sits above.
const ownerRank = math.MaxInt

// RankOf is the authority one identity has over another and over the role
// stack. It is the top of their roles, unless they own the server, in which
// case it is above everything.
func (h *Hub) RankOf(userID int64, roleIDs []int64) int {
	if h.IsOwner(userID) {
		return ownerRank
	}
	return h.HighestRolePosition(roleIDs)
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

// Add registers an authenticated session, unless the server is at capacity.
//
// When the same identity is already connected the previous session is returned
// so the caller can close it: one connection per identity keeps presence
// unambiguous. Somebody reconnecting therefore takes no new place and is never
// refused for want of one.
//
// The capacity check lives in here rather than at the call site because it has
// to happen under the same lock as the insert. Asking whether there is room
// and then taking it are two moments, and several authentications landing
// between them is exactly how a server ends up over its own limit.
func (h *Hub) Add(s *Session) (displaced *Session, full bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	prev, held := h.byUser[s.UserID()]
	if !held && len(h.sessions) >= h.cfg.Server.MaxUsers {
		return nil, true
	}
	if held && prev != s {
		delete(h.sessions, prev.ID)
		displaced = prev
	}
	h.sessions[s.ID] = s
	h.byUser[s.UserID()] = s
	return displaced, false
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

// Members is the member list one session sees: everybody who holds an account,
// plus the guests who are connected right now.
//
// A member who is not connected is listed as offline rather than left out,
// which is what gives hiding somewhere to hide: an invisible member sits in
// the list exactly as somebody genuinely away does, and the two are the same
// entry. A guest has no such entry to disappear into — the identity itself
// only exists while its connection does — so a hidden guest is still left out
// of the list altogether.
func (h *Hub) Members(ctx context.Context, viewer *Session) ([]protocol.User, error) {
	members, err := h.st.ListMembers(ctx)
	if err != nil {
		return nil, err
	}

	sessions := h.Sessions()
	users := make([]protocol.User, 0, len(members)+len(sessions))
	connected := make(map[int64]bool, len(sessions))
	for _, other := range sessions {
		view := other.view()
		if other.UserID() != viewer.UserID() && HidesPresence(view.Status) && !view.Registered {
			continue
		}
		connected[other.UserID()] = true
		users = append(users, h.MaskUser(viewer, view))
	}

	// Whoever is left is away, hidden members included: they were masked into
	// the same shape above, so the entry they already took is the one that
	// stands.
	for _, m := range members {
		if connected[m.ID] {
			continue
		}
		users = append(users, offlineView(m.User, m.RoleIDs, h.IsOwner(m.ID)))
	}
	return users, nil
}

// offlineMemberView is the view of one member who has no session, which is what
// a change made to somebody absent has to carry.
func (h *Hub) offlineMemberView(ctx context.Context, u store.User) (protocol.User, error) {
	roleIDs, err := h.st.RoleIDsForUser(ctx, u.ID)
	if err != nil {
		return protocol.User{}, err
	}
	return offlineView(u, roleIDs, h.IsOwner(u.ID)), nil
}

// HidesPresence reports whether a status is one that makes a connected user
// look offline to everyone but themselves.
//
// Now that the list carries absent members too, looking offline is a place to
// hide rather than the tell it used to be: a hidden user is one more name in
// the crowd of people who are away. What the status still decides is traffic.
// Somebody absent generates no events at all, so a hidden user may not either.
func HidesPresence(status string) bool {
	return status == "invisible" || status == "offline"
}

// MaskUser is what stands between a user view and somebody who is not its
// subject: it drops a channel the viewer may not see, and flattens a hidden
// user into the same offline entry the list already holds for everybody away.
//
// Flattening is the whole of hiding for a member, rather than a fallback
// behind leaving them out. A guest is the exception: an unclaimed identity has
// no offline entry to be flattened into, so callers building a list have to
// leave a hidden guest out of it.
func (h *Hub) MaskUser(viewer *Session, view protocol.User) protocol.User {
	if viewer.UserID() != view.ID && HidesPresence(view.Status) {
		view.Online = false
		view.Status = "offline"
		// A custom status that keeps changing on an offline user is itself a
		// tell, so it goes with the rest of the presence.
		view.CustomStatus = ""
		view.ChannelID = nil
	}
	if view.ChannelID != nil && !h.SessionCanView(viewer, *view.ChannelID) {
		view.ChannelID = nil
	}
	if viewer.UserID() != view.ID {
		// What somebody accepts privately is their setting to read and nobody
		// else's to see. Finding out that a message will not be delivered is
		// what sending one is for.
		view.DMPrivacy = ""
	}
	return view
}

// BroadcastUserUpdated announces a change to a connected user whose presence
// has not changed, which is every change but the one that sets a status.
func (h *Hub) BroadcastUserUpdated(view protocol.User) {
	h.BroadcastUserPresence(view.Status, view)
}

// BroadcastMemberUpdated announces a change somebody else made to a member: a
// rename by a moderator, a role granted or revoked. These are the only changes
// that reach a member who is not here, so this is the one path that speaks
// about them at all.
//
// It says nothing about presence, and so has nothing to keep quiet about: the
// masked view a hidden member goes out as is the offline entry every viewer
// already holds for them, which is the same frame an absent member's rename
// produces. What must not come through here is a change a hidden user makes to
// their own profile — only they could have made it, so it would say they are
// here. BroadcastUserPresence is where those go, and it stays silent.
func (h *Hub) BroadcastMemberUpdated(view protocol.User) {
	// The subject always sees themselves whole; masking is for everybody else.
	if own, ok := h.SessionForUser(view.ID); ok {
		own.Send(protocol.Event(protocol.EvUserUpdated, protocol.UserEvent{User: view}))
	}
	for _, s := range h.Sessions() {
		if s.UserID() == view.ID {
			continue
		}
		s.Send(protocol.Event(protocol.EvUserUpdated, protocol.UserEvent{User: h.MaskUser(s, view)}))
	}
}

// BroadcastUserPresence announces a change to a connected user. was is the
// status the rest of the server last saw them with, which is what decides how
// the change has to read to everybody else.
//
// A genuinely offline user generates no events at all, so neither may a hidden
// one: becoming hidden is announced as a departure, staying hidden is announced
// to nobody, and becoming visible again arrives as an ordinary update, which is
// the same frame a client already treats as an arrival.
//
// The departure is not a removal. A member stays in every list, in the offline
// part of it, which is where hiding puts somebody and where the connection
// ending would have put them anyway.
func (h *Hub) BroadcastUserPresence(was string, view protocol.User) {
	// The subject always sees themselves whole; masking is for everybody else.
	if own, ok := h.SessionForUser(view.ID); ok {
		own.Send(protocol.Event(protocol.EvUserUpdated, protocol.UserEvent{User: view}))
	}

	hiddenBefore, hiddenNow := HidesPresence(was), HidesPresence(view.Status)
	for _, s := range h.Sessions() {
		if s.UserID() == view.ID {
			continue
		}
		switch {
		case hiddenNow && hiddenBefore:
			// They are not in this viewer's list to update, and saying
			// anything at all would reveal that they could have been.
		case hiddenNow:
			s.Send(protocol.Event(protocol.EvUserDisconnected, protocol.UserDisconnectedEvent{UserID: view.ID}))
		default:
			s.Send(protocol.Event(protocol.EvUserUpdated, protocol.UserEvent{User: h.MaskUser(s, view)}))
		}
	}
}
