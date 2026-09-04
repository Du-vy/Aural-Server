package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// readLimit bounds one gateway frame.
//
// Nearly every frame is a message and fits in a few kilobytes. The exception
// is GUILD_CREATE, which arrives once per server at connection time and
// carries every channel and role in it; on a large community that is megabytes
// in one frame. The limit is set where an unusually large guild still connects
// and a runaway frame still cannot exhaust memory.
const readLimit = 8 << 20

// Heartbeat bookkeeping.
const (
	// missedBeatsBeforeReset is how many unacknowledged heartbeats mean the
	// connection is dead. TCP will hold a socket open long after the other end
	// has stopped answering, and Discord's own client treats a missed
	// acknowledgement as the signal to tear down rather than waiting for a
	// read error that may never come.
	missedBeatsBeforeReset = 2

	// writeTimeout bounds one frame going out, so a stalled socket surfaces as
	// an error instead of a blocked goroutine.
	writeTimeout = 15 * time.Second
)

// Reconnect backoff. A gateway that refuses a connection is usually Discord
// having a bad minute, so the first few retries are quick and the ceiling is
// low enough that a relay comes back on its own within a minute of an outage
// ending.
const (
	minBackoff = 2 * time.Second
	maxBackoff = 60 * time.Second
)

// Handlers is what a caller does with the events this client carries.
//
// Every one of them is called from the read loop, in order, and must not
// block: a handler that waits on the network stalls every other event on the
// connection. The relay satisfies this by doing its work on a queue of its
// own.
type Handlers struct {
	// Ready is called each time a session is established, with the bot's own
	// account. It fires again after a reconnect, so anything it sets up must
	// tolerate being set up twice.
	Ready func(self User)
	// GuildAvailable is called once per guild the bot is in, after Ready, and
	// again whenever one changes. It is what the settings screen lists.
	GuildAvailable func(g Guild)
	// Message, MessageEdited and MessageDeleted carry the three things that
	// can happen to a line of a channel.
	Message        func(m Message)
	MessageEdited  func(m Message)
	MessageDeleted func(channelID string, messageIDs []string)
}

// Options configures a client.
type Options struct {
	// Token is the bot token, without the "Bot " prefix.
	Token string
	// Handlers may leave any field nil, which drops that event.
	Handlers Handlers
	Log      *slog.Logger

	// gatewayURL and apiBase override the endpoints, so a test can point a
	// whole client at a server it controls. Empty means Discord.
	gatewayURL string
}

// Client is one bot connection to Discord.
//
// It owns the socket, reconnects on its own, and keeps just enough of the
// guilds it can see to turn the ids inside a message into names. It is safe
// for concurrent use: the read loop is the only writer of the caches, and
// everything a caller reads goes through the mutex.
type Client struct {
	token    string
	handlers Handlers
	log      *slog.Logger
	dialURL  string

	// rest carries the HTTP half — the webhook calls and the file downloads.
	// It is here so a caller holds one object rather than two.
	rest *REST

	mu       sync.RWMutex
	self     User
	guilds   map[string]Guild
	channels map[string]Channel
	roles    map[string]Role
	// connected is whether a session is currently established, which is what
	// the settings screen reports back to an administrator.
	connected bool
	// lastError is why the last connection ended, kept so a misconfiguration
	// can be shown in the UI instead of only in the log.
	lastError string

	// writeMu serialises frames. The heartbeat runs on its own timer and would
	// otherwise interleave with an identify or a resume.
	writeMu sync.Mutex
	conn    *websocket.Conn

	// Session state, read and written only by the connection goroutine.
	sessionID string
	resumeURL string
	sequence  int64
}

