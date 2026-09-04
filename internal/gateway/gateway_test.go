package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/gateway"
	"github.com/aural-chat/aural-server/internal/permissions"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// harness is a running gateway backed by a throwaway database.
type harness struct {
	t      *testing.T
	http   *httptest.Server
	store  *store.Store
	server *gateway.Server
	cfg    *config.Config
}

func newHarness(t *testing.T, tune func(*config.Config)) *harness {
	t.Helper()

	ctx := context.Background()
	cfg := config.Default()
	if tune != nil {
		tune(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := gateway.New(ctx, &cfg, "", db, log)
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}

	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	return &harness{t: t, http: ts, store: db, server: server, cfg: &cfg}
}

// client is a protocol-speaking test client.
type client struct {
	t    *testing.T
	conn *websocket.Conn
	ctx  context.Context
	seq  int
	// pending holds events read while waiting for a reply.
	pending []protocol.Envelope
	// hello is the first frame the server sent, kept because two of the things
	// it carries — the server preview and the device salt — are only ever sent
	// there.
	hello protocol.Hello
}

func (h *harness) dial() *client {
	h.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	h.t.Cleanup(cancel)

	url := "ws" + strings.TrimPrefix(h.http.URL, "http") + "/ws"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		h.t.Fatalf("dial: %v", err)
	}
	h.t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })

	c := &client{t: h.t, conn: conn, ctx: ctx}
	hello := c.next()
	if hello.Op != protocol.EvHello {
		h.t.Fatalf("expected %s first, got %s", protocol.EvHello, hello.Op)
	}
	if err := json.Unmarshal(hello.Data, &c.hello); err != nil {
		h.t.Fatalf("decode hello: %v", err)
	}
	return c
}

// next reads one frame off the wire.
func (c *client) next() protocol.Envelope {
	c.t.Helper()

	_, raw, err := c.conn.Read(c.ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		c.t.Fatalf("decode frame: %v", err)
	}
	return env
}

// send writes a request without waiting for its reply.
func (c *client) send(op string, payload any) string {
	c.t.Helper()

	c.seq++
	id := "req-" + string(rune('a'+c.seq%26)) + time.Now().Format("150405.000000")

	body, err := json.Marshal(payload)
	if err != nil {
		c.t.Fatalf("encode payload: %v", err)
	}
	frame, err := json.Marshal(protocol.Envelope{ID: id, Op: op, Data: body})
	if err != nil {
		c.t.Fatalf("encode frame: %v", err)
	}
	if err := c.conn.Write(c.ctx, websocket.MessageText, frame); err != nil {
		c.t.Fatalf("write: %v", err)
	}
	return id
}

// do sends a request and returns its reply, stashing any event that arrives in
// the meantime so a later waitEvent can still find it.
func (c *client) do(op string, payload any) protocol.Envelope {
	c.t.Helper()

	id := c.send(op, payload)
	for range 64 {
		env := c.next()
		if env.ID == id {
			return env
		}
		c.pending = append(c.pending, env)
	}
	c.t.Fatalf("no reply to %s", op)
	return protocol.Envelope{}
}

// ok sends a request, requires success, and decodes the result payload.
func ok[T any](c *client, op string, payload any) T {
	c.t.Helper()

	env := c.do(op, payload)
	if env.Op == protocol.OpError {
		c.t.Fatalf("%s failed: %s: %s", op, env.Error.Code, env.Error.Message)
	}
	var out T
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &out); err != nil {
			c.t.Fatalf("decode %s result: %v", op, err)
		}
	}
	return out
}

// fails sends a request and requires a specific error code back.
func (c *client) fails(op string, payload any, wantCode string) {
	c.t.Helper()

	env := c.do(op, payload)
	if env.Op != protocol.OpError {
		c.t.Fatalf("%s unexpectedly succeeded", op)
	}
	if env.Error.Code != wantCode {
		c.t.Fatalf("%s: got error %q, want %q (%s)", op, env.Error.Code, wantCode, env.Error.Message)
	}
}

