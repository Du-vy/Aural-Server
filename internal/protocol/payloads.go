package protocol

// Channel types.
const (
	ChannelCategory = "category"
	ChannelText     = "text"
	ChannelVoice    = "voice"
)

// Voice hosting modes a server can advertise. They differ only in who relays:
// the wire format of the audio plane is the same either way.
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
	// VoiceMode is kept alongside Voice because it has been in this frame since
	// v0.1 and a client older than the audio plane still reads it.
	VoiceMode string  `json:"voiceMode"`
	Voice     Voice   `json:"voice"`
	Uploads   Uploads `json:"uploads"`
	// KlipyEnabled reports that this server holds a Klipy credential and will
	// proxy GIF and sticker lookups. The credential itself never leaves the
	// server: it is the operator's, not the client's, and this preview is
	// unauthenticated.
	KlipyEnabled bool `json:"klipyEnabled"`
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

// Voice is what a client is told about the audio plane before it opens a
// session, so it can configure its encoder once rather than discover the
// server's limits by being refused.
//
// It carries no ICE servers. Those may hold TURN credentials, and this
// structure travels in the unauthenticated server preview at GET /info; they
// are handed out in the reply to voice.connect instead, which is behind an
// identity.
type Voice struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode"`
	// SampleRate is the highest rate the encoder is asked for, in hertz. Opus
	// always runs on a 48 kHz clock, so this is a ceiling on quality rather
	// than a change of clock.
	SampleRate int `json:"sampleRate"`
	// Bitrate is where a client starts; MinBitrate and MaxBitrate bound where
	// it may go. All three are bits per second.
	Bitrate    int  `json:"bitrate"`
	MinBitrate int  `json:"minBitrate"`
	MaxBitrate int  `json:"maxBitrate"`
	FEC        bool `json:"fec"`
	DTX        bool `json:"dtx"`
	Stereo     bool `json:"stereo"`
	// MaxParticipants caps live audio sessions in one channel. Zero leaves it
	// to the channel's own user limit.
	MaxParticipants int `json:"maxParticipants"`
}

