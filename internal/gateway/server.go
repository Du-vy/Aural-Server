package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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

// Server ties the HTTP listener to the hub.
type Server struct {
	cfg  *config.Config
	st   *store.Store
	log  *slog.Logger
	hub  *Hub
	http *http.Server
	seq  atomic.Int64
	// uploads throttles the HTTP upload endpoint, which has no session of its
	// own to hang a limiter off.
	uploads *uploadLimiters
}

// New builds the gateway. cfgPath is where runtime configuration edits are
// written back; pass an empty string to keep them in memory only.
func New(ctx context.Context, cfg *config.Config, cfgPath string, st *store.Store, log *slog.Logger) (*Server, error) {
	hub, err := NewHub(ctx, cfg, cfgPath, st, log)
	if err != nil {
		return nil, err
	}

	s := &Server{cfg: cfg, st: st, log: log, hub: hub, uploads: newUploadLimiters()}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", s.handleInfo)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /ws", s.handleWebSocket)
	mux.HandleFunc("POST /upload", s.handleUpload)
	mux.HandleFunc("OPTIONS /upload", s.handlePreflight)
	mux.HandleFunc("GET "+uploadPrefix+"{key}/{filename}", s.handleAttachment)
	mux.HandleFunc("OPTIONS "+uploadPrefix+"{key}/{filename}", s.handlePreflight)
	mux.HandleFunc("GET /unfurl", s.handleUnfurl)
	mux.HandleFunc("OPTIONS /unfurl", s.handlePreflight)

	s.http = &http.Server{
		Addr:              cfg.Address(),
		Handler:           mux,
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
		go s.sweepPending(ctx)
	}

	go func() {
		var err error
		if s.cfg.TLS.Enabled {
			s.log.Info("listening", slog.String("address", s.cfg.Address()), slog.String("scheme", "wss"))
			err = s.http.ListenAndServeTLS(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
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
	origin := r.Header.Get("Origin")
	if !s.cfg.OriginAllowed(origin) {
		s.log.Warn("rejected websocket origin", slog.String("origin", origin))
		http.Error(w, "origin not allowed", http.StatusForbidden)
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

	session := newSession(s.seq.Add(1), s.hub, conn, s.log)
	defer s.finishSession(session)

	session.Send(protocol.Event(protocol.EvHello, protocol.Hello{
		Server:      s.hub.serverInfo(),
		HeartbeatMs: int(heartbeatInterval.Milliseconds()),
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
	s.hub.Remove(session)

	if _, stillOnline := s.hub.SessionForUser(userID); stillOnline {
		// A newer connection took the identity over; presence never dropped.
		return
	}
	s.hub.Broadcast(protocol.Event(protocol.EvUserDisconnected, protocol.UserDisconnectedEvent{UserID: userID}))
	s.log.Info("disconnected", slog.Int64("user", userID))
}

// EnsureOwnerToken mints the one-time token that grants the admin role, unless
// somebody already holds that role or a token is still outstanding. It returns
// the token to print exactly once; an empty string means there was nothing to
// issue.
func EnsureOwnerToken(ctx context.Context, st *store.Store, hub *Hub) (string, error) {
	adminRoleID := hub.AdminRoleID()
	if adminRoleID == 0 {
		return "", errors.New("gateway: the managed admin role is missing")
	}

	admins, err := st.CountUsersWithRole(ctx, adminRoleID)
	if err != nil {
		return "", err
	}
	if admins > 0 {
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
