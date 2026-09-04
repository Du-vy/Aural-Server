package protocol

// Channel types.
//
// The four below ChannelVoice hold posts rather than a stream of messages: an
// entry with a title, and a thread of comments hanging off it. They differ in
// what an entry carries and in how a client lays them out, not in the ops that
// reach them, so PostChannel is what every check on the server asks.
const (
	ChannelCategory     = "category"
	ChannelText         = "text"
	ChannelVoice        = "voice"
	ChannelAnnouncement = "announcement" // a few write, everybody comments
	ChannelForum        = "forum"        // topics anybody may start
	ChannelMedia        = "media"        // entries whose point is the file
	ChannelCalendar     = "calendar"     // entries that happen at a time
)

// PostChannel reports whether a channel type holds posts.
func PostChannel(channelType string) bool {
	switch channelType {
	case ChannelAnnouncement, ChannelForum, ChannelMedia, ChannelCalendar:
		return true
	default:
		return false
	}
}

// RSVP responses to a calendar event.
const (
	RSVPGoing    = "going"
	RSVPMaybe    = "maybe"
	RSVPDeclined = "declined"
	// RSVPNone withdraws an answer. It is a response rather than a separate op
	// because "I have not said" and "I am not coming" are different things, and
	// a client that lets somebody take an answer back needs to say which.
	RSVPNone = ""
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
	ManagedAdmin      = "admin"      // every permission, and only the owner may edit it
)

// ServerInfo is the public description of a server. It is served both over the
// WebSocket (in Hello) and over plain HTTP at GET /info, so a client can preview
// a server before connecting to it.
type ServerInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Icon is the path of this server's picture, relative to the server's own
	// origin exactly as an avatar is, and empty when the server has none. A
	// client that reads it draws it wherever it would otherwise draw the first
	// letter of the name.
	Icon                string `json:"icon,omitempty"`
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
	// DirectMessages reports that this server carries private conversations.
	// An operator can switch them off, and a client that is told so hides that
	// whole interface rather than offering something every send is refused for.
	DirectMessages bool `json:"directMessages"`
	// Expressions is what this server accepts as a custom emoji, sticker or
	// soundboard clip. The client needs it before it uploads: the trimmer has
	// to know how long a sound may be, and the picker has to know when there
	// is no slot left.
	Expressions ExpressionLimits `json:"expressions"`
	// Registration is what this server will accept as a username and password.
	// It travels for the same reason the upload limits do: a client that knows
	// the policy says so under the field, and refuses in the form rather than
	// after a round trip that can only answer in the server's language.
	Registration RegistrationLimits `json:"registration"`
}

// RegistrationLimits is the account policy a client validates against before
// it sends anything.
type RegistrationLimits struct {
	MinPasswordLength int `json:"minPasswordLength"`
	MinUsernameLength int `json:"minUsernameLength"`
	MaxUsernameLength int `json:"maxUsernameLength"`
}

// ExpressionLimits is the ceiling on what a server carries for its own people.
// Byte counts are decimal strings, as everywhere else in this protocol.
type ExpressionLimits struct {
	MaxEmojis   int `json:"maxEmojis"`
	MaxStickers int `json:"maxStickers"`
	MaxSounds   int `json:"maxSounds"`
	// MaxSoundSeconds is how long one clip may run. The client trims to it
	// before uploading rather than being refused afterwards.
	MaxSoundSeconds int    `json:"maxSoundSeconds"`
	MaxEmojiBytes   string `json:"maxEmojiBytes"`
	MaxStickerBytes string `json:"maxStickerBytes"`
	MaxSoundBytes   string `json:"maxSoundBytes"`
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
	ID int64 `json:"id"`
	// Owner marks the identity that owns this server. It is not a role and no
	// role produces it: the owner holds every permission and outranks every
	// role for as long as they own the server, whatever roles they are given
	// or stripped of.
	Owner        bool    `json:"owner,omitempty"`
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
	// DMPrivacy is "everyone", "registered" or "none", and is only ever set on
	// your own entry: what somebody accepts privately is theirs to read and
	// nobody else's to see. Everybody else's copy of you carries an empty
	// string, and finding out that a message will not be delivered is what
	// sending one is for.
	DMPrivacy string `json:"dmPrivacy,omitempty"`
}

