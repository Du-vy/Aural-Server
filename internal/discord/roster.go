package discord

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"strings"
)

// Who is on the Discord side.
//
// The bridge on its own answers "what was said there" and never "who is
// there", which is the gap this closes: a channel bridged into a room whose
// member list shows only the people who have already moved reads as a room
// half the conversation is coming from nowhere.
//
// Two things make it awkward, and both are Discord's rather than ours.
//
// The first is that both intents this needs are privileged. An administrator
// has to switch them on in the developer portal, and Discord refuses the whole
// connection — bridge included — if they identify without them. So the client
// asks for them, and gives them up rather than losing the bridge; see
// RosterIntents and classifyClose.
//
// The second is that GUILD_CREATE does not carry every member. It carries up
// to large_threshold of them, and the rest have to be asked for over the
// gateway and arrive in chunks. That is what requestMembers and the
// GUILD_MEMBERS_CHUNK case are for.

// maxRosterMembers bounds what one guild may hold in memory.
//
// A bridge is for a community that is moving, which is not a hundred-thousand
// member server, and a roster is drawn in a sidebar. Past this the list stops
// growing and the roster says it is partial rather than the process quietly
// spending a gigabyte on names nobody will scroll to.
const maxRosterMembers = 10_000

// largeThreshold is how many members Discord sends with GUILD_CREATE before it
// starts leaving them out. 250 is its ceiling, and asking for it is what keeps
// the usual guild from needing a chunk request at all.
const largeThreshold = 250

// rosterAvatarSize is the picture size asked of Discord's CDN for a member
// list. It matches what a relayed message's author is fetched at, so the two
// share a cache entry for the same person.
const rosterAvatarSize = 128

// Presence statuses, as Discord spells them. "invisible" never reaches a bot:
// it is reported as offline, which is the whole point of it.
const (
	StatusOnline  = "online"
	StatusIdle    = "idle"
	StatusDND     = "dnd"
	StatusOffline = "offline"
)

// GuildMember is one person on the Discord side, as a member list draws them.
type GuildMember struct {
	ID string
	// Name is what a reader sees: the per-guild nickname, then the account's
	// display name, then the bare handle — the same order a message's author
	// is resolved in, so the same person reads the same way in both places.
	Name string
	// Handle is the unique @handle. It is what a mention has to be written as,
	// because it is the one spelling of a Discord account that never contains
	// a space and never collides.
	Handle string
	// Avatar is an absolute URL on Discord's CDN. Never empty: an account with
	// no picture resolves to the default Discord serves for it.
	Avatar string
	Bot    bool
	// Status is one of the four above. Somebody with no presence at all is
	// offline, which is also what a guild that never sent one reports.
	Status string
}

// Online reports whether this member counts as around. Idle and do-not-disturb
// do: they are somebody who is there and has said something about how much
// attention they have, which is not the same as being away.
func (m GuildMember) Online() bool { return m.Status != "" && m.Status != StatusOffline }

// guildRoster is one guild's members and how complete the set is.
type guildRoster struct {
	members map[string]GuildMember
	// total is what Discord says the guild holds, which is not always what is
	// held here: a guild past maxRosterMembers is kept in part.
	total int
}

// rawMember is the member object as it arrives, in GUILD_CREATE, in a chunk,
// and in GUILD_MEMBER_ADD/UPDATE alike.
type rawMember struct {
	User   User    `json:"user"`
	Nick   *string `json:"nick"`
	Avatar *string `json:"avatar"`
}

// rawPresence is the presence object, which names only the account it is about.
type rawPresence struct {
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Status string `json:"status"`
}

// member turns one raw member into what is cached, resolving the name and the
// picture the same way a relayed message resolves its author's.
func (r rawMember) member(guildID string) GuildMember {
	name := r.User.Username
	if display := strings.TrimSpace(r.User.GlobalName); display != "" {
		name = display
	}
	if r.Nick != nil && strings.TrimSpace(*r.Nick) != "" {
		name = strings.TrimSpace(*r.Nick)
	}

	avatar := r.User.AvatarURL(rosterAvatarSize)
	if r.Avatar != nil && *r.Avatar != "" && guildID != "" {
		avatar = guildAvatarURL(guildID, r.User.ID, *r.Avatar, rosterAvatarSize)
	}

	return GuildMember{
		ID:     r.User.ID,
		Name:   name,
		Handle: r.User.Username,
		Avatar: avatar,
		Bot:    r.User.Bot,
		Status: StatusOffline,
	}
}

