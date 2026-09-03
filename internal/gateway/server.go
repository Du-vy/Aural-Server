package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/aural-chat/aural-server/internal/auth"
	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/protocol"
	"github.com/aural-chat/aural-server/internal/store"
)

// shutdownGrace is how long in-flight HTTP requests get to finish on stop.
const shutdownGrace = 5 * time.Second

// pendingHeadroom is how many connections may be open beyond the user limit.
//
// max_users counts identities, and a connection has none until it
// authenticates, so without a ceiling here an exposed server can be kept busy
// by sockets that never say who they are. The headroom is sized to let
// everybody on a full server be reconnecting at once — which happens every
// time the server restarts — without leaving room for much else.
const pendingHeadroom = 256

// Server ties the HTTP listener to the hub.
type Server struct {
	cfg  *config.Config
	st   *store.Store
	log  *slog.Logger
	hub  *Hub
	http *http.Server
	seq  atomic.Int64
	// live counts open WebSocket connections, authenticated or not. The hub
	// counts identities, which is a different number and the one max_users is
	// about; this one is what bounds the sockets underneath them.
	live atomic.Int64
	// uploads, unfurls and klipy throttle the HTTP endpoints, which have no
	// session of their own to hang a limiter off. Each costs something
	// different — disk, an outbound fetch, somebody else's quota — so each
	// gets its own rate rather than sharing one.
	uploads *userLimiters
	unfurls *userLimiters
	klipy   *userLimiters
	// deliveries throttles the webhook endpoints. It is keyed by webhook
	// rather than by identity, because a webhook has no identity: the URL is
	// the caller, and it is the URL a sender has to be paced by.
	deliveries *userLimiters
	// trustedProxies is server.trusted_proxies, parsed. Empty means no
	// forwarding header is believed, which is right for a server reached
	// directly.
	trustedProxies []netip.Prefix
}

// New builds the gateway. cfgPath is where runtime configuration edits are
// written back; pass an empty string to keep them in memory only.
func New(ctx context.Context, cfg *config.Config, cfgPath string, st *store.Store, log *slog.Logger) (*Server, error) {
	hub, err := NewHub(ctx, cfg, cfgPath, st, log)
	if err != nil {
		return nil, err
	}

	trusted, err := parseTrustedProxies(cfg.Server.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("gateway: server.trusted_proxies: %w", err)
	}

	s := &Server{
		cfg: cfg, st: st, log: log, hub: hub,
		uploads:        newUserLimiters(uploadBurst, uploadsPerSecond),
		unfurls:        newUserLimiters(unfurlBurst, unfurlsPerSecond),
		klipy:          newUserLimiters(klipyBurst, klipyPerSecond),
		deliveries:     newUserLimiters(deliveryBurst, deliveriesPerSecond),
		trustedProxies: trusted,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", s.handleInfo)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /ws", s.handleWebSocket)
	mux.HandleFunc("POST /upload", s.handleUpload)
	mux.HandleFunc("OPTIONS /upload", s.handlePreflight)
	mux.HandleFunc("POST /upload/avatar", s.handleAvatarUpload)
	mux.HandleFunc("OPTIONS /upload/avatar", s.handlePreflight)
	mux.HandleFunc("POST /upload/banner", s.handleBannerUpload)
	mux.HandleFunc("OPTIONS /upload/banner", s.handlePreflight)
	mux.HandleFunc("POST /upload/server-icon", s.handleServerIconUpload)
	mux.HandleFunc("OPTIONS /upload/server-icon", s.handlePreflight)
	mux.HandleFunc("POST /upload/emoji", s.handleEmojiUpload)
	mux.HandleFunc("OPTIONS /upload/emoji", s.handlePreflight)
	mux.HandleFunc("POST /upload/sticker", s.handleStickerUpload)
	mux.HandleFunc("OPTIONS /upload/sticker", s.handlePreflight)
	mux.HandleFunc("POST /upload/sound", s.handleSoundUploadHTTP)
	mux.HandleFunc("OPTIONS /upload/sound", s.handlePreflight)
	mux.HandleFunc("GET "+uploadPrefix+"{key}/{filename}", s.handleAttachment)
	mux.HandleFunc("OPTIONS "+uploadPrefix+"{key}/{filename}", s.handlePreflight)
	mux.HandleFunc("GET /unfurl", s.handleUnfurl)
	mux.HandleFunc("OPTIONS /unfurl", s.handlePreflight)
	mux.HandleFunc("GET /klipy/{kind}/{action}", s.handleKlipy)
	mux.HandleFunc("OPTIONS /klipy/{kind}/{action}", s.handlePreflight)

	// The webhook API, mounted where Discord mounts its own so that an
	// application already posting to a Discord webhook needs no change but the
	// URL.
	s.registerWebhookRoutes(mux, webhookPrefix)

	s.http = &http.Server{
		Addr: cfg.Address(),
		// An explicit API version in the path — /api/v10/webhooks/... — is
		// normalised away before routing, so both shapes of a pasted Discord
		// URL reach the same handlers.
		Handler:           stripAPIVersion(mux),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: a WebSocket connection is meant to stay open.
		IdleTimeout: 120 * time.Second,
		BaseContext: func(_ net.Listener) context.Context { return ctx },
	}
	return s, nil
}

