package gateway

import (
	"crypto/subtle"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/protocol"
)

const (
	maxNicknameRunes = 32
	minNicknameRunes = 1
	maxChannelName   = 64
	maxTopicRunes    = 512
	maxRoleName      = 48
	maxMessageRunes  = 2000
	// maxMessageNewlines keeps one message from scrolling everybody else's
	// history off the screen.
	maxMessageNewlines = 30
)

// cleanText strips control characters and collapses surrounding whitespace, so
// no display name can smuggle newlines or terminal escapes into another client.
func cleanText(in string) string {
	var b strings.Builder
	b.Grow(len(in))
	for _, r := range in {
		if r == utf8.RuneError || unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// validateNickname normalises and checks a display name.
func validateNickname(raw string) (string, *protocol.Error) {
	name := cleanText(raw)
	n := utf8.RuneCountInString(name)
	if n < minNicknameRunes {
		return "", protocol.Errorf(protocol.ErrBadRequest, "nickname must not be empty")
	}
	if n > maxNicknameRunes {
		return "", protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("nickname must be at most %d characters", maxNicknameRunes))
	}
	return name, nil
}

// validateUsername checks an account name against the server policy. Usernames
// are deliberately narrower than nicknames: they are an identifier people type,
// not a label they display.
func validateUsername(cfg *config.Config, raw string) (string, *protocol.Error) {
	name := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(name)
	if n < cfg.Registration.MinUsernameLength || n > cfg.Registration.MaxUsernameLength {
		return "", protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("username must be between %d and %d characters",
				cfg.Registration.MinUsernameLength, cfg.Registration.MaxUsernameLength))
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return "", protocol.Errorf(protocol.ErrBadRequest,
				"username may only contain letters, digits, dot, underscore and hyphen")
		}
	}
	return name, nil
}

// validatePassword checks a password against the server policy.
func validatePassword(cfg *config.Config, password string) *protocol.Error {
	if utf8.RuneCountInString(password) < cfg.Registration.MinPasswordLength {
		return protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("password must be at least %d characters", cfg.Registration.MinPasswordLength))
	}
	if len(password) > 512 {
		return protocol.Errorf(protocol.ErrBadRequest, "password is too long")
	}
	return nil
}

// checkServerPassword gates the whole server when one is configured.
func checkServerPassword(cfg *config.Config, given string) *protocol.Error {
	want := cfg.Server.Password
	if want == "" {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(want), []byte(given)) != 1 {
		return protocol.Errorf(protocol.ErrServerPassword, "server password is missing or wrong")
	}
	return nil
}

// validateChannelName normalises and checks a channel name.
func validateChannelName(raw string) (string, *protocol.Error) {
	name := cleanText(raw)
	if name == "" {
		return "", protocol.Errorf(protocol.ErrBadRequest, "channel name must not be empty")
	}
	if utf8.RuneCountInString(name) > maxChannelName {
		return "", protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("channel name must be at most %d characters", maxChannelName))
	}
	return name, nil
}

// validateTopic normalises and checks a channel topic.
func validateTopic(raw string) (string, *protocol.Error) {
	topic := cleanText(raw)
	if utf8.RuneCountInString(topic) > maxTopicRunes {
		return "", protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("topic must be at most %d characters", maxTopicRunes))
	}
	return topic, nil
}

// validateRoleName normalises and checks a role name.
func validateRoleName(raw string) (string, *protocol.Error) {
	name := cleanText(raw)
	if name == "" {
		return "", protocol.Errorf(protocol.ErrBadRequest, "role name must not be empty")
	}
	if utf8.RuneCountInString(name) > maxRoleName {
		return "", protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("role name must be at most %d characters", maxRoleName))
	}
	return name, nil
}

// validateColor accepts an empty string or a #rrggbb hex colour.
func validateColor(raw string) (string, *protocol.Error) {
	color := strings.TrimSpace(raw)
	if color == "" {
		return "", nil
	}
	if len(color) != 7 || color[0] != '#' {
		return "", protocol.Errorf(protocol.ErrBadRequest, "color must be empty or in #rrggbb form")
	}
	for _, r := range color[1:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return "", protocol.Errorf(protocol.ErrBadRequest, "color must be empty or in #rrggbb form")
		}
	}
	return strings.ToLower(color), nil
}

// validateChannelType rejects anything outside the three known kinds.
func validateChannelType(kind string) *protocol.Error {
	switch kind {
	case protocol.ChannelCategory, protocol.ChannelText, protocol.ChannelVoice:
		return nil
	default:
		return protocol.Errorf(protocol.ErrBadRequest, "channel type must be category, text or voice")
	}
}

// cleanMessage strips control characters but keeps line breaks, which carry
// meaning inside a message in a way they never do inside a name. A run of
// blank lines is capped so one message cannot scroll everybody else's history
// off the screen.
func cleanMessage(in string) string {
	var b strings.Builder
	b.Grow(len(in))

	newlines := 0
	for _, r := range in {
		switch {
		case r == '\n':
			newlines++
			if newlines <= maxMessageNewlines {
				b.WriteRune(r)
			}
			continue
		case r == '\t':
			b.WriteRune(' ')
		case r == utf8.RuneError, unicode.IsControl(r):
			// Dropped: no other control character means anything here.
			continue
		default:
			b.WriteRune(r)
		}
		newlines = 0
	}
	return strings.Trim(b.String(), " \n")
}

// validateMessageContent normalises and checks the body of a message.
func validateMessageContent(raw string) (string, *protocol.Error) {
	content := cleanMessage(raw)
	if content == "" {
		return "", protocol.Errorf(protocol.ErrBadRequest, "a message cannot be empty")
	}
	if utf8.RuneCountInString(content) > maxMessageRunes {
		return "", protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("a message must be at most %d characters", maxMessageRunes))
	}
	return content, nil
}
