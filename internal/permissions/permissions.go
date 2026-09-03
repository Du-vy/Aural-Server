// Package permissions defines the permission bitmask and the rules that
// resolve a set of roles, plus optional per-channel overwrites, into the
// effective permissions of one user.
package permissions

import (
	"fmt"
	"strconv"
	"strings"
)

// Permission is a bitmask. It is transported as a decimal string so that a
// 64-bit value survives a JavaScript client, where numbers above 2^53 lose
// precision.
type Permission uint64

const (
	ViewChannel    Permission = 1 << 0 // see a channel and its members
	Connect        Permission = 1 << 1 // join a voice channel
	Speak          Permission = 1 << 2 // transmit in a voice channel
	SendMessages   Permission = 1 << 3 // post in a text channel
	ChangeNickname Permission = 1 << 4
	Register       Permission = 1 << 5 // claim a guest identity as an account
	AttachFiles    Permission = 1 << 6 // post files alongside a message
	// SendDirectMessages covers private conversations, which are between two
	// people rather than in any channel: no overwrite can reach them, so this
	// bit is only ever read from the server-wide mask.
	SendDirectMessages Permission = 1 << 7

	ManageChannels  Permission = 1 << 8
	ManageRoles     Permission = 1 << 9
	ManageServer    Permission = 1 << 10
	ManageNicknames Permission = 1 << 11
	// ManageMessages covers other people's messages. Deleting your own needs
	// no permission at all.
	ManageMessages Permission = 1 << 12
	// ManageWebhooks covers creating, editing and deleting the webhooks of a
	// channel. It is its own bit rather than part of ManageChannels because a
	// webhook URL is a standing permission to post: handing somebody the right
	// to mint one is a larger thing than letting them rename a channel.
	ManageWebhooks Permission = 1 << 13
	// CreatePosts covers starting an entry in a channel that holds them: an
	// announcement, a forum topic, a media item, an event. It is separate from
	// SendMessages because those channels distinguish writing an entry from
	// commenting on one — an announcement channel is exactly the case where
	// everybody may reply and only a few may post — and SendMessages alone
	// could not express that.
	CreatePosts Permission = 1 << 14

	KickUsers   Permission = 1 << 16
	MoveUsers   Permission = 1 << 17
	MuteUsers   Permission = 1 << 18
	DeafenUsers Permission = 1 << 19

	// Administrator bypasses every other check, including channel overwrites.
	Administrator Permission = 1 << 31
)

// None is the empty mask.
const None Permission = 0

// order fixes the presentation order used by Names and by the client UI.
var order = []Permission{
	ViewChannel, Connect, Speak, SendMessages, ChangeNickname, Register, AttachFiles,
	SendDirectMessages,
	CreatePosts,
	ManageChannels, ManageRoles, ManageServer, ManageNicknames, ManageMessages,
	ManageWebhooks,
	KickUsers, MoveUsers, MuteUsers, DeafenUsers,
	Administrator,
}

var names = map[Permission]string{
	ViewChannel:        "ViewChannel",
	Connect:            "Connect",
	Speak:              "Speak",
	SendMessages:       "SendMessages",
	ChangeNickname:     "ChangeNickname",
	Register:           "Register",
	AttachFiles:        "AttachFiles",
	SendDirectMessages: "SendDirectMessages",
	CreatePosts:        "CreatePosts",
	ManageChannels:     "ManageChannels",
	ManageRoles:        "ManageRoles",
	ManageServer:       "ManageServer",
	ManageNicknames:    "ManageNicknames",
	ManageMessages:     "ManageMessages",
	ManageWebhooks:     "ManageWebhooks",
	KickUsers:          "KickUsers",
	MoveUsers:          "MoveUsers",
	MuteUsers:          "MuteUsers",
	DeafenUsers:        "DeafenUsers",
	Administrator:      "Administrator",
}

// All is every defined permission, which is what Administrator resolves to.
var All = func() Permission {
	var p Permission
	for _, bit := range order {
		p |= bit
	}
	return p
}()

// DefaultEveryone is what an unconfigured server grants to every connected
// user, guests included: they can look around, talk, write to somebody
// privately, and claim an account.
const DefaultEveryone = ViewChannel | Connect | Speak | SendMessages | ChangeNickname |
	Register | AttachFiles | SendDirectMessages | CreatePosts

// DefaultRegistered is granted on top of DefaultEveryone once a user claims an
// account. It is deliberately empty so that a fresh server treats guests and
// members alike until an administrator decides otherwise.
const DefaultRegistered = None

// DefaultAdmin is the managed admin role, handed out by redeeming the one-time
// owner token.
const DefaultAdmin = Administrator

// Has reports whether every bit in want is set. Administrator satisfies any
// request.
func (p Permission) Has(want Permission) bool {
	if p&Administrator != 0 {
		return true
	}
	return p&want == want
}

// Names lists the set bits by name, in declaration order.
func (p Permission) Names() []string {
	out := make([]string, 0, len(order))
	for _, bit := range order {
		if p&bit != 0 {
			out = append(out, names[bit])
		}
	}
	return out
}

// String renders the mask as the decimal string used on the wire.
func (p Permission) String() string {
	return strconv.FormatUint(uint64(p), 10)
}

// Parse reads a decimal string mask. An empty string is the empty mask, which
// lets optional wire fields be omitted.
func Parse(s string) (Permission, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return None, nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return None, fmt.Errorf("invalid permission mask %q", s)
	}
	return Permission(v) & All, nil
}

// MustParse is Parse for values that are known-good, such as defaults.
func MustParse(s string) Permission {
	p, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return p
}

// Overwrite is a per-channel adjustment attached to one role.
type Overwrite struct {
	RoleID int64
	Allow  Permission
	Deny   Permission
}

// Resolve computes the server-wide permissions of a user from the roles it
// holds. everyoneMask must be included in roleMasks by the caller.
func Resolve(roleMasks []Permission) Permission {
	var base Permission
	for _, m := range roleMasks {
		base |= m
	}
	if base&Administrator != 0 {
		return All
	}
	return base
}

// ResolveInChannel applies channel overwrites on top of a server-wide mask.
//
// The order matches what Discord users already expect: the everyone overwrite
// is applied first, then the union of the overwrites of every other role the
// user holds. Denies inside one step are applied before allows, so an explicit
// allow on any of the roles wins over a deny on another.
func ResolveInChannel(base Permission, everyoneRoleID int64, userRoleIDs []int64, overwrites []Overwrite) Permission {
	if base&Administrator != 0 {
		return All
	}

	byRole := make(map[int64]Overwrite, len(overwrites))
	for _, ow := range overwrites {
		byRole[ow.RoleID] = ow
	}

	if ow, ok := byRole[everyoneRoleID]; ok {
		base &^= ow.Deny
		base |= ow.Allow
	}

	var allow, deny Permission
	for _, roleID := range userRoleIDs {
		if roleID == everyoneRoleID {
			continue
		}
		if ow, ok := byRole[roleID]; ok {
			allow |= ow.Allow
			deny |= ow.Deny
		}
	}
	base &^= deny
	base |= allow

	// A channel nobody may see is a channel nobody may act in.
	if base&ViewChannel == 0 {
		return None
	}
	return base
}
