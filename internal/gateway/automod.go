package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// The bounds on what a rule set may ask for. They are here rather than in the
// configuration file because they are about what the engine can do in the time
// a message send is allowed to take, not about what an operator wants.
const (
	maxAutoModWords     = 500
	maxAutoModWordRunes = 64
	maxAutoModDomains   = 100
	maxAutoModExempt    = 64
	// recentWindow is how far back the flood and repetition rules ever look.
	// Everything older is dropped as it goes past, which is what keeps the
	// per-session history a handful of entries rather than a transcript.
	recentWindow = 60 * time.Second
	// maxRecent bounds that history whatever the configured window is.
	maxRecent = 32
)

// censorMark is what a censored word is replaced with. One glyph per letter,
// so the shape of the sentence survives and it is obvious what happened.
const censorMark = '█'

// urlPattern finds anything a reader would click.
//
// It deliberately catches more than a strict URL parser would: a bare
// "example.com/thing" is a link to everybody who reads it, and a rule that only
// caught the ones written with a scheme would be defeated by deleting eight
// characters. The cost is the occasional false positive on a sentence with no
// space after a full stop, which is why the rule is off by default and why
// censoring is offered as an alternative to refusing the message.
var urlPattern = regexp.MustCompile(
	`(?i)\b(?:[a-z][a-z0-9+.-]*://[^\s<>"']+|(?:www\.)?[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*\.[a-z]{2,24}(?:/[^\s<>"']*)?)`)

// mentionPattern counts who a message addresses. It matches the two forms the
// client writes — an id and a name — plus the two that address everybody.
var mentionPattern = regexp.MustCompile(`(?i)<@!?\d+>|@everyone\b|@here\b|@[\p{L}\p{N}_.-]{1,32}`)

// autoModVerdict is what the engine decided about one message.
type autoModVerdict struct {
	// Content is what should be stored. It differs from what was sent only
	// when a rule censored something.
	Content string
	// Blocked names the rule that refused the message, or is empty.
	Blocked string
	// Censored names the rule that masked part of it, or is empty.
	Censored string
}

// AutoMod is the rule set currently in force.
func (h *Hub) AutoMod() protocol.AutoModConfig {
	h.cacheMu.RLock()
	defer h.cacheMu.RUnlock()
	return h.automod
}

// setAutoMod replaces the cached rule set.
func (h *Hub) setAutoMod(cfg protocol.AutoModConfig) {
	h.cacheMu.Lock()
	h.automod = cfg
	h.cacheMu.Unlock()
}

// ReloadAutoMod reads the stored rules into the cache. A row that will not
// decode leaves the defaults in place, which moderate nothing: rules nobody can
// read are not rules to enforce a guess at.
func (h *Hub) ReloadAutoMod(ctx context.Context) error {
	cfg := protocol.DefaultAutoMod()
	if err := h.st.AutoMod(ctx, &cfg); err != nil {
		h.log.Error("read the automatic moderation rules", slog.Any("error", err))
		h.setAutoMod(protocol.DefaultAutoMod())
		return nil
	}
	h.setAutoMod(normaliseAutoMod(cfg))
	return nil
}

// screenMessage runs every rule over a message somebody is sending.
//
// It is called on the way in, before anything is written, so a blocked message
// never existed and a censored one was never stored uncensored.
func (s *Session) screenMessage(ctx context.Context, channelID int64, content string) (autoModVerdict, *protocol.Error) {
	return s.screen(ctx, channelID, content, true)
}

// screenText runs the rules that are about what a message says, and not the two
// that are about how often one is sent.
//
// It is what an edit and a post title go through. Both have to be screened —
// a rule that only looked at sends would be worked around by posting a full
// stop and editing it — but neither is a new message: counting an edit towards
// the flood limit would refuse somebody for fixing a typo, and comparing a
// title against the body that was screened a line earlier would report every
// post as a repeat of itself.
func (s *Session) screenText(ctx context.Context, channelID int64, content string) (autoModVerdict, *protocol.Error) {
	return s.screen(ctx, channelID, content, false)
}

