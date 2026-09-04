package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aural-chat/aural-server/internal/discord"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// The Aural to Discord half of the bridge.
//
// Every one of these is called after the message has already been written and
// broadcast here. That order is on purpose: a bridge is a courtesy to the
// people who have not moved yet, and a Discord outage must not be able to slow
// down, fail or reorder a message for the people who have.

// OutboundMessage relays a message written here to the Discord channel its
// link points at.
//
// It returns immediately. The work is queued behind whatever that link is
// already sending, which keeps a channel arriving in the order it was written.
func (r *discordRelay) OutboundMessage(m store.Message, attachments []store.Attachment) {
	link, ok := r.outboundLink(m)
	if !ok {
		return
	}
	r.enqueue(link.ID, func(ctx context.Context) {
		if err := r.deliverToDiscord(ctx, link, m, attachments); err != nil {
			r.log.Warn("relay a message into Discord",
				slog.Int64("link", link.ID), slog.Int64("message", m.ID), slog.Any("error", err))
			r.noteFailure(ctx, link.ID, err)
		}
	})
}

// OutboundEdit carries a change made here to the copy on Discord.
func (r *discordRelay) OutboundEdit(m store.Message) {
	link, ok := r.outboundLink(m)
	if !ok || !link.RelayEdits {
		return
	}
	r.enqueue(link.ID, func(ctx context.Context) {
		if err := r.pushEdit(ctx, link, m); err != nil &&
			!errors.Is(err, store.ErrNotFound) && !errors.Is(err, discord.ErrNotFound) {
			r.log.Warn("relay an edit into Discord",
				slog.Int64("link", link.ID), slog.Any("error", err))
		}
	})
}

// OutboundDelete removes the Discord copy of a message deleted here.
//
// It takes the whole message rather than an id because the caller is holding
// one and this needs the channel and the webhook tag off it — by the time the
// queue reaches the work, the row is gone.
func (r *discordRelay) OutboundDelete(m store.Message) {
	link, ok := r.outboundLink(m)
	if !ok || !link.RelayEdits {
		return
	}
	r.enqueue(link.ID, func(ctx context.Context) {
		if err := r.pushDelete(ctx, link, m.ID); err != nil &&
			!errors.Is(err, store.ErrNotFound) && !errors.Is(err, discord.ErrNotFound) {
			r.log.Warn("relay a deletion into Discord",
				slog.Int64("link", link.ID), slog.Any("error", err))
		}
	})
}

// outboundLink decides whether a message should cross, and to where.
//
// This is where the second half of the loop guard sits. A message written by
// the relay itself carries the webhooks row the relay writes under, and
// sending it back to the server it came from is exactly the loop this whole
// design exists to make impossible.
func (r *discordRelay) outboundLink(m store.Message) (store.RelayLink, bool) {
	// A comment on a post is not a line of the channel timeline. Relaying one
	// would drop it into Discord with no thread to hang it off and no way back,
	// so posts stay on this side.
	if m.PostID != nil {
		return store.RelayLink{}, false
	}
	if r.isInboundMessage(m) {
		return store.RelayLink{}, false
	}
	link, ok := r.linkForChannel(m.ChannelID)
	if !ok || !link.ToDiscord() {
		return store.RelayLink{}, false
	}
	if r.current() == nil {
		// The gateway session is what proves the token still works. Posting
		// through a webhook would technically succeed without one, but a relay
		// that is half up is harder to diagnose than one that is down.
		return store.RelayLink{}, false
	}
	return link, true
}