// Overwrite is a per-channel permission adjustment for one role. Allow and Deny
// are decimal strings so that a 64-bit mask survives a JavaScript client.
type Overwrite struct {
	RoleID int64  `json:"roleId"`
	Allow  string `json:"allow"`
	Deny   string `json:"deny"`
}

// Channel is a node of the channel tree. Categories hold other channels; every
// other type is always a leaf.
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
	// PostID is set on a message that belongs to a post: its body, or one of
	// the comments under it. It is absent on everything written straight into
	// a text channel, which is what tells a client whether a message.created
	// belongs in the channel timeline or inside a thread it may not have open.
	PostID    *int64 `json:"postId,omitempty"`
	Author    string `json:"author"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"createdAt"`
	EditedAt  *int64 `json:"editedAt"`
	// Attachments are the files posted with the message. They live and die
	// with it: deleting the message deletes the files.
	Attachments []Attachment `json:"attachments,omitempty"`
	// Webhook is set on, and only on, a message that arrived through one. It
	// is what tells a client to render an application rather than a member who
	// has since been deleted, which is the other reason UserID is nil.
	Webhook *MessageWebhook `json:"webhook,omitempty"`
	// Embeds are the rich cards the message carries. Only a webhook produces
	// them today; the field is on Message rather than on the webhook half
	// because an embed belongs to the message, not to its sender.
	Embeds []Embed `json:"embeds,omitempty"`
}

// MessageWebhook is the sender of a message that came in through a webhook.
//
// The name is already in Message.Author, because that is where every renderer
// reads a name from. What is here is the rest of what a webhook message needs:
// which webhook it was, so its own messages can be edited through its URL, and
// the picture this particular delivery chose.
type MessageWebhook struct {
	ID int64 `json:"id"`
	// Avatar is an absolute URL, or absent. A webhook is an outside service,
	// so nothing about its picture is hosted here.
	Avatar *string `json:"avatar,omitempty"`
}

// Post is one entry of a channel that holds entries: an announcement, a forum
// topic, a media item, a calendar event.
//
// Body is a Message, and the comments under it are Messages too, carrying the
// post's id. That is the whole of the design: a post is a title and some
// metadata in front of an ordinary thread, so everything a message already has
// — files, edits, embeds, deletion, moderation — reaches a post without a
// second implementation of any of it.
type Post struct {
	ID        int64  `json:"id"`
	ChannelID int64  `json:"channelId"`
	UserID    *int64 `json:"userId"` // nil once an author's account is gone
	Author    string `json:"author"`
	Title     string `json:"title"`
	// Locked closes the thread: no further comments, and the existing ones
	// stay readable. Only somebody with ManageMessages may set it.
	Locked bool `json:"locked"`
	// Pinned lifts the post to the top of its channel's listing.
	Pinned    bool   `json:"pinned"`
	CreatedAt int64  `json:"createdAt"`
	EditedAt  *int64 `json:"editedAt"`
	// Body is the first message of the thread: what the author wrote, and the
	// files they wrote it with. It is absent only for a post whose body was
	// deleted out from under it by a purge, which a client renders as a post
	// with a title and nothing else rather than as an error.
	Body *Message `json:"body,omitempty"`
	// Comments is how many messages hang off the post, not counting the body.
	Comments int `json:"comments"`
	// LastCommentAt is when the thread was last added to, or the creation time
	// of a post nobody has answered. It is what a forum listing sorts on.
	LastCommentAt int64 `json:"lastCommentAt"`
	// Event is set on, and only on, a post in a calendar channel.
	Event *PostEventDetails `json:"event,omitempty"`
	// RSVP travels with a calendar post: the tallies everybody sees, and the
	// answer of whoever is being sent the frame.
	RSVP *PostRSVPSummary `json:"rsvp,omitempty"`
}

// PostEventDetails is when and where a calendar post happens.
//
// The two timestamps are Unix seconds, as everywhere else in this protocol.
// An all-day event still carries a start: the day it falls on is read from it,
// in the reader's own zone, which is what makes one date land on the same day
// for everybody who is looking at their own calendar.
type PostEventDetails struct {
	StartsAt int64 `json:"startsAt"`
	// EndsAt is absent for an event with no stated finish.
	EndsAt   *int64 `json:"endsAt,omitempty"`
	AllDay   bool   `json:"allDay"`
	Location string `json:"location,omitempty"`
}

