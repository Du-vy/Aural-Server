package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/discord"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// The Discord relay, over the protocol.
//
// Every op here needs ManageServer, and not ManageWebhooks. A link is not a
// channel-scoped thing an administrator of one channel should be able to make:
// it points this whole server at an outside service, carries a bot token
// somebody has to trust, and hands a webhook URL — a standing permission to
// post into somebody else's Discord — to whoever can read the settings screen.
// That is a server-wide decision, so it takes a server-wide permission.

// maxRelayLinks bounds how many pairs one server may hold.
//
// Discord rate-limits a bot's gateway connections and its webhook deliveries,
// and a server bridging fifty channels is a server that will spend its life
// throttled. Twenty-five is far past what a real migration needs and near
// enough to keep the failure legible.
const maxRelayLinks = 25

// requireRelayAdmin is the permission check every op here starts with.
func (s *Session) requireRelayAdmin() *protocol.Error {
	base, _ := s.Permissions()
	if base&permissions.Administrator != 0 || base&permissions.ManageServer != 0 {
		return nil
	}
	return protocol.Errorf(protocol.ErrForbidden, "you may not manage this server")
}

// handleRelayGet answers with the whole relay state: whether it is on, whether
// the bot is connected, which Discord servers it can see, and every link.
func handleRelayGet(ctx context.Context, s *Session, _ json.RawMessage) (any, *protocol.Error) {
	if failure := s.requireRelayAdmin(); failure != nil {
		return nil, failure
	}
	return protocol.RelayEvent{Relay: s.hub.discord.State(ctx)}, nil
}

// handleRelayConfigure switches the relay on or off and sets the bot token.
//
// The token is write-only. A request that omits it keeps the one already
// stored, which is what lets the screen toggle the relay without ever having
// been sent a credential it would then have to hold.
func handleRelayConfigure(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	if failure := s.requireRelayAdmin(); failure != nil {
		return nil, failure
	}
	req, failure := decode[protocol.RelayConfigureRequest](raw)
	if failure != nil {
		return nil, failure
	}

	token := ""
	setToken := false
	if req.BotToken != nil {
		token = strings.TrimSpace(*req.BotToken)
		setToken = true
		if token != "" && !plausibleBotToken(token) {
			return nil, protocol.Errorf(protocol.ErrBadRequest,
				"that does not look like a Discord bot token")
		}
	}
	if req.Enabled && !setToken && s.hub.RelaySettings().BotToken == "" {
		return nil, protocol.Errorf(protocol.ErrBadRequest,
			"the relay needs a bot token before it can be switched on")
	}

	if err := s.hub.updateRelaySettings(req.Enabled, setToken, token); err != nil {
		return nil, internalError(s, "save the relay settings", err)
	}
	s.hub.discord.Restart()

	s.hub.auditRelay(ctx, s, protocol.AuditRelayConfigure, req.Enabled)
	state := s.hub.discord.State(ctx)
	s.hub.BroadcastRelayState(ctx)
	return protocol.RelayEvent{Relay: state}, nil
}

