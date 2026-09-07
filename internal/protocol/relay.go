package protocol

// The Discord relay, as an administrator sees it.
//
// A link is one pair of channels — one here, one on a Discord server — and a
// webhook URL for the direction that goes out. The whole state is sent at
// once, rather than a link at a time, because the screen that renders it needs
// all of it to be useful: which servers the bot can see, which channels are in
// them, which are already spoken for, and whether the bot is connected at all.
//
// None of this leaves the sessions that may manage the server. A webhook URL
// is a standing permission to post into somebody else's channel, which is the
// same reason webhook.list is restricted.

// RelayChannel is one channel on the Discord side, as the picker lists it.
type RelayChannel struct {
	// ID is a Discord snowflake. It is a string for the reason every id from
	// Discord is: the values do not survive a JavaScript number.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Type is Discord's channel type. The client uses it to draw the right
	// icon and to tell a thread from a channel.
	Type int `json:"type"`
	// ParentID is the category or the parent channel, so the picker can group
	// what it lists the way Discord does.
	ParentID string `json:"parentId,omitempty"`
	// Linked is set on a channel some link already points at, so the picker
	// can grey it out rather than let an administrator build a conflict and be
	// refused for it.
	Linked bool `json:"linked,omitempty"`
}

// RelayGuild is one Discord server the bot has been added to.
type RelayGuild struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Icon is an absolute URL on Discord's CDN, or absent.
	Icon     string         `json:"icon,omitempty"`
	Channels []RelayChannel `json:"channels"`
}

// RelayLink is one channel pair.
type RelayLink struct {
	ID        int64 `json:"id"`
	ChannelID int64 `json:"channelId"`
	// DiscordGuildID and DiscordChannelID name the other side. The names
	// beside them are resolved live from what the bot can see, and are absent
	// while it is disconnected or has been removed from that server — which is
	// itself worth showing, so the client renders the id in that case rather
	// than an empty row.
	DiscordGuildID     string `json:"discordGuildId,omitempty"`
	DiscordChannelID   string `json:"discordChannelId"`
	DiscordGuildName   string `json:"discordGuildName,omitempty"`
	DiscordChannelName string `json:"discordChannelName,omitempty"`
	// WebhookURL is rebuilt from the two halves it is stored as. It is a
	// credential, and only reaches somebody who may manage the server.
	WebhookURL string `json:"webhookUrl"`
	// Direction is "both", "to_aural" or "to_discord".
	Direction   string `json:"direction"`
	Enabled     bool   `json:"enabled"`
	Attachments bool   `json:"attachments"`
	Edits       bool   `json:"edits"`
	CreatedAt   int64  `json:"createdAt"`
	// LastRelayedAt is zero until the first message crosses, which is what
	// tells an administrator whether a link was ever wired up at all.
	LastRelayedAt int64 `json:"lastRelayedAt"`
	// LastError is the last failure this link hit, so a broken bridge explains
	// itself on the screen rather than only in the log.
	LastError string `json:"lastError,omitempty"`
}

// RelayState is the whole of it.
type RelayState struct {
	Enabled bool `json:"enabled"`
	// Configured is whether a bot token has been set. The token itself is
	// never sent back: it is a password, and a screen that can display one is
	// a screen that leaks it.
	Configured bool `json:"configured"`
	// Connected is whether a gateway session is up right now.
	Connected bool `json:"connected"`
	// BotName and BotID are the account the token belongs to, learned when it
	// connects. They are how an administrator confirms the token is the one
	// they meant to paste.
	BotName string `json:"botName,omitempty"`
	BotID   string `json:"botId,omitempty"`
	// Error is why the relay is not connected, in the words the failure came
	// in. An unset intent says so here.
	Error string `json:"error,omitempty"`
	// RosterError is why the member list has no Discord side, when the bridge
	// itself is fine. It is separate from Error because the two are different
	// severities: this one means everything works except a sidebar, and it is
	// almost always the two privileged intents not being switched on.
	RosterError string       `json:"rosterError,omitempty"`
	Guilds      []RelayGuild `json:"guilds"`
	Links       []RelayLink  `json:"links"`
}