// ICEServer is one STUN or TURN server, in the shape RTCConfiguration expects.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// ICECandidate is one trickled ICE candidate, field for field as the browser
// serialises RTCIceCandidate.
type ICECandidate struct {
	Candidate        string  `json:"candidate"`
	SDPMid           *string `json:"sdpMid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	UsernameFragment *string `json:"usernameFragment,omitempty"`
}

// Signalling frame kinds carried by voice.signal.
const (
	SignalOffer     = "offer"
	SignalAnswer    = "answer"
	SignalCandidate = "candidate"
	// SignalEnd says the sender has finished trickling candidates, which lets
	// the far end stop waiting for a path that is not coming.
	SignalEnd = "end"
)

// ServerPeer is the target id that addresses the server's own relay rather
// than another client. It cannot collide with a user: identities start at 1.
const ServerPeer int64 = 0

// VoiceState is what everybody in a channel is told about one participant.
//
// Sitting in a voice channel and holding a live audio session are two different
// things: a client with no microphone, or one whose media never came up, is a
// member of the channel with Connected false. Presence is user.move; this is
// the audio on top of it.
type VoiceState struct {
	UserID    int64 `json:"userId"`
	ChannelID int64 `json:"channelId"`
	Connected bool  `json:"connected"`
	// SelfMute and SelfDeaf are the participant's own choices.
	SelfMute bool `json:"selfMute"`
	SelfDeaf bool `json:"selfDeaf"`
	// Mute and Deaf are imposed by a moderator, or by lacking Speak. They are
	// separate from the self flags because unmuting yourself must not undo
	// being muted by somebody else.
	Mute bool `json:"mute"`
	Deaf bool `json:"deaf"`
	// Host reports that this participant relays the channel, which only ever
	// happens in client_host mode.
	Host bool `json:"host"`
}

// Muted reports whether any reason to stop this participant transmitting
// applies, which is the only question a renderer or a relay ever asks.
func (v VoiceState) Muted() bool { return v.SelfMute || v.Mute }

// Deafened reports the same for receiving. Deafening implies muting, exactly
// as it does everywhere else, because listening is why you are in the channel.
func (v VoiceState) Deafened() bool { return v.SelfDeaf || v.Deaf }

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
	// ICEServers are the STUN and TURN servers this client should use.
	//
	// They are here rather than only in the voice.connect reply because a
	// server-hosted session has to build its peer connection in order to
	// produce the offer that reply answers: the configuration is needed one
	// step before the reply that would otherwise carry it. This snapshot is
	// authenticated, which is the property that keeps a TURN credential out of
	// the public preview.
	ICEServers []ICEServer `json:"iceServers"`
	// VoiceStates covers every participant of every voice channel the caller
	// may see. It is a list rather than a field on User because a user has one
	// identity and at most one voice state, and only while they are in a
	// channel: folding it into User would put an empty object on everybody.
	VoiceStates []VoiceState `json:"voiceStates"`
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
	// Voice replaces the whole audio-plane configuration at once. It is not a
	// per-field patch because the fields constrain one another - a bitrate
	// range has to be read together to be checked - and a half-applied range
	// is not a state worth being able to reach.
	Voice *VoiceSettings `json:"voice,omitempty"`
}

// VoiceSettings is the part of the audio plane an administrator may change at
// runtime. The deployment details - the public address, the port range, the
// ICE servers - are deliberately absent: they belong to the machine, not to
// whoever happens to hold ManageServer.
type VoiceSettings struct {
	Enabled         bool   `json:"enabled"`
	Mode            string `json:"mode"`
	SampleRate      int    `json:"sampleRate"`
	Bitrate         int    `json:"bitrate"`
	MinBitrate      int    `json:"minBitrate"`
	MaxBitrate      int    `json:"maxBitrate"`
	FEC             bool   `json:"fec"`
	DTX             bool   `json:"dtx"`
	Stereo          bool   `json:"stereo"`
	MaxParticipants int    `json:"maxParticipants"`
}

type UserUpdateRequest struct {
	// UserID defaults to the caller when omitted.
	UserID       *int64  `json:"userId,omitempty"`
	Nickname     *string `json:"nickname,omitempty"`
	Status       *string `json:"status,omitempty"`
	CustomStatus *string `json:"customStatus,omitempty"`
	// Avatar and Banner are absent to leave a picture alone and empty to remove
	// it. A JSON null cannot carry that difference: encoding/json turns both an
	// absent key and an explicit null into a nil pointer, so a pointer-to-a-
	// pointer looks exactly like an untouched field when the client sends null.
	// An empty string is what "remove this picture" looks like on the wire, and
	// it can never collide with a real value: a picture must name a file this
	// server stores.
	Avatar *string `json:"avatar,omitempty"`
	Banner *string `json:"banner,omitempty"`
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

// --- voice -------------------------------------------------------------------

// VoiceConnectRequest opens a media session in the voice channel the caller is
// already sitting in.
//
// In server_host mode SDP carries the caller's offer and the reply carries the
// server's answer. In client_host mode it is empty: the channel's host peer is
// the one that offers, and the reply only says who that is.
type VoiceConnectRequest struct {
	ChannelID int64  `json:"channelId"`
	SDP       string `json:"sdp,omitempty"`
}

// VoiceConnectResult is everything a client needs to bring media up.
type VoiceConnectResult struct {
	ChannelID int64  `json:"channelId"`
	Mode      string `json:"mode"`
	// SDP is the server's answer in server_host mode, and empty otherwise.
	SDP string `json:"sdp,omitempty"`
	// HostUserID names the peer that relays this channel in client_host mode.
	// It is the caller itself when the caller was elected, which is what tells
	// a first arrival that it is the one everybody else will dial.
	HostUserID *int64 `json:"hostUserId,omitempty"`
	// HostEpoch increments on every election, so a client can drop signalling
	// that belongs to a host that has already been replaced.
	HostEpoch  int64       `json:"hostEpoch,omitempty"`
	ICEServers []ICEServer `json:"iceServers"`
	Voice      Voice       `json:"voice"`
	// Participants is the voice state of everyone already in the channel,
	// which saves a joining client from having to infer it from events that
	// were sent before it was listening.
	Participants []VoiceState `json:"participants"`
}

// VoiceSignalRequest carries one SDP or ICE frame towards a peer.
type VoiceSignalRequest struct {
	// TargetID is the peer this is for. ServerPeer addresses the server's own
	// relay, which is the only target that exists in server_host mode.
	TargetID  int64         `json:"targetId"`
	Kind      string        `json:"kind"`
	SDP       string        `json:"sdp,omitempty"`
	Candidate *ICECandidate `json:"candidate,omitempty"`
	// Tracks maps an SDP media id to the user whose audio it carries, and
	// travels with an offer in client_host mode only.
	//
	// The server-hosted relay does not need it: it names each participant in
	// the stream id it sends, and a receiver reads the identity straight off
	// the track. A relaying client cannot do that — a browser forwarding
	// somebody else's track has no way to rename it — so the host says which
	// media id is whose, and the server passes it along without reading it.
	Tracks map[string]int64 `json:"tracks,omitempty"`
}

// VoiceStateRequest sets the caller's own mute and deafen. Absent fields are
// left alone, which is what lets a deafen toggle not disturb a mute.
type VoiceStateRequest struct {
	SelfMute *bool `json:"selfMute,omitempty"`
	SelfDeaf *bool `json:"selfDeaf,omitempty"`
}

// VoiceModerateRequest mutes or deafens somebody else. It needs MuteUsers or
// DeafenUsers, and the caller must outrank the target.
type VoiceModerateRequest struct {
	UserID int64 `json:"userId"`
	Mute   *bool `json:"mute,omitempty"`
	Deaf   *bool `json:"deaf,omitempty"`
}

// VoiceSpeakingRequest announces that the caller started or stopped speaking.
// It is sent on transitions only: a frame per packet would be a second audio
// stream over the control socket.
type VoiceSpeakingRequest struct {
	Speaking bool `json:"speaking"`
}

// VoiceStateEvent reports one participant's voice state to the channel.
type VoiceStateEvent struct {
	State VoiceState `json:"state"`
}

// VoiceSpeakingEvent is the same transition, fanned out to the channel.
type VoiceSpeakingEvent struct {
	UserID    int64 `json:"userId"`
	ChannelID int64 `json:"channelId"`
	Speaking  bool  `json:"speaking"`
}

// VoiceSignalEvent delivers a signalling frame. FromUserID is ServerPeer when
// the server's own relay sent it.
type VoiceSignalEvent struct {
	FromUserID int64         `json:"fromUserId"`
	ChannelID  int64         `json:"channelId"`
	Kind       string        `json:"kind"`
	SDP        string        `json:"sdp,omitempty"`
	Candidate  *ICECandidate `json:"candidate,omitempty"`
	// Tracks is the map described on VoiceSignalRequest, relayed unread.
	Tracks map[string]int64 `json:"tracks,omitempty"`
}

// Actions a VoicePeerEvent can ask the host for.
const (
	PeerAdd    = "add"
	PeerRemove = "remove"
)

// VoicePeerEvent tells the host of a client_host channel that somebody is
// waiting to be dialled, or has gone. Only the host receives it.
type VoicePeerEvent struct {
	ChannelID int64  `json:"channelId"`
	UserID    int64  `json:"userId"`
	Action    string `json:"action"`
	Epoch     int64  `json:"epoch"`
}

// VoiceHostEvent announces the result of an election in a client_host channel.
// HostUserID is null when the channel has emptied.
type VoiceHostEvent struct {
	ChannelID  int64  `json:"channelId"`
	HostUserID *int64 `json:"hostUserId"`
	Epoch      int64  `json:"epoch"`
}

// Reasons a media session is reset.
const (
	ResetHostChanged   = "host_changed"   // the relaying peer was replaced
	ResetConfigChanged = "config_changed" // the audio plane was reconfigured
	ResetFailed        = "failed"         // the transport gave up
	ResetDisabled      = "disabled"       // voice was switched off
)

// VoiceResetEvent tells a client its media session is gone and that opening a
// new one is the way back. It is not an error: a host handover is the ordinary
// case, and the client is expected to reconnect rather than report anything.
type VoiceResetEvent struct {
	ChannelID int64  `json:"channelId"`
	Reason    string `json:"reason"`
}
