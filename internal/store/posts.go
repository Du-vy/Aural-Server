package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aural-chat/aural-server/internal/protocol"
)

// Post is one entry of a channel that holds entries rather than a stream: an
// announcement, a forum topic, a media item, a calendar event.
//
// The words and the files are not here. They are the first message of the
// thread, named by RootMessageID, and the comments are the rest of the
// messages carrying this post's id. That is what lets a post inherit
// attachments, edits, deletion and moderation from messages rather than
// reimplement any of them.
type Post struct {
	ID        int64
	ChannelID int64
	// UserID is nil once the author's account has been removed. Author holds
	// the name to show in that case.
	UserID *int64
	Author string
	Title  string
	// RootMessageID is nil only for a post whose body was removed by a
	// moderation purge, which leaves the title and the thread standing.
	RootMessageID *int64
	Locked        bool
	Pinned        bool
	CreatedAt     int64
	EditedAt      *int64
	// StartsAt is set on, and only on, a calendar post. It is what a query
	// asks about rather than the channel type, which lives on another table.
	StartsAt *int64
	EndsAt   *int64
	AllDay   bool
	Location string
}

// Event reports whether the post happens at a time.
func (p Post) Event() bool { return p.StartsAt != nil }

// PostStats is what a listing needs to know about a thread without reading it:
// how long it is, and when it was last added to.
type PostStats struct {
	Comments      int
	LastCommentAt int64
}

// PostRSVPCounts tallies the answers to a calendar post.
type PostRSVPCounts struct {
	Going    int
	Maybe    int
	Declined int
}

// postColumns resolves the author live from the users table for the same
// reason messageColumns does: the row is the person, so a rename shows up
// throughout the history rather than only on what is written next.
const postColumns = `p.id, p.channel_id, p.user_id, COALESCE(u.nickname, p.author),
	p.title, p.root_message_id, p.locked, p.pinned, p.created_at, p.edited_at,
	p.starts_at, p.ends_at, p.all_day, p.location`

const postFrom = ` FROM posts p LEFT JOIN users u ON u.id = p.user_id`

func scanPost(row interface{ Scan(...any) error }) (Post, error) {
	var p Post
	err := row.Scan(&p.ID, &p.ChannelID, &p.UserID, &p.Author, &p.Title, &p.RootMessageID,
		&p.Locked, &p.Pinned, &p.CreatedAt, &p.EditedAt,
		&p.StartsAt, &p.EndsAt, &p.AllDay, &p.Location)
	if errors.Is(err, sql.ErrNoRows) {
		return Post{}, ErrNotFound
	}
	if err != nil {
		return Post{}, fmt.Errorf("store: scan post: %w", err)
	}
	return p, nil
}

func scanPosts(rows *sql.Rows, size int) ([]Post, error) {
	defer rows.Close()

	out := make([]Post, 0, size)
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read posts: %w", err)
	}
	return out, nil
}

// CreatePost writes a post and the message that is its body, and returns both.
//
// The two rows point at each other, so they are written in one transaction:
// the message needs the post's id to belong to the thread, and the post needs
// the message's id to know where its body is. A failure at either step leaves
// neither, which is what keeps a post from existing with nothing in it.
func (s *Store) CreatePost(ctx context.Context, p Post, content string) (Post, Message, error) {
	ts := now()
	var postID, bodyID int64

	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO posts (channel_id, user_id, author, title, created_at,
				starts_at, ends_at, all_day, location)
			 VALUES (?, ?, (SELECT nickname FROM users WHERE id = ?), ?, ?, ?, ?, ?, ?)`,
			p.ChannelID, p.UserID, p.UserID, p.Title, ts,
			p.StartsAt, p.EndsAt, p.AllDay, p.Location)
		if err != nil {
			return fmt.Errorf("store: create post: %w", err)
		}
		if postID, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("store: create post: %w", err)
		}

		bodyID, err = insertMessage(ctx, tx, p.ChannelID, &postID, *p.UserID, content, nil, ts)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE posts SET root_message_id = ? WHERE id = ?`, bodyID, postID); err != nil {
			return fmt.Errorf("store: create post: %w", err)
		}
		return nil
	})
	if err != nil {
		return Post{}, Message{}, err
	}

	created, err := s.PostByID(ctx, postID)
	if err != nil {
		return Post{}, Message{}, err
	}
	body, err := s.MessageByID(ctx, bodyID)
	if err != nil {
		return Post{}, Message{}, err
	}
	return created, body, nil
}

