package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aural-chat/aural-server/internal/auth"
	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/discord"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// The Discord relay: a bridge that carries one text channel here to one text
// channel on a Discord server, and back.
//
// It exists because communities do not move all at once. Some people leave
// Discord the day a server opens here and some never do, and in between there
// is a stretch — weeks, usually — where a conversation split across two
// applications is the thing that decides whether the move sticks. A bridge is
// what makes that stretch survivable: the people who moved talk here, the
// people who have not talk there, and neither group is talking into a room
// with nobody in it.
//
// A message is impersonated rather than attributed to a bot. Discord's side is
// a webhook, which lets one URL post under a different name and picture for
// every message; this side is a webhook row, which the store already carries
// per-message names and avatars for. So a bridged conversation reads as the
// people in it on both ends, and that is not a cosmetic detail — a channel
// where forty messages in a row are posted by "Relay Bot" is a channel nobody
// reads.
//
// Nothing here is confused with the voice relay, which is the other thing this
// package calls a relay: that one forwards RTP between people in a voice
// channel and shares nothing with this but the word.
//
// # Not looping
//
// The failure this design is most exposed to is the obvious one: a message
// crosses to Discord, comes back as a new message, crosses again, and the two
// servers fill each other up until somebody notices. Guarding it by comparing
// content would be a guess, and would break the moment somebody quotes
// themselves.
//
// Instead each side tags what it sends, with an identity rather than a
// heuristic:
//
//   - Outbound messages go through a Discord webhook whose id this server
//     knows, because it parsed it out of the URL an administrator pasted. When
//     that message arrives back over the gateway it carries webhook_id, and
//     that field being one of ours is proof it is our own echo.
//   - Inbound messages are written under a webhooks row this relay owns. When
//     the outbound side looks at a new message and finds that row's id in
//     webhook_id, it is looking at something it wrote itself.
//
// Both are exact. Neither can be defeated by what a message says.

// relayQueueDepth is how many pending deliveries one link may hold.
//
// The queue exists because the Discord gateway's read loop must never block:
// an event handler that waits on an HTTP call stalls every other event on the
// connection, heartbeats included. A full queue drops the message and says so,
// which is worse than delivering it late and better than wedging the socket.
const relayQueueDepth = 256

// relayDeliveryTimeout bounds one delivery, downloads included.
const relayDeliveryTimeout = 2 * time.Minute

// relayAvatarSize is the picture size asked of Discord's CDN. Big enough for a
// retina avatar, small enough that it is not a photograph.
const relayAvatarSize = 128

// relayInboundWebhookName is what the webhooks row created for a link is
// called. It is only ever seen by an administrator reading the channel's
// webhook list, where it should be obvious what made it and why.
const relayInboundWebhookName = "Discord"

// relayJob is one unit of work on a link's queue.
type relayJob func(ctx context.Context)

// discordRelay bridges Discord channels to Aural channels.
type discordRelay struct {
	hub *Hub
	st  *store.Store
	log *slog.Logger

	// mu guards everything below it. The maps are rebuilt wholesale on a
	// change rather than mutated, so a reader holding one is holding a
	// consistent set.
	mu     sync.RWMutex
	client *discord.Client
	// byChannel and byDiscord are the same links indexed the two ways they are
	// looked up: by the Aural channel a message was written in, and by the
	// Discord channel one arrived from.
	byChannel map[int64]store.RelayLink
	byDiscord map[string]store.RelayLink
	// ownWebhooks is every Discord webhook id this relay posts through. It is
	// the outbound half of the loop guard, and is a set rather than a per-link
	// field so that a message arriving in a channel whose link was just
	// repointed is still recognised as ours.
	ownWebhooks map[string]struct{}
	// inboundWebhooks is every webhooks row this relay writes under, which is
	// the inbound half of the same guard.
	inboundWebhooks map[int64]struct{}
	// cancel stops the current client. Nil when nothing is running.
	cancel context.CancelFunc
	// queues are the per-link delivery goroutines. One per link keeps the
	// order of a channel intact — messages arrive in the order they were sent
	// — while stopping one slow link from holding up the others.
	queues map[int64]chan relayJob
	// stopped is closed when the relay shuts down, which is what releases the
	// queue goroutines.
	stopped chan struct{}
	// root is the server's own lifetime, captured by Run. A client started
	// later — by an administrator switching the relay on from the settings
	// screen — hangs off this rather than off the request that started it, so
	// that it still goes away when the server does and does not outlive the
	// process as an orphaned goroutine.
	root context.Context
}

