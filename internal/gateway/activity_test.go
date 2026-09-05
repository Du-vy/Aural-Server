package gateway_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/protocol"
)

// listening is one plausible report from a media session: the shape the client
// sends every time a track changes.
func listening(name, details, state string) *protocol.Activity {
	return &protocol.Activity{
		Type:      "listening",
		Name:      name,
		Details:   details,
		State:     state,
		StartedAt: 1_756_600_000,
	}
}

// report sends one activity and returns the caller's own view of themselves.
func (c *client) report(activity *protocol.Activity) protocol.User {
	c.t.Helper()
	event := ok[protocol.UserEvent](c, protocol.OpUserActivity,
		protocol.UserActivityRequest{Activity: activity})
	return event.User
}

func TestActivityReachesEverybodyElse(t *testing.T) {
	h := newHarness(t, nil)

	watcher := h.dial()
	watcher.guest("Ana")

	player := h.dial()
	ready := player.guest("Bruno")
	watcher.waitEvent(protocol.EvUserConnected)

	own := player.report(listening("Spotify", "Teardrop", "Massive Attack"))
	if own.Activity == nil {
		t.Fatal("the reporter was not given their own activity back")
	}
	if own.Activity.Details != "Teardrop" {
		t.Fatalf("own activity details: got %q, want %q", own.Activity.Details, "Teardrop")
	}

	seen := userOf(t, watcher.waitEvent(protocol.EvUserUpdated))
	if seen.ID != ready.User.ID {
		t.Fatalf("update was about user %d, want %d", seen.ID, ready.User.ID)
	}
	if seen.Activity == nil {
		t.Fatal("the activity did not reach the other member")
	}
	if seen.Activity.Type != "listening" || seen.Activity.Name != "Spotify" {
		t.Fatalf("activity arrived as %+v", *seen.Activity)
	}
	if seen.Activity.StartedAt != 1_756_600_000 {
		t.Fatalf("activity startedAt: got %d, want 1756600000", seen.Activity.StartedAt)
	}
}

func TestActivityIsClearedByReportingNothing(t *testing.T) {
	h := newHarness(t, nil)

	watcher := h.dial()
	watcher.guest("Ana")

	player := h.dial()
	player.guest("Bruno")
	watcher.waitEvent(protocol.EvUserConnected)

	player.report(listening("Spotify", "Teardrop", "Massive Attack"))
	watcher.waitEvent(protocol.EvUserUpdated)

	own := player.report(nil)
	if own.Activity != nil {
		t.Fatalf("clearing left %+v behind", *own.Activity)
	}
	seen := userOf(t, watcher.waitEvent(protocol.EvUserUpdated))
	if seen.Activity != nil {
		t.Fatalf("the other member still sees %+v", *seen.Activity)
	}
}

// An unchanged report is what a source that polls sends whenever nothing has
// happened. Forwarding those would redraw every member list on the server for
// no change, so the second one has to go no further than the reply.
func TestRepeatingAnActivityIsNotBroadcast(t *testing.T) {
	h := newHarness(t, nil)

	watcher := h.dial()
	watcher.guest("Ana")

	player := h.dial()
	player.guest("Bruno")
	watcher.waitEvent(protocol.EvUserConnected)

	player.report(listening("Spotify", "Teardrop", "Massive Attack"))
	watcher.waitEvent(protocol.EvUserUpdated)

	own := player.report(listening("Spotify", "Teardrop", "Massive Attack"))
	if own.Activity == nil || own.Activity.Details != "Teardrop" {
		t.Fatal("the repeated report did not come back with the activity that stands")
	}
	if env, got := watcher.eventWithin(protocol.EvUserUpdated, quietFor); got {
		t.Fatalf("an unchanged activity was broadcast: %s", string(env.Data))
	}
}

