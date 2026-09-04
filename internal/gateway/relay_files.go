package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/aural-chat/aural-server/internal/discord"
	"github.com/aural-chat/aural-server/internal/store"
	"github.com/aural-chat/aural-server/internal/uploads"
)

// Files across the bridge.
//
// The two directions are not symmetrical, and deliberately so.
//
// Coming in, the file is fetched from Discord's CDN and stored here. Linking to
// it instead would look cheaper and would be worse: a Discord attachment URL
// carries a signature that expires, so a channel bridged for a year would be
// full of images that stopped loading, and every reader would be fetching from
// Discord to read a server that left it.
//
// Going out, the bytes are uploaded to Discord rather than linked, because the
// link would have to be an address on this server that Discord can reach — and
// the whole point of a self-hosted server is that plenty of them are not
// reachable from anywhere but a living room. Uploading works either way.

// fetchInboundFiles downloads the files on a Discord message.
//
// A file that will not come across is skipped rather than fatal: the words
// beside it are the message, and losing them because a CDN had a bad second
// would be the wrong trade.
func (r *discordRelay) fetchInboundFiles(ctx context.Context, link store.RelayLink,
	m discord.Message) ([]savedUpload, error) {

	if !link.RelayAttachments || len(m.Attachments) == 0 {
		return nil, nil
	}
	files := r.hub.Files()
	if files == nil {
		return nil, nil
	}
	limit := r.attachmentLimit()
	if limit <= 0 {
		return nil, nil
	}

	maxFiles := r.hub.cfg.Uploads.MaxPerMessage
	rest := r.restClient()

	var (
		saved []savedUpload
		first error
	)
	for _, a := range m.Attachments {
		if len(saved) >= maxFiles {
			break
		}
		if a.Size > limit {
			r.log.Debug("skipping a relayed attachment over the size limit",
				slog.String("file", a.Filename), slog.Int64("size", a.Size))
			continue
		}

		upload, err := r.storeRemoteFile(ctx, rest, a, limit)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		saved = append(saved, upload)
	}
	return saved, first
}

// storeRemoteFile pulls one file onto this server's disk.
func (r *discordRelay) storeRemoteFile(ctx context.Context, rest *discord.REST,
	a discord.Attachment, limit int64) (savedUpload, error) {

	body, declared, err := rest.Download(ctx, a.URL, limit)
	if err != nil {
		return savedUpload{}, err
	}
	defer body.Close()

	filename := cleanFilename(a.Filename)
	if filename == "" {
		filename = "attachment"
	}

	// SaveWithLimit is the second of the two size checks: the length Discord
	// declared was a claim, and this one is the body actually being counted as
	// it lands.
	written, err := r.hub.Files().SaveWithLimit(io.LimitReader(body, limit+1), declared, limit)
	switch {
	case errors.Is(err, uploads.ErrTooLarge):
		return savedUpload{}, fmt.Errorf("relayed file %q is over the size limit", filename)
	case errors.Is(err, uploads.ErrQuotaExceeded):
		return savedUpload{}, errors.New("this server has no storage left for relayed files")
	case err != nil:
		return savedUpload{}, err
	}

	upload := savedUpload{key: written.Key, size: written.Size, filename: filename}
	if path, err := r.hub.Files().Path(written.Key); err == nil {
		if width, height, ok := uploads.Dimensions(path); ok {
			upload.width, upload.height = &width, &height
		}
	}
	return upload, nil
}

// attachInboundFiles binds downloaded files to the message that carries them.
func (r *discordRelay) attachInboundFiles(ctx context.Context, link store.RelayLink,
	messageID int64, files []savedUpload) ([]store.Attachment, error) {

	if len(files) == 0 {
		return nil, nil
	}
	out := make([]store.Attachment, 0, len(files))
	for _, f := range files {
		created, err := r.st.CreatePostedAttachment(ctx, store.Attachment{
			MessageID:   &messageID,
			ChannelID:   link.ChannelID,
			StorageKey:  f.key,
			Filename:    f.filename,
			ContentType: uploads.ContentType(f.filename),
			Size:        f.size,
			Width:       f.width,
			Height:      f.height,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, created)
	}
	return out, nil
}

// discardUploads gives back the bytes of a delivery that did not become a
// message.
func (r *discordRelay) discardUploads(files []savedUpload) {
	saved := r.hub.Files()
	if saved == nil {
		return
	}
	for _, f := range files {
		saved.Remove(f.key, f.size)
	}
}

// outboundFiles opens the files on an Aural message for upload to Discord.
//
// The reader is opened lazily, once per attempt, because a delivery that hits
// a rate limit is repeated and a reader that has already been drained cannot
// be. A file over the limit is left behind with its URL appended to the text
// instead, which is the honest thing: the message says a file was posted and
// where to find it, rather than pretending there was none.
func (r *discordRelay) outboundFiles(attachments []store.Attachment) (files []discord.OutboundFile, skipped []store.Attachment) {
	saved := r.hub.Files()
	if saved == nil {
		return nil, attachments
	}
	limit := r.attachmentLimit()

	for _, a := range attachments {
		if limit <= 0 || a.Size > limit {
			skipped = append(skipped, a)
			continue
		}
		key := a.StorageKey
		files = append(files, discord.OutboundFile{
			Filename:    a.Filename,
			ContentType: a.ContentType,
			Size:        a.Size,
			Open: func() (io.ReadCloser, error) {
				file, _, err := saved.Open(key)
				if err != nil {
					return nil, err
				}
				return file, nil
			},
		})
	}
	return files, skipped
}

// attachmentLimit is the size ceiling on one relayed file, in bytes.
//
// It is the smaller of what an operator configured and what this server would
// accept for an upload of its own: a bridge should not be a way around the
// file limit somebody set.
func (r *discordRelay) attachmentLimit() int64 {
	settings := r.hub.RelaySettings()
	limit := settings.MaxAttachmentBytes
	if limit <= 0 {
		return 0
	}
	if files := r.hub.Files(); files != nil {
		if ceiling := files.MaxFileBytes(); ceiling > 0 && ceiling < limit {
			return ceiling
		}
	}
	return limit
}
