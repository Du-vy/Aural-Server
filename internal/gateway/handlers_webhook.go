package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/aural-chat/aural-server/internal/auth"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

const (
	// maxWebhookName matches what Discord accepts, because the name is very
	// often copied straight out of an integration's configuration screen.
	maxWebhookName = 80
	// minWebhookName keeps a webhook from being attributed to nothing.
	minWebhookName = 1
	// maxWebhooksPerChannel bounds how many standing invitations to post one
	// channel may hold. Discord's ceiling is the same, and integrations are
	// counted in ones and twos.
	maxWebhooksPerChannel = 15
	// maxAvatarURL bounds the picture URL. It is somebody else's address, held
	// only to be put in an <img> tag.
	maxAvatarURL = 512
)

// handleWebhookList reads the webhooks the caller may manage.
//
// A channel the caller may not manage webhooks in is left out rather than
// refused, exactly as an invisible channel is left out of the channel tree: a
// list must not be the one place that admits something exists.
func handleWebhookList(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.WebhookListRequest](raw)
	if failure != nil {
		return nil, failure
	}

	var channelIDs []int64
	for _, channel := range s.hub.VisibleChannels(s) {
		if channel.Type != protocol.ChannelText {
			continue
		}
		if req.ChannelID != 0 && channel.ID != req.ChannelID {
			continue
		}
		if s.hub.requireChannelPermission(s, &channel.ID, permissions.ManageWebhooks) != nil {
			continue
		}
		channelIDs = append(channelIDs, channel.ID)
	}

	hooks, err := s.hub.st.WebhooksForChannels(ctx, channelIDs)
	if err != nil {
		return nil, internalError(s, "read the webhooks", err)
	}

	views := make([]protocol.Webhook, 0, len(hooks))
	for _, wh := range hooks {
		views = append(views, webhookView(wh))
	}
	return protocol.WebhookListResult{Webhooks: views}, nil
}

// handleWebhookCreate mints a URL that posts into one channel.
func handleWebhookCreate(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.WebhookCreateRequest](raw)
	if failure != nil {
		return nil, failure
	}
	name, failure := validateWebhookName(req.Name)
	if failure != nil {
		return nil, failure
	}
	avatar, failure := validateWebhookAvatar(req.Avatar)
	if failure != nil {
		return nil, failure
	}
	if failure := s.requireWebhookChannel(req.ChannelID); failure != nil {
		return nil, failure
	}

	count, err := s.hub.st.CountWebhooksInChannel(ctx, req.ChannelID)
	if err != nil {
		return nil, internalError(s, "count the webhooks", err)
	}
	if count >= maxWebhooksPerChannel {
		return nil, protocol.Errorf(protocol.ErrConflict,
			fmt.Sprintf("a channel may hold at most %d webhooks", maxWebhooksPerChannel))
	}

	token, err := auth.NewWebhookToken()
	if err != nil {
		return nil, internalError(s, "mint the webhook token", err)
	}
	creator := s.UserID()
	created, err := s.hub.st.CreateWebhook(ctx, store.Webhook{
		ChannelID: req.ChannelID,
		Name:      name,
		Avatar:    avatar,
		Token:     token,
		CreatorID: &creator,
	})
	if err != nil {
		return nil, internalError(s, "create the webhook", err)
	}

	// The token is not logged, here or anywhere: it is the whole of the
	// webhook's authentication, and a log file is the one place a secret ends
	// up being read by somebody who was never handed it.
	s.log.Info("webhook created",
		slog.Int64("webhook", created.ID), slog.Int64("channel", created.ChannelID),
		slog.String("name", created.Name))

	return protocol.WebhookEvent{Webhook: webhookView(created)}, nil
}