// PostByID loads one post.
func (s *Store) PostByID(ctx context.Context, id int64) (Post, error) {
	return scanPost(s.db.QueryRowContext(ctx,
		`SELECT `+postColumns+postFrom+` WHERE p.id = ?`, id))
}

// PostsBefore reads one page of a channel's entries, newest first.
//
// It pages by id alone, and pinning takes no part in that order: a cursor that
// moved rows around as they were pinned would hand a client the same entry
// twice and skip another. Pinned entries are read separately, by PinnedPosts,
// and floated to the top of what a client holds.
func (s *Store) PostsBefore(ctx context.Context, channelID, before int64, limit int) ([]Post, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if before > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+postColumns+postFrom+
				` WHERE p.channel_id = ? AND p.id < ? ORDER BY p.id DESC LIMIT ?`,
			channelID, before, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+postColumns+postFrom+
				` WHERE p.channel_id = ? ORDER BY p.id DESC LIMIT ?`,
			channelID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list posts: %w", err)
	}
	return scanPosts(rows, limit)
}

// PinnedPosts reads the pinned entries of a channel, newest first. They travel
// with the first page of a listing however old they are, which is the whole
// point of pinning one.
func (s *Store) PinnedPosts(ctx context.Context, channelID int64, limit int) ([]Post, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+postColumns+postFrom+
			` WHERE p.channel_id = ? AND p.pinned = 1 ORDER BY p.id DESC LIMIT ?`,
		channelID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list pinned posts: %w", err)
	}
	return scanPosts(rows, limit)
}

// PostsInRange reads the calendar entries that start inside a window, earliest
// first. It is how a client asks for a month rather than for a page.
func (s *Store) PostsInRange(ctx context.Context, channelID, from, to int64, limit int) ([]Post, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+postColumns+postFrom+
			` WHERE p.channel_id = ? AND p.starts_at IS NOT NULL
			   AND p.starts_at >= ? AND p.starts_at < ?
			 ORDER BY p.starts_at ASC, p.id ASC LIMIT ?`,
		channelID, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list posts in range: %w", err)
	}
	return scanPosts(rows, limit)
}

// HasPostsBefore reports whether anything older than id remains, which is what
// tells a client another page is worth asking for.
func (s *Store) HasPostsBefore(ctx context.Context, channelID, id int64) (bool, error) {
	return s.probeHistory(ctx,
		`SELECT EXISTS(SELECT 1 FROM posts WHERE channel_id = ? AND id < ?)`, channelID, id)
}

// UpdatePost writes back a post the caller has already patched. The body is
// not here: it is a message, and it is edited as one.
func (s *Store) UpdatePost(ctx context.Context, p Post) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE posts SET title = ?, locked = ?, pinned = ?, edited_at = ?,
			starts_at = ?, ends_at = ?, all_day = ?, location = ? WHERE id = ?`,
		p.Title, p.Locked, p.Pinned, now(), p.StartsAt, p.EndsAt, p.AllDay, p.Location, p.ID)
	if err != nil {
		return fmt.Errorf("store: update post: %w", err)
	}
	return requireOneRow(res, "post")
}

// DeletePost removes a post, and with it every message of its thread and every
// file those messages carried.
func (s *Store) DeletePost(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM posts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete post: %w", err)
	}
	return requireOneRow(res, "post")
}

// DeletedPostTarget names a post a purge removed, so the gateway can tell
// clients precisely what disappeared.
type DeletedPostTarget struct {
	ID        int64
	ChannelID int64
}

// DeletePostsByUser removes the posts one user wrote, optionally only those
// newer than a cutoff in seconds.
//
// It is the post half of the message purge a kick can carry: deleting
// everything somebody wrote would otherwise leave their posts standing with no
// words in them.
func (s *Store) DeletePostsByUser(ctx context.Context, userID, cutoff int64) ([]DeletedPostTarget, error) {
	where := `user_id = ?`
	args := []any{userID}
	if cutoff > 0 {
		where += ` AND created_at >= ?`
		args = append(args, cutoff)
	}

	targets, err := s.postTargets(ctx, `SELECT id, channel_id FROM posts WHERE `+where, args)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM posts WHERE `+where, args...); err != nil {
		return nil, fmt.Errorf("store: delete posts: %w", err)
	}
	return targets, nil
}

func (s *Store) postTargets(ctx context.Context, query string, args []any) ([]DeletedPostTarget, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list posts: %w", err)
	}
	defer rows.Close()

	var out []DeletedPostTarget
	for rows.Next() {
		var t DeletedPostTarget
		if err := rows.Scan(&t.ID, &t.ChannelID); err != nil {
			return nil, fmt.Errorf("store: scan post: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list posts: %w", err)
	}
	return out, nil
}