// Hub exposes the hub, which the command layer needs for bootstrap tasks.
func (s *Server) Hub() *Hub { return s.hub }

// Handler exposes the routes without the listener, which is what lets a test
// drive the gateway over an httptest server.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Run serves until ctx is cancelled, then drains and stops.
func (s *Server) Run(ctx context.Context) error {
	errs := make(chan error, 1)

	if s.hub.Files() != nil {
		// Before the listener, deliberately: the sweep is only unambiguous
		// while nothing is uploading.
		s.sweepOrphanedFiles(ctx)
	}
	go s.sweepMaintenance(ctx)
	// Keeps the address the relay advertises current on a connection whose own
	// address is not. It returns immediately on a server configured with a
	// literal, which has nothing to watch.
	go s.hub.WatchPublicIP(ctx)
	// Publishes this server's address to a dynamic DNS provider. It returns
	// immediately unless the ddns block is switched on.
	go s.watchDDNS(ctx)

	if s.cfg.TLS.Enabled {
		// A certificate this server obtains for itself has to exist before the
		// listener asks for it, so the first order runs here, in the open,
		// where its failure is reported rather than logged into a goroutine.
		if s.cfg.TLS.ACME.Enabled {
			if err := s.startACME(ctx); err != nil {
				return err
			}
		}

		// Loaded here rather than left to ListenAndServeTLS, so that a renewal
		// is picked up by the running server instead of waiting for a restart
		// nobody schedules. It is also what makes the renewals below take
		// effect: they write the files, and this notices.
		reloader, err := newCertReloader(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile, s.log)
		if err != nil {
			return err
		}
		s.http.TLSConfig = &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: reloader.GetCertificate,
		}
	}

	go func() {
		var err error
		if s.cfg.TLS.Enabled {
			s.log.Info("listening", slog.String("address", s.cfg.Address()), slog.String("scheme", "wss"))
			// Both empty: the certificate comes from TLSConfig.GetCertificate.
			err = s.http.ListenAndServeTLS("", "")
		} else {
			s.log.Info("listening", slog.String("address", s.cfg.Address()), slog.String("scheme", "ws"))
			err = s.http.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	s.log.Info("shutting down")
	if s.hub.relay != nil {
		s.hub.relay.Close()
	}
	for _, session := range s.hub.Sessions() {
		session.Close(websocket.StatusGoingAway, "server shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("gateway: shutdown: %w", err)
	}
	return <-errs
}

// handleInfo answers the public preview of the server. It is deliberately
// unauthenticated: a client shows it before connecting, and the future Aural
// Hub will read it to list servers.
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(s.hub.serverInfo()); err != nil {
		s.log.Debug("write info response", slog.Any("error", err))
	}
}

// handleHealth is a liveness probe for process supervisors.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// handleWebSocket upgrades a request and runs the session on it.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Where the connection came from, as far as this server can honestly tell.
	// It is the one thing an operator of an exposed server wants in the log
	// and, until now, the one thing that was not in it.
	peer := clientIP(r, s.trustedProxies)

	origin := r.Header.Get("Origin")
	if !s.cfg.OriginAllowed(origin) {
		s.log.Warn("rejected websocket origin",
			slog.String("origin", origin), slog.String("peer", peer))
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}

	// Bans are deliberately not enforced here, where the only thing known
	// about a connection is where it came from.
	//
	// Refusing a banned address at the upgrade would save a socket and four
	// rate limiters, and would also be the one check that cannot tell who is
	// asking. An address is shared — a household, a university, one phone
	// network — so on the day somebody bans a troublemaker from the same
	// network as themselves, that check is what locks the owner out of their
	// own server with no way back in. Every ban is therefore enforced on the
	// authentication op, where the identity is known and the owner is exempt.
	// An unauthenticated socket is already bounded by the auth deadline and by
	// the headroom above max_users.

	// Counted before the upgrade, so a flood is refused with an HTTP status a
	// client can read rather than with a socket that is opened and then shut.
	live := s.live.Add(1)
	defer s.live.Add(-1)
	if limit := int64(s.cfg.Server.MaxUsers) + pendingHeadroom; live > limit {
		s.log.Warn("refused a connection: too many are already open",
			slog.Int64("open", live), slog.Int64("limit", limit), slog.String("peer", peer))
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Origin is checked above against the server configuration, which is
		// the policy a self-hosted server actually wants.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionContextTakeover,
	})
	if err != nil {
		s.log.Debug("websocket upgrade failed", slog.Any("error", err))
		return
	}

	// The peer travels with every line this session logs, so an authentication
	// that fails, a rate limit that trips or a client that falls behind can be
	// traced back to where it came from.
	session := newSession(s.seq.Add(1), s.hub, conn, peer, s.log.With(slog.String("peer", peer)))
	defer s.finishSession(session)

	session.Send(protocol.Event(protocol.EvHello, protocol.Hello{
		Server:      s.hub.serverInfo(),
		HeartbeatMs: int(heartbeatInterval.Milliseconds()),
		DeviceSalt:  s.hub.DeviceSalt(),
	}))
	session.serve(r.Context())
}