// waitEvent returns the next event with the given op, draining stashed frames
// first.
func (c *client) waitEvent(op string) protocol.Envelope {
	c.t.Helper()

	for i, env := range c.pending {
		if env.Op == op {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			return env
		}
	}
	for range 64 {
		env := c.next()
		if env.Op == op {
			return env
		}
		c.pending = append(c.pending, env)
	}
	c.t.Fatalf("event %s never arrived", op)
	return protocol.Envelope{}
}

// guest authenticates as a fresh guest.
func (c *client) guest(nickname string) protocol.Ready {
	c.t.Helper()
	return ok[protocol.Ready](c, protocol.OpAuthGuest, protocol.AuthGuestRequest{Nickname: nickname})
}

func TestGuestGetsIdentityAndSeededState(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()

	ready := c.guest("Pablo")

	if ready.SessionToken == "" {
		t.Fatal("guest was not given a session token")
	}
	if ready.User.Registered {
		t.Fatal("a fresh guest must not be registered")
	}
	if ready.User.Nickname != "Pablo" {
		t.Fatalf("nickname: got %q", ready.User.Nickname)
	}
	if ready.User.ID == 0 {
		t.Fatal("guest was not given an id")
	}

	// The seed installs one category holding a text and a voice channel.
	if len(ready.Channels) != 3 {
		t.Fatalf("channels: got %d, want 3", len(ready.Channels))
	}
	if len(ready.Roles) != 3 {
		t.Fatalf("roles: got %d, want 3", len(ready.Roles))
	}

	perms, err := permissions.Parse(ready.Permissions)
	if err != nil {
		t.Fatalf("parse permissions: %v", err)
	}
	if !perms.Has(permissions.Connect | permissions.Speak | permissions.Register) {
		t.Fatalf("a default guest should be able to talk and register, got %v", perms.Names())
	}
	if perms.Has(permissions.ManageChannels) {
		t.Fatal("a guest must not be able to manage channels")
	}
}

func TestTokenResumesTheSameIdentity(t *testing.T) {
	h := newHarness(t, nil)

	first := h.dial()
	ready := first.guest("Pablo")
	first.conn.Close(websocket.StatusNormalClosure, "")

	second := h.dial()
	resumed := ok[protocol.Ready](second, protocol.OpAuthToken, protocol.AuthTokenRequest{Token: ready.SessionToken})

	if resumed.User.ID != ready.User.ID {
		t.Fatalf("token resumed a different identity: got %d, want %d", resumed.User.ID, ready.User.ID)
	}
	if resumed.SessionToken != "" {
		t.Fatal("resuming must not mint a second token")
	}

	bad := h.dial()
	bad.fails(protocol.OpAuthToken, protocol.AuthTokenRequest{Token: "not-a-real-token"}, protocol.ErrInvalidCredentials)
}

// TestRegisterKeepsTheIdentity is the heart of the login design: claiming an
// account must upgrade the guest in place, never replace it with a new user.
func TestRegisterKeepsTheIdentity(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()

	ready := c.guest("Pablo")
	guestID := ready.User.ID

	claimed := ok[protocol.AuthRegisterResult](c, protocol.OpAuthRegister,
		protocol.AuthRegisterRequest{Username: "pablo", Password: "correct-horse"})

	if claimed.User.ID != guestID {
		t.Fatalf("claiming created a new identity: got %d, want %d", claimed.User.ID, guestID)
	}
	if !claimed.User.Registered || claimed.User.Username == nil || *claimed.User.Username != "pablo" {
		t.Fatalf("identity was not claimed: %+v", claimed.User)
	}

	// Claiming grants the managed registered role on top of everyone.
	if len(claimed.User.Roles) != 2 {
		t.Fatalf("roles after claiming: got %v, want everyone plus registered", claimed.User.Roles)
	}

	// And the credentials now work from a device that never saw the token.
	fresh := h.dial()
	signedIn := ok[protocol.Ready](fresh, protocol.OpAuthLogin,
		protocol.AuthLoginRequest{Username: "pablo", Password: "correct-horse"})
	if signedIn.User.ID != guestID {
		t.Fatalf("login reached a different identity: got %d, want %d", signedIn.User.ID, guestID)
	}
	if signedIn.SessionToken == "" {
		t.Fatal("login must mint a token for the device signing in")
	}
}

