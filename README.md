# Aural Server

The self-hosted server for [Aural](https://github.com/Du-vy?tab=repositories&q=Aural), an open source
voice and chat platform that pairs a Discord-like client with TeamSpeak-like
servers: you run your own, and people reach it by address.

Written in Go with no cgo, so a plain `go build` produces a single static binary
for any target Go supports.

> **Status: v0.2.** Identity, channels, roles, permissions and text messaging
> are complete and tested. The audio plane is not implemented yet — see
> [Roadmap](#roadmap).

## Quick start

```sh
go build ./cmd/aural-server
./aural-server
```

The first run needs no arguments. It writes `config.json`, creates `aural.db`,
and prints a one-time **owner token**:

```
  ---------------------------------------------------------------
   Owner token: 8Il_1-tbnCy-O1dJI-okAY_

   Redeem it once from a connected client to become this server
   administrator. It is stored hashed and cannot be shown again;
   run with -new-owner-token to issue a replacement.
  ---------------------------------------------------------------
```

Connect a client to `YOUR-IP:9871` and redeem that token to become the
administrator. Check the server is up with:

```sh
curl http://localhost:9871/info
```

### Flags

| Flag | Effect |
| --- | --- |
| `-config PATH` | Configuration file to use. Default `config.json`. |
| `-new-owner-token` | Issue a fresh owner token and exit. |
| `-print-config` | Print the default configuration and exit. |
| `-version` | Print the version and exit. |

## Configuration

`config.json` is created on first run and every key is optional — anything you
leave out falls back to its default. See `config.example.json` for the full file.

```jsonc
{
  "server": {
    "name": "Aural Server",
    "bind": "0.0.0.0",
    "port": 9871,
    "password": "",            // gates the whole server; empty means no gate
    "max_users": 64,
    "allowed_origins": ["*"]   // browser Origin filter on the WebSocket upgrade
  },
  "registration": {
    "enabled": true,           // may guests claim an account?
    "allow_guests": true,      // may unregistered users connect at all?
    "min_password_length": 8
  },
  "voice": { "mode": "client_host" },  // client_host | server_host
  "uploads": {
    "enabled": true,
    "path": "uploads",              // where files are written
    "max_file_bytes": 52428800,     // 50 MiB, per file
    "max_total_bytes": 5368709120,  // 5 GiB across the whole server; 0 = no cap
    "max_per_message": 10,
    "pending_ttl_minutes": 60       // how long an unposted upload is kept
  },
  "tls": { "enabled": false, "cert_file": "", "key_file": "" },
  "database": { "path": "aural.db" },
  "log": { "level": "info", "format": "text" }
}
```

Setting both `registration.enabled` and `registration.allow_guests` to `false` is
rejected at startup, since nobody could ever connect.

`server.name` and `server.description` can also be changed at runtime by an
administrator, and the change is written back to this file.

### File uploads

Both ceilings are yours to set: `max_file_bytes` caps one file and
`max_total_bytes` caps everything the server stores. A client is told both
before it sends anything, so a file that is too large is refused in the picker
rather than after a long upload. Setting `uploads.enabled` to `false` stops new
uploads while still serving the files already posted.

Files are written under `uploads.path` and named by an unguessable key, which is
also the only credential their download URL carries — a browser cannot put an
`Authorization` header on an `<img>` tag. **Anyone given the link can fetch the
file**, whether or not they could see the channel it was posted in. If that is
not the trade you want for your server, turn uploads off.

Deleting a message deletes the files it carried, on disk and in the database.
That is the whole of the moderation story: anyone who may delete the message —
its author, or a holder of `ManageMessages` — takes its files with it.

## How identity works

There is one table of users. **A guest is a user with no username yet.**

When somebody connects for the first time they get an identity and a session
token. The client stores the token and replays it on the next connection, which
is what makes a returning guest the same person rather than a new one.

If the server allows registration, that user can *claim* their identity with a
username and password. Claiming **updates the existing row** — the id, the
nickname and every role attached to them survive. From then on they can sign in
from any device, and losing the client no longer costs them the account.

```
auth.guest  ->  identity + session token
                     |
                     |  auth.register (username + password)
                     v
                same identity, now an account
                     |
                     |  auth.login from any device
                     v
                same identity + a token for that device
```

A guest who loses their token loses that identity for good. That is the honest
trade-off of not asking for credentials up front, and the reason claiming is
worth doing. Passwords are hashed with Argon2id; session tokens carry 256 bits of
entropy and are stored only as a SHA-256 hash.

Server operators control both halves independently: `allow_guests` decides
whether unregistered users may connect, `enabled` decides whether they may claim
an account, and the `Register` permission decides which roles are allowed to.

## Roles and permissions

Discord-shaped: roles carry a permission bitmask, channels carry per-role
overwrites, and roles are ranked by position.

Three roles are built in and cannot be deleted:

| Role | Held by | Default permissions |
| --- | --- | --- |
| `everyone` | Everyone, guests included | View, Connect, Speak, SendMessages, ChangeNickname, Register, AttachFiles |
| `Member` | Anyone who has claimed an account | None — a fresh server treats guests and members alike |
| `Admin` | Whoever redeems the owner token | Administrator |

Overwrites inherit down the tree, so denying `ViewChannel` on a category hides
everything inside it. The role hierarchy is enforced on every write: you can only
act on roles and users ranked strictly below you, and you can only grant
permissions you already hold. `Administrator` deliberately does **not** bypass
the hierarchy, so two administrators cannot remove each other.

The full permission table and resolution rules are in
[docs/PROTOCOL.md](docs/PROTOCOL.md#permissions).

## Layout

```
cmd/aural-server      entry point, flags, startup and shutdown
internal/config       JSON configuration, defaults and validation
internal/store        SQLite schema, migrations and every query
internal/auth         Argon2id passwords and opaque session tokens
internal/permissions  the bitmask and the resolution rules
internal/uploads      attachment storage on disk, quota and content types
internal/protocol     the wire format, shared with the client
internal/gateway      HTTP and WebSocket, the hub, and every op handler
internal/logging      structured logger setup
docs/PROTOCOL.md      canonical protocol specification
```

`internal/protocol` is the contract the client is written against. Changing it
means changing the client, and a breaking change means bumping
`protocol.Version`.

## Development

Go 1.26+ is the only prerequisite. [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md)
covers the full setup, cross-compiling, and the protocol-change checklist.

```sh
go test ./...          # includes an end-to-end test over a real WebSocket
go vet ./...
gofmt -l .
```

The gateway test suite drives a real server through an `httptest` listener and
covers the identity flow, ownership, permissions and presence.

Running `go test -race` is worth doing before any change to the hub, since it is
the only concurrent part of the server. It is also the one thing here that needs
cgo and a gcc-compatible C toolchain — the race detector is ThreadSanitizer, a
native runtime. Nothing else does, which is why `modernc.org/sqlite` was chosen
over the cgo-based driver, and why cross-compiling needs nothing but `GOOS` and
`GOARCH`.

## Roadmap

**v0.1** — identity and registration, channel tree, roles, permissions,
presence, one connection per identity.

**v0.2** — text channels: messages, paged history, editing, deletion,
per-connection rate limiting, and the `ManageMessages` permission. Emoji need
no server support beyond not mangling them, which the test suite pins.

**v0.3** — file attachments: an HTTP upload endpoint, range-served downloads,
configurable per-file and server-wide storage ceilings, the `AttachFiles`
permission, and files that are deleted along with their message.

**v0.4 (here)** — search: `message.search` across every channel a caller may
read, filtered by author, channel, date and the kind of content a message
carries, with the conversation either side of each hit. History gained two more
cursors, `after` and `around`, so a result can be opened where it was written.

**v0.5** — the audio plane. The protocol already advertises which of the two
hosting models a server runs:

- `client_host` — the first user to enter a voice channel relays its audio for
  everyone in it. The host can hand off to another member, and if it disconnects
  the next user in the channel takes over.
- `server_host` — the server relays all audio, the traditional model.

**Later** — bans, per-user permission overwrites, screen sharing, and Aural
Hub, a directory for finding public servers.

## License

[GNU AGPL-3.0-or-later](LICENSE).

Aural is meant to be self-hosted, so the network clause is the point: anyone who
runs a modified Aural server for other people has to offer those people the
modified source. A plain GPL would not require that, because running a server is
not distributing it.

This binds the server software, not the conversations people have on it or the
configuration you run it with.
