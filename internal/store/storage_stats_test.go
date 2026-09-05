package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestStorageBreakdown(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test_storage.db")
	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Initial empty breakdown
	bd, err := s.StorageBreakdown(ctx)
	if err != nil {
		t.Fatalf("StorageBreakdown: %v", err)
	}
	if bd.Attachments.Total.Count != 0 || bd.Attachments.Total.Bytes != 0 {
		t.Errorf("expected 0 attachments, got %+v", bd.Attachments.Total)
	}
	if bd.DatabaseBytes <= 0 {
		t.Errorf("expected database file size > 0, got %d", bd.DatabaseBytes)
	}

	channels, err := s.AllChannels(ctx)
	if err != nil || len(channels) == 0 {
		t.Fatalf("AllChannels: %v", err)
	}
	channelID := channels[0].ID

	// Insert test attachment: 1 video (1000 bytes) and 1 image (500 bytes)
	_, err = s.CreateAttachment(ctx, Attachment{
		ChannelID:   channelID,
		Filename:    "video.mp4",
		ContentType: "video/mp4",
		StorageKey:  "123456789012345678901234",
		Size:        1000,
	})
	if err != nil {
		t.Fatalf("CreateAttachment video: %v", err)
	}

	_, err = s.CreateAttachment(ctx, Attachment{
		ChannelID:   channelID,
		Filename:    "photo.png",
		ContentType: "image/png",
		StorageKey:  "abcdefghijklmnopqrstuvwx",
		Size:        500,
	})
	if err != nil {
		t.Fatalf("CreateAttachment image: %v", err)
	}

	// Re-check breakdown
	bd, err = s.StorageBreakdown(ctx)
	if err != nil {
		t.Fatalf("StorageBreakdown after insert: %v", err)
	}

	if bd.Attachments.Videos.Count != 1 || bd.Attachments.Videos.Bytes != 1000 {
		t.Errorf("expected 1 video with 1000 bytes, got %+v", bd.Attachments.Videos)
	}
	if bd.Attachments.Images.Count != 1 || bd.Attachments.Images.Bytes != 500 {
		t.Errorf("expected 1 image with 500 bytes, got %+v", bd.Attachments.Images)
	}
	if bd.Attachments.Total.Count != 2 || bd.Attachments.Total.Bytes != 1500 {
		t.Errorf("expected 2 attachments with 1500 bytes, got %+v", bd.Attachments.Total)
	}
}
