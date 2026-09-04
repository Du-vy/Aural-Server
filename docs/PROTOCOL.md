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
the socket. Everything but the first two and the webhook routes takes the same
bearer session token the socket resumes with, in an
`Authorization: Bearer <token>` header; a webhook carries its credential in its
own path instead:

| Endpoint  | Purpose                                                              |
| --------- | -------------------------------------------------------------------- |
| `GET /info`   | Unauthenticated server preview. A client shows it before connecting, and the future Aural Hub reads it to list servers. Sends `Access-Control-Allow-Origin: *`. |
| `GET /health` | Liveness probe for process supervisors. Returns `ok`.            |
| `POST /upload` | Uploads one file. See [Attachments](#attachments).               |
| `POST /upload/avatar`, `POST /upload/banner` | Replaces the caller's own avatar or profile banner. |
| `GET /attachments/{key}/{filename}` | Serves an uploaded file.                |
| `GET /unfurl?url=` | Fetches a page and returns its OpenGraph metadata. See [Link previews](#link-previews). |
| `GET /klipy/{kind}/{action}` | Proxies a GIF or sticker lookup. See [GIFs and stickers](#gifs-and-stickers). |
| `/api/webhooks/{id}/{token}` and below | Discord's webhook API, so an application already posting to one works here by changing the URL. See [Webhooks](#webhooks). |

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
| `dm_disabled` | This server does not carry private conversations. |
| `dm_blocked` | A privacy setting refuses this private message. |
| `banned` | A ban in force catches this connection. The message names the reason and, when the ban ends, the date. |
| `automod_blocked` | A rule refused the message. Separate from `forbidden`: nothing about the writer is wrong. |
| `expression_limit` | This server already holds as many emoji, stickers or sounds as it is configured to. |

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

Both sides advertise a **range** of revisions rather than a single one. The
server sends `server.protocolVersion` (the newest it speaks) and
`server.minProtocolVersion` (the oldest it still accepts); the client holds the
same pair for itself. They talk when the two ranges overlap, and the revision
in use is the newest both ends can speak.

A client should refuse to continue only when there is no overlap: the server is
newer than anything the client understands, or older than anything it still
supports. `minProtocolVersion` is absent from a server older than this rule, and
a client that finds it missing should read it as equal to `protocolVersion`.

The point of the range is that the two sides are updated by different people. A
server is self-hosted, so it moves when its operator pulls a new image, while a
client updates itself. Strict equality would mean the first breaking change cut
every client off from every server whose operator had not pulled yet.

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
  "server":      ServerInfo,
  "conversations": [Conversation] // private threads, newest first; absent when off
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
  "owner": true,           // absent unless this identity owns the server
  "nickname": "Pablo",
  "username": "pablo",     // null while still a guest
  "registered": true,
  "roles": [1, 2],
  "channelId": 3,          // null when in no voice channel
  "online": true,
  "dmPrivacy": "everyone"  // only on your own entry; see Private conversations
}

// Channel — "category" holds others; every other type is a leaf
{
  "id": 3,
  "parentId": 1,           // null at the tree root
  "name": "Lobby",
  "type": "voice",         // category | text | voice
                           // | announcement | forum | media | calendar
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

// Post — one entry of an announcement, forum, media or calendar channel
{
  "id": 7,
  "channelId": 5,
  "userId": 4,             // null once the author's account is gone
  "author": "Pablo",
  "title": "Server downtime on Friday",
  "locked": false,         // closed: no more comments, no more answers
  "pinned": false,         // floats to the top of its channel
  "createdAt": 1756600000,
  "editedAt": null,
  "body": { /* Message */ },   // the first message of the thread
  "comments": 3,               // messages under it, body not counted
  "lastCommentAt": 1756600900, // creation time when nobody has answered
  "event": {                   // calendar channels only
    "startsAt": 1756684800,
    "endsAt": 1756692000,      // absent when no finish was stated
    "allDay": false,
    "location": "Meeting room 2"
  },
  "rsvp": {                    // travels with an event
    "going": 4, "maybe": 1, "declined": 0,
    "own": "going"             // the answer of whoever this frame went to
  }
}

// Message
{
  "id": 12,
  "channelId": 2,
  "postId": 7,             // absent on a message written into a text channel
  "userId": 4,             // null once the author's account is gone
  "author": "Pablo",       // resolved live from the users table
  "content": "Hello",
  "createdAt": 1756600000, // Unix seconds
  "editedAt": null,
  "attachments": [],       // absent when the message carries no files
  // Both absent on everything a person wrote. See Webhooks.
  "webhook": { "id": 3, "avatar": "https://..." },
  "embeds": []             // Discord-shaped cards; snake_case inside
}

// Webhook — a URL that posts into one channel. Only ever sent to somebody
// holding ManageWebhooks there, because the token is the whole of its
// authentication.
{
  "id": 3,
  "channelId": 2,
  "name": "Buildbot",
  "avatar": "https://example.com/icon.png", // absent when it has none
  "token": "kK1...",
  "url": "/api/webhooks/3/kK1...",  // relative to the server root
  "creatorId": 4,          // absent once that account is gone
  "createdAt": 1756600000,
  "lastUsedAt": 0          // zero until the first delivery
}

// DirectMessage — one line of a private conversation
{
  "id": 4,
  "conversationId": 2,
  "userId": 4,             // null once the author's account is gone
  "author": "Pablo",       // resolved live, exactly as on Message
  "content": "hello",
  "createdAt": 1756600000,
  "editedAt": null
}

// Conversation — one private thread, as it looks to one of the two in it
{
  "id": 2,
  "userId": 7,             // the OTHER participant, from the reader's side
  "lastMessageAt": 1756600000,
  "lastMessage": DirectMessage, // absent until something is said
  "unread": 3
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
  "protocolVersion": 1,       // newest revision this server speaks
  "minProtocolVersion": 1,    // oldest it still accepts
  "softwareVersion": "0.5.0",
  "maxUsers": 64,
  "onlineUsers": 3,
  "passwordProtected": false,
  "registrationEnabled": true,
  "guestsAllowed": true,
  "voiceMode": "client_host",  // client_host | server_host
  "directMessages": true,      // whether this server carries private conversations
  "uploads": {
    "enabled": true,
    "maxFileBytes": "52428800",     // decimal strings, for the same reason
    "maxTotalBytes": "5368709120",  // "0" means no ceiling but the disk
    "usedBytes": "1048576",
    "maxPerMessage": 10
  },
  "expressions": {
    "maxEmojis": 50,
    "maxStickers": 50,
    "maxSounds": 50,
    "maxSoundSeconds": 10,        // the trimmer cuts to this before uploading
    "maxEmojiBytes": "524288",
    "maxStickerBytes": "1048576",
    "maxSoundBytes": "4194304"
  }
}

// Ban - one standing refusal
{
  "id": 3,
  "userId": 9,                 // null once the identity is gone, which for a
                               // guest is immediately
  "userNickname": "Nuisance",
  "userUsername": null,
  "actorId": 4,
  "actorNickname": "Pablo",
  "reason": "spam",
  "createdAt": 1735689600,
  "expiresAt": null,           // null is permanent
  "active": true,              // false once the date has passed; the row stays
  // What it catches, counted rather than named: an address and a device hash
  // identify somebody off this server too.
  "matches": [
    { "kind": "user", "count": 1 },
    { "kind": "device", "count": 2 }
  ]
}

// AuditEntry - one line of the record of what moderators did
{
  "id": 41,
  "actorId": 4,
  "actorName": "Pablo",
  "action": "user.ban",
  "targetType": "user",        // user | role | channel | message | post
                               // | server | webhook | expression | sound
  "targetId": 9,
  "targetName": "Nuisance",    // captured as it read at the time
  "reason": "spam",
  "changes": [{ "key": "expires", "before": "", "after": "2026-01-01T00:00:00Z" }],
  "createdAt": 1735689600
}

// Expression - a custom emoji or sticker
{
  "id": 2,
  "kind": "emoji",             // emoji | sticker
  "name": "shrug",             // what writers type between colons
  "url": "/attachments/<key>/shrug.png",
  "animated": false,
  "size": "4096",
  "creatorId": 4,
  "createdAt": 1735689600
}

// Sound - one soundboard clip
{
  "id": 1,
  "name": "Airhorn",
  "emoji": "",                 // the glyph on the button; may be empty
  "url": "/attachments/<key>/airhorn.wav",
  "durationMs": 2000,
  "volume": 100,               // 0-100, the clip's own level
  "size": "192044",
  "creatorId": 4,
  "createdAt": 1735689600
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
| 7 | `SendDirectMessages` | Write to another member privately |
| 8 | `ManageChannels` | Create, edit and delete channels |
| 9 | `ManageRoles` | Manage roles and channel overwrites |
| 10 | `ManageServer` | Rename the server |
| 11 | `ManageNicknames` | Change other nicknames |
| 12 | `ManageMessages` | Delete other people's messages |
| 13 | `ManageWebhooks` | Create, edit and revoke the webhooks of a channel |
| 14 | `CreatePosts` | Start an entry in an announcement, forum, media or calendar channel |
| 15 | `UseSoundboard` | Play one of the server's stored sounds at a voice channel |
| 16 | `KickUsers` | Disconnect a user |
| 17 | `MoveUsers` | Move a user between voice channels |
| 18 | `MuteUsers` | Reserved for voice moderation |
| 19 | `DeafenUsers` | Reserved for voice moderation |
| 20 | `BanUsers` | Refuse somebody the server, and the address and device behind them |
| 21 | `ViewAuditLog` | Read the record of what moderators did |
| 22 | `ManageExpressions` | Upload and remove custom emoji, stickers and soundboard sounds |
| 31 | `Administrator` | Everything, unconditionally |

### Resolution

0. If the user owns the server, they hold everything. Stop. Ownership is not a
   role, so this holds however few roles they have.
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
highest position among their roles, and the owner's rank is above every role
there is. You may only:

- edit, move or delete a role **strictly below** your own rank,
- grant or revoke a role **strictly below** your own rank,
- act on a user whose rank is **strictly below** your own.

Additionally, a permission change may only touch bits you hold yourself. This is
what stops anyone with `ManageRoles` from promoting themselves.

`Administrator` does **not** bypass the hierarchy — otherwise two administrators
could remove each other. The managed `admin` role is the top of the stack, so
its holders cannot edit it, move it, or hand it out; the owner, standing above
it, can do all three.

## Ownership

A fresh server prints a one-time **owner token** on first start. Redeeming it
makes the redeeming identity the **owner** of the server. The token is stored
hashed, is burned on use, and can be reissued with
`aural-server -new-owner-token` — redeeming a replacement hands the server over,
which is how an operator recovers from a lost owner account.

Ownership is a property of the identity rather than a role it holds, as in
Discord. It grants every permission and a rank above every role, it appears
nowhere in the role editor, and it survives the owner holding no role at all.
The user object carries `"owner": true` for whoever holds it.

```jsonc
{ "op": "server.claimAdmin", "d": { "token": "8Il_1-tbnCy-O1dJI-okAY_" } }
// result: { "user": User }
```

## Operations

Every op below needs an authenticated session.

| Op | Permission | Notes |
| --- | --- | --- |
| `server.claimAdmin` | — | Redeems the one-time owner token and makes the caller the owner. Grants no role. |
| `server.update` | `ManageServer` | `{ name?, description?, klipyApiKey?, voice? }`. Persisted to the configuration file. `voice` replaces the audio plane whole and restarts every call. |
| `user.update` | `ChangeNickname`, or `ManageNicknames` for others | `{ userId?, nickname }`. Renaming somebody else works while they are offline; the rest of the fields are your own and need a connection. |
| `user.move` | `Connect` on the destination; `MoveUsers` for others | `{ userId?, channelId }`. `channelId: null` leaves. The target must be connected. |
| `user.kick` | `KickUsers` | `{ userId, reason?, deleteMessages? }`. Ends the connection and removes the identity. `deleteMessages` purges what they wrote: `none`, `1d`, `7d`, `30d` or `all`. |
| `ban.list` | `BanUsers` | `{}`. Newest first. The handles a ban catches are counted, never named. |
| `ban.create` | `BanUsers`, and rank over the target | `{ userId, reason?, duration?, deleteMessages?, matchIp?, matchDevice? }`. `duration` is in seconds; zero, or absent, is permanent. Both match flags default to on. |
| `ban.delete` | `BanUsers` | `{ banId }`. Lifts the ban and every handle it held. |
| `audit.list` | `ViewAuditLog` | `{ actorId?, action?, before?, limit? }`. Pages backwards by entry id. There is no op to write one. |
| `automod.get` | `ManageServer` | `{}`. The whole rule set. |
| `automod.update` | `ManageServer` | `{ config }`. Replaces it whole. What comes back is what is now in force, bounded and de-duplicated. |
| `expression.update` | `ManageExpressions` | `{ expressionId, name }`. Renames a custom emoji or sticker. The picture itself is never edited. |
| `expression.delete` | `ManageExpressions` | `{ expressionId }`. Takes the file with it. |
| `sound.update` | `ManageExpressions` | `{ soundId, name?, emoji?, volume? }`. `volume` runs 0–100. |
| `sound.delete` | `ManageExpressions` | `{ soundId }`. Takes the file with it. |
| `sound.play` | `UseSoundboard` in the channel you are sitting in | `{ soundId }`. The channel is not a parameter: it is wherever you are. Refused while moderator-muted. Rate limited far more tightly than a message. |
| `channel.create` | `ManageChannels` on the parent | `{ name, type, parentId?, topic?, position?, userLimit? }`. |
| `channel.update` | `ManageChannels` on the channel; `ManageRoles` to touch `overwrites` | `{ channelId, name?, topic?, parentId?, position?, userLimit?, overwrites? }`. |
| `channel.delete` | `ManageChannels` on the channel | `{ channelId }`. Cascades to descendants. |
| `post.create` | `CreatePosts` on the channel, plus `AttachFiles` to carry files | `{ channelId, title, content?, attachments?, event? }`. Post channels only. A media post needs a file; a calendar post needs an `event` and nothing else may carry one. Rate limited on the same bucket as `message.send`. |
| `post.list` | `ViewChannel` on the channel | `{ channelId, before?, from?, to?, limit? }`. `before` pages backwards by post id; `from`/`to` read a calendar as a window in time, at most a year wide. |
| `post.update` | Author for `title` and `event`; `ManageMessages` for `locked` and `pinned` | `{ postId, title?, locked?, pinned?, event? }`. The body is a message: it is edited through `message.edit`. |
| `post.delete` | Author, or `ManageMessages` on the channel | `{ postId }`. Takes the whole thread, and its files, with it. |
| `post.rsvp` | `ViewChannel` on the channel | `{ postId, response }`. `going`, `maybe`, `declined`, or `""` to withdraw. Calendar posts only, and not on a locked one. |
| `message.send` | `SendMessages`, plus `AttachFiles` to carry files | `{ channelId, content, postId?, attachments? }`. Without `postId`, a text channel only. With one, a comment on that post, which must be in the channel named and must not be locked. Rate limited. |
| `message.history` | `ViewChannel` on the channel | `{ channelId, postId?, before?, after?, around?, limit? }`. One cursor at a time; `limit` defaults to 50, capped at 100. With `postId`, reads that post's comments, `before` only, and the body is not among them. |
| `message.search` | `ViewChannel`, per channel | `{ query?, channelIds?, authorIds?, has?, after?, before?, sort?, limit?, offset? }`. Runs only over the channels the caller may read. Rate limited. |
| `message.edit` | Author only | `{ messageId, content }`. |
| `message.delete` | Author, or `ManageMessages` on the channel | `{ messageId }`. Refused for the body of a post: `post.delete` is the act that was meant. |
| `dm.list` | — | `{}`. Every private conversation you are in, newest first. |
| `dm.history` | — | `{ userId, before?, after?, around?, limit? }`. Cursors work as in `message.history`. A thread that does not exist yet returns `conversationId: 0` and no messages. |
| `dm.send` | `SendDirectMessages`, plus both privacy settings | `{ userId, content }`. Opens the conversation if it is the first thing either has said. Rate limited on the same bucket as `message.send`. |
| `dm.edit` | Author only | `{ messageId, content }`. |
| `dm.delete` | Author only | `{ messageId }`. There is no moderator in a private conversation. |
| `dm.read` | — | `{ userId, messageId }`. Moves your own read marker; it never moves backwards. |
| `role.create` | `ManageRoles` | `{ name, color?, permissions?, hoist? }`. Lands below your rank. |
| `role.update` | `ManageRoles` | `{ roleId, name?, color?, permissions?, position?, hoist? }`. |
| `role.delete` | `ManageRoles` | `{ roleId }`. Managed roles cannot be deleted. |
| `role.assign` / `role.unassign` | `ManageRoles` | `{ userId, roleId }`. The target may be offline. |
| `webhook.list` | `ManageWebhooks`, per channel | `{ channelId? }`. Omit the channel to read every one the caller may manage. A channel they may not is left out rather than refused. Carries the tokens. |
| `webhook.create` | `ManageWebhooks` on the channel | `{ channelId, name, avatar? }`. Text channels only, at most 15 per channel. |
| `webhook.update` | `ManageWebhooks` on both channels | `{ webhookId, name?, avatar?, channelId? }`. Moving one needs the permission where it is going, since that is the same thing as minting one there. |
| `webhook.delete` | `ManageWebhooks` on the channel | `{ webhookId }`. Revokes the URL; what it posted stays. |
| `relay.get` | `ManageServer` | No body. The whole relay state. See [The Discord relay](#the-discord-relay). |
| `relay.configure` | `ManageServer` | `{ enabled, botToken? }`. Omitting the token keeps the one stored; it is never sent back. |
| `relay.create` | `ManageServer` | `{ channelId, webhookUrl, discordChannelId?, direction?, attachments, edits }`. The webhook URL is verified against Discord before anything is written. |
| `relay.update` | `ManageServer` | `{ id, channelId?, webhookUrl?, direction?, enabled?, attachments?, edits? }`. Anything omitted is left as it was. |
| `relay.delete` | `ManageServer` | `{ id }`. Unpairs the channels; what already crossed stays on both sides. |
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

## Moderation

### Bans

A kick ends a connection. A ban is a standing decision, and it is stored as one:
a row saying who, why, by whom and until when, and a set of **handles** it is
matched against.

There are three kinds of handle, and a ban usually carries several:

| Kind | What it is | What it is worth |
| --- | --- | --- |
| `user` | The identity itself | Exact, and replaced for free on a server that hands out guest identities |
| `ip` | The address it connected from | Changes on a reboot; shared by a household, a university, one phone network |
| `device` | The machine behind it, as this server sees it | Survives a new account and a cleared browser profile |

Which handles a ban picks up is decided by the server, from where that identity
has actually been seen — the connection it is on right now, and the last few
addresses and devices recorded against it. `matchIp` and `matchDevice` are the
switches that say how far to reach.

**The device handle is a hash the client computes.** `hello` carries a
`deviceSalt`: a random value this server minted once and keeps. A client folds
it into whatever stable attributes it can read about the machine and sends the
hash as `device` on the auth op that follows. So the value identifies a machine
*on this server*, which is what makes a ban survive a new account, and is
unrelated to the value the same machine presents anywhere else, so it cannot be
used to follow somebody between servers. A client that sends none matches
nothing, which is not an error: it is a way of making a ban stick, not a
credential.

None of this is a wall, and it is not meant to be. Somebody who reinstalls their
system, or runs a client they modified, is through. What it raises is the cost of
the ordinary case: closing the client, opening it again, and coming straight back
as a new guest.

**Bans are enforced on the authentication op, never on the upgrade.** An address
is shared, so the one check that could not tell who was asking is the one that
would eventually lock somebody's own staff out. By the time the auth op runs the
identity is known, and:

- **The owner is never refused**, however a ban was issued. A server whose owner
  cannot get in has no way back.
- **A handle the moderator issuing the ban, or the owner, is also sitting behind
  is not attached to it.** Banning a troublemaker from your own network must not
  ban you.

The ban list never carries the handle values. A moderator deciding whether to
lift a ban needs to know that it reaches a machine, not which machine, and an
address identifies somebody off this server as well as on it.

A ban whose date has passed stops being enforced the moment it does, and stays
in the list as a record of what was done.

### The audit log

Every moderation action writes one entry: who did it, what they did, what it was
done to, and which fields changed. Everything about the target is captured as it
read at the time, so a role deleted last week still has its name in the log.

There is no op to write an entry. A record a client could append to would be a
record of what somebody claimed to have done.

`audit.entry` is pushed to the sessions holding `ViewAuditLog` as entries are
written, so an open settings screen stays current.

A blocked message is deliberately not copied into the log. It was never accepted
by this server, and writing it into a table more people can read than could have
read the message would make the blocking the thing that published it.

### Automatic moderation

`automod.get` and `automod.update` read and write one object. It is whole rather
than per field because the rules constrain one another, and a half-applied edit
is not a state worth being able to reach.

Six rules, each with `enabled`, an `action`, and its own `exemptRoles` on top of
the server-wide list:

| Rule | Matches | Actions |
| --- | --- | --- |
| `words` | Listed terms, folded the way search folds them, so one entry catches every capitalisation and accent. `wholeWord` keeps a list from catching an innocent word that contains a banned one. | `block`, `censor` |
| `links` | URLs, including the ones written without a scheme. `allowedDomains` are let through, and an entry covers its own subdomains. | `block`, `censor` |
| `mentions` | More than `limit` people addressed in one message. | `block` |
| `caps` | More than `percent` of the cased letters upper case, ignoring messages shorter than `minLength`. | `block` |
| `flood` | More than `messages` sent within `seconds`. | `block` |
| `repetition` | The same message `times` in a row. | `block` |

A rule that has nothing to mask can only block: a message that mentions too many
people cannot be partly mentioned. The server rewrites `censor` to `block` on
those rather than refusing the edit.

Rules run on the way in, so a blocked message never existed and a censored one
was never stored uncensored. Edits and post titles are screened too — a rule
that only looked at sends would be worked around by posting a full stop and
editing it — but neither counts towards `flood` or `repetition`: an edit is not
a new message.

Holding `Administrator` is exemption in itself, which is the rule the permission
mask follows everywhere else on this server.

A refused message answers `automod_blocked`, with a message naming what stopped
it. It is a separate code from `forbidden` because nothing about the writer is
wrong: the same person may send the same message with one word changed.

## Expressions and the soundboard

### Custom emoji and stickers

One table, one namespace, one management screen. They differ in where a client
draws them: an emoji goes inline in a line of text, a sticker is the whole of a
message.

Uploading is HTTP, like every other file:

```
POST /upload/emoji?name=<name>
POST /upload/sticker?name=<name>
Authorization: Bearer <session token>
Content-Type: multipart/form-data, one part named "file"
```

`ManageExpressions`, PNG / GIF / WebP / JPG, and one name per kind. A name is
two to thirty-two characters of letters, digits and underscores — narrow because
it sits between two colons in the middle of a sentence, and anything that could
also be punctuation would make `:name:` ambiguous with the text around it.

The reply is an `Expression`, and `expression.created` reaches everybody.

**Nothing is rewritten on the way in.** A message written with `:shrug:` stores
`:shrug:`, and a client resolves it against the emoji table when it renders. So
history survives an emoji being renamed or deleted, and reading a server that
carries none shows the colons somebody actually typed.

The whole table travels in `ready`, because a message cannot be rendered without
it: `:shrug:` in the very first line of history has to resolve before anything is
drawn.

### The soundboard

Short clips anybody in a voice channel may play at the room.

```
POST /upload/sound?name=<name>&emoji=<emoji>
Authorization: Bearer <session token>
Content-Type: multipart/form-data, one part named "file"
```

`ManageExpressions`, and **WAV only**. That is what makes the length limit
enforceable rather than merely declared: a RIFF header states its own duration,
so the server reads how long a clip runs instead of believing a number the
uploader chose. The client decodes whatever was picked, cuts the range somebody
chose, and re-encodes — which is also what makes trimming a three-minute song
down to eight seconds something that happens in the picker rather than in
another application.

The ceilings are per server and are advertised in `ServerInfo.expressions`, so a
client knows before it uploads: `maxEmojis`, `maxStickers`, `maxSounds`,
`maxSoundSeconds`, and a byte ceiling for each kind. They are configured under
`expressions` in the configuration file.

`sound.play` names only the clip. The channel is wherever the caller is sitting,
because playing a sound into a room you are not in is not something anybody
should be able to do. The server checks `UseSoundboard` there, refuses a
participant a moderator has muted, and pushes `sound.played` to everybody in the
channel.

**Each client fetches the clip and mixes it into its own output.** Nothing is
injected into anybody's microphone. So it sounds the same to everybody, it works
identically whether the call is relayed by the server or by another participant,
and being deafened silences it exactly as it silences everything else.

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

### Webhooks

A webhook is a URL that posts into one text channel with no identity behind it.
It is what a build server, a monitor or an automation is given so that what it
has to say arrives in a channel without anybody writing a client.

**The whole surface is Discord's, byte for byte.** The path, the payload, the
status codes, the error bodies and the rate-limit headers are the ones an
application posting to a Discord webhook is already written against, so an
operator who has one of those changes the URL and nothing else. That
compatibility is the feature; everything odd-looking below follows from it.

### The URL

```
http(s)://<server>/api/webhooks/{id}/{token}
```

`webhook.create` returns the path; a client resolves it against the address it
already reached the server by, exactly as it does an attachment URL. An explicit
API version is accepted and ignored, so `/api/v10/webhooks/{id}/{token}` — the
shape some tools rewrite a pasted URL into — reaches the same endpoint.

The token is the whole of the authentication. There is nothing else to present
and nothing else to check: **anyone holding the URL can post to that channel**,
which is the same bargain Discord makes and the reason the token is stored as it
was minted rather than hashed. It has to be readable back out of the settings
screen every time somebody wires up an integration, and deleting the webhook is
how it is revoked.

### `POST /api/webhooks/{id}/{token}`

The body is either JSON or `multipart/form-data` with a `payload_json` part and
one part per file.

| Field | Effect |
| --- | --- |
| `content` | The message text. At most 2000 characters, cleaned the same way `message.send` cleans one. |
| `username` | Overrides the name **this message** is attributed to. The webhook keeps its own. |
| `avatar_url` | Overrides the picture for this message. Must be `http(s)`. |
| `embeds` | Up to 10 cards, in Discord's embed shape (see below). |
| `tts`, `allowed_mentions`, `components`, `poll`, `flags`, `thread_name`, `applied_tags`, `attachments` | Accepted and ignored. |

Anything else in the body is ignored rather than refused, so a payload written
against a newer Discord still delivers its message.

A delivery must carry **something**: content, a card or a file. One that carries
none is `400` with Discord's `50006`. Everything else that is over a limit is
**clamped rather than refused** — a title cut to 256 characters, a colour masked
into 24 bits, a URL dropped if its scheme is not `http(s)` — because a rejection
turns a cosmetic overflow into a notification that never arrives.

The reply is `204` with no body, or `200` with the message when `?wait=true` is
given. That message is in Discord's shape, ids as decimal strings, and the field
worth reading is `id`: it is what the message endpoints below take.

Files are bound to the message as it is written. There is no pending step and no
`AttachFiles` check, because a webhook has no session to hold an upload between
the two and no identity to hold a permission. They are otherwise ordinary
attachments — same directory, same quota, same URLs, and they die with the
message.

### `POST /api/webhooks/{id}/{token}/slack` and `/github`

The two payload dialects Discord also accepts on a webhook URL.

`slack` takes a Slack message — `text`, `username`, `icon_url`, and
`attachments` with their `color`, `title`, `fields` and `footer` — and renders
each attachment as one card. Slack's `<url|label>` link syntax is rewritten to
Markdown. It answers with a bare `ok`, which is what tools written against Slack
check for.

`github` takes GitHub's own event schema and the `X-GitHub-Event` header, and
renders the event as a card: pushes, issues, comments, pull requests, reviews,
releases, branches, forks, stars and workflow runs each get their own shape, and
anything else gets a line naming the event. An event with nothing worth drawing
is answered `204` rather than refused, because a 4xx shows in the repository as
a failed hook and earns a retry.

### The message a webhook posted

| Route | Does |
| --- | --- |
| `GET /api/webhooks/{id}/{token}` | The webhook object. |
| `PATCH /api/webhooks/{id}/{token}` | `{ name?, avatar? }`. `avatar` is a URL; this server hosts no picture for a webhook. |
| `DELETE /api/webhooks/{id}/{token}` | Revokes the URL. |
| `GET`/`PATCH`/`DELETE /api/webhooks/{id}/{token}/messages/{messageId}` | Read, rewrite or remove one message. |

`PATCH` on a message is a patch: an absent `content` or `embeds` is left alone.
It is what a status page or a long-running job uses to keep one message current
instead of posting a hundred, and it is why the execute endpoint bothers to
answer with an id.

**A webhook may only touch its own messages.** One posted by somebody else — or
by another webhook — reports `10008 Unknown Message`, exactly as one that does
not exist. The URL is a way to post, not a moderation credential.

### Rate limits

One bucket per webhook: a burst of 5, refilling at Discord's own ceiling for a
channel of thirty messages a minute. Every response carries `X-RateLimit-Limit`,
`-Remaining`, `-Reset`, `-Reset-After` and `-Bucket`; a rejection is `429` with
`Retry-After` and the body

```json
{ "message": "You are being rate limited.", "retry_after": 1.234, "global": false }
```

### Errors

JSON, in Discord's shape rather than this protocol's, because a client library
switches on the numbers: `{ "message": "...", "code": 10015 }`. The codes raised
are `10008` unknown message, `10015` unknown webhook, `40005` request too large,
`50001` missing access, `50006` empty message, `50027` invalid token and `50035`
invalid form body.

These routes answer any origin. A webhook carries its own credential in its path
and reads no cookie and no session, so the browser's origin has nothing to do
with whether a delivery is allowed.

### Managing them

Four ops, listed with the rest in [Operations](#operations). There are **no
webhook events**, deliberately: a webhook object carries the token that is the
whole of its authentication, so it is only ever handed to somebody who asked for
it and holds `ManageWebhooks` in that channel. A screen that shows them re-reads
the list after every change.

A channel may hold at most 15 webhooks. Deleting one revokes the URL and leaves
the history alone: what was posted through it keeps the name and picture it was
posted under, because the history is a record of what was said rather than of
who may still say it.

### Embeds

`Embed` and everything under it are **Discord's objects, field for field and
name for name, `snake_case` included** — the one place this protocol does not
use `camelCase`. Translating them would mean a second specification to keep in
step with somebody else's, and every field that fell out of step would be one a
service could send and a reader could not show.

```json
{
  "title": "Disk almost full",
  "description": "`/dev/sda1` is at **91%**",
  "url": "https://example.com/alert",
  "timestamp": "2026-09-03T10:00:00Z",
  "color": 15029579,
  "footer": { "text": "node-1", "icon_url": "https://…" },
  "image": { "url": "https://…", "width": 800, "height": 400 },
  "thumbnail": { "url": "https://…" },
  "author": { "name": "monitor", "url": "https://…", "icon_url": "https://…" },
  "fields": [{ "name": "Host", "value": "node-1", "inline": true }]
}
```

Limits are Discord's: 10 embeds, a 256-character title, a 4096-character
description, 25 fields of 256 and 1024, a 2048-character footer and a
256-character author name. `color` is a 24-bit RGB integer. Descriptions and
field values are Markdown.


## Events

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

## Private conversations

A conversation is a **pair of identities** and nothing else: no name, no topic,
no membership, because there is never anybody to add. It is addressed on the
wire by the other person's `userId`, never by an id the client would first have
to learn — a name in the member list is all somebody has to start from.

The same thread therefore reaches its two sides under two different names:
`conversation.userId` is always *the other one*, from the point of view of
whoever the frame was sent to. That is what lets a client key its conversations
by person and render the first frame that arrives without holding a map.

Private messages carry no attachments. An upload is bound to the channel it was
made for, and there is no channel here to bind one to.

### Privacy

Every identity carries `dmPrivacy`, one of:

| Value | Who may write to you |
| --- | --- |
| `everyone` | Anybody on this server. The default. |
| `registered` | Only members who have claimed an account. |
| `none` | Nobody. |

It is set with `user.update` (`{ "dmPrivacy": "registered" }`), only ever on
yourself: no permission lets anybody set it for somebody else.

Two rules follow it:

- **It is read from both ends of a send.** A message is refused with
  `dm_blocked` when the recipient will not hear from the sender *or* when the
  sender's own setting excludes the recipient. Otherwise somebody who wants no
  private messages could still open a thread nobody may answer, which is a
  worse place to be than either answer alone.
- **It is nobody else's business.** `dmPrivacy` is populated only on your own
  entry; every other copy of you carries an empty string. Finding out that a
  message will not be delivered is what sending one is for.

`SendDirectMessages` gates the feature by role, and `server.allow_direct_messages`
switches it off for the whole server — advertised as `directMessages` in
`ServerInfo`, and every op behind it then answers `dm_disabled`.

### Unread

The server keeps a **read marker** per participant: the id of the newest line
that side has seen. `conversation.unread` counts what sits past it, so a badge
survives a restart rather than being whatever the client happened to see live.
Sending moves your own marker, which is why your own writing is never unread,
and `dm.read` only ever moves it forwards.

## Posts

Four channel types hold **posts** rather than a stream of messages:
`announcement`, `forum`, `media` and `calendar`. A post is a title and some
metadata in front of an ordinary thread.

The body of a post is a `Message` carrying the post's id, and the comments under
it are messages carrying the same id. There is no second kind of message and no
second kind of attachment: files, edits, deletion, moderation and rate limits
all reach a post exactly as they reach a line of a text channel.

That has three consequences worth stating plainly:

- **A channel's timeline never holds its posts' messages.** `message.history`
  without a `postId` reads the messages of a channel that belong to no post,
  which in a post channel is none of them. Reading one with a `postId` reads
  that thread, and the body is not part of the page: it arrives with the post.
- **The body is deleted with the post, never on its own.** `message.delete`
  refuses it, because a post whose body went would be a title standing over
  nothing. `post.delete` takes the whole thread, and its files, together.
- **Search does not reach posts in v1.** It runs over text channels, which is
  where it has always run.

The four types differ in what an entry carries and in who may write one, not in
the ops that reach them:

| Type | An entry is | Made by |
| --- | --- | --- |
| `announcement` | A notice everybody may comment on | Denying `CreatePosts` to `@everyone` in an overwrite, leaving `SendMessages` alone |
| `forum` | A topic anybody may start | The default: `CreatePosts` is in `DefaultEveryone` |
| `media` | A file, with the words optional | The server refuses an entry with no file |
| `calendar` | Something that happens at a time | The server requires `event`, and refuses one anywhere else |

Pinned entries travel with the first page of a listing however old they are, and
take no part in the cursor: paging back never repeats one. A client floats them
to the top of what it holds.

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
| `post.created` | `{ post }` | Everyone who may see the channel. |
| `post.updated` | `{ post }` | Everyone who may see the channel. `rsvp.own` is empty in the broadcast; the caller's own reply carries theirs. |
| `post.deleted` | `{ postId, channelId }` | Everyone who may see the channel. |
| `post.rsvp` | `{ postId, channelId, userId, response, rsvp }` | Everyone who may see the channel. The tallies are everybody's and `userId` names whose answer changed, so `rsvp.own` is empty: a client updates its own only when `userId` is its own. |
| `message.created` | `{ message }` | Everyone who may see the channel. A comment carries `postId`. |
| `message.updated` | `{ message }` | Everyone who may see the channel. |
| `message.deleted` | `{ messageId, channelId }` | Everyone who may see the channel. |
| `dm.created` | `{ conversation, message }` | The two people in the thread, each naming the other. |
| `dm.updated` | `{ userId, message }` | Both, when a line is edited. |
| `dm.deleted` | `{ userId, conversationId, messageId }` | Both, when a line is removed. |
| `role.created` / `role.updated` / `role.deleted` | `{ role }` / `{ roleId }` | Everyone. |
| `server.updated` | `{ server }` | Everyone. |
| `voice.*` | See [Voice](#voice) | The audio plane. |
| `ban.created` / `ban.deleted` | Reaches the sessions holding `BanUsers`. |
| `audit.entry` | One new line of the log. Reaches the sessions holding `ViewAuditLog`. |
| `automod.updated` | Reaches the sessions holding `ManageServer`. |
| `relay.updated` | `{ relay }`, the whole state after any change. Reaches the sessions holding `ManageServer`, and only those: it names webhook URLs, which are credentials. |
| `expression.created` / `expression.updated` / `expression.deleted` | The custom emoji and stickers. Everybody, since everybody renders them. |
| `sound.created` / `sound.updated` / `sound.deleted` | The soundboard. |
| `sound.played` | `{ soundId, userId, channelId }`. Reaches everybody sitting in that voice channel, whether or not they hold a media session. |

When a permission change could add or remove channels from what somebody is
allowed to see, the affected clients receive a fresh `ready` event rather than a
patch. Patching that incrementally would cost more bookkeeping than a rare full
refresh is worth.

## The Discord relay

A relay carries one text channel here to one channel on a Discord server, and
back. It is for the shape a migration actually takes: a community does not move
all at once, and for the weeks in between, a conversation split across two
applications is what decides whether the move sticks.

The two sides are asymmetric in how they are reached and symmetric in what a
reader sees.

- **Discord to here.** A bot account, connected to Discord's gateway with the
  `GUILDS`, `GUILD_MESSAGES` and `MESSAGE_CONTENT` intents. The last of those is
  privileged and off by default; without it Discord delivers every message with
  an empty body, which is the single most common way this is misconfigured. The
  message is written as a webhook message — `userId: null`, `webhook` set — so
  it carries the author's Discord name and picture per message.
- **Here to Discord.** A Discord webhook, whose URL an administrator mints in
  that channel's own integration settings. Each delivery overrides `username`
  and `avatar_url`, so one URL posts as every member of this server.

So a bridged channel reads as the people in it on both ends, rather than as a
bot repeating them.

### Not looping

The obvious failure is a message crossing, coming back as new, and crossing
again. It is prevented by an identity on each side rather than by comparing
content, which would be a guess and would break the moment somebody quoted
themselves.

- A message this server posts goes through a Discord webhook whose id it knows,
  because it parsed it out of the URL that was pasted. When that message arrives
  back over the gateway it carries `webhook_id`, and that field being one of
  ours is proof it is our own echo.
- A message arriving from Discord is written under a `webhooks` row the relay
  owns. When the outbound side finds that row's id in a new message, it is
  looking at something it wrote itself.

Both are exact, and neither can be defeated by what a message says. A link that
is switched off still recognises its own echoes, because a message already in
flight has to be dropped after the bridge stops relaying.

### What crosses

Text, files, embeds, edits and deletions, each way. A reply becomes a one-line
quote of what it answers, since there is nothing to hang a thread off here.
Discord's `<@id>`, `<@&id>`, `<#id>`, `<:name:id>` and `<t:unix>` are resolved to
the names and dates a reader would have seen, because a mention here is the name
it names rather than an id beside one — see the header of `src/lib/mentions.ts`.

Files are carried as bytes rather than as links, both ways. A Discord attachment
URL carries a signature that expires, so linking would fill a bridged channel
with images that stopped loading; and a link the other way would have to be an
address on this server, which plenty of self-hosted ones do not have.

Nothing pings. Outbound content is posted with `allowed_mentions` suppressing
everything, and `@everyone`, `@here` and anything shaped like a Discord mention
are broken with a zero-width character. People on this server are not moderated
by Discord's moderators, and an unfiltered bridge would hand any one of them
`@everyone` on a server they are not in.

Comments on posts do not cross: there is no thread on the Discord side to hang
one off, and no way back.

### Moderation

Automatic moderation applies to what arrives. The rules about content — banned
words, links, mention counts, capitals — are run over every relayed message, and
one that a rule refuses is never written. The rules about pace are not: flood
and repetition measure one connection's own history, and a relayed author has no
connection here, so counting them against a shared queue would let one talkative
person on Discord silence everybody else on it.

A bridge that skipped the rules would be worse than no bridge, because the word
list would look enforced and would not be.

### Configuration

The bot token and the links are set over the protocol, and can also be seeded
from the `relay` block of the configuration file so a container can be deployed
already bridged. A link named in the file is matched by its Discord channel, so
editing the file changes the link it created rather than adding a second one;
the first edit made from the settings screen clears the file's copy, so a
restart cannot resurrect a link somebody deleted.

`relay.public_url` is the address Discord fetches relayed avatars from. It is
guessed from the ACME domain, the dynamic DNS name or the resolved public
address when it is not set, and an unguessable one costs the per-author pictures
on the Discord side and nothing else.

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

Group conversations, files in a private conversation, and blocking somebody
outright: `dmPrivacy` is a door held for a class of people, not a list of names.

Per-user permission overwrites and screen sharing. Video would reuse the whole
of [Voice](#voice) - the signalling is codec-agnostic - and needs a second
track and a way to say which is which.

Voice states are not persisted, for the same reason channel membership is not:
a mute belongs to a session. An identity that reconnects arrives unmuted, and a
moderator's mute has to be applied again.
