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
	maxPostTitle     = 128
	maxPostLocation  = 128
	// maxTimestamp bounds a date somebody may send: a little past the year
	// 2100, which is far enough out for a calendar and near enough that a
	// number arriving in milliseconds by mistake is refused rather than
	// scheduled four thousand years from now.
	maxTimestamp = 4_200_000_000
	// maxEventLength caps how long one event may run.
	maxEventLength  = 366 * 24 * 3600
	maxMessageRunes = 2000
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

// validateChannelType rejects anything outside the known kinds.
func validateChannelType(kind string) *protocol.Error {
	switch kind {
	case protocol.ChannelCategory, protocol.ChannelText, protocol.ChannelVoice:
		return nil
	default:
		if protocol.PostChannel(kind) {
			return nil
		}
		return protocol.Errorf(protocol.ErrBadRequest,
			"channel type must be category, text, voice, announcement, forum, media or calendar")
	}
}

// validatePostTitle normalises and checks the title of a post. Unlike a
// message, a post always has one: it is what a listing shows before anybody
// opens the thread.
func validatePostTitle(raw string) (string, *protocol.Error) {
	title := cleanText(raw)
	if title == "" {
		return "", protocol.Errorf(protocol.ErrBadRequest, "a post needs a title")
	}
	if utf8.RuneCountInString(title) > maxPostTitle {
		return "", protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("a title must be at most %d characters", maxPostTitle))
	}
	return title, nil
}

// validatePostEvent checks the when and where of a calendar post.
//
// It is required in a calendar channel and refused everywhere else, so the
// channel type is the one thing that decides whether a post is an event: a
// forum topic with a start time would be an event nothing renders.
func validatePostEvent(channelType string, in *protocol.PostEventDetails) (*protocol.PostEventDetails, *protocol.Error) {
	if channelType != protocol.ChannelCalendar {
		if in != nil {
			return nil, protocol.Errorf(protocol.ErrBadRequest,
				"only a post in a calendar channel happens at a time")
		}
		return nil, nil
	}
	if in == nil {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "a calendar post needs a start time")
	}
	if in.StartsAt <= 0 || in.StartsAt > maxTimestamp {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "a start time must be a Unix timestamp in seconds")
	}
	out := protocol.PostEventDetails{StartsAt: in.StartsAt, AllDay: in.AllDay}
	if in.EndsAt != nil {
		end := *in.EndsAt
		if end < in.StartsAt {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "an event cannot end before it starts")
		}
		if end-in.StartsAt > maxEventLength {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "an event cannot last longer than a year")
		}
		out.EndsAt = &end
	}
	location := cleanText(in.Location)
	if utf8.RuneCountInString(location) > maxPostLocation {
		return nil, protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("a location must be at most %d characters", maxPostLocation))
	}
	out.Location = location
	return &out, nil
}

// validatePostWindow checks the bounds of a calendar query. An open end is
// filled in rather than refused: a client that asks for everything from a date
// is asking for the year after it.
func validatePostWindow(from, to int64) (int64, int64, *protocol.Error) {
	if from < 0 || to < 0 || from > maxTimestamp || to > maxTimestamp {
		return 0, 0, protocol.Errorf(protocol.ErrBadRequest, "a window is bounded by Unix timestamps in seconds")
	}
	if to == 0 {
		to = from + maxCalendarWindow
	}
	if to <= from {
		return 0, 0, protocol.Errorf(protocol.ErrBadRequest, "a window has to end after it starts")
	}
	if to-from > maxCalendarWindow {
		return 0, 0, protocol.Errorf(protocol.ErrBadRequest, "that window is too wide; ask for a year at a time")
	}
	return from, to, nil
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
//
// hasAttachments relaxes the one rule files change: a message that carries a
// picture says something without a word of text, so emptiness is only an error
// when there is nothing else in the message either.
func validateMessageContent(raw string, hasAttachments bool) (string, *protocol.Error) {
	content := cleanMessage(raw)
	if content == "" && !hasAttachments {
		return "", protocol.Errorf(protocol.ErrBadRequest, "a message cannot be empty")
	}
	if utf8.RuneCountInString(content) > maxMessageRunes {
		return "", protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("a message must be at most %d characters", maxMessageRunes))
	}
	return content, nil
}
