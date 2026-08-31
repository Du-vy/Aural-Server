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

The default port is **9871**. Two plain HTTP endpoints sit alongside the socket:

| Endpoint  | Purpose                                                              |
| --------- | -------------------------------------------------------------------- |
| `GET /info`   | Unauthenticated server preview. A client shows it before connecting, and the future Aural Hub reads it to list servers. Sends `Access-Control-Allow-Origin: *`. |
| `GET /health` | Liveness probe for process supervisors. Returns `ok`.            |

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
  "users":      [User],    // everyone currently connected
  "channels":   [Channel], // only the channels the caller may see
  "roles":      [Role],    // the whole role table
  "permissions": "1234",   // the caller's resolved server-wide mask
  "server":      ServerInfo
}
```

`channels` is filtered to what the caller may see, and the `channelId` of a user
sitting somewhere the caller cannot see is reported as `null`. A restricted
channel never leaks through the presence list.

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
  "editedAt": null
}

// ServerInfo
{
  "name": "Aural Server",
  "description": "",
  "protocolVersion": 1,
  "softwareVersion": "0.1.0",
  "maxUsers": 64,
  "onlineUsers": 3,
  "passwordProtected": false,
  "registrationEnabled": true,
  "guestsAllowed": true,
  "voiceMode": "client_host"  // client_host | server_host
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
| `server.update` | `ManageServer` | `{ name?, description? }`. Persisted to the configuration file. |
| `user.update` | `ChangeNickname`, or `ManageNicknames` for others | `{ userId?, nickname }`. |
| `user.move` | `Connect` on the destination; `MoveUsers` for others | `{ userId?, channelId }`. `channelId: null` leaves. |
| `user.kick` | `KickUsers` | `{ userId, reason? }`. Disconnects; there are no bans in v0.1. |
| `channel.create` | `ManageChannels` on the parent | `{ name, type, parentId?, topic?, position?, userLimit? }`. |
| `channel.update` | `ManageChannels` on the channel; `ManageRoles` to touch `overwrites` | `{ channelId, name?, topic?, parentId?, position?, userLimit?, overwrites? }`. |
| `channel.delete` | `ManageChannels` on the channel | `{ channelId }`. Cascades to descendants. |
| `message.send` | `SendMessages` on the channel | `{ channelId, content }`. Text channels only. Rate limited. |
| `message.history` | `ViewChannel` on the channel | `{ channelId, before?, limit? }`. Pages backwards; `limit` defaults to 50, capped at 100. |
| `message.edit` | Author only | `{ messageId, content }`. |
| `message.delete` | Author, or `ManageMessages` on the channel | `{ messageId }`. |
| `role.create` | `ManageRoles` | `{ name, color?, permissions?, hoist? }`. Lands below your rank. |
| `role.update` | `ManageRoles` | `{ roleId, name?, color?, permissions?, position?, hoist? }`. |
| `role.delete` | `ManageRoles` | `{ roleId }`. Managed roles cannot be deleted. |
| `role.assign` / `role.unassign` | `ManageRoles` | `{ userId, roleId }`. The target may be offline. |

Three notes on messages:

- **Only the author may edit.** No permission overrides this, `Administrator`
  included: putting words in somebody's mouth is not moderation. A moderator who
  objects to a message deletes it, which is visible, rather than rewriting it,
  which is not.
- **`message.history` is ordered oldest first** and returns `hasMore`, which
  reports whether anything older than the first entry remains. Paging uses
  `before`, an exclusive message id, so a page stays stable while new messages
  arrive at the other end.
- **Posting is rate limited** per connection, as a token bucket. Exceeding it
  returns `rate_limited`; the message is not stored.
- **Content is sanitised, not transformed.** Control characters are dropped and
  tabs become spaces, but line breaks are kept, and so is everything that holds
  an emoji sequence together — zero width joiners, variation selectors, skin
  tone modifiers and regional indicators. `content` comes back byte for byte.

Two notes on `user.move`:

- Only **voice** channels can be joined. Text channels carry no presence and are
  selected client side.
- `parentId` in `channel.update` is three-valued: absent leaves the parent alone,
  `null` detaches the channel to the root, and a number reparents it.

## Events

| Event | Payload | Sent to |
| --- | --- | --- |
| `hello` | `{ server, heartbeatMs }` | The connecting client, before auth. |
| `ready` | Full snapshot | A client whose visible state went stale. |
| `user.connected` | `{ user }` | Everyone but the arriving client. |
| `user.disconnected` | `{ userId }` | Everyone. Not sent when a new connection displaced the old one. |
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
- **Messages outlive presence.** `ready.users` lists only who is connected, so
  the author of an older message is usually somebody the client has never seen.
  That is why every message carries `author` as well as `userId`.

## Not in version 1

The media plane. `server.voiceMode` already advertises which of the two hosting
models a server uses — `client_host`, where the first user in a channel relays
its audio, or `server_host`, where the server does — but no signalling op exists
yet. Adding it will bump the protocol version.