// screen is both of the above. sending decides whether the rules about pace
// apply and whether this message joins the short history they compare against.
func (s *Session) screen(ctx context.Context, channelID int64, content string, sending bool) (autoModVerdict, *protocol.Error) {
	verdict := autoModVerdict{Content: content}

	cfg := s.hub.AutoMod()
	if !cfg.Enabled || s.exemptFromAutoMod(cfg.ExemptRoles) {
		return verdict, nil
	}
	for _, id := range cfg.ExemptChannels {
		if id == channelID {
			return verdict, nil
		}
	}

	folded := store.Fold(content)

	// The rules that refuse outright come first, so a message that is going to
	// be rejected is not censored on the way to being rejected.
	if rule := cfg.Flood.AutoModRule; sending && rule.Enabled && !s.exemptFromAutoMod(rule.ExemptRoles) {
		if s.flooding(cfg.Flood) {
			return verdict, s.autoModRefusal(ctx, channelID, "flood",
				"you are sending messages too quickly")
		}
	}
	if rule := cfg.Repetition.AutoModRule; sending && rule.Enabled && !s.exemptFromAutoMod(rule.ExemptRoles) {
		if s.repeating(cfg.Repetition.Times, folded) {
			return verdict, s.autoModRefusal(ctx, channelID, "repetition",
				"that message has already been sent")
		}
	}
	if rule := cfg.Mentions.AutoModRule; rule.Enabled && !s.exemptFromAutoMod(rule.ExemptRoles) {
		if limit := cfg.Mentions.Limit; limit > 0 && countMentions(content) > limit {
			return verdict, s.autoModRefusal(ctx, channelID, "mentions",
				fmt.Sprintf("a message may address at most %d people here", limit))
		}
	}
	if rule := cfg.Caps.AutoModRule; rule.Enabled && !s.exemptFromAutoMod(rule.ExemptRoles) {
		if shouting(content, cfg.Caps.Percent, cfg.Caps.MinLength) {
			return verdict, s.autoModRefusal(ctx, channelID, "caps",
				"that message is mostly capitals")
		}
	}

	if rule := cfg.Links.AutoModRule; rule.Enabled && !s.exemptFromAutoMod(rule.ExemptRoles) {
		masked, found := screenLinks(verdict.Content, cfg.Links.AllowedDomains, rule.Action)
		if found {
			if rule.Action == protocol.AutoModCensor {
				verdict.Content, verdict.Censored = masked, "links"
			} else {
				return verdict, s.autoModRefusal(ctx, channelID, "links",
					"links are not allowed here")
			}
		}
	}

	if rule := cfg.Words.AutoModRule; rule.Enabled && !s.exemptFromAutoMod(rule.ExemptRoles) {
		masked, found := screenWords(verdict.Content, cfg.Words.Words, cfg.Words.WholeWord,
			rule.Action)
		if found {
			if rule.Action == protocol.AutoModCensor {
				verdict.Content, verdict.Censored = masked, "words"
			} else {
				return verdict, s.autoModRefusal(ctx, channelID, "words",
					"that message contains a word this server does not allow")
			}
		}
	}

	if verdict.Censored != "" {
		s.hub.auditAutoMod(ctx, s, channelID, verdict.Censored, protocol.AutoModCensor)
	}
	// Only a message that is actually going to be sent, and that survived every
	// rule, counts towards the flood and repetition history: a refused one is
	// not something the writer should be penalised for twice, and an edit is
	// not a new message at all.
	if sending {
		s.rememberMessage(folded)
	}
	return verdict, nil
}

// autoModRefusal records the block and builds what the writer is told.
func (s *Session) autoModRefusal(ctx context.Context, channelID int64, rule, message string) *protocol.Error {
	s.hub.auditAutoMod(ctx, s, channelID, rule, protocol.AutoModBlock)
	return protocol.Errorf(protocol.ErrAutoModBlocked, message)
}

