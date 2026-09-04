package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Conversation is one private thread, which is a pair of identities and
// nothing else: there is no name, no topic and no membership to manage,
// because there is never anybody to add.
//
// The pair is stored as the lower id and the higher one rather than as an
// opener and a recipient. Who wrote first stops mattering the moment somebody
// replies, and ordering the pair is what turns "is there already one of these"
// into a unique index instead of a search for a row that could have been
// written either way round.
type Conversation struct {
	ID       int64
	UserLow  int64
	UserHigh int64
	// LowReadID and HighReadID are the newest message each side has read. They
	// only ever move forwards, so a client paging through old lines cannot
	// bring back a badge that reading has already cleared.
	LowReadID     int64
	HighReadID    int64
	CreatedAt     int64
	LastMessageAt int64
}

// Involves reports whether an identity is one of the two participants.
func (c Conversation) Involves(userID int64) bool {
	return c.UserLow == userID || c.UserHigh == userID
}

// PeerOf is the other participant, which is the whole of how a conversation is
// named to the person reading it. It is zero for anybody not in it.
func (c Conversation) PeerOf(userID int64) int64 {
	switch userID {
	case c.UserLow:
		return c.UserHigh
	case c.UserHigh:
		return c.UserLow
	default:
		return 0
	}
}

// ReadIDFor is one participant's own read marker.
func (c Conversation) ReadIDFor(userID int64) int64 {
	if userID == c.UserLow {
		return c.LowReadID
	}
	return c.HighReadID
}

// DirectMessage is one line of a private conversation. It is a message in
// every way that matters to a reader, and differs from one posted in a channel
// only in what it hangs off: a conversation rather than a channel, which is
// why the two are separate tables rather than one with half its columns empty.
type DirectMessage struct {
	ID             int64
	ConversationID int64
	// UserID is nil once the author's account has been removed. Author holds
	// the name to show in that case.
	UserID    *int64
	Author    string
	Content   string
	CreatedAt int64
	EditedAt  *int64
	// ReplyToID is set on a message that replies to another line in the same
	// conversation. NULL is an ordinary message.
	ReplyToID *int64
}

// The author is resolved live from the users table, exactly as it is for a
// channel message, so a rename shows up throughout the conversation.
const directMessageColumns = `m.id, m.conversation_id, m.user_id,
	COALESCE(u.nickname, m.author), m.content, m.created_at, m.edited_at, m.reply_to_id`

const directMessageFrom = ` FROM direct_messages m LEFT JOIN users u ON u.id = m.user_id`

const conversationColumns = `id, user_low, user_high, low_read_id, high_read_id, created_at, last_message_at`

func scanConversation(row interface{ Scan(...any) error }) (Conversation, error) {
	var c Conversation
	err := row.Scan(&c.ID, &c.UserLow, &c.UserHigh, &c.LowReadID, &c.HighReadID, &c.CreatedAt, &c.LastMessageAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, ErrNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("store: scan conversation: %w", err)
	}
	return c, nil
}

func scanDirectMessage(row interface{ Scan(...any) error }) (DirectMessage, error) {
	var m DirectMessage
	err := row.Scan(&m.ID, &m.ConversationID, &m.UserID, &m.Author, &m.Content, &m.CreatedAt, &m.EditedAt, &m.ReplyToID)
	if errors.Is(err, sql.ErrNoRows) {
		return DirectMessage{}, ErrNotFound
	}
	if err != nil {
		return DirectMessage{}, fmt.Errorf("store: scan direct message: %w", err)
	}
	return m, nil
}

// orderPair puts two identities in the order the table stores them.
func orderPair(a, b int64) (low, high int64) {
	if a < b {
		return a, b
	}
	return b, a
}

// ConversationBetween finds the thread two identities share, if they have one.
func (s *Store) ConversationBetween(ctx context.Context, a, b int64) (Conversation, error) {
	low, high := orderPair(a, b)
	return scanConversation(s.db.QueryRowContext(ctx,
		`SELECT `+conversationColumns+` FROM dm_conversations WHERE user_low = ? AND user_high = ?`,
		low, high))
}