// handleRelayCreate pairs a channel here with one on Discord.
func handleRelayCreate(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	if failure := s.requireRelayAdmin(); failure != nil {
		return nil, failure
	}
	req, failure := decode[protocol.RelayCreateRequest](raw)
	if failure != nil {
		return nil, failure
	}

	if failure := s.requireRelayableChannel(req.ChannelID); failure != nil {
		return nil, failure
	}
	existing, err := s.hub.st.RelayLinks(ctx)
	if err != nil {
		return nil, internalError(s, "list the relay links", err)
	}
	if len(existing) >= maxRelayLinks {
		return nil, protocol.Errorf(protocol.ErrConflict,
			"this server already has as many relay links as it may hold")
	}

	webhookID, token, err := discord.ParseWebhookURL(req.WebhookURL)
	if err != nil {
		return nil, protocol.Errorf(protocol.ErrBadRequest, err.Error())
	}

	direction := strings.TrimSpace(req.Direction)
	if direction == "" {
		direction = store.RelayBoth
	}
	if !store.ValidRelayDirection(direction) {
		return nil, protocol.Errorf(protocol.ErrBadRequest,
			"direction must be \"both\", \"to_aural\" or \"to_discord\"")
	}

	// Discord is asked last, once everything that can be checked here has
	// been: a request that is wrong in a way this server can see should be
	// told so without a round trip to somebody else's API first.
	//
	// The call itself earns its keep. It turns the two most common mistakes —
	// a webhook somebody deleted, and a URL copied from the wrong channel —
	// into a message on the screen rather than a bridge that silently carries
	// nothing.
	info, err := s.hub.discord.restClient().FetchWebhook(ctx, webhookID, token)
	if err != nil {
		return nil, relayWebhookFailure(err)
	}

	discordChannel := strings.TrimSpace(req.DiscordChannelID)
	if discordChannel == "" {
		discordChannel = info.ChannelID
	} else if discordChannel != info.ChannelID {
		return nil, protocol.Errorf(protocol.ErrBadRequest,
			"that webhook posts into a different Discord channel than the one named")
	}

	if _, err := s.hub.st.CreateRelayLink(ctx, store.RelayLink{
		ChannelID:        req.ChannelID,
		DiscordGuildID:   info.GuildID,
		DiscordChannelID: discordChannel,
		WebhookID:        webhookID,
		WebhookToken:     token,
		Direction:        direction,
		Enabled:          true,
		RelayAttachments: req.Attachments,
		RelayEdits:       req.Edits,
	}); errors.Is(err, store.ErrConflict) {
		return nil, protocol.Errorf(protocol.ErrConflict,
			"one of those channels is already bridged")
	} else if err != nil {
		return nil, internalError(s, "create the relay link", err)
	}

	if err := s.hub.discord.reloadLinks(ctx); err != nil {
		return nil, internalError(s, "reload the relay links", err)
	}
	s.hub.auditRelay(ctx, s, protocol.AuditRelayCreate, true)
	s.hub.BroadcastRelayState(ctx)
	return protocol.RelayEvent{Relay: s.hub.discord.State(ctx)}, nil
}

// handleRelayUpdate changes one link. Anything the request leaves out is left
// as it was.
func handleRelayUpdate(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	if failure := s.requireRelayAdmin(); failure != nil {
		return nil, failure
	}
	req, failure := decode[protocol.RelayUpdateRequest](raw)
	if failure != nil {
		return nil, failure
	}

	current, err := s.hub.st.RelayLinkByID(ctx, req.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such relay link")
	}
	if err != nil {
		return nil, internalError(s, "read the relay link", err)
	}

	if req.ChannelID != nil && *req.ChannelID != current.ChannelID {
		if failure := s.requireRelayableChannel(*req.ChannelID); failure != nil {
			return nil, failure
		}
		// The webhooks row belongs to the channel it was made in, so a link
		// pointed at a different channel needs a new one. Clearing it here is
		// what makes the next inbound message provision one in the right place.
		current.ChannelID = *req.ChannelID
		current.SourceWebhookID = nil
	}
	if req.WebhookURL != nil {
		webhookID, token, err := discord.ParseWebhookURL(*req.WebhookURL)
		if err != nil {
			return nil, protocol.Errorf(protocol.ErrBadRequest, err.Error())
		}
		info, err := s.hub.discord.restClient().FetchWebhook(ctx, webhookID, token)
		if err != nil {
			return nil, relayWebhookFailure(err)
		}
		current.WebhookID, current.WebhookToken = webhookID, token
		current.DiscordChannelID, current.DiscordGuildID = info.ChannelID, info.GuildID
	}
	if req.Direction != nil {
		if !store.ValidRelayDirection(*req.Direction) {
			return nil, protocol.Errorf(protocol.ErrBadRequest,
				"direction must be \"both\", \"to_aural\" or \"to_discord\"")
		}
		current.Direction = *req.Direction
	}
	if req.Enabled != nil {
		current.Enabled = *req.Enabled
	}
	if req.Attachments != nil {
		current.RelayAttachments = *req.Attachments
	}
	if req.Edits != nil {
		current.RelayEdits = *req.Edits
	}

	if _, err := s.hub.st.UpdateRelayLink(ctx, current); errors.Is(err, store.ErrConflict) {
		return nil, protocol.Errorf(protocol.ErrConflict,
			"one of those channels is already bridged")
	} else if err != nil {
		return nil, internalError(s, "update the relay link", err)
	}

	if err := s.hub.discord.reloadLinks(ctx); err != nil {
		return nil, internalError(s, "reload the relay links", err)
	}
	s.hub.auditRelay(ctx, s, protocol.AuditRelayUpdate, current.Enabled)
	s.hub.BroadcastRelayState(ctx)
	return protocol.RelayEvent{Relay: s.hub.discord.State(ctx)}, nil
}

