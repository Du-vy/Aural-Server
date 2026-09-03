package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

const (
	minExpressionName = 2
	maxExpressionName = 32
	maxSoundName      = 32
	// maxSoundEmoji bounds the label on a button. A few runes rather than one,
	// because a single emoji is very often several code points: a flag, a skin
	// tone, a family.
	maxSoundEmoji = 8
)

// validateExpressionName checks what writers will type to reach an emoji.
//
// It is deliberately narrow — letters, digits and underscore — because the name
// sits between two colons in the middle of a sentence, and anything that could
// also be punctuation would make `:name:` ambiguous with the text around it.
func validateExpressionName(raw string) (string, *protocol.Error) {
	name := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(name)
	if n < minExpressionName || n > maxExpressionName {
		return "", protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("a name must be between %d and %d characters",
				minExpressionName, maxExpressionName))
	}
	for _, r := range name {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '_':
		default:
			return "", protocol.Errorf(protocol.ErrBadRequest,
				"a name may only contain letters, digits and underscores")
		}
	}
	return strings.ToLower(name), nil
}

// handleExpressionUpdate renames a custom emoji or sticker.
func handleExpressionUpdate(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.ExpressionUpdateRequest](raw)
	if failure != nil {
		return nil, failure
	}

	base, _ := s.Permissions()
	if !base.Has(permissions.ManageExpressions) {
		return nil, protocol.Errorf(protocol.ErrForbidden,
			"you are not allowed to manage this server's emoji")
	}
	name, failure := validateExpressionName(req.Name)
	if failure != nil {
		return nil, failure
	}

	existing, err := s.hub.st.ExpressionByID(ctx, req.ExpressionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such emoji")
	}
	if err != nil {
		return nil, internalError(s, "read that emoji", err)
	}

	updated, err := s.hub.st.RenameExpression(ctx, req.ExpressionID, name)
	if errors.Is(err, store.ErrConflict) {
		return nil, protocol.Errorf(protocol.ErrConflict, "something here already has that name")
	}
	if errors.Is(err, store.ErrNotFound) {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such emoji")
	}
	if err != nil {
		return nil, internalError(s, "rename that emoji", err)
	}

	if err := s.hub.ReloadExpressions(ctx); err != nil {
		s.log.Warn("reload expressions", slog.Any("error", err))
	}
	view := expressionView(updated)
	s.hub.Broadcast(protocol.Event(protocol.EvExpressionUpdated, protocol.ExpressionEvent{Expression: view}))

	entry := auditTarget(protocol.AuditTargetExpression, updated.ID, updated.Name)
	entry.Action = protocol.AuditExpressionEdit
	entry.Changes = changed(nil, "name", existing.Name, updated.Name)
	s.hub.audit(ctx, s, entry)

	return protocol.ExpressionEvent{Expression: view}, nil
}

// handleExpressionDelete removes a custom emoji or sticker, and the file behind
// it. Messages that already used it keep the text they were written with, which
// then renders as the plain `:name:` it always was.
func handleExpressionDelete(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.ExpressionDeleteRequest](raw)
	if failure != nil {
		return nil, failure
	}

	base, _ := s.Permissions()
	if !base.Has(permissions.ManageExpressions) {
		return nil, protocol.Errorf(protocol.ErrForbidden,
			"you are not allowed to manage this server's emoji")
	}

	removed, err := s.hub.st.DeleteExpression(ctx, req.ExpressionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such emoji")
	}
	if err != nil {
		return nil, internalError(s, "delete that emoji", err)
	}
	if files := s.hub.Files(); files != nil {
		files.Remove(removed.StorageKey, removed.Size)
	}
	if err := s.hub.ReloadExpressions(ctx); err != nil {
		s.log.Warn("reload expressions", slog.Any("error", err))
	}

	event := protocol.ExpressionDeletedEvent{ExpressionID: removed.ID, Kind: removed.Kind}
	s.hub.Broadcast(protocol.Event(protocol.EvExpressionDeleted, event))

	entry := auditTarget(protocol.AuditTargetExpression, removed.ID, removed.Name)
	entry.Action = protocol.AuditExpressionDel
	s.hub.audit(ctx, s, entry)

	return event, nil
}

// --- the soundboard ---------------------------------------------------------

