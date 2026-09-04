package discord

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Discord writes the things a message refers to as ids in angle brackets, and
// resolves them to names in the client rather than in the text. Aural does the
// opposite: a mention is the name it names, resolved when it is drawn. See the
// header of src/lib/mentions.ts on the client for why.
//
// So the ids have to become names on the way across. A relayed message that
// still said <@876543210987654321> would be unreadable to everybody it reached,
// and would resolve to nobody here even if it were parsed, because those are
// Discord's ids and this server has never seen them.
var (
	patternUser    = regexp.MustCompile(`<@!?(\d{15,25})>`)
	patternRole    = regexp.MustCompile(`<@&(\d{15,25})>`)
	patternChannel = regexp.MustCompile(`<#(\d{15,25})>`)
	patternEmoji   = regexp.MustCompile(`<(a?):([A-Za-z0-9_]{1,64}):(\d{15,25})>`)
	patternTime    = regexp.MustCompile(`<t:(-?\d{1,15})(?::([tTdDfFR]))?>`)
)

// Resolver turns the ids in a message into the names a reader would have seen.
// The gateway client satisfies it from the guilds it caches.
type Resolver interface {
	RoleName(id string) (string, bool)
	ChannelName(id string) (string, bool)
}

// RenderContent rewrites one Discord message into the plain text Aural stores.
//
// Everything it cannot resolve degrades to something readable rather than
// disappearing: an unknown role becomes @unknown-role rather than an empty
// space, because a sentence with a hole in it reads as a bug and a sentence
// naming something the reader cannot see reads as what it is.
func RenderContent(m Message, resolve Resolver) string {
	text := m.Content
	if text == "" {
		return ""
	}

	// Users first, from the message's own mentions array: it carries the
	// accounts by id, so no cache and no extra call is needed.
	if len(m.Mentions) > 0 {
		byID := make(map[string]User, len(m.Mentions))
		for _, u := range m.Mentions {
			byID[u.ID] = u
		}
		text = patternUser.ReplaceAllStringFunc(text, func(match string) string {
			id := patternUser.FindStringSubmatch(match)[1]
			if u, ok := byID[id]; ok {
				return "@" + displayName(u)
			}
			return "@unknown-user"
		})
	} else {
		text = patternUser.ReplaceAllString(text, "@unknown-user")
	}

	text = patternRole.ReplaceAllStringFunc(text, func(match string) string {
		id := patternRole.FindStringSubmatch(match)[1]
		if resolve != nil {
			if name, ok := resolve.RoleName(id); ok {
				return "@" + name
			}
		}
		return "@unknown-role"
	})

	text = patternChannel.ReplaceAllStringFunc(text, func(match string) string {
		id := patternChannel.FindStringSubmatch(match)[1]
		if resolve != nil {
			if name, ok := resolve.ChannelName(id); ok {
				return "#" + name
			}
		}
		return "#unknown-channel"
	})

	// A custom emoji becomes :name:, which is exactly how Aural spells its own.
	// A server that happens to carry an emoji by the same name draws it; one
	// that does not leaves the characters, which is what this client already
	// does with any :name: it cannot resolve.
	text = patternEmoji.ReplaceAllString(text, ":$2:")

	// A Discord timestamp renders in the reader's own zone and locale, which
	// is not a thing plain text can do. UTC is the one answer that is wrong for
	// everybody equally rather than silently wrong for one reader.
	text = patternTime.ReplaceAllStringFunc(text, func(match string) string {
		parts := patternTime.FindStringSubmatch(match)
		seconds, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return match
		}
		return formatTimestamp(seconds, parts[2])
	})

	return text
}

// displayName is DisplayName for a bare user, with no guild member to consult.
func displayName(u User) string {
	if name := strings.TrimSpace(u.GlobalName); name != "" {
		return name
	}
	return u.Username
}

// formatTimestamp renders one of Discord's timestamp styles as text.
func formatTimestamp(seconds int64, style string) string {
	t := time.Unix(seconds, 0).UTC()
	switch style {
	case "t":
		return t.Format("15:04 UTC")
	case "T":
		return t.Format("15:04:05 UTC")
	case "d":
		return t.Format("2006-01-02")
	case "D":
		return t.Format("2 January 2006")
	case "R":
		return relativeTime(t)
	case "F":
		return t.Format("Monday, 2 January 2006 15:04 UTC")
	default: // "f", and no style at all, are the same
		return t.Format("2 January 2006 15:04 UTC")
	}
}

// relativeTime renders Discord's R style: how far off the instant is.
func relativeTime(t time.Time) string {
	d := time.Until(t)
	suffix := "from now"
	if d < 0 {
		d, suffix = -d, "ago"
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes %s", int(d.Minutes()), suffix)
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours %s", int(d.Hours()), suffix)
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d days %s", int(d.Hours()/24), suffix)
	default:
		return t.Format("2 January 2006")
	}
}

// EscapeOutbound neutralises the markup a message picks up on the way into
// Discord.
//
// Text written here is plain, and Discord reads it as Markdown with mentions
// in it. Two things follow. Anything shaped like <@123> would resolve to
// whoever that id belongs to on the Discord side — allowed_mentions already
// stops it pinging, but it would still render as somebody's name in a sentence
// they had nothing to do with — and @everyone would read as an attempt at one
// even where it cannot fire. Both are defanged by breaking the syntax with a
// zero-width character rather than by deleting anything, so the words a person
// wrote still reach the page.
func EscapeOutbound(text string) string {
	if text == "" {
		return ""
	}
	// A zero-width space between the sigil and what follows it. Invisible to a
	// reader, and enough that neither Discord's parser nor its mention
	// resolver sees a token.
	const zwsp = "​"

	replacer := strings.NewReplacer(
		"@everyone", "@"+zwsp+"everyone",
		"@here", "@"+zwsp+"here",
		"<@", "<"+zwsp+"@",
		"<#", "<"+zwsp+"#",
	)
	return replacer.Replace(text)
}

// TruncateRunes cuts text to at most n runes, never mid-character, appending an
// ellipsis when it had to cut.
//
// Both sides happen to cap a message at two thousand characters, so this fires
// only on the seam: a relayed message that gains a quoted reply header can
// cross the line the original sat just under.
func TruncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	// The ellipsis is a rune of its own, so the cut is one short of the limit:
	// the result has to fit under n, not land on it.
	count := 0
	for i := range s {
		if count == n-1 {
			return s[:i] + "…"
		}
		count++
	}
	return s
}