func TestLoginRejectsWrongCredentials(t *testing.T) {
	h := newHarness(t, nil)

	c := h.dial()
	c.guest("Pablo")
	ok[protocol.AuthRegisterResult](c, protocol.OpAuthRegister,
		protocol.AuthRegisterRequest{Username: "pablo", Password: "correct-horse"})

	wrong := h.dial()
	wrong.fails(protocol.OpAuthLogin,
		protocol.AuthLoginRequest{Username: "pablo", Password: "wrong-horse"}, protocol.ErrInvalidCredentials)
	// A username nobody holds must be indistinguishable from a wrong password.
	wrong.fails(protocol.OpAuthLogin,
		protocol.AuthLoginRequest{Username: "nobody", Password: "wrong-horse"}, protocol.ErrInvalidCredentials)
}

func TestUsernameIsTakenOnlyOnce(t *testing.T) {
	h := newHarness(t, nil)

	first := h.dial()
	first.guest("First")
	ok[protocol.AuthRegisterResult](first, protocol.OpAuthRegister,
		protocol.AuthRegisterRequest{Username: "pablo", Password: "correct-horse"})

	second := h.dial()
	second.guest("Second")
	second.fails(protocol.OpAuthRegister,
		protocol.AuthRegisterRequest{Username: "PABLO", Password: "another-password"}, protocol.ErrUsernameTaken)
}

func TestRegistrationCanBeClosed(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Registration.Enabled = false })

	c := h.dial()
	c.guest("Pablo")
	c.fails(protocol.OpAuthRegister,
		protocol.AuthRegisterRequest{Username: "pablo", Password: "correct-horse"}, protocol.ErrRegistrationClosed)
}

func TestGuestsCanBeRefused(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Registration.AllowGuests = false })

	c := h.dial()
	c.fails(protocol.OpAuthGuest, protocol.AuthGuestRequest{Nickname: "Pablo"}, protocol.ErrGuestsDisabled)
}

func TestServerPasswordGatesEveryone(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Server.Password = "hunter2" })

	c := h.dial()
	c.fails(protocol.OpAuthGuest, protocol.AuthGuestRequest{Nickname: "Pablo"}, protocol.ErrServerPassword)

	good := h.dial()
	good.guest2(t, "Pablo", "hunter2")
}

// guest2 authenticates as a guest with a server password.
func (c *client) guest2(t *testing.T, nickname, serverPassword string) protocol.Ready {
	t.Helper()
	return ok[protocol.Ready](c, protocol.OpAuthGuest,
		protocol.AuthGuestRequest{Nickname: nickname, ServerPassword: serverPassword})
}

func TestOwnerTokenClaimsTheServerExactlyOnce(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	token, err := gateway.EnsureOwnerToken(ctx, h.store, h.server.Hub())
	if err != nil {
		t.Fatalf("ensure owner token: %v", err)
	}
	if token == "" {
		t.Fatal("a fresh server must issue an owner token")
	}

	owner := h.dial()
	ready := owner.guest("Pablo")
	owner.fails(protocol.OpServerClaimAdmin, protocol.ClaimAdminRequest{Token: "wrong"}, protocol.ErrInvalidCredentials)

	claimed := ok[protocol.UserEvent](owner, protocol.OpServerClaimAdmin, protocol.ClaimAdminRequest{Token: token})
	if !claimed.User.Owner {
		t.Fatalf("claiming did not mark the owner: %+v", claimed.User)
	}
	// Ownership is not a role: the claimer is left holding exactly what they
	// held before, which for a guest is the everyone role alone.
	if len(claimed.User.Roles) != len(ready.User.Roles) {
		t.Fatalf("claiming ownership changed the roles held: got %v, want %v",
			claimed.User.Roles, ready.User.Roles)
	}

	// It is authority all the same. The snapshot that follows the claim says so.
	snapshot := owner.waitEvent(protocol.EvReady)
	var refreshed protocol.Ready
	if err := json.Unmarshal(snapshot.Data, &refreshed); err != nil {
		t.Fatalf("decode ready: %v", err)
	}
	perms, err := permissions.Parse(refreshed.Permissions)
	if err != nil {
		t.Fatalf("parse permissions: %v", err)
	}
	if !perms.Has(permissions.Administrator) {
		t.Fatalf("the owner must hold every permission, got %v", perms.Names())
	}

	// The token is one-time: a second holder of the same string gets nothing.
	other := h.dial()
	other.guest("Other")
	other.fails(protocol.OpServerClaimAdmin, protocol.ClaimAdminRequest{Token: token}, protocol.ErrForbidden)
}

