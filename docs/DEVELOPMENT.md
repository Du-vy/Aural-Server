# Development setup

Everything you need installed to work on the Aural server, why each piece is
needed, and how to tell whether it is working. Written to be followed from a
clean machine without remembering anything.

The short version: **install Go, run the server.** There is deliberately nothing
else. If this page feels long, it is because it explains the reasoning, not
because the setup is complicated.

---

## Go 1.26 or newer

Download: <https://go.dev/dl>

```sh
go version      # go1.26.6 known good
```

That is the entire required toolchain.

### Why there is no C compiler here

SQLite is C, and the usual Go driver (`mattn/go-sqlite3`) embeds it, which drags
in cgo and a per-platform C toolchain. This project uses
[`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) instead — SQLite
transpiled to pure Go — specifically so that none of that is needed.

The consequences are worth knowing, because they are the reason for the choice:

```sh
go env CGO_ENABLED          # 0
CGO_ENABLED=0 go build ./...   # builds clean
```

- `go build` alone produces a static binary. No DLLs, no shared libraries.
- Cross-compiling is just two environment variables (see below).
- **`go test -race` needs a toolchain the rest of the build does not.** The race
  detector is ThreadSanitizer, a native C++ runtime, so it needs cgo and a
  **gcc-compatible** compiler. MSVC does not satisfy it: `go env CC` says `gcc`.

That last point is the one exception to the no-C rule, and it is worth
satisfying locally rather than leaving to CI — see
[the race detector](#the-race-detector) below. Nothing that ships is built with
cgo; it is only ever switched on to instrument a test run.

---

## Run it

```sh
git clone https://github.com/Du-vy/Aural-Server.git
cd Aural-Server
go run ./cmd/aural-server
```

On first run it creates `config.json` and `aural.db` beside itself, then prints
a **one-time owner token**:

```
owner token: xxxxx-xxxxx-xxxxx-xxxxx
```

**Keep it.** Redeeming it in the client is what makes you an administrator. It is
shown once and stored only as a hash, so it cannot be recovered — but it can be
replaced:

```sh
go run ./cmd/aural-server -new-owner-token
```

On Windows there is a `run.bat` that does the same with an explicit config path.

### Flags

| Flag | What it does |
| --- | --- |
| `-config PATH` | Configuration file. Default `config.json`. |
| `-version` | Print the version and exit. |
| `-new-owner-token` | Issue a fresh owner token and exit. |
| `-print-config` | Print the default configuration and exit. |

`-print-config` is how [`config.example.json`](../config.example.json) is kept
honest — it is generated, not hand-written, so it cannot drift from the code:

```sh
go run ./cmd/aural-server -print-config > config.example.json
```

### Runtime files are not committed

`config.json`, `aural.db` and its `-shm` / `-wal` siblings are gitignored. They
are the state of *your* server, not of the project. Deleting all four gives you
a clean first run, which is the fastest way to re-test the bootstrap path.

---

## The checks

```sh
gofmt -l .          # silence means formatted; anything listed needs -w
go vet ./...
go test ./...
```

`go test ./...` covers two packages, and the interesting one is
`internal/gateway`: it stands up a real HTTP server and drives real WebSocket
connections through it, rather than mocking the transport. It covers guest
identity, token resume, registration keeping the same identity, wrong
credentials, server passwords, the one-time owner token, channel permissions,
voice presence and connection displacement.

`internal/permissions` is a plain unit test of the bitmask resolution rules.

### The race detector

The gateway is the concurrent part of this codebase — the hub, the read and
write pumps, the slow reads dispatched off the read loop, the certificate
reloader, and the watchers that follow a changing public address — so this
check has real value.

It needs cgo and a gcc-compatible compiler, which the rest of the build does
not. On Linux and macOS that is already there:

```sh
go test -race ./...
```

On Windows, install [MSYS2](https://www.msys2.org) and its UCRT64 toolchain:

```sh
pacman -S mingw-w64-ucrt-x86_64-gcc
```

Then put it on `PATH` for the run:

```sh
PATH="/c/msys64/ucrt64/bin:$PATH" CGO_ENABLED=1 go test -race ./...
```

```powershell
# PowerShell
$env:PATH = "C:\msys64\ucrt64\bin;$env:PATH"; $env:CGO_ENABLED = "1"
go test -race ./...
```

Leaving the toolchain on the global `PATH` is fine, and does not compromise the
static build. It does flip the default — `go env CGO_ENABLED` reads `1` once gcc
is visible — but nothing in this dependency tree imports `C`, so the binary
comes out byte-for-byte equivalent and with no DLL imports either way.
Cross-compiling is unaffected too, since Go turns cgo off by itself whenever
`GOOS`/`GOARCH` differ from the host. Setting it explicitly per command, as
above, is simply a way of not having to remember any of that.

A clean run only means something for code the tests actually execute
concurrently, which is why `internal/gateway/concurrency_test.go` exists: it
drives the shared state that nothing else exercises in parallel. If you add
concurrent machinery, add a test that runs it from several goroutines at once,
or the detector will have nothing to say about it.

To satisfy yourself the detector is armed rather than merely compiling, write a
throwaway test with an obvious race in it — two goroutines incrementing the
same `int` — and watch it report `WARNING: DATA RACE`.

---

## Cross-compiling

No cgo means this is the whole story:

```sh
GOOS=linux   GOARCH=amd64 go build -o aural-server       ./cmd/aural-server
GOOS=linux   GOARCH=arm64 go build -o aural-server-arm64 ./cmd/aural-server
GOOS=windows GOARCH=amd64 go build -o aural-server.exe   ./cmd/aural-server
GOOS=darwin  GOARCH=arm64 go build -o aural-server-macos ./cmd/aural-server
```

In PowerShell, set them as `$env:GOOS` on separate lines instead. The `arm64`
Linux build is the one for a Raspberry Pi.

---

## Working on the protocol

[`docs/PROTOCOL.md`](PROTOCOL.md) is the canonical specification, and this
repository owns it. The client mirrors it by hand:

```
internal/protocol      ⇄  Aural-Client  src/lib/protocol.ts
internal/permissions   ⇄  Aural-Client  src/lib/permissions.ts
```

Nothing generates one from the other, so **a change here is a change in three
places**: the Go package, the Markdown, and the TypeScript mirror.

The client's `npm run smoke` is what catches it when that is forgotten — it
asserts the client resolves the same permission mask the server sent. Run it
after any protocol change:

```sh
# here
go run ./cmd/aural-server

# in the client repository
npm run smoke -- --address 127.0.0.1:9871 --owner-token PASTE-TOKEN-HERE
```

### Two things that will bite you

**Permission masks travel as decimal strings.** They are `uint64`, and a
JavaScript number loses precision above 2^53. If you add a field carrying a
mask, it is a `string` on the wire and a `bigint` in the client — never a
number.

**A pointer-to-pointer field is not a mistake.** In `ChannelUpdateRequest`:

```go
ParentID **int64 `json:"parentId,omitempty"`
```

Three states have to be distinguishable over JSON: absent means *leave the
parent alone*, `null` means *detach to the tree root*, and a value means *move
under it*. One level of pointer cannot tell the first two apart.

---

## Configuration

[`config.example.json`](../config.example.json) is the annotated reference.
The fields most worth knowing while developing:

| | |
| --- | --- |
| `server.port` | Default `9871`. The client assumes it when you omit the port. |
| `server.password` | Empty disables the server password prompt. |
| `server.allowed_origins` | `["*"]` in development. Tighten for a public server. |
| `registration.enabled` | Whether guests may claim an account. |
| `registration.allow_guests` | Whether unregistered users may connect at all. |
| `voice.enabled` | Turns the audio plane off. Voice channels stay joinable and carry nothing. |
| `voice.mode` | `server_host` or `client_host`. See [Voice](#voice) below. |
| `voice.udp_port_min` / `voice.udp_port_max` | The media ports. Both `0` lets the OS pick. |
| `voice.public_ip` | The address the relay advertises, for a server behind a 1:1 NAT. |
| `tls.enabled` | Off by default; the client speaks `ws://` then. |

Setting `allow_guests: false` **and** `enabled: false` is rejected at startup:
it would be a server nobody can ever enter. `Validate()` catches it rather than
letting you find out from the far side of a refused connection.

---

## Voice

The audio plane is [`internal/voice`](../internal/voice), built on
[pion/webrtc](https://github.com/pion/webrtc). It is pure Go, so it changes
nothing about the toolchain: the binary is still static and still needs no C
compiler.

Nothing here encodes or decodes anything. A relay forwards RTP packets it does
not look inside, which is why a server carrying a room full of people still
needs no codec. What that costs is that the server cannot know who is speaking
or enforce anything about the audio itself; what it buys is that the whole media
path is about four hundred lines.

### Testing it

`go test ./internal/gateway -run TestServerHostedRelay` is the one that matters.
It stands up two real peer connections against the real gateway, runs a real ICE
handshake and checks that packets one client sends come out of the other, in
both directions — the second by way of the renegotiation the relay does when a
second participant arrives. If the media plane is wired up plausibly and does
not work, that is the test that says so.

Everything else about voice is frames, and `voice_test.go` covers it: election
and handover, the mute pairs, the permissions on moderation, and the
reconfiguration path.

There is no test of `client_host` media, and there cannot usefully be one here:
the relaying is done by a browser forwarding somebody else's track, which is not
something this repository contains. The server's part of that mode — electing a
host, relaying signalling between exactly the right two peers, and starting the
room over when the host leaves — is covered.

### Ports

Media does not go over port 9871. With `udp_port_min` and `udp_port_max` at zero
the operating system picks a port per call, which is fine on a development
machine and useless behind a firewall. Set a range when testing through one.

A server whose interface holds a private address advertises that address and no
client outside can reach it. `public_ip` is the fix, and the trade is that
clients on the same LAN then have to hairpin through the router.

The loopback candidate is offered deliberately, which is what makes a client on
the same machine as the server work with no network at all.

---

## CI & Releases
 
GitHub Actions workflows are set up under `.github/workflows`:

- **`ci.yml`**: Runs on pull requests and pushes to `main`. Checks `gofmt`, `go vet`, and runs `go test -race ./...` on Linux, plus a Dockerfile build check.
- **`release.yml`**: Manual release pipeline (`workflow_dispatch`). Validates version from `internal/buildinfo/buildinfo.go`, compiles standalone binaries for all platforms (Linux, Windows, macOS, FreeBSD), computes SHA-256 checksums, and publishes multi-architecture Docker images (`linux/amd64`, `linux/arm64`, `linux/arm/v7`) to GitHub Container Registry (`ghcr.io`).

---

## Editor

VS Code with the official **Go** extension (`golang.go`). On first open it
offers to install `gopls`, `dlv` and friends — accept. Nothing else is needed,
and `.vscode/` is gitignored.

---

## Versions known to work

Recorded from a working machine on 2026-08-31. Not minimums — just what has
actually been verified here.

| | |
| --- | --- |
| Go | 1.26.6 |
| Git | 2.55.0 |
| OS | Windows 10 IoT Enterprise LTSC 2021 |

Direct dependencies, from [`go.mod`](../go.mod):

| | |
| --- | --- |
| `github.com/coder/websocket` | v1.8.15 |
| `golang.org/x/crypto` | v0.55.0 (Argon2id) |
| `modernc.org/sqlite` | v1.57.0 (pure-Go SQLite) |