// newDiscordRelay builds the bridge. It does not connect: Start does, and only
// once the configuration says to.
func newDiscordRelay(hub *Hub) *discordRelay {
	return &discordRelay{
		hub:             hub,
		st:              hub.st,
		log:             hub.log.With(slog.String("component", "discord-relay")),
		byChannel:       map[int64]store.RelayLink{},
		byDiscord:       map[string]store.RelayLink{},
		ownWebhooks:     map[string]struct{}{},
		inboundWebhooks: map[int64]struct{}{},
		queues:          map[int64]chan relayJob{},
		stopped:         make(chan struct{}),
	}
}

// --- lifecycle --------------------------------------------------------------

// Run brings the relay up and keeps it up until ctx ends.
//
// It returns immediately on a server with the relay switched off, and it never
// returns an error: a bridge that cannot connect is a degraded server, not a
// broken one, and refusing to start over it would take the chat down with it.
func (r *discordRelay) Run(ctx context.Context) {
	defer close(r.stopped)

	r.mu.Lock()
	r.root = ctx
	r.mu.Unlock()

	if err := r.seedLinksFromConfig(ctx); err != nil {
		r.log.Error("apply the relay links from the configuration file", slog.Any("error", err))
	}
	if err := r.reloadLinks(ctx); err != nil {
		r.log.Error("load the relay links", slog.Any("error", err))
	}

	settings := r.hub.RelaySettings()
	if settings.Enabled && settings.BotToken != "" {
		r.start(ctx, settings.BotToken)
	} else {
		r.log.Debug("discord relay is switched off")
	}

	// Held open either way. The relay can be switched on from the settings
	// screen while the server runs, and the stop below is what tears down a
	// client started that way when the process ends.
	<-ctx.Done()
	r.stop()
}

// start opens a client and runs it until its own context ends.
func (r *discordRelay) start(parent context.Context, token string) {
	r.stop()

	ctx, cancel := context.WithCancel(parent)
	client := discord.NewClient(discord.Options{
		Token: token,
		Log:   r.log,
		Handlers: discord.Handlers{
			Ready:          r.onReady,
			GuildAvailable: r.onGuild,
			Message:        func(m discord.Message) { r.onDiscordMessage(ctx, m) },
			MessageEdited:  func(m discord.Message) { r.onDiscordEdit(ctx, m) },
			MessageDeleted: func(channelID string, ids []string) { r.onDiscordDelete(ctx, channelID, ids) },
		},
	})

	r.mu.Lock()
	r.client = client
	r.cancel = cancel
	r.mu.Unlock()

	go func() {
		err := client.Run(ctx)
		r.mu.Lock()
		// Only forget it if this is still the current client: a restart has
		// already replaced the field, and stomping on it would report the new
		// connection as dead.
		if r.client == client {
			r.client = nil
		}
		r.mu.Unlock()

		if err != nil && ctx.Err() == nil {
			r.log.Error("discord relay stopped", slog.Any("error", err))
			r.hub.BroadcastRelayState(context.WithoutCancel(ctx))
		}
	}()
}

// stop tears the current client down, if there is one.
func (r *discordRelay) stop() {
	r.mu.Lock()
	cancel := r.cancel
	r.cancel, r.client = nil, nil
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

// Restart applies a change to the bot token or the enabled flag.
//
// It takes no context on purpose: the connection belongs to the server's
// lifetime, not to the request that asked for it, and a client hung off a
// request context would either be cancelled the moment the reply was written
// or, uncancelled, outlive the process.
func (r *discordRelay) Restart() {
	r.mu.RLock()
	root := r.root
	r.mu.RUnlock()
	if root == nil {
		// Run has not started yet, which on a live server cannot happen: the
		// settings screen is reachable only over a connection this server is
		// already serving.
		return
	}

	settings := r.hub.RelaySettings()
	if !settings.Enabled || settings.BotToken == "" {
		r.stop()
		return
	}
	r.start(root, settings.BotToken)
}

// client reads the connected client, which is nil while the relay is off.
func (r *discordRelay) current() *discord.Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.client
}

// --- links ------------------------------------------------------------------

// reloadLinks rebuilds the caches and the loop-guard sets from the database.
// Every write to relay_links calls it.
func (r *discordRelay) reloadLinks(ctx context.Context) error {
	links, err := r.st.RelayLinks(ctx)
	if err != nil {
		return err
	}
	r.setLinks(links)
	return nil
}

