package store

import (
	"context"
	"fmt"
)

// StorageCountSize pairs a resource count with total bytes.
type StorageCountSize struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

// StorageBreakdown captures storage utilization across attachments,
// user profile media, expressions, soundboard audio, and the SQLite database.
type StorageBreakdown struct {
	Attachments struct {
		Videos StorageCountSize `json:"videos"`
		Images StorageCountSize `json:"images"`
		Audio  StorageCountSize `json:"audio"`
		Files  StorageCountSize `json:"files"`
		Total  StorageCountSize `json:"total"`
	} `json:"attachments"`

	Profiles struct {
		Avatars StorageCountSize `json:"avatars"`
		Banners StorageCountSize `json:"banners"`
		Total   StorageCountSize `json:"total"`
	} `json:"profiles"`

	Expressions struct {
		Emojis   StorageCountSize `json:"emojis"`
		Stickers StorageCountSize `json:"stickers"`
		Sounds   StorageCountSize `json:"sounds"`
		Total    StorageCountSize `json:"total"`
	} `json:"expressions"`

	DatabaseBytes int64 `json:"databaseBytes"`
	TotalBytes    int64 `json:"totalBytes"`
}

// StorageBreakdown aggregates disk space used by attachments, profiles,
// expressions, sounds, and the database file.
func (s *Store) StorageBreakdown(ctx context.Context) (StorageBreakdown, error) {
	var out StorageBreakdown

	// 1. Attachments breakdown
	var (
		videoBytes, videoCount       int64
		imageBytes, imageCount       int64
		audioBytes, audioCount       int64
		fileBytes, fileCount         int64
		totalAttBytes, totalAttCount int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN content_type LIKE 'video/%' THEN size ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN content_type LIKE 'video/%' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN content_type LIKE 'image/%' THEN size ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN content_type LIKE 'image/%' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN content_type LIKE 'audio/%' THEN size ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN content_type LIKE 'audio/%' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN content_type NOT LIKE 'video/%' AND content_type NOT LIKE 'image/%' AND content_type NOT LIKE 'audio/%' THEN size ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN content_type NOT LIKE 'video/%' AND content_type NOT LIKE 'image/%' AND content_type NOT LIKE 'audio/%' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(size), 0),
			COUNT(*)
		FROM attachments
	`).Scan(&videoBytes, &videoCount, &imageBytes, &imageCount, &audioBytes, &audioCount, &fileBytes, &fileCount, &totalAttBytes, &totalAttCount)
	if err != nil {
		return out, fmt.Errorf("store: query attachment breakdown: %w", err)
	}

	out.Attachments.Videos = StorageCountSize{Count: int(videoCount), Bytes: videoBytes}
	out.Attachments.Images = StorageCountSize{Count: int(imageCount), Bytes: imageBytes}
	out.Attachments.Audio = StorageCountSize{Count: int(audioCount), Bytes: audioBytes}
	out.Attachments.Files = StorageCountSize{Count: int(fileCount), Bytes: fileBytes}
	out.Attachments.Total = StorageCountSize{Count: int(totalAttCount), Bytes: totalAttBytes}

	// 2. Profile media breakdown
	var (
		avatarBytes, avatarCount       int64
		bannerBytes, bannerCount       int64
		totalProfBytes, totalProfCount int64
	)
	err = s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN kind = 'avatar' THEN size ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN kind = 'avatar' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN kind = 'banner' THEN size ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN kind = 'banner' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(size), 0),
			COUNT(*)
		FROM profile_media
	`).Scan(&avatarBytes, &avatarCount, &bannerBytes, &bannerCount, &totalProfBytes, &totalProfCount)
	if err != nil {
		return out, fmt.Errorf("store: query profile media breakdown: %w", err)
	}

	out.Profiles.Avatars = StorageCountSize{Count: int(avatarCount), Bytes: avatarBytes}
	out.Profiles.Banners = StorageCountSize{Count: int(bannerCount), Bytes: bannerBytes}
	out.Profiles.Total = StorageCountSize{Count: int(totalProfCount), Bytes: totalProfBytes}

	// 3. Expressions breakdown
	var (
		emojiBytes, emojiCount         int64
		stickerBytes, stickerCount     int64
		totalExprBytes, totalExprCount int64
	)
	err = s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN kind = 'emoji' THEN size ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN kind = 'emoji' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN kind = 'sticker' THEN size ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN kind = 'sticker' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(size), 0),
			COUNT(*)
		FROM expressions
	`).Scan(&emojiBytes, &emojiCount, &stickerBytes, &stickerCount, &totalExprBytes, &totalExprCount)
	if err != nil {
		return out, fmt.Errorf("store: query expressions breakdown: %w", err)
	}

	out.Expressions.Emojis = StorageCountSize{Count: int(emojiCount), Bytes: emojiBytes}
	out.Expressions.Stickers = StorageCountSize{Count: int(stickerCount), Bytes: stickerBytes}

	// 4. Sounds breakdown
	var soundBytes, soundCount int64
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0), COUNT(*) FROM sounds`).Scan(&soundBytes, &soundCount)
	if err != nil {
		return out, fmt.Errorf("store: query sounds breakdown: %w", err)
	}
	out.Expressions.Sounds = StorageCountSize{Count: int(soundCount), Bytes: soundBytes}
	out.Expressions.Total = StorageCountSize{
		Count: int(totalExprCount + soundCount),
		Bytes: totalExprBytes + soundBytes,
	}

	// 5. Database file size
	out.DatabaseBytes = s.DatabaseFileSize()

	// Total stored bytes across everything
	out.TotalBytes = out.Attachments.Total.Bytes + out.Profiles.Total.Bytes + out.Expressions.Total.Bytes + out.DatabaseBytes

	return out, nil
}