// auditAutoMod records that a rule fired.
//
// It names the rule and the channel and nothing else. The message itself is
// deliberately not in the log: a blocked message was never accepted by this
// server, and writing it into a table that moderators read would make the
// blocking the thing that published it.
func (h *Hub) auditAutoMod(ctx context.Context, s *Session, channelID int64, rule, action string) {
	name := ""
	if channel, ok := h.Channel(channelID); ok {
		name = channel.Name
	}
	entry := auditTarget(protocol.AuditTargetChannel, channelID, name)
	entry.Action = protocol.AuditAutoModAction
	entry.Reason = rule + " (" + action + ")"
	entry.Changes = []store.AuditChange{{Key: "author", After: s.User().Nickname}}
	h.audit(ctx, nil, entry)
}

// exemptFromAutoMod reports whether this session is outside the rules.
//
// Holding Administrator is exemption in itself, which is the same rule the
// permission mask follows everywhere else in this server: a bit that satisfies
// every check would be a strange thing to have to also list by role.
func (s *Session) exemptFromAutoMod(roleIDs []int64) bool {
	base, held := s.Permissions()
	if base&permissions.Administrator != 0 {
		return true
	}
	if len(roleIDs) == 0 {
		return false
	}
	for _, want := range roleIDs {
		for _, have := range held {
			if have == want {
				return true
			}
		}
	}
	return false
}

// --- the individual rules ---------------------------------------------------

// screenWords masks or reports the listed terms.
//
// The comparison is made on a folded copy so that one entry catches the word
// however it was written, but the replacement is made on the original: the
// folded text is not what anybody typed, and a censor that returned it would
// quietly strip the accents off every other word in the sentence.
//
// Folding can change a string's length — ß folds to "ss" — so a folded offset
// is not an offset into the original. The two are walked together instead,
// which keeps the mapping exact whatever the input.
func screenWords(content string, words []string, wholeWord bool, action string) (string, bool) {
	if len(words) == 0 || content == "" {
		return content, false
	}

	runes := []rune(content)
	// foldedAt[i] is where rune i of the original starts in the folded copy.
	foldedAt := make([]int, len(runes)+1)
	var folded strings.Builder
	for i, r := range runes {
		foldedAt[i] = folded.Len()
		folded.WriteString(store.Fold(string(r)))
	}
	foldedAt[len(runes)] = folded.Len()
	haystack := folded.String()

	// The reverse map: a byte offset in the folded copy back to the rune it
	// came from. Every byte of a folded expansion points at its own rune, so a
	// match that starts inside one is attributed to the character that
	// produced it.
	origin := make([]int, folded.Len()+1)
	for i := range runes {
		for b := foldedAt[i]; b < foldedAt[i+1]; b++ {
			origin[b] = i
		}
	}
	origin[folded.Len()] = len(runes)

	masked := make([]rune, len(runes))
	copy(masked, runes)
	found := false

	for _, word := range words {
		needle := store.Fold(strings.TrimSpace(word))
		if needle == "" {
			continue
		}
		from := 0
		for {
			at := strings.Index(haystack[from:], needle)
			if at < 0 {
				break
			}
			start := from + at
			end := start + len(needle)
			from = start + 1

			if wholeWord && !isWordBoundary(haystack, start, end) {
				continue
			}
			found = true
			if action != protocol.AutoModCensor {
				// Nothing to mask when the message is going to be refused;
				// knowing that one word matched is the whole answer.
				return content, true
			}
			for i := origin[start]; i < origin[end] && i < len(masked); i++ {
				if !unicode.IsSpace(masked[i]) {
					masked[i] = censorMark
				}
			}
		}
	}
	if !found {
		return content, false
	}
	return string(masked), true
}