// deliverToDiscord posts one Aural message through the link's webhook.
func (r *discordRelay) deliverToDiscord(ctx context.Context, link store.RelayLink,
	m store.Message, attachments []store.Attachment) error {

	content := discord.EscapeOutbound(m.Content)

	var files []discord.OutboundFile
	var skipped []store.Attachment
	if link.RelayAttachments {
		files, skipped = r.outboundFiles(attachments)
	} else {
		skipped = attachments
	}
	// A file that could not be uploaded is named instead, with a link if this
	// server has an address the reader could follow.
	if note := r.describeSkipped(skipped); note != "" {
		if content != "" {
			content += "\n"
		}
		content += note
	}

	if strings.TrimSpace(content) == "" && len(files) == 0 {
		return nil
	}
	// The quote goes on after the emptiness check, so a message that had
	// nothing to send stays unsent rather than crossing as a bare header.
	content = r.outboundReplyQuote(ctx, m) + content
	content = discord.TruncateRunes(content, maxMessageRunes)

	out := discord.OutboundMessage{
		Content:   content,
		Username:  r.outboundName(m),
		AvatarURL: r.outboundAvatar(m),
		Files:     files,
	}

	posted, err := r.restClient().Execute(ctx, link.WebhookID, link.WebhookToken, out)
	if err != nil {
		return err
	}

	if posted.ID != "" {
		if err := r.st.MapRelayMessage(ctx, store.RelayMessage{
			AuralID:   m.ID,
			LinkID:    link.ID,
			DiscordID: posted.ID,
			Origin:    store.RelayOriginAural,
		}); err != nil {
			r.log.Warn("record what a relayed message is called on Discord", slog.Any("error", err))
		}
	}

	r.noteSuccess(ctx, link.ID)
	r.log.Debug("relayed a message into Discord",
		slog.Int64("link", link.ID), slog.Int64("message", m.ID), slog.Int("files", len(files)))
	return nil
}

// outboundReplyQuote renders the header a reply crosses with, or "" for a
// message that answers nothing.
//
// A webhook cannot post a Discord reply: Execute carries no message_reference,
// and the bridge writes as a webhook by design. So a reply crosses as the same
// one-line quote the inbound half writes when it has no reply to carry either,
// and a thread reads the same from both sides. Without it an answer lands on
// Discord with nothing saying what it answers.
func (r *discordRelay) outboundReplyQuote(ctx context.Context, m store.Message) string {
	if m.ReplyToID == nil {
		return ""
	}
	target, err := r.st.MessageByID(ctx, *m.ReplyToID)
	if err != nil {
		// The message it answered is gone. The reply is still worth sending;
		// there is just nothing left to name.
		return ""
	}
	author := discord.EscapeOutbound(oneLine(target.Author))
	quoted := discord.TruncateRunes(discord.EscapeOutbound(oneLine(target.Content)), quotedReplyRunes)
	if quoted == "" {
		return fmt.Sprintf("> replying to %s\n", author)
	}
	return fmt.Sprintf("> %s: %s\n", author, quoted)
}

// pushEdit rewrites the Discord copy of a message edited here.
func (r *discordRelay) pushEdit(ctx context.Context, link store.RelayLink, m store.Message) error {
	pair, err := r.st.RelayMessageByAural(ctx, m.ID)
	if err != nil {
		return err
	}
	// As on the other side: an edit travels away from where the message was
	// written, never back towards it.
	if pair.Origin != store.RelayOriginAural {
		return nil
	}

	content := discord.EscapeOutbound(m.Content)
	if strings.TrimSpace(content) == "" {
		// Discord refuses an empty message, and a message here can be nothing
		// but its files. Leaving the copy as it was is better than deleting
		// what somebody edited rather than removed.
		return nil
	}
	// The copy keeps the header the reply crossed with: an edit changes what
	// was said, not what it was said in answer to.
	content = discord.TruncateRunes(r.outboundReplyQuote(ctx, m)+content, maxMessageRunes)

	err = r.restClient().Edit(ctx, link.WebhookID, link.WebhookToken, pair.DiscordID,
		discord.OutboundMessage{Content: content})
	if errors.Is(err, discord.ErrNotFound) {
		// Somebody deleted it on the Discord side. The pairing is stale, and
		// keeping it would make every later edit retry a message that is gone.
		if forget := r.st.ForgetRelayMessage(ctx, m.ID); forget != nil {
			r.log.Debug("forget a stale relay pairing", slog.Any("error", forget))
		}
		return nil
	}
	return err
}

