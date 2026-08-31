package credential

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/auth"
	"github.com/starhui-dev/bablo/internal/httpapi"
)

const (
	adminCredentialsPath = "/api/v1/admin/credentials"
	adminPoolsPath       = "/api/v1/admin/credential-pools"
	maximumJSONBody      = 2 << 20
)

type handler struct {
	service *Service
	logger  *slog.Logger
}

// NewHandler creates the administrator-only Credential and pool HTTP surface.
// Secret values are accepted for create/rotate but never returned.
func NewHandler(service *Service, logger *slog.Logger) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("credential HTTP handler requires a service")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &handler{service: service, logger: logger}, nil
}

type createRequest struct {
	ProviderID       uuid.UUID         `json:"provider_id"`
	ExternalStableID string            `json:"external_stable_id"`
	SourceKind       string            `json:"source_kind"`
	Region           string            `json:"region"`
	MaxConcurrency   int               `json:"max_concurrency"`
	ProxyRef         string            `json:"proxy_ref"`
	Metadata         map[string]string `json:"metadata"`
	Secrets          []SecretInput     `json:"secrets"`
	Enabled          *bool             `json:"enabled"`
}

type updateRequest struct {
	Region         *string            `json:"region"`
	ProxyRef       *string            `json:"proxy_ref"`
	Metadata       *map[string]string `json:"metadata"`
	Status         *string            `json:"status"`
	MaxConcurrency *int               `json:"max_concurrency"`
}

type rotateRequest struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type poolRequest struct {
	ProviderID uuid.UUID         `json:"provider_id"`
	Name       string            `json:"name"`
	Metadata   map[string]string `json:"metadata"`
	Enabled    *bool             `json:"enabled"`
}

type memberRequest struct {
	CredentialID uuid.UUID `json:"credential_id"`
	Priority     int       `json:"priority"`
	Weight       int       `json:"weight"`
	Enabled      *bool     `json:"enabled"`
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case r.URL.Path == adminCredentialsPath:
		switch r.Method {
		case http.MethodGet:
			h.listCredentials(w, r)
		case http.MethodPost:
			h.createCredential(w, r)
		default:
			h.methodNotAllowed(w, r, "GET, POST")
		}
	case strings.HasPrefix(r.URL.Path, adminCredentialsPath+"/"):
		h.serveCredentialResource(w, r)
	case r.URL.Path == adminPoolsPath:
		switch r.Method {
		case http.MethodGet:
			h.listPools(w, r)
		case http.MethodPost:
			h.createPool(w, r)
		default:
			h.methodNotAllowed(w, r, "GET, POST")
		}
	case strings.HasPrefix(r.URL.Path, adminPoolsPath+"/"):
		h.servePoolResource(w, r)
	default:
		h.writeError(w, r, ErrNotFound)
	}
}

func (h *handler) listCredentials(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	cursor, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	page, err := h.service.List(r.Context(), cursor, limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": page.Credentials, "next_cursor": encodeCursor(page.NextCursor)})
}

func (h *handler) createCredential(w http.ResponseWriter, r *http.Request) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, auth.ErrSessionInvalid)
		return
	}
	var payload createRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	enabled := true
	if payload.Enabled != nil {
		enabled = *payload.Enabled
	}
	created, err := h.service.Create(r.Context(), session.UserID, CreateInput{
		ProviderID:       payload.ProviderID,
		ExternalStableID: payload.ExternalStableID,
		SourceKind:       payload.SourceKind,
		Region:           payload.Region,
		MaxConcurrency:   payload.MaxConcurrency,
		ProxyRef:         payload.ProxyRef,
		Metadata:         payload.Metadata,
		Secrets:          payload.Secrets,
		Enabled:          enabled,
	}, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"credential": created})
}