// NewClient builds a client. It does not connect; Run does.
func NewClient(opts Options) *Client {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	dial := opts.gatewayURL
	if dial == "" {
		dial = gatewayURL
	}
	return &Client{
		token:    strings.TrimSpace(opts.Token),
		handlers: opts.Handlers,
		log:      log,
		dialURL:  dial,
		rest:     newREST(strings.TrimSpace(opts.Token), log),
		guilds:   map[string]Guild{},
		channels: map[string]Channel{},
		roles:    map[string]Role{},
	}
}

// REST exposes the HTTP half of the same bot identity.
func (c *Client) REST() *REST { return c.rest }

// Self is the bot's own account, zero until the first session is established.
func (c *Client) Self() User {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.self
}

// Connected reports whether a session is currently up, and why the last one
// ended if it is not.
func (c *Client) Connected() (bool, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected, c.lastError
}

// Guilds lists the servers this bot is in, with their channels and roles.
func (c *Client) Guilds() []Guild {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Guild, 0, len(c.guilds))
	for _, g := range c.guilds {
		out = append(out, g)
	}
	return out
}

// Channel looks one channel up by id.
func (c *Client) Channel(id string) (Channel, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ch, ok := c.channels[id]
	return ch, ok
}

// RoleName resolves a role id to its name, for rendering a mention.
func (c *Client) RoleName(id string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.roles[id]
	return r.Name, ok
}

// ChannelName resolves a channel id to its name, for the same reason.
func (c *Client) ChannelName(id string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ch, ok := c.channels[id]
	return ch.Name, ok
}

