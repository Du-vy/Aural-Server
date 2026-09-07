package gateway

import (
	"sync"
	"time"

	"github.com/aural-chat/aural-server/internal/discord"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
)

// The Discord side of a bridged channel's member list.
//
// A bridge that carries what is said but never who is there leaves half the
// room invisible: somebody looking at the sidebar of a bridged channel sees
// only the people who have already moved, and has no way to know whether the
// three who have not are around to answer. That is the gap this closes, and it
// is also what makes tagging somebody on the other side possible — a name has
// to be offerable before it can be typed.
//
// It is a payload of its own rather than more entries in the member list
// proper. A Discord account has no row on this server: no roles, no
// permissions, no private thread, nothing to kick or ban. Folding one into
// Users would mean every op that takes a user id having to answer what it
// means for somebody who is not here, which is a large price for a sidebar.

const (
	// maxRosterMembers bounds what one roster frame carries.
	//
	// The list is ordered with whoever is around first, so a guild past this
	// loses its offline members before its present ones — which is the right
	// end to lose, since the question a sidebar answers is who could reply.
	// Total still says how many there are in all.
	maxRosterMembers = 200

	// rosterBroadcastInterval is the shortest gap between two frames for the
	// same channel.
	//
	// Presence is the noisiest thing Discord sends: on a guild of a few
	// hundred people somebody is going idle every few seconds, and every one of
	// those would otherwise be a roster to every client watching the channel.
	// Coalescing costs a few seconds of staleness on a sidebar, which nobody
	// can perceive, and saves a broadcast storm nobody would forgive.
	rosterBroadcastInterval = 5 * time.Second
)

// rosterPublisher coalesces roster changes into one broadcast per interval.
//
// Discord reports a change per person; a client wants a list. The two are
// reconciled by throwing away everything but the fact that a guild moved, and
// then rebuilding whatever links point at it when the timer comes round.
type rosterPublisher struct {
	relay *discordRelay

	mu sync.Mutex
	// dirty is the guilds that have changed since the last flush.
	dirty map[string]struct{}
	// timer is the pending flush, nil when nothing is waiting.
	timer *time.Timer
}

func newRosterPublisher(relay *discordRelay) *rosterPublisher {
	return &rosterPublisher{relay: relay, dirty: map[string]struct{}{}}
}

// touch records that a guild's people moved and arranges for a flush.
func (p *rosterPublisher) touch(guildID string) {
	if guildID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	p.dirty[guildID] = struct{}{}
	if p.timer != nil {
		return
	}
	p.timer = time.AfterFunc(rosterBroadcastInterval, p.flush)
}

// flush publishes every dirty guild's rosters and clears the slate.
func (p *rosterPublisher) flush() {
	p.mu.Lock()
	guilds := make([]string, 0, len(p.dirty))
	for id := range p.dirty {
		guilds = append(guilds, id)
	}
	p.dirty = map[string]struct{}{}
	p.timer = nil
	p.mu.Unlock()

	for _, guildID := range guilds {
		p.relay.broadcastRoster(guildID)
	}
}

// stop cancels a pending flush. Called when the client goes, so a relay that
// has been switched off does not publish one last roster a minute later.
func (p *rosterPublisher) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.dirty = map[string]struct{}{}
}

// onRoster is what the Discord client calls when somebody in a guild joined,
// left, was renamed, or changed presence.
func (r *discordRelay) onRoster(guildID string) {
	r.rosterPub.touch(guildID)
}

// broadcastRoster sends the current roster of every channel bridged to one
// guild, to everybody who can see that channel.
func (r *discordRelay) broadcastRoster(guildID string) {
	for _, roster := range r.rostersForGuild(guildID) {
		r.hub.BroadcastChannelEvent(
			protocol.Event(protocol.EvRelayRoster, protocol.RelayRosterEvent{Roster: roster}),
			roster.ChannelID)
	}
}

