package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type requestIDKey struct{}

var requestSequence atomic.Uint64

// RequestID returns the request ID assigned by the HTTP middleware.
func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}

func withRequestID(next http.Handler, logger *slog.Logger, requestCount *atomic.Uint64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := normalizeRequestID(r.Header.Get("X-Request-ID"))
		if requestID == "" {
			requestID = newRequestID()
		}
		requestCount.Add(1)
		w.Header().Set("X-Request-ID", requestID)

		started := time.Now()
		recorder := &statusWriter{ResponseWriter: w}
		request := r.WithContext(context.WithValue(r.Context(), requestIDKey{}, requestID))
		next.ServeHTTP(recorder, request)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}
		logger.Info("http_request",
			"request_id", requestID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"bytes", recorder.bytes,
			"latency_ms", time.Since(started).Milliseconds(),
		)
	})
}

func normalizeRequestID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return ""
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' {
			continue
		}
		return ""
	}
	return value
}

func newRequestID() string {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return "req_" + hex.EncodeToString(bytes[:])
	}
	return "req_fallback_" + formatUint(requestSequence.Add(1))
}