// TestOnlyTheOwnerMayEditTheAdminRole pins the difference between owning the
// server and holding the role that grants every permission: an administrator
// may do anything the mask allows, but not to the role that granted it and not
// to the owner.
func TestOnlyTheOwnerMayEditTheAdminRole(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	token, err := gateway.EnsureOwnerToken(ctx, h.store, h.server.Hub())
	if err != nil {
		t.Fatalf("ensure owner token: %v", err)
	}

	owner := h.dial()
	ownerReady := owner.guest("Owner")
	claimed := ok[protocol.UserEvent](owner, protocol.OpServerClaimAdmin, protocol.ClaimAdminRequest{Token: token})

	var adminRoleID int64
	for _, role := range ownerReady.Roles {
		if role.Managed == protocol.ManagedAdmin {
			adminRoleID = role.ID
		}
	}
	if adminRoleID == 0 {
		t.Fatal("no admin role in the snapshot")
	}

	// The owner hands the admin role out, which is itself a thing only somebody
	// above that role can do.
	admin := h.dial()
	adminReady := admin.guest("Admin")
	ok[protocol.UserEvent](owner, protocol.OpRoleAssign,
		protocol.RoleMembershipRequest{UserID: adminReady.User.ID, RoleID: adminRoleID})

	// The administrator may manage roles below them,
	created := ok[protocol.RoleEvent](admin, protocol.OpRoleCreate, protocol.RoleCreateRequest{Name: "Moderator"})
	if created.Role.ID == 0 {
		t.Fatal("an administrator must be able to create a role")
	}
	// but not the role that grants them that, nor its holders' owner.
	admin.fails(protocol.OpRoleUpdate,
		protocol.RoleUpdateRequest{RoleID: adminRoleID, Name: ptr("Overlord")}, protocol.ErrForbidden)
	admin.fails(protocol.OpRoleUpdate,
		protocol.RoleUpdateRequest{RoleID: adminRoleID, Permissions: ptr(permissions.None.String())}, protocol.ErrForbidden)
	admin.fails(protocol.OpUserKick,
		protocol.UserKickRequest{UserID: claimed.User.ID}, protocol.ErrForbidden)

	// The owner may, holding no role at all.
	updated := ok[protocol.RoleEvent](owner, protocol.OpRoleUpdate,
		protocol.RoleUpdateRequest{RoleID: adminRoleID, Name: ptr("Staff")})
	if updated.Role.Name != "Staff" {
		t.Fatalf("the owner could not rename the admin role: %+v", updated.Role)
	}
	if _, err := h.store.RoleByManaged(ctx, protocol.ManagedAdmin); err != nil {
		t.Fatalf("the admin role must survive being edited: %v", err)
	}
}

