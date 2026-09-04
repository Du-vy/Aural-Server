package gateway

import (
	"encoding/json"
	"net/url"
	"strconv"

	"github.com/aural-chat/aural-server/internal/buildinfo"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// roleView converts a stored role into its wire form.
func roleView(r store.Role) protocol.Role {
	return protocol.Role{
		ID:          r.ID,
		Name:        r.Name,
		Color:       r.Color,
		Permissions: r.Permissions.String(),
		Position:    r.Position,
		Hoist:       r.Hoist,
		Managed:     r.Managed,
	}
}

// channelView converts a stored channel into its wire form.
func channelView(c store.Channel) protocol.Channel {
	out := protocol.Channel{
		ID:         c.ID,
		ParentID:   c.ParentID,
		Name:       c.Name,
		Type:       c.Type,
		Topic:      c.Topic,
		Position:   c.Position,
		UserLimit:  c.UserLimit,
		Overwrites: make([]protocol.Overwrite, 0, len(c.Overwrites)),
	}
	for _, ow := range c.Overwrites {
		out.Overwrites = append(out.Overwrites, protocol.Overwrite{
			RoleID: ow.RoleID,
			Allow:  ow.Allow.String(),
			Deny:   ow.Deny.String(),
		})
	}
	return out
}

// postView converts a stored post into its wire form.
//
// The body is passed in rather than read here because a listing renders every
// body it needs in one pass, and because the one path that has just written a
// post already holds it.
func postView(p store.Post, body *protocol.Message, stats store.PostStats,
	counts store.PostRSVPCounts, own string) protocol.Post {
	out := protocol.Post{
		ID:        p.ID,
		ChannelID: p.ChannelID,
		UserID:    p.UserID,
		Author:    p.Author,
		Title:     p.Title,
		Locked:    p.Locked,
		Pinned:    p.Pinned,
		CreatedAt: p.CreatedAt,
		EditedAt:  p.EditedAt,
		Body:      body,
		Comments:  stats.Comments,
		// A thread nobody has answered was last active when it was written,
		// which is what keeps a listing sortable by one field.
		LastCommentAt: stats.LastCommentAt,
	}
	if out.LastCommentAt == 0 {
		out.LastCommentAt = p.CreatedAt
	}
	if p.Event() {
		out.Event = &protocol.PostEventDetails{
			StartsAt: *p.StartsAt,
			EndsAt:   p.EndsAt,
			AllDay:   p.AllDay,
			Location: p.Location,
		}
		summary := rsvpView(counts, own)
		out.RSVP = &summary
	}
	return out
}

// rsvpView converts stored tallies and one identity's answer into the wire
// form. An empty own is somebody who has not answered, which is not the same
// as somebody who has declined.
func rsvpView(counts store.PostRSVPCounts, own string) protocol.PostRSVPSummary {
	return protocol.PostRSVPSummary{
		Going:    counts.Going,
		Maybe:    counts.Maybe,
		Declined: counts.Declined,
		Own:      own,
	}
}

// messageView converts a stored message and the files it carries into the wire
// form. A message with no files carries an absent field rather than an empty
// list, which keeps the common frame the size it has always been.
func messageView(m store.Message, attachments []store.Attachment) protocol.Message {
	out := protocol.Message{
		ID:        m.ID,
		ChannelID: m.ChannelID,
		PostID:    m.PostID,
		UserID:    m.UserID,
		Author:    m.Author,
		Content:   m.Content,
		CreatedAt: m.CreatedAt,
		EditedAt:  m.EditedAt,
		Embeds:    decodeEmbeds(m.Embeds),
	}
	if m.WebhookID != nil {
		out.Webhook = &protocol.MessageWebhook{ID: *m.WebhookID, Avatar: m.WebhookAvatar}
	}
	for _, a := range attachments {
		out.Attachments = append(out.Attachments, attachmentView(a))
	}
	return out
}

// decodeEmbeds reads back the JSON array a webhook message stores its cards in.
//
// A row that will not decode is rendered as a message with no cards rather
// than as an error: the words are the message, and one malformed column is not
// a reason to withhold them.
func decodeEmbeds(raw *string) []protocol.Embed {
	if raw == nil || *raw == "" {
		return nil
	}
	var out []protocol.Embed
	if err := json.Unmarshal([]byte(*raw), &out); err != nil {
		return nil
	}
	return out
}

// webhookPath is the URL a delivery is posted to, relative to the server root
// so that every way of reaching this server builds the same working address.
//
// The shape is Discord's, which is the whole point: an application already
// pointed at one only has to be given this instead.
func webhookPath(id int64, token string) string {
	return webhookPrefix + strconv.FormatInt(id, 10) + "/" + token
}

// webhookView converts a stored webhook into its wire form. It carries the
// token, so it only ever goes to somebody who may manage it.
func webhookView(wh store.Webhook) protocol.Webhook {
	return protocol.Webhook{
		ID:         wh.ID,
		ChannelID:  wh.ChannelID,
		Name:       wh.Name,
		Avatar:     wh.Avatar,
		Token:      wh.Token,
		URL:        webhookPath(wh.ID, wh.Token),
		CreatorID:  wh.CreatorID,
		CreatedAt:  wh.CreatedAt,
		LastUsedAt: wh.LastUsedAt,
	}
}

// directMessageView converts one stored private line into its wire form.
//
// It carries no attachments because a private conversation carries no files:
// an upload is bound to the channel it was made for, and there is no channel
// here to bind one to.
func directMessageView(m store.DirectMessage) protocol.DirectMessage {
	return protocol.DirectMessage{
		ID:             m.ID,
		ConversationID: m.ConversationID,
		UserID:         m.UserID,
		Author:         m.Author,
		Content:        m.Content,
		CreatedAt:      m.CreatedAt,
		EditedAt:       m.EditedAt,
	}
}

// attachmentView converts a stored attachment into its wire form.
//
// The URL is relative and carries the filename as its last segment, so a
// browser saving the file gets the name it was uploaded under, and a client
// reaching the server by address, by hostname or through a proxy all build the
// same working link from the address they already hold.
func attachmentView(a store.Attachment) protocol.Attachment {
	return protocol.Attachment{
		ID:          a.ID,
		Filename:    a.Filename,
		ContentType: a.ContentType,
		Size:        strconv.FormatInt(a.Size, 10),
		URL:         uploadPrefix + a.StorageKey + "/" + url.PathEscape(a.Filename),
		Width:       a.Width,
		Height:      a.Height,
	}
}

// overwritesFromView converts wire overwrites back into stored ones, rejecting
// masks that do not parse and roles that do not exist.
func overwritesFromView(list []protocol.Overwrite, roleExists func(int64) bool) ([]permissions.Overwrite, error) {
	out := make([]permissions.Overwrite, 0, len(list))
	for _, ow := range list {
		if !roleExists(ow.RoleID) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "overwrite refers to unknown role")
		}
		allow, err := permissions.Parse(ow.Allow)
		if err != nil {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "overwrite allow mask is not a decimal permission string")
		}
		deny, err := permissions.Parse(ow.Deny)
		if err != nil {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "overwrite deny mask is not a decimal permission string")
		}
		out = append(out, permissions.Overwrite{RoleID: ow.RoleID, Allow: allow, Deny: deny &^ allow})
	}
	return out, nil
}

