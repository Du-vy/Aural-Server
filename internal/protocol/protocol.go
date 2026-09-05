// Package protocol defines the wire format spoken between an Aural client and
// an Aural server. Frames are JSON objects sent over a single WebSocket.
//
// There are three kinds of frame:
//
//	request   client -> server, carries an "id" chosen by the client
//	reply     server -> client, echoes the request "id" (op "result" or "error")
//	event     server -> client, has no "id" and may arrive at any time
//
// The canonical human-readable specification lives in docs/PROTOCOL.md.
package protocol

import "encoding/json"

// Version is the newest protocol revision this build speaks, and MinVersion the
// oldest it still accepts from a client. Version is bumped on any breaking
// change; MinVersion is raised only when carrying the older revision genuinely
// stops being possible.
//
// They are a range rather than a single number because the two sides of an
// Aural conversation are updated by different people. Servers are self-hosted,
// so an operator pulls a new image when they get round to it, while a client
// updates itself. Under a rule of strict equality the first breaking change
// would cut every client off from every server that had not been pulled yet.
// A range is what lets the two move independently: both ends advertise theirs,
// and they talk as long as the ranges overlap.
const (
	Version    = 1
	MinVersion = 1
)

// Envelope is the single JSON frame exchanged over the WebSocket.
type Envelope struct {
	// ID correlates a reply with the request that caused it. Events omit it.
	ID string `json:"id,omitempty"`
	// Op names the operation (requests) or the event (server pushes).
	Op string `json:"op"`
	// Data holds the operation payload, kept raw so handlers can decode it
	// into the concrete type the op expects.
	Data json.RawMessage `json:"d,omitempty"`
	// Error is set on, and only on, OpError replies.
	Error *Error `json:"error,omitempty"`
}

// Reply ops. Every request receives exactly one of these.
const (
	OpResult = "result"
	OpError  = "error"
)

