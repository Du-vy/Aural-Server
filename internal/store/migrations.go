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

	// 10: server ownership, which used to be nothing but a grant of the admin
	// role. A server that already had an administrator keeps one as its owner:
	// the earliest identity holding the role, which on a server whose token was
	// redeemed once is the identity that redeemed it.
	`
	INSERT OR IGNORE INTO meta (key, value)
	SELECT 'owner_user_id', CAST(ur.user_id AS TEXT)
	FROM user_roles ur JOIN roles r ON r.id = ur.role_id
	WHERE r.managed = 'admin'
	ORDER BY ur.user_id ASC
	LIMIT 1;
	`,

	// 11: webhooks, and the columns a message posted by one needs.
	//
	// A webhook is a URL that posts into one channel and nothing else. The
	// token in that URL is the whole of its authentication, so unlike a session
	// token it is stored as it was minted rather than hashed: whoever manages
	// the channel has to be able to read the URL back out of the settings
	// screen, which is the entire point of the feature. The bound on the damage
	// is the webhook itself — it can post to one channel, and revoking it is
	// one delete.
	`
	CREATE TABLE webhooks (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		channel_id   INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
		name         TEXT    NOT NULL,
		-- The default avatar shown for messages this webhook posts, which a
		-- single message may still override.
		avatar       TEXT,
		token        TEXT    NOT NULL UNIQUE,
		creator_id   INTEGER REFERENCES users(id) ON DELETE SET NULL,
		created_at   INTEGER NOT NULL,
		-- Zero until the first delivery, which is what tells an administrator
		-- whether an integration was ever wired up at the other end.
		last_used_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX idx_webhooks_channel ON webhooks(channel_id);

	-- Deliberately not a foreign key. A message posted by a webhook keeps the
	-- name and picture it was posted under for as long as the history does, and
	-- ON DELETE SET NULL would quietly rewrite every one of them into the shape
	-- a message left by a deleted account has. Deleting a webhook revokes the
	-- URL; it does not edit what was already said through it.
	ALTER TABLE messages ADD COLUMN webhook_id INTEGER;
	-- The avatar this one message was posted under, which is the webhook's own
	-- unless the payload overrode it.
	ALTER TABLE messages ADD COLUMN webhook_avatar TEXT;
	-- The embeds carried by the message, as the JSON array they arrived in.
	-- NULL is a message with none, which is nearly all of them.
	ALTER TABLE messages ADD COLUMN embeds TEXT;
	CREATE INDEX idx_messages_webhook ON messages(webhook_id) WHERE webhook_id IS NOT NULL;
	`,
	// 12: posts, the entries of an announcement, forum, media or calendar
	// channel.
	//
	// A post is a title and some metadata in front of an ordinary thread. Its
	// body and its comments are rows in messages carrying post_id, which is
	// what makes files, edits, deletion and moderation reach a post without a
	// second implementation of any of them. The channel timeline is therefore
	// the messages of a channel with no post_id, and a thread is the messages
	// with one.
	`
	CREATE TABLE posts (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
		-- As in messages: an author who is deleted leaves their posts behind,
		-- attributed to the name captured in author.
		user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
		author     TEXT    NOT NULL,
		title      TEXT    NOT NULL,
		-- The first message of the thread. It is set immediately after the row
		-- is inserted, in the same transaction, because the message needs the
		-- post id and the post needs the message id. NULL means the body was
		-- deleted out from under the post by a moderation purge, which renders
		-- as a post with a title and nothing else.
		root_message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
		locked     INTEGER NOT NULL DEFAULT 0,
		pinned     INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		edited_at  INTEGER,
		-- Calendar posts only. NULL starts_at is what tells the two kinds
		-- apart in a query, so it carries the same meaning as the channel type
		-- and is cheaper to ask.
		starts_at  INTEGER,
		ends_at    INTEGER,
		all_day    INTEGER NOT NULL DEFAULT 0,
		location   TEXT    NOT NULL DEFAULT ''
	);
	-- A channel's entries are read newest first, exactly as its history is.
	CREATE INDEX idx_posts_channel ON posts(channel_id, id DESC);
	-- A calendar is read as a window in time rather than as a page.
	CREATE INDEX idx_posts_starts ON posts(channel_id, starts_at) WHERE starts_at IS NOT NULL;

	-- NULL is a message written straight into a text channel, which is every
	-- row that exists today. ON DELETE CASCADE is what makes deleting a post
	-- take its thread with it, and the thread take its files.
	ALTER TABLE messages ADD COLUMN post_id INTEGER REFERENCES posts(id) ON DELETE CASCADE;
	-- A thread is read oldest first, the order it is rendered in.
	CREATE INDEX idx_messages_post ON messages(post_id, id) WHERE post_id IS NOT NULL;

	CREATE TABLE post_rsvps (
		post_id    INTEGER NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		-- 'going', 'maybe' or 'declined'. Withdrawing an answer deletes the
		-- row: having said nothing is not one of the three answers.
		response   TEXT    NOT NULL,
		created_at INTEGER NOT NULL,
		PRIMARY KEY (post_id, user_id)
	);

	-- Existing servers seeded their everyone role before CreatePosts existed,
	-- and a server that upgrades should behave like a fresh one: a forum is
	-- somewhere anybody may start a topic until an administrator says
	-- otherwise. An announcement channel is made read-only with an overwrite,
	-- which is the same shape as making a text channel read-only.
	UPDATE roles SET permissions = permissions | 16384 WHERE managed = 'everyone';
	`,
	// 13: moderation that outlives a connection, the record of it, and the
	// files a server carries for its own people.
	//
	// A ban is one decision with several handles on it. The row in bans is the
	// decision — who, why, by whom, until when — and the rows in ban_matches
	// are the things a connection is compared against: an identity, an
	// address, a device. Splitting them is what lets one ban reach the account
	// and the two addresses it was last seen from without becoming three
	// unrelated bans that have to be lifted one at a time, and what makes the
	// check on every connection a single indexed lookup.
	`
	CREATE TABLE bans (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		-- The identity as it was. Deliberately not a foreign key: banning a
		-- guest deletes the row it names, and a ban that forgot who it was for
		-- the moment it took effect would be unreadable in the list.
		user_id        INTEGER,
		user_nickname  TEXT    NOT NULL,
		user_username  TEXT,
		actor_id       INTEGER,
		actor_nickname TEXT    NOT NULL,
		reason         TEXT    NOT NULL DEFAULT '',
		created_at     INTEGER NOT NULL,
		-- NULL is permanent. A ban that has expired is left in place rather
		-- than deleted, so the list still shows that it happened.
		expires_at     INTEGER
	);
	CREATE INDEX idx_bans_created ON bans(created_at DESC);
	CREATE INDEX idx_bans_user ON bans(user_id) WHERE user_id IS NOT NULL;

	CREATE TABLE ban_matches (
		ban_id INTEGER NOT NULL REFERENCES bans(id) ON DELETE CASCADE,
		-- 'user', 'ip' or 'device'.
		kind   TEXT NOT NULL,
		value  TEXT NOT NULL,
		PRIMARY KEY (kind, value)
	);
	CREATE INDEX idx_ban_matches_ban ON ban_matches(ban_id);

	-- Where an identity has connected from. It is what lets a ban issued
	-- against an account also reach the address and the device behind it,
	-- which is the whole of what makes banning a guest mean anything: the
	-- identity itself lasts no longer than the connection.
	--
	-- The device value is a hash the client computes over a salt this server
	-- minted, so it identifies a machine here and is meaningless anywhere
	-- else. A client that sends none is recorded with an empty one, which
	-- matches nothing.
	CREATE TABLE identity_marks (
		user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		ip            TEXT    NOT NULL DEFAULT '',
		device        TEXT    NOT NULL DEFAULT '',
		first_seen_at INTEGER NOT NULL,
		last_seen_at  INTEGER NOT NULL,
		PRIMARY KEY (user_id, ip, device)
	);
	CREATE INDEX idx_identity_marks_ip ON identity_marks(ip) WHERE ip <> '';
	CREATE INDEX idx_identity_marks_device ON identity_marks(device) WHERE device <> '';
	CREATE INDEX idx_identity_marks_seen ON identity_marks(last_seen_at);

	-- What moderators did, in the order they did it. Every entry names an
	-- actor, an action, and the thing it was done to, captured as it read at
	-- the time: a role that is later deleted still has a name in the log.
	CREATE TABLE audit_entries (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		actor_id    INTEGER,
		actor_name  TEXT    NOT NULL,
		action      TEXT    NOT NULL,
		target_type TEXT    NOT NULL DEFAULT '',
		target_id   INTEGER,
		target_name TEXT    NOT NULL DEFAULT '',
		reason      TEXT    NOT NULL DEFAULT '',
		-- A JSON array of {key, before, after}, or NULL for an action that
		-- changed nothing that can be written down that way.
		changes     TEXT,
		created_at  INTEGER NOT NULL
	);
	CREATE INDEX idx_audit_created ON audit_entries(id DESC);
	CREATE INDEX idx_audit_actor ON audit_entries(actor_id, id DESC);
	CREATE INDEX idx_audit_action ON audit_entries(action, id DESC);

	-- The custom emoji and stickers a server carries. One table: they are the
	-- same object — a name, a picture, and a namespace everybody shares — and
	-- differ only in where a client renders them.
	CREATE TABLE expressions (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		-- 'emoji' or 'sticker'.
		kind         TEXT    NOT NULL,
		name         TEXT    NOT NULL,
		storage_key  TEXT    NOT NULL UNIQUE,
		filename     TEXT    NOT NULL,
		content_type TEXT    NOT NULL,
		size         INTEGER NOT NULL,
		animated     INTEGER NOT NULL DEFAULT 0,
		creator_id   INTEGER REFERENCES users(id) ON DELETE SET NULL,
		created_at   INTEGER NOT NULL
	);
	-- A name is what a writer types, so it has to be unique within its kind.
	CREATE UNIQUE INDEX idx_expressions_name ON expressions(kind, name);

	-- The soundboard: short clips anybody in a voice channel may play at the
	-- room. The bytes live in the upload store beside everything else; this is
	-- the row that names one and keeps it out of the orphan sweep.
	CREATE TABLE sounds (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		name         TEXT    NOT NULL,
		-- The emoji shown on the button, which may be empty.
		emoji        TEXT    NOT NULL DEFAULT '',
		storage_key  TEXT    NOT NULL UNIQUE,
		filename     TEXT    NOT NULL,
		content_type TEXT    NOT NULL,
		size         INTEGER NOT NULL,
		duration_ms  INTEGER NOT NULL DEFAULT 0,
		-- Per-sound playback level, 0..100, so one clip recorded hot does not
		-- have to be re-cut to sit beside the others.
		volume       INTEGER NOT NULL DEFAULT 100,
		creator_id   INTEGER REFERENCES users(id) ON DELETE SET NULL,
		created_at   INTEGER NOT NULL
	);
	CREATE INDEX idx_sounds_name ON sounds(name);

	-- Existing servers seeded their everyone role before the soundboard
	-- existed, and a server that upgrades should behave like a fresh one:
	-- playing a sound is something anybody in the channel may do until an
	-- administrator says otherwise.
	UPDATE roles SET permissions = permissions | 32768 WHERE managed = 'everyone';
	`,
	// 14: the Discord relay, which carries one text channel in both
	// directions.
	//
	// A link is a pair — one channel here, one channel there — plus the
	// webhook URL messages go out through. The webhook is Discord's own, minted
	// in its channel settings, and its id is the load-bearing part: a message
	// this server relays into Discord arrives back over the gateway carrying
	// it, which is what tells the relay it is looking at its own echo. That is
	// the whole loop guard in one direction, and it is an identity rather than
	// a guess about the content.
	//
	// The other direction is the mirror of it. A message that arrived from
	// Discord is written with source_webhook_id in its webhook_id column, so
	// the outbound side recognises it by the same kind of tag and does not send
	// it back where it came from.
	`
	CREATE TABLE relay_links (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		channel_id         INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
		-- Discord ids are snowflakes: 64-bit values that do not survive a
		-- JavaScript number, so they are text everywhere, as they are on the
		-- wire.
		discord_guild_id   TEXT    NOT NULL DEFAULT '',
		discord_channel_id TEXT    NOT NULL,
		-- The two halves of the outgoing webhook URL, kept apart because the
		-- id is read on every inbound message and the token only on an
		-- outbound one. Stored as minted, for the same reason the webhooks
		-- table stores its own that way: an administrator has to be able to
		-- read the URL back out of the settings screen.
		webhook_id         TEXT    NOT NULL,
		webhook_token      TEXT    NOT NULL,
		-- 'both', 'to_aural' or 'to_discord'. A one-way link is what a
		-- community announcing into Discord without opening a door back wants.
		direction          TEXT    NOT NULL DEFAULT 'both',
		enabled            INTEGER NOT NULL DEFAULT 1,
		-- Whether files and edits cross with the words. They are separate
		-- switches because they cost different things: an attachment is
		-- bandwidth and disk on both sides, an edit is a second call per
		-- change.
		relay_attachments  INTEGER NOT NULL DEFAULT 1,
		relay_edits        INTEGER NOT NULL DEFAULT 1,
		-- The webhooks row that messages arriving from Discord are written
		-- under. It is what gives them a name and a picture per message, and
		-- what the outbound side reads to recognise its own inbound traffic.
		source_webhook_id  INTEGER,
		created_at         INTEGER NOT NULL,
		last_relayed_at    INTEGER NOT NULL DEFAULT 0,
		-- The last failure, kept so a broken link explains itself in the
		-- settings screen rather than only in the log.
		last_error         TEXT    NOT NULL DEFAULT ''
	);
	-- One link per channel on each side. Two links pointing at the same Discord
	-- channel would each relay the other's output, which is a loop the webhook
	-- guard cannot see because both halves are legitimately ours.
	CREATE UNIQUE INDEX idx_relay_links_channel ON relay_links(channel_id);
	CREATE UNIQUE INDEX idx_relay_links_discord ON relay_links(discord_channel_id);

	-- What one message is called on the other side.
	--
	-- Deliberately not a foreign key onto messages, for the reason webhook_id
	-- is not one: the row has to still be readable while the message it names
	-- is being deleted, which is exactly when the relay needs it. Rows whose
	-- message is gone are swept.
	CREATE TABLE relay_messages (
		aural_id   INTEGER PRIMARY KEY,
		link_id    INTEGER NOT NULL REFERENCES relay_links(id) ON DELETE CASCADE,
		discord_id TEXT    NOT NULL,
		-- 'discord' or 'aural': which side wrote it first. An edit is pushed
		-- only away from its origin, so a message that came from Discord and
		-- was edited there is not then pushed back at Discord.
		origin     TEXT    NOT NULL,
		created_at INTEGER NOT NULL
	);
	CREATE INDEX idx_relay_messages_discord ON relay_messages(link_id, discord_id);
	`,
	// 15: webhook source on messages, so relayed messages keep their attribution
	// across restarts and history fetches.
	`
	ALTER TABLE messages ADD COLUMN webhook_source TEXT;

	UPDATE messages
	SET webhook_source = 'discord'
	WHERE id IN (SELECT aural_id FROM relay_messages WHERE origin = 'discord')
	   OR (webhook_id IS NOT NULL AND webhook_id IN (SELECT source_webhook_id FROM relay_links WHERE source_webhook_id IS NOT NULL));
	`,
	// 16: message replies.
	`
	ALTER TABLE messages ADD COLUMN reply_to_id INTEGER;
	ALTER TABLE direct_messages ADD COLUMN reply_to_id INTEGER;
	`,
	// 17: indexes on the foreign keys that point at a user.
	//
	// SQLite enforces ON DELETE by looking for the deleted parent in every
	// child table, and a child column with no index behind it is looked for by
	// scanning the whole of that table — once per row deleted. Every one of
	// these columns was in exactly that state, so removing a guest meant a full
	// pass over messages, over direct_messages and over attachments, and the
	// retention sweep removes them in batches.
	//
	// It is not a slow query in some rarely used corner: the database takes one
	// connection at a time, so for as long as the sweep runs nothing else on the
	// server can read or write. On a database of two hundred thousand messages
	// the quarter-hourly sweep held it for seven seconds; with these it holds it
	// for a tenth of one.
	//
	// The two columns that already carry a partial index — messages.post_id and
	// bans.user_id — are deliberately left alone. SQLite can prove that
	// "post_id = ?" implies "post_id IS NOT NULL", so the partial index it has
	// is the index the foreign key needs.
	`
	CREATE INDEX IF NOT EXISTS idx_messages_user        ON messages(user_id);
	CREATE INDEX IF NOT EXISTS idx_direct_messages_user ON direct_messages(user_id);
	CREATE INDEX IF NOT EXISTS idx_attachments_user     ON attachments(user_id);
	CREATE INDEX IF NOT EXISTS idx_posts_user           ON posts(user_id);
	CREATE INDEX IF NOT EXISTS idx_post_rsvps_user      ON post_rsvps(user_id);
	`,
	// 18: read markers on channels, so an unread badge survives the client
	// being closed.
	//
	// This is the same shape the private threads have carried since they were
	// written — a marker per participant, and a count of what sits past it —
	// moved onto channels. Until now a channel's badge lived only in the
	// client's memory, so quitting was indistinguishable from reading
	// everything.
	//
	// Two pieces are needed rather than one. The table is where a marker goes
	// once somebody has read a channel. The column on users is what a channel
	// they have never opened counts from, and it is the answer to the question
	// a bare table cannot answer: a missing row has to mean "read" for the
	// history that predates this member, and "unread" for a channel created
	// after they arrived, which is one meaning too many for the absence of a
	// row. So the absence means "count from the member's epoch", the epoch is
	// the newest message that existed when they first appeared, and both
	// readings fall out of it — a year of history from before somebody joined
	// starts read, and a channel opened last week starts unread to the first
	// line.
	//
	// Existing members are stamped with the newest message there is, which is
	// what makes this upgrade quiet: nobody logs in the morning after to a
	// server-wide wall of badges for conversations they read months ago.
	`
	CREATE TABLE channel_reads (
		user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		channel_id   INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
		-- The newest message this member has seen here. It only ever moves
		-- forwards, so paging back through old lines cannot bring back a badge
		-- that reading has already cleared.
		last_read_id INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (user_id, channel_id)
	);
	-- user_id leads the primary key's own index, so only the other half of it
	-- needs one — for the reason migration 17 gives, deleting a channel looks
	-- for its rows here and would otherwise scan the table to find them.
	CREATE INDEX idx_channel_reads_channel ON channel_reads(channel_id);

	ALTER TABLE users ADD COLUMN unread_epoch INTEGER NOT NULL DEFAULT 0;
	UPDATE users SET unread_epoch = (SELECT COALESCE(MAX(id), 0) FROM messages);

	-- A post's title is scanned for mentions along with its body, and the body
	-- is a message row the title hangs off. Reading the two together walks
	-- from the message to the post that owns it, which is the direction this
	-- column has never been indexed in.
	CREATE INDEX IF NOT EXISTS idx_posts_root_message ON posts(root_message_id)
		WHERE root_message_id IS NOT NULL;
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
