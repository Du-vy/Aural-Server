package gateway

import (
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

// messageView converts a stored message and the files it carries into the wire
// form. A message with no files carries an absent field rather than an empty
// list, which keeps the common frame the size it has always been.
func messageView(m store.Message, attachments []store.Attachment) protocol.Message {
	out := protocol.Message{
		ID:        m.ID,
		ChannelID: m.ChannelID,
		UserID:    m.UserID,
		Author:    m.Author,
		Content:   m.Content,
		CreatedAt: m.CreatedAt,
		EditedAt:  m.EditedAt,
	}
	for _, a := range attachments {
		out.Attachments = append(out.Attachments, attachmentView(a))
	}
	return out
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
func userView(u store.User, roleIDs []int64, channelID *int64, online bool) protocol.User {
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
	}
}

// serverInfo is the public description handed to clients and to GET /info.
func (h *Hub) serverInfo() protocol.ServerInfo {
	cfg := h.cfg
	name, description := h.ServerIdentity()
	return protocol.ServerInfo{
		Name:                name,
		Description:         description,
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
