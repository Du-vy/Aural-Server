# Aural protocol, version 1

This is the canonical specification of the wire format between an Aural client
and an Aural server. The Go definitions in `internal/protocol` are the reference
implementation of everything described here.

## Transport

A single WebSocket carries the whole session.

```
ws://HOST:PORT/ws      plaintext
wss://HOST:PORT/ws     when tls.enabled is set in the server configuration
```

The default port is **9871**. A handful of plain HTTP endpoints sit alongside
the socket. Everything but the first two takes the same bearer session token the
socket resumes with, in an `Authorization: Bearer <token>` header:

| Endpoint  | Purpose                                                              |
| --------- | -------------------------------------------------------------------- |
| `GET /info`   | Unauthenticated server preview. A client shows it before connecting, and the future Aural Hub reads it to list servers. Sends `Access-Control-Allow-Origin: *`. |
| `GET /health` | Liveness probe for process supervisors. Returns `ok`.            |
| `POST /upload` | Uploads one file. See [Attachments](#attachments).               |
| `POST /upload/avatar`, `POST /upload/banner` | Replaces the caller's own avatar or profile banner. |
| `GET /attachments/{key}/{filename}` | Serves an uploaded file.                |
| `GET /unfurl?url=` | Fetches a page and returns its OpenGraph metadata. See [Link previews](#link-previews). |
| `GET /klipy/{kind}/{action}` | Proxies a GIF or sticker lookup. See [GIFs and stickers](#gifs-and-stickers). |

Because `GET /info` is unauthenticated, everything it carries is public. No
credential the operator configures appears in it: the Klipy integration is
reported as the boolean `klipyEnabled` and never as the key behind it.

### Link previews

`GET /unfurl?url=<target>` fetches the target and returns the OpenGraph and
`<meta>` data found in it, which is what lets a client show a rich preview for a
link that would otherwise be blocked by CORS.

It requires a session, for the same reason the upload endpoint does: the fetch
goes out from the server's address rather than the caller's, so an open one is
an anonymous fetcher pointed at the public internet. Results are cached in
SQLite for `unfurl.cache_ttl_days`, and a cache hit costs the caller nothing
against the rate limit. Targets that resolve to a loopback, private, link-local,
carrier-grade NAT or NAT64-mapped address are refused, and only `http` and
`https` are followed.

### GIFs and stickers

`GET /klipy/{kind}/{action}` proxies one lookup to Klipy, where `kind` is `gifs`
or `stickers` and `action` is `categories` (gifs only), `trending` or `search`.
`q` carries a search term and `limit` bounds the result count. The answer is
Klipy's own JSON, handed back untouched.

The credential belongs to the operator and never reaches a client. Klipy carries
it in the request path, so a client holding it would leak it into every proxy
log between the two; `ServerInfo.klipyEnabled` reports only whether the server
can answer at all. A Klipy key is also rated by the hour rather than by the
member, so one cache in front of one key is what lets a room full of people open
the same trending list for a single upstream call.

Files travel over HTTP rather than over the socket: a file does not fit the
64 KiB frame budget, and HTTP is what gives an upload a progress bar and a
download range requests, seeking and ordinary caching.

The server pings every 30 seconds and drops a peer that does not pong within 10.
`hello.heartbeatMs` reports the interval so a client can size its own timeout.
One inbound frame may not exceed 64 KiB.

## Frames

Every frame is a JSON object with the same envelope.

```jsonc
{
  "id": "c17",        // request correlation id, chosen by the client; absent on events
  "op": "channel.create",
  "d":  { },          // payload, shape depends on op
  "error": { }        // present only when op is "error"
}
```

There are three kinds of frame:

- **request** — client to server, carries an `id`.
- **reply** — server to client, echoes the `id`, with `op` of `result` or `error`.
  Exactly one reply is sent per request.
- **event** — server to client, no `id`, may arrive at any time.

An error reply carries a machine-readable code. Clients switch on `code` and
never on `message`.

```jsonc
{ "id": "c17", "op": "error", "error": { "code": "forbidden", "message": "..." } }
```

| Code | Meaning |
| --- | --- |
| `bad_request` | Malformed payload or a value that fails validation. |
| `unauthorized` | The op needs an authenticated session. |
| `forbidden` | Authenticated, but not permitted. |
| `not_found` | No such channel, role or user. Also returned instead of `forbidden` when admitting the target exists would leak it. |
| `conflict` | The request collides with current state, such as a full channel. |
| `internal` | The server failed. The cause is logged, not sent. |
| `unsupported_version` | Protocol revision mismatch. |
| `server_full` | Capacity reached. |
| `server_password` | The server password is missing or wrong. |
| `guests_disabled` | This server accepts only registered accounts. |
| `registration_closed` | Registration is switched off. |
| `invalid_credentials` | Wrong username, password or token. |
| `username_taken` | That username already exists. |
| `already_registered` | This identity already has an account. |
| `rate_limited` | Too many attempts. |
| `too_large` | One upload exceeded the per-file ceiling. |
| `storage_full` | The server-wide upload ceiling is reached. |
| `uploads_disabled` | This server does not accept file uploads. |

## Numbers

Permission masks are 64-bit and travel as **decimal strings** (`"1234"`), because
a JavaScript client loses precision above 2^53. Ids are sequential integers and
travel as plain JSON numbers.

## Connecting

1. The client opens the socket.
2. The server immediately sends a `hello` event. No request is answered before
   authentication apart from the `auth.*` ops.
3. The client picks one authentication op. Its **reply payload is the full state
   snapshot** described under `ready` below.

```jsonc
// event: hello
{
  "op": "hello",
  "d": {
    "server": { /* ServerInfo, same shape as GET /info */ },
    "heartbeatMs": 30000
  }
}
```

A client should compare `server.protocolVersion` against its own and refuse to
continue on a mismatch.

At most **six** authentication attempts are allowed on one connection; the
seventh closes the socket.

## Identity and login

An identity is a row in the `users` table. A guest is simply an identity with no
username yet. Claiming an account **updates that row in place**: the id, the
nickname and everything attached to them survive. A guest becomes an account, it
is never replaced by one.

```
auth.guest  ->  new identity + session token
                     |
                     |  auth.register (username + password)
                     v
                same identity, now an account
                     |
                     |  auth.login from any device
                     v
                same identity + a new session token for that device
```

The session token is what makes a returning guest the same person. It is minted
once, handed to the client, and stored only as a SHA-256 hash. Losing it means
losing a guest identity permanently — which is exactly why claiming an account
matters: a username and password can rebuild the session from any device.

### `auth.guest`

```jsonc
{ "op": "auth.guest", "d": { "nickname": "Pablo", "serverPassword": "optional" } }
```

Fails with `guests_disabled` when the server accepts only accounts. Replies with
`ready`, including a `sessionToken`.

### `auth.token`

```jsonc
{ "op": "auth.token", "d": { "token": "stored-session-token", "serverPassword": "optional" } }
```

Resumes an identity. Replies with `ready` and **no** `sessionToken`, since the
client already holds it.

### `auth.login`

```jsonc
{ "op": "auth.login", "d": { "username": "pablo", "password": "...", "serverPassword": "optional" } }
```

Replies with `ready` including a fresh `sessionToken` for the device signing in.
A username that does not exist costs the same time as a wrong password, so the
two cannot be told apart.

### `auth.register`

Requires an authenticated session and the `Register` permission, and fails with
`registration_closed` when the server has registration switched off.

```jsonc
{ "op": "auth.register", "d": { "username": "pablo", "password": "..." } }
// result: { "user": User }
```

Passwords are hashed with Argon2id and stored in PHC format.

### `auth.logout`

Revokes the session token in use and closes the connection.

## `ready`

The state snapshot. It is the reply payload of every successful `auth.*` op, and
it is also pushed as a `ready` **event** whenever a permission change makes the
client's view of the tree stale.

```jsonc
{
  "sessionToken": "...",   // only on auth.guest and auth.login
  "user":        User,     // the caller
  "users":      [User],    // every member, plus the guests connected now
  "channels":   [Channel], // only the channels the caller may see
  "roles":      [Role],    // the whole role table
  "permissions": "1234",   // the caller's resolved server-wide mask
  "server":      ServerInfo
}
```

`channels` is filtered to what the caller may see, and the `channelId` of a user
sitting somewhere the caller cannot see is reported as `null`. A restricted
channel never leaks through the presence list.

`users` is the roster rather than a list of who is here: every registered
identity is in it, `online: false` and `status: "offline"` when they are not
connected, plus the guests connected right now. A guest is never listed while
away — the identity lasts no longer than the connection that made it — so a
client should drop a guest from its list when one goes offline, and keep a
member.

## Objects

```jsonc
// User
{
  "id": 4,
  "nickname": "Pablo",
  "username": "pablo",     // null while still a guest
  "registered": true,
  "roles": [1, 2],
  "channelId": 3,          // null when in no voice channel
  "online": true
}

// Channel — "category" holds others; "text" and "voice" are always leaves
{
  "id": 3,
  "parentId": 1,           // null at the tree root
  "name": "Lobby",
  "type": "voice",         // category | text | voice
  "topic": "",
  "position": 1,
  "userLimit": 0,          // voice only, 0 means unlimited
  "overwrites": [{ "roleId": 1, "allow": "0", "deny": "4" }]
}

// Role
{
  "id": 2,
  "name": "Member",
  "color": "#3ba55d",      // empty or #rrggbb
  "permissions": "63",
  "position": 1,
  "hoist": false,
  "managed": "registered"  // "" | everyone | registered | admin
}

// Message
{
  "id": 12,
  "channelId": 2,
  "userId": 4,             // null once the author's account is gone
  "author": "Pablo",       // resolved live from the users table
  "content": "Hello",
  "createdAt": 1756600000, // Unix seconds
  "editedAt": null,
  "attachments": []        // absent when the message carries no files
}

// Attachment
{
  "id": 7,
  "filename": "screenshot.png",
  "contentType": "image/png",   // decided by the server, never by the uploader
  "size": "184320",             // decimal string, as byte counts can exceed 2^53
  "url": "/attachments/Yk3.../screenshot.png",  // relative to the server root
  "width": 1280,                // images only, and only when readable
  "height": 720
}

// ServerInfo
{
  "name": "Aural Server",
  "description": "",
  "protocolVersion": 1,
  "softwareVersion": "0.5.0",
  "maxUsers": 64,
  "onlineUsers": 3,
  "passwordProtected": false,
  "registrationEnabled": true,
  "guestsAllowed": true,
  "voiceMode": "client_host",  // client_host | server_host
  "uploads": {
    "enabled": true,
    "maxFileBytes": "52428800",     // decimal strings, for the same reason
    "maxTotalBytes": "5368709120",  // "0" means no ceiling but the disk
    "usedBytes": "1048576",
    "maxPerMessage": 10
  }
}
```

## Permissions

A 64-bit mask. `Administrator` bypasses every other check.

| Bit | Name | Grants |
| --- | --- | --- |
| 0 | `ViewChannel` | See a channel and who is in it |
| 1 | `Connect` | Join a voice channel |
| 2 | `Speak` | Transmit in a voice channel |
| 3 | `SendMessages` | Post in a text channel |
| 4 | `ChangeNickname` | Change your own nickname |
| 5 | `Register` | Claim your identity as an account |
| 6 | `AttachFiles` | Post files alongside a message |
| 8 | `ManageChannels` | Create, edit and delete channels |
| 9 | `ManageRoles` | Manage roles and channel overwrites |
| 10 | `ManageServer` | Rename the server |
| 11 | `ManageNicknames` | Change other nicknames |
| 12 | `ManageMessages` | Delete other people's messages |
| 16 | `KickUsers` | Disconnect a user |
| 17 | `MoveUsers` | Move a user between voice channels |
| 18 | `MuteUsers` | Reserved for voice moderation |
| 19 | `DeafenUsers` | Reserved for voice moderation |
| 31 | `Administrator` | Everything, unconditionally |

### Resolution

1. If the user holds `Administrator`, they hold everything. Stop.
2. Union the masks of every role the user holds, including the implicit
   `everyone` role and, for a registered user, the implicit `registered` role.
3. Walk the channel tree from the outermost category down to the channel. At
   each level: apply the `everyone` overwrite first (`deny` then `allow`), then
   the union of the overwrites of every other role the user holds (again `deny`
   then `allow`). An explicit allow on any role beats a deny on another.
4. If `ViewChannel` was lost at any level, the result is the empty mask.
   A channel you cannot see grants nothing inside it.

Overwrites therefore **inherit down the tree**: denying `ViewChannel` on a
category hides everything in it.

### Hierarchy

Roles are ordered by `position`; `everyone` is always 0. A user's rank is the
highest position among their roles. You may only:

- edit, move or delete a role **strictly below** your own rank,
- grant or revoke a role **strictly below** your own rank,
- act on a user whose rank is **strictly below** your own.

Additionally, a permission change may only touch bits you hold yourself. This is
what stops anyone with `ManageRoles` from promoting themselves.

`Administrator` does **not** bypass the hierarchy — otherwise two administrators
could remove each other.

## Ownership

A fresh server prints a one-time **owner token** on first start. Redeeming it
grants the managed `admin` role. The token is stored hashed, is burned on use,
and can be reissued with `aural-server -new-owner-token`.

```jsonc
{ "op": "server.claimAdmin", "d": { "token": "8Il_1-tbnCy-O1dJI-okAY_" } }
// result: { "user": User }
```

## Operations

Every op below needs an authenticated session.

| Op | Permission | Notes |
| --- | --- | --- |
| `server.claimAdmin` | — | Redeems the one-time owner token. |
| `server.update` | `ManageServer` | `{ name?, description?, klipyApiKey?, voice? }`. Persisted to the configuration file. `voice` replaces the audio plane whole and restarts every call. |
| `user.update` | `ChangeNickname`, or `ManageNicknames` for others | `{ userId?, nickname }`. Renaming somebody else works while they are offline; the rest of the fields are your own and need a connection. |
| `user.move` | `Connect` on the destination; `MoveUsers` for others | `{ userId?, channelId }`. `channelId: null` leaves. The target must be connected. |
| `user.kick` | `KickUsers` | `{ userId, reason? }`. Disconnects, so the target must be connected; there are no bans in v0.1. |
| `channel.create` | `ManageChannels` on the parent | `{ name, type, parentId?, topic?, position?, userLimit? }`. |
| `channel.update` | `ManageChannels` on the channel; `ManageRoles` to touch `overwrites` | `{ channelId, name?, topic?, parentId?, position?, userLimit?, overwrites? }`. |
| `channel.delete` | `ManageChannels` on the channel | `{ channelId }`. Cascades to descendants. |
| `message.send` | `SendMessages`, plus `AttachFiles` to carry files | `{ channelId, content, attachments? }`. Text channels only. Rate limited. |
| `message.history` | `ViewChannel` on the channel | `{ channelId, before?, after?, around?, limit? }`. One cursor at a time; `limit` defaults to 50, capped at 100. |
| `message.search` | `ViewChannel`, per channel | `{ query?, channelIds?, authorIds?, has?, after?, before?, sort?, limit?, offset? }`. Runs only over the channels the caller may read. Rate limited. |
| `message.edit` | Author only | `{ messageId, content }`. |
| `message.delete` | Author, or `ManageMessages` on the channel | `{ messageId }`. |
| `role.create` | `ManageRoles` | `{ name, color?, permissions?, hoist? }`. Lands below your rank. |
| `role.update` | `ManageRoles` | `{ roleId, name?, color?, permissions?, position?, hoist? }`. |
| `role.delete` | `ManageRoles` | `{ roleId }`. Managed roles cannot be deleted. |
| `role.assign` / `role.unassign` | `ManageRoles` | `{ userId, roleId }`. The target may be offline. |
| `voice.*` | See [Voice](#voice) | The audio plane. |

Three notes on messages:

- **Only the author may edit.** No permission overrides this, `Administrator`
  included: putting words in somebody's mouth is not moderation. A moderator who
  objects to a message deletes it, which is visible, rather than rewriting it,
  which is not.
- **`message.history` is ordered oldest first** and returns `hasMore` and
  `hasMoreAfter`, which report whether anything remains before the first entry
  or past the last one. The three cursors are exclusive of one another and all
  exclusive of the message they name: `before` pages backwards, `after` pages
  forwards, and `around` centres a page on one message, which is how a search
  result is opened in the conversation it came from. Sending none of them reads
  the newest page. Sending more than one is `bad_request`.
- **Posting is rate limited** per connection, as a token bucket. Exceeding it
  returns `rate_limited`; the message is not stored.
- **Content is sanitised, not transformed.** Control characters are dropped and
  tabs become spaces, but line breaks are kept, and so is everything that holds
  an emoji sequence together — zero width joiners, variation selectors, skin
  tone modifiers and regional indicators. `content` comes back byte for byte.

Two notes on `user.move`:

- Only **voice** channels can be joined. Text channels carry no presence and are
  selected client side.
- Leaving a voice channel closes the media session in it, whether the caller
  left or a moderator moved them. There is no way to keep audio open in a
  channel you are not in.
- `parentId` in `channel.update` is three-valued: absent leaves the parent alone,
  `null` detaches the channel to the root, and a number reparents it.

## Voice

Audio never travels on the WebSocket. That socket carries signalling — offers,
answers and ICE candidates — and the audio itself goes over WebRTC as Opus,
encoded by the sender and decoded by the receiver. Nothing in between decodes
anything, which is why a server that relays a call still needs no codec.

Sitting in a voice channel and holding a live audio session are two different
things, and every op below depends on the distinction:

- **Being in the channel** is presence. It is `user.move`, it has worked that
  way since v0.1, and it is what `User.channelId` reports.
- **Having audio** is a media session on top of it. It is `voice.connect`, and
  somebody with no microphone, or whose media never came up, is a full member of
  the channel with `connected: false`.

### Hosting modes

`ServerInfo.voice.mode` says which of the two a server runs. The signalling is
the same either way; only the peer on the far end differs.

**`server_host`** — the server relays. One peer connection carries the client's
audio up and everybody else's back down, each on its own track. It needs nothing
of the client beyond a working WebRTC stack, and no NAT traversal beyond
reaching the server, which the client has already done. It costs the operator
upstream bandwidth for every listener in every call.

**`client_host`** — the first participant to open a media session relays. It
holds one connection per other participant, plays what arrives and forwards each
person's track on to the others. It costs the server nothing. It needs a STUN
server, and usually a TURN server, in `voice.ice_servers`, because both ends are
behind somebody's router. When the relaying client leaves, the room is emptied
and everybody opens a new session; whoever gets there first hosts the next one.

### Signalling

| Op | Permission | Notes |
| --- | --- | --- |
| `voice.connect` | Already in the channel | `{ channelId, sdp? }`. `sdp` is the caller's offer in `server_host` and is absent in `client_host`. Rate limited. |
| `voice.leave` | — | Closes the media session without leaving the channel. |
| `voice.signal` | — | `{ targetId, kind, sdp?, candidate?, tracks? }`. Rate limited. |
| `voice.state` | — | `{ selfMute?, selfDeaf? }`. Absent fields are left alone. |
| `voice.moderate` | `MuteUsers` / `DeafenUsers`, and rank | `{ userId, mute?, deaf? }`. |
| `voice.speaking` | — | `{ speaking }`. Transitions only. Rate limited. |

`voice.connect` replies with `{ channelId, mode, sdp?, hostUserId?, hostEpoch?,
iceServers, voice, participants }`. In `server_host` the `sdp` is the server's
answer and the session is live. In `client_host` there is no answer: the reply
names the host, and either this client is it — in which case it waits to be told
who to dial — or it waits for that host's offer.

`targetId` on `voice.signal` is `0` for the server's own relay, which is the
only target `server_host` accepts. In `client_host` a frame may only travel
between a participant and the channel's host: the topology is a star, and a
client signalling a third party is trying to build something the server has not
agreed to carry.

`kind` is `offer`, `answer`, `candidate` or `end`. **Exactly one side of any
link offers**: the relay in `server_host`, the elected host in `client_host`.
There is no glare to resolve because there is never a second offerer.

`tracks` maps an SDP media id to the user whose audio it carries, and travels
with an offer in `client_host` only. The server-hosted relay needs none of it —
it names each participant in the stream id, as `av-<userId>` — but a relaying
browser cannot rename somebody else's track, so the host says which media id is
whose. The server passes the map along without reading it.

**A client must hold signalling it is not ready for rather than discard it.**
The relay starts gathering candidates the moment it has an answer, so its first
candidates can reach a client before the `voice.connect` reply does: both travel
on the same socket and the reply is sent last. The same race exists between two
peers in `client_host`.

### Voice state

`VoiceState` is `{ userId, channelId, connected, selfMute, selfDeaf, mute, deaf,
host }`, and one arrives in `ready.voiceStates` for every participant of every
voice channel the caller may see.

The mute flags come in pairs because they have different owners. `selfMute` is
the participant's own choice and theirs to undo; `mute` was imposed by a
moderator, or by not holding `Speak`, and is not. Unmuting yourself must not
undo somebody else's mute, which one flag could not express. Deafening implies
muting in both pairs.

In `server_host` a mute is enforced by the relay dropping what that participant
sends, so it holds whatever the client does. In `client_host` there is no relay
to enforce it: the host is told, through the same `voice.state` event everybody
receives, and a host running modified code could ignore it. That is a real
difference between the modes and is worth knowing before choosing one.

### Events

| Event | Payload | Sent to |
| --- | --- | --- |
| `voice.state` | `{ state }` | Everyone who may see the channel. |
| `voice.speaking` | `{ userId, channelId, speaking }` | Everyone who may see the channel. |
| `voice.signal` | `{ fromUserId, channelId, kind, sdp?, candidate?, tracks? }` | The addressed client. `fromUserId` is `0` for the relay. |
| `voice.peer` | `{ channelId, userId, action, epoch }` | The host of a `client_host` channel, and nobody else. |
| `voice.host` | `{ channelId, hostUserId, epoch }` | Everyone who may see the channel. |
| `voice.reset` | `{ channelId, reason }` | Whoever must start over. |

`voice.reset` is not an error. It is the single way back from everything: a host
handover (`host_changed`), an audio plane an administrator reconfigured
(`config_changed`), a transport that gave up (`failed`), and voice being
switched off (`disabled`). A client receiving one tears its media down and calls
`voice.connect` again. Making recovery the same path as an ordinary call is what
makes it a path that works.

`epoch` increments on every election, so a client can drop signalling that
belongs to a host it has already moved past.

### Codec

Opus, always, at a 48 kHz clock. `ServerInfo.voice` carries what the server will
accept before anything is negotiated:

```jsonc
{
  "enabled": true,
  "mode": "server_host",
  "sampleRate": 48000,     // maxplaybackrate; Opus's clock is always 48 kHz
  "bitrate": 64000,        // where a client starts
  "minBitrate": 16000,     // and the range it may move within
  "maxBitrate": 128000,
  "fec": true,             // useinbandfec
  "dtx": false,            // usedtx
  "stereo": false,
  "maxParticipants": 0     // 0 leaves the ceiling to the channel's user limit
}
```

`sampleRate` is the `maxplaybackrate` hint, and its permitted values are the
rates Opus actually encodes at: 8000, 12000, 16000, 24000 and 48000. **44100 is
not one of them.** Opus resamples internally to the nearest of these, so naming
44100 would ask for something the codec would silently not do; the server
refuses it rather than accepting it and doing something else.

`ServerInfo` carries no ICE servers, because `GET /info` is unauthenticated and
a TURN credential has no business being public. They travel in `ready` and in
the `voice.connect` reply, both of which are behind an identity.

### Errors

| Code | Meaning |
| --- | --- |
| `voice_disabled` | This server carries no audio. |
| `voice_failed` | The media session could not be set up. |

## Search

`message.search` looks through the history of every channel the caller may read.
It is a read, so it needs no permission of its own: what narrows it is the
permission model deciding which channels the query may run over at all. A
channel the caller cannot see is dropped from `channelIds` rather than refused,
because it is absent from their channel tree and a search must not be the one
place that admits it exists.

```jsonc
{
  "op": "message.search",
  "d": {
    "query": "deploy \"release notes\"",
    "channelIds": [2],
    "authorIds": [7],
    "has": ["link"],
    "after": 1767225600,
    "before": 1769904000,
    "sort": "relevance",
    "limit": 25,
    "offset": 0
  }
}
// result: { "hits": [ { "message": Message, "before"?: Message, "after"?: Message } ],
//           "total": 128, "offset": 0, "limit": 25 }
```

Every field narrows the result and they are combined with AND; entries within
one field are alternatives, so two authors mean "either of them". A request that
narrows nothing at all is `bad_request`: a search needs something to look for.

- **`query` is free text.** Whitespace separates terms, all of which must appear
  in the message, and double quotes hold a phrase together. At most eight terms
  are read from one query.
- **Matching is by substring, not by word.** Text is compared against a folded
  copy of the message: lower case, with Latin accents removed. So `cafe` finds
  "CAFÉ", `ploy` finds "deployment", and `世界` finds "你好世界". A word index
  would have to decide where words end, and no one tokenizer decides that
  correctly for Spanish and Chinese at once. Only the Latin blocks are folded:
  in Devanagari the Unicode category that holds Latin accents holds vowels
  instead, and stripping those would change the word.
- **`has`** is any of `link`, `file`, `image`, `video`, `sound`. `link` matches
  an http(s) URL in the text; the rest match the media type of a file the
  message carries.
- **`after` is inclusive and `before` exclusive**, both Unix seconds, so one day
  is `[start, start + 86400)`.
- **`sort`** is `newest` (the default), `oldest`, or `relevance`. Relevance has
  no corpus statistics behind it and does not pretend to: a message where the
  whole query appears as written outranks one where the words merely all appear,
  a message that repeats them outranks one that mentions them once, and recency
  breaks the remaining ties. Without terms to weigh, it is `newest`.
- **`total` counts every match**, not just the page, which is what lets a client
  page through them. `limit` defaults to 25 and is capped at 50; `offset` may not
  exceed 5000, past which refining the search finds what scrolling will not.
- **A hit carries its neighbours.** `before` and `after` are the messages either
  side of it in its own channel, absent at the edges of a history. They travel
  with the hit because a line of chat rarely means anything alone.

## Attachments

A file is posted in two steps. It is uploaded on its own, and the message that
carries it names the id that came back. That split is what lets a client show an
upload finishing while its author is still typing, and what lets the server
refuse a file on its own terms rather than in the middle of sending a message.

### `POST /upload?channel=<id>`

```
Authorization: Bearer <session token>
Content-Type: multipart/form-data, with one part named "file"
```

The token is the same one `auth.token` resumes with. The caller must hold both
`SendMessages` and `AttachFiles` in that channel, and the channel must be a text
channel. On success the reply is `201` with one **Attachment** object; on failure
it is a JSON body `{ "error": { "code", "message" } }` using the codes above, so
one table of errors covers both halves of the protocol.

An upload is **pending** until a message claims it. It belongs to the uploader
and to the channel it was made for, it can be claimed exactly once, and one that
is never posted is swept after `uploads.pending_ttl_minutes`.

Avatars and banners are recorded apart from attachments. They belong to a user
rather than to a message, so the sweep that reclaims abandoned uploads would
otherwise take every one of them; keeping them in their own table is also what
lets a restart count their bytes against `uploads.max_total_bytes`.

At startup, before the listener opens, the server walks the upload directory and
deletes files that neither table names — what a crash between writing the bytes
and writing the row leaves behind. Files written in the last minute are spared,
and files the server could not have written itself are never touched.

### `GET /attachments/{key}/{filename}`

The `key` is an unguessable 192-bit value minted per upload, and it is the whole
of the access control: a client cannot attach an `Authorization` header to an
`<img>` or `<video>` tag, so the URL is the capability. This is the same model a
CDN-backed chat uses, and it has the same consequence — **anyone given the link
can fetch the file**, whether or not they could see the channel it was posted in.

The response supports range requests, so a video can be seeked and a client can
read only the head of a large text file. Three things are fixed by the server
and never by the uploader:

- **The content type comes from the extension**, against a fixed table. Anything
  unrecognised is served as `application/octet-stream`.
- **`X-Content-Type-Options: nosniff`** is always sent, so a browser cannot sniff
  its way to a type the server did not choose.
- **Only media, PDFs and plain text are served inline.** Everything else, SVG
  included, is sent as a download. Adding `?download=1` forces a download for
  any file.

### Lifetime

Files live and die with their message. `message.delete` removes the rows and
then the files, and `channel.delete` does the same for everything the channel
held. There is no separate permission and no separate operation for deleting a
file: **moderating a file is moderating the message it arrived in**, so anyone
who may delete the message — its author, or a holder of `ManageMessages` — takes
its files with it. `message.edit` never touches attachments, which is also why a
message that carries a file may be edited down to no text at all.

Two limits are configured on the server and advertised in `ServerInfo.uploads`,
so a client can refuse a file in the picker rather than after a long transfer:
`maxFileBytes` for one file and `maxTotalBytes` for everything the server holds.
They default to 50 MiB and 5 GiB.

## Events

| Event | Payload | Sent to |
| --- | --- | --- |
| `hello` | `{ server, heartbeatMs }` | The connecting client, before auth. |
| `ready` | Full snapshot | A client whose visible state went stale. |
| `user.connected` | `{ user }` | Everyone but the arriving client. |
| `user.disconnected` | `{ userId }` | Everyone. The connection ended: a member becomes offline, a guest leaves the list. Not sent when a new connection displaced the old one. |
| `user.updated` | `{ user }` | Everyone. |
| `user.moved` | `{ userId, from, to }` | Everyone who may see either end. The other end is reported as `null`. |
| `channel.created` | `{ channel }` | Everyone who may see it. |
| `channel.updated` | `{ channel }` | Everyone who may see it. |
| `channel.deleted` | `{ channelId, cascaded }` | Everyone. |
| `message.created` | `{ message }` | Everyone who may see the channel. |
| `message.updated` | `{ message }` | Everyone who may see the channel. |
| `message.deleted` | `{ messageId, channelId }` | Everyone who may see the channel. |
| `role.created` / `role.updated` / `role.deleted` | `{ role }` / `{ roleId }` | Everyone. |
| `server.updated` | `{ server }` | Everyone. |
| `voice.*` | See [Voice](#voice) | The audio plane. |

When a permission change could add or remove channels from what somebody is
allowed to see, the affected clients receive a fresh `ready` event rather than a
patch. Patching that incrementally would cost more bookkeeping than a rare full
refresh is worth.

## Presence rules

- **One connection per identity.** A second connection with the same identity
  displaces the first, which is closed with `signed in from another connection`.
  The displaced session does not emit `user.disconnected`.
- **Channel membership is not persisted.** A user is in a voice channel for as
  long as the connection lasts, exactly as in TeamSpeak.
- A user whose permission to be somewhere is revoked is moved out immediately.
- **Messages outlive presence.** `ready.users` carries the members, but a guest
  who has been pruned is gone from it altogether, so the author of an older
  message is not always somebody the client can look up. That is why every
  message carries `author` as well as `userId`.
- **Invisible is offline.** A member who sets `invisible` is reported to
  everybody else exactly as a member who is away: `online: false`,
  `status: "offline"`, no channel and no custom status. Going invisible reaches
  other clients as `user.disconnected` and coming back as `user.updated`, which
  are the frames a real disconnect and a real arrival send. While hidden they
  generate no events of their own, because somebody away generates none either.
  A guest has no offline entry to hide in, so a hidden guest is left out of the
  list entirely instead.
- **A change made to somebody who is away is still announced.** Renaming a
  member or granting them a role sends `user.updated` carrying their offline
  entry, whether they are absent or hiding: these are the only things that
  happen to a member who is not connected.

## Not in version 1

Bans, per-user permission overwrites, and screen sharing. Video would reuse the
whole of [Voice](#voice) — the signalling is codec-agnostic — and needs a
second track and a way to say which is which.

Voice states are not persisted, for the same reason channel membership is not:
a mute belongs to a session. An identity that reconnects arrives unmuted, and a
moderator's mute has to be applied again.
