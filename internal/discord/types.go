package discord

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// Message types this relay carries.
//
// Discord numbers every kind of thing that can appear in a channel, and most
// of them are notices rather than messages: somebody joined, a message was
// pinned, the server was boosted. Relaying those would fill an Aural channel
// with events about a Discord server its readers are not in, so only the two
// kinds a person actually typed are carried.
const (
	MessageTypeDefault = 0
	MessageTypeReply   = 19
)

// User is the account behind a message.
type User struct {
	ID string `json:"id"`
	// Username is the unique handle. GlobalName is the display name Discord
	// added when it dropped discriminators, and is what a reader actually sees
	// — but it is optional, and empty on an account that never set one.
	Username      string  `json:"username"`
	GlobalName    string  `json:"global_name"`
	Discriminator string  `json:"discriminator"`
	Avatar        *string `json:"avatar"`
	Bot           bool    `json:"bot"`
}

// Member is what one guild knows about a user: the nickname and picture they
// set for that server specifically, which override the account's own.
type Member struct {
	Nick   *string  `json:"nick"`
	Avatar *string  `json:"avatar"`
	Roles  []string `json:"roles"`
}

// Attachment is one file on a message.
type Attachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	URL         string `json:"url"`
	ProxyURL    string `json:"proxy_url"`
	ContentType string `json:"content_type"`
	Width       *int   `json:"width"`
	Height      *int   `json:"height"`
}

// StickerItem is the little of a sticker that rides on a message. The full
// object needs another call; the id and the format are enough to build the
// image URL, which is all a relay wants.
type StickerItem struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	FormatType int    `json:"format_type"`
}