func TestChannelsNeedManageChannels(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	guest := h.dial()
	guest.guest("Guest")
	guest.fails(protocol.OpChannelCreate,
		protocol.ChannelCreateRequest{Name: "Nope", Type: protocol.ChannelVoice}, protocol.ErrForbidden)

	token, err := gateway.EnsureOwnerToken(ctx, h.store, h.server.Hub())
	if err != nil {
		t.Fatalf("ensure owner token: %v", err)
	}

	admin := h.dial()
	admin.guest("Admin")
	ok[protocol.UserEvent](admin, protocol.OpServerClaimAdmin, protocol.ClaimAdminRequest{Token: token})

	created := ok[protocol.ChannelEvent](admin, protocol.OpChannelCreate,
		protocol.ChannelCreateRequest{Name: "Strategy", Type: protocol.ChannelVoice, UserLimit: 5})
	if created.Channel.Name != "Strategy" || created.Channel.Type != protocol.ChannelVoice {
		t.Fatalf("created channel: %+v", created.Channel)
	}

	// The guest, who may see the channel, is told it appeared.
	event := guest.waitEvent(protocol.EvChannelCreated)
	var announced protocol.ChannelEvent
	if err := json.Unmarshal(event.Data, &announced); err != nil {
		t.Fatalf("decode channel event: %v", err)
	}
	if announced.Channel.ID != created.Channel.ID {
		t.Fatalf("announced the wrong channel: %+v", announced.Channel)
	}

	// Categories may not be nested inside other channels.
	admin.fails(protocol.OpChannelCreate,
		protocol.ChannelCreateRequest{
			Name:     "Bad",
			Type:     protocol.ChannelCategory,
			ParentID: &created.Channel.ID,
		}, protocol.ErrBadRequest)
}

func TestJoiningVoiceChannels(t *testing.T) {
	h := newHarness(t, nil)

	first := h.dial()
	ready := first.guest("First")

	var voiceID int64
	for _, ch := range ready.Channels {
		if ch.Type == protocol.ChannelVoice {
			voiceID = ch.ID
		}
	}
	if voiceID == 0 {
		t.Fatal("the seed should include a voice channel")
	}

	second := h.dial()
	second.guest("Second")

	// The first client learns the second one connected.
	first.waitEvent(protocol.EvUserConnected)

	ok[struct{}](second, protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &voiceID})

	moved := first.waitEvent(protocol.EvUserMoved)
	var event protocol.UserMovedEvent
	if err := json.Unmarshal(moved.Data, &event); err != nil {
		t.Fatalf("decode move event: %v", err)
	}
	if event.To == nil || *event.To != voiceID {
		t.Fatalf("move event: %+v", event)
	}
	if event.From != nil {
		t.Fatalf("the user came from nowhere, got %v", *event.From)
	}

	// Text channels carry no presence, so they cannot be joined.
	var textID int64
	for _, ch := range ready.Channels {
		if ch.Type == protocol.ChannelText {
			textID = ch.ID
		}
	}
	second.fails(protocol.OpUserMove, protocol.UserMoveRequest{ChannelID: &textID}, protocol.ErrBadRequest)
}

func TestSecondConnectionDisplacesTheFirst(t *testing.T) {
	h := newHarness(t, nil)

	first := h.dial()
	ready := first.guest("Pablo")

	second := h.dial()
	resumed := ok[protocol.Ready](second, protocol.OpAuthToken, protocol.AuthTokenRequest{Token: ready.SessionToken})
	if resumed.User.ID != ready.User.ID {
		t.Fatal("the second connection should hold the same identity")
	}

	// The displaced connection is closed rather than left as a ghost.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, err := first.conn.Read(first.ctx); err != nil {
			return
		}
	}
	t.Fatal("the first connection was never closed")
}

func TestInfoEndpointDescribesTheServer(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Server.Name = "Test Server"
		c.Voice.Mode = protocol.VoiceModeServerHost
	})

	res, err := http.Get(h.http.URL + "/info")
	if err != nil {
		t.Fatalf("get info: %v", err)
	}
	defer res.Body.Close()

	var info protocol.ServerInfo
	if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
		t.Fatalf("decode info: %v", err)
	}
	if info.Name != "Test Server" {
		t.Fatalf("name: got %q", info.Name)
	}
	if info.ProtocolVersion != protocol.Version {
		t.Fatalf("protocol version: got %d, want %d", info.ProtocolVersion, protocol.Version)
	}
	if info.MinProtocolVersion != protocol.MinVersion {
		t.Fatalf("min protocol version: got %d, want %d", info.MinProtocolVersion, protocol.MinVersion)
	}
	if info.VoiceMode != protocol.VoiceModeServerHost {
		t.Fatalf("voice mode: got %q", info.VoiceMode)
	}
}

