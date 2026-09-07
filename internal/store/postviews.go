package store

import (
	"context"
	"fmt"
)

// Which entries somebody has opened.
//
// This is the per-item twin of a channel's read marker, and it exists because
// a marker cannot answer the question a wall of pictures raises. A marker is a
// frontier — everything before it is read — which is exactly right for a
// conversation, where the lines arrive in the order they are read. A media
// channel is not read in that order: somebody opens the picture that caught
// their eye, then another three rows down, then comes back tomorrow for the
// rest. What is wanted there is the set of what has been seen, not the point
// the eye has reached.
//
// So a row per opened post, and no row at all for anything else. Two things
// stand in for the rows that were never written:
//
//   - The member's own entries. Somebody has seen what they posted, and
//     writing a row to say so would be bookkeeping about nothing.
//   - Anything older than the member. `users.unread_epoch` is the newest
//     message that existed when they first appeared, and a post's body is a
//     message, so a body at or below the epoch is a post from before they
//     arrived. It was never theirs to catch up on.
//
// The second is also what makes the feature arrive quietly on a server that
// already has a gallery in it: on the morning after the upgrade, nothing is
// suddenly new.

// viewedPredicate decides whether one post counts as seen by one member. It is
// written against `p` (posts), `u` (users) and `v` (that member's post_views
// row, outer joined).
const viewedPredicate = `(
	   v.post_id IS NOT NULL
	OR p.user_id = u.id
	OR (p.root_message_id IS NOT NULL AND p.root_message_id <= u.unread_epoch)
)`

// MarkPostsViewed records that one member has opened these entries.
//
// The channel is part of the statement rather than checked before it: a caller
// naming a post in a channel they cannot see marks nothing, which is the same
// answer they would get from asking about it any other way. Marking something
// already marked keeps the first time, because that is when it was seen.
func (s *Store) MarkPostsViewed(ctx context.Context, userID, channelID int64, postIDs []int64, at int64) error {
	if len(postIDs) == 0 {
		return nil
	}

	args := append([]any{userID, at}, idArgs(postIDs)...)
	args = append(args, channelID)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO post_views (post_id, user_id, viewed_at)
		 SELECT p.id, ?, ? FROM posts p
		  WHERE p.id IN (`+placeholders(len(postIDs))+`)
		    AND p.channel_id = ?
		 ON CONFLICT (post_id, user_id) DO NOTHING`,
		args...); err != nil {
		return fmt.Errorf("store: mark posts viewed: %w", err)
	}
	return nil
}

// ViewedPosts reports which of the named entries one member has already seen.
// A post that has not been is absent rather than false.
func (s *Store) ViewedPosts(ctx context.Context, userID int64, postIDs []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(postIDs) == 0 {
		return out, nil
	}

	args := append([]any{userID}, idArgs(postIDs)...)
	rows, err := s.db.QueryContext(ctx,
		`SELECT p.id
		   FROM posts p
		   JOIN users u ON u.id = ?
		   LEFT JOIN post_views v ON v.post_id = p.id AND v.user_id = u.id
		  WHERE p.id IN (`+placeholders(len(postIDs))+`)
		    AND `+viewedPredicate,
		args...)
	if err != nil {
		return nil, fmt.Errorf("store: read viewed posts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: read viewed posts: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read viewed posts: %w", err)
	}
	return out, nil
}