// handleSoundUpdate relabels a clip or changes how loud it plays.
func handleSoundUpdate(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.SoundUpdateRequest](raw)
	if failure != nil {
		return nil, failure
	}

	base, _ := s.Permissions()
	if !base.Has(permissions.ManageExpressions) {
		return nil, protocol.Errorf(protocol.ErrForbidden,
			"you are not allowed to manage this server's sounds")
	}

	existing, err := s.hub.st.SoundByID(ctx, req.SoundID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such sound")
	}
	if err != nil {
		return nil, internalError(s, "read that sound", err)
	}

	name, emoji, volume := existing.Name, existing.Emoji, existing.Volume
	if req.Name != nil {
		name, failure = validateSoundName(*req.Name)
		if failure != nil {
			return nil, failure
		}
	}
	if req.Emoji != nil {
		emoji, failure = validateSoundEmoji(*req.Emoji)
		if failure != nil {
			return nil, failure
		}
	}
	if req.Volume != nil {
		volume = *req.Volume
		if volume < 0 || volume > 100 {
			return nil, protocol.Errorf(protocol.ErrBadRequest, "a volume runs from 0 to 100")
		}
	}

	updated, err := s.hub.st.UpdateSound(ctx, req.SoundID, name, emoji, volume)
	if errors.Is(err, store.ErrNotFound) {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such sound")
	}
	if err != nil {
		return nil, internalError(s, "update that sound", err)
	}
	if err := s.hub.ReloadSounds(ctx); err != nil {
		s.log.Warn("reload sounds", slog.Any("error", err))
	}

	view := soundView(updated)
	s.hub.Broadcast(protocol.Event(protocol.EvSoundUpdated, protocol.SoundEvent{Sound: view}))

	entry := auditTarget(protocol.AuditTargetSound, updated.ID, updated.Name)
	entry.Action = protocol.AuditSoundEdit
	entry.Changes = changed(nil, "name", existing.Name, updated.Name)
	entry.Changes = changed(entry.Changes, "volume",
		strconv.Itoa(existing.Volume), strconv.Itoa(updated.Volume))
	s.hub.audit(ctx, s, entry)

	return protocol.SoundEvent{Sound: view}, nil
}

// handleSoundDelete removes a clip and the file behind it.
func handleSoundDelete(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.SoundDeleteRequest](raw)
	if failure != nil {
		return nil, failure
	}

	base, _ := s.Permissions()
	if !base.Has(permissions.ManageExpressions) {
		return nil, protocol.Errorf(protocol.ErrForbidden,
			"you are not allowed to manage this server's sounds")
	}

	removed, err := s.hub.st.DeleteSound(ctx, req.SoundID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such sound")
	}
	if err != nil {
		return nil, internalError(s, "delete that sound", err)
	}
	if files := s.hub.Files(); files != nil {
		files.Remove(removed.StorageKey, removed.Size)
	}
	if err := s.hub.ReloadSounds(ctx); err != nil {
		s.log.Warn("reload sounds", slog.Any("error", err))
	}

	event := protocol.SoundDeletedEvent{SoundID: removed.ID}
	s.hub.Broadcast(protocol.Event(protocol.EvSoundDeleted, event))

	entry := auditTarget(protocol.AuditTargetSound, removed.ID, removed.Name)
	entry.Action = protocol.AuditSoundDel
	s.hub.audit(ctx, s, entry)

	return event, nil
}

// handleSoundPlay plays a clip at the voice channel the caller is sitting in.
//
// The channel is not a parameter. It is wherever the caller is, because playing
// a sound into a room you are not in is not something anybody should be able to
// do, and because the permission that matters is the one that applies there.
func handleSoundPlay(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.SoundPlayRequest](raw)
	if failure != nil {
		return nil, failure
	}

	channelID := s.ChannelID()
	if channelID == nil {
		return nil, protocol.Errorf(protocol.ErrBadRequest, "you are not in a voice channel")
	}
	if failure := s.hub.requireChannelPermission(s, channelID, permissions.UseSoundboard); failure != nil {
		return nil, failure
	}
	// Being muted by a moderator is being told to stop making noise in this
	// room, and a soundboard is noise in this room.
	if s.voiceSnapshot().mute {
		return nil, protocol.Errorf(protocol.ErrForbidden, "you have been muted here")
	}
	// Paced tightly on purpose. A clip is up to ten seconds of audio played at
	// everybody in the channel at once, which makes it the one thing here that
	// is annoying at a rate an ordinary message never is.
	if !s.soundboard.allow() {
		return nil, protocol.Errorf(protocol.ErrRateLimited, "you are playing sounds too quickly")
	}

	sound, ok := s.hub.Sound(req.SoundID)
	if !ok {
		return nil, protocol.Errorf(protocol.ErrNotFound, "no such sound")
	}

	event := protocol.SoundPlayedEvent{
		SoundID:   sound.ID,
		UserID:    s.UserID(),
		ChannelID: *channelID,
	}
	// Everybody sitting in the channel, whether or not they hold a media
	// session: somebody with no microphone is still in the room and still
	// hears what is played in it.
	frame := protocol.Event(protocol.EvSoundPlayed, event)
	for _, other := range s.hub.Sessions() {
		if id := other.ChannelID(); id != nil && *id == *channelID {
			other.Send(frame)
		}
	}
	_ = ctx
	return event, nil
}

// validateSoundName normalises the label on a soundboard button. It is far
// looser than an emoji name: nobody types it, they read it.
func validateSoundName(raw string) (string, *protocol.Error) {
	name := cleanText(raw)
	if name == "" {
		return "", protocol.Errorf(protocol.ErrBadRequest, "a sound needs a name")
	}
	if utf8.RuneCountInString(name) > maxSoundName {
		return "", protocol.Errorf(protocol.ErrBadRequest,
			fmt.Sprintf("a sound name must be at most %d characters", maxSoundName))
	}
	return name, nil
}

// validateSoundEmoji checks the glyph shown on the button, which may be absent.
func validateSoundEmoji(raw string) (string, *protocol.Error) {
	emoji := cleanText(raw)
	if utf8.RuneCountInString(emoji) > maxSoundEmoji {
		return "", protocol.Errorf(protocol.ErrBadRequest, "that is more than one emoji")
	}
	return emoji, nil
}
