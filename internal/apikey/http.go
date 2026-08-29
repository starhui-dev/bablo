package apikey

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/auth"
	"github.com/starhui-dev/bablo/internal/httpapi"
)

const (
	apiKeyPath      = "/api/v1/me/api-keys"
	maximumJSONBody = 32 << 10
)

type handler struct {
	service *Service
	logger  *slog.Logger
}

// NewHandler creates the user self-service API Key HTTP surface.
func NewHandler(service *Service, logger *slog.Logger) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("API key HTTP handler requires a service")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &handler{service: service, logger: logger}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, auth.ErrSessionInvalid)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, apiKeyPath)
	if path == r.URL.Path {
		h.writeError(w, r, ErrNotFound)
		return
	}
	if path == "" || path == "/" {
		switch r.Method {
		case http.MethodGet:
			h.list(w, r, session.UserID)
		case http.MethodPost:
			h.create(w, r, session.UserID)
		default:
			w.Header().Set("Allow", "GET, POST")
			h.writeError(w, r, errMethodNotAllowed)
		}
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 1 || len(parts) > 2 {
		h.writeError(w, r, ErrNotFound)
		return
	}
	keyID, err := uuid.Parse(parts[0])
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodPatch {
			w.Header().Set("Allow", "PATCH")
			h.writeError(w, r, errMethodNotAllowed)
			return
		}
		h.update(w, r, session.UserID, keyID)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		h.writeError(w, r, errMethodNotAllowed)
		return
	}
	switch parts[1] {
	case "rotate":
		h.rotate(w, r, session.UserID, keyID)
	case "revoke":
		h.revoke(w, r, session.UserID, keyID)
	default:
		h.writeError(w, r, ErrNotFound)
	}
}

var errMethodNotAllowed = errors.New("method not allowed")

type createRequest struct {
	Name               string     `json:"name"`
	ExpiresAt          *time.Time `json:"expires_at"`
	AllowedModels      []string   `json:"allowed_models"`
	IPAllowlist        []string   `json:"ip_allowlist"`
	RPMLimit           *int64     `json:"rpm_limit"`
	TPMLimit           *int64     `json:"tpm_limit"`
	DailyBudgetMinor   *int64     `json:"daily_budget_minor"`
	MonthlyBudgetMinor *int64     `json:"monthly_budget_minor"`
}

type optionalJSON[T any] struct {
	Set   bool
	Null  bool
	Value T
}

func (value *optionalJSON[T]) UnmarshalJSON(data []byte) error {
	value.Set = true
	if string(data) == "null" {
		value.Null = true
		return nil
	}
	return json.Unmarshal(data, &value.Value)
}

type updateRequest struct {
	Name               optionalJSON[string]    `json:"name"`
	ExpiresAt          optionalJSON[time.Time] `json:"expires_at"`
	AllowedModels      optionalJSON[[]string]  `json:"allowed_models"`
	IPAllowlist        optionalJSON[[]string]  `json:"ip_allowlist"`
	RPMLimit           optionalJSON[int64]     `json:"rpm_limit"`
	TPMLimit           optionalJSON[int64]     `json:"tpm_limit"`
	DailyBudgetMinor   optionalJSON[int64]     `json:"daily_budget_minor"`
	MonthlyBudgetMinor optionalJSON[int64]     `json:"monthly_budget_minor"`
}

