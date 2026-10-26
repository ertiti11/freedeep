// Package server is the OpenAI / Anthropic / Responses compatible HTTP proxy.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ertiti11/freedeep/internal/config"
	"github.com/ertiti11/freedeep/internal/deepseek"
	"github.com/ertiti11/freedeep/internal/state"
)

// ContextCompactedHeader is set when the upstream prompt was compacted.
const ContextCompactedHeader = "X-Freedeep-Context-Compacted"

const maxBodyBytes = 10 << 20

// Server is the proxy.
type Server struct {
	cfg      *config.Config
	store    *state.Store
	ds       *deepseek.Client
	host, ip string
	inFlight atomic.Int64
	// sleep is overridable so tests do not wait on retry back-off.
	sleep func(context.Context, time.Duration) bool
}

// New creates a server.
func New(cfg *config.Config, store *state.Store, ds *deepseek.Client) *Server {
	host, _ := os.Hostname()
	return &Server{cfg: cfg, store: store, ds: ds, host: host, ip: primaryIPv4(), sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func primaryIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "localhost"
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				return ipn.IP.String()
			}
		}
	}
	return "localhost"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func errorBody(message, typ string, extra map[string]any) map[string]any {
	e := map[string]any{"message": message, "type": typ}
	for k, v := range extra {
		e[k] = v
	}
	return map[string]any{"error": e}
}

// SetCORSHeaders sets the static CORS response headers.
func SetCORSHeaders(h http.Header) {
	h.Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Agent-Session, Anthropic-Version, X-Api-Key")
	h.Set("Access-Control-Expose-Headers", ContextCompactedHeader)
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleHealth)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /v1/model-capabilities", s.handleCapabilities)
	mux.HandleFunc("GET /api/model-capabilities", s.handleCapabilities)
	mux.HandleFunc("GET /v1/sessions", s.handleSessions)
	mux.HandleFunc("POST /reset-session", s.handleResetSession)
	mux.HandleFunc("POST /v1/chat/completions", s.completionHandler(modeOpenAI))
	mux.HandleFunc("POST /v1/messages", s.completionHandler(modeAnthropic))
	mux.HandleFunc("POST /v1/responses", s.completionHandler(modeResponses))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, errorBody("Not found", "not_found", nil))
	})
	return s.middleware(mux)
}

func (s *Server) authorized(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	// Anthropic SDKs send the key as x-api-key instead of a bearer token.
	if auth == "" && r.Header.Get("X-Api-Key") != "" {
		auth = "Bearer " + r.Header.Get("X-Api-Key")
	}
	return config.IsProxyAuthorized(auth, s.cfg.ProxyAPIKey)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		w.Header().Set("Vary", "Origin")
		if !config.IsBrowserOriginAllowed(origin, s.cfg.CORSOrigins) {
			writeJSON(w, http.StatusForbidden, errorBody("Browser origin is not allowed", "cors_error", nil))
			return
		}
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", config.NormalizeOrigin(origin))
		}
		SetCORSHeaders(w.Header())
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		public := r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/health" || r.URL.Path == "/readyz")
		if !public && !s.authorized(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, errorBody("Invalid or missing proxy API key", "authentication_error", nil))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	health := map[string]any{"status": "ok", "service": "freedeep", "watermark": Watermark}
	if s.cfg.ProxyAPIKey == "" || s.authorized(r) {
		health["models"] = deepseek.SupportedModelIDs()
		health["unsupported_models"] = deepseek.UnsupportedModelIDs()
		health["agents"] = s.store.SessionCount()
		health["in_flight"] = s.inFlight.Load()
		health["accounts"] = s.store.Accounts()
		health["config_ready"] = s.store.HasReadyCredentials()
		health["session_reuse"] = map[string]any{
			"strategy":     "sticky per x-agent-session/user",
			"ttl_minutes":  int(math.Round(state.SessionTTL.Minutes())),
			"max_messages": state.MaxMessageDepth,
			"reset_all":    "POST /reset-session?agent=all",
		}
	}
	writeJSON(w, http.StatusOK, health)
}

// handleReady is a readiness probe: 503 unless an account can serve now.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ready, total := s.store.ReadyCount()
	status := http.StatusOK
	if ready == 0 {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ready": ready > 0, "ready_accounts": ready, "total_accounts": total})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	var data []any
	for _, m := range deepseek.Models {
		if m.Supported {
			data = append(data, map[string]any{"id": m.ID, "object": "model", "created": 1700000000, "owned_by": "deepseek-web", "real_model": m.RealModel, "capabilities": m.Capabilities})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{}
	for _, m := range deepseek.Models {
		data[m.ID] = m
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "model_capabilities", "watermark": Watermark, "data": data})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	list := s.store.Sessions()
	writeJSON(w, http.StatusOK, map[string]any{"agents": list, "total": len(list)})
}

func (s *Server) handleResetSession(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		agent = "default"
	}
	if agent == "all" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "all_sessions_cleared", "count": s.store.ClearSessions()})
		return
	}
	n, preview, ok := s.store.ResetAgent(agent)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "No session for agent: " + agent})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "session_reset", "agent": agent, "history_preserved": n, "history": preview})
}

// agentID picks the session key: X-Agent-Session, then the request's
// session/user field, then the client IP ("dev-agent" for loopback).
func agentID(r *http.Request, session, user string) string {
	if v := r.Header.Get("X-Agent-Session"); v != "" {
		return v
	}
	if session != "" {
		return session
	}
	if user != "" {
		return user
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if config.IsLoopbackHost(host) {
		return "dev-agent"
	}
	if host == "" {
		return "unknown"
	}
	return host
}

// SweepLoop evicts idle sessions every interval until ctx is done.
func (s *Server) SweepLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.store.SweepIdle(2 * state.SessionTTL)
		}
	}
}

// ListenAndServe runs the server until ctx is cancelled, then drains
// in-flight requests for up to 10 seconds.
func (s *Server) ListenAndServe(ctx context.Context) error {
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go s.SweepLoop(ctx, 10*time.Minute)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	slog.Info("server listening", "addr", "http://"+addr)
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
