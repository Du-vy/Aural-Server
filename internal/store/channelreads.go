package store

import (
	"context"
	"fmt"
)

// ChannelUnread is how much of one channel sits past somebody's read marker.
//
// It is the channel-shaped twin of the badge a private thread carries, and it
// exists for the same reason: a client that has just connected has to know
// what is waiting for it before it opens anything.
type ChannelUnread struct {
	ChannelID int64
	Count     int
	// LastReadID is the marker the count was taken from — the effective one,
	// so a channel with no row of its own reports the member's epoch rather
	// than zero.
	LastReadID int64
}

// UnreadMention is the little of an unread message that deciding the colour of
// a badge needs.
//
// Deliberately not a Message. The server has no notion of a mention — a
// mention is the name it names, resolved against whoever the client knows when
// it is drawn, which is why renaming somebody renames them throughout the
// history — so what travels is the text, and the client decides what it names.
// Sending whole message views instead would carry attachments, embeds and
// reply previews that nothing here reads, for messages nobody has asked to
// see.
type UnreadMention struct {
	ChannelID int64
	// Content is the words to be scanned. For the body of a post it is the
	// title and the body together, in that order, because a client counting
	// the post live scans both.
	Content string
	// UserID is the author, or 0 for a webhook. It is here so a client can
	// leave out its own writing without a second query.
	UserID int64
	// ReplyToUserID is who the message answers, or 0. Answering somebody
	// reaches as far as writing their name out would, and it is the one route
	// to naming a person that is a field rather than a convention over the
	// words.
	ReplyToUserID int64
}

// effectiveMarker is where counting starts for one member in one channel.
//
// A row in channel_reads is the marker once they have read the channel. With
// no row the epoch on the member stands in, which is the newest message that
// existed when they first appeared: history from before somebody joined starts
// read, and a channel created after they joined starts unread to its first
// line. MAX of the two rather than the row alone so a marker can never be
// dragged behind the epoch.
const effectiveMarker = `MAX(COALESCE(cr.last_read_id, 0), u.unread_epoch)`

// unreadFrom is the join every unread query walks: one member, the messages of
// the channels asked about, and that member's marker in each.
const unreadFrom = `
	   FROM users u
	   JOIN messages m
	     ON m.channel_id IN (%s)
	   LEFT JOIN channel_reads cr
	     ON cr.user_id = u.id AND cr.channel_id = m.channel_id`

// UnreadChannelCounts is how many messages each of the named channels holds
// past one member's marker. A channel with nothing waiting is absent rather
// than zero, exactly as an idle conversation is.
//
// Posts are counted through the message rows they hang off: a post's body is a
// message, and so is every comment under it, so one count over messages gives
// the same number a client accumulates live from post and comment events.
func (s *Store) UnreadChannelCounts(ctx context.Context, userID int64, channelIDs []int64) (map[int64]ChannelUnread, error) {
	out := map[int64]ChannelUnread{}
	if len(channelIDs) == 0 {
		return out, nil
	}

	args := append(idArgs(channelIDs), userID)
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.channel_id, COUNT(*), `+effectiveMarker+
			fmt.Sprintf(unreadFrom, placeholders(len(channelIDs)))+`
		  WHERE u.id = ?
		    AND m.id > `+effectiveMarker+`
		  GROUP BY m.channel_id`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("store: count unread channels: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var entry ChannelUnread
		if err := rows.Scan(&entry.ChannelID, &entry.Count, &entry.LastReadID); err != nil {
			return nil, fmt.Errorf("store: count unread channels: %w", err)
		}
		out[entry.ChannelID] = entry
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: count unread channels: %w", err)
	}
	return out, nil
}

// UnreadChannelMentions reads the newest unread messages across the named
// channels, most recent first, so a client can work out which badges name it.
//
// It is capped rather than complete. The cap is what keeps a member returning
// to a busy weekend from being handed every line of it at the door, and what
// the cap costs is narrow: a channel with more waiting than it reaches keeps
// its count and loses only the highlight on its oldest end.
func (s *Store) UnreadChannelMentions(ctx context.Context, userID int64, channelIDs []int64, limit int) ([]UnreadMention, error) {
	if len(channelIDs) == 0 || limit <= 0 {
		return nil, nil
	}

	args := append(idArgs(channelIDs), userID, limit)
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.channel_id,
		        CASE
		          WHEN p.id IS NULL   THEN m.content
		          WHEN m.content = '' THEN p.title
		          ELSE p.title || ': ' || m.content
		        END,
		        COALESCE(m.user_id, 0),
		        COALESCE(r.user_id, 0)`+
			fmt.Sprintf(unreadFrom, placeholders(len(channelIDs)))+`
		   -- The post this message is the body of, when it is one, for its
		   -- title. A comment joins nothing here: it hangs off its post
		   -- through post_id, not through being that post's root.
		   LEFT JOIN posts p ON p.root_message_id = m.id
		   -- The message being answered, for its author alone. A reply whose
		   -- message has since been deleted joins nothing and so names
		   -- nobody, which is what a client holding a dead reference decides
		   -- as well.
		   LEFT JOIN messages r ON r.id = m.reply_to_id
		  WHERE u.id = ?
		    AND m.id > `+effectiveMarker+`
		  ORDER BY m.id DESC
		  LIMIT ?`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("store: read unread channel messages: %w", err)
	}
	defer rows.Close()

	out := make([]UnreadMention, 0, limit)
	for rows.Next() {
		var entry UnreadMention
		if err := rows.Scan(&entry.ChannelID, &entry.Content, &entry.UserID, &entry.ReplyToUserID); err != nil {
			return nil, fmt.Errorf("store: read unread channel messages: %w", err)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read unread channel messages: %w", err)
	}
	return out, nil
}

// MarkChannelRead moves one member's marker in one channel up to messageID, or
// up to whatever is newest there when messageID is 0 — which is what "I am
// looking at this channel" means, and all a client has to say to clear a badge.
//
// The marker never moves backwards, so paging back through old lines cannot
// bring back a badge that reading has already cleared, and a second client
// still catching up cannot undo what the first one read.
func (s *Store) MarkChannelRead(ctx context.Context, userID, channelID, messageID int64) (ChannelUnread, error) {
	if messageID <= 0 {
		if err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(id), 0) FROM messages WHERE channel_id = ?`,
			channelID).Scan(&messageID); err != nil {
			return ChannelUnread{}, fmt.Errorf("store: mark channel read: %w", err)
		}
	}

	// The insert carries the epoch as its floor for the reason the read side
	// does: marking an empty channel read must not leave a marker sitting
	// behind history that already counted as read.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO channel_reads (user_id, channel_id, last_read_id)
		 SELECT u.id, ?, MAX(?, u.unread_epoch) FROM users u WHERE u.id = ?
		 ON CONFLICT (user_id, channel_id) DO UPDATE
		    SET last_read_id = MAX(excluded.last_read_id, channel_reads.last_read_id)`,
		channelID, messageID, userID); err != nil {
		return ChannelUnread{}, fmt.Errorf("store: mark channel read: %w", err)
	}

	counts, err := s.UnreadChannelCounts(ctx, userID, []int64{channelID})
	if err != nil {
		return ChannelUnread{}, err
	}
	if entry, ok := counts[channelID]; ok {
		return entry, nil
	}
	return ChannelUnread{ChannelID: channelID, Count: 0, LastReadID: messageID}, nil
}