// rostersForGuild builds one roster per link pointing at a guild.
//
// A guild bridged into two channels here produces two rosters with the same
// people in them. That is on purpose: the roster is keyed by the channel it is
// drawn beside, and a client that had to resolve a guild id to a channel would
// be holding a second index of the relay for no reason.
func (r *discordRelay) rostersForGuild(guildID string) []protocol.RelayRoster {
	client := r.current()
	if client == nil {
		return nil
	}

	r.mu.RLock()
	links := make([]int64, 0, len(r.byChannel))
	for channelID, link := range r.byChannel {
		if link.DiscordGuildID == guildID && link.Enabled {
			links = append(links, channelID)
		}
	}
	r.mu.RUnlock()
	if len(links) == 0 {
		return nil
	}

	roster := r.rosterOf(client, guildID)
	out := make([]protocol.RelayRoster, 0, len(links))
	for _, channelID := range links {
		view := roster
		view.ChannelID = channelID
		out = append(out, view)
	}
	return out
}

// Rosters is every bridged channel's Discord side, for the channels a session
// may see. It is what travels in the snapshot.
func (r *discordRelay) Rosters(s *Session) []protocol.RelayRoster {
	client := r.current()
	if client == nil {
		return nil
	}

	r.mu.RLock()
	byGuild := map[string][]int64{}
	for channelID, link := range r.byChannel {
		if link.Enabled && link.DiscordGuildID != "" {
			byGuild[link.DiscordGuildID] = append(byGuild[link.DiscordGuildID], channelID)
		}
	}
	r.mu.RUnlock()

	var out []protocol.RelayRoster
	for guildID, channels := range byGuild {
		roster := r.rosterOf(client, guildID)
		for _, channelID := range channels {
			if failure := r.hub.requireChannelPermission(s, &channelID, permissions.ViewChannel); failure != nil {
				continue
			}
			view := roster
			view.ChannelID = channelID
			out = append(out, view)
		}
	}
	return out
}

// rosterOf renders one guild's members, without a channel: the callers above
// stamp that on, because the same list is drawn beside every channel the guild
// is bridged into.
func (r *discordRelay) rosterOf(client *discord.Client, guildID string) protocol.RelayRoster {
	roster := protocol.RelayRoster{Members: []protocol.RelayMember{}}

	for _, g := range client.Guilds() {
		if g.ID == guildID {
			roster.GuildName = g.Name
			break
		}
	}

	members, total := client.GuildRoster(guildID)
	roster.Total = total
	if len(members) > maxRosterMembers {
		members = members[:maxRosterMembers]
	}
	for _, m := range members {
		roster.Members = append(roster.Members, protocol.RelayMember{
			ID:     m.ID,
			Name:   m.Name,
			Handle: m.Handle,
			Avatar: m.Avatar,
			Bot:    m.Bot,
			Status: m.Status,
		})
	}

	// An empty list with a reason is a different thing from an empty list, and
	// the difference is the one an administrator can act on: the two intents
	// this needs are off by default and have to be switched on by hand.
	if len(roster.Members) == 0 {
		roster.Unavailable = client.RosterDenied()
	}
	return roster
}

// resolveOutboundMentions rewrites the @names in an outgoing message into the
// ids Discord resolves, and returns the ids that may be pinged.
//
// This is the one place a mention is allowed to cross. Everything else is
// deliberately defanged on the way out — see EscapeOutbound and the header of
// allowedMentionsNone — because content coming from here is written by people
// Discord's moderators have no reach over, and an unfiltered bridge hands any
// one of them @everyone on a server they are not in.
//
// What makes this safe is that it is a whitelist built from resolution rather
// than a filter over what somebody typed. Only names that resolve to an actual
// member of the guild on the other side become mentions, only those ids are
// listed as allowed, and @everyone, @here and roles resolve to nothing and so
// can never be reached. The worst a caller can do is ping one person who is in
// the channel they are already talking to.
func (r *discordRelay) resolveOutboundMentions(guildID, text string) (string, []string) {
	if guildID == "" || text == "" {
		return text, nil
	}
	client := r.current()
	if client == nil {
		return text, nil
	}

	var allowed []string
	seen := map[string]struct{}{}

	rewritten := discord.RewriteMentions(text, func(name string) (string, bool) {
		member, ok := client.MemberByName(guildID, name)
		if !ok {
			return "", false
		}
		if _, already := seen[member.ID]; !already {
			seen[member.ID] = struct{}{}
			allowed = append(allowed, member.ID)
		}
		return member.ID, true
	})
	return rewritten, allowed
}