// isWordBoundary reports whether a match sits on its own rather than inside a
// longer word, which is what stops a list from catching an innocent word that
// happens to contain a banned one.
func isWordBoundary(text string, start, end int) bool {
	before := ' '
	if start > 0 {
		before = rune(text[start-1])
	}
	after := ' '
	if end < len(text) {
		after = rune(text[end])
	}
	return !isWordRune(before) && !isWordRune(after)
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// screenLinks masks or reports the URLs a message carries, sparing the domains
// the server allows.
func screenLinks(content string, allowed []string, action string) (string, bool) {
	spans := urlPattern.FindAllStringIndex(content, -1)
	if len(spans) == 0 {
		return content, false
	}

	var out strings.Builder
	last := 0
	found := false
	for _, span := range spans {
		candidate := content[span[0]:span[1]]
		if domainAllowed(candidate, allowed) {
			continue
		}
		found = true
		if action != protocol.AutoModCensor {
			return content, true
		}
		out.WriteString(content[last:span[0]])
		out.WriteString(strings.Repeat(string(censorMark), len([]rune(candidate))))
		last = span[1]
	}
	if !found {
		return content, false
	}
	out.WriteString(content[last:])
	return out.String(), true
}

// domainAllowed reports whether a URL points at somewhere the server permits.
// An entry covers its own subdomains, because that is what somebody writing
// "github.com" into an allow list means.
func domainAllowed(raw string, allowed []string) bool {
	if len(allowed) == 0 {
		return false
	}
	host := strings.ToLower(raw)
	if scheme := strings.Index(host, "://"); scheme >= 0 {
		host = host[scheme+3:]
	}
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	host = strings.TrimSuffix(strings.SplitN(host, "/", 2)[0], ".")
	host = strings.SplitN(host, ":", 2)[0]

	for _, entry := range allowed {
		want := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(entry, ".")))
		if want == "" {
			continue
		}
		if host == want || strings.HasSuffix(host, "."+want) {
			return true
		}
	}
	return false
}

// countMentions is how many people one message addresses.
func countMentions(content string) int {
	return len(mentionPattern.FindAllString(content, -1))
}

// shouting reports whether a message is mostly capitals. Only letters that
// have a case are counted: a message in a script with no capitals cannot be
// shouting, and one made of numbers is not either.
func shouting(content string, percent, minLength int) bool {
	if percent <= 0 || percent > 100 {
		return false
	}
	upper, cased := 0, 0
	for _, r := range content {
		if !unicode.IsLetter(r) {
			continue
		}
		if unicode.IsUpper(r) || unicode.IsLower(r) {
			cased++
			if unicode.IsUpper(r) {
				upper++
			}
		}
	}
	if cased < minLength || cased == 0 {
		return false
	}
	return upper*100/cased >= percent
}

// --- what one connection has said recently ----------------------------------

// rememberMessage adds a message to the short history the flood and repetition
// rules compare against, dropping whatever has aged out.
func (s *Session) rememberMessage(folded string) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	kept := s.recent[:0]
	for _, entry := range s.recent {
		if now.Sub(entry.at) <= recentWindow {
			kept = append(kept, entry)
		}
	}
	s.recent = append(kept, recentMessage{at: now, folded: folded})
	if len(s.recent) > maxRecent {
		s.recent = s.recent[len(s.recent)-maxRecent:]
	}
}

// flooding reports whether the flood rule is about to fire: this message would
// be one too many inside the configured window.
func (s *Session) flooding(rule protocol.AutoModFlood) bool {
	limit, window := rule.Messages, time.Duration(rule.Seconds)*time.Second
	if limit <= 0 || window <= 0 {
		return false
	}
	cutoff := time.Now().Add(-window)

	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := 0
	for _, entry := range s.recent {
		if entry.at.After(cutoff) {
			seen++
		}
	}
	return seen >= limit
}