func TestUnauthenticatedOpsAreRejected(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()

	c.fails(protocol.OpChannelCreate,
		protocol.ChannelCreateRequest{Name: "Nope", Type: protocol.ChannelVoice}, protocol.ErrUnauthorized)
	c.fails("nonsense.op", struct{}{}, protocol.ErrBadRequest)
}

// A reorder is a decision about the whole stack, so it is refused as a whole
// when any part of it reaches at or above the caller — including the case that
// matters most, an administrator lifting a role of their own above the one
// that grants them the authority to try.
func TestRoleReorderRespectsTheHierarchy(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	token, err := gateway.EnsureOwnerToken(ctx, h.store, h.server.Hub())
	if err != nil {
		t.Fatalf("ensure owner token: %v", err)
	}

	owner := h.dial()
	ownerReady := owner.guest("Owner")

	var adminRoleID int64
	for _, role := range ownerReady.Roles {
		if role.Managed == protocol.ManagedAdmin {
			adminRoleID = role.ID
		}
	}
	ok[protocol.UserEvent](owner, protocol.OpServerClaimAdmin, protocol.ClaimAdminRequest{Token: token})

	admin := h.dial()
	adminReady := admin.guest("Admin")
	ok[protocol.UserEvent](owner, protocol.OpRoleAssign,
		protocol.RoleMembershipRequest{UserID: adminReady.User.ID, RoleID: adminRoleID})

	lower := ok[protocol.RoleEvent](admin, protocol.OpRoleCreate, protocol.RoleCreateRequest{Name: "Lower"})
	upper := ok[protocol.RoleEvent](admin, protocol.OpRoleCreate, protocol.RoleCreateRequest{Name: "Upper"})

	// The stack, bottom-up, as it stands: the managed registered role, then the
	// two just created, then admin at the top.
	stack := make([]int64, 0, 4)
	for _, role := range h.server.Hub().SortedRoles() {
		if role.Managed != protocol.ManagedEveryone {
			stack = append(stack, role.ID)
		}
	}
	if len(stack) != 4 || stack[3] != adminRoleID {
		t.Fatalf("unexpected starting stack: %v", stack)
	}

	// Swapping two roles below the administrator is theirs to do.
	swapped := []int64{stack[0], upper.Role.ID, lower.Role.ID, adminRoleID}
	result := ok[protocol.RoleReorderResult](admin, protocol.OpRoleReorder,
		protocol.RoleReorderRequest{RoleIDs: swapped})
	if len(result.Roles) != 4 {
		t.Fatalf("a reorder must report the whole stack: %+v", result.Roles)
	}
	if result.Roles[1].ID != upper.Role.ID || result.Roles[2].ID != lower.Role.ID {
		t.Fatalf("the stack was not reordered: %+v", result.Roles)
	}
	if result.Roles[1].Position >= result.Roles[2].Position {
		t.Fatalf("positions must follow the order asked for: %+v", result.Roles)
	}

	// Lifting a role over the one that grants the authority to ask is not.
	admin.fails(protocol.OpRoleReorder, protocol.RoleReorderRequest{
		RoleIDs: []int64{stack[0], upper.Role.ID, adminRoleID, lower.Role.ID},
	}, protocol.ErrForbidden)

	// Nor is naming a stack that is not the stack.
	admin.fails(protocol.OpRoleReorder,
		protocol.RoleReorderRequest{RoleIDs: []int64{lower.Role.ID, lower.Role.ID, upper.Role.ID, adminRoleID}},
		protocol.ErrBadRequest)
	admin.fails(protocol.OpRoleReorder,
		protocol.RoleReorderRequest{RoleIDs: []int64{lower.Role.ID, upper.Role.ID}}, protocol.ErrBadRequest)

	// The owner sits above every role and may move the admin role itself.
	ok[protocol.RoleReorderResult](owner, protocol.OpRoleReorder, protocol.RoleReorderRequest{
		RoleIDs: []int64{stack[0], adminRoleID, upper.Role.ID, lower.Role.ID},
	})
}
