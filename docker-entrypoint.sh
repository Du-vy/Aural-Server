#!/bin/sh
# Entrypoint for the Aural server container.
#
# It exists for the one thing an image cannot settle at build time: who owns
# /data. A named volume is seeded from the image, ownership included, so the
# chown done during the build is enough. A bind mount is not seeded at all — it
# arrives with whatever the host says, and Docker creates a missing host
# directory as root:root. A container that had already dropped to an
# unprivileged user then fails on its first write, and what it reports is
# SQLite's "unable to open database file (14)", which names neither the
# directory nor the cause.
#
# So the container starts as root, takes ownership of the data directory, and
# drops to PUID:PGID to run the server. Starting it with an explicit user
# (docker run --user, compose "user:") skips all of that: there is nothing to
# chown from an unprivileged process, so it only checks that the directory is
# usable and says what to run when it is not.
set -e

DATA_DIR="${AURAL_DATA_DIR:-/data}"
PUID="${PUID:-10001}"
PGID="${PGID:-10001}"

probe="$DATA_DIR/.aural-write-test"

# writable reports whether the server will be able to create its database and
# uploads. "$1" is the user to test as, or "-" for the current one.
writable() {
	if [ "$1" = "-" ]; then
		touch "$probe" 2>/dev/null || return 1
	else
		su-exec "$1" touch "$probe" 2>/dev/null || return 1
	fi
	rm -f "$probe" 2>/dev/null || true
	return 0
}

refuse() {
	echo "aural-server: $DATA_DIR is not writable by uid $1." >&2
	echo "aural-server: on the host, run: sudo chown -R $1 <the directory mounted at $DATA_DIR>" >&2
	exit 1
}

if [ "$(id -u)" -ne 0 ]; then
	mkdir -p "$DATA_DIR" 2>/dev/null || true
	writable - || refuse "$(id -u):$(id -g)"
	export HOME="$DATA_DIR"
	exec aural-server "$@"
fi

mkdir -p "$DATA_DIR"

# Recursive only when the top of the tree is wrong. Once ownership is right,
# every later start skips a walk that a large uploads directory makes slow.
if [ "$(stat -c %u "$DATA_DIR")" != "$PUID" ] || [ "$(stat -c %g "$DATA_DIR")" != "$PGID" ]; then
	chown -R "$PUID:$PGID" "$DATA_DIR" 2>/dev/null ||
		echo "aural-server: could not change the ownership of $DATA_DIR" >&2
fi

writable "$PUID:$PGID" || refuse "$PUID:$PGID"

export HOME="$DATA_DIR"
exec su-exec "$PUID:$PGID" aural-server "$@"