// Hiding is the whole point of `invisible`, and an activity is the one field
// that changes without its owner doing anything: left alone it would keep
// announcing somebody who has chosen to look offline.
func TestAnInvisibleMemberLeaksNoActivity(t *testing.T) {
	h := newHarness(t, nil)

	watcher := h.dial()
	watcher.guest("Ana")

	hidden, hiddenReady := h.member("Bruno", "bruno")
	watcher.waitEvent(protocol.EvUserConnected)
	// Claiming an account is itself an update. Draining it here is what keeps
	// the silence asserted below about the activity and nothing else.
	watcher.waitEvent(protocol.EvUserUpdated)
	hidden.setStatus("invisible")
	watcher.waitEvent(protocol.EvUserDisconnected)

	own := hidden.report(listening("Spotify", "Teardrop", "Massive Attack"))
	if own.Activity == nil {
		t.Fatal("a hidden member should still see their own activity")
	}
	if env, got := watcher.eventWithin(protocol.EvUserUpdated, quietFor); got {
		t.Fatalf("a hidden member's activity was announced: %s", string(env.Data))
	}

	// And it is absent from the list a client builds from scratch, which is
	// where a leak would otherwise survive the silence above.
	arriving := h.dial()
	ready := arriving.guest("Carla")
	entry, found := userInList(ready.Users, hiddenReady.User.ID)
	if !found {
		t.Fatal("the hidden member is missing from the snapshot entirely")
	}
	requireOffline(t, entry, "the hidden member")
	if entry.Activity != nil {
		t.Fatalf("the hidden member's snapshot entry carries %+v", *entry.Activity)
	}
}

// An activity belongs to the connection that reported it. Nothing stores one,
// so the entry a member leaves behind must not carry the last thing they were
// doing — otherwise everybody is listening to a track that ended hours ago.
func TestActivityDoesNotOutliveTheConnection(t *testing.T) {
	h := newHarness(t, nil)

	watcher := h.dial()
	watcher.guest("Ana")

	player, playerReady := h.member("Bruno", "bruno")
	watcher.waitEvent(protocol.EvUserConnected)
	// The update that claiming an account produced, so that the one waited on
	// after the report really is the report.
	watcher.waitEvent(protocol.EvUserUpdated)
	player.report(listening("Spotify", "Teardrop", "Massive Attack"))
	if seen := userOf(t, watcher.waitEvent(protocol.EvUserUpdated)); seen.Activity == nil {
		t.Fatal("the activity never reached the other member")
	}

	player.conn.Close(websocket.StatusNormalClosure, "")
	watcher.waitEvent(protocol.EvUserDisconnected)

	arriving := h.dial()
	ready := arriving.guest("Carla")
	entry, found := userInList(ready.Users, playerReady.User.ID)
	if !found {
		t.Fatal("the absent member is missing from the snapshot")
	}
	if entry.Activity != nil {
		t.Fatalf("an absent member's entry carries %+v", *entry.Activity)
	}

	// Signing back in is the other half: the new connection has reported
	// nothing, so it must not inherit what the old one said.
	back := h.dial()
	signedIn := ok[protocol.Ready](back, protocol.OpAuthLogin,
		protocol.AuthLoginRequest{Username: "bruno", Password: "correct-horse"})
	if signedIn.User.Activity != nil {
		t.Fatalf("a fresh connection inherited %+v", *signedIn.User.Activity)
	}
}

