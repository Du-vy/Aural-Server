package protocol

// The kinds of identifier a ban is matched on. A ban usually carries several:
// they are of different strengths, and only together do they amount to
// anything against somebody who is willing to make a new account.
const (
	MatchUser   = "user"   // the identity itself
	MatchIP     = "ip"     // the address it connected from
	MatchDevice = "device" // the machine behind it, as this server sees it
)

// Ban is one standing refusal, as a client renders it.
type Ban struct {
	ID int64 `json:"id"`
	// UserID is null once the identity is gone, which is immediately for a
	// guest: banning one deletes the row it named. The captured nickname is
	// what the list is read by.
	UserID        *int64  `json:"userId"`
	UserNickname  string  `json:"userNickname"`
	UserUsername  *string `json:"userUsername"`
	ActorID       *int64  `json:"actorId"`
	ActorNickname string  `json:"actorNickname"`
	Reason        string  `json:"reason"`
	CreatedAt     int64   `json:"createdAt"`
	// ExpiresAt is null for a permanent ban.
	ExpiresAt *int64 `json:"expiresAt"`
	// Matches is what the ban actually catches. The values are never sent:
	// an address and a device hash identify a person outside this server as
	// well as inside it, and a moderator only needs to know that the handle
	// exists, not what it is.
	Matches []BanMatchSummary `json:"matches"`
	// Active is false for a ban whose date has passed. It is kept in the list
	// as a record of what was done.
	Active bool `json:"active"`
}

// BanMatchSummary says that a ban holds a handle of one kind, without saying
// what the handle is.
type BanMatchSummary struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

type BanListRequest struct{}

type BanListResult struct {
	Bans []Ban `json:"bans"`
}

// BanCreateRequest bans somebody. The identity is named; which handles the ban
// picks up is decided by the server from where that identity has connected.
type BanCreateRequest struct {
	UserID int64  `json:"userId"`
	Reason string `json:"reason,omitempty"`
	// Duration is how long the ban lasts, in seconds. Zero is permanent.
	Duration int64 `json:"duration,omitempty"`
	// DeleteMessages purges history, taking the same values user.kick does:
	// "none", "1d", "7d", "30d" or "all".
	DeleteMessages string `json:"deleteMessages,omitempty"`
	// MatchIP and MatchDevice decide how far the ban reaches. Both default to
	// on: a ban that only names an account is one a guest walks straight back
	// through. They are still switches, because an address is often shared —
	// a university, a household, one phone network — and banning it can catch
	// people nobody meant to catch.
	MatchIP     *bool `json:"matchIp,omitempty"`
	MatchDevice *bool `json:"matchDevice,omitempty"`
}

type BanDeleteRequest struct {
	BanID int64 `json:"banId"`
}

type BanEvent struct {
	Ban Ban `json:"ban"`
}

type BanDeletedEvent struct {
	BanID int64 `json:"banId"`
}

// --- the audit log ----------------------------------------------------------

// Audit actions. They are stable strings rather than numbers so that a log
// written by one build reads correctly in the next.
const (
	AuditUserKick       = "user.kick"
	AuditUserBan        = "user.ban"
	AuditUserUnban      = "user.unban"
	AuditUserUpdate     = "user.update"
	AuditRoleCreate     = "role.create"
	AuditRoleUpdate     = "role.update"
	AuditRoleDelete     = "role.delete"
	AuditRoleAssign     = "role.assign"
	AuditRoleUnassign   = "role.unassign"
	AuditChannelCreate  = "channel.create"
	AuditChannelUpdate  = "channel.update"
	AuditChannelDelete  = "channel.delete"
	AuditMessageDelete  = "message.delete"
	AuditPostDelete     = "post.delete"
	AuditServerUpdate   = "server.update"
	AuditOwnerClaim     = "server.claim"
	AuditWebhookCreate  = "webhook.create"
	AuditWebhookDelete  = "webhook.delete"
	AuditExpressionAdd  = "expression.create"
	AuditExpressionEdit = "expression.update"
	AuditExpressionDel  = "expression.delete"
	AuditSoundAdd       = "sound.create"
	AuditSoundEdit      = "sound.update"
	AuditSoundDel       = "sound.delete"
	AuditAutoModUpdate  = "automod.update"
	AuditAutoModAction  = "automod.action"
)

// The kinds of thing an audit entry can be about.
const (
	AuditTargetUser       = "user"
	AuditTargetRole       = "role"
	AuditTargetChannel    = "channel"
	AuditTargetMessage    = "message"
	AuditTargetPost       = "post"
	AuditTargetServer     = "server"
	AuditTargetWebhook    = "webhook"
	AuditTargetExpression = "expression"
	AuditTargetSound      = "sound"
)

// AuditEntry is one line of the log.
type AuditEntry struct {
	ID        int64  `json:"id"`
	ActorID   *int64 `json:"actorId"`
	ActorName string `json:"actorName"`
	Action    string `json:"action"`
	// TargetType and TargetName describe what was acted on, captured as it
	// read at the time: a role deleted last week still has its name here.
	TargetType string        `json:"targetType,omitempty"`
	TargetID   *int64        `json:"targetId,omitempty"`
	TargetName string        `json:"targetName,omitempty"`
	Reason     string        `json:"reason,omitempty"`
	Changes    []AuditChange `json:"changes,omitempty"`
	CreatedAt  int64         `json:"createdAt"`
}

