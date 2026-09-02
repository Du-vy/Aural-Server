# syntax=docker/dockerfile:1

# -----------------------------------------------------------------------------
# Build stage
# -----------------------------------------------------------------------------
ARG GO_VERSION=alpine
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS builder

WORKDIR /src

# Install build dependencies
RUN apk --no-cache add ca-certificates tzdata git

# Cache go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source tree
COPY . .

# Target architecture arguments supplied automatically by Docker Buildx
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=dev

# Compile static binary for target platform
RUN set -e; \
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

# CA certificates for TLS (DDNS, Let's Encrypt, Unfurls), tzdata, and wget for healthchecks
RUN apk --no-cache add ca-certificates tzdata wget

# Create non-root user and group
RUN addgroup -g 10001 -S aural && \
    adduser -u 10001 -S aural -G aural -h /data -s /bin/sh

# Install server binary
COPY --from=builder /out/aural-server /usr/local/bin/aural-server

# Working directory and permissions
WORKDIR /data
RUN chown -R aural:aural /data

USER aural:aural

# 9871: HTTP / WebSocket API & Signalling
# 40000-40100/udp: WebRTC media audio plane
EXPOSE 9871
EXPOSE 40000-40100/udp

# Persistent volume for database, config, uploads, and ACME certificates
VOLUME ["/data"]

# Healthcheck testing the unauthenticated /info endpoint
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:9871/info || exit 1

ENTRYPOINT ["aural-server"]
CMD ["-config", "/data/config.json"]