// finishSession removes a closed session and, unless it was displaced by a new
// connection of the same identity, announces the departure.
func (s *Server) finishSession(session *Session) {
	session.Close(websocket.StatusNormalClosure, "")

	if !session.Authed() {
		return
	}
	userID := session.UserID()
	status := session.User().Status

	// Whether this session still holds the identity, or a newer connection has
	// taken it over. It decides everything below, because the audio plane is
	// keyed by user rather than by session: tearing it down on behalf of a
	// displaced connection would cut off the call the new one has already
	// opened. Somebody who drops and comes straight back is exactly the person
	// this happens to, so it is not a rare ordering.
	current, stillOnline := s.hub.SessionForUser(userID)
	displaced := stillOnline && current != session

	if !displaced {
		// The audio plane first: it has a room to repair and, in client_host
		// mode, possibly an election to run, and both need the session still
		// findable.
		if channelID := session.voiceChannel(); channelID != 0 {
			s.hub.leaveVoice(session, channelID, false)
		}
		if s.hub.relay != nil {
			s.hub.relay.LeaveAll(userID)
		}
	} else {
		// The identity's audio belongs to the connection that now holds it.
		// This session only has to stop believing it is in a channel.
		session.clearVoiceSession(0)
	}
	s.hub.Remove(session)

	// Read again rather than reusing the check above: a new connection may have
	// arrived in between, and presence is the one thing that must not announce
	// a departure for somebody who is still here.
	if _, taken := s.hub.SessionForUser(userID); taken {
		// A newer connection took the identity over; presence never dropped.
		return
	}
	if !HidesPresence(status) {
		// Nobody was told they were here, so nobody is told they have gone: a
		// departure for somebody the rest of the server already believes is
		// offline is what would give away that they were not.
		//
		// The frame says the connection ended, not that the person left. A
		// member drops into the offline part of the list they were always in;
		// a guest, whose identity lasts no longer than the connection, drops
		// out of it entirely.
		s.hub.Broadcast(protocol.Event(protocol.EvUserDisconnected, protocol.UserDisconnectedEvent{UserID: userID}))
	}
	s.log.Info("disconnected", slog.Int64("user", userID))
}

// EnsureOwnerToken mints the one-time token that claims the server, unless
// somebody already owns it or a token is still outstanding. It returns the
// token to print exactly once; an empty string means there was nothing to
// issue.
func EnsureOwnerToken(ctx context.Context, st *store.Store, hub *Hub) (string, error) {
	if hub.AdminRoleID() == 0 {
		return "", errors.New("gateway: the managed admin role is missing")
	}

	owner, err := st.OwnerUserID(ctx)
	if err != nil {
		return "", err
	}
	if owner != 0 {
		return "", nil
	}

	if _, err := st.Meta(ctx, store.MetaOwnerTokenHash); err == nil {
		// A token is already outstanding. Only its hash is stored, so it cannot
		// be shown again; rotating is the way to get a fresh one.
		return "", nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}

	return RotateOwnerToken(ctx, st)
}

// RotateOwnerToken replaces any outstanding owner token with a new one and
// returns it.
func RotateOwnerToken(ctx context.Context, st *store.Store) (string, error) {
	raw, hash, err := auth.NewOwnerToken()
	if err != nil {
		return "", err
	}
	if err := st.SetMeta(ctx, store.MetaOwnerTokenHash, hash); err != nil {
		return "", err
	}
	return raw, nil
}