// ConversationByID loads one thread.
func (s *Store) ConversationByID(ctx context.Context, id int64) (Conversation, error) {
	return scanConversation(s.db.QueryRowContext(ctx,
		`SELECT `+conversationColumns+` FROM dm_conversations WHERE id = ?`, id))
}

// EnsureConversation returns the thread two identities share, opening one if
// this is the first thing either has said to the other.
//
// The insert is ignored rather than guarded by a lookup: two people writing to
// each other at the same moment is exactly the case a check-then-insert gets
// wrong, and the unique index already says which of the two writes wins.
func (s *Store) EnsureConversation(ctx context.Context, a, b int64) (Conversation, error) {
	low, high := orderPair(a, b)
	ts := now()
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO dm_conversations (user_low, user_high, created_at, last_message_at)
		 VALUES (?, ?, ?, ?)`, low, high, ts, ts); err != nil {
		return Conversation{}, fmt.Errorf("store: open conversation: %w", err)
	}
	return s.ConversationBetween(ctx, a, b)
}

// ConversationsFor lists the threads one identity is in, most recently spoken
// in first. A thread nobody has written in yet is still listed: opening one is
// how a client shows an empty conversation it is about to write into.
func (s *Store) ConversationsFor(ctx context.Context, userID int64, limit int) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+conversationColumns+` FROM dm_conversations
		 WHERE user_low = ? OR user_high = ?
		 ORDER BY last_message_at DESC, id DESC LIMIT ?`,
		userID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list conversations: %w", err)
	}
	defer rows.Close()

	out := make([]Conversation, 0, limit)
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list conversations: %w", err)
	}
	return out, nil
}

// CreateDirectMessage stores one line and returns it as it will be rendered.
//
// It also moves the sender's own read marker to the message just written.
// Without that, everything you send counts as something you have not read, and
// a badge on your own conversation is the one thing it can never mean.
func (s *Store) CreateDirectMessage(ctx context.Context, conversationID, userID int64, content string, replyToID *int64) (DirectMessage, error) {
	var created DirectMessage
	err := s.tx(ctx, func(tx *sql.Tx) error {
		ts := now()
		res, err := tx.ExecContext(ctx,
			`INSERT INTO direct_messages (conversation_id, user_id, author, content, reply_to_id, created_at)
			 VALUES (?, ?, (SELECT nickname FROM users WHERE id = ?), ?, ?, ?)`,
			conversationID, userID, userID, content, replyToID, ts)
		if err != nil {
			return fmt.Errorf("store: create direct message: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: create direct message: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE dm_conversations
			    SET last_message_at = ?,
			        low_read_id  = CASE WHEN user_low  = ? THEN ? ELSE low_read_id  END,
			        high_read_id = CASE WHEN user_high = ? THEN ? ELSE high_read_id END
			  WHERE id = ?`,
			ts, userID, id, userID, id, conversationID); err != nil {
			return fmt.Errorf("store: touch conversation: %w", err)
		}
		created, err = scanDirectMessage(tx.QueryRowContext(ctx,
			`SELECT `+directMessageColumns+directMessageFrom+` WHERE m.id = ?`, id))
		return err
	})
	if err != nil {
		return DirectMessage{}, err
	}
	return created, nil
}

// DirectMessageByID loads one line.
func (s *Store) DirectMessageByID(ctx context.Context, id int64) (DirectMessage, error) {
	return scanDirectMessage(s.db.QueryRowContext(ctx,
		`SELECT `+directMessageColumns+directMessageFrom+` WHERE m.id = ?`, id))
}

