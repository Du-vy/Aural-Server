// Package discord speaks the small part of Discord's API a relay needs.
//
// It is deliberately not a Discord library. A relay reads messages out of a
// set of channels and posts messages into them through webhooks, and that is
// two things: a WebSocket that carries events, and a handful of HTTP calls
// against a webhook URL. Everything else Discord's API covers — slash
// commands, voice, guild administration, sharding, the interaction gateway —
// is surface this server has no use for and would have to keep working anyway.
//
// The alternative was DiscordGo, which is a fine library and roughly thirty
// thousand lines of one. Pulling it in would add a second WebSocket
// implementation alongside the one the gateway already uses, for a feature
// that needs about six opcodes. What is here reuses coder/websocket, adds no
// dependency at all, and is small enough to read in one sitting.
//
// Nothing in this package knows what Aural is. It hands whole Discord messages
// to a callback and accepts whole messages to send; the mapping between the
// two worlds lives in the gateway, where the channels and the permissions are.
package discord

import (
	"errors"
	"time"
)

// The API this package is written against. Discord keeps old versions working
// for a long time and then stops; pinning the number here is what makes a
// forced upgrade one edit rather than a search.
const (
	apiBase    = "https://discord.com/api/v10"
	cdnBase    = "https://cdn.discordapp.com"
	gatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"
)

// userAgent identifies this client. Discord requires one on REST calls and
// rejects a request without it, which is a failure mode worth naming rather
// than discovering.
const userAgent = "DiscordBot (https://github.com/aural-chat/aural-server, 1.0)"

// requestTimeout bounds one REST call.
const requestTimeout = 30 * time.Second

// Intents are the event families a connection asks for.
//
// Only three matter here. Guilds is what makes Discord tell us which channels
// exist at all; GuildMessages carries the events; MessageContent is the
// privileged one that decides whether those events arrive with their text or
// with an empty content field. A bot without it relays blank messages, which
// is the single most common way this feature is misconfigured, so the failure
// is detected and reported rather than left to look like a bug here.
const (
	IntentGuilds         = 1 << 0
	IntentGuildMessages  = 1 << 9
	IntentMessageContent = 1 << 15

	// RelayIntents is what this client identifies with.
	RelayIntents = IntentGuilds | IntentGuildMessages | IntentMessageContent
)

// Gateway opcodes. These are the whole protocol as far as a relay is
// concerned: the rest of the numbers Discord defines are for features this
// client does not use.
const (
	opDispatch            = 0  // an event, named by the frame's t field
	opHeartbeat           = 1  // sent by us on a timer, and by Discord on demand
	opIdentify            = 2  // the first frame of a new session
	opPresenceUpdate      = 3  // what the bot's own status looks like
	opResume              = 6  // pick a dropped session back up
	opReconnect           = 7  // Discord asking us to reconnect and resume
	opRequestGuildMembers = 8  // unused; listed so the numbering reads whole
	opInvalidSession      = 9  // the session is gone; identify again
	opHello               = 10 // first frame in, carries the heartbeat interval
	opHeartbeatACK        = 11 // the answer to opHeartbeat
)

// Close codes Discord sends that no amount of reconnecting will fix. Every
// one of them is a configuration mistake — a bad token, an intent that was
// never switched on in the developer portal — so the connection stops and
// says so instead of retrying a wrong answer forever.
var fatalCloseCodes = map[int]string{
	4004: "the bot token was rejected",
	4010: "invalid shard",
	4011: "this bot is in too many guilds to connect without sharding",
	4012: "the gateway API version this build uses is no longer accepted",
	4013: "the intents this client asks for are not valid",
	4014: "the message content intent is not enabled for this bot — switch it on in the Discord developer portal, under Bot > Privileged Gateway Intents",
}

// ErrFatal wraps a failure the caller must not retry. The message says which.
var ErrFatal = errors.New("discord: unrecoverable")

// fatalf builds an ErrFatal carrying its own explanation.
type fatalError struct{ reason string }

func (e *fatalError) Error() string { return "discord: " + e.reason }
func (e *fatalError) Is(target error) bool {
	return target == ErrFatal
}

// Fatal reports whether an error is one that reconnecting cannot fix.
func Fatal(err error) bool { return errors.Is(err, ErrFatal) }
