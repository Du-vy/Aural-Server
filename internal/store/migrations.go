package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
)

// migrations are applied in order. Each entry moves the schema from version
// index to index+1, so entries are append-only: never edit one that has
// shipped, add another below it.
var migrations = []string{
	// 1: initial schema.
	`
	CREATE TABLE users (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		nickname      TEXT    NOT NULL,
		username      TEXT    UNIQUE COLLATE NOCASE,
		password_hash TEXT,
		registered_at INTEGER,
		created_at    INTEGER NOT NULL,
		last_seen_at  INTEGER NOT NULL
	);

	CREATE TABLE tokens (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		token_hash   TEXT    NOT NULL UNIQUE,
		label        TEXT    NOT NULL DEFAULT '',
		created_at   INTEGER NOT NULL,
		last_used_at INTEGER NOT NULL
	);
	CREATE INDEX idx_tokens_user ON tokens(user_id);

	CREATE TABLE channels (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		parent_id  INTEGER REFERENCES channels(id) ON DELETE CASCADE,
		name       TEXT    NOT NULL,
		type       TEXT    NOT NULL,
		topic      TEXT    NOT NULL DEFAULT '',
		position   INTEGER NOT NULL DEFAULT 0,
		user_limit INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE INDEX idx_channels_parent ON channels(parent_id);

	CREATE TABLE roles (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		name        TEXT    NOT NULL,
		color       TEXT    NOT NULL DEFAULT '',
		permissions INTEGER NOT NULL DEFAULT 0,
		position    INTEGER NOT NULL DEFAULT 0,
		hoist       INTEGER NOT NULL DEFAULT 0,
		managed     TEXT    NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL
	);
	CREATE UNIQUE INDEX idx_roles_managed ON roles(managed) WHERE managed <> '';

	CREATE TABLE user_roles (
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		role_id INTEGER NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
		PRIMARY KEY (user_id, role_id)
	);

	CREATE TABLE channel_overwrites (
		channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
		role_id    INTEGER NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
		allow      INTEGER NOT NULL DEFAULT 0,
		deny       INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (channel_id, role_id)
	);

	CREATE TABLE meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	`,
	// 2: text channel messages.
	`
	CREATE TABLE messages (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
		-- An author who is deleted leaves their messages behind, attributed to
		-- the name captured in author rather than vanishing from the history.
		user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
		author     TEXT    NOT NULL,
		content    TEXT    NOT NULL,
		created_at INTEGER NOT NULL,
		edited_at  INTEGER
	);
	-- History is always read newest first within one channel.
	CREATE INDEX idx_messages_channel ON messages(channel_id, id DESC);
	`,
	// 3: file attachments.
	`
	CREATE TABLE attachments (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		-- NULL while the file has been uploaded but the message carrying it has
		-- not been sent yet. Deleting the message takes its files with it, which
		-- is what makes moderation of a file the same act as moderation of the
		-- message it arrived in.
		message_id   INTEGER REFERENCES messages(id) ON DELETE CASCADE,
		user_id      INTEGER REFERENCES users(id) ON DELETE SET NULL,
		-- The channel the file was uploaded for. It is checked again when the
		-- message is sent, so a file cannot be moved into a channel its uploader
		-- may not post in.
		channel_id   INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
		-- storage_key is both the name on disk and the unguessable part of the
		-- download URL.
		storage_key  TEXT    NOT NULL UNIQUE,
		filename     TEXT    NOT NULL,
		content_type TEXT    NOT NULL,
		size         INTEGER NOT NULL,
		width        INTEGER,
		height       INTEGER,
		created_at   INTEGER NOT NULL
	);
	CREATE INDEX idx_attachments_message ON attachments(message_id);
	CREATE INDEX idx_attachments_pending ON attachments(created_at) WHERE message_id IS NULL;

	-- Existing servers seeded their everyone role before AttachFiles existed.
	-- Granting it here keeps a server that upgrades behaving like a fresh one,
	-- which is what an administrator who never touched the role expects.
	UPDATE roles SET permissions = permissions | 64 WHERE managed = 'everyone';
	`,
	// 4: message search.
	`
	-- search_text is the message folded down to what a search compares against:
	-- lower case, accents removed. It is kept beside the content rather than
	-- derived at query time because folding is done in Go, where the rules are
	-- readable, rather than in SQL, where LOWER() only knows ASCII.
	--
	-- NULL means "not folded yet", which is what every row an upgrading server
	-- already holds starts as. Backfill runs once at open and turns them all
	-- into strings, after which the partial index below is empty and stays so.
	ALTER TABLE messages ADD COLUMN search_text TEXT;
	CREATE INDEX idx_messages_unindexed ON messages(id) WHERE search_text IS NULL;

	-- A search narrowed by date walks this rather than every message it could
	-- otherwise have to fold through.
	CREATE INDEX idx_messages_created ON messages(created_at);

	-- has:image and friends resolve to "does this message carry a file of that
	-- kind", which is a lookup by message and then by type.
	CREATE INDEX idx_attachments_type ON attachments(message_id, content_type);
	`,
	// 5: OpenGraph link preview cache.
	`
	CREATE TABLE link_previews (
		url_hash   TEXT    PRIMARY KEY,
		url        TEXT    NOT NULL,
		data_json  TEXT    NOT NULL,
		fetched_at INTEGER NOT NULL
	);
	CREATE INDEX idx_link_previews_fetched ON link_previews(fetched_at);
	`,
	// 6: user avatars, banners, and status.
	`
	ALTER TABLE users ADD COLUMN avatar TEXT;
	ALTER TABLE users ADD COLUMN banner TEXT;
	ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'online';
	ALTER TABLE users ADD COLUMN custom_status TEXT NOT NULL DEFAULT '';
	`,
	// 7: the files behind avatars and banners.
	//
	// They cannot live in attachments: that table needs a channel, and the
	// sweep that reclaims abandoned uploads deletes every row in it that no
	// message carries, which is every avatar there would ever be. Their own
	// table is what lets the quota count them and a restart remember them.
	`
	CREATE TABLE profile_media (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		-- kind is 'avatar' or 'banner'. A user holds at most one of each; the
		-- previous row is deleted as the new one is written.
		kind        TEXT    NOT NULL,
		storage_key TEXT    NOT NULL UNIQUE,
		filename    TEXT    NOT NULL,
		size        INTEGER NOT NULL,
		created_at  INTEGER NOT NULL
	);
	CREATE INDEX idx_profile_media_owner ON profile_media(user_id, kind);
	`,
	// 8: private conversations.
	//
	// A conversation is a pair of identities, held as the lower id and the
	// higher one rather than as a sender and a recipient: the pair is the
	// conversation, and storing it ordered is what makes "is there already one
	// of these" a unique index rather than a search for a row that could have
	// been written either way round.
	//
	// Both ends cascade. An identity that is deleted — a guest swept by the
	// retention job — takes its conversations with it, because half a private
	// conversation is not something the other person can reply to.
	`
	CREATE TABLE dm_conversations (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_low   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		user_high  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		-- The newest message each side has read, so a badge survives a restart
		-- rather than being whatever the client happened to see live.
		low_read_id     INTEGER NOT NULL DEFAULT 0,
		high_read_id    INTEGER NOT NULL DEFAULT 0,
		created_at      INTEGER NOT NULL,
		last_message_at INTEGER NOT NULL
	);
	CREATE UNIQUE INDEX idx_dm_pair ON dm_conversations(user_low, user_high);
	-- One list per person, most recently spoken in first: the two indexes are
	-- the two sides somebody's own id can be on.
	CREATE INDEX idx_dm_low  ON dm_conversations(user_low, last_message_at DESC);
	CREATE INDEX idx_dm_high ON dm_conversations(user_high, last_message_at DESC);

	CREATE TABLE direct_messages (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		conversation_id INTEGER NOT NULL REFERENCES dm_conversations(id) ON DELETE CASCADE,
		-- As in messages: an author who is deleted leaves what they wrote
		-- behind, attributed to the name captured in author.
		user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
		author     TEXT    NOT NULL,
		content    TEXT    NOT NULL,
		created_at INTEGER NOT NULL,
		edited_at  INTEGER
	);
	CREATE INDEX idx_direct_messages_conversation ON direct_messages(conversation_id, id DESC);

	-- Who may write to you privately: 'everyone', 'registered' (only members
	-- who have claimed an account), or 'none'. It is read from both sides of a
	-- send, so turning it off stops the replies as well as the openings.
	ALTER TABLE users ADD COLUMN dm_privacy TEXT NOT NULL DEFAULT 'everyone';

	-- Existing servers seeded their everyone role before SendDirectMessages
	-- existed. Granting it here leaves a server that upgrades behaving like a
	-- fresh one, exactly as the AttachFiles migration did.
	UPDATE roles SET permissions = permissions | 128 WHERE managed = 'everyone';
	`,
	// 9: moderation kick log.
	`
	CREATE TABLE kicks (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id            INTEGER,
		user_nickname      TEXT NOT NULL,
		user_username      TEXT,
		actor_id           INTEGER,
		actor_nickname     TEXT NOT NULL,
		reason             TEXT NOT NULL,
		deleted_messages   TEXT NOT NULL DEFAULT 'none',
		created_at         INTEGER NOT NULL
	);
	CREATE INDEX idx_kicks_user ON kicks(user_id);
	CREATE INDEX idx_kicks_created ON kicks(created_at DESC);
	`,
}