// repeating reports whether this message is the same as the last few.
func (s *Session) repeating(times int, folded string) bool {
	if times <= 0 || folded == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	run := 0
	for i := len(s.recent) - 1; i >= 0; i-- {
		if s.recent[i].folded != folded {
			break
		}
		run++
		if run >= times {
			return true
		}
	}
	return false
}

// --- validation -------------------------------------------------------------

// normaliseAutoMod brings a rule set inside the bounds the engine can run, and
// fills in the fields a client left out. It is applied to what arrives over the
// wire and to what is read back from the database, so a row written by an older
// build is corrected rather than trusted.
func normaliseAutoMod(cfg protocol.AutoModConfig) protocol.AutoModConfig {
	cfg.ExemptRoles = clampIDs(cfg.ExemptRoles)
	cfg.ExemptChannels = clampIDs(cfg.ExemptChannels)

	cfg.Words.AutoModRule = normaliseRule(cfg.Words.AutoModRule, protocol.AutoModCensor, true)
	cfg.Links.AutoModRule = normaliseRule(cfg.Links.AutoModRule, protocol.AutoModBlock, true)
	// These three have nothing to mask: a message that mentions too many
	// people cannot be partly mentioned, so blocking is the only action.
	cfg.Mentions.AutoModRule = normaliseRule(cfg.Mentions.AutoModRule, protocol.AutoModBlock, false)
	cfg.Caps.AutoModRule = normaliseRule(cfg.Caps.AutoModRule, protocol.AutoModBlock, false)
	cfg.Flood.AutoModRule = normaliseRule(cfg.Flood.AutoModRule, protocol.AutoModBlock, false)
	cfg.Repetition.AutoModRule = normaliseRule(cfg.Repetition.AutoModRule, protocol.AutoModBlock, false)

	words := make([]string, 0, min(len(cfg.Words.Words), maxAutoModWords))
	seen := map[string]bool{}
	for _, word := range cfg.Words.Words {
		word = strings.TrimSpace(word)
		if word == "" || len([]rune(word)) > maxAutoModWordRunes {
			continue
		}
		folded := store.Fold(word)
		if folded == "" || seen[folded] {
			continue
		}
		seen[folded] = true
		words = append(words, word)
		if len(words) >= maxAutoModWords {
			break
		}
	}
	cfg.Words.Words = words

	domains := make([]string, 0, min(len(cfg.Links.AllowedDomains), maxAutoModDomains))
	for _, domain := range cfg.Links.AllowedDomains {
		domain = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(domain, ".")))
		if domain == "" || strings.ContainsAny(domain, " /\\") {
			continue
		}
		domains = append(domains, domain)
		if len(domains) >= maxAutoModDomains {
			break
		}
	}
	cfg.Links.AllowedDomains = domains

	cfg.Mentions.Limit = clamp(cfg.Mentions.Limit, 1, 50, 5)
	cfg.Caps.Percent = clamp(cfg.Caps.Percent, 10, 100, 70)
	cfg.Caps.MinLength = clamp(cfg.Caps.MinLength, 4, 500, 12)
	cfg.Flood.Messages = clamp(cfg.Flood.Messages, 2, 30, 5)
	cfg.Flood.Seconds = clamp(cfg.Flood.Seconds, 1, 60, 5)
	cfg.Repetition.Times = clamp(cfg.Repetition.Times, 2, 20, 3)
	return cfg
}

// normaliseRule fixes up the half of a rule every rule has. allowCensor is
// false for the rules that have nothing to mask, and forces them to block.
func normaliseRule(rule protocol.AutoModRule, fallback string, allowCensor bool) protocol.AutoModRule {
	switch rule.Action {
	case protocol.AutoModBlock:
	case protocol.AutoModCensor:
		if !allowCensor {
			rule.Action = protocol.AutoModBlock
		}
	default:
		rule.Action = fallback
	}
	rule.ExemptRoles = clampIDs(rule.ExemptRoles)
	return rule
}