// setLinks swaps in a new set of links and the guard sets derived from it.
//
// It is separate from reloadLinks so that the cache swap can be exercised
// without a database: this is the one piece of the relay that a read on the
// Discord read loop and a write from an administrator's request reach at the
// same moment.
func (r *discordRelay) setLinks(links []store.RelayLink) {
	byChannel := make(map[int64]store.RelayLink, len(links))
	byDiscord := make(map[string]store.RelayLink, len(links))
	own := make(map[string]struct{}, len(links))
	inbound := make(map[int64]struct{}, len(links))
	for _, l := range links {
		byChannel[l.ChannelID] = l
		byDiscord[l.DiscordChannelID] = l
		// The guard sets deliberately ignore l.Enabled. A link switched off
		// stops relaying, but a message already in flight through it must
		// still be recognised as ours rather than picked up as new traffic.
		own[l.WebhookID] = struct{}{}
		if l.SourceWebhookID != nil {
			inbound[*l.SourceWebhookID] = struct{}{}
		}
	}

	r.mu.Lock()
	r.byChannel, r.byDiscord = byChannel, byDiscord
	r.ownWebhooks, r.inboundWebhooks = own, inbound
	r.mu.Unlock()
}

// linkForDiscord finds the link a Discord channel belongs to.
func (r *discordRelay) linkForDiscord(channelID string) (store.RelayLink, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	l, ok := r.byDiscord[channelID]
	return l, ok
}

// linkForChannel finds the link an Aural channel belongs to.
func (r *discordRelay) linkForChannel(channelID int64) (store.RelayLink, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	l, ok := r.byChannel[channelID]
	return l, ok
}

// isOwnEcho reports whether a Discord message is one this server posted.
func (r *discordRelay) isOwnEcho(m discord.Message) bool {
	if m.WebhookID == "" {
		return false
	}
	r.mu.RLock()
	_, ours := r.ownWebhooks[m.WebhookID]
	r.mu.RUnlock()
	return ours
}

// isInboundMessage reports whether an Aural message is one this relay wrote,
// which is what stops it being sent back to the server it came from.
func (r *discordRelay) isInboundMessage(m store.Message) bool {
	if m.WebhookID == nil {
		return false
	}
	r.mu.RLock()
	_, ours := r.inboundWebhooks[*m.WebhookID]
	r.mu.RUnlock()
	return ours
}

// seedLinksFromConfig copies the links named in the configuration file into
// the database, once.
//
// It is what lets a container be deployed already bridged. A link is matched by
// its Discord channel, so editing the file changes the link it created rather
// than adding a second one, and a link an administrator later edits in the
// settings screen is not overwritten on the next restart unless the file names
// it differently.
func (r *discordRelay) seedLinksFromConfig(ctx context.Context) error {
	settings := r.hub.RelaySettings()
	if len(settings.Links) == 0 {
		return nil
	}

	for i, want := range settings.Links {
		webhookID, token, err := discord.ParseWebhookURL(want.WebhookURL)
		if err != nil {
			r.log.Error("relay.links webhook URL is unusable",
				slog.Int("index", i), slog.Any("error", err))
			continue
		}

		discordChannel := want.DiscordChannelID
		if discordChannel == "" {
			// A webhook belongs to exactly one channel, so Discord can be
			// asked which. It costs one call on startup and saves an
			// administrator hunting for an id with developer mode on.
			info, err := r.restClient().FetchWebhook(ctx, webhookID, token)
			if err != nil {
				r.log.Error("relay.links webhook could not be read from Discord",
					slog.Int("index", i), slog.Any("error", err))
				continue
			}
			discordChannel = info.ChannelID
		}

		link := store.RelayLink{
			ChannelID:        want.ChannelID,
			DiscordChannelID: discordChannel,
			WebhookID:        webhookID,
			WebhookToken:     token,
			Direction:        want.Direction,
			Enabled:          true,
			RelayAttachments: want.Attachments == nil || *want.Attachments,
			RelayEdits:       want.Edits == nil || *want.Edits,
		}
		if link.Direction == "" {
			link.Direction = store.RelayBoth
		}

		existing, err := r.st.RelayLinkByDiscordChannel(ctx, discordChannel)
		switch {
		case errors.Is(err, store.ErrNotFound):
			if _, err := r.st.CreateRelayLink(ctx, link); err != nil {
				r.log.Error("create the relay link named in the configuration file",
					slog.Int("index", i), slog.Any("error", err))
				continue
			}
			r.log.Info("relay link created from the configuration file",
				slog.Int64("channel", link.ChannelID), slog.String("discord", discordChannel))
		case err != nil:
			return err
		default:
			// Carry across what the file does not describe, so a re-seed does
			// not throw away the webhooks row the link has provisioned.
			link.ID = existing.ID
			link.SourceWebhookID = existing.SourceWebhookID
			link.DiscordGuildID = existing.DiscordGuildID
			if _, err := r.st.UpdateRelayLink(ctx, link); err != nil {
				r.log.Error("update the relay link named in the configuration file",
					slog.Int("index", i), slog.Any("error", err))
			}
		}
	}
	return nil
}

