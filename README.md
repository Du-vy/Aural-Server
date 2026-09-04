# Aural Server

The self-hosted server for [Aural](https://github.com/Du-vy?tab=repositories&q=Aural), an open source
voice and chat platform that pairs a Discord-like client with TeamSpeak-like
servers: you run your own, and people reach it by address.

Written in Go with no cgo, so a plain `go build` produces a single static binary
for any target Go supports.

> **Status: v0.8.** Identity, channels, roles, permissions, text messaging,
> private conversations, attachments, search, the audio plane and
> Discord-compatible webhooks are all implemented and tested.

## Quick start

### 1. Traditional standalone binary

You can download ready-to-run precompiled binaries for Linux (x86_64, ARM64, ARMv7), Windows (x86_64, ARM64), macOS (Apple Silicon and Intel), and FreeBSD directly from the [GitHub Releases](https://github.com/Du-vy/Aural-Server/releases) page.

Or compile from source:

```sh
go build ./cmd/aural-server
./aural-server
```

The first run needs no arguments. It writes `config.json`, creates `aural.db`,
and prints a one-time **owner token**:

```
  ---------------------------------------------------------------
   Owner token: 8Il_1-tbnCy-O1dJI-okAY_

   Redeem it once from a connected client to claim ownership.
   It is stored hashed and cannot be shown again;
   run with -new-owner-token to issue a replacement.
  ---------------------------------------------------------------
```

Connect a client to `YOUR-IP:9871` and redeem that token to become the owner of
the server. Check the server is up with:

```sh
curl http://localhost:9871/info
```

### 2. Docker & Docker Compose

A multi-architecture image (`linux/amd64`, `linux/arm64`, `linux/arm/v7`) is published to GitHub Container Registry (`ghcr.io/du-vy/aural-server:latest`).

Run with Docker Compose:

```sh
# Fetch docker-compose.yml and start
curl -O https://raw.githubusercontent.com/Du-vy/Aural-Server/main/docker-compose.yml
docker compose up -d
docker compose logs -f
```

Or run with plain Docker:

```sh
docker run -d \
  --name aural-server \
  --restart unless-stopped \
  -p 9871:9871 \
  -p 40000-40100:40000-40100/udp \
  -v $(pwd)/data:/data \
  ghcr.io/du-vy/aural-server:latest
```

All persistent data (`config.json`, `aural.db`, `uploads/`, `acme/`) is stored inside the `/data` volume. To issue a new owner token via Docker:

```sh
docker compose exec aural docker-entrypoint.sh -new-owner-token -config /data/config.json
```

The server runs as an unprivileged user, and the container takes ownership of
`/data` on startup so that a bind-mounted host directory works without any
preparation. Set `PUID` and `PGID` to run as some other uid/gid — a host user
that already owns the directory, say. Starting the container with an explicit
`--user` skips that step entirely, in which case the directory has to be owned
by that user beforehand.

Voice media uses ephemeral UDP ports until `voice.udp_port_min` and
`voice.udp_port_max` are set, so a published port range only carries traffic
once the same range is pinned in `config.json`.

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
    "allowed_origins": ["*"],  // browser Origin filter on the WebSocket upgrade
    "allow_direct_messages": true, // private conversations between two members
    "trusted_proxies": []      // whose X-Forwarded-For to believe; empty = nobody
  },
  "registration": {
    "enabled": true,           // may guests claim an account?
    "allow_guests": true,      // may unregistered users connect at all?
    "min_password_length": 8
  },
  "voice": {
    "enabled": true,
    "mode": "server_host",     // server_host | client_host
    "sample_rate": 48000,      // 8000 | 12000 | 16000 | 24000 | 48000
    "bitrate": 64000,          // bits per second, where a client starts
    "min_bitrate": 16000,      // and the range it may move within
    "max_bitrate": 128000,
    "fec": true,               // recover a lost packet from the next one
    "dtx": false,              // stop sending during silence
    "stereo": false,
    "max_participants": 0,     // 0 leaves the ceiling to the channel
    "public_ip": "",           // an IP, or a hostname to re-resolve; see below
    "udp_port_min": 0,         // 0 lets the OS choose the media ports
    "udp_port_max": 0,
    "ice_servers": []          // STUN and TURN, needed by client_host
  },
  "uploads": {
    "enabled": true,
    "path": "uploads",              // where files are written
    "max_file_bytes": 52428800,     // 50 MiB, per file
    "max_total_bytes": 5368709120,  // 5 GiB across the whole server; 0 = no cap
    "max_per_message": 10,
    "pending_ttl_minutes": 60       // how long an unposted upload is kept
  },
  "expressions": {
    "max_emojis": 50,          // custom emoji this server carries
    "max_stickers": 50,
    "max_sounds": 50,          // soundboard clips
    "max_sound_seconds": 10,   // how long one clip may run
    "max_emoji_bytes": 524288,
    "max_sticker_bytes": 1048576,
    "max_sound_bytes": 4194304
  },
  "relay": {
    "enabled": false,          // bridge channels to a Discord server
    "bot_token": "",           // from the Bot page of a Discord application
    "public_url": "",          // where Discord fetches relayed avatars from
    "max_attachment_bytes": 8388608,
    "links": []                // seed pairs here; the settings screen owns them after
  },
  "ddns": {
    "enabled": false,          // keep a dynamic DNS record pointing here
    "provider": "",            // duckdns | cloudflare
    "domain": "",
    "token": "",
    "interval_minutes": 5
  },
  "retention": {
    "token_idle_days": 90,     // revoke a session token nobody has used since
    "guest_idle_days": 30      // drop guest identities that cannot return; 0 = keep
  },
  "tls": {
    "enabled": false,
    "cert_file": "",           // filled in for you when acme is on
    "key_file": "",
    "acme": {
      "enabled": false,        // get a certificate over the DNS-01 challenge
      "domains": [],           // defaults to ddns.domain
      "email": "",
      "staging": false         // work a deployment out against this first
    }
  },
  "database": { "path": "aural.db" },
  "log": { "level": "info", "format": "text" }
}
```

Setting both `registration.enabled` and `registration.allow_guests` to `false` is
rejected at startup, since nobody could ever connect.

`server.name` and `server.description` can also be changed at runtime by an
administrator, and the change is written back to this file.

### Private conversations

Two members can write to each other outside any channel. A conversation is a
pair of identities and nothing else, addressed by the other person's id, and it
carries no files: an upload is bound to the channel it was made for.

Each member decides who may reach them, with `dmPrivacy` — `everyone`,
`registered` (only members who have claimed an account) or `none`. The setting
is read from **both** ends of a send, so turning it off stops your own writing
as well as everybody else's: otherwise somebody who wants no private messages
could still open a thread nobody may answer. It is never shown to anybody but
its owner.

Operators have two switches of their own: the `SendDirectMessages` permission
gates the feature by role, and `server.allow_direct_messages: false` turns it
off for the whole server. Conversations already written are kept either way, so
switching it back on loses nothing.

### Voice

Audio is Opus over WebRTC. The WebSocket carries only signalling; the media
travels as RTP and nothing on this server decodes it, which is why relaying a
call still needs no codec and no cgo.

`voice.mode` chooses who relays:

- **`server_host`** (the default) — the server relays. It works with no further
  setup, because the server already holds an address every client can reach,
  which is the one thing NAT traversal is otherwise short of. It costs upstream
  bandwidth for every listener in every call.
- **`client_host`** — the first person in a channel relays for the rest, and the
  next one takes over when they leave. It costs the server nothing and needs at
  least a STUN server in `ice_servers`, usually a TURN server too, because both
  ends are behind somebody's router.

Both modes are switchable at runtime by an administrator holding `ManageServer`,
which rewrites this file and asks everybody in a call to reconnect.

**Ports.** Media does not go through port 9871. With `udp_port_min` and
`udp_port_max` at zero the operating system picks a port per call, which is fine
on a host with nothing in front of it and useless behind a firewall that has to
be told what to open. Set a range and open it:

```jsonc
"udp_port_min": 40000,
"udp_port_max": 40100
```

**Behind NAT.** A server whose own interface holds a private address advertises
that address in its ICE candidates, and no client outside could ever reach it.
Set `public_ip` to the address clients actually reach the server on. The
trade-off is that clients on the same LAN then have to hairpin through the
router to reach it, which not every router does.

`public_ip` takes any of three things, and on a home connection the literal is
the one you do **not** want:

| Value | What happens |
| --- | --- |
| `"203.0.113.5"` | Advertised as written. Correct until your provider changes it. |
| `"aural.duckdns.org"` | Re-resolved every five minutes. Follows a dynamic DNS record. |
| `""` with a `stun:` entry in `ice_servers` | Discovered by asking a STUN server. No setting to maintain. |

When the answer changes, the relay is rebuilt and everybody in a call
renegotiates — about a second of silence, once, instead of voice that stays
broken until somebody restarts the server. Nothing is watched at all when the
value is a literal, since it cannot change.

**Bitrate.** `min_bitrate` and `max_bitrate` bound what a member may choose and
`bitrate` is where they start. The defaults — 16 to 128 kb/s, starting at 64 —
put transparent speech in the middle and leave room either side.

`sample_rate` is the highest rate Opus is asked to encode at. Its permitted
values are the rates Opus actually encodes at: 8000, 12000, 16000, 24000 and
48000. **44100 is not one of them**, and is rejected rather than accepted and
quietly rounded: Opus always runs on a 48 kHz clock and resamples internally, so
naming 44100 would ask for something the codec would not do.

### Home servers, dynamic addresses and TLS

This is the deployment Aural is mostly aimed at: one machine, on a domestic
connection, whose address is not the operator's to keep. Three things follow
from that, and the server handles each of them.

**A name that follows the address.** Set the `ddns` block and the server keeps
the record current itself, so there is no `ddclient` to install and no router
firmware to trust:

```jsonc
"ddns": {
  "enabled": true,
  "provider": "duckdns",        // or "cloudflare"
  "domain": "aural.duckdns.org",
  "token": "…",
  "interval_minutes": 5
}
```

It finds the address by asking a STUN server, because the machine's own
interface holds a private one. On Cloudflare, use a scoped API token with
**DNS:Edit** on the zone — never a global key. Add `"zone_id"` if the token is
too narrow to list zones; otherwise the zone is found from the name. The record
is created if it does not exist yet, and written only when the address has
actually moved.

If something else already updates your record, leave this off and just name it
in `voice.public_ip`.

**A certificate that renews itself.** Turn on `tls.acme` and the server obtains
one from Let's Encrypt over the DNS-01 challenge, using the same DNS
credentials:

```jsonc
"tls": {
  "enabled": true,
  "acme": { "enabled": true, "email": "you@example.com" }
}
```

DNS-01 rather than HTTP-01 deliberately: HTTP-01 needs port 80 reachable from
the internet, which residential providers frequently block and the operator
cannot do anything about. DNS-01 needs nothing inbound at all.

The `ddns` block must carry `provider`, `domain` and `token` for this, whether
or not `ddns.enabled` is on — turning publishing off does not mean forgetting
how to reach your DNS. Work a new deployment out with `"staging": true` first:
its certificates are trusted by nothing, and its rate limits will forgive the
mistakes the real ones will not.

The certificate is renewed thirty days before it expires and re-read off disk
without a restart — which is also true of a certificate you obtained yourself
and dropped into `cert_file` and `key_file`.

**A word about Cloudflare.** If you put the orange cloud in front of this
server, voice will not work through it, and no configuration fixes that:

- The proxy carries **no UDP at all**, so WebRTC media cannot pass through it.
- It only proxies WebSocket traffic on the ports it terminates — 443, 2053,
  2083, 2087, 2096, 8443 — so the default 9871 does not reach you either.

A proxied deployment therefore needs a second, **grey-clouded** record pointing
straight at your address for the audio plane, with `voice.public_ip` set to it,
or a TURN server in `ice_servers` that the media can go through instead. Keep
`"proxied": false` (the default) on any record voice arrives on.

**Behind a reverse proxy.** Caddy, nginx or a tunnel in front of the server
means every request arrives from it, and the client's address survives only in
`X-Forwarded-For` — a header written by whoever spoke to the proxy, and worth
nothing on its own. List the proxy in `server.trusted_proxies` and it is
believed, one hop at a time, as far back as the hops you named:

```jsonc
"trusted_proxies": ["127.0.0.1", "172.18.0.0/16"]
```

Empty, the default, ignores the header entirely, which is right for a server
reached directly.

**Keeping the database from growing forever.** A server that runs for years
mints a fresh identity for every guest who ever visits, and a token that never
expires. The `retention` block sweeps both: tokens nobody has presented in
`token_idle_days`, and then guest identities last seen `guest_idle_days` ago
that hold no token — which is to say, ones that could never come back as
themselves anyway. Set either to `0` to keep them forever.

Nothing anybody said is affected. A message records its author's name on itself
and the link back to the account is nullable, so history reads exactly as it did
before. Registered accounts are never touched.

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

## Moderation

### Kicks and bans

A kick ends a connection. A ban is a standing decision, and the difference
matters on a server that hands out guest identities to anybody who asks: an
account is one click away from being replaced, so banning one buys nothing on
its own.

A ban therefore carries up to three **handles**, and is matched on any of them:

| Handle | Strength |
| --- | --- |
| The account | Exact, and free to replace |
| The address it connected from | Changes on a reboot; shared by a household, a university, one phone network |
| The machine behind it | Survives a new account and a cleared browser profile |

The machine handle is the interesting one, and it is deliberately not hardware
fingerprinting. There are no serial numbers here, no volume identifiers, no
registry reads, no spawned processes and no canvas readback — partly because
that is the road to being flagged by an antivirus scanner, and mostly because it
does not work any better: somebody who reinstalls their system is through either
way. What the client sends is a hash over a random identifier it wrote once into
the steadiest directory the platform offers, plus what the operating system
hands any process for free.

That hash is salted with a value **this server** minted and keeps, so the same
machine presents a different identifier to every server it connects to: the
value means something here, which is what makes a ban survive a new account and
a cleared browser profile, and means nothing anywhere else, so it cannot be used
to follow somebody around. The server never learns the material behind it.

None of it is a wall. What it raises is the cost of the ordinary case: closing
the client, opening it again, and coming straight back as a new guest.

Two rules keep the feature from turning on the people using it:

- **The owner is never refused**, however a ban was issued. A server whose owner
  cannot get in has no way back.
- **A handle the moderator issuing the ban, or the owner, is sitting behind is
  not attached to it.** An address is shared, and banning a troublemaker from
  your own network must not ban you.

Bans need the new `BanUsers` permission and rank over the target. The list shows
what each ban catches, counted rather than named: a moderator deciding whether
to lift one needs to know that it reaches a machine, not which machine.

### The audit log

Every moderation action writes a line: who did it, what they did, what it was
done to, and which fields changed — all captured as they read at the time, so a
role deleted last week still has its name in the log.

Nothing can write to it but the actions themselves. It is behind its own
`ViewAuditLog` permission, which grants nothing else: reading the log is how a
server holds its own staff to account, so it is deliberately reachable without
any power to act.

A blocked message is not copied into it. It was never accepted by this server,
and writing it into a table more people can read than could have read the
message would make the blocking the thing that published it.

### AutoMod

Six rules, applied to every message as it arrives, off until an administrator
switches them on:

| Rule | What it catches |
| --- | --- |
| Blocked words | Listed terms, matched however they were capitalised or accented |
| Links | URLs, including the ones written without a scheme, with an allow list |
| Mention spam | More than N people addressed at once |
| Shouting | A message that is mostly capitals |
| Flooding | More than N messages within M seconds |
| Repetition | The same message N times in a row |

Each either refuses the message or masks the offending part, and each carries
its own list of exempt roles on top of a server-wide one — because those are
genuinely different exemptions: staff are usually exempt from everything, while
one rule is very often lifted for a single role that nothing else applies to.
Holding `Administrator` is exemption in itself. Channels can be exempted too.

Rules run on the way in, so a blocked message never existed and a censored one
was never stored uncensored. Edits and post titles are screened as well, since a
rule that only looked at sends would be worked around by posting a full stop and
editing it.

## Expressions and the soundboard

Custom **emoji** are written into a message as `:name:` and rendered inline.
Custom **stickers** are sent instead of a message. Both are uploaded over the
same HTTP endpoints attachments use, under the new `ManageExpressions`
permission, and both are capped per server.

Nothing is rewritten on the way in: a message written with `:shrug:` stores
`:shrug:`, and a client resolves it when it renders. So history survives an
emoji being renamed or deleted, and reading a server that carries none shows the
colons somebody actually typed.

The **soundboard** is short clips anybody in a voice channel may play at the
room, under `UseSoundboard`. Fifty clips of up to ten seconds by default, both
configurable.

Two decisions worth naming:

- **A clip is uploaded as WAV**, whatever the file it was cut from. That is what
  makes the length limit enforceable rather than merely declared: a RIFF header
  states its own duration, so the server reads how long a clip runs instead of
  believing a number the uploader chose. The client decodes whatever was picked,
  trims the range somebody chose in the picker, and re-encodes.
- **Each client plays the clip itself.** Nothing is injected into anybody's
  microphone: the server says which clip was played, and every client in the
  channel fetches it and mixes it into its own output. So it sounds the same to
  everybody, works identically whether the call is relayed by the server or by
  another participant, and being deafened silences it exactly as it silences
  everything else.

## Webhooks

A webhook is a URL that posts into one text channel with no account behind it —
what a build server, a monitor or an automation is given so that what it has to
say arrives in a channel without anybody writing a client.

**It speaks Discord's webhook API exactly.** The path, the payload, the status
codes, the error bodies and the rate-limit headers are the ones an application
posting to a Discord webhook is already written against, so anything you have
pointed at one works here by changing the URL and nothing else:

```sh
curl -X POST http://YOUR-SERVER:9871/api/webhooks/3/TOKEN \
  -H 'Content-Type: application/json' \
  -d '{"content":"deploy finished","username":"Buildbot",
       "embeds":[{"title":"v1.4.0","description":"12 commits","color":3061344}]}'