// Request ops, sent by the client.
const (
	OpAuthGuest    = "auth.guest"    // take a fresh guest identity
	OpAuthToken    = "auth.token"    // resume a stored session token
	OpAuthLogin    = "auth.login"    // sign in with username + password
	OpAuthRegister = "auth.register" // claim the current identity
	OpAuthLogout   = "auth.logout"   // revoke the session token in use

	OpServerClaimAdmin = "server.claimAdmin" // redeem the one-time owner token
	OpServerUpdate     = "server.update"     // rename / re-describe the server
	OpServerMetrics    = "server.metrics"    // query live server metrics & storage breakdown

	OpUserUpdate = "user.update" // change own nickname
	OpUserMove   = "user.move"   // move self (or another user) between channels
	OpUserKick   = "user.kick"

	// Bans. A kick ends a connection; a ban is a standing refusal, so it has
	// its own small set of ops and a list that outlives everybody named in it.
	OpBanList   = "ban.list"
	OpBanCreate = "ban.create"
	OpBanDelete = "ban.delete" // lift one

	// The record of what moderators did. There is no op to write one: entries
	// are produced by the actions themselves, never by a client.
	OpAuditList = "audit.list"

	// Automatic moderation. The whole rule set is read and written at once,
	// because the rules constrain one another and a half-applied edit is not a
	// state worth being able to reach.
	OpAutoModGet    = "automod.get"
	OpAutoModUpdate = "automod.update"

	// Custom emoji and stickers. Creating one is an upload over HTTP, like
	// every other file; these are what is left — renaming and removing.
	OpExpressionUpdate = "expression.update"
	OpExpressionDelete = "expression.delete"

	// The soundboard. Uploading a clip is an HTTP upload; playing one is a
	// frame, because everybody in the channel has to hear it at once.
	OpSoundUpdate = "sound.update"
	OpSoundDelete = "sound.delete"
	OpSoundPlay   = "sound.play"

	OpChannelCreate = "channel.create"
	OpChannelUpdate = "channel.update"
	OpChannelDelete = "channel.delete"

	// Posts: the entries of a channel that holds entries rather than a
	// stream. One set of ops serves announcements, forum topics, media items
	// and calendar events, because they differ in what they carry and in who
	// may write one, not in what may be done to them.
	OpPostCreate = "post.create"
	OpPostList   = "post.list" // page through one channel's entries
	OpPostUpdate = "post.update"
	OpPostDelete = "post.delete"
	OpPostRSVP   = "post.rsvp" // answer a calendar event

	OpMessageSend    = "message.send"
	OpMessageHistory = "message.history" // page through a channel
	OpMessageSearch  = "message.search"  // look through every readable channel
	OpMessageEdit    = "message.edit"
	OpMessageDelete  = "message.delete"

	// Private conversations. They belong to a pair of identities rather than
	// to any channel, so none of the channel ops reaches them and they have
	// their own small set instead.
	OpDMList    = "dm.list"    // every conversation this identity is in
	OpDMHistory = "dm.history" // page through one of them
	OpDMSend    = "dm.send"
	OpDMEdit    = "dm.edit"
	OpDMDelete  = "dm.delete"
	OpDMRead    = "dm.read" // move your own read marker up

	// Webhooks: URLs that post into one channel without an identity behind
	// them. There are no webhook events, deliberately — a webhook object
	// carries the token that is the whole of its authentication, so it is only
	// ever handed to somebody who asked for it and may manage it.
	OpWebhookList   = "webhook.list"
	OpWebhookCreate = "webhook.create"
	OpWebhookUpdate = "webhook.update"
	OpWebhookDelete = "webhook.delete"

	// The Discord relay: channel pairs that carry messages between this server
	// and a Discord one. Every op needs ManageServer — a link carries a
	// webhook URL, which is a standing permission to post, and points the
	// server at an outside service on the operator's behalf.
	OpRelayGet       = "relay.get"       // the whole state: bot, guilds, links
	OpRelayConfigure = "relay.configure" // switch it on, set the bot token
	OpRelayCreate    = "relay.create"
	OpRelayUpdate    = "relay.update"
	OpRelayDelete    = "relay.delete"

	OpRoleCreate   = "role.create"
	OpRoleUpdate   = "role.update"
	OpRoleReorder  = "role.reorder"
	OpRoleDelete   = "role.delete"
	OpRoleAssign   = "role.assign"
	OpRoleUnassign = "role.unassign"

	// The audio plane. Sitting in a voice channel is user.move, exactly as it
	// has always been; these carry the media session on top of it, so a client
	// that cannot do audio is still a first-class member of the channel.
	OpVoiceConnect  = "voice.connect"  // open a media session in the channel
	OpVoiceLeave    = "voice.leave"    // close it without leaving the channel
	OpVoiceSignal   = "voice.signal"   // one SDP or ICE frame
	OpVoiceState    = "voice.state"    // set your own mute and deafen
	OpVoiceModerate = "voice.moderate" // mute or deafen somebody else
	OpVoiceSpeaking = "voice.speaking" // announce a speaking transition
)

// Event ops, pushed by the server.
const (
	EvHello = "hello" // first frame on every connection, before authentication
	EvReady = "ready" // full state snapshot, sent once authentication succeeds

	EvUserConnected    = "user.connected"
	EvUserDisconnected = "user.disconnected"
	EvUserUpdated      = "user.updated"
	EvUserMoved        = "user.moved"
	EvUserRemoved      = "user.removed"

	EvBanCreated = "ban.created"
	EvBanDeleted = "ban.deleted"

	// EvAuditEntry carries one new line of the log, so a settings screen that
	// is open updates rather than going stale. It only reaches the sessions
	// allowed to read the log at all.
	EvAuditEntry = "audit.entry"

	EvAutoModUpdated = "automod.updated"

	EvExpressionCreated = "expression.created"
	EvExpressionUpdated = "expression.updated"
	EvExpressionDeleted = "expression.deleted"

	EvSoundCreated = "sound.created"
	EvSoundUpdated = "sound.updated"
	EvSoundDeleted = "sound.deleted"
	// EvSoundPlayed reaches everybody sitting in the voice channel it was
	// played in. Each client fetches the clip and mixes it into its own
	// output, which is what makes the soundboard work identically whoever is
	// relaying the call.
	EvSoundPlayed = "sound.played"

	EvChannelCreated = "channel.created"
	EvChannelUpdated = "channel.updated"
	EvChannelDeleted = "channel.deleted"

	EvPostCreated = "post.created"
	EvPostUpdated = "post.updated"
	EvPostDeleted = "post.deleted"
	EvPostRSVP    = "post.rsvp" // one answer to a calendar post

	EvMessageCreated = "message.created"
	EvMessageUpdated = "message.updated"
	EvMessageDeleted = "message.deleted"

	// Every dm event carries userId: the other participant, from the point of
	// view of whoever it was sent to. The two sides of one conversation
	// therefore receive the same message under two different names, which is
	// what saves a client from having to hold a map of conversation ids to
	// people before it can render the first frame that arrives.
	EvDMCreated = "dm.created"
	EvDMUpdated = "dm.updated"
	EvDMDeleted = "dm.deleted"

	EvRoleCreated = "role.created"
	EvRoleUpdated = "role.updated"
	EvRoleDeleted = "role.deleted"

	EvServerUpdated = "server.updated"

	// EvRelayUpdated carries the whole relay state after any change to it,
	// and reaches only the sessions that may manage the server: it names
	// webhook URLs, which are credentials.
	EvRelayUpdated = "relay.updated"

	EvVoiceState    = "voice.state"    // somebody's voice state changed
	EvVoiceSpeaking = "voice.speaking" // somebody started or stopped speaking
	EvVoiceSignal   = "voice.signal"   // an SDP or ICE frame addressed to you
	EvVoicePeer     = "voice.peer"     // client_host: dial this peer, or drop it
	EvVoiceHost     = "voice.host"     // client_host: who relays a channel now
	EvVoiceReset    = "voice.reset"    // your media session is gone; start over
)