// userView converts a user plus its live presence into the wire form.
//
// owner is passed in rather than read off the user because ownership is not
// stored on the row: it is a property of the server, which only the hub holds.
func userView(u store.User, roleIDs []int64, channelID *int64, online, owner bool) protocol.User {
	ids := roleIDs
	if ids == nil {
		ids = []int64{}
	}
	status := u.Status
	if status == "" {
		status = "online"
	}
	return protocol.User{
		ID:           u.ID,
		Owner:        owner,
		Nickname:     u.Nickname,
		Username:     u.Username,
		Registered:   u.Registered(),
		Roles:        ids,
		ChannelID:    channelID,
		Online:       online,
		Status:       status,
		CustomStatus: u.CustomStatus,
		Avatar:       u.Avatar,
		Banner:       u.Banner,
		// Only ever reaches its own subject: MaskUser clears it for everybody
		// else, and offlineView never carries one at all.
		DMPrivacy: u.DMPrivacy,
	}
}

// offlineView is how a member who is not connected reaches everybody else.
//
// The stored status is dropped rather than shown: it is what the user picked
// for the next time they are here, not something anyone may act on now. The
// custom status goes with it, because it is the one field a hidden user can
// keep changing while the rest of the server believes they are gone — showing
// the stored value would let a watcher time the change and tell hiding apart
// from being away.
func offlineView(u store.User, roleIDs []int64, owner bool) protocol.User {
	view := userView(u, roleIDs, nil, false, owner)
	view.Status = "offline"
	view.CustomStatus = ""
	view.DMPrivacy = ""
	return view
}

