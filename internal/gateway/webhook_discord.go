package gateway

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// This file holds the shapes an Aural webhook speaks to the outside world in.
// They are Discord's, name for name, because that is the entire promise of the
// feature: an application already posting to a Discord webhook must work after
// changing nothing but the URL it posts to.
//
// Everything that arrives is treated as untrusted input from a service nobody
// here operates. Fields that are not understood are ignored rather than
// refused — a payload written for a newer Discord must still deliver its
// message — and fields that are understood are clamped to the limits below
// rather than rejected, because a rejection turns a cosmetic overflow into a
// notification that never arrives.

// Discord's own limits on a message. Anything longer is truncated to these.
const (
	maxEmbedsPerMessage    = 10
	maxEmbedTitle          = 256
	maxEmbedDescription    = 4096
	maxEmbedFields         = 25
	maxEmbedFieldName      = 256
	maxEmbedFieldValue     = 1024
	maxEmbedFooterText     = 2048
	maxEmbedAuthorName     = 256
	maxWebhookUsername     = 80
	maxWebhookAvatarURLLen = 2048
)

// Discord JSON error codes. A client library switches on these, so the ones we
// can raise carry the number Discord raises them with.
const (
	codeUnknownMessage  = 10008
	codeUnknownWebhook  = 10015
	codeRequestTooLarge = 40005
	codeMissingAccess   = 50001
	codeEmptyMessage    = 50006
	codeInvalidFormBody = 50035
	codeInvalidToken    = 50027
)

// discordError is the body of every failed request to a webhook endpoint.
type discordError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

// discordRateLimited is the body of a 429, which is a different shape from
// every other error Discord returns and is the one clients parse most.
type discordRateLimited struct {
	Message    string  `json:"message"`
	RetryAfter float64 `json:"retry_after"`
	Global     bool    `json:"global"`
}

// executePayload is the body of POST /api/webhooks/{id}/{token}.
//
// The fields this server has nothing to do with are still declared, so that
// decoding one is not an error and a sender that includes them is not refused
// for it. They are read and dropped.
type executePayload struct {
	Content   string           `json:"content"`
	Username  string           `json:"username"`
	AvatarURL string           `json:"avatar_url"`
	Embeds    []protocol.Embed `json:"embeds"`

	// Accepted and ignored. TTS has no meaning without speech synthesis;
	// components, polls and threads are Discord features this server does not
	// have; allowed_mentions describes a suppression this server does not
	// perform, since a mention here is resolved by the reading client from the
	// text rather than compiled into the message.
	TTS             bool            `json:"tts"`
	AllowedMentions json.RawMessage `json:"allowed_mentions"`
	Components      json.RawMessage `json:"components"`
	Poll            json.RawMessage `json:"poll"`
	Flags           int             `json:"flags"`
	ThreadName      string          `json:"thread_name"`
	AppliedTags     json.RawMessage `json:"applied_tags"`
	Attachments     json.RawMessage `json:"attachments"`
}

// discordWebhook is the object GET /api/webhooks/{id}/{token} answers with.
// Type 1 is Discord's "incoming webhook", which is the only kind this is.
type discordWebhook struct {
	ID            string  `json:"id"`
	Type          int     `json:"type"`
	Name          string  `json:"name"`
	Avatar        *string `json:"avatar"`
	ChannelID     string  `json:"channel_id"`
	GuildID       string  `json:"guild_id"`
	ApplicationID *string `json:"application_id"`
	Token         string  `json:"token"`
	URL           string  `json:"url"`
}

// discordUser is the author of a message, as a webhook's own messages report
// it: an application rather than an account.
type discordUser struct {
	ID            string  `json:"id"`
	Username      string  `json:"username"`
	Avatar        *string `json:"avatar"`
	Discriminator string  `json:"discriminator"`
	Bot           bool    `json:"bot"`
}

// discordAttachment is one file on a message, in the shape Discord reports.
type discordAttachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	URL         string `json:"url"`
	ProxyURL    string `json:"proxy_url"`
	ContentType string `json:"content_type,omitempty"`
	Width       *int   `json:"width,omitempty"`
	Height      *int   `json:"height,omitempty"`
}

// discordMessage is what ?wait=true answers with, and what the message
// endpoints return.
//
// Very few senders read it; the ones that do read it to keep the id, so that
// they can edit the message later. That is the field this exists for.
type discordMessage struct {
	ID              string              `json:"id"`
	Type            int                 `json:"type"`
	ChannelID       string              `json:"channel_id"`
	Content         string              `json:"content"`
	Author          discordUser         `json:"author"`
	Attachments     []discordAttachment `json:"attachments"`
	Embeds          []protocol.Embed    `json:"embeds"`
	Timestamp       string              `json:"timestamp"`
	EditedTimestamp *string             `json:"edited_timestamp"`
	Flags           int                 `json:"flags"`
	MentionEveryone bool                `json:"mention_everyone"`
	Mentions        []discordUser       `json:"mentions"`
	MentionRoles    []string            `json:"mention_roles"`
	Pinned          bool                `json:"pinned"`
	TTS             bool                `json:"tts"`
	WebhookID       string              `json:"webhook_id,omitempty"`
}

