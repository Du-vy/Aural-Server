# syntax=docker/dockerfile:1

# -----------------------------------------------------------------------------
# Build stage
# -----------------------------------------------------------------------------
# Pinned to the minor version go.mod asks for: a floating "alpine" tag builds
# fine today and picks up the next major toolchain without anybody deciding to.
ARG GO_VERSION=1.26-alpine
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS builder

WORKDIR /src

# Certificates for the module proxy. Nothing else is needed: the build is pure
# Go, and the source tree arrives through the build context rather than git.
RUN apk --no-cache add ca-certificates

# Cache go modules
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy source tree
COPY . .

# Target architecture arguments supplied automatically by Docker Buildx
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=dev

# Compile static binary for target platform
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    set -e; \
    export GOARM=""; \
    if [ "${TARGETARCH}" = "arm" ]; then \
      export GOARM="${TARGETVARIANT#v}"; \
      [ -z "${GOARM}" ] && export GOARM="7"; \
    fi; \
    echo "Building aural-server for OS=${TARGETOS} ARCH=${TARGETARCH} ARM=${GOARM} VERSION=${VERSION}"; \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags="-s -w -X github.com/aural-chat/aural-server/internal/buildinfo.Version=${VERSION}" \
      -o /out/aural-server ./cmd/aural-server

# -----------------------------------------------------------------------------
# Runtime stage
# -----------------------------------------------------------------------------
FROM alpine:latest

LABEL org.opencontainers.image.title="Aural Server" \
      org.opencontainers.image.description="Self-hosted voice and chat server for Aural" \
      org.opencontainers.image.url="https://github.com/Du-vy/Aural-Server" \
      org.opencontainers.image.source="https://github.com/Du-vy/Aural-Server" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"

# CA certificates for TLS (DDNS, Let's Encrypt, Unfurls), tzdata for TZ, and
# su-exec so the entrypoint can drop privileges. The healthcheck uses busybox's
# own wget, which alpine already carries.
RUN apk --no-cache add ca-certificates tzdata su-exec

# Create non-root user and group. PUID and PGID override the ids the server
# runs as, for a bind mount whose host directory belongs to somebody else.
ENV PUID=10001 \
    PGID=10001
RUN addgroup -g 10001 -S aural && \
    adduser -u 10001 -S aural -G aural -h /data -s /bin/sh

# Install server binary
COPY --from=builder /out/aural-server /usr/local/bin/aural-server

# Install the entrypoint. The sed is for the Windows checkouts this repo is
# written on: git hands them a CRLF copy, and a CR at the end of the shebang
# makes the image fail to start with "no such file or directory".
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN sed -i 's/\r$//' /usr/local/bin/docker-entrypoint.sh && \
    chmod 0755 /usr/local/bin/docker-entrypoint.sh

# Working directory and permissions. This settles the ownership of a named or
# anonymous volume, which Docker seeds from the image; a bind mount ignores it
# and is handled by the entrypoint at startup instead.
WORKDIR /data
RUN chown -R aural:aural /data

# Deliberately still root: the entrypoint takes ownership of /data and then
# drops to PUID:PGID, so the server itself never runs privileged. Under a
# runAsNonRoot policy, start the container with an explicit user
# (--user 10001:10001) and pre-create the data directory with that owner.

# 9871: HTTP / WebSocket API & Signalling
# 40000-40100/udp: WebRTC media audio plane
EXPOSE 9871
EXPOSE 40000-40100/udp

# Persistent volume for database, config, uploads, and ACME certificates
VOLUME ["/data"]

# Healthcheck testing the unauthenticated /info endpoint. It tries plain HTTP
# first and TLS second, because which one answers depends on the config file;
# AURAL_HEALTHCHECK_PORT follows server.port when that is changed from 9871.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:${AURAL_HEALTHCHECK_PORT:-9871}/info" || \
      wget -q -O /dev/null --no-check-certificate "https://127.0.0.1:${AURAL_HEALTHCHECK_PORT:-9871}/info" || \
      exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["-config", "/data/config.json"]
