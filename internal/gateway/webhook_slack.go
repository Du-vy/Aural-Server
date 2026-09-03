package gateway

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// Slack-shaped deliveries, accepted at /api/webhooks/{id}/{token}/slack.
//
// Discord accepts this dialect on the same path, which is why a great many
// services offer "Slack-compatible webhook" as their only option and are
// pointed at Discord anyway. Accepting it here costs one translation and means
// those services need no adapter either.

// slackPayloadBody is the incoming body. Slack's schema is much larger than
// this; what is here is the part that has a meaning once rendered as a message.
type slackPayloadBody struct {
	Text        string            `json:"text"`
	Username    string            `json:"username"`
	IconURL     string            `json:"icon_url"`
	IconEmoji   string            `json:"icon_emoji"`
	Attachments []slackAttachment `json:"attachments"`
}

type slackAttachment struct {
	Fallback   string       `json:"fallback"`
	Color      string       `json:"color"`
	Pretext    string       `json:"pretext"`
	AuthorName string       `json:"author_name"`
	AuthorLink string       `json:"author_link"`
	AuthorIcon string       `json:"author_icon"`
	Title      string       `json:"title"`
	TitleLink  string       `json:"title_link"`
	Text       string       `json:"text"`
	Fields     []slackField `json:"fields"`
	ImageURL   string       `json:"image_url"`
	ThumbURL   string       `json:"thumb_url"`
	Footer     string       `json:"footer"`
	FooterIcon string       `json:"footer_icon"`
	// Slack sends the timestamp as a number on some paths and as a string on
	// others, so it is read as whichever arrived.
	Timestamp json.Number `json:"ts"`
}

type slackField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

// slackNamedColors are the three words Slack accepts in place of a hex value.
var slackNamedColors = map[string]int{
	"good":    0x2eb886,
	"warning": 0xdaa038,
	"danger":  0xa30100,
}

// slackPayload translates a Slack body into the delivery the rest of the
// pipeline already knows how to post.
func slackPayload(body []byte) (executePayload, *deliveryFailure) {
	var in slackPayloadBody
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			return executePayload{}, deliveryError(http.StatusBadRequest, codeInvalidFormBody,
				"Invalid Form Body")
		}
	}

	out := executePayload{
		Content:  slackText(in.Text),
		Username: in.Username,
		// icon_emoji has no URL behind it, so only an icon_url can become a
		// picture. A sender that gave an emoji keeps the webhook's own.
		AvatarURL: in.IconURL,
	}
	for _, a := range in.Attachments {
		out.Embeds = append(out.Embeds, slackEmbed(a))
	}
	return out, nil
}

// slackEmbed renders one Slack attachment as a card.
func slackEmbed(a slackAttachment) protocol.Embed {
	// Slack draws the pretext above the attachment and the text inside it.
	// There is one description here, so they are joined in the order they are
	// read, which keeps a two-part message reading as one.
	description := strings.TrimSpace(strings.Join(
		nonEmpty(slackText(a.Pretext), slackText(a.Text)), "\n\n"))
	if description == "" {
		// A sender that only filled in the fallback still meant to say
		// something, and the fallback is what it meant to say.
		description = slackText(a.Fallback)
	}

	embed := protocol.Embed{
		Title:       a.Title,
		Description: description,
		URL:         a.TitleLink,
		Color:       slackColor(a.Color),
	}
	if a.AuthorName != "" {
		embed.Author = &protocol.EmbedAuthor{
			Name: a.AuthorName, URL: a.AuthorLink, IconURL: a.AuthorIcon,
		}
	}
	if a.Footer != "" {
		embed.Footer = &protocol.EmbedFooter{Text: a.Footer, IconURL: a.FooterIcon}
	}
	if a.ImageURL != "" {
		embed.Image = &protocol.EmbedMedia{URL: a.ImageURL}
	}
	if a.ThumbURL != "" {
		embed.Thumbnail = &protocol.EmbedMedia{URL: a.ThumbURL}
	}
	if ts := slackTimestamp(a.Timestamp); ts != "" {
		embed.Timestamp = ts
	}
	for _, f := range a.Fields {
		embed.Fields = append(embed.Fields, protocol.EmbedField{
			Name: f.Title, Value: slackText(f.Value), Inline: f.Short,
		})
	}
	return embed
}

// slackColor reads the three named colours Slack allows as well as a hex value.
func slackColor(raw string) *int {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return nil
	}
	if named, ok := slackNamedColors[value]; ok {
		return &named
	}
	parsed, err := strconv.ParseInt(strings.TrimPrefix(value, "#"), 16, 32)
	if err != nil {
		return nil
	}
	out := int(parsed)
	return &out
}

// slackTimestamp turns Slack's epoch seconds — which arrive as "1699999999" or
// as "1699999999.000200" — into the instant a card carries.
func slackTimestamp(raw json.Number) string {
	value := strings.TrimSpace(raw.String())
	if value == "" {
		return ""
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || seconds <= 0 {
		return ""
	}
	return time.Unix(int64(seconds), 0).UTC().Format(time.RFC3339)
}

// slackText rewrites the parts of Slack's mrkdwn that would otherwise be read
// as literal punctuation.
//
// The links are the ones that matter: <https://example.com|label> is Slack's
// only link syntax, and left alone it reads as an angle bracket and a pipe
// rather than as the label somebody wrote.
func slackText(in string) string {
	if in == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(in))

	for {
		open := strings.IndexByte(in, '<')
		if open < 0 {
			b.WriteString(in)
			break
		}
		shut := strings.IndexByte(in[open:], '>')
		if shut < 0 {
			b.WriteString(in)
			break
		}
		shut += open

		b.WriteString(in[:open])
		inner := in[open+1 : shut]
		in = in[shut+1:]

		switch {
		case inner == "":
		case strings.HasPrefix(inner, "!"):
			// <!here>, <!channel>, <!everyone>: a broadcast this server has no
			// equivalent of, written out as the word it stands for.
			b.WriteString("@" + strings.TrimPrefix(strings.SplitN(inner, "|", 2)[0], "!"))
		case strings.HasPrefix(inner, "#"), strings.HasPrefix(inner, "@"):
			// A channel or user id in Slack's workspace, which means nothing
			// here. The label, when there is one, is what a reader can use.
			if label := after(inner, "|"); label != "" {
				b.WriteString(label)
			} else {
				b.WriteString(inner)
			}
		default:
			url, label, found := strings.Cut(inner, "|")
			if !found || label == "" {
				b.WriteString(url)
			} else {
				b.WriteString("[" + label + "](" + url + ")")
			}
		}
	}
	return b.String()
}

// after returns what follows the first separator, or an empty string.
func after(s, sep string) string {
	_, rest, found := strings.Cut(s, sep)
	if !found {
		return ""
	}
	return rest
}

// nonEmpty drops the blanks from a list, so joining it never leaves a gap.
func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}
