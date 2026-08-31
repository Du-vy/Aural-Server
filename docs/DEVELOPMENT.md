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
- **`go test -race` does not work.** The race detector is ThreadSanitizer, a
  native C++ runtime, so it needs cgo and a **gcc-compatible** compiler. MSVC
  does not satisfy it: `go env CC` says `gcc`.

That last point is a real gap, not a shrug. The fix is to run the race detector
in CI on Linux rather than installing a second C toolchain on a Windows dev
machine — see [CI](#ci) below.

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

```sh
go test -race ./...     # fails on Windows without a gcc toolchain
```

The gateway hub is the concurrent part of this codebase, so this check has real
value. On Windows it needs mingw-w64 (via [w64devkit](https://github.com/skeeto/w64devkit)
or [MSYS2](https://www.msys2.org)) — but preferring CI is the recommendation, so
that it runs on every push rather than when someone remembers.

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
| `voice.mode` | `client_host` or `server_host`. Advertised to the client. |
| `tls.enabled` | Off by default; the client speaks `ws://` then. |

Setting `allow_guests: false` **and** `enabled: false` is rejected at startup:
it would be a server nobody can ever enter. `Validate()` catches it rather than
letting you find out from the far side of a refused connection.

---

## CI

Not set up yet. When it is, it should run on Linux:

```sh
gofmt -l .
go vet ./...
go test -race ./...
```

Linux runners have gcc already, which is what makes `-race` free there and
awkward on Windows.

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