// Run keeps a session up until ctx is cancelled.
//
// It returns nil on cancellation and an error only for a failure retrying
// cannot fix — a rejected token, an intent that was never switched on. Every
// other failure is logged and reconnected through, because a relay that stops
// on the first network blip is a relay somebody has to restart by hand.
func (c *Client) Run(ctx context.Context) error {
	if c.token == "" {
		return &fatalError{reason: "no bot token configured"}
	}

	backoff := minBackoff
	for {
		start := time.Now()
		err := c.session(ctx)

		if ctx.Err() != nil {
			return nil
		}
		if Fatal(err) {
			c.setError(err.Error())
			return err
		}
		if err != nil {
			c.setError(err.Error())
			c.log.Warn("discord relay disconnected", slog.Any("error", err))
		}

		// A session that lasted a while was healthy, so the next failure
		// starts over from the short delay rather than inheriting the long one
		// a previous outage ended on.
		if time.Since(start) > time.Minute {
			backoff = minBackoff
		}
		// Jitter, so a Discord-wide restart does not bring every relay in the
		// world back in the same second.
		wait := backoff + time.Duration(rand.Int63n(int64(time.Second)))
		c.log.Info("discord relay reconnecting", slog.Duration("in", wait))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// frame is the envelope every gateway message arrives in.
type frame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s"`
	T  string          `json:"t"`
}

// session runs one connection from dial to close.
func (c *Client) session(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// A resume goes back to the URL the session was handed; a fresh identify
	// goes to the front door.
	dial := c.dialURL
	resuming := c.sessionID != "" && c.resumeURL != ""
	if resuming {
		dial = c.resumeURL + "?v=10&encoding=json"
	}

	conn, _, err := websocket.Dial(ctx, dial, &websocket.DialOptions{
		HTTPHeader: httpHeader(),
	})
	if err != nil {
		return fmt.Errorf("dial gateway: %w", err)
	}
	conn.SetReadLimit(readLimit)
	defer conn.CloseNow()

	c.writeMu.Lock()
	c.conn = conn
	c.writeMu.Unlock()

	// HELLO is always the first frame in, and carries the only number that
	// decides how the rest of the connection is paced.
	hello, err := readFrame(ctx, conn)
	if err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	if hello.Op != opHello {
		return fmt.Errorf("expected hello, got opcode %d", hello.Op)
	}
	var helloData struct {
		HeartbeatInterval int64 `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(hello.D, &helloData); err != nil || helloData.HeartbeatInterval <= 0 {
		return errors.New("hello carried no heartbeat interval")
	}
	interval := time.Duration(helloData.HeartbeatInterval) * time.Millisecond

	if resuming {
		c.log.Debug("discord relay resuming session", slog.String("session", c.sessionID))
		if err := c.send(ctx, opResume, map[string]any{
			"token":      c.token,
			"session_id": c.sessionID,
			"seq":        c.sequence,
		}); err != nil {
			return err
		}
	} else if err := c.identify(ctx); err != nil {
		return err
	}

	// acked is written by the read loop and read by the heartbeat, so it is
	// passed as a channel rather than a field: one signal per acknowledgement,
	// buffered so a beat that arrives while nobody is looking is not lost.
	acked := make(chan struct{}, 1)
	beats := make(chan struct{}, 1)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		if err := c.heartbeat(ctx, interval, acked, beats); err != nil {
			c.log.Debug("discord heartbeat stopped", slog.Any("error", err))
		}
	}()
	defer wg.Wait()

	for {
		f, err := readFrame(ctx, conn)
		if err != nil {
			c.setDisconnected()
			if ctx.Err() != nil {
				return nil
			}
			return c.classifyClose(err)
		}
		if f.S != nil {
			c.sequence = *f.S
		}

		switch f.Op {
		case opDispatch:
			c.dispatch(f)

		case opHeartbeat:
			// Discord asking for one out of band. Answering promptly is what
			// keeps it from tearing the connection down.
			select {
			case beats <- struct{}{}:
			default:
			}

		case opHeartbeatACK:
			select {
			case acked <- struct{}{}:
			default:
			}

		case opReconnect:
			// A planned move to another gateway node. The session survives it,
			// so the loop comes straight back with a resume.
			c.log.Debug("discord asked us to reconnect")
			c.setDisconnected()
			return nil

		case opInvalidSession:
			// The d field says whether the session can still be resumed. It
			// usually cannot, in which case the stored ids are cleared so the
			// next attempt identifies fresh — retrying a resume Discord has
			// already refused is how a relay ends up in a loop.
			var resumable bool
			_ = json.Unmarshal(f.D, &resumable)
			if !resumable {
				c.sessionID, c.resumeURL, c.sequence = "", "", 0
			}
			c.log.Debug("discord invalidated the session", slog.Bool("resumable", resumable))
			c.setDisconnected()
			// Discord asks for a short random wait before identifying again.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Duration(1+rand.Intn(4)) * time.Second):
			}
			return nil
		}
	}
}

// identify opens a fresh session.
func (c *Client) identify(ctx context.Context) error {
	return c.send(ctx, opIdentify, map[string]any{
		"token":   c.token,
		"intents": RelayIntents,
		"properties": map[string]string{
			"os":      "linux",
			"browser": "aural",
			"device":  "aural",
		},
		// The bot shows as online with a line saying what it is. A relay that
		// appears offline in the member list is one people assume is broken.
		"presence": map[string]any{
			"status": "online",
			"since":  0,
			"afk":    false,
			"activities": []map[string]any{
				{"name": "Aural", "type": 0, "state": "Bridging Aural"},
			},
		},
	})
}

// heartbeat keeps the connection alive and notices when the other end stops
// answering.
//
// The first beat is delayed by a random fraction of the interval, which is what
// Discord asks for: without it every bot in the world beats on the same tick.
func (c *Client) heartbeat(ctx context.Context, interval time.Duration, acked, beats <-chan struct{}) error {
	jitter := time.Duration(rand.Int63n(int64(interval)))
	timer := time.NewTimer(jitter)
	defer timer.Stop()

	outstanding := 0
	for {
		select {
		case <-ctx.Done():
			return nil

		case <-acked:
			outstanding = 0
			continue

		case <-beats:
			// An out-of-band request. It does not reset the timer: the regular
			// cadence is what Discord measures.

		case <-timer.C:
			timer.Reset(interval)
		}

		if outstanding >= missedBeatsBeforeReset {
			// The socket is open and the other end is not listening. Closing
			// here is what turns a zombie into a reconnect.
			return errors.New("discord stopped acknowledging heartbeats")
		}
		if err := c.send(ctx, opHeartbeat, c.sequenceOrNil()); err != nil {
			return err
		}
		outstanding++
	}
}

// sequenceOrNil renders the last sequence number for a heartbeat. A connection
// that has not received a numbered frame yet sends null, not zero.
func (c *Client) sequenceOrNil() any {
	if c.sequence == 0 {
		return nil
	}
	return c.sequence
}

// send writes one frame.
func (c *Client) send(ctx context.Context, op int, data any) error {
	raw, err := json.Marshal(frameOut{Op: op, D: data})
	if err != nil {
		return fmt.Errorf("encode opcode %d: %w", op, err)
	}

	c.writeMu.Lock()
	conn := c.conn
	c.writeMu.Unlock()
	if conn == nil {
		return errors.New("no connection")
	}

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	// The write itself is serialised: the heartbeat timer and the read loop
	// both reach here, and coder/websocket allows one writer at a time.
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return conn.Write(writeCtx, websocket.MessageText, raw)
}

// frameOut is an outgoing frame. It is a separate type from frame because the
// payload going out is a value rather than raw JSON.
type frameOut struct {
	Op int `json:"op"`
	D  any `json:"d"`
}

// readFrame reads and decodes one frame.
func readFrame(ctx context.Context, conn *websocket.Conn) (frame, error) {
	kind, raw, err := conn.Read(ctx)
	if err != nil {
		return frame{}, err
	}
	if kind != websocket.MessageText {
		return frame{}, errors.New("expected text frames")
	}
	var f frame
	if err := json.Unmarshal(raw, &f); err != nil {
		return frame{}, fmt.Errorf("decode frame: %w", err)
	}
	return f, nil
}

// classifyClose turns a read failure into either a plain error to retry or a
// fatal one to stop on. The distinction is the close code: a token Discord has
// refused will be refused again on every reconnect for as long as it is wrong.
func (c *Client) classifyClose(err error) error {
	code := int(websocket.CloseStatus(err))
	if reason, fatal := fatalCloseCodes[code]; fatal {
		return &fatalError{reason: reason}
	}
	return err
}

// --- dispatch ---------------------------------------------------------------

// dispatch routes one event to the handler and the caches.
func (c *Client) dispatch(f frame) {
	switch f.T {
	case "READY":
		var ready struct {
			User             User   `json:"user"`
			SessionID        string `json:"session_id"`
			ResumeGatewayURL string `json:"resume_gateway_url"`
		}
		if err := json.Unmarshal(f.D, &ready); err != nil {
			c.log.Warn("decode discord ready", slog.Any("error", err))
			return
		}
		c.sessionID = ready.SessionID
		c.resumeURL = ready.ResumeGatewayURL

		c.mu.Lock()
		c.self = ready.User
		c.connected = true
		c.lastError = ""
		c.mu.Unlock()

		c.log.Info("discord relay connected",
			slog.String("bot", ready.User.Username), slog.String("id", ready.User.ID))
		if c.handlers.Ready != nil {
			c.handlers.Ready(ready.User)
		}

	case "RESUMED":
		c.mu.Lock()
		c.connected = true
		c.lastError = ""
		c.mu.Unlock()
		c.log.Debug("discord relay resumed")

	case "GUILD_CREATE", "GUILD_UPDATE":
		var g Guild
		if err := json.Unmarshal(f.D, &g); err != nil {
			return
		}
		c.cacheGuild(g)
		if c.handlers.GuildAvailable != nil {
			c.handlers.GuildAvailable(g)
		}

	case "GUILD_DELETE":
		var g Guild
		if err := json.Unmarshal(f.D, &g); err != nil {
			return
		}
		// Unavailable is an outage, not a removal: the bot is still in the
		// guild and the cache should survive it.
		if !g.Unavailable {
			c.forgetGuild(g.ID)
		}

	case "CHANNEL_CREATE", "CHANNEL_UPDATE", "THREAD_CREATE", "THREAD_UPDATE":
		var ch Channel
		if err := json.Unmarshal(f.D, &ch); err == nil {
			c.cacheChannel(ch)
		}

	case "CHANNEL_DELETE", "THREAD_DELETE":
		var ch Channel
		if err := json.Unmarshal(f.D, &ch); err == nil {
			c.mu.Lock()
			delete(c.channels, ch.ID)
			c.mu.Unlock()
		}

	case "GUILD_ROLE_CREATE", "GUILD_ROLE_UPDATE":
		var payload struct {
			Role Role `json:"role"`
		}
		if err := json.Unmarshal(f.D, &payload); err == nil {
			c.mu.Lock()
			c.roles[payload.Role.ID] = payload.Role
			c.mu.Unlock()
		}

	case "GUILD_ROLE_DELETE":
		var payload struct {
			RoleID string `json:"role_id"`
		}
		if err := json.Unmarshal(f.D, &payload); err == nil {
			c.mu.Lock()
			delete(c.roles, payload.RoleID)
			c.mu.Unlock()
		}

	case "MESSAGE_CREATE":
		if c.handlers.Message == nil {
			return
		}
		var m Message
		if err := json.Unmarshal(f.D, &m); err != nil {
			c.log.Warn("decode discord message", slog.Any("error", err))
			return
		}
		c.handlers.Message(m)

	case "MESSAGE_UPDATE":
		if c.handlers.MessageEdited == nil {
			return
		}
		var m Message
		if err := json.Unmarshal(f.D, &m); err != nil {
			return
		}
		c.handlers.MessageEdited(m)

	case "MESSAGE_DELETE":
		if c.handlers.MessageDeleted == nil {
			return
		}
		var payload struct {
			ID        string `json:"id"`
			ChannelID string `json:"channel_id"`
		}
		if err := json.Unmarshal(f.D, &payload); err == nil {
			c.handlers.MessageDeleted(payload.ChannelID, []string{payload.ID})
		}

	case "MESSAGE_DELETE_BULK":
		if c.handlers.MessageDeleted == nil {
			return
		}
		var payload struct {
			IDs       []string `json:"ids"`
			ChannelID string   `json:"channel_id"`
		}
		if err := json.Unmarshal(f.D, &payload); err == nil && len(payload.IDs) > 0 {
			c.handlers.MessageDeleted(payload.ChannelID, payload.IDs)
		}
	}
}

// cacheGuild records a guild and everything in it that a mention can name.
func (c *Client) cacheGuild(g Guild) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.guilds[g.ID] = g
	for _, r := range g.Roles {
		c.roles[r.ID] = r
	}
	for _, ch := range append(append([]Channel{}, g.Channels...), g.Threads...) {
		c.channels[ch.ID] = ch
	}
}

// forgetGuild drops a guild the bot was removed from, and everything that was
// only reachable through it.
func (c *Client) forgetGuild(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	g, ok := c.guilds[id]
	if !ok {
		return
	}
	for _, r := range g.Roles {
		delete(c.roles, r.ID)
	}
	for _, ch := range append(append([]Channel{}, g.Channels...), g.Threads...) {
		delete(c.channels, ch.ID)
	}
	delete(c.guilds, id)
}

func (c *Client) cacheChannel(ch Channel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.channels[ch.ID] = ch
}

func (c *Client) setDisconnected() {
	c.mu.Lock()
	c.connected = false
	c.mu.Unlock()
}

func (c *Client) setError(reason string) {
	c.mu.Lock()
	c.connected = false
	c.lastError = reason
	c.mu.Unlock()
}
