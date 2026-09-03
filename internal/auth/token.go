package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// tokenBytes is the entropy of a session token. 32 bytes is far beyond guessing
// range, which is what lets the tokens be stored under a plain hash.
const tokenBytes = 32

// NewToken mints a session token. The raw value is handed to the client once
// and never stored; only its hash goes in the database.
func NewToken() (raw, hash string, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("auth: read token entropy: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, HashToken(raw), nil
}

// HashToken is the lookup key for a token. Session tokens carry full entropy,
// so an unsalted SHA-256 is enough: there is nothing to brute force, and the
// fast hash keeps a reconnect cheap. Passwords, which do need stretching, go
// through HashPassword instead.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])
}

// NewOwnerToken mints the one-time token printed on first start, which grants
// the admin role to whoever redeems it. It is formatted in dash-separated
// groups because a human usually has to copy it out of a terminal.
func NewOwnerToken() (raw, hash string, err error) {
	buf := make([]byte, 15)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("auth: read owner token entropy: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(buf)
	var groups []string
	for i := 0; i < len(encoded); i += 5 {
		end := min(i+5, len(encoded))
		groups = append(groups, encoded[i:end])
	}
	raw = strings.Join(groups, "-")
	return raw, HashToken(raw), nil
}

// webhookTokenBytes is the entropy of a webhook token. A webhook URL is
// unauthenticated by anything else: whoever holds it may post, so the token
// carries the whole weight and is sized like a session token rather than like
// the owner token a human has to retype.
const webhookTokenBytes = 32

// NewWebhookToken mints the secret half of a webhook URL.
//
// Unlike a session token it is returned to be stored as it is, not hashed:
// whoever manages a channel has to be able to read the URL back out of the
// settings screen every time they wire up an integration, which a hash cannot
// answer. Revoking one is deleting the webhook.
func NewWebhookToken() (string, error) {
	buf := make([]byte, webhookTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: read webhook token entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