// handleWebhookUpdate renames a webhook, repoints its picture, or moves it to
// another channel.
func handleWebhookUpdate(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.WebhookUpdateRequest](raw)
	if failure != nil {
		return nil, failure
	}
	current, failure := s.loadManageableWebhook(ctx, req.WebhookID)
	if failure != nil {
		return nil, failure
	}

	if req.Name != nil {
		name, failure := validateWebhookName(*req.Name)
		if failure != nil {
			return nil, failure
		}
		current.Name = name
	}
	if req.Avatar != nil {
		avatar, failure := validateWebhookAvatar(*req.Avatar)
		if failure != nil {
			return nil, failure
		}
		current.Avatar = avatar
	}
	if req.ChannelID != nil && *req.ChannelID != current.ChannelID {
		// Moving a webhook is minting one in the destination, so it needs the
		// permission there and not only where it currently sits.
		if failure := s.requireWebhookChannel(*req.ChannelID); failure != nil {
			return nil, failure
		}
		count, err := s.hub.st.CountWebhooksInChannel(ctx, *req.ChannelID)
		if err != nil {
			return nil, internalError(s, "count the webhooks", err)
		}
		if count >= maxWebhooksPerChannel {
			return nil, protocol.Errorf(protocol.ErrConflict,
				fmt.Sprintf("a channel may hold at most %d webhooks", maxWebhooksPerChannel))
		}
		current.ChannelID = *req.ChannelID
	}

	if err := s.hub.st.UpdateWebhook(ctx, current); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such webhook")
		}
		return nil, internalError(s, "update the webhook", err)
	}
	return protocol.WebhookEvent{Webhook: webhookView(current)}, nil
}

// handleWebhookDelete revokes a URL. What was posted through it stays.
func handleWebhookDelete(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.WebhookDeleteRequest](raw)
	if failure != nil {
		return nil, failure
	}
	current, failure := s.loadManageableWebhook(ctx, req.WebhookID)
	if failure != nil {
		return nil, failure
	}

	if err := s.hub.st.DeleteWebhook(ctx, current.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, protocol.Errorf(protocol.ErrNotFound, "no such webhook")
		}
		return nil, internalError(s, "delete the webhook", err)
	}
	s.log.Info("webhook deleted",
		slog.Int64("webhook", current.ID), slog.Int64("channel", current.ChannelID))

	return protocol.WebhookDeleteRequest{WebhookID: current.ID}, nil
}

// --- helpers ----------------------------------------------------------------

// requireWebhookChannel checks that a channel is a text channel the caller may
// see and may mint a webhook in.
func (s *Session) requireWebhookChannel(channelID int64) *protocol.Error {
	channel, ok := s.hub.Channel(channelID)
	if !ok {
		return protocol.Errorf(protocol.ErrNotFound, "no such channel")
	}
	if failure := s.hub.requireChannelPermission(s, &channelID, permissions.ManageWebhooks); failure != nil {
		return failure
	}
	// Checked after the permission so an invisible channel reports "not found"
	// rather than leaking its type.
	if channel.Type != protocol.ChannelText {
		return protocol.Errorf(protocol.ErrBadRequest, "that channel does not carry messages")
	}
	return nil
}

// loadManageableWebhook reads a webhook the caller may act on. One in a channel
// they cannot manage reports "not found", exactly as the channel would.
func (s *Session) loadManageableWebhook(ctx context.Context, id int64) (store.Webhook, *protocol.Error) {
	wh, err := s.hub.st.WebhookByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Webhook{}, protocol.Errorf(protocol.ErrNotFound, "no such webhook")
		}
		return store.Webhook{}, internalError(s, "read the webhook", err)
	}
	if !s.hub.SessionCanView(s, wh.ChannelID) ||
		s.hub.requireChannelPermission(s, &wh.ChannelID, permissions.ManageWebhooks) != nil {
		return store.Webhook{}, protocol.Errorf(protocol.ErrNotFound, "no such webhook")
	}
	return wh, nil
}

// validateWebhookName normalises and checks what messages will be attributed to.
func validateWebhookName(raw string) (string, *protocol.Error) {
	name := cleanText(raw)
	n := utf8.RuneCountInString(name)
	if n < minWebhookName {
		return "", protocol.Errorf(protocol.ErrBadRequest, "a webhook needs a name")
	}
	if n > maxWebhookName {
		return "", protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("a webhook name must be at most %d characters", maxWebhookName))
	}
	return name, nil
}

// validateWebhookAvatar checks a picture URL, which is somebody else's address
// and is only ever put in an <img> tag.
//
// An empty string clears it, which is why the result is a pointer: "no picture"
// and "leave the picture alone" are different requests and must not collapse
// into the same value.
func validateWebhookAvatar(raw string) (*string, *protocol.Error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	if len(trimmed) > maxAvatarURL {
		return nil, protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("an avatar URL must be at most %d characters", maxAvatarURL))
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, protocol.Errorf(protocol.ErrBadRequest,
			"an avatar must be an absolute http or https URL")
	}
	return &trimmed, nil
}