// GuildRoster is everybody the bot can see in one guild, ordered the way a
// member list draws them: whoever is around first, then by name.
//
// total is what Discord says the guild holds, so a caller can say "and 900
// more" when the cache is capped.
func (c *Client) GuildRoster(guildID string) (members []GuildMember, total int) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	roster, ok := c.rosters[guildID]
	if !ok {
		return nil, 0
	}
	out := make([]GuildMember, 0, len(roster.members))
	for _, m := range roster.members {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online() != out[j].Online() {
			return out[i].Online()
		}
		if !strings.EqualFold(out[i].Name, out[j].Name) {
			return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
		}
		return out[i].ID < out[j].ID
	})
	total = roster.total
	if total < len(out) {
		total = len(out)
	}
	return out, total
}

// MemberByName resolves what somebody typed after an @ to an account in one
// guild, for turning a mention written here into one Discord will resolve.
//
// The handle is tried first and is the only spelling that is guaranteed to be
// unique, so it is also the only one a collision can be decided in favour of.
// A display name is tried second, matched without regard to case, and only
// when exactly one member answers to it: two people called "Sam" are two people
// nobody can safely be pinged as.
func (c *Client) MemberByName(guildID, name string) (GuildMember, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return GuildMember{}, false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	roster, ok := c.rosters[guildID]
	if !ok {
		return GuildMember{}, false
	}
	var byName GuildMember
	matches := 0
	for _, m := range roster.members {
		if strings.EqualFold(m.Handle, name) {
			return m, true
		}
		if strings.EqualFold(m.Name, name) {
			byName = m
			matches++
		}
	}
	if matches == 1 {
		return byName, true
	}
	return GuildMember{}, false
}

// --- keeping it current ------------------------------------------------------

// cacheRoster records the members and presences that came with a guild, or
// with one chunk of one.
func (c *Client) cacheRoster(guildID string, members []rawMember, presences []rawPresence, total int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	roster, ok := c.rosters[guildID]
	if !ok {
		roster = &guildRoster{members: map[string]GuildMember{}}
		c.rosters[guildID] = roster
	}
	if total > roster.total {
		roster.total = total
	}

	for _, raw := range members {
		if raw.User.ID == "" {
			continue
		}
		// Past the cap, only people already known are updated. Dropping the
		// new ones rather than the old keeps the list stable instead of
		// churning it on every chunk.
		if _, known := roster.members[raw.User.ID]; !known && len(roster.members) >= maxRosterMembers {
			continue
		}
		next := raw.member(guildID)
		// A member frame says nothing about presence, so whatever is known
		// about it survives the update.
		if previous, known := roster.members[raw.User.ID]; known {
			next.Status = previous.Status
		}
		roster.members[raw.User.ID] = next
	}

	for _, p := range presences {
		member, known := roster.members[p.User.ID]
		if !known {
			// A presence for somebody the member list has not reached. There
			// is no name to draw them with, so it is dropped rather than
			// guessed at; the chunk carrying them will bring it.
			continue
		}
		member.Status = normaliseStatus(p.Status)
		roster.members[p.User.ID] = member
	}
}

// forgetRoster drops a guild's members along with the guild.
func (c *Client) forgetRoster(guildID string) {
	c.mu.Lock()
	delete(c.rosters, guildID)
	c.mu.Unlock()
}

// dropMember removes somebody who left the guild.
func (c *Client) dropMember(guildID, userID string) {
	c.mu.Lock()
	if roster, ok := c.rosters[guildID]; ok {
		delete(roster.members, userID)
		if roster.total > 0 {
			roster.total--
		}
	}
	c.mu.Unlock()
}

