package gateway

import (
	"net/url"
	"strconv"

	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// formatID renders an identity as the string a ban match is keyed by. Ban
// handles are text — an address, a hash — so the account is stored as text too
// rather than giving the table a fourth column that is only ever set for one
// of the three kinds.
func formatID(id int64) string { return strconv.FormatInt(id, 10) }

// banView converts a stored ban into its wire form.
//
// The handles are counted, never sent. An address and a device hash identify
// somebody outside this server as well as inside it, and a moderator deciding
// whether to lift a ban needs to know that it reaches a machine, not which
// machine it reaches.
func banView(b store.Ban, at int64) protocol.Ban {
	counts := map[string]int{}
	for _, m := range b.Matches {
		counts[m.Kind]++
	}
	matches := make([]protocol.BanMatchSummary, 0, len(counts))
	for _, kind := range []string{protocol.MatchUser, protocol.MatchIP, protocol.MatchDevice} {
		if n := counts[kind]; n > 0 {
			matches = append(matches, protocol.BanMatchSummary{Kind: kind, Count: n})
		}
	}

	return protocol.Ban{
		ID:            b.ID,
		UserID:        b.UserID,
		UserNickname:  b.UserNickname,
		UserUsername:  b.UserUsername,
		ActorID:       b.ActorID,
		ActorNickname: b.ActorNickname,
		Reason:        b.Reason,
		CreatedAt:     b.CreatedAt,
		ExpiresAt:     b.ExpiresAt,
		Matches:       matches,
		Active:        b.Active(at),
	}
}

// auditView converts a stored log entry into its wire form.
func auditView(e store.AuditEntry) protocol.AuditEntry {
	out := protocol.AuditEntry{
		ID:         e.ID,
		ActorID:    e.ActorID,
		ActorName:  e.ActorName,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		TargetName: e.TargetName,
		Reason:     e.Reason,
		CreatedAt:  e.CreatedAt,
	}
	for _, c := range e.Changes {
		out.Changes = append(out.Changes, protocol.AuditChange{
			Key: c.Key, Before: c.Before, After: c.After,
		})
	}
	return out
}

// expressionView converts a stored emoji or sticker into its wire form. The URL
// is relative and carries the filename last, exactly as an attachment's does,
// so every way of reaching this server builds the same working link.
func expressionView(e store.Expression) protocol.Expression {
	return protocol.Expression{
		ID:        e.ID,
		Kind:      e.Kind,
		Name:      e.Name,
		URL:       uploadPrefix + e.StorageKey + "/" + url.PathEscape(e.Filename),
		Animated:  e.Animated,
		Size:      strconv.FormatInt(e.Size, 10),
		CreatorID: e.CreatorID,
		CreatedAt: e.CreatedAt,
	}
}

// soundView converts a stored soundboard clip into its wire form.
func soundView(s store.Sound) protocol.Sound {
	return protocol.Sound{
		ID:         s.ID,
		Name:       s.Name,
		Emoji:      s.Emoji,
		URL:        uploadPrefix + s.StorageKey + "/" + url.PathEscape(s.Filename),
		DurationMs: s.DurationMs,
		Volume:     s.Volume,
		Size:       strconv.FormatInt(s.Size, 10),
		CreatorID:  s.CreatorID,
		CreatedAt:  s.CreatedAt,
	}
}