func (h *handler) serveCredentialResource(w http.ResponseWriter, r *http.Request) {
	value := strings.Trim(strings.TrimPrefix(r.URL.Path, adminCredentialsPath), "/")
	parts := strings.Split(value, "/")
	if len(parts) < 1 || len(parts) > 2 {
		h.writeError(w, r, ErrNotFound)
		return
	}
	credentialID, err := uuid.Parse(parts[0])
	if err != nil {
		h.writeError(w, r, ErrNotFound)
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			h.getCredential(w, r, credentialID)
		case http.MethodPatch:
			h.updateCredential(w, r, credentialID)
		default:
			h.methodNotAllowed(w, r, "GET, PATCH")
		}
		return
	}
	switch parts[1] {
	case "rotate":
		if r.Method != http.MethodPost {
			h.methodNotAllowed(w, r, "POST")
			return
		}
		h.rotateCredential(w, r, credentialID)
	case "reencrypt":
		if r.Method != http.MethodPost {
			h.methodNotAllowed(w, r, "POST")
			return
		}
		h.reencryptCredential(w, r, credentialID)
	case "health":
		if r.Method != http.MethodGet {
			h.methodNotAllowed(w, r, "GET")
			return
		}
		h.getHealth(w, r, credentialID)
	default:
		h.writeError(w, r, ErrNotFound)
	}
}

func (h *handler) getCredential(w http.ResponseWriter, r *http.Request, credentialID uuid.UUID) {
	value, err := h.service.Get(r.Context(), credentialID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credential": value})
}

func (h *handler) updateCredential(w http.ResponseWriter, r *http.Request, credentialID uuid.UUID) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, auth.ErrSessionInvalid)
		return
	}
	var payload updateRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	value, err := h.service.Update(r.Context(), session.UserID, credentialID, UpdateInput{
		Region: payload.Region, ProxyRef: payload.ProxyRef, Metadata: payload.Metadata, Status: payload.Status, MaxConcurrency: payload.MaxConcurrency,
	}, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credential": value})
}

func (h *handler) rotateCredential(w http.ResponseWriter, r *http.Request, credentialID uuid.UUID) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, auth.ErrSessionInvalid)
		return
	}
	var payload rotateRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	value, err := h.service.Rotate(r.Context(), session.UserID, credentialID, SecretInput{Kind: payload.Kind, Value: payload.Value}, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credential": value})
}

func (h *handler) reencryptCredential(w http.ResponseWriter, r *http.Request, credentialID uuid.UUID) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, auth.ErrSessionInvalid)
		return
	}
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind == "" || decodeEmptyJSON(w, r) != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	value, err := h.service.Reencrypt(r.Context(), session.UserID, credentialID, kind, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credential": value})
}

func (h *handler) getHealth(w http.ResponseWriter, r *http.Request, credentialID uuid.UUID) {
	value, err := h.service.Get(r.Context(), credentialID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"health": value.Health})
}

func (h *handler) listPools(w http.ResponseWriter, r *http.Request) {
	providerID, err := uuid.Parse(r.URL.Query().Get("provider_id"))
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	cursor, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	page, err := h.service.ListPools(r.Context(), providerID, cursor, limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": page.Pools, "next_cursor": encodeCursor(page.NextCursor)})
}

func (h *handler) createPool(w http.ResponseWriter, r *http.Request) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, auth.ErrSessionInvalid)
		return
	}
	var payload poolRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	enabled := true
	if payload.Enabled != nil {
		enabled = *payload.Enabled
	}
	pool, err := h.service.CreatePool(r.Context(), session.UserID, PoolInput{ProviderID: payload.ProviderID, Name: payload.Name, Metadata: payload.Metadata, Enabled: enabled}, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"pool": pool})
}