// restClient is the HTTP half, which works with no gateway session: a webhook
// URL authenticates itself, so a link can be verified before a bot token is
// ever set.
func (r *discordRelay) restClient() *discord.REST {
	if client := r.current(); client != nil {
		return client.REST()
	}
	// A throwaway client with no token. Only the webhook endpoints are
	// reachable through it, which is exactly what a verification needs.
	return discord.NewClient(discord.Options{Log: r.log}).REST()
}

// --- queues -----------------------------------------------------------------

// enqueue hands one delivery to a link's worker.
//
// It never blocks. The Discord read loop calls this, and a blocked handler
// there stops heartbeats and takes the whole connection down with it, so a
// full queue drops the delivery and logs it rather than waiting for room.
func (r *discordRelay) enqueue(linkID int64, job relayJob) {
	r.mu.Lock()
	queue, ok := r.queues[linkID]
	if !ok {
		queue = make(chan relayJob, relayQueueDepth)
		r.queues[linkID] = queue
		go r.drain(queue)
	}
	r.mu.Unlock()

	select {
	case queue <- job:
	default:
		r.log.Warn("relay queue is full, dropping a message",
			slog.Int64("link", linkID), slog.Int("depth", relayQueueDepth))
	}
}

// drain runs one link's deliveries, one at a time and in order.
func (r *discordRelay) drain(queue chan relayJob) {
	for {
		select {
		case <-r.stopped:
			return
		case job := <-queue:
			ctx, cancel := context.WithTimeout(context.Background(), relayDeliveryTimeout)
			job(ctx)
			cancel()
		}
	}
}

// --- Discord to Aural -------------------------------------------------------

// onReady records the bot account and tells the administrators watching.
func (r *discordRelay) onReady(self discord.User) {
	r.hub.BroadcastRelayState(context.Background())
	r.log.Info("discord relay ready", slog.String("bot", self.Username))
}

// onGuild fills in the guild a link points at, the first time the bot can see
// it. The id is stored on the link so the settings screen can name the server
// even while the bot is offline.
func (r *discordRelay) onGuild(g discord.Guild) {
	ctx := context.Background()
	for _, ch := range append(append([]discord.Channel{}, g.Channels...), g.Threads...) {
		link, ok := r.linkForDiscord(ch.ID)
		if !ok || link.DiscordGuildID == g.ID {
			continue
		}
		link.DiscordGuildID = g.ID
		if _, err := r.st.UpdateRelayLink(ctx, link); err != nil {
			r.log.Debug("record the guild a relay link is in", slog.Any("error", err))
			continue
		}
		if err := r.reloadLinks(ctx); err != nil {
			r.log.Debug("reload relay links", slog.Any("error", err))
		}
	}
	r.hub.BroadcastRelayState(ctx)
}

// onDiscordMessage is the inbound entry point. It decides, cheaply, whether a
// message is one this server wants, and hands everything else to a queue.
func (r *discordRelay) onDiscordMessage(_ context.Context, m discord.Message) {
	if !m.Relayable() {
		return
	}
	// The loop guard, first and unconditionally: a message this server posted
	// is dropped before anything else looks at it.
	if r.isOwnEcho(m) {
		return
	}
	link, ok := r.linkForDiscord(m.ChannelID)
	if !ok || !link.ToAural() {
		return
	}
	// The bot's own account, which should never post — it speaks through
	// webhooks — but a shared token might be doing something else somewhere.
	if client := r.current(); client != nil && m.Author.ID == client.Self().ID {
		return
	}

	r.enqueue(link.ID, func(ctx context.Context) {
		if err := r.deliverToAural(ctx, link, m); err != nil {
			r.log.Warn("relay a Discord message into Aural",
				slog.Int64("link", link.ID), slog.Any("error", err))
			r.noteFailure(ctx, link.ID, err)
		}
	})
}

// onDiscordEdit carries a change made on Discord back to the message it
// produced here.
func (r *discordRelay) onDiscordEdit(_ context.Context, m discord.Message) {
	if r.isOwnEcho(m) {
		return
	}
	link, ok := r.linkForDiscord(m.ChannelID)
	if !ok || !link.ToAural() || !link.RelayEdits {
		return
	}
	// A MESSAGE_UPDATE arrives for things that are not edits at all — a link
	// this server never saw finishing its preview, a message being pinned. One
	// carrying no text and no cards is one of those, and rewriting a message
	// to nothing on the strength of it would delete what somebody wrote.
	if m.Content == "" && len(m.Embeds) == 0 {
		return
	}

	r.enqueue(link.ID, func(ctx context.Context) {
		if err := r.applyDiscordEdit(ctx, link, m); err != nil && !errors.Is(err, store.ErrNotFound) {
			r.log.Warn("apply a Discord edit", slog.Int64("link", link.ID), slog.Any("error", err))
		}
	})
}