// serverInfo is the public description handed to clients and to GET /info.
func (h *Hub) serverInfo() protocol.ServerInfo {
	cfg := h.cfg
	name, description, icon := h.ServerIdentity()
	return protocol.ServerInfo{
		Name:                name,
		Description:         description,
		Icon:                icon,
		ProtocolVersion:     protocol.Version,
		SoftwareVersion:     buildinfo.Version,
		MaxUsers:            cfg.Server.MaxUsers,
		OnlineUsers:         h.OnlineCount(),
		PasswordProtected:   cfg.Server.Password != "",
		RegistrationEnabled: cfg.Registration.Enabled,
		GuestsAllowed:       cfg.Registration.AllowGuests,
		VoiceMode:           cfg.Voice.Mode,
		Voice:               h.voiceInfo(),
		Uploads:             h.uploadInfo(),
		KlipyEnabled:        h.KlipyAPIKey() != "",
		DirectMessages:      h.DirectMessagesEnabled(),
		Expressions:         h.expressionLimits(),
		Registration: protocol.RegistrationLimits{
			MinPasswordLength: cfg.Registration.MinPasswordLength,
			MinUsernameLength: cfg.Registration.MinUsernameLength,
			MaxUsernameLength: cfg.Registration.MaxUsernameLength,
		},
	}
}

// expressionLimits is what a client is told before it uploads a custom emoji,
// sticker or sound: how many slots are left to fill and how big and how long
// one may be. The trimmer needs the duration before it can cut anything, which
// is why this travels in the preview rather than being discovered by refusal.
func (h *Hub) expressionLimits() protocol.ExpressionLimits {
	cfg := h.cfg.Expressions
	return protocol.ExpressionLimits{
		MaxEmojis:       cfg.MaxEmojis,
		MaxStickers:     cfg.MaxStickers,
		MaxSounds:       cfg.MaxSounds,
		MaxSoundSeconds: cfg.MaxSoundSeconds,
		MaxEmojiBytes:   strconv.FormatInt(cfg.MaxEmojiBytes, 10),
		MaxStickerBytes: strconv.FormatInt(cfg.MaxStickerBytes, 10),
		MaxSoundBytes:   strconv.FormatInt(cfg.MaxSoundBytes, 10),
	}
}

// uploadInfo is what a client is told about attachments before it sends one,
// so a file that is too large is refused in the picker rather than after a
// long transfer.
func (h *Hub) uploadInfo() protocol.Uploads {
	if h.files == nil {
		return protocol.Uploads{
			Enabled:        false,
			MaxFileBytes:   "0",
			MaxAvatarBytes: "0",
			MaxBannerBytes: "0",
			MaxTotalBytes:  "0",
			UsedBytes:      "0",
		}
	}
	return protocol.Uploads{
		Enabled:        true,
		MaxFileBytes:   strconv.FormatInt(h.files.MaxFileBytes(), 10),
		MaxAvatarBytes: strconv.FormatInt(h.cfg.Uploads.MaxAvatarBytes, 10),
		MaxBannerBytes: strconv.FormatInt(h.cfg.Uploads.MaxBannerBytes, 10),
		MaxTotalBytes:  strconv.FormatInt(h.files.MaxTotalBytes(), 10),
		UsedBytes:      strconv.FormatInt(h.files.UsedBytes(), 10),
		MaxPerMessage:  h.cfg.Uploads.MaxPerMessage,
	}
}