// AuditChange is one field an action altered, rendered rather than typed: the
// log is read by a person, not replayed by a machine.
type AuditChange struct {
	Key    string `json:"key"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// AuditListRequest pages backwards through the log.
type AuditListRequest struct {
	// ActorID narrows to what one moderator did.
	ActorID int64 `json:"actorId,omitempty"`
	// Action narrows to one kind of action.
	Action string `json:"action,omitempty"`
	// Before is the id of the oldest entry already held, exclusive.
	Before int64 `json:"before,omitempty"`
	Limit  int   `json:"limit,omitempty"`
}

type AuditListResult struct {
	Entries []AuditEntry `json:"entries"`
	HasMore bool         `json:"hasMore"`
}

type AuditEntryEvent struct {
	Entry AuditEntry `json:"entry"`
}

// --- automatic moderation ---------------------------------------------------

// What a rule does when it matches.
const (
	// AutoModBlock refuses the message. The writer is told which rule stopped
	// it, because a message that vanishes silently reads as a broken client.
	AutoModBlock = "block"
	// AutoModCensor lets the message through with the offending words masked.
	// It is only offered by rules that have something to mask; a rule about
	// how many people one message mentions has nothing to replace.
	AutoModCensor = "censor"
)

// AutoModConfig is the whole rule set, read and written at once.
//
// Every rule carries its own exempt roles on top of the global list, because
// the two exemptions are genuinely different: staff are usually exempt from
// everything, while one rule — no links, say — is very often lifted for a
// single role that nothing else applies to.
type AutoModConfig struct {
	Enabled bool `json:"enabled"`
	// ExemptRoles are exempt from every rule.
	ExemptRoles []int64 `json:"exemptRoles"`
	// ExemptChannels are channels no rule applies in.
	ExemptChannels []int64 `json:"exemptChannels"`

	Words      AutoModWords      `json:"words"`
	Links      AutoModLinks      `json:"links"`
	Mentions   AutoModMentions   `json:"mentions"`
	Caps       AutoModCaps       `json:"caps"`
	Flood      AutoModFlood      `json:"flood"`
	Repetition AutoModRepetition `json:"repetition"`
}

// AutoModRule is what every rule has: whether it runs, what it does, and who
// it does not apply to.
type AutoModRule struct {
	Enabled bool   `json:"enabled"`
	Action  string `json:"action"`
	// ExemptRoles are exempt from this rule alone.
	ExemptRoles []int64 `json:"exemptRoles"`
}

// AutoModWords is a list of terms nobody may write.
type AutoModWords struct {
	AutoModRule
	// Words are matched against the message folded down the way search folds
	// it: lower case, Latin accents removed. So one entry catches the word
	// however it was capitalised or accented.
	Words []string `json:"words"`
	// WholeWord matches only complete words, which is what stops a list from
	// catching the innocent word that happens to contain a banned one.
	WholeWord bool `json:"wholeWord"`
}

// AutoModLinks refuses URLs, with an allow list for the ones a server lives on.
type AutoModLinks struct {
	AutoModRule
	// AllowedDomains are let through. A leading dot covers subdomains, and an
	// entry without one still matches its own subdomains, because that is what
	// somebody writing "github.com" means.
	AllowedDomains []string `json:"allowedDomains"`
}

// AutoModMentions caps how many people one message may address at once.
type AutoModMentions struct {
	AutoModRule
	Limit int `json:"limit"`
}

// AutoModCaps refuses a message that is mostly capitals.
type AutoModCaps struct {
	AutoModRule
	// Percent is how much of the letters may be upper case before the rule
	// fires. MinLength keeps it off short messages, where "OK" is not shouting.
	Percent   int `json:"percent"`
	MinLength int `json:"minLength"`
}

// AutoModFlood caps how fast one person may write.
type AutoModFlood struct {
	AutoModRule
	Messages int `json:"messages"`
	Seconds  int `json:"seconds"`
}

// AutoModRepetition refuses the same message sent again and again.
type AutoModRepetition struct {
	AutoModRule
	// Times is how many identical messages in a row are allowed.
	Times int `json:"times"`
}

// DefaultAutoMod is what a server that has never configured anything runs.
// Every rule is off: automatic moderation that arrived switched on would be
// moderating a server nobody asked it to.
func DefaultAutoMod() AutoModConfig {
	return AutoModConfig{
		Enabled:        false,
		ExemptRoles:    []int64{},
		ExemptChannels: []int64{},
		Words: AutoModWords{
			AutoModRule: AutoModRule{Action: AutoModCensor, ExemptRoles: []int64{}},
			Words:       []string{},
			WholeWord:   true,
		},
		Links: AutoModLinks{
			AutoModRule:    AutoModRule{Action: AutoModBlock, ExemptRoles: []int64{}},
			AllowedDomains: []string{},
		},
		Mentions: AutoModMentions{
			AutoModRule: AutoModRule{Action: AutoModBlock, ExemptRoles: []int64{}},
			Limit:       5,
		},
		Caps: AutoModCaps{
			AutoModRule: AutoModRule{Action: AutoModBlock, ExemptRoles: []int64{}},
			Percent:     70,
			MinLength:   12,
		},
		Flood: AutoModFlood{
			AutoModRule: AutoModRule{Action: AutoModBlock, ExemptRoles: []int64{}},
			Messages:    5,
			Seconds:     5,
		},
		Repetition: AutoModRepetition{
			AutoModRule: AutoModRule{Action: AutoModBlock, ExemptRoles: []int64{}},
			Times:       3,
		},
	}
}

type AutoModGetRequest struct{}

type AutoModResult struct {
	Config AutoModConfig `json:"config"`
}

// AutoModUpdateRequest replaces the whole rule set.
type AutoModUpdateRequest struct {
	Config AutoModConfig `json:"config"`
}

type AutoModUpdatedEvent struct {
	Config AutoModConfig `json:"config"`
}