func (h *handler) servePoolResource(w http.ResponseWriter, r *http.Request) {
	value := strings.Trim(strings.TrimPrefix(r.URL.Path, adminPoolsPath), "/")
	parts := strings.Split(value, "/")
	if len(parts) < 1 || len(parts) > 3 {
		h.writeError(w, r, ErrNotFound)
		return
	}
	poolID, err := uuid.Parse(parts[0])
	if err != nil {
		h.writeError(w, r, ErrNotFound)
		return
	}
	if len(parts) == 1 {
		h.writeError(w, r, ErrNotFound)
		return
	}
	if parts[1] != "members" {
		h.writeError(w, r, ErrNotFound)
		return
	}
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, auth.ErrSessionInvalid)
		return
	}
	if len(parts) == 2 && r.Method == http.MethodPost {
		var payload memberRequest
		if err := decodeJSON(w, r, &payload); err != nil {
			h.writeError(w, r, ErrInvalidInput)
			return
		}
		enabled := true
		if payload.Enabled != nil {
			enabled = *payload.Enabled
		}
		err := h.service.AddMember(r.Context(), session.UserID, poolID, MembershipInput{CredentialID: payload.CredentialID, Priority: payload.Priority, Weight: payload.Weight, Enabled: enabled}, httpapi.RequestID(r.Context()))
		if err != nil {
			h.writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
		return
	}
	if len(parts) == 3 && r.Method == http.MethodDelete {
		credentialID, err := uuid.Parse(parts[2])
		if err != nil {
			h.writeError(w, r, ErrNotFound)
			return
		}
		if err := h.service.RemoveMember(r.Context(), session.UserID, poolID, credentialID, httpapi.RequestID(r.Context())); err != nil {
			h.writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	h.methodNotAllowed(w, r, "POST, DELETE")
}

func parseLimit(value string) (int, error) {
	if value == "" {
		return 50, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > maximumPageSize {
		return 0, ErrInvalidInput
	}
	return limit, nil
}

func encodeCursor(value string) string {
	if value == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeCursor(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) > 384 {
		return "", ErrInvalidInput
	}
	return string(decoded), nil
}

func (h *handler) methodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	h.writeError(w, r, errMethodNotAllowed)
}

var errMethodNotAllowed = errors.New("method not allowed")

func (h *handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, errorType, code, message := publicError(err)
	if status >= http.StatusInternalServerError {
		h.logger.Error("credential_request_error", "request_id", httpapi.RequestID(r.Context()), "error", err)
	}
	writeJSON(w, status, map[string]any{"error": map[string]string{
		"type": errorType, "code": code, "message": message, "request_id": httpapi.RequestID(r.Context()),
	}})
}

func publicError(err error) (int, string, string, string) {
	switch {
	case errors.Is(err, errMethodNotAllowed):
		return http.StatusMethodNotAllowed, "invalid_request", "method_not_allowed", "请求方法不受支持。"
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest, "invalid_request", "invalid_request", "Credential 参数不符合要求。"
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "not_found", "credential_not_found", "Credential 不存在。"
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, "invalid_request", "credential_conflict", "Credential 状态或版本冲突。"
	case errors.Is(err, ErrSecretUnavailable), errors.Is(err, ErrSecretVersion):
		return http.StatusConflict, "credential_error", "secret_unavailable", "Credential secret 当前不可用。"
	case errors.Is(err, ErrCredentialInactive):
		return http.StatusConflict, "credential_error", "credential_inactive", "Credential 当前未启用。"
	case errors.Is(err, ErrUnsupported):
		return http.StatusNotImplemented, "credential_error", "unsupported", "Credential 操作暂不支持。"
	case errors.Is(err, auth.ErrSessionInvalid):
		return http.StatusUnauthorized, "authentication_error", "invalid_session", "需要登录。"
	default:
		return http.StatusInternalServerError, "internal_error", "internal_error", "服务暂时无法处理请求。"
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

func decodeEmptyJSON(w http.ResponseWriter, r *http.Request) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	var payload map[string]json.RawMessage
	if err := decodeJSON(w, r, &payload); err != nil {
		return err
	}
	if len(payload) != 0 {
		return ErrInvalidInput
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
