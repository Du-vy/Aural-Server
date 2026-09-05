package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentReadsAndWrites drives the database the way the gateway does:
// several histories being paged and searched while messages are still being
// written, all at once.
//
// It is here because the pool is open. With one connection none of this could
// interleave and so none of it could go wrong; with several, a write
// transaction that reads before it writes is exactly the shape SQLite refuses
// with SQLITE_BUSY_SNAPSHOT when the transaction is deferred. Every write path
// below therefore has to come back clean, not merely eventually.
func TestConcurrentReadsAndWrites(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO channels (name, type, created_at) VALUES ('general', 'text', 0)`); err != nil {
		t.Fatal(err)
	}
	users := make([]int64, 8)
	for i := range users {
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO users (nickname, username, created_at, last_seen_at) VALUES (?, ?, 0, 0)`,
			fmt.Sprintf("u%d", i), fmt.Sprintf("u%d", i))
		if err != nil {
			t.Fatal(err)
		}
		users[i], _ = res.LastInsertId()
	}

	// Something for the readers to walk.
	if err := s.tx(ctx, func(tx *sql.Tx) error {
		for i := range 2000 {
			body := fmt.Sprintf("mensaje de prueba numero %d", i)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO messages (channel_id, user_id, author, content, search_text, created_at)
				 VALUES (1, 1, 'u0', ?, ?, ?)`, body, foldForSearch(body), int64(i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	const rounds = 40
	var wg sync.WaitGroup
	fail := func(what string, err error) {
		t.Errorf("%s: %v", what, err)
	}

	for _, u := range users {
		wg.Add(1)
		go func(userID int64) {
			defer wg.Done()
			for range rounds {
				// A plain write.
				m, err := s.CreateMessage(ctx, 1, nil, userID, "hola", nil)
				if err != nil {
					fail("CreateMessage", err)
					return
				}
				// A transaction that reads before it writes, which is the one
				// the deferred locking would have refused.
				if _, _, err := s.ReplaceProfileMedia(ctx, ProfileMedia{
					UserID:     userID,
					Kind:       KindAvatar,
					StorageKey: fmt.Sprintf("k-%d-%d", userID, m.ID),
					Filename:   "a.png",
					Size:       1,
				}); err != nil {
					fail("ReplaceProfileMedia", err)
					return
				}
				if _, err := s.UpdateMessageContent(ctx, m.ID, "hola de nuevo"); err != nil {
					fail("UpdateMessageContent", err)
					return
				}
			}
		}(u)
	}

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				if _, err := s.MessagesBefore(ctx, 1, 0, 50); err != nil {
					fail("MessagesBefore", err)
					return
				}
				if _, _, err := s.SearchMessages(ctx, SearchFilter{
					Terms:      []string{"prueba"},
					ChannelIDs: []int64{1},
					Limit:      25,
				}); err != nil {
					fail("SearchMessages", err)
					return
				}
			}
		}()
	}

	wg.Wait()

	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE content = 'hola de nuevo'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if want := len(users) * rounds; count != want {
		t.Errorf("edited messages = %d, want %d", count, want)
	}
}