// RelayConfigureRequest switches the relay on and sets the bot token.
type RelayConfigureRequest struct {
	Enabled bool `json:"enabled"`
	// BotToken is optional on an update: a request that omits it keeps the
	// token already stored, which is what lets the screen toggle the relay
	// without holding a credential it was never sent.
	BotToken *string `json:"botToken,omitempty"`
}

// RelayCreateRequest pairs a channel here with one on Discord.
type RelayCreateRequest struct {
	ChannelID int64 `json:"channelId"`
	// WebhookURL is minted in the Discord channel's own integration settings.
	// It names the channel by itself, so DiscordChannelID may be left out and
	// is only read when it is given.
	WebhookURL       string `json:"webhookUrl"`
	DiscordChannelID string `json:"discordChannelId,omitempty"`
	Direction        string `json:"direction,omitempty"`
	Attachments      bool   `json:"attachments"`
	Edits            bool   `json:"edits"`
}

// RelayUpdateRequest changes one link. Every field but the id is optional, and
// an omitted one is left as it was.
type RelayUpdateRequest struct {
	ID          int64   `json:"id"`
	ChannelID   *int64  `json:"channelId,omitempty"`
	WebhookURL  *string `json:"webhookUrl,omitempty"`
	Direction   *string `json:"direction,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
	Attachments *bool   `json:"attachments,omitempty"`
	Edits       *bool   `json:"edits,omitempty"`
}

// RelayDeleteRequest unpairs two channels.
type RelayDeleteRequest struct {
	ID int64 `json:"id"`
}

// RelayEvent carries the whole state after a change.
type RelayEvent struct {
	Relay RelayState `json:"relay"`
}

// The roster: who is on the Discord side of a bridged channel.
//
// Unlike everything above it, this is not administration. It reaches every
// session that can see the channel, because it is drawn in the member list
// beside the people who are actually here — which is the whole point of it. A
// bridged room whose sidebar shows only the half of the conversation that has
// already moved reads as a room where messages arrive from nowhere.
//
// Nothing here is an Aural identity, and it is deliberately a separate payload
// rather than more entries in Users. A Discord account has no row on this
// server, no roles, no permissions and no private thread; folding one into the
// member list proper would mean every op that takes a user id having to answer
// what it means for somebody who is not here.

// RelayMember is one person on the Discord side.
type RelayMember struct {
	// ID is a Discord snowflake, and a string for the reason every id from
	// Discord is: the values do not survive a JavaScript number.
	ID string `json:"id"`
	// Name is what a reader sees: the per-guild nickname if they set one, then
	// their display name, then their handle.
	Name string `json:"name"`
	// Handle is the unique @handle. It is what a mention has to be spelled as
	// to be sure of reaching the right account, so it travels alongside the
	// name even though it is usually the same thing.
	Handle string `json:"handle,omitempty"`
	// Avatar is an absolute URL on Discord's CDN.
	Avatar string `json:"avatar,omitempty"`
	Bot    bool   `json:"bot,omitempty"`
	// Status is "online", "idle", "dnd" or "offline". Somebody invisible is
	// reported offline, which is what being invisible is for.
	Status string `json:"status"`
}

// RelayRoster is the Discord side of one bridged channel.
type RelayRoster struct {
	// ChannelID is the channel here, not the one on Discord: it is what the
	// member list is keyed by.
	ChannelID int64         `json:"channelId"`
	GuildName string        `json:"guildName,omitempty"`
	Members   []RelayMember `json:"members"`
	// Total is how many members that Discord server has in all. It is larger
	// than the list when a big guild has been trimmed to what a sidebar can
	// usefully draw, and the client says "and N more" rather than implying the
	// list is everybody.
	Total int `json:"total"`
	// Unavailable is why there is nobody in the list, when the reason is
	// worth showing — most often the two privileged intents this needs not
	// being switched on for the bot. Empty when the roster is simply empty.
	Unavailable string `json:"unavailable,omitempty"`
}

// RelayRosterEvent carries one channel's roster after it changed.
type RelayRosterEvent struct {
	Roster RelayRoster `json:"roster"`
}