```

That includes the two dialects Discord also accepts on a webhook URL: GitHub's
own event schema at `/github`, rendered into a card per event, and Slack's
message format at `/slack`. A repository or an alerting tool that only knows how
to talk to one of those needs no adapter either.

The URL is minted from **Server Settings → Integrations**, by anyone holding the
new `ManageWebhooks` permission in the channel. The token in it is the whole of
the authentication: **anyone given the URL can post to that channel**, which is
the same bargain Discord makes, and deleting the webhook is how it is revoked.
Messages already posted through one stay in the channel, attributed to the name
and picture they were posted under.

The full surface — every route, every field, the limits and the error codes — is
in [docs/PROTOCOL.md](docs/PROTOCOL.md#webhooks).

## The Discord relay

Communities do not move all at once. Some people leave Discord the day a server
opens here and some never do, and in between there is a stretch — weeks, usually
— where a conversation split across two applications is the thing that decides
whether the move sticks.

So a channel here can be bridged to a channel on a Discord server, both ways.
The people who moved talk here, the people who have not talk there, and neither
group is talking into a room with nobody in it.

**Messages wear the name and picture of whoever wrote them.** A bridged channel
reads as the people in it, not as forty messages in a row posted by "Relay Bot".
Text, files, embeds, edits, deletions and replies all cross; a Discord reply to
a message this server already holds arrives as a reply, and anything else
becomes a short quote of what it answers; Discord's `<@id>` mentions and
`<t:…>` timestamps are resolved into the names and dates a reader would have
seen.

**Nothing loops.** A message that crosses is tagged by identity on each side —
the Discord webhook it went out through, and the internal webhook row it came in
under — so its own echo is recognised exactly rather than guessed at from what
it says.

**Your rules still apply.** AutoMod screens what arrives from Discord, so a word
list is not something to be walked around by typing it on the other side.
Nothing relayed can ping: `@everyone` and anything shaped like a Discord mention
is defanged on the way out, because people here are not moderated by Discord's
moderators.

### Setting one up

From **Server Settings → Discord Relay**, which walks through it, or from the
`relay` block of `config.json` for a container that should come up already
bridged. In short:

1. Create an application in the [Discord Developer
   Portal](https://discord.com/developers/applications) and open its **Bot**
   page. Reset the token and copy it.
2. On the same page, switch on the **Message Content Intent**. Without it
   Discord delivers every message with an empty body — this is the one step
   that produces a bridge which connects happily and carries nothing.
3. Under **OAuth2 → URL Generator**, tick `bot` and `Read Messages/View
   Channels`, and open the URL to add the bot to your Discord server.
4. In each Discord channel you want bridged: **Edit Channel → Integrations →
   Webhooks → New Webhook**, and copy its URL.
5. Paste the token and each webhook URL into the settings screen.

The bot needs no permissions beyond reading the channels being bridged, and the
webhook is what it writes through — Aural never needs anybody's Discord
password, and the token can be revoked from the Developer Portal at any time.

Each link can run both ways, or only one, and can carry or drop files and edits
independently. Managing them needs `ManageServer`: a webhook URL is a standing
permission to post into somebody else's channel.

It costs no new dependency. The Discord gateway is a WebSocket carrying JSON and
the webhook side is plain HTTP, so `internal/discord` speaks both directly over
the WebSocket library the server already uses.

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
| `everyone` | Everyone, guests included | View, Connect, Speak, SendMessages, ChangeNickname, Register, AttachFiles, SendDirectMessages, CreatePosts, UseSoundboard |
| `Member` | Anyone who has claimed an account | None — a fresh server treats guests and members alike |
| `Admin` | Whoever the owner grants it to | Administrator |

Overwrites inherit down the tree, so denying `ViewChannel` on a category hides
everything inside it. The role hierarchy is enforced on every write: you can only
act on roles and users ranked strictly below you, and you can only grant
permissions you already hold. `Administrator` deliberately does **not** bypass
the hierarchy, so two administrators cannot remove each other.

Above all of it stands the **owner**, the identity that redeemed the owner
token. As in Discord, ownership is not a role: it grants every permission and a
rank above every role, so the owner alone can edit the `Admin` role and the
administrators holding it cannot — not their own role, and not the owner.

The full permission table and resolution rules are in
[docs/PROTOCOL.md](docs/PROTOCOL.md#permissions).

## Layout

```
cmd/aural-server      entry point, flags, startup and shutdown
internal/config       JSON configuration, defaults and validation
internal/store        SQLite schema, migrations and every query
internal/auth         Argon2id passwords and opaque session tokens
internal/permissions  the bitmask and the resolution rules
internal/uploads      attachment storage on disk, quota, content types, WAV length
internal/voice        the Opus parameters, and the WebRTC relay
internal/discord      the Discord gateway and webhook API, for the relay
internal/publicip     the address the relay advertises: literal, DNS or STUN
internal/ddns         DuckDNS and Cloudflare: address records and DNS-01
internal/acme         obtaining and renewing a certificate over DNS-01
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