// pushDelete removes the Discord copy of a message deleted here.
func (r *discordRelay) pushDelete(ctx context.Context, link store.RelayLink, auralID int64) error {
	pair, err := r.st.RelayMessageByAural(ctx, auralID)
	if err != nil {
		return err
	}
	if pair.Origin != store.RelayOriginAural {
		return nil
	}

	err = r.restClient().Delete(ctx, link.WebhookID, link.WebhookToken, pair.DiscordID)
	if err != nil && !errors.Is(err, discord.ErrNotFound) {
		return err
	}
	return r.st.ForgetRelayMessage(ctx, auralID)
}

// outboundName is the name the message is posted under on Discord.
//
// Discord refuses a few names outright — anything containing "discord", and
// the reserved "everyone" and "here" — with a 400 that would lose the message.
// They are nudged rather than rejected, because a person called Discordia
// should not be the one member of a server who cannot be heard across the
// bridge.
func (r *discordRelay) outboundName(m store.Message) string {
	name := cleanText(m.Author)
	if name == "" {
		name = "Aural"
	}

	// A zero-width space breaks the string Discord matches on while leaving
	// the name looking exactly as it did.
	const zwsp = "​"
	if at := strings.Index(strings.ToLower(name), "discord"); at >= 0 {
		name = name[:at+4] + zwsp + name[at+4:]
	}
	switch strings.ToLower(name) {
	case "everyone", "here":
		name += zwsp
	}
	return truncateRunes(name, maxWebhookUsername)
}

// outboundAvatar is the picture the message is posted under.
//
// Discord fetches it from its own network, so it has to be an absolute address
// that resolves out there. A server that has not been told one — and could not
// guess — posts with no override, which leaves the webhook's own picture: less
// good, and better than a broken image on every message.
func (r *discordRelay) outboundAvatar(m store.Message) string {
	if m.UserID == nil {
		return ""
	}
	base := r.hub.publicBaseURL()
	if base == "" {
		return ""
	}
	user, err := r.st.UserByID(context.Background(), *m.UserID)
	if err != nil || user.Avatar == nil || *user.Avatar == "" {
		return ""
	}

	avatar := *user.Avatar
	if strings.HasPrefix(avatar, "http://") || strings.HasPrefix(avatar, "https://") {
		return avatar
	}
	return base + "/" + strings.TrimPrefix(avatar, "/")
}

// describeSkipped names the files that could not be uploaded.
func (r *discordRelay) describeSkipped(skipped []store.Attachment) string {
	if len(skipped) == 0 {
		return ""
	}
	base := r.hub.publicBaseURL()

	var lines []string
	for _, a := range skipped {
		if base != "" {
			lines = append(lines, fmt.Sprintf("[%s] %s%s", a.Filename, base,
				attachmentView(a).URL))
			continue
		}
		lines = append(lines, fmt.Sprintf("[%s, %s — too large to relay]",
			a.Filename, humanBytes(a.Size)))
	}
	return strings.Join(lines, "\n")
}

// --- where the outbound side is called from ---------------------------------

// relayMessage hands a newly written message to the bridge, if there is one.
//
// It is a method on Hub rather than on the relay so that every call site can be
// one line and can be written without knowing whether a relay exists: a server
// with the feature switched off has a nil field here and pays a nil check.
func (h *Hub) relayMessage(m store.Message, attachments []store.Attachment) {
	if h.discord != nil {
		h.discord.OutboundMessage(m, attachments)
	}
}

func (h *Hub) relayEdit(m store.Message) {
	if h.discord != nil {
		h.discord.OutboundEdit(m)
	}
}

func (h *Hub) relayDelete(m store.Message) {
	if h.discord != nil {
		h.discord.OutboundDelete(m)
	}
}

// BroadcastRelayState sends the relay's state to everybody watching it.
//
// It reaches only the sessions that may manage the server, because the state
// names webhook URLs and those are credentials.
func (h *Hub) BroadcastRelayState(ctx context.Context) {
	if h.discord == nil {
		return
	}
	state := h.discord.State(ctx)
	event := protocol.Event(protocol.EvRelayUpdated, protocol.RelayEvent{Relay: state})
	h.BroadcastTo(event, func(s *Session) bool {
		base, _ := s.Permissions()
		return base&permissions.ManageServer != 0
	})
}
