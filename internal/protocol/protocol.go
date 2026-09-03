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

// Version is the protocol revision. A client refuses to talk to a server that
// reports a different major revision. It is bumped on any breaking change.
const Version = 1

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

	OpUserUpdate = "user.update" // change own nickname
	OpUserMove   = "user.move"   // move self (or another user) between channels
	OpUserKick   = "user.kick"

	OpChannelCreate = "channel.create"
	OpChannelUpdate = "channel.update"
	OpChannelDelete = "channel.delete"

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

	OpRoleCreate   = "role.create"
	OpRoleUpdate   = "role.update"
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

	EvChannelCreated = "channel.created"
	EvChannelUpdated = "channel.updated"
	EvChannelDeleted = "channel.deleted"

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
	ErrVoiceDisabled      = "voice_disabled" // this server runs no audio plane
	ErrVoiceFailed        = "voice_failed"   // the media session could not be set up
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
