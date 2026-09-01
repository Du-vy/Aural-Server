package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aural-chat/aural-server/internal/store"
)

func TestLinkPreviewCache(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	urlHash := "abc123hash"
	rawURL := "https://example.com/post/1"
	dataJSON := `{"title":"Example Post","description":"Test"}`

	// 1. Missing item returns ErrNotFound
	_, err = st.GetLinkPreview(ctx, urlHash, 0)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing preview, got: %v", err)
	}

	// 2. Save preview
	now := time.Now().Unix()
	if err := st.SaveLinkPreview(ctx, urlHash, rawURL, dataJSON, now); err != nil {
		t.Fatalf("save link preview: %v", err)
	}

	// 3. Fetch cached preview (validAfter <= now)
	got, err := st.GetLinkPreview(ctx, urlHash, now-10)
	if err != nil {
		t.Fatalf("get link preview: %v", err)
	}
	if got != dataJSON {
		t.Fatalf("expected %s, got %s", dataJSON, got)
	}

	// 4. Fetch expired preview (validAfter > now)
	_, err = st.GetLinkPreview(ctx, urlHash, now+100)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for expired preview, got: %v", err)
	}

	// 5. Update preview (upsert)
	updatedJSON := `{"title":"Updated Post"}`
	if err := st.SaveLinkPreview(ctx, urlHash, rawURL, updatedJSON, now+50); err != nil {
		t.Fatalf("update link preview: %v", err)
	}
	got, err = st.GetLinkPreview(ctx, urlHash, now)
	if err != nil || got != updatedJSON {
		t.Fatalf("expected updated %s, got: %s (err: %v)", updatedJSON, got, err)
	}

	// 6. Prune previews
	pruned, err := st.PruneLinkPreviews(ctx, now+100)
	if err != nil {
		t.Fatalf("prune link previews: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected 1 pruned row, got %d", pruned)
	}

	// 7. Verify pruned
	_, err = st.GetLinkPreview(ctx, urlHash, 0)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after pruning, got: %v", err)
	}
}