// onDiscordDelete removes the messages a Discord deletion produced here.
func (r *discordRelay) onDiscordDelete(_ context.Context, channelID string, ids []string) {
	link, ok := r.linkForDiscord(channelID)
	if !ok || !link.ToAural() || !link.RelayEdits {
		return
	}
	r.enqueue(link.ID, func(ctx context.Context) {
		for _, id := range ids {
			if err := r.applyDiscordDelete(ctx, link, id); err != nil && !errors.Is(err, store.ErrNotFound) {
				r.log.Warn("apply a Discord deletion",
					slog.Int64("link", link.ID), slog.Any("error", err))
			}
		}
	})
}

// deliverToAural writes one Discord message into its Aural channel and tells
// everybody in it.
func (r *discordRelay) deliverToAural(ctx context.Context, link store.RelayLink, m discord.Message) error {
	channel, ok := r.hub.Channel(link.ChannelID)
	if !ok || channel.Type != protocol.ChannelText {
		return fmt.Errorf("relay link %d points at a channel that is not a text channel", link.ID)
	}

	webhookID, err := r.inboundWebhook(ctx, link)
	if err != nil {
		return err
	}

	// A Discord reply that points at a message this server already holds is
	// carried as a reply of its own. The client draws the preview from that,
	// so the quoted header renderInbound would otherwise write into the body
	// is left off: one reply, said once.
	replyToID := r.nativeReply(ctx, link, m)
	content := r.renderInbound(m, replyToID == nil)
	embeds := sanitiseEmbeds(m.Embeds)

	// The server's own rules apply to what arrives, exactly as they do to what
	// is typed here. A bridge that let a word list be walked around by writing
	// on the other side would be a hole in the moderation rather than a
	// feature.
	verdict, blocked := r.hub.screenRelayed(ctx, link.ChannelID, content, m.DisplayName())
	if blocked {
		r.log.Info("automatic moderation refused a relayed message",
			slog.Int64("link", link.ID), slog.String("author", m.DisplayName()))
		return nil
	}
	content = verdict

	files, err := r.fetchInboundFiles(ctx, link, m)
	if err != nil {
		// A file that would not come across is not a reason to lose the words
		// beside it. The message goes on without it, and the failure is noted
		// on the link.
		r.log.Warn("fetch a relayed attachment", slog.Int64("link", link.ID), slog.Any("error", err))
		r.noteFailure(ctx, link.ID, err)
	}

	if content == "" && len(embeds) == 0 && len(files) == 0 {
		r.discardUploads(files)
		return nil
	}
	if runes := []rune(content); len(runes) > maxMessageRunes {
		content = string(runes[:maxMessageRunes])
	}

	encoded, err := embedsJSON(embeds)
	if err != nil {
		r.discardUploads(files)
		return err
	}

	// A name made of nothing but characters cleanText drops would leave a
	// message attributed to an empty string, which renders as a blank line
	// where the author should be. Naming the service is worse than the name
	// and better than nothing.
	author := truncateRunes(cleanText(m.DisplayName()), maxWebhookUsername)
	if author == "" {
		author = relayInboundWebhookName
	}

	avatar := m.AvatarURL(relayAvatarSize)
	source := protocol.MessageSourceDiscord
	replyTo := resolveMessageReply(ctx, r.st, replyToID)
	created, err := r.st.CreateWebhookMessage(ctx, store.Message{
		ChannelID:     link.ChannelID,
		Author:        author,
		Content:       content,
		WebhookID:     &webhookID,
		WebhookAvatar: &avatar,
		WebhookSource: &source,
		Embeds:        encoded,
		ReplyToID:     replyToID,
	})
	if err != nil {
		r.discardUploads(files)
		return err
	}

	attachments, err := r.attachInboundFiles(ctx, link, created.ID, files)
	if err != nil {
		if delErr := r.st.DeleteMessage(ctx, created.ID); delErr != nil {
			r.log.Error("roll back a relayed message whose files could not be attached",
				slog.Int64("message", created.ID), slog.Any("error", delErr))
		}
		return err
	}

	if err := r.st.MapRelayMessage(ctx, store.RelayMessage{
		AuralID:   created.ID,
		LinkID:    link.ID,
		DiscordID: m.ID,
		Origin:    store.RelayOriginDiscord,
	}); err != nil {
		// The message is delivered either way. Losing the pairing costs a
		// later edit or deletion, not this.
		r.log.Warn("record what a relayed message is called on Discord", slog.Any("error", err))
	}

	view := messageView(created, attachments, replyTo)
	if view.Webhook != nil {
		view.Webhook.Source = protocol.MessageSourceDiscord
	}
	r.hub.BroadcastChannelEvent(
		protocol.Event(protocol.EvMessageCreated, protocol.MessageEvent{Message: view}),
		created.ChannelID)

	r.noteSuccess(ctx, link.ID)
	r.log.Debug("relayed a Discord message",
		slog.Int64("link", link.ID), slog.String("author", m.DisplayName()),
		slog.Int("files", len(attachments)))
	return nil
}

