package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Attachment is one file posted alongside a message.
//
// A row exists from the moment the file lands on disk, before the message that
// carries it has been sent: MessageID stays nil until the message is posted.
// That two-step is what lets a client show an upload finishing while the writer
// is still typing, and what lets the server refuse a file on its own terms
// rather than in the middle of sending a message.
type Attachment struct {
	ID int64
	// MessageID is nil while the file is still pending.
	MessageID *int64
	// UserID is nil once the uploader's account is gone.
	UserID    *int64
	ChannelID int64
	// StorageKey names the file on disk and forms the unguessable part of its
	// download URL.
	StorageKey  string
	Filename    string
	ContentType string
	Size        int64
	// Width and Height are set for images whose dimensions could be read, so a
	// client can reserve the right space before the file arrives.
	Width     *int
	Height    *int
	CreatedAt int64
}

const attachmentColumns = `id, message_id, user_id, channel_id, storage_key,
	filename, content_type, size, width, height, created_at`

func scanAttachment(row interface{ Scan(...any) error }) (Attachment, error) {
	var a Attachment
	err := row.Scan(&a.ID, &a.MessageID, &a.UserID, &a.ChannelID, &a.StorageKey,
		&a.Filename, &a.ContentType, &a.Size, &a.Width, &a.Height, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, ErrNotFound
	}
	if err != nil {
		return Attachment{}, fmt.Errorf("store: scan attachment: %w", err)
	}
	return a, nil
}

func scanAttachments(rows *sql.Rows) ([]Attachment, error) {
	defer rows.Close()

	var out []Attachment
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read attachments: %w", err)
	}
	return out, nil
}

// CreateAttachment records an uploaded file that no message carries yet.
func (s *Store) CreateAttachment(ctx context.Context, a Attachment) (Attachment, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO attachments (message_id, user_id, channel_id, storage_key,
			filename, content_type, size, width, height, created_at)
		 VALUES (NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.UserID, a.ChannelID, a.StorageKey, a.Filename, a.ContentType,
		a.Size, a.Width, a.Height, now())
	if err != nil {
		return Attachment{}, fmt.Errorf("store: create attachment: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Attachment{}, fmt.Errorf("store: create attachment: %w", err)
	}
	return s.AttachmentByID(ctx, id)
}

// AttachmentByID loads one attachment.
func (s *Store) AttachmentByID(ctx context.Context, id int64) (Attachment, error) {
	return scanAttachment(s.db.QueryRowContext(ctx,
		`SELECT `+attachmentColumns+` FROM attachments WHERE id = ?`, id))
}

// AttachmentByStorageKey resolves the key that appears in a download URL.
func (s *Store) AttachmentByStorageKey(ctx context.Context, key string) (Attachment, error) {
	return scanAttachment(s.db.QueryRowContext(ctx,
		`SELECT `+attachmentColumns+` FROM attachments WHERE storage_key = ?`, key))
}

// idArgs binds a list of ids as query arguments, to go with placeholders.
func idArgs(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

// AttachmentsForMessages groups the attachments of a page of messages by the
// message they belong to. One query serves a whole page, which is what keeps
// reading history a fixed number of round trips.
func (s *Store) AttachmentsForMessages(ctx context.Context, messageIDs []int64) (map[int64][]Attachment, error) {
	out := map[int64][]Attachment{}
	if len(messageIDs) == 0 {
		return out, nil
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+attachmentColumns+` FROM attachments
		 WHERE message_id IN (`+placeholders(len(messageIDs))+`) ORDER BY id`,
		idArgs(messageIDs)...)
	if err != nil {
		return nil, fmt.Errorf("store: read attachments: %w", err)
	}
	found, err := scanAttachments(rows)
	if err != nil {
		return nil, err
	}
	for _, a := range found {
		if a.MessageID == nil {
			continue
		}
		out[*a.MessageID] = append(out[*a.MessageID], a)
	}
	return out, nil
}

// AttachmentsForMessage lists the files one message carries, oldest first.
func (s *Store) AttachmentsForMessage(ctx context.Context, messageID int64) ([]Attachment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+attachmentColumns+` FROM attachments WHERE message_id = ? ORDER BY id`, messageID)
	if err != nil {
		return nil, fmt.Errorf("store: read attachments: %w", err)
	}
	return scanAttachments(rows)
}

// ErrAttachmentUnavailable is returned by ClaimAttachments when one of the ids
// is not a pending file of this uploader in this channel: already posted, gone,
// or somebody else's.
var ErrAttachmentUnavailable = errors.New("store: attachment is not available to claim")

// ClaimAttachments binds pending files to the message that carries them.
//
// The ownership and channel checks are part of the UPDATE rather than a read
// beforehand, so two messages racing for the same upload cannot both win: the
// second one matches no rows.
func (s *Store) ClaimAttachments(ctx context.Context, messageID, userID, channelID int64, ids []int64) ([]Attachment, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	args := append([]any{messageID}, idArgs(ids)...)
	args = append(args, userID, channelID)

	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE attachments SET message_id = ?
			 WHERE id IN (`+placeholders(len(ids))+`)
			   AND message_id IS NULL
			   AND user_id = ?
			   AND channel_id = ?`, args...)
		if err != nil {
			return fmt.Errorf("store: claim attachments: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: claim attachments: %w", err)
		}
		if affected != int64(len(ids)) {
			return ErrAttachmentUnavailable
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.AttachmentsForMessage(ctx, messageID)
}

// AttachmentsForChannels lists every file held by every message in a set of
// channels. Deleting a channel cascades to its messages and their attachment
// rows, so what it held has to be read before the delete rather than after.
func (s *Store) AttachmentsForChannels(ctx context.Context, channelIDs []int64) ([]Attachment, error) {
	if len(channelIDs) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+attachmentColumns+` FROM attachments WHERE channel_id IN (`+
			placeholders(len(channelIDs))+`)`, idArgs(channelIDs)...)
	if err != nil {
		return nil, fmt.Errorf("store: read attachments: %w", err)
	}
	return scanAttachments(rows)
}

// DeleteAttachment removes one attachment row.
func (s *Store) DeleteAttachment(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM attachments WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete attachment: %w", err)
	}
	return requireOneRow(res, "attachment")
}

// TotalAttachmentBytes is how much every stored file adds up to, which is what
// the configured server-wide ceiling is measured against. It is read once at
// startup; the running total is kept in memory from there.
func (s *Store) TotalAttachmentBytes(ctx context.Context) (int64, error) {
	var total sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT SUM(size) FROM attachments`).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: total attachment bytes: %w", err)
	}
	return total.Int64, nil
}

// PendingAttachmentsBefore lists uploads that were never posted and are older
// than cutoff. A writer who picks a file and then abandons the message leaves
// one behind, and it must not be kept forever.
func (s *Store) PendingAttachmentsBefore(ctx context.Context, cutoff int64, limit int) ([]Attachment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+attachmentColumns+` FROM attachments
		 WHERE message_id IS NULL AND created_at < ? ORDER BY id LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("store: read pending attachments: %w", err)
	}
	return scanAttachments(rows)
}