func TestActivityIsBounded(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	c.guest("Ana")

	cases := []struct {
		name     string
		activity *protocol.Activity
		wantCode string
	}{
		{
			name:     "an unknown verb",
			activity: &protocol.Activity{Type: "competing", Name: "Chess"},
			wantCode: protocol.ErrBadRequest,
		},
		{
			name:     "no name to put the verb in front of",
			activity: &protocol.Activity{Type: "playing", Name: "   "},
			wantCode: protocol.ErrBadRequest,
		},
		{
			name:     "a name longer than a member list could ever draw",
			activity: &protocol.Activity{Type: "playing", Name: strings.Repeat("a", 129)},
			wantCode: protocol.ErrBadRequest,
		},
		{
			name: "artwork fetched in the clear",
			activity: &protocol.Activity{
				Type: "playing", Name: "Chess", Image: "http://example.com/cover.png",
			},
			wantCode: protocol.ErrBadRequest,
		},
		{
			name: "artwork that is not a picture at all",
			activity: &protocol.Activity{
				Type: "playing", Name: "Chess", Image: "javascript:alert(1)",
			},
			wantCode: protocol.ErrBadRequest,
		},
		{
			name: "artwork too big to broadcast",
			activity: &protocol.Activity{
				Type: "playing", Name: "Chess",
				Image: "data:image/png;base64," + strings.Repeat("A", 24*1024),
			},
			wantCode: protocol.ErrTooLarge,
		},
		{
			name: "a timer that runs backwards",
			activity: &protocol.Activity{
				Type: "playing", Name: "Chess", StartedAt: 2_000, EndsAt: 1_000,
			},
			wantCode: protocol.ErrBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.fails(protocol.OpUserActivity,
				protocol.UserActivityRequest{Activity: tc.activity}, tc.wantCode)
		})
	}

	// And the forms that are allowed really are: a hosted URL and a small
	// picture carried inline, which are the two things the two sources produce.
	own := c.report(&protocol.Activity{
		Type:      "playing",
		Name:      "Chess",
		Image:     "https://example.com/cover.png",
		Icon:      "data:image/png;base64,AAAA",
		ImageText: "Board",
		Party:     &protocol.ActivityParty{Size: 2, Max: 8},
	})
	if own.Activity == nil {
		t.Fatal("a well-formed activity was dropped")
	}
	if own.Activity.Party == nil || own.Activity.Party.Max != 8 {
		t.Fatalf("the party did not survive: %+v", *own.Activity)
	}
}

// A game names its artwork by a key that means something only to Discord. It
// is turned into a path back to this server, so that resolving it happens once
// here rather than once per member against a third party.
func TestAnAssetKeyBecomesAPathOnThisServer(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	c.guest("Ana")

	own := c.report(&protocol.Activity{
		Type:  "playing",
		Name:  "Warframe",
		Image: "asset:230684596683866113/warframe_logo",
		Icon:  "asset:230684596683866113/excalibur",
	})
	if own.Activity == nil {
		t.Fatal("the activity was dropped")
	}
	if got, want := own.Activity.Image, "/activity-assets/230684596683866113/warframe_logo"; got != want {
		t.Fatalf("image: got %q, want %q", got, want)
	}
	if got, want := own.Activity.Icon, "/activity-assets/230684596683866113/excalibur"; got != want {
		t.Fatalf("icon: got %q, want %q", got, want)
	}
}

func TestAMalformedAssetKeyIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	c.guest("Ana")

	for _, image := range []string{
		"asset:notanapplication/logo",
		"asset:230684596683866113",
		"asset:230684596683866113/../../secrets",
		"asset:230684596683866113/logo?size=4096",
	} {
		t.Run(image, func(t *testing.T) {
			c.fails(protocol.OpUserActivity, protocol.UserActivityRequest{
				Activity: &protocol.Activity{Type: "playing", Name: "Warframe", Image: image},
			}, protocol.ErrBadRequest)
		})
	}
}

// An operator who has switched artwork off has said this server does not call
// Discord. Losing the whole activity over a picture nobody asked for would be
// the wrong trade, so the reference goes and the text stays.
func TestArtworkIsDroppedWhenTheServerWillNotFetchIt(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Activity.Assets = false
	})
	c := h.dial()
	c.guest("Ana")

	own := c.report(&protocol.Activity{
		Type:    "playing",
		Name:    "Warframe",
		Details: "Plains of Eidolon",
		Image:   "asset:230684596683866113/warframe_logo",
	})
	if own.Activity == nil {
		t.Fatal("the activity was dropped along with the artwork")
	}
	if own.Activity.Image != "" {
		t.Fatalf("artwork survived as %q", own.Activity.Image)
	}
	if own.Activity.Details != "Plains of Eidolon" {
		t.Fatalf("the text did not survive: %+v", *own.Activity)
	}
}