// PostRSVPSummary counts the answers to a calendar post.
type PostRSVPSummary struct {
	Going    int `json:"going"`
	Maybe    int `json:"maybe"`
	Declined int `json:"declined"`
	// Own is the answer of the identity this frame was sent to, or empty for
	// somebody who has not answered.
	Own string `json:"own"`
}

// Webhook is a URL that posts into one channel with no identity behind it.
//
// It is only ever sent to somebody who may manage webhooks in that channel,
// because Token is the whole of its authentication: anybody holding it can
// post, and there is nothing else to check.
type Webhook struct {
	ID        int64   `json:"id"`
	ChannelID int64   `json:"channelId"`
	Name      string  `json:"name"`
	Avatar    *string `json:"avatar,omitempty"`
	Token     string  `json:"token"`
	// URL is the path a delivery is posted to, relative to the server root, so
	// a client that reached the server by address, by hostname or through a
	// proxy all build the same working URL from the address they already hold.
	URL string `json:"url"`
	// CreatorID is nil once the account that made it is gone. The webhook
	// keeps working: an integration must not break because an administrator
	// left.
	CreatorID *int64 `json:"creatorId,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	// LastUsedAt is zero until the first delivery, which is what tells an
	// administrator whether the other end was ever wired up.
	LastUsedAt int64 `json:"lastUsedAt"`
}

// The embed objects below are Discord's, field for field and name for name,
// snake_case included.
//
// That is deliberate and it is the whole point of the feature: an application
// that already posts to a Discord webhook must work by changing nothing but
// the URL. Translating these into the camelCase the rest of this protocol uses
// would mean a second specification to keep in step with somebody else's, and
// every field that fell out of step would be one a service could send and a
// reader could not show. They travel through as they arrived.

// Embed is one rich card attached to a message.
type Embed struct {
	Title       string `json:"title,omitempty"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
	// Timestamp is an ISO 8601 instant, shown in the footer.
	Timestamp string `json:"timestamp,omitempty"`
	// Color is the stripe down the left edge, as a 24-bit RGB integer.
	Color     *int           `json:"color,omitempty"`
	Footer    *EmbedFooter   `json:"footer,omitempty"`
	Image     *EmbedMedia    `json:"image,omitempty"`
	Thumbnail *EmbedMedia    `json:"thumbnail,omitempty"`
	Video     *EmbedMedia    `json:"video,omitempty"`
	Provider  *EmbedProvider `json:"provider,omitempty"`
	Author    *EmbedAuthor   `json:"author,omitempty"`
	Fields    []EmbedField   `json:"fields,omitempty"`
}

type EmbedFooter struct {
	Text    string `json:"text"`
	IconURL string `json:"icon_url,omitempty"`
}