**v0.4** — search: `message.search` across every channel a caller may
read, filtered by author, channel, date and the kind of content a message
carries, with the conversation either side of each hit. History gained two more
cursors, `after` and `around`, so a result can be opened where it was written.

**Unreleased** — private conversations: a thread per pair of identities,
paged like a channel, with a per-member privacy setting read from both ends of
every send, a read marker the server keeps so an unread badge survives a
restart, the `SendDirectMessages` permission, and a server-wide switch.

**v0.5 (here)** — the audio plane: Opus over WebRTC, both hosting models,
per-channel rooms, host election and handover, self and moderated mute and
deafen, speaking indicators, and a bitrate range an administrator sets and a
member chooses within. Signalling is six ops on the existing socket; the media
is RTP the server forwards without looking inside.

Also everything a server on a domestic connection needs to survive one: an
advertised address that follows a dynamic DNS record or is discovered by STUN,
built-in DuckDNS and Cloudflare updating, a certificate obtained and renewed
over the DNS-01 challenge, reverse-proxy awareness, and retention windows for
the rows a long-running server would otherwise keep forever.

**Unreleased** — webhooks: a URL per channel that an outside service posts
into, speaking Discord's webhook API field for field so that anything already
pointed at one works by changing nothing but the address. Rich embeds, file
deliveries, the GitHub and Slack payload dialects, per-webhook rate limits, and
the `ManageWebhooks` permission.

**Unreleased** — moderation that outlives a connection: bans matched on the
account, the address and a salted per-server device identifier, an audit log
of what moderators did, and AutoMod. Plus the things a server carries for its
own people: custom emoji, stickers, and a soundboard.

**Unreleased** — the Discord relay: channels bridged both ways to a Discord
server, with each message wearing the name and picture of whoever wrote it,
files and edits carried across, an exact loop guard on each side, and AutoMod
applied to what arrives. Built directly on the WebSocket library the server
already uses, so it costs no new dependency.

**Later** — bots and a bot API, per-user permission overwrites, screen
sharing, and Aural Hub, a directory for finding public servers.

## License

[GNU AGPL-3.0-or-later](LICENSE).

Aural is meant to be self-hosted, so the network clause is the point: anyone who
runs a modified Aural server for other people has to offer those people the
modified source. A plain GPL would not require that, because running a server is
not distributing it.

This binds the server software, not the conversations people have on it or the
configuration you run it with.
