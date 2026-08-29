package httpapi

import (
	"net/http"
	"sync"

	"github.com/starhui-dev/bablo/internal/config"
)

// Readiness tracks dependency probes. A configured dependency is not ready until
// the owning stage performs a real connection/probe and calls Set.
type Readiness struct {
	mu     sync.RWMutex
	checks map[string]string
}

func NewReadiness(cfg config.Config) *Readiness {
	postgres := "not_configured"
	if cfg.DatabaseURL != "" {
		postgres = "not_initialized"
	}
	redis := "not_configured"
	if cfg.RedisURL != "" {
		redis = "not_initialized"
	}
	return &Readiness{checks: map[string]string{
		"http":      "ok",
		"postgres":  postgres,
		"redis":     redis,
		"inference": "not_initialized",
	}}
}

// Set records a successful or failed probe for a named dependency.
func (r *Readiness) Set(name string, ready bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ready {
		r.checks[name] = "ok"
		return
	}
	r.checks[name] = "not_ready"
}

// Snapshot returns a copy so handlers never hold the lock while encoding JSON.
func (r *Readiness) Snapshot() (bool, map[string]string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	checks := make(map[string]string, len(r.checks))
	ready := true
	for name, state := range r.checks {
		checks[name] = state
		if state != "ok" {
			ready = false
		}
	}
	return ready, checks
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	written, err := w.ResponseWriter.Write(payload)
	w.bytes += written
	return written, err
}

func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