// Error codes. Clients switch on Code, never on Message.
const (
	ErrBadRequest         = "bad_request"
	ErrUnauthorized       = "unauthorized"
	ErrForbidden          = "forbidden"
	ErrNotFound           = "not_found"
	ErrConflict           = "conflict"
	ErrInternal           = "internal"
	ErrUnsupportedVersion = "unsupported_version"
	ErrServerFull         = "server_full"
	ErrServerPassword     = "server_password"
	ErrGuestsDisabled     = "guests_disabled"
	ErrRegistrationClosed = "registration_closed"
	ErrInvalidCredentials = "invalid_credentials"
	ErrUsernameTaken      = "username_taken"
	ErrAlreadyRegistered  = "already_registered"
	ErrRateLimited        = "rate_limited"
	ErrTooLarge           = "too_large"    // one upload exceeded the file ceiling
	ErrStorageFull        = "storage_full" // the server-wide upload ceiling is reached
	ErrUploadsDisabled    = "uploads_disabled"
	ErrDMDisabled         = "dm_disabled"    // this server carries no private messages
	ErrDMBlocked          = "dm_blocked"     // the other person does not accept them
	ErrPostLocked         = "post_locked"    // no more comments are accepted on it
	ErrVoiceDisabled      = "voice_disabled" // this server runs no audio plane
	ErrVoiceFailed        = "voice_failed"   // the media session could not be set up
	// ErrBanned is the refusal a banned connection is given. Its message
	// carries the reason and, when the ban ends, when.
	ErrBanned = "banned"
	// ErrAutoModBlocked is a message a rule refused to accept. It is separate
	// from forbidden because nothing about the writer is wrong: the same
	// person may send the same message with one word changed.
	ErrAutoModBlocked = "automod_blocked"
	// ErrExpressionLimit is a server that already holds as many emoji,
	// stickers or sounds as it is configured to.
	ErrExpressionLimit = "expression_limit"
)

// Error is the payload of an OpError reply.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Errorf builds a protocol error with a fixed code and a human message.
func Errorf(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Event builds a server-pushed frame.
func Event(op string, payload any) Envelope {
	return Envelope{Op: op, Data: marshal(payload)}
}

// Result builds a successful reply to the request identified by id.
func Result(id string, payload any) Envelope {
	return Envelope{ID: id, Op: OpResult, Data: marshal(payload)}
}

// Failure builds an error reply to the request identified by id.
func Failure(id string, err *Error) Envelope {
	return Envelope{ID: id, Op: OpError, Error: err}
}

// marshal encodes a payload that is statically known to be JSON-safe. Every
// payload in this package is a plain struct of primitives, slices and maps, so
// a failure here can only mean a new payload type was declared with a field
// encoding/json cannot handle.
func marshal(payload any) json.RawMessage {
	if payload == nil {
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return json.RawMessage(`{"_marshalError":true}`)
	}
	return raw
}
