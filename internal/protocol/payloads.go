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
	Name                string  `json:"name"`
	Description         string  `json:"description"`
	ProtocolVersion     int     `json:"protocolVersion"`
	SoftwareVersion     string  `json:"softwareVersion"`
	MaxUsers            int     `json:"maxUsers"`
	OnlineUsers         int     `json:"onlineUsers"`
	PasswordProtected   bool    `json:"passwordProtected"`
	RegistrationEnabled bool    `json:"registrationEnabled"`
	GuestsAllowed       bool    `json:"guestsAllowed"`
	VoiceMode           string  `json:"voiceMode"`
	Uploads             Uploads `json:"uploads"`
	KlipyAPIKey         string  `json:"klipyApiKey,omitempty"`
}

// Uploads tells a client what this server accepts before it sends anything, so
// a file that is too large is refused in the picker rather than after a long
// transfer. Byte counts are decimal strings for the same reason permission
// masks are: they can exceed what a JavaScript number represents exactly.
type Uploads struct {
	Enabled bool `json:"enabled"`
	// MaxFileBytes caps one file.
	MaxFileBytes string `json:"maxFileBytes"`
	// MaxAvatarBytes caps one user avatar image.
	MaxAvatarBytes string `json:"maxAvatarBytes"`
	// MaxBannerBytes caps one user banner image.
	MaxBannerBytes string `json:"maxBannerBytes"`
	// MaxTotalBytes caps everything the server stores. "0" means no ceiling.
	MaxTotalBytes string `json:"maxTotalBytes"`
	// UsedBytes is how much of that ceiling is already taken.
	UsedBytes string `json:"usedBytes"`
	// MaxPerMessage caps how many files one message may carry.
	MaxPerMessage int `json:"maxPerMessage"`
}

// User is a member of the server. Guests are users too: they simply have no
// username yet.
type User struct {
	ID           int64   `json:"id"`
	Nickname     string  `json:"nickname"`
	Username     *string `json:"username"` // nil while the user is still a guest
	Registered   bool    `json:"registered"`
	Roles        []int64 `json:"roles"`
	ChannelID    *int64  `json:"channelId"` // nil when the user is in no channel
	Online       bool    `json:"online"`
	Status       string  `json:"status"` // "online", "idle", "dnd", "offline", "invisible"
	CustomStatus string  `json:"customStatus,omitempty"`
	Avatar       *string `json:"avatar,omitempty"`
	Banner       *string `json:"banner,omitempty"`
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
	// Attachments are the files posted with the message. They live and die
	// with it: deleting the message deletes the files.
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment is one file carried by a message.
//
// URL is relative to the server root, so a client that reached the server by
// address, by hostname or through a reverse proxy all build the same working
// link from the address they already hold. It embeds an unguessable key and
// needs no further authentication, which is what lets an <img>, <audio> or
// <video> tag load it directly.
type Attachment struct {
	ID          int64  `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	// Size is a decimal string: a file may be larger than 2^53 bytes.
	Size string `json:"size"`
	URL  string `json:"url"`
	// Width and Height are set for images whose dimensions could be read.
	Width  *int `json:"width,omitempty"`
	Height *int `json:"height,omitempty"`
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
	KlipyAPIKey *string `json:"klipyApiKey,omitempty"`
}

type UserUpdateRequest struct {
	// UserID defaults to the caller when omitted.
	UserID       *int64   `json:"userId,omitempty"`
	Nickname     *string  `json:"nickname,omitempty"`
	Status       *string  `json:"status,omitempty"`
	CustomStatus *string  `json:"customStatus,omitempty"`
	Avatar       **string `json:"avatar,omitempty"`
	Banner       **string `json:"banner,omitempty"`
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
	// Attachments are the ids returned by POST /upload. A message may carry
	// files with no text of its own, which is the one case where empty content
	// is accepted.
	Attachments []int64 `json:"attachments,omitempty"`
}

// MessageHistoryRequest reads one page of a channel.
//
// The three cursors are exclusive of one another and all are exclusive of the
// message they name, so paging stays stable while new messages arrive at the
// end. Sending none of them reads the newest page.
type MessageHistoryRequest struct {
	ChannelID int64 `json:"channelId"`
	// Before pages backwards, stopping short of this id.
	Before int64 `json:"before,omitempty"`
	// After pages forwards, starting past this id. It is what a client walks
	// back to the present with after jumping into the middle of a channel.
	After int64 `json:"after,omitempty"`
	// Around centres a page on this id, which is how a search result is opened
	// in the conversation it came from.
	Around int64 `json:"around,omitempty"`
	Limit  int   `json:"limit,omitempty"`
}

// MessageHistoryResult is ordered oldest first, the order it is rendered in.
type MessageHistoryResult struct {
	ChannelID int64     `json:"channelId"`
	Messages  []Message `json:"messages"`
	// HasMore reports whether older messages remain before the first one here.
	HasMore bool `json:"hasMore"`
	// HasMoreAfter reports whether newer messages remain past the last one
	// here, which is how a client knows it is holding the present or a window
	// somewhere behind it.
	HasMoreAfter bool `json:"hasMoreAfter"`
}

// Search sort orders.
const (
	SortNewest    = "newest"    // most recent first, the default
	SortOldest    = "oldest"    // least recent first
	SortRelevance = "relevance" // best text match first, newest breaking ties
)

// Kinds of content a search can require a message to carry.
const (
	HasLink  = "link"  // the text contains an http(s) URL
	HasFile  = "file"  // the message carries an attachment of any kind
	HasImage = "image" // ... an image
	HasVideo = "video" // ... a video
	HasSound = "sound" // ... an audio file
)

// MessageSearchRequest looks through every channel the caller may read.
//
// Every field narrows the result, and all of them are combined with AND: a
// search with a query and two channels means "this text, in either of these
// channels". Within one repeated field the entries are alternatives, so two
// authors mean "either of them", which is what a filter chip in the interface
// reads as.
type MessageSearchRequest struct {
	// Query is free text. Whitespace separates terms, all of which must appear
	// somewhere in the message; double quotes hold a phrase together.
	Query string `json:"query,omitempty"`
	// ChannelIDs narrows to these channels. Ones the caller may not read are
	// dropped rather than refused, exactly as they are absent from the tree.
	ChannelIDs []int64 `json:"channelIds,omitempty"`
	AuthorIDs  []int64 `json:"authorIds,omitempty"`
	// Has requires each named kind of content to be present.
	Has []string `json:"has,omitempty"`
	// After and Before bound the send time, in Unix seconds. After is
	// inclusive and Before is exclusive, so one day is [start, start+86400).
	After  int64  `json:"after,omitempty"`
	Before int64  `json:"before,omitempty"`
	Sort   string `json:"sort,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	Offset int    `json:"offset,omitempty"`
}

// MessageSearchHit is one match and the conversation immediately around it.
//
// The neighbours travel with the hit because a line of chat rarely means
// anything alone: what makes a result recognisable is the message before it.
type MessageSearchHit struct {
	Message Message `json:"message"`
	// Before and After hold the message either side of the hit in its own
	// channel, when there is one.
	Before *Message `json:"before,omitempty"`
	After  *Message `json:"after,omitempty"`
}

// MessageSearchResult is one page of matches.
type MessageSearchResult struct {
	Hits []MessageSearchHit `json:"hits"`
	// Total is how many messages matched in all, which is what lets a client
	// page and say how much it found.
	Total  int `json:"total"`
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
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