type EmbedMedia struct {
	URL    string `json:"url,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

type EmbedProvider struct {
	Name string `json:"name,omitempty"`
	URL  string `json:"url,omitempty"`
}

type EmbedAuthor struct {
	Name    string `json:"name"`
	URL     string `json:"url,omitempty"`
	IconURL string `json:"icon_url,omitempty"`
}

// EmbedField is one name/value pair. Inline fields share a row, up to three
// across.
type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// DirectMessage is one line of a private conversation.
//
// It is a Message in everything a reader can see, and differs only in what it
// hangs off: a conversation rather than a channel. The two are separate types
// for the same reason they are separate tables — a message belongs to one or
// the other, never to both, and a shared type would carry an empty half
// through every frame.
type DirectMessage struct {
	ID             int64  `json:"id"`
	ConversationID int64  `json:"conversationId"`
	UserID         *int64 `json:"userId"` // nil once an author's account is gone
	Author         string `json:"author"`
	Content        string `json:"content"`
	CreatedAt      int64  `json:"createdAt"`
	EditedAt       *int64 `json:"editedAt"`
}

// Conversation is one private thread as it looks to one of the two people in
// it. UserID is therefore the other one: the same row reaches the two sides
// under two different names, which is what lets a client key its conversations
// by person without first learning which id it was given.
type Conversation struct {
	ID     int64 `json:"id"`
	UserID int64 `json:"userId"`
	// LastMessageAt is when the thread was last written in, which is the order
	// a list of them is kept in. One nobody has written in yet carries the
	// moment it was opened.
	LastMessageAt int64 `json:"lastMessageAt"`
	// LastMessage is the line a list shows under the name. It is absent from a
	// conversation that has just been opened and not yet written in.
	LastMessage *DirectMessage `json:"lastMessage,omitempty"`
	// Unread is how much has arrived since this side last read it. It counts
	// from a marker the server keeps, so a badge survives a restart.
	Unread int `json:"unread"`
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
	// DeviceSalt is a random value this server minted once and keeps. A client
	// folds it into the stable machine attributes it can read and sends the
	// hash as Device on the auth op that follows.
	//
	// Salting is the whole design. The value identifies a machine on this
	// server, which is what makes a ban survive a new account and a cleared
	// browser profile; the same machine produces an unrelated value on every
	// other server, so nothing here can be used to follow somebody around.
	// Absent from a server that has no use for it.
	DeviceSalt string `json:"deviceSalt,omitempty"`
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
	// Conversations is every private thread this identity is in, newest first,
	// with what is waiting in each. It travels in the snapshot because a badge
	// is the whole reason to know a thread exists before opening it.
	Conversations []Conversation `json:"conversations,omitempty"`
	// Expressions is every custom emoji and sticker the server carries. It is
	// in the snapshot rather than fetched because a message cannot be rendered
	// without it: `:shrug:` in the very first line of history has to resolve.
	Expressions []Expression `json:"expressions,omitempty"`
	// Sounds is the soundboard, which the panel is drawn from.
	Sounds []Sound `json:"sounds,omitempty"`
}

// --- requests ---------------------------------------------------------------

type AuthGuestRequest struct {
	Nickname       string `json:"nickname"`
	ServerPassword string `json:"serverPassword,omitempty"`
	// Device is what a ban against a machine is matched on. See Hello.
	Device string `json:"device,omitempty"`
}

type AuthTokenRequest struct {
	Token          string `json:"token"`
	ServerPassword string `json:"serverPassword,omitempty"`
	Device         string `json:"device,omitempty"`
}

type AuthLoginRequest struct {
	Username       string `json:"username"`
	Password       string `json:"password"`
	ServerPassword string `json:"serverPassword,omitempty"`
	Device         string `json:"device,omitempty"`
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
	// Icon is only ever sent empty, to take the picture away. Setting one is
	// an upload, not a field: the bytes go to POST /upload/server-icon and the
	// path is the server's answer, never the client's claim.
	Icon        *string `json:"icon,omitempty"`
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
	// DMPrivacy sets who may write to you privately: "everyone", "registered"
	// or "none". It is your own setting and cannot be changed for anybody else,
	// whatever permissions the caller holds.
	DMPrivacy *string `json:"dmPrivacy,omitempty"`
}

type UserMoveRequest struct {
	// UserID defaults to the caller when omitted.
	UserID *int64 `json:"userId,omitempty"`
	// ChannelID is null to leave the current channel.
	ChannelID *int64 `json:"channelId"`
}

type UserKickRequest struct {
	UserID         int64  `json:"userId"`
	Reason         string `json:"reason,omitempty"`
	DeleteMessages string `json:"deleteMessages,omitempty"`
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

// PostCreateRequest starts an entry in a channel that holds them.
//
// Content and Attachments are the body, and go through exactly the checks
// message.send makes of them: a media post is the case where the files are the
// whole point, and an announcement is the case where the words are.
type PostCreateRequest struct {
	ChannelID   int64   `json:"channelId"`
	Title       string  `json:"title"`
	Content     string  `json:"content,omitempty"`
	Attachments []int64 `json:"attachments,omitempty"`
	// Event is required in a calendar channel and refused everywhere else.
	Event *PostEventDetails `json:"event,omitempty"`
}

// PostListRequest pages through one channel's entries.
//
// Before pages backwards by post id, newest first, exactly as message.history
// does. From and To instead ask for a window in time and are only meaningful
// in a calendar channel, where a client renders a month rather than a page.
type PostListRequest struct {
	ChannelID int64 `json:"channelId"`
	Before    int64 `json:"before,omitempty"`
	From      int64 `json:"from,omitempty"`
	To        int64 `json:"to,omitempty"`
	Limit     int   `json:"limit,omitempty"`
}

// PostListResult is ordered the way the channel is read: pinned posts first,
// then newest first — or, for a window of a calendar, earliest event first.
type PostListResult struct {
	ChannelID int64  `json:"channelId"`
	Posts     []Post `json:"posts"`
	// HasMore reports whether older entries remain past the last one here. It
	// is always false for a window, which is bounded by dates rather than by
	// how much fits in one page.
	HasMore bool `json:"hasMore"`
}

// PostUpdateRequest edits a post. An absent field is left alone.
//
// The body is not here: it is a message, so it is edited through message.edit
// like any other, by its author and nobody else. What this op carries is what
// belongs to the post rather than to the writing — its title, whether it is
// closed, whether it is pinned, and when the event happens.
type PostUpdateRequest struct {
	PostID int64   `json:"postId"`
	Title  *string `json:"title,omitempty"`
	Locked *bool   `json:"locked,omitempty"`
	Pinned *bool   `json:"pinned,omitempty"`
	// Event replaces the whole of an event, so a client sends back the fields
	// it did not change. Absent leaves the existing one untouched.
	Event *PostEventDetails `json:"event,omitempty"`
}

type PostDeleteRequest struct {
	PostID int64 `json:"postId"`
}

// PostRSVPRequest answers a calendar post. An empty response withdraws an
// answer already given.
type PostRSVPRequest struct {
	PostID   int64  `json:"postId"`
	Response string `json:"response"`
}

type MessageSendRequest struct {
	ChannelID int64  `json:"channelId"`
	Content   string `json:"content"`
	// PostID comments on a post instead of writing into the channel. The
	// channel is still named, and still has to be the one the post is in: a
	// comment is a message in that channel, visible to exactly the people the
	// channel is.
	PostID int64 `json:"postId,omitempty"`
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
	// PostID reads the comments under one post rather than the channel
	// timeline. A post's own body is not a page of its comments: it arrives
	// with the post, so paging back through a long thread never re-sends it.
	PostID int64 `json:"postId,omitempty"`
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
	ChannelID int64 `json:"channelId"`
	// PostID echoes the thread the page came from, so a client holding
	// several open threads can tell which one answered.
	PostID   int64     `json:"postId,omitempty"`
	Messages []Message `json:"messages"`
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

// --- private conversations ---------------------------------------------------

// DMListRequest reads every conversation the caller is in. It takes nothing: a
// person has as many private threads as they have, and the server bounds the
// list rather than the caller paging it.
type DMListRequest struct{}

type DMListResult struct {
	Conversations []Conversation `json:"conversations"`
}

// DMHistoryRequest pages through the conversation with one person.
//
// It names the person rather than the conversation, because a name in the
// member list is all a client has to start from: the thread may not exist yet,
// and asking for its history is a perfectly good way to find that out.
//
// The three cursors work exactly as they do in MessageHistoryRequest.
type DMHistoryRequest struct {
	UserID int64 `json:"userId"`
	Before int64 `json:"before,omitempty"`
	After  int64 `json:"after,omitempty"`
	Around int64 `json:"around,omitempty"`
	Limit  int   `json:"limit,omitempty"`
}

// DMHistoryResult is ordered oldest first, the order it is rendered in. A
// conversation that has never been opened comes back with a zero id and no
// messages rather than as an error.
type DMHistoryResult struct {
	UserID         int64           `json:"userId"`
	ConversationID int64           `json:"conversationId"`
	Messages       []DirectMessage `json:"messages"`
	HasMore        bool            `json:"hasMore"`
	HasMoreAfter   bool            `json:"hasMoreAfter"`
}

// DMSendRequest writes to one person, opening the conversation if this is the
// first thing either of them has said to the other.
type DMSendRequest struct {
	UserID  int64  `json:"userId"`
	Content string `json:"content"`
}

type DMEditRequest struct {
	MessageID int64  `json:"messageId"`
	Content   string `json:"content"`
}

type DMDeleteRequest struct {
	MessageID int64 `json:"messageId"`
}

// DMReadRequest moves the caller's own read marker in one conversation up to a
// message they have seen. The marker never moves backwards.
type DMReadRequest struct {
	UserID    int64 `json:"userId"`
	MessageID int64 `json:"messageId"`
}

// WebhookListRequest reads the webhooks the caller may manage. A zero
// ChannelID lists every such channel's, which is what the settings screen asks
// for; naming one narrows it to that channel.
type WebhookListRequest struct {
	ChannelID int64 `json:"channelId,omitempty"`
}

type WebhookListResult struct {
	Webhooks []Webhook `json:"webhooks"`
}

type WebhookCreateRequest struct {
	ChannelID int64 `json:"channelId"`
	// Name is what messages posted through the webhook are attributed to,
	// unless a delivery overrides it.
	Name string `json:"name"`
	// Avatar is an absolute http(s) URL, or empty for none.
	Avatar string `json:"avatar,omitempty"`
}

// WebhookUpdateRequest is a patch: an absent field is left alone. Moving a
// webhook to another channel needs the permission in both, since it is the
// same thing as minting one there.
type WebhookUpdateRequest struct {
	WebhookID int64   `json:"webhookId"`
	Name      *string `json:"name,omitempty"`
	Avatar    *string `json:"avatar,omitempty"`
	ChannelID *int64  `json:"channelId,omitempty"`
}

type WebhookDeleteRequest struct {
	WebhookID int64 `json:"webhookId"`
}

type WebhookEvent struct {
	Webhook Webhook `json:"webhook"`
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

// RoleReorderRequest restacks the hierarchy in one move.
//
// RoleIDs is the whole stack from the bottom up, minus the everyone role,
// which is always beneath everything and is not addressable here. Sending the
// complete order rather than one role and a number is what makes this atomic:
// positions are only meaningful relative to each other, so a reorder is one
// decision about the whole stack and is accepted or refused as one.
type RoleReorderRequest struct {
	RoleIDs []int64 `json:"roleIds"`
}

// RoleReorderResult is the stack as it stands afterwards, bottom-up, so a
// client that guessed wrong about the everyone role or a managed one is
// corrected by the reply rather than by the next reload.
type RoleReorderResult struct {
	Roles []Role `json:"roles"`
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

type UserRemovedEvent struct {
	UserID int64  `json:"userId"`
	Reason string `json:"reason,omitempty"`
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

type PostEvent struct {
	Post Post `json:"post"`
}

// PostDeletedEvent carries the channel as well, for the same reason
// MessageDeletedEvent does.
type PostDeletedEvent struct {
	PostID    int64 `json:"postId"`
	ChannelID int64 `json:"channelId"`
}

// PostRSVPEvent reports one answer to a calendar post.
//
// It is its own event rather than a post.updated because the tallies are the
// same for everybody while the answer is one person's: sending a whole Post
// would either leak whose answer it was as everybody's, or force every
// recipient to forget their own. So the counts travel once, and UserID says
// whose answer changed — the one client it belongs to updates Own from it.
type PostRSVPEvent struct {
	PostID    int64           `json:"postId"`
	ChannelID int64           `json:"channelId"`
	UserID    int64           `json:"userId"`
	Response  string          `json:"response"`
	RSVP      PostRSVPSummary `json:"rsvp"`
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

// DMCreatedEvent delivers one private line to the two people in it. The
// conversation travels with it because the receiving side may never have heard
// of it: the first thing somebody says to you is also how you learn the thread
// exists.
type DMCreatedEvent struct {
	Conversation Conversation  `json:"conversation"`
	Message      DirectMessage `json:"message"`
}

// DMUpdatedEvent is an edit. UserID is the other participant, as everywhere.
type DMUpdatedEvent struct {
	UserID  int64         `json:"userId"`
	Message DirectMessage `json:"message"`
}

type DMDeletedEvent struct {
	UserID         int64 `json:"userId"`
	ConversationID int64 `json:"conversationId"`
	MessageID      int64 `json:"messageId"`
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