// How much of the message a reply answers travels in the quoted header the
// bridge writes when it cannot carry the reply as a reply. The point is to
// identify what is being answered, not to repeat it.
const quotedReplyRunes = 120

// oneLine folds a message into the single line a blockquote can hold. A
// newline would end the quote and leave the rest of it standing as body text,
// so the breaks become spaces rather than being dropped with the other control
// characters.
func oneLine(in string) string {
	return cleanText(strings.ReplaceAll(in, "\n", " "))
}

// nativeReply returns the id of the Aural message a Discord reply points at,
// when this server holds a copy of it, and nil otherwise — a reply to
// something written before the bridge existed, or to something already gone.
func (r *discordRelay) nativeReply(ctx context.Context, link store.RelayLink, m discord.Message) *int64 {
	if m.Referenced == nil {
		return nil
	}
	pair, err := r.st.RelayMessageByDiscord(ctx, link.ID, m.Referenced.ID)
	if err != nil {
		return nil
	}
	return &pair.AuralID
}

// renderInbound turns a Discord message into the text Aural stores: the ids
// resolved to names, a quoted header for a reply this side cannot carry as
// one, and a line naming any stickers, which have no equivalent here.
//
// quoteReply is false when the reply is being carried as a reply, which is the
// case whenever the message it answers is one this server already holds.
func (r *discordRelay) renderInbound(m discord.Message, quoteReply bool) string {
	var b strings.Builder

	if quoteReply && m.Referenced != nil {
		// A reply is rendered as a one-line quote of what it answers, which is
		// how a person reading only this side keeps the thread. It is cut
		// short deliberately: the point is to identify the message, not to
		// repeat it.
		quoted := discord.TruncateRunes(oneLine(m.Referenced.Content), quotedReplyRunes)
		if quoted != "" {
			fmt.Fprintf(&b, "> %s: %s\n", m.Referenced.DisplayName(), quoted)
		} else {
			fmt.Fprintf(&b, "> replying to %s\n", m.Referenced.DisplayName())
		}
	}

	b.WriteString(discord.RenderContent(m, r.current()))

	for _, sticker := range m.Stickers {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		if href := sticker.StickerURL(); href != "" {
			fmt.Fprintf(&b, "[sticker: %s] %s", sticker.Name, href)
		} else {
			fmt.Fprintf(&b, "[sticker: %s]", sticker.Name)
		}
	}

	return cleanMessage(b.String())
}

// inboundWebhook returns the webhooks row a link writes under, creating one the
// first time it is needed.
//
// The row is what gives an inbound message a per-message name and picture, and
// what the outbound side reads to recognise its own traffic. An administrator
// who deletes it from the channel's webhook list gets a new one rather than a
// broken bridge, which is why this checks rather than trusting the stored id.
func (r *discordRelay) inboundWebhook(ctx context.Context, link store.RelayLink) (int64, error) {
	if link.SourceWebhookID != nil {
		if _, err := r.st.WebhookByID(ctx, *link.SourceWebhookID); err == nil {
			return *link.SourceWebhookID, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return 0, err
		}
	}

	token, err := auth.NewWebhookToken()
	if err != nil {
		return 0, err
	}
	created, err := r.st.CreateWebhook(ctx, store.Webhook{
		ChannelID: link.ChannelID,
		Name:      relayInboundWebhookName,
		Token:     token,
	})
	if err != nil {
		return 0, err
	}
	if err := r.st.SetRelayLinkSourceWebhook(ctx, link.ID, created.ID); err != nil {
		return 0, err
	}
	if err := r.reloadLinks(ctx); err != nil {
		return 0, err
	}
	r.log.Info("provisioned the webhook a relay link writes under",
		slog.Int64("link", link.ID), slog.Int64("webhook", created.ID))
	return created.ID, nil
}