// migrate brings the schema up to len(migrations) using SQLite's own
// user_version counter, so no bookkeeping table is needed.
func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("store: database schema is version %d but this build only knows %d, refusing to downgrade",
			version, len(migrations))
	}

	for i := version; i < len(migrations); i++ {
		err := s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return fmt.Errorf("store: migration %d: %w", i+1, err)
			}
			// PRAGMA does not take a bound parameter.
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
				return fmt.Errorf("store: stamp schema version %d: %w", i+1, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	// Folding a message for search happens as it is written, so only the ones
	// written before that column existed need catching up.
	if err := s.backfillSearchText(ctx); err != nil {
		return err
	}
	return s.seed(ctx)
}

// seed installs the managed roles and a starter channel tree the first time a
// database is opened. It is a no-op on an existing server.
func (s *Store) seed(ctx context.Context) error {
	var roleCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM roles`).Scan(&roleCount); err != nil {
		return fmt.Errorf("store: count roles: %w", err)
	}
	if roleCount > 0 {
		return nil
	}

	ts := now()
	return s.tx(ctx, func(tx *sql.Tx) error {
		insertRole := func(name, color string, perms permissions.Permission, position int, hoist bool, managed string) error {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO roles (name, color, permissions, position, hoist, managed, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				name, color, int64(perms), position, boolToInt(hoist), managed, ts)
			return err
		}
		if err := insertRole("everyone", "", permissions.DefaultEveryone, 0, false, protocol.ManagedEveryone); err != nil {
			return fmt.Errorf("store: seed everyone role: %w", err)
		}
		if err := insertRole("Member", "#3ba55d", permissions.DefaultRegistered, 1, false, protocol.ManagedRegistered); err != nil {
			return fmt.Errorf("store: seed registered role: %w", err)
		}
		if err := insertRole("Admin", "#e8544a", permissions.DefaultAdmin, 100, true, protocol.ManagedAdmin); err != nil {
			return fmt.Errorf("store: seed admin role: %w", err)
		}

		res, err := tx.ExecContext(ctx,
			`INSERT INTO channels (parent_id, name, type, position, created_at) VALUES (NULL, ?, ?, 0, ?)`,
			"General", protocol.ChannelCategory, ts)
		if err != nil {
			return fmt.Errorf("store: seed category: %w", err)
		}
		categoryID, err := res.LastInsertId()
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO channels (parent_id, name, type, topic, position, created_at) VALUES (?, ?, ?, ?, 0, ?)`,
			categoryID, "general", protocol.ChannelText, "Welcome to Aural", ts); err != nil {
			return fmt.Errorf("store: seed text channel: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO channels (parent_id, name, type, position, created_at) VALUES (?, ?, ?, 1, ?)`,
			categoryID, "Lobby", protocol.ChannelVoice, ts); err != nil {
			return fmt.Errorf("store: seed voice channel: %w", err)
		}
		return nil
	})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: create %s: %w", dir, err)
	}
	return nil
}

// isUniqueViolation recognises the constraint failures the callers turn into
// ErrConflict.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