// MessageReference names the message a reply is answering.
type MessageReference struct {
	MessageID string `json:"message_id"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
}

// Message is one line of a Discord channel.
//
// Only the fields a relay reads are declared. An unknown field is dropped by
// the decoder rather than refused, which is what keeps this working when
// Discord adds one.
type Message struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
	Type      int    `json:"type"`
	Content   string `json:"content"`
	Author    User   `json:"author"`
	// Member is present on a message sent in a guild, and carries the
	// per-guild nickname and avatar that a reader of that guild actually sees.
	Member *Member `json:"member"`
	// WebhookID is set on a message a webhook posted, and is the whole of the
	// loop guard: a message this server relayed into Discord comes back
	// carrying the id of the webhook it was posted through, and is dropped on
	// sight. See internal/gateway/relay.go.
	WebhookID   string            `json:"webhook_id"`
	Attachments []Attachment      `json:"attachments"`
	Embeds      []protocol.Embed  `json:"embeds"`
	Stickers    []StickerItem     `json:"sticker_items"`
	Mentions    []User            `json:"mentions"`
	Reference   *MessageReference `json:"message_reference"`
	Referenced  *Message          `json:"referenced_message"`
	Timestamp   string            `json:"timestamp"`
	EditedAt    *string           `json:"edited_timestamp"`
	Flags       int               `json:"flags"`
}

// Relayable reports whether a message is one a person typed, rather than one
// of Discord's own notices about the channel.
func (m Message) Relayable() bool {
	return m.Type == MessageTypeDefault || m.Type == MessageTypeReply
}

// DisplayName is the name to show a message under, in the order a Discord
// client itself resolves it: the per-guild nickname first, then the account's
// display name, then the bare handle.
func (m Message) DisplayName() string {
	if m.Member != nil && m.Member.Nick != nil && strings.TrimSpace(*m.Member.Nick) != "" {
		return strings.TrimSpace(*m.Member.Nick)
	}
	if name := strings.TrimSpace(m.Author.GlobalName); name != "" {
		return name
	}
	return m.Author.Username
}

// AvatarURL is the picture to show a message under, resolved the same way: the
// per-guild avatar wins over the account's, and an account with none falls
// back to the coloured default Discord serves, so a relayed message is never
// pictureless.
//
// size is a power of two between 16 and 4096; Discord rejects anything else.
func (m Message) AvatarURL(size int) string {
	if m.Member != nil && m.Member.Avatar != nil && *m.Member.Avatar != "" && m.GuildID != "" {
		return fmt.Sprintf("%s/guilds/%s/users/%s/avatars/%s.%s?size=%d",
			cdnBase, m.GuildID, m.Author.ID, *m.Member.Avatar, avatarExt(*m.Member.Avatar), size)
	}
	return m.Author.AvatarURL(size)
}

// AvatarURL builds the account's own picture URL.
func (u User) AvatarURL(size int) string {
	if u.Avatar != nil && *u.Avatar != "" {
		return fmt.Sprintf("%s/avatars/%s/%s.%s?size=%d",
			cdnBase, u.ID, *u.Avatar, avatarExt(*u.Avatar), size)
	}
	return fmt.Sprintf("%s/embed/avatars/%d.png", cdnBase, defaultAvatarIndex(u))
}

// avatarExt picks the format an avatar hash is served in. A hash beginning
// a_ is animated, and asking for it as a PNG serves a still frame; asking for
// a still one as a GIF is a 415.
func avatarExt(hash string) string {
	if strings.HasPrefix(hash, "a_") {
		return "gif"
	}
	return "png"
}

// defaultAvatarIndex picks which of Discord's six default pictures an account
// with no avatar is drawn with.
//
// There are two rules, because there are two generations of account. A legacy
// account still has a discriminator and is indexed by it modulo five; an
// account migrated to the unique-handle scheme has the discriminator "0" and
// is indexed by its snowflake's timestamp bits modulo six. Getting this wrong
// is cosmetic, so an id that will not parse falls back to the first.
func defaultAvatarIndex(u User) int {
	if u.Discriminator != "" && u.Discriminator != "0" {
		if n, err := strconv.Atoi(u.Discriminator); err == nil {
			return n % 5
		}
		return 0
	}
	id, err := strconv.ParseUint(u.ID, 10, 64)
	if err != nil {
		return 0
	}
	return int((id >> 22) % 6)
}

// StickerURL builds the image for a sticker item. Lottie stickers are vector
// animations only a Discord client can play, so they get no URL and the relay
// falls back to naming them.
func (s StickerItem) StickerURL() string {
	switch s.FormatType {
	case 1, 2: // PNG, APNG
		return fmt.Sprintf("%s/stickers/%s.png?size=160", cdnBase, s.ID)
	case 4: // GIF
		return fmt.Sprintf("%s/stickers/%s.gif?size=160", cdnBase, s.ID)
	default: // 3, Lottie
		return ""
	}
}

// --- the objects the gateway caches -----------------------------------------

// Role is a guild role, kept only so that a role mention in a message can be
// rendered as the name a reader would have seen.
type Role struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Channel is a guild channel, kept for the same reason — to turn a channel
// mention into a name — and so the settings screen can list what a link may be
// pointed at.
type Channel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     int    `json:"type"`
	ParentID string `json:"parent_id"`
	Position int    `json:"position"`
}

// Discord channel types a relay can be pointed at. A text channel and an
// announcement channel behave identically for this purpose, a voice channel
// has a text side too, and a thread is addressed exactly like a channel.
const (
	ChannelGuildText          = 0
	ChannelGuildVoice         = 2
	ChannelGuildCategory      = 4
	ChannelGuildAnnouncement  = 5
	ChannelAnnouncementThread = 10
	ChannelPublicThread       = 11
	ChannelPrivateThread      = 12
)

// Writable reports whether a channel is one messages can be relayed through.
func (c Channel) Writable() bool {
	switch c.Type {
	case ChannelGuildText, ChannelGuildVoice, ChannelGuildAnnouncement,
		ChannelAnnouncementThread, ChannelPublicThread, ChannelPrivateThread:
		return true
	default:
		return false
	}
}

// Guild is one Discord server, as much of it as the relay keeps.
type Guild struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Icon        *string   `json:"icon"`
	Roles       []Role    `json:"roles"`
	Channels    []Channel `json:"channels"`
	Threads     []Channel `json:"threads"`
	Unavailable bool      `json:"unavailable"`
}