// A picture that is already somewhere loadable is left exactly as it is: there
// is nothing for this server to resolve and no reason for it to be in the way.
func TestAHostedPictureIsNotRewritten(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	c.guest("Ana")

	own := c.report(&protocol.Activity{
		Type:  "playing",
		Name:  "A game",
		Image: "https://media.discordapp.net/external/abc/https/example.com/art.png",
	})
	if own.Activity == nil {
		t.Fatal("the activity was dropped")
	}
	if own.Activity.Image != "https://media.discordapp.net/external/abc/https/example.com/art.png" {
		t.Fatalf("a hosted picture was rewritten to %q", own.Activity.Image)
	}
}

// getAsset fetches artwork the way a browser does: with no Authorization
// header, because an <img> tag cannot send one.
func getAsset(t *testing.T, h *harness, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(h.http.URL + path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer resp.Body.Close()

	var body struct {
		Error protocol.Error `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.Error.Code
}

// The regression this exists for: the endpoint used to require a bearer token,
// which meant it could never work at all. Artwork is loaded by an <img> tag and
// a browser cannot attach a header to one, so every request was refused and the
// picture silently never appeared.
//
// What it must do instead is answer — and refuse on the grounds that actually
// bound it, which is whether anybody here is playing the thing.
func TestArtworkIsNotBehindABearerToken(t *testing.T) {
	h := newHarness(t, nil)

	status, code := getAsset(t, h, "/activity-assets/422772752647323649/default")
	if status == http.StatusUnauthorized {
		t.Fatal("artwork asked for a token an <img> tag cannot send")
	}
	if status != http.StatusNotFound || code != protocol.ErrNotFound {
		t.Fatalf("got %d %q, want 404 not_found for artwork nobody is reporting", status, code)
	}
}

// The gate is the set of things members are actually reporting. It is checked
// directly here because exercising it through the endpoint would mean fetching
// from Discord, and a test that needs the internet is a test that fails on a
// train.
func TestOnlyReportedArtworkMayBeFetched(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	c.guest("Ana")

	hub := h.server.Hub()
	if hub.ReportsActivityAsset("422772752647323649", "default") {
		t.Fatal("artwork was fetchable before anybody reported it")
	}

	c.report(&protocol.Activity{
		Type:  "playing",
		Name:  "Warframe",
		Image: "asset:422772752647323649/default",
	})
	if !hub.ReportsActivityAsset("422772752647323649", "default") {
		t.Fatal("artwork somebody is reporting was not fetchable")
	}
	if hub.ReportsActivityAsset("422772752647323649", "somethingelse") {
		t.Fatal("artwork nobody named was fetchable")
	}
	if hub.ReportsActivityAsset("999999999999999999", "default") {
		t.Fatal("artwork from another application was fetchable")
	}

	// And it closes again when they stop, so the openable set never grows
	// beyond what is being played.
	c.report(nil)
	if hub.ReportsActivityAsset("422772752647323649", "default") {
		t.Fatal("artwork stayed fetchable after the activity was cleared")
	}
}

func TestArtworkEndpointRefusesMalformedReferences(t *testing.T) {
	h := newHarness(t, nil)

	for _, path := range []string{
		"/activity-assets/notanapplication/default",
		"/activity-assets/422772752647323649/has%20a%20space",
	} {
		status, code := getAsset(t, h, path)
		if status != http.StatusBadRequest || code != protocol.ErrBadRequest {
			t.Fatalf("%s: got %d %q, want 400 bad_request", path, status, code)
		}
	}
}

func TestArtworkEndpointIsOffWhenTheOperatorSaysSo(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Activity.Assets = false
	})

	status, code := getAsset(t, h, "/activity-assets/422772752647323649/default")
	if status != http.StatusForbidden || code != "activity_assets_disabled" {
		t.Fatalf("got %d %q, want 403 activity_assets_disabled", status, code)
	}
}

// A party of nobody says nothing an absent party does not already say, so it
// is dropped rather than carried as two zeroes for every client to special-case.
func TestAnEmptyPartyIsDropped(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	c.guest("Ana")

	own := c.report(&protocol.Activity{
		Type:  "playing",
		Name:  "Chess",
		Party: &protocol.ActivityParty{},
	})
	if own.Activity == nil {
		t.Fatal("the activity was dropped")
	}
	if own.Activity.Party != nil {
		t.Fatalf("an empty party was kept as %+v", *own.Activity.Party)
	}
}