// applyDiscordEdit rewrites the Aural message a Discord message produced.
func (r *discordRelay) applyDiscordEdit(ctx context.Context, link store.RelayLink, m discord.Message) error {
	pair, err := r.st.RelayMessageByDiscord(ctx, link.ID, m.ID)
	if err != nil {
		return err
	}
	// An edit only travels away from where the message was written. A message
	// that started here and was mirrored there is edited here, and pushing
	// Discord's copy of it back would overwrite the original with its own echo.
	if pair.Origin != store.RelayOriginDiscord {
		return nil
	}

	content := r.renderInbound(m, r.nativeReply(ctx, link, m) == nil)
	verdict, blocked := r.hub.screenRelayed(ctx, link.ChannelID, content, m.DisplayName())
	if blocked {
		return nil
	}
	if runes := []rune(verdict); len(runes) > maxMessageRunes {
		verdict = string(runes[:maxMessageRunes])
	}

	encoded, err := embedsJSON(sanitiseEmbeds(m.Embeds))
	if err != nil {
		return err
	}
	updated, err := r.st.UpdateWebhookMessage(ctx, pair.AuralID, verdict, encoded)
	if err != nil {
		return err
	}

	attachments, err := r.st.AttachmentsForMessage(ctx, updated.ID)
	if err != nil {
		return err
	}
	view := messageView(updated, attachments, resolveMessageReply(ctx, r.st, updated.ReplyToID))
	if view.Webhook != nil {
		view.Webhook.Source = protocol.MessageSourceDiscord
	}
	r.hub.BroadcastChannelEvent(
		protocol.Event(protocol.EvMessageUpdated, protocol.MessageEvent{Message: view}),
		updated.ChannelID)
	return nil
}

// applyDiscordDelete removes the Aural message a Discord message produced.
func (r *discordRelay) applyDiscordDelete(ctx context.Context, link store.RelayLink, discordID string) error {
	pair, err := r.st.RelayMessageByDiscord(ctx, link.ID, discordID)
	if err != nil {
		return err
	}
	if pair.Origin != store.RelayOriginDiscord {
		return nil
	}

	message, err := r.st.MessageByID(ctx, pair.AuralID)
	if err != nil {
		return err
	}
	attachments, err := r.st.AttachmentsForMessage(ctx, message.ID)
	if err != nil {
		return err
	}
	if err := r.st.DeleteMessage(ctx, message.ID); err != nil {
		return err
	}
	r.hub.RemoveFiles(attachments)
	if err := r.st.ForgetRelayMessage(ctx, message.ID); err != nil {
		r.log.Debug("forget a relayed message pairing", slog.Any("error", err))
	}

	r.hub.BroadcastChannelEvent(protocol.Event(protocol.EvMessageDeleted,
		protocol.MessageDeletedEvent{MessageID: message.ID, ChannelID: message.ChannelID}),
		message.ChannelID)
	return nil
}

// --- bookkeeping ------------------------------------------------------------

// noteSuccess records that a link is working.
func (r *discordRelay) noteSuccess(ctx context.Context, linkID int64) {
	if err := r.st.TouchRelayLink(ctx, linkID, ""); err != nil {
		r.log.Debug("record a relay delivery", slog.Any("error", err))
	}
}

// noteFailure records why a link is not, so the settings screen can say so.
func (r *discordRelay) noteFailure(ctx context.Context, linkID int64, cause error) {
	if err := r.st.TouchRelayLink(ctx, linkID, truncateRunes(cause.Error(), 300)); err != nil {
		r.log.Debug("record a relay failure", slog.Any("error", err))
	}
}

// --- the state an administrator sees ----------------------------------------

// State assembles what the settings screen renders.
func (r *discordRelay) State(ctx context.Context) protocol.RelayState {
	settings := r.hub.RelaySettings()

	state := protocol.RelayState{
		Enabled:    settings.Enabled,
		Configured: settings.BotToken != "",
		Guilds:     []protocol.RelayGuild{},
		Links:      []protocol.RelayLink{},
	}

	client := r.current()
	if client != nil {
		self := client.Self()
		state.BotName, state.BotID = self.Username, self.ID
		state.Connected, state.Error = client.Connected()
	}

	links, err := r.st.RelayLinks(ctx)
	if err != nil {
		r.log.Warn("list relay links", slog.Any("error", err))
		return state
	}

	linked := make(map[string]struct{}, len(links))
	for _, l := range links {
		linked[l.DiscordChannelID] = struct{}{}
	}

	if client != nil {
		for _, g := range client.Guilds() {
			guild := protocol.RelayGuild{
				ID: g.ID, Name: g.Name, Channels: []protocol.RelayChannel{},
			}
			if g.Icon != nil && *g.Icon != "" {
				guild.Icon = discordGuildIcon(g.ID, *g.Icon)
			}
			for _, ch := range append(append([]discord.Channel{}, g.Channels...), g.Threads...) {
				if !ch.Writable() {
					continue
				}
				_, taken := linked[ch.ID]
				guild.Channels = append(guild.Channels, protocol.RelayChannel{
					ID: ch.ID, Name: ch.Name, Type: ch.Type,
					ParentID: ch.ParentID, Linked: taken,
				})
			}
			state.Guilds = append(state.Guilds, guild)
		}
	}

	for _, l := range links {
		state.Links = append(state.Links, r.linkView(client, l))
	}
	return state
}

