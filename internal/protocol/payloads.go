package protocol

// Channel types.
const (
	ChannelCategory = "category"
	ChannelText     = "text"
	ChannelVoice    = "voice"
)

// Voice hosting modes a server can advertise. Only the mode is exchanged in
// v1; the media plane itself lands in a later protocol revision.
const (
	VoiceModeClientHost = "client_host" // the first user in a channel relays its audio
	VoiceModeServerHost = "server_host" // the server relays all audio
)

// Managed role kinds. A managed role cannot be deleted and its membership is
// maintained by the server rather than by administrators.
const (
	ManagedNone       = ""
	ManagedEveryone   = "everyone"   // every connected user, guests included
	ManagedRegistered = "registered" // every user who has claimed an account
	ManagedAdmin      = "admin"      // granted by redeeming the owner token
)

// ServerInfo is the public description of a server. It is served both over the
// WebSocket (in Hello) and over plain HTTP at GET /info, so a client can preview
// a server before connecting to it.
type ServerInfo struct {
	Name                string `json:"name"`
	Description         string `json:"description"`
	ProtocolVersion     int    `json:"protocolVersion"`
	SoftwareVersion     string `json:"softwareVersion"`
	MaxUsers            int    `json:"maxUsers"`
	OnlineUsers         int    `json:"onlineUsers"`
	PasswordProtected   bool   `json:"passwordProtected"`
	RegistrationEnabled bool   `json:"registrationEnabled"`
	GuestsAllowed       bool   `json:"guestsAllowed"`
	VoiceMode           string `json:"voiceMode"`
}

// User is a member of the server. Guests are users too: they simply have no
// username yet.
type User struct {
	ID         int64   `json:"id"`
	Nickname   string  `json:"nickname"`
	Username   *string `json:"username"` // nil while the user is still a guest
	Registered bool    `json:"registered"`
	Roles      []int64 `json:"roles"`
	ChannelID  *int64  `json:"channelId"` // nil when the user is in no channel
	Online     bool    `json:"online"`
}

// Overwrite is a per-channel permission adjustment for one role. Allow and Deny
// are decimal strings so that a 64-bit mask survives a JavaScript client.
type Overwrite struct {
	RoleID int64  `json:"roleId"`
	Allow  string `json:"allow"`
	Deny   string `json:"deny"`
}

// Channel is a node of the channel tree. Categories hold other channels; text
// and voice channels are always leaves.
type Channel struct {
	ID         int64       `json:"id"`
	ParentID   *int64      `json:"parentId"`
	Name       string      `json:"name"`
	Type       string      `json:"type"`
	Topic      string      `json:"topic"`
	Position   int         `json:"position"`
	UserLimit  int         `json:"userLimit"` // voice only, 0 means unlimited
	Overwrites []Overwrite `json:"overwrites"`
}

// Role is a named bundle of permissions. Permissions is a decimal string for
// the same reason as Overwrite.
type Role struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Permissions string `json:"permissions"`
	Position    int    `json:"position"`
	Hoist       bool   `json:"hoist"`
	Managed     string `json:"managed"`
}

// Message is one post in a text channel.
//
// Author is carried alongside UserID because a client only knows the users who
// are currently connected: presence is not persisted, so the author of an old
// message is very often somebody the client has never seen. It is resolved
// server-side from the users table, so a rename shows up throughout the
// history rather than only on new messages.
type Message struct {
	ID        int64  `json:"id"`
	ChannelID int64  `json:"channelId"`
	UserID    *int64 `json:"userId"` // nil once an author's account is gone
	Author    string `json:"author"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"createdAt"`
	EditedAt  *int64 `json:"editedAt"`
}

// Hello is the first frame the server sends, before any authentication. It lets
// the client check protocol compatibility and decide which auth op to send.
type Hello struct {
	Server      ServerInfo `json:"server"`
	HeartbeatMs int        `json:"heartbeatMs"`
}

// Ready is the full state snapshot delivered once authentication succeeds.
type Ready struct {
	// SessionToken is returned by auth.guest and auth.login. The client stores
	// it and replays it through auth.token on the next connection. It is empty
	// when the session was itself resumed from a token.
	SessionToken string     `json:"sessionToken,omitempty"`
	User         User       `json:"user"`
	Users        []User     `json:"users"`
	Channels     []Channel  `json:"channels"`
	Roles        []Role     `json:"roles"`
	Permissions  string     `json:"permissions"` // resolved server-wide mask of the caller
	Server       ServerInfo `json:"server"`
}

// --- requests ---------------------------------------------------------------

type AuthGuestRequest struct {
	Nickname       string `json:"nickname"`
	ServerPassword string `json:"serverPassword,omitempty"`
}

type AuthTokenRequest struct {
	Token          string `json:"token"`
	ServerPassword string `json:"serverPassword,omitempty"`
}