// PostStatsFor reads the thread length and last activity of a set of posts in
// one query, so a listing costs the same handful of queries however long it is.
//
// The body is not a comment, so it is excluded in the query rather than
// subtracted afterwards: a post whose body was purged has exactly as many
// comments as it has messages.
func (s *Store) PostStatsFor(ctx context.Context, postIDs []int64) (map[int64]PostStats, error) {
	out := map[int64]PostStats{}
	if len(postIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.post_id, COUNT(*), MAX(m.created_at)
		 FROM messages m JOIN posts p ON p.id = m.post_id
		 WHERE m.post_id IN (`+placeholders(len(postIDs))+`)
		   AND (p.root_message_id IS NULL OR m.id <> p.root_message_id)
		 GROUP BY m.post_id`, idArgs(postIDs)...)
	if err != nil {
		return nil, fmt.Errorf("store: read post stats: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			postID int64
			stats  PostStats
		)
		if err := rows.Scan(&postID, &stats.Comments, &stats.LastCommentAt); err != nil {
			return nil, fmt.Errorf("store: read post stats: %w", err)
		}
		out[postID] = stats
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read post stats: %w", err)
	}
	return out, nil
}

// AttachmentsForPost lists every file in a post's thread.
//
// It is read before a post is deleted, because the rows go with it through the
// cascade and what was held on disk has to be known while they still exist.
func (s *Store) AttachmentsForPost(ctx context.Context, postID int64) ([]Attachment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+attachmentColumns+` FROM attachments
		 WHERE message_id IN (SELECT id FROM messages WHERE post_id = ?)`, postID)
	if err != nil {
		return nil, fmt.Errorf("store: list post attachments: %w", err)
	}
	return scanAttachments(rows)
}

// SetRSVP records one answer to a calendar post, replacing whatever that
// identity had answered before.
func (s *Store) SetRSVP(ctx context.Context, postID, userID int64, response string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO post_rsvps (post_id, user_id, response, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(post_id, user_id) DO UPDATE SET response = excluded.response`,
		postID, userID, response, now())
	if err != nil {
		return fmt.Errorf("store: set rsvp: %w", err)
	}
	return nil
}

// DeleteRSVP withdraws an answer. Withdrawing one that was never given is not
// an error: the end state is what the caller asked for either way.
func (s *Store) DeleteRSVP(ctx context.Context, postID, userID int64) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM post_rsvps WHERE post_id = ? AND user_id = ?`, postID, userID); err != nil {
		return fmt.Errorf("store: withdraw rsvp: %w", err)
	}
	return nil
}

// RSVPCountsFor tallies the answers to a set of posts in one query.
func (s *Store) RSVPCountsFor(ctx context.Context, postIDs []int64) (map[int64]PostRSVPCounts, error) {
	out := map[int64]PostRSVPCounts{}
	if len(postIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT post_id, response, COUNT(*) FROM post_rsvps
		 WHERE post_id IN (`+placeholders(len(postIDs))+`)
		 GROUP BY post_id, response`, idArgs(postIDs)...)
	if err != nil {
		return nil, fmt.Errorf("store: tally rsvps: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			postID   int64
			response string
			count    int
		)
		if err := rows.Scan(&postID, &response, &count); err != nil {
			return nil, fmt.Errorf("store: tally rsvps: %w", err)
		}
		counts := out[postID]
		switch response {
		case protocol.RSVPGoing:
			counts.Going = count
		case protocol.RSVPMaybe:
			counts.Maybe = count
		case protocol.RSVPDeclined:
			counts.Declined = count
		}
		out[postID] = counts
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: tally rsvps: %w", err)
	}
	return out, nil
}

// RSVPsOf reads one identity's own answers to a set of posts.
func (s *Store) RSVPsOf(ctx context.Context, userID int64, postIDs []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(postIDs) == 0 {
		return out, nil
	}
	args := append([]any{userID}, idArgs(postIDs)...)
	rows, err := s.db.QueryContext(ctx,
		`SELECT post_id, response FROM post_rsvps
		 WHERE user_id = ? AND post_id IN (`+placeholders(len(postIDs))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read own rsvps: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			postID   int64
			response string
		)
		if err := rows.Scan(&postID, &response); err != nil {
			return nil, fmt.Errorf("store: read own rsvps: %w", err)
		}
		out[postID] = response
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read own rsvps: %w", err)
	}
	return out, nil
}