type keyResponse struct {
	ID                 uuid.UUID  `json:"id"`
	Name               string     `json:"name"`
	Prefix             string     `json:"prefix"`
	Status             string     `json:"status"`
	ExpiresAt          *time.Time `json:"expires_at"`
	AllowedModels      []string   `json:"allowed_models"`
	IPAllowlist        []string   `json:"ip_allowlist"`
	RPMLimit           *int64     `json:"rpm_limit"`
	TPMLimit           *int64     `json:"tpm_limit"`
	DailyBudgetMinor   *int64     `json:"daily_budget_minor"`
	MonthlyBudgetMinor *int64     `json:"monthly_budget_minor"`
	LastUsedAt         *time.Time `json:"last_used_at"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	RotatedAt          *time.Time `json:"rotated_at"`
	SecretVersion      int64      `json:"secret_version"`
}

func viewKey(key Key) keyResponse {
	return keyResponse{
		ID:                 key.ID,
		Name:               key.Name,
		Prefix:             key.Prefix,
		Status:             key.Status,
		ExpiresAt:          key.ExpiresAt,
		AllowedModels:      nonNilStrings(key.AllowedModels),
		IPAllowlist:        nonNilStrings(key.IPAllowlist),
		RPMLimit:           key.RPMLimit,
		TPMLimit:           key.TPMLimit,
		DailyBudgetMinor:   key.DailyBudgetMinor,
		MonthlyBudgetMinor: key.MonthlyBudgetMinor,
		LastUsedAt:         key.LastUsedAt,
		CreatedAt:          key.CreatedAt,
		UpdatedAt:          key.UpdatedAt,
		RotatedAt:          key.RotatedAt,
		SecretVersion:      key.SecretVersion,
	}
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func (h *handler) list(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
	keys, err := h.service.List(r.Context(), userID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	views := make([]keyResponse, 0, len(keys))
	for _, key := range keys {
		views = append(views, viewKey(key))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": views})
}

func (h *handler) create(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
	var payload createRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	created, err := h.service.Create(r.Context(), userID, CreateInput{
		Name:               payload.Name,
		ExpiresAt:          payload.ExpiresAt,
		AllowedModels:      payload.AllowedModels,
		IPAllowlist:        payload.IPAllowlist,
		RPMLimit:           payload.RPMLimit,
		TPMLimit:           payload.TPMLimit,
		DailyBudgetMinor:   payload.DailyBudgetMinor,
		MonthlyBudgetMinor: payload.MonthlyBudgetMinor,
	}, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"api_key": viewKey(created.Key), "secret": created.Secret})
}

func (h *handler) update(w http.ResponseWriter, r *http.Request, userID, keyID uuid.UUID) {
	var payload updateRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	input, err := payload.input()
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	key, err := h.service.Update(r.Context(), userID, keyID, input, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_key": viewKey(key)})
}

func (payload updateRequest) input() (UpdateInput, error) {
	var input UpdateInput
	setFields := 0
	if payload.Name.Set {
		setFields++
		if payload.Name.Null {
			return UpdateInput{}, ErrInvalidInput
		}
		input.Name = &payload.Name.Value
	}
	if payload.ExpiresAt.Set {
		setFields++
		input.ExpiresAt.Set = true
		if !payload.ExpiresAt.Null {
			input.ExpiresAt.Value = &payload.ExpiresAt.Value
		}
	}
	if payload.AllowedModels.Set {
		setFields++
		if payload.AllowedModels.Null {
			return UpdateInput{}, ErrInvalidInput
		}
		input.AllowedModels = &payload.AllowedModels.Value
	}
	if payload.IPAllowlist.Set {
		setFields++
		if payload.IPAllowlist.Null {
			return UpdateInput{}, ErrInvalidInput
		}
		input.IPAllowlist = &payload.IPAllowlist.Value
	}
	input.RPMLimit, setFields = optionalLimit(payload.RPMLimit, setFields)
	input.TPMLimit, setFields = optionalLimit(payload.TPMLimit, setFields)
	input.DailyBudgetMinor, setFields = optionalLimit(payload.DailyBudgetMinor, setFields)
	input.MonthlyBudgetMinor, setFields = optionalLimit(payload.MonthlyBudgetMinor, setFields)
	if setFields == 0 {
		return UpdateInput{}, ErrInvalidInput
	}
	return input, nil
}

func optionalLimit(value optionalJSON[int64], count int) (OptionalInt64, int) {
	if !value.Set {
		return OptionalInt64{}, count
	}
	result := OptionalInt64{Set: true}
	if !value.Null {
		result.Value = &value.Value
	}
	return result, count + 1
}

func (h *handler) rotate(w http.ResponseWriter, r *http.Request, userID, keyID uuid.UUID) {
	created, err := h.service.Rotate(r.Context(), userID, keyID, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_key": viewKey(created.Key), "secret": created.Secret})
}

func (h *handler) revoke(w http.ResponseWriter, r *http.Request, userID, keyID uuid.UUID) {
	key, err := h.service.Revoke(r.Context(), userID, keyID, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_key": viewKey(key)})
}

type principalContextKey struct{}

// PrincipalFromContext returns the authenticated inference identity.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}

// IdentityMiddleware validates Bearer identity and source IP. The downstream
// handler must still call Authorize after parsing model and token usage.
func (s *Service) IdentityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values("Authorization")
		if len(values) != 1 {
			writeAPIKeyError(w, r, ErrInvalidKey, slog.Default())
			return
		}
		parts := strings.Fields(values[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			writeAPIKeyError(w, r, ErrInvalidKey, slog.Default())
			return
		}
		source, err := remoteIP(r.RemoteAddr)
		if err != nil {
			writeAPIKeyError(w, r, ErrIPDenied, slog.Default())
			return
		}
		principal, err := s.Authenticate(r.Context(), parts[1], source)
		if err != nil {
			writeAPIKeyError(w, r, err, slog.Default())
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal)))
	})
}

func remoteIP(remoteAddr string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return netip.ParseAddr(host)
	}
	return netip.ParseAddr(remoteAddr)
}

func (h *handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	writeAPIKeyError(w, r, err, h.logger)
}

func writeAPIKeyError(w http.ResponseWriter, r *http.Request, err error, logger *slog.Logger) {
	status, errorType, code, message := publicAPIKeyError(err)
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "60")
	}
	if status >= http.StatusInternalServerError {
		logger.Error("apikey_request_error", "request_id", httpapi.RequestID(r.Context()), "error", err)
	}
	writeJSON(w, status, map[string]any{"error": map[string]string{
		"type": errorType, "code": code, "message": message, "request_id": httpapi.RequestID(r.Context()),
	}})
}

func publicAPIKeyError(err error) (int, string, string, string) {
	switch {
	case errors.Is(err, errMethodNotAllowed):
		return http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "Method not allowed."
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest, "invalid_request_error", "invalid_request", "The request is invalid."
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "invalid_request_error", "not_found", "The API key was not found."
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, "invalid_request_error", "conflict", "The API key state conflicts with this operation."
	case errors.Is(err, auth.ErrSessionInvalid):
		return http.StatusUnauthorized, "authentication_error", "invalid_session", "Authentication is required."
	case errors.Is(err, ErrInvalidKey):
		return http.StatusUnauthorized, "authentication_error", "invalid_api_key", "The API key is invalid."
	case errors.Is(err, ErrIPDenied):
		return http.StatusForbidden, "permission_error", "ip_not_allowed", "The source IP is not allowed."
	case errors.Is(err, ErrModelDenied):
		return http.StatusForbidden, "permission_error", "model_not_allowed", "The API key cannot access this model."
	case errors.Is(err, ErrRateLimited):
		return http.StatusTooManyRequests, "rate_limit_error", "rate_limited", "The API key rate limit was exceeded."
	case errors.Is(err, ErrRateLimitUnavailable):
		return http.StatusServiceUnavailable, "api_error", "rate_limit_unavailable", "Rate limiting is temporarily unavailable."
	default:
		return http.StatusInternalServerError, "api_error", "internal_error", "The request could not be completed."
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maximumJSONBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