// RepliesForDirectMessages loads the referenced direct messages for the given
// reply IDs in a single query, returning them keyed by their own message ID.
func (s *Store) RepliesForDirectMessages(ctx context.Context, replyIDs []int64) (map[int64]DirectMessage, error) {
	if len(replyIDs) == 0 {
		return map[int64]DirectMessage{}, nil
	}
	seen := make(map[int64]struct{}, len(replyIDs))
	unique := make([]any, 0, len(replyIDs))
	for _, id := range replyIDs {
		if id <= 0 {
			continue
		}
		if _, exists := seen[id]; !exists {
			seen[id] = struct{}{}
			unique = append(unique, id)
		}
	}
	if len(unique) == 0 {
		return map[int64]DirectMessage{}, nil
	}

	query := fmt.Sprintf(`SELECT %s%s WHERE m.id IN (%s)`,
		directMessageColumns, directMessageFrom, placeholders(len(unique)))
	rows, err := s.db.QueryContext(ctx, query, unique...)
	if err != nil {
		return nil, fmt.Errorf("store: read reply direct messages: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]DirectMessage, len(unique))
	for rows.Next() {
		m, err := scanDirectMessage(rows)
		if err != nil {
			return nil, err
		}
		out[m.ID] = m
	}
	return out, rows.Err()
}

// DirectMessagesBefore reads one page of a conversation, newest first. A zero
// before starts at the newest line.
func (s *Store) DirectMessagesBefore(ctx context.Context, conversationID, before int64, limit int) ([]DirectMessage, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if before > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+directMessageColumns+directMessageFrom+
				` WHERE m.conversation_id = ? AND m.id < ? ORDER BY m.id DESC LIMIT ?`,
			conversationID, before, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+directMessageColumns+directMessageFrom+
				` WHERE m.conversation_id = ? ORDER BY m.id DESC LIMIT ?`,
			conversationID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read conversation: %w", err)
	}
	return scanDirectMessages(rows, limit)
}

// DirectMessagesAfter reads one page forwards from after, oldest first.
func (s *Store) DirectMessagesAfter(ctx context.Context, conversationID, after int64, limit int) ([]DirectMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+directMessageColumns+directMessageFrom+
			` WHERE m.conversation_id = ? AND m.id > ? ORDER BY m.id ASC LIMIT ?`,
		conversationID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("store: read conversation: %w", err)
	}
	return scanDirectMessages(rows, limit)
}

// DirectMessagesAround reads a page centred on one line, oldest first.
func (s *Store) DirectMessagesAround(ctx context.Context, conversationID, id int64, limit int) ([]DirectMessage, error) {
	older, err := s.DirectMessagesBefore(ctx, conversationID, id+1, limit/2+1)
	if err != nil {
		return nil, err
	}
	newer, err := s.DirectMessagesAfter(ctx, conversationID, id, limit-len(older))
	if err != nil {
		return nil, err
	}

	out := make([]DirectMessage, 0, len(older)+len(newer))
	for i := len(older) - 1; i >= 0; i-- {
		out = append(out, older[i])
	}
	return append(out, newer...), nil
}

// HasDirectMessagesBefore reports whether anything older than id remains.
func (s *Store) HasDirectMessagesBefore(ctx context.Context, conversationID, id int64) (bool, error) {
	return s.probeHistory(ctx,
		`SELECT EXISTS(SELECT 1 FROM direct_messages WHERE conversation_id = ? AND id < ?)`,
		conversationID, id)
}

// HasDirectMessagesAfter reports whether anything newer than id remains.
func (s *Store) HasDirectMessagesAfter(ctx context.Context, conversationID, id int64) (bool, error) {
	return s.probeHistory(ctx,
		`SELECT EXISTS(SELECT 1 FROM direct_messages WHERE conversation_id = ? AND id > ?)`,
		conversationID, id)
}

