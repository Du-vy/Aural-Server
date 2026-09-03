package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// handleAutoModGet reads the rule set.
//
// It is behind ManageServer rather than sent in the snapshot, and deliberately:
// the word list is the one thing on this server that is worth reading precisely
// because it says what not to write.
func handleAutoModGet(_ context.Context, s *Session, _ json.RawMessage) (any, *protocol.Error) {
	base, _ := s.Permissions()
	if !base.Has(permissions.ManageServer) {
		return nil, protocol.Errorf(protocol.ErrForbidden,
			"you are not allowed to see the automatic moderation rules")
	}
	return protocol.AutoModResult{Config: s.hub.AutoMod()}, nil
}

// handleAutoModUpdate replaces the whole rule set.
//
// Whole rather than per field, because the rules constrain one another — a list
// of words and what to do about them, a window and how many messages fit in it
// — and a half-applied edit is not a state worth being able to reach.
func handleAutoModUpdate(ctx context.Context, s *Session, raw json.RawMessage) (any, *protocol.Error) {
	req, failure := decode[protocol.AutoModUpdateRequest](raw)
	if failure != nil {
		return nil, failure
	}

	base, _ := s.Permissions()
	if !base.Has(permissions.ManageServer) {
		return nil, protocol.Errorf(protocol.ErrForbidden,
			"you are not allowed to change the automatic moderation rules")
	}

	// A role or channel that does not exist is dropped rather than refused: it
	// is nearly always one that was deleted while the settings screen was open,
	// and refusing the whole edit over it would be the wrong answer to that.
	config := req.Config
	config.ExemptRoles = s.hub.knownRoles(config.ExemptRoles)
	config.ExemptChannels = s.hub.knownChannels(config.ExemptChannels)
	config.Words.ExemptRoles = s.hub.knownRoles(config.Words.ExemptRoles)
	config.Links.ExemptRoles = s.hub.knownRoles(config.Links.ExemptRoles)
	config.Mentions.ExemptRoles = s.hub.knownRoles(config.Mentions.ExemptRoles)
	config.Caps.ExemptRoles = s.hub.knownRoles(config.Caps.ExemptRoles)
	config.Flood.ExemptRoles = s.hub.knownRoles(config.Flood.ExemptRoles)
	config.Repetition.ExemptRoles = s.hub.knownRoles(config.Repetition.ExemptRoles)

	config = normaliseAutoMod(config)

	previous := s.hub.AutoMod()
	if err := s.hub.st.SetAutoMod(ctx, config); err != nil {
		return nil, internalError(s, "save the automatic moderation rules", err)
	}
	s.hub.setAutoMod(config)

	// Only the people who may read the rules are told they changed. Everybody
	// else finds out the way the rules are meant to be found out: by writing
	// something that is not allowed.
	event := protocol.Event(protocol.EvAutoModUpdated, protocol.AutoModUpdatedEvent{Config: config})
	s.hub.BroadcastTo(event, func(other *Session) bool {
		otherBase, _ := other.Permissions()
		return otherBase.Has(permissions.ManageServer)
	})

	entry := auditTarget(protocol.AuditTargetServer, 0, "automod")
	entry.Action = protocol.AuditAutoModUpdate
	entry.Changes = autoModChanges(previous, config)
	s.hub.audit(ctx, s, entry)

	s.log.Info("automatic moderation updated",
		slog.Int64("by", s.UserID()), slog.Bool("enabled", config.Enabled))

	return protocol.AutoModResult{Config: config}, nil
}

// autoModChanges summarises an edit for the log.
//
// It records which rules went on or off and how big the word list is, not the
// words themselves: the log is read by more people than the settings screen is,
// and copying the list into it would publish exactly what it exists to keep
// out of messages.
func autoModChanges(before, after protocol.AutoModConfig) []store.AuditChange {
	var out []store.AuditChange
	out = changed(out, "enabled", boolWord(before.Enabled), boolWord(after.Enabled))
	out = changed(out, "words", ruleWord(before.Words.AutoModRule), ruleWord(after.Words.AutoModRule))
	out = changed(out, "wordCount",
		strconv.Itoa(len(before.Words.Words)), strconv.Itoa(len(after.Words.Words)))
	out = changed(out, "links", ruleWord(before.Links.AutoModRule), ruleWord(after.Links.AutoModRule))
	out = changed(out, "mentions", ruleWord(before.Mentions.AutoModRule), ruleWord(after.Mentions.AutoModRule))
	out = changed(out, "caps", ruleWord(before.Caps.AutoModRule), ruleWord(after.Caps.AutoModRule))
	out = changed(out, "flood", ruleWord(before.Flood.AutoModRule), ruleWord(after.Flood.AutoModRule))
	out = changed(out, "repetition", ruleWord(before.Repetition.AutoModRule), ruleWord(after.Repetition.AutoModRule))
	return out
}

func ruleWord(rule protocol.AutoModRule) string {
	if !rule.Enabled {
		return "off"
	}
	return rule.Action
}

func boolWord(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

// knownRoles drops the ids that name no role.
func (h *Hub) knownRoles(ids []int64) []int64 {
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := h.Role(id); ok {
			out = append(out, id)
		}
	}
	return out
}

// knownChannels drops the ids that name no channel.
func (h *Hub) knownChannels(ids []int64) []int64 {
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := h.Channel(id); ok {
			out = append(out, id)
		}
	}
	return out
}