// linkView renders one link, naming the Discord side from what the bot can
// currently see.
func (r *discordRelay) linkView(client *discord.Client, l store.RelayLink) protocol.RelayLink {
	view := protocol.RelayLink{
		ID:               l.ID,
		ChannelID:        l.ChannelID,
		DiscordGuildID:   l.DiscordGuildID,
		DiscordChannelID: l.DiscordChannelID,
		WebhookURL:       discordWebhookURL(l.WebhookID, l.WebhookToken),
		Direction:        l.Direction,
		Enabled:          l.Enabled,
		Attachments:      l.RelayAttachments,
		Edits:            l.RelayEdits,
		CreatedAt:        l.CreatedAt,
		LastRelayedAt:    l.LastRelayedAt,
		LastError:        l.LastError,
	}
	if client == nil {
		return view
	}
	if name, ok := client.ChannelName(l.DiscordChannelID); ok {
		view.DiscordChannelName = name
	}
	for _, g := range client.Guilds() {
		if g.ID == l.DiscordGuildID {
			view.DiscordGuildName = g.Name
			break
		}
	}
	return view
}

// discordWebhookURL rebuilds the URL a link was configured with, which is what
// the settings screen shows back.
func discordWebhookURL(id, token string) string {
	return "https://discord.com/api/webhooks/" + url.PathEscape(id) + "/" + url.PathEscape(token)
}

// discordGuildIcon builds a server's picture URL on Discord's CDN.
func discordGuildIcon(guildID, hash string) string {
	ext := "png"
	if strings.HasPrefix(hash, "a_") {
		ext = "gif"
	}
	return "https://cdn.discordapp.com/icons/" + url.PathEscape(guildID) + "/" +
		url.PathEscape(hash) + "." + ext + "?size=64"
}

// --- configuration ----------------------------------------------------------

// RelaySettings is a copy of the relay block, taken under the lock that guards
// the configuration a running server may edit.
func (h *Hub) RelaySettings() config.Relay {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()

	// Copied rather than shared: the slice inside it would otherwise be a
	// second reference to the configuration the settings screen writes.
	out := h.cfg.Relay
	out.Links = append([]config.RelayLink(nil), h.cfg.Relay.Links...)
	return out
}

// publicBaseURL is where this server is reachable from the internet.
//
// Discord fetches the avatar of a relayed message itself, from its own network,
// so the URL has to be absolute and has to resolve out there. Nothing about a
// bridged message arrives over HTTP, so there is no request to read a host off:
// it is either configured or guessed from what the server already knows about
// how it is reached.
//
// An empty answer is not a failure. It costs the per-author pictures on the
// Discord side and nothing else.
func (h *Hub) publicBaseURL() string {
	h.cfgMu.RLock()
	configured := h.cfg.Relay.PublicURL
	tls := h.cfg.TLS
	ddns := h.cfg.DDNS
	port := h.cfg.Server.Port
	h.cfgMu.RUnlock()

	if configured != "" {
		return configured
	}

	scheme := "http"
	if tls.Enabled {
		scheme = "https"
	}

	// In order of how much the operator has told us. A certificate names the
	// hostname people actually type; a dynamic DNS record names the one that
	// tracks this machine; a public address is the last resort and is at least
	// reachable.
	host := ""
	switch {
	case tls.ACME.Enabled && len(tls.ACME.Domains) > 0:
		host = tls.ACME.Domains[0]
	case ddns.Enabled && ddns.Domain != "":
		host = ddns.Domain
	default:
		host = h.PublicIP()
	}
	if host == "" {
		return ""
	}

	// The default port for the scheme is left off, so the common deployment
	// behind a proxy on 443 produces the address people type.
	if (scheme == "https" && port == 443) || (scheme == "http" && port == 80) {
		return scheme + "://" + host
	}
	return scheme + "://" + host + ":" + strconv.Itoa(port)
}