// handleRelayDelete unpairs two channels.
//
// What was already relayed stays on both sides. The history is a record of what
// was said, and deleting a bridge is not a decision to unsay it.
func handleRelayDelete(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	if failure := s.requireRelayAdmin(); failure != nil {
		return nil, failure
	}
	req, failure := decode[protocol.RelayDeleteRequest](raw)
	if failure != nil {
		return nil, failure
	}

	if err := s.hub.st.DeleteRelayLink(ctx, req.ID); errors.Is(err, store.ErrNotFound) {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such relay link")
	} else if err != nil {
		return nil, internalError(s, "delete the relay link", err)
	}

	if err := s.hub.discord.reloadLinks(ctx); err != nil {
		return nil, internalError(s, "reload the relay links", err)
	}
	s.hub.auditRelay(ctx, s, protocol.AuditRelayDelete, false)
	s.hub.BroadcastRelayState(ctx)
	return protocol.RelayEvent{Relay: s.hub.discord.State(ctx)}, nil
}

// requireRelayableChannel checks that a link may point at a channel: it has to
// exist, and it has to be a text channel, because a bridge carries messages and
// nothing else does.
func (s *Session) requireRelayableChannel(channelID int64) *protocol.Error {
	channel, ok := s.hub.Channel(channelID)
	if !ok {
		return protocol.Errorf(protocol.ErrNotFound, "no such channel")
	}
	if channel.Type != protocol.ChannelText {
		return protocol.Errorf(protocol.ErrBadRequest, "only a text channel can be bridged")
	}
	return nil
}

// relayWebhookFailure turns a failure talking to Discord into something an
// administrator can act on, rather than a stack of somebody else's HTTP.
func relayWebhookFailure(err error) *protocol.Error {
	switch {
	case errors.Is(err, discord.ErrNotFound):
		return protocol.Errorf(protocol.ErrNotFound,
			"Discord does not have that webhook — it may have been deleted")
	case errors.Is(err, discord.ErrUnauthorized):
		return protocol.Errorf(protocol.ErrForbidden,
			"Discord refused that webhook URL")
	default:
		return protocol.Errorf(protocol.ErrBadRequest,
			"could not reach Discord to check that webhook: "+err.Error())
	}
}

// plausibleBotToken rejects the paste that is not a token at all.
//
// It is a shape check and not a validation: only Discord can say whether a
// token works, and it says so on the first connection. What this catches is the
// common mistake of pasting a client secret, an application id, or a whole
// webhook URL into the token box, where the real answer would otherwise be a
// bridge that silently never connects.
func plausibleBotToken(token string) bool {
	// The bounds are deliberately loose. Discord has changed the length of a
	// token twice, and a false rejection here is worse than a false accept: a
	// token that is wrong is refused by Discord on the first connection and
	// says so on the screen, whereas one refused here cannot be entered at
	// all.
	if len(token) < 50 || len(token) > 120 {
		return false
	}
	if strings.Contains(token, " ") || strings.Contains(token, "/") {
		return false
	}
	// A bot token is three base64url segments separated by full stops.
	return strings.Count(token, ".") == 2
}

// updateRelaySettings writes the relay block and persists it.
func (h *Hub) updateRelaySettings(enabled, setToken bool, token string) error {
	h.cfgMu.Lock()
	h.cfg.Relay.Enabled = enabled
	if setToken {
		h.cfg.Relay.BotToken = token
	}
	// Links live in the database once the server has started, so the copy in
	// the file is a seed rather than a record. Clearing it on the first edit
	// from the settings screen is what stops a restart resurrecting a link
	// somebody deleted here.
	h.cfg.Relay.Links = []config.RelayLink{}
	snapshot := *h.cfg
	h.cfgMu.Unlock()

	if h.cfgPath == "" {
		return nil
	}
	return config.Save(h.cfgPath, snapshot)
}

// auditRelay records a change to the bridge. The bot token is never in it: an
// audit log is read by more people than the settings screen is.
func (h *Hub) auditRelay(ctx context.Context, s *Session, action string, enabled bool) {
	entry := auditTarget(protocol.AuditTargetServer, 0, "relay")
	entry.Action = action
	entry.Changes = []store.AuditChange{{
		Key:   "enabled",
		After: map[bool]string{true: "true", false: "false"}[enabled],
	}}
	h.audit(ctx, s, entry)
}
