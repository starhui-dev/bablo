// Package httpapi owns the bootstrap HTTP surface shared by control and data planes.
package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/starhui-dev/bablo/internal/config"
)

// Server is the Bablo HTTP server. Domain handlers are added in later stages.
type Server struct {
	config              config.Config
	logger              *slog.Logger
	version             string
	httpServer          *http.Server
	requestCount        atomic.Uint64
	readiness           *Readiness
	authHandler         http.Handler
	apiKeyHandler       http.Handler
	modelHandler        http.Handler
	adminCatalogHandler http.Handler
}

// Option configures optional domain HTTP surfaces.
type Option func(*Server)

// WithAuthHandler mounts the Web Session authentication surface.
func WithAuthHandler(handler http.Handler) Option {
	return func(server *Server) {
		server.authHandler = handler
	}
}

// WithAPIKeyHandler mounts the protected user API Key surface.
func WithAPIKeyHandler(handler http.Handler) Option {
	return func(server *Server) {
		server.apiKeyHandler = handler
	}
}

// WithModelHandler mounts the authenticated user model catalog.
func WithModelHandler(handler http.Handler) Option {
	return func(server *Server) {
		server.modelHandler = handler
	}
}

// WithAdminCatalogHandler mounts administrator-only model, provider, and price management.
func WithAdminCatalogHandler(handler http.Handler) Option {
	return func(server *Server) {
		server.adminCatalogHandler = handler
	}
}

// New constructs the bootstrap server without opening a listener.
func New(cfg config.Config, logger *slog.Logger, version string, options ...Option) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	server := &Server{
		config:    cfg,
		logger:    logger,
		version:   version,
		readiness: NewReadiness(cfg),
	}
	for _, option := range options {
		if option != nil {
			option(server)
		}
	}
	server.httpServer = &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           server.routes(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
	return server
}

// Handler exposes the fully wrapped handler for tests and future embedding.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// Start blocks until the HTTP server stops.
func (s *Server) Start() error {
	s.logger.Info("bablo_http_started", "addr", s.config.HTTPAddr, "version", s.version)
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown stops accepting requests and waits for active handlers.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// SetDependencyReady is used by later data/inference stages after real probes pass.
func (s *Server) SetDependencyReady(name string, ready bool) {
	s.readiness.Set(name, ready)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/readyz", s.readyz)
	mux.HandleFunc("/metrics", s.metrics)
	authHandler := s.authHandler
	if authHandler == nil {
		authHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": map[string]string{
					"type":       "authentication_error",
					"code":       "auth_unavailable",
					"message":    "认证服务尚未配置。",
					"request_id": RequestID(r.Context()),
				},
			})
		})
	}
	mux.Handle("/api/v1/auth/", authHandler)
	mux.Handle("/api/v1/admin/users/", authHandler)
	if s.apiKeyHandler != nil {
		mux.Handle("/api/v1/me/api-keys", s.apiKeyHandler)
		mux.Handle("/api/v1/me/api-keys/", s.apiKeyHandler)
	}
	if s.modelHandler != nil {
		mux.Handle("/api/v1/models", s.modelHandler)
	}
	if s.adminCatalogHandler != nil {
		mux.Handle("/api/v1/admin/models", s.adminCatalogHandler)
		mux.Handle("/api/v1/admin/models/", s.adminCatalogHandler)
		mux.Handle("/api/v1/admin/providers", s.adminCatalogHandler)
		mux.Handle("/api/v1/admin/providers/", s.adminCatalogHandler)
		mux.Handle("/api/v1/admin/provider-models", s.adminCatalogHandler)
		mux.Handle("/api/v1/admin/provider-models/", s.adminCatalogHandler)
		mux.Handle("/api/v1/admin/prices", s.adminCatalogHandler)
		mux.Handle("/api/v1/admin/routes", s.adminCatalogHandler)
		mux.Handle("/api/v1/admin/routes/", s.adminCatalogHandler)
		mux.Handle("/api/v1/admin/credentials", s.adminCatalogHandler)
		mux.Handle("/api/v1/admin/credentials/", s.adminCatalogHandler)
		mux.Handle("/api/v1/admin/credential-pools", s.adminCatalogHandler)
		mux.Handle("/api/v1/admin/credential-pools/", s.adminCatalogHandler)
		mux.Handle("/api/v1/admin/prices/", s.adminCatalogHandler)
	}
	mux.HandleFunc("/", s.notFound)
	return withRequestID(mux, s.logger, &s.requestCount)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}

	ready, checks := s.readiness.Snapshot()
	status := http.StatusOK
	state := "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		state = "not_ready"
	}
	writeJSON(w, status, map[string]any{
		"status":  state,
		"version": s.version,
		"checks":  checks,
	})
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte("# HELP bablo_http_requests_total Total HTTP requests received.\n"))
	_, _ = w.Write([]byte("# TYPE bablo_http_requests_total counter\n"))
	_, _ = w.Write([]byte("bablo_http_requests_total "))
	_, _ = w.Write([]byte(formatUint(s.requestCount.Load())))
	_, _ = w.Write([]byte("\n# HELP bablo_build_info Build information.\n# TYPE bablo_build_info gauge\n"))
	_, _ = w.Write([]byte("bablo_build_info{version=\"" + escapeLabel(s.version) + "\"} 1\n"))
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// The helpers stay local until metrics is replaced by the project's chosen library.
func formatUint(value uint64) string {
	if value == 0 {
		return "0"
	}
	var buf [20]byte
	index := len(buf)
	for value > 0 {
		index--
		buf[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buf[index:])
}

func escapeLabel(value string) string {
	quoted, _ := json.Marshal(value)
	if len(quoted) >= 2 {
		return string(quoted[1 : len(quoted)-1])
	}
	return ""
}