// normaliseStatus maps what Discord sends to the four this package uses.
// Anything unrecognised is offline, which is the safe reading: claiming
// somebody is around when they may not be is the wrong way to be wrong.
func normaliseStatus(status string) string {
	switch status {
	case StatusOnline, StatusIdle, StatusDND:
		return status
	default:
		return StatusOffline
	}
}

// requestMembers asks the gateway for the members GUILD_CREATE left out.
//
// Discord sends up to large_threshold of them with the guild and expects the
// rest to be asked for, in chunks, over the same socket. A limit of zero with
// an empty query means "all of them", which is only allowed with the members
// intent — the one this whole file is conditional on.
func (c *Client) requestMembers(ctx context.Context, guildID string) {
	if err := c.send(ctx, opRequestGuildMembers, map[string]any{
		"guild_id":  guildID,
		"query":     "",
		"limit":     0,
		"presences": true,
	}); err != nil {
		c.log.Warn("ask discord for the rest of a guild's members",
			slog.String("guild", guildID), slog.Any("error", err))
	}
}

// rosterEvent handles the roster half of the dispatch table. It reports
// whether it recognised the event, so the main switch can fall through.
func (c *Client) rosterEvent(f frame) bool {
	switch f.T {
	case "GUILD_MEMBERS_CHUNK":
		var payload struct {
			GuildID   string        `json:"guild_id"`
			Members   []rawMember   `json:"members"`
			Presences []rawPresence `json:"presences"`
		}
		if err := json.Unmarshal(f.D, &payload); err != nil || payload.GuildID == "" {
			return true
		}
		c.cacheRoster(payload.GuildID, payload.Members, payload.Presences, 0)
		c.rosterChanged(payload.GuildID)

	case "GUILD_MEMBER_ADD", "GUILD_MEMBER_UPDATE":
		var payload struct {
			GuildID string `json:"guild_id"`
			rawMember
		}
		if err := json.Unmarshal(f.D, &payload); err != nil || payload.GuildID == "" {
			return true
		}
		total := 0
		if f.T == "GUILD_MEMBER_ADD" {
			// Nudges the total up with the arrival, so a capped roster's "and
			// N more" keeps counting.
			c.mu.RLock()
			if roster, ok := c.rosters[payload.GuildID]; ok {
				total = roster.total + 1
			}
			c.mu.RUnlock()
		}
		c.cacheRoster(payload.GuildID, []rawMember{payload.rawMember}, nil, total)
		c.rosterChanged(payload.GuildID)

	case "GUILD_MEMBER_REMOVE":
		var payload struct {
			GuildID string `json:"guild_id"`
			User    User   `json:"user"`
		}
		if err := json.Unmarshal(f.D, &payload); err != nil || payload.GuildID == "" {
			return true
		}
		c.dropMember(payload.GuildID, payload.User.ID)
		c.rosterChanged(payload.GuildID)

	case "PRESENCE_UPDATE":
		var payload struct {
			GuildID string `json:"guild_id"`
			rawPresence
		}
		if err := json.Unmarshal(f.D, &payload); err != nil || payload.GuildID == "" {
			return true
		}
		c.cacheRoster(payload.GuildID, nil, []rawPresence{payload.rawPresence}, 0)
		c.rosterChanged(payload.GuildID)

	default:
		return false
	}
	return true
}

// rosterChanged tells the caller something about a guild's people moved. It is
// deliberately coarse — the guild, not the person — because the caller
// broadcasts a whole roster anyway, and on a busy guild the alternative is one
// notification per presence flicker.
func (c *Client) rosterChanged(guildID string) {
	if c.handlers.RosterChanged != nil {
		c.handlers.RosterChanged(guildID)
	}
}

// guildAvatarURL is the per-guild picture, which overrides the account's.
func guildAvatarURL(guildID, userID, hash string, size int) string {
	return cdnBase + "/guilds/" + guildID + "/users/" + userID + "/avatars/" + hash +
		"." + avatarExt(hash) + "?size=" + strconv.Itoa(size)
}