// clampIDs bounds and de-duplicates a list of ids, and makes sure it is a list
// rather than null: a client reading the rules back should find an empty array
// where it left one.
func clampIDs(ids []int64) []int64 {
	out := make([]int64, 0, min(len(ids), maxAutoModExempt))
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) >= maxAutoModExempt {
			break
		}
	}
	return out
}

// clamp bounds a number, substituting a default for one that was left out.
func clamp(value, low, high, fallback int) int {
	if value == 0 {
		return fallback
	}
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// screenRelayed runs the content rules over a message that arrived from
// somewhere else.
//
// A bridge must not be a hole in the moderation. A word list that only applied
// to what was typed here would be walked around by typing it on Discord
// instead, which is a worse outcome than not having the bridge: the rule would
// look enforced and would not be.
//
// It is not screenMessage with a fake session, because the rules divide in
// two. The ones about content — mentions, capitals, links, words — mean the
// same thing whoever wrote them, and are applied. The ones about pace — flood
// and repetition — measure one connection's own history, and a relayed author
// has no connection here; counting them against a shared queue would let one
// talkative person on Discord silence everybody else on it. Role exemptions do
// not apply either, for the plainest of reasons: whoever wrote this holds no
// roles on this server.
//
// It returns the content to store and whether the message was refused outright.
func (h *Hub) screenRelayed(ctx context.Context, channelID int64, content, author string) (string, bool) {
	cfg := h.AutoMod()
	if !cfg.Enabled {
		return content, false
	}
	for _, id := range cfg.ExemptChannels {
		if id == channelID {
			return content, false
		}
	}

	if rule := cfg.Mentions.AutoModRule; rule.Enabled {
		if limit := cfg.Mentions.Limit; limit > 0 && countMentions(content) > limit {
			h.auditRelayAutoMod(ctx, channelID, author, "mentions", protocol.AutoModBlock)
			return content, true
		}
	}
	if rule := cfg.Caps.AutoModRule; rule.Enabled {
		if shouting(content, cfg.Caps.Percent, cfg.Caps.MinLength) {
			h.auditRelayAutoMod(ctx, channelID, author, "caps", protocol.AutoModBlock)
			return content, true
		}
	}

	censored := ""
	if rule := cfg.Links.AutoModRule; rule.Enabled {
		masked, found := screenLinks(content, cfg.Links.AllowedDomains, rule.Action)
		if found {
			if rule.Action != protocol.AutoModCensor {
				h.auditRelayAutoMod(ctx, channelID, author, "links", protocol.AutoModBlock)
				return content, true
			}
			content, censored = masked, "links"
		}
	}
	if rule := cfg.Words.AutoModRule; rule.Enabled {
		masked, found := screenWords(content, cfg.Words.Words, cfg.Words.WholeWord, rule.Action)
		if found {
			if rule.Action != protocol.AutoModCensor {
				h.auditRelayAutoMod(ctx, channelID, author, "words", protocol.AutoModBlock)
				return content, true
			}
			content, censored = masked, "words"
		}
	}

	if censored != "" {
		h.auditRelayAutoMod(ctx, channelID, author, censored, protocol.AutoModCensor)
	}
	return content, false
}

// auditRelayAutoMod records that a rule fired on a relayed message.
//
// It names the author as they appeared on the other side, which is the only
// identity there is: there is no account here to point at, and a log entry that
// said nothing about who wrote it would be a log entry nobody could act on.
func (h *Hub) auditRelayAutoMod(ctx context.Context, channelID int64, author, rule, action string) {
	name := ""
	if channel, ok := h.Channel(channelID); ok {
		name = channel.Name
	}
	entry := auditTarget(protocol.AuditTargetChannel, channelID, name)
	entry.Action = protocol.AuditAutoModAction
	entry.Reason = rule + " (" + action + ", relayed)"
	entry.Changes = []store.AuditChange{{Key: "author", After: author}}
	h.audit(ctx, nil, entry)
}
