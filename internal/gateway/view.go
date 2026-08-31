package gateway

import (
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
	return protocol.User{
		ID:         u.ID,
		Nickname:   u.Nickname,
		Username:   u.Username,
		Registered: u.Registered(),
		Roles:      ids,
		ChannelID:  channelID,
		Online:     online,
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
	}
}