// UpdateDirectMessageContent rewrites one line and stamps it as edited.
func (s *Store) UpdateDirectMessageContent(ctx context.Context, id int64, content string) (DirectMessage, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE direct_messages SET content = ?, edited_at = ? WHERE id = ?`, content, now(), id)
	if err != nil {
		return DirectMessage{}, fmt.Errorf("store: edit direct message: %w", err)
	}
	if err := requireOneRow(res, "direct message"); err != nil {
		return DirectMessage{}, err
	}
	return s.DirectMessageByID(ctx, id)
}

// DeleteDirectMessage removes one line.
func (s *Store) DeleteDirectMessage(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM direct_messages WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete direct message: %w", err)
	}
	return requireOneRow(res, "direct message")
}

// LastDirectMessages reads the newest line of each of several conversations in
// one query, which is what a list of threads is rendered from: the preview
// under a name is the last thing that was said in it.
func (s *Store) LastDirectMessages(ctx context.Context, conversationIDs []int64) (map[int64]DirectMessage, error) {
	if len(conversationIDs) == 0 {
		return map[int64]DirectMessage{}, nil
	}
	in := placeholders(len(conversationIDs))
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+directMessageColumns+directMessageFrom+
			` WHERE m.id IN (SELECT MAX(id) FROM direct_messages
			                  WHERE conversation_id IN (`+in+`) GROUP BY conversation_id)`,
		idArgs(conversationIDs)...)
	if err != nil {
		return nil, fmt.Errorf("store: read conversation previews: %w", err)
	}
	page, err := scanDirectMessages(rows, len(conversationIDs))
	if err != nil {
		return nil, err
	}
	out := make(map[int64]DirectMessage, len(page))
	for _, m := range page {
		out[m.ConversationID] = m
	}
	return out, nil
}

// UnreadDirectCounts is how many lines each of somebody's conversations holds
// past their own read marker.
//
// Sending moves that marker, so what this counts is always somebody else's
// writing. A conversation with nothing unread is absent rather than zero.
func (s *Store) UnreadDirectCounts(ctx context.Context, userID int64) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, COUNT(m.id)
		   FROM dm_conversations c
		   JOIN direct_messages m
		     ON m.conversation_id = c.id
		    AND m.id > (CASE WHEN c.user_low = ? THEN c.low_read_id ELSE c.high_read_id END)
		  WHERE c.user_low = ? OR c.user_high = ?
		  GROUP BY c.id`, userID, userID, userID)
	if err != nil {
		return nil, fmt.Errorf("store: count unread conversations: %w", err)
	}
	defer rows.Close()

	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, fmt.Errorf("store: count unread conversations: %w", err)
		}
		out[id] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: count unread conversations: %w", err)
	}
	return out, nil
}

// UnreadDirectCount is how much of one conversation sits past one
// participant's read marker, which is the badge on a single thread.
func (s *Store) UnreadDirectCount(ctx context.Context, conversationID, userID int64) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*)
		   FROM direct_messages m
		   JOIN dm_conversations c ON c.id = m.conversation_id
		  WHERE c.id = ?
		    AND m.id > (CASE WHEN c.user_low = ? THEN c.low_read_id ELSE c.high_read_id END)`,
		conversationID, userID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: count unread conversation: %w", err)
	}
	return count, nil
}

// MarkConversationRead moves one participant's read marker up to messageID.
// The marker never moves backwards, so paging through old lines cannot bring
// back a badge that reading has already cleared.
func (s *Store) MarkConversationRead(ctx context.Context, conversationID, userID, messageID int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE dm_conversations
		    SET low_read_id  = CASE WHEN user_low  = ? AND ? > low_read_id  THEN ? ELSE low_read_id  END,
		        high_read_id = CASE WHEN user_high = ? AND ? > high_read_id THEN ? ELSE high_read_id END
		  WHERE id = ?`,
		userID, messageID, messageID, userID, messageID, messageID, conversationID)
	if err != nil {
		return fmt.Errorf("store: mark conversation read: %w", err)
	}
	return requireOneRow(res, "conversation")
}

// scanDirectMessages drains a direct message query. size is only a slice hint.
func scanDirectMessages(rows *sql.Rows, size int) ([]DirectMessage, error) {
	defer rows.Close()

	out := make([]DirectMessage, 0, size)
	for rows.Next() {
		m, err := scanDirectMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read direct messages: %w", err)
	}
	return out, nil
}