type AuthLoginRequest struct {
	Username       string `json:"username"`
	Password       string `json:"password"`
	ServerPassword string `json:"serverPassword,omitempty"`
}

type AuthRegisterRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// AuthRegisterResult reports the account the identity was bound to.
type AuthRegisterResult struct {
	User User `json:"user"`
}

type ClaimAdminRequest struct {
	Token string `json:"token"`
}

type ServerUpdateRequest struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
}

type UserUpdateRequest struct {
	// UserID defaults to the caller when omitted.
	UserID   *int64  `json:"userId,omitempty"`
	Nickname *string `json:"nickname,omitempty"`
}

type UserMoveRequest struct {
	// UserID defaults to the caller when omitted.
	UserID *int64 `json:"userId,omitempty"`
	// ChannelID is null to leave the current channel.
	ChannelID *int64 `json:"channelId"`
}

type UserKickRequest struct {
	UserID int64  `json:"userId"`
	Reason string `json:"reason,omitempty"`
}

type ChannelCreateRequest struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	ParentID  *int64 `json:"parentId,omitempty"`
	Topic     string `json:"topic,omitempty"`
	Position  *int   `json:"position,omitempty"`
	UserLimit int    `json:"userLimit,omitempty"`
}

type ChannelUpdateRequest struct {
	ChannelID int64   `json:"channelId"`
	Name      *string `json:"name,omitempty"`
	Topic     *string `json:"topic,omitempty"`
	// ParentID is absent to leave the parent alone and null to detach the
	// channel to the tree root.
	ParentID   **int64     `json:"parentId,omitempty"`
	Position   *int        `json:"position,omitempty"`
	UserLimit  *int        `json:"userLimit,omitempty"`
	Overwrites []Overwrite `json:"overwrites,omitempty"`
}

type ChannelDeleteRequest struct {
	ChannelID int64 `json:"channelId"`
}

type MessageSendRequest struct {
	ChannelID int64  `json:"channelId"`
	Content   string `json:"content"`
}

// MessageHistoryRequest pages backwards through a channel. Before is the id to
// stop short of, so paging is stable while new messages arrive at the end.
type MessageHistoryRequest struct {
	ChannelID int64 `json:"channelId"`
	Before    int64 `json:"before,omitempty"`
	Limit     int   `json:"limit,omitempty"`
}

// MessageHistoryResult is ordered oldest first, the order it is rendered in.
type MessageHistoryResult struct {
	ChannelID int64     `json:"channelId"`
	Messages  []Message `json:"messages"`
	// HasMore reports whether older messages remain before the first one here.
	HasMore bool `json:"hasMore"`
}

type MessageEditRequest struct {
	MessageID int64  `json:"messageId"`
	Content   string `json:"content"`
}

type MessageDeleteRequest struct {
	MessageID int64 `json:"messageId"`
}

type RoleCreateRequest struct {
	Name        string `json:"name"`
	Color       string `json:"color,omitempty"`
	Permissions string `json:"permissions,omitempty"`
	Hoist       bool   `json:"hoist,omitempty"`
}

type RoleUpdateRequest struct {
	RoleID      int64   `json:"roleId"`
	Name        *string `json:"name,omitempty"`
	Color       *string `json:"color,omitempty"`
	Permissions *string `json:"permissions,omitempty"`
	Position    *int    `json:"position,omitempty"`
	Hoist       *bool   `json:"hoist,omitempty"`
}

type RoleDeleteRequest struct {
	RoleID int64 `json:"roleId"`
}

type RoleMembershipRequest struct {
	UserID int64 `json:"userId"`
	RoleID int64 `json:"roleId"`
}

// --- events -----------------------------------------------------------------

type UserEvent struct {
	User User `json:"user"`
}

type UserDisconnectedEvent struct {
	UserID int64 `json:"userId"`
}

// UserMovedEvent reports a channel change. Both ends are included so a client
// can update the two member lists without consulting its own state.
type UserMovedEvent struct {
	UserID int64  `json:"userId"`
	From   *int64 `json:"from"`
	To     *int64 `json:"to"`
}

type ChannelEvent struct {
	Channel Channel `json:"channel"`
}

type ChannelDeletedEvent struct {
	ChannelID int64 `json:"channelId"`
	// Cascaded lists descendants removed along with the channel.
	Cascaded []int64 `json:"cascaded"`
}

type MessageEvent struct {
	Message Message `json:"message"`
}

// MessageDeletedEvent carries the channel as well, so a client can drop the
// message without searching every channel it has cached.
type MessageDeletedEvent struct {
	MessageID int64 `json:"messageId"`
	ChannelID int64 `json:"channelId"`
}

type RoleEvent struct {
	Role Role `json:"role"`
}

type RoleDeletedEvent struct {
	RoleID int64 `json:"roleId"`
}

type ServerUpdatedEvent struct {
	Server ServerInfo `json:"server"`
}
