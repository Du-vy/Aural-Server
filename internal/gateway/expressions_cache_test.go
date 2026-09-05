package gateway_test

import (
	"context"
	"testing"

	"github.com/aural-chat/aural-server/internal/store"
)

// TestStoredFileByKeyTracksTheTables covers the cache the attachment route now
// answers emoji, stickers and soundboard clips from.
//
// Serving them from memory is only correct while the memory agrees with the
// tables, so what is checked here is the agreement rather than the speed: a
// file that has been recorded is found, one that has been removed is not, and
// a key nobody minted is not found at all — which is what sends the request on
// to the queries that look for an attachment or a picture instead.
func TestStoredFileByKeyTracksTheTables(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	hub := h.server.Hub()

	if _, _, _, found := hub.StoredFileByKey("nothing-was-ever-stored-here"); found {
		t.Error("an unminted key was answered from the cache")
	}

	emoji, err := h.store.CreateExpression(ctx, store.Expression{
		Kind:        store.KindEmoji,
		Name:        "party",
		StorageKey:  "key-emoji",
		Filename:    "party.png",
		ContentType: "image/png",
		Size:        12,
	})
	if err != nil {
		t.Fatalf("create expression: %v", err)
	}
	sound, err := h.store.CreateSound(ctx, store.Sound{
		Name:        "airhorn",
		StorageKey:  "key-sound",
		Filename:    "airhorn.ogg",
		ContentType: "audio/ogg",
		Size:        34,
	})
	if err != nil {
		t.Fatalf("create sound: %v", err)
	}

	// Nothing is visible until the caches are reloaded, which is what every
	// write path does; before that the route falls through to the database.
	if _, _, _, found := hub.StoredFileByKey("key-emoji"); found {
		t.Error("an expression was cached before the reload that publishes it")
	}

	if err := hub.ReloadExpressions(ctx); err != nil {
		t.Fatalf("reload expressions: %v", err)
	}
	if err := hub.ReloadSounds(ctx); err != nil {
		t.Fatalf("reload sounds: %v", err)
	}

	name, kind, _, found := hub.StoredFileByKey("key-emoji")
	if !found || name != "party.png" || kind != "image/png" {
		t.Errorf("emoji = (%q, %q, %v), want (party.png, image/png, true)", name, kind, found)
	}
	name, kind, _, found = hub.StoredFileByKey("key-sound")
	if !found || name != "airhorn.ogg" || kind != "audio/ogg" {
		t.Errorf("sound = (%q, %q, %v), want (airhorn.ogg, audio/ogg, true)", name, kind, found)
	}

	// Reloading one must not forget the other: they are two tables behind one
	// lookup, and rebuilding the index from either alone would drop the rest.
	if err := hub.ReloadExpressions(ctx); err != nil {
		t.Fatalf("reload expressions: %v", err)
	}
	if _, _, _, found := hub.StoredFileByKey("key-sound"); !found {
		t.Error("reloading the expressions dropped the soundboard from the index")
	}

	if _, err := h.store.DeleteExpression(ctx, emoji.ID); err != nil {
		t.Fatalf("delete expression: %v", err)
	}
	if _, err := h.store.DeleteSound(ctx, sound.ID); err != nil {
		t.Fatalf("delete sound: %v", err)
	}
	if err := hub.ReloadExpressions(ctx); err != nil {
		t.Fatalf("reload expressions: %v", err)
	}
	if err := hub.ReloadSounds(ctx); err != nil {
		t.Fatalf("reload sounds: %v", err)
	}

	if _, _, _, found := hub.StoredFileByKey("key-emoji"); found {
		t.Error("a removed expression is still served from the cache")
	}
	if _, _, _, found := hub.StoredFileByKey("key-sound"); found {
		t.Error("a removed sound is still served from the cache")
	}
}