// snowflake renders an id the way Discord does: a decimal string, because the
// values it uses do not survive a JavaScript number and every client library
// therefore expects text.
func snowflake(id int64) string { return strconv.FormatInt(id, 10) }

// --- sanitising -------------------------------------------------------------

// truncateRunes cuts a string to at most n runes, never mid-character.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// safeURL keeps a URL only if it is one a client may safely follow or load.
//
// Everything here is somebody else's address, and some of it ends up in an
// <img src> or an <a href>. A scheme this server does not recognise is dropped
// rather than passed on, so no payload can turn a rendered card into a way of
// running something in a reader's client.
func safeURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > maxWebhookAvatarURLLen {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return ""
	}
	switch parsed.Scheme {
	case "http", "https":
		return trimmed
	default:
		return ""
	}
}

// sanitiseEmbeds clamps a payload's cards to what this server will store and a
// client will render.
//
// Nothing here refuses: an embed over a limit is cut down, an unusable URL is
// dropped, and a card left with nothing in it at all is removed. A monitoring
// alert that overruns a field by ten characters must still arrive.
func sanitiseEmbeds(in []protocol.Embed) []protocol.Embed {
	if len(in) == 0 {
		return nil
	}
	if len(in) > maxEmbedsPerMessage {
		in = in[:maxEmbedsPerMessage]
	}

	out := make([]protocol.Embed, 0, len(in))
	for _, e := range in {
		clean := protocol.Embed{
			Title: truncateRunes(cleanText(e.Title), maxEmbedTitle),
			// The description is the one field that carries a written
			// paragraph, so line breaks survive it exactly as they do in a
			// message somebody typed.
			Description: truncateRunes(cleanMessage(e.Description), maxEmbedDescription),
			URL:         safeURL(e.URL),
			Timestamp:   cleanText(e.Timestamp),
			Color:       clampColor(e.Color),
			Type:        "rich",
		}
		if e.Footer != nil && strings.TrimSpace(e.Footer.Text) != "" {
			clean.Footer = &protocol.EmbedFooter{
				Text:    truncateRunes(cleanText(e.Footer.Text), maxEmbedFooterText),
				IconURL: safeURL(e.Footer.IconURL),
			}
		}
		if e.Author != nil && strings.TrimSpace(e.Author.Name) != "" {
			clean.Author = &protocol.EmbedAuthor{
				Name:    truncateRunes(cleanText(e.Author.Name), maxEmbedAuthorName),
				URL:     safeURL(e.Author.URL),
				IconURL: safeURL(e.Author.IconURL),
			}
		}
		clean.Image = sanitiseEmbedMedia(e.Image)
		clean.Thumbnail = sanitiseEmbedMedia(e.Thumbnail)
		clean.Video = sanitiseEmbedMedia(e.Video)
		if e.Provider != nil && (strings.TrimSpace(e.Provider.Name) != "" || safeURL(e.Provider.URL) != "") {
			clean.Provider = &protocol.EmbedProvider{
				Name: truncateRunes(cleanText(e.Provider.Name), maxEmbedAuthorName),
				URL:  safeURL(e.Provider.URL),
			}
		}

		fields := e.Fields
		if len(fields) > maxEmbedFields {
			fields = fields[:maxEmbedFields]
		}
		for _, f := range fields {
			name := truncateRunes(cleanText(f.Name), maxEmbedFieldName)
			value := truncateRunes(cleanMessage(f.Value), maxEmbedFieldValue)
			if name == "" && value == "" {
				continue
			}
			clean.Fields = append(clean.Fields, protocol.EmbedField{
				Name: name, Value: value, Inline: f.Inline,
			})
		}

		if embedIsEmpty(clean) {
			continue
		}
		out = append(out, clean)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sanitiseEmbedMedia(m *protocol.EmbedMedia) *protocol.EmbedMedia {
	if m == nil {
		return nil
	}
	href := safeURL(m.URL)
	if href == "" {
		return nil
	}
	// The dimensions are the sender's claim about somebody else's file. They
	// are kept only as a hint for reserving space, so anything absurd is
	// dropped rather than believed.
	width, height := m.Width, m.Height
	if width < 0 || width > 10000 {
		width = 0
	}
	if height < 0 || height > 10000 {
		height = 0
	}
	return &protocol.EmbedMedia{URL: href, Width: width, Height: height}
}

// embedIsEmpty reports a card with nothing left to draw, which is what a
// payload full of fields this server dropped reduces to.
func embedIsEmpty(e protocol.Embed) bool {
	return e.Title == "" && e.Description == "" && e.URL == "" &&
		len(e.Fields) == 0 && e.Image == nil && e.Thumbnail == nil &&
		e.Video == nil && e.Author == nil && e.Footer == nil
}

// clampColor keeps a colour inside the 24 bits an RGB value has. Discord sends
// one as an integer, and a sender that computes it can overflow into a value no
// renderer can use.
func clampColor(in *int) *int {
	if in == nil {
		return nil
	}
	value := *in & 0xFFFFFF
	return &value
}

// embedsJSON encodes the cards for storage, returning nil for a message with
// none so the column stays NULL on nearly every row.
func embedsJSON(embeds []protocol.Embed) (*string, error) {
	if len(embeds) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(embeds)
	if err != nil {
		return nil, err
	}
	encoded := string(raw)
	return &encoded, nil
}
