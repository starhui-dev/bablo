package route

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
	adminRoutesPath = "/api/v1/admin/routes"
	maximumJSONBody = 2 << 20
)

type handler struct {
	service *Service
	logger  *slog.Logger
}

// NewHandler creates the administrator-only Route management and preview API.
func NewHandler(service *Service, logger *slog.Logger) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("route HTTP handler requires a service")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &handler{service: service, logger: logger}, nil
}

type createRequest struct {
	ModelID    uuid.UUID         `json:"model_id"`
	MatchType  string            `json:"match_type"`
	MatchValue string            `json:"match_value"`
	Metadata   map[string]string `json:"metadata"`
	Enabled    *bool             `json:"enabled"`
	Targets    []targetRequest   `json:"targets"`
}

type updateRequest struct {
	Metadata map[string]string `json:"metadata"`
	Enabled  *bool             `json:"enabled"`
}

type publishRequest struct {
	Targets []targetRequest `json:"targets"`
}

type targetRequest struct {
	ProviderModelID  uuid.UUID         `json:"provider_model_id"`
	CredentialPoolID uuid.UUID         `json:"credential_pool_id"`
	Priority         int               `json:"priority"`
	Weight           int               `json:"weight"`
	CommercialPolicy string            `json:"commercial_policy"`
	Enabled          *bool             `json:"enabled"`
	Metadata         map[string]string `json:"metadata"`
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case r.URL.Path == adminRoutesPath:
		switch r.Method {
		case http.MethodGet:
			h.list(w, r)
		case http.MethodPost:
			h.create(w, r)
		default:
			h.methodNotAllowed(w, r, "GET, POST")
		}
	case r.URL.Path == adminRoutesPath+"/preview":
		if r.Method != http.MethodGet {
			h.methodNotAllowed(w, r, "GET")
			return
		}
		h.preview(w, r)
	case strings.HasPrefix(r.URL.Path, adminRoutesPath+"/"):
		h.resource(w, r)
	default:
		h.writeError(w, r, ErrNotFound)
	}
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	var modelID *uuid.UUID
	if value := strings.TrimSpace(r.URL.Query().Get("model_id")); value != "" {
		parsed, err := uuid.Parse(value)
		if err != nil {
			h.writeError(w, r, ErrInvalidInput)
			return
		}
		modelID = &parsed
	}
	cursor, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	page, err := h.service.List(r.Context(), modelID, cursor, limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": page.Routes, "next_cursor": encodeCursor(page.NextCursor)})
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
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
		ModelID:    payload.ModelID,
		MatchType:  payload.MatchType,
		MatchValue: payload.MatchValue,
		Metadata:   payload.Metadata,
		Enabled:    enabled,
		Targets:    targetInputs(payload.Targets),
	}, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"route": created})
}

func (h *handler) resource(w http.ResponseWriter, r *http.Request) {
	value := strings.Trim(strings.TrimPrefix(r.URL.Path, adminRoutesPath), "/")
	parts := strings.Split(value, "/")
	if len(parts) < 1 || len(parts) > 2 {
		h.writeError(w, r, ErrNotFound)
		return
	}
	routeID, err := uuid.Parse(parts[0])
	if err != nil {
		h.writeError(w, r, ErrNotFound)
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			h.get(w, r, routeID)
		case http.MethodPatch:
			h.update(w, r, routeID)
		default:
			h.methodNotAllowed(w, r, "GET, PATCH")
		}
		return
	}
	if parts[1] != "versions" {
		h.writeError(w, r, ErrNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.listVersions(w, r, routeID)
	case http.MethodPost:
		h.publish(w, r, routeID)
	default:
		h.methodNotAllowed(w, r, "GET, POST")
	}
}

func (h *handler) get(w http.ResponseWriter, r *http.Request, routeID uuid.UUID) {
	value, err := h.service.Get(r.Context(), routeID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"route": value})
}

func (h *handler) update(w http.ResponseWriter, r *http.Request, routeID uuid.UUID) {
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
	var metadata *map[string]string
	if payload.Metadata != nil {
		metadata = &payload.Metadata
	}
	updated, err := h.service.Update(r.Context(), session.UserID, routeID, UpdateInput{Metadata: metadata, Enabled: payload.Enabled}, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"route": updated})
}

func (h *handler) publish(w http.ResponseWriter, r *http.Request, routeID uuid.UUID) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, auth.ErrSessionInvalid)
		return
	}
	var payload publishRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	published, err := h.service.PublishVersion(r.Context(), session.UserID, routeID, PublishInput{Targets: targetInputs(payload.Targets)}, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"route": published})
}

func (h *handler) listVersions(w http.ResponseWriter, r *http.Request, routeID uuid.UUID) {
	versions, err := h.service.ListVersions(r.Context(), routeID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": versions})
}

func (h *handler) preview(w http.ResponseWriter, r *http.Request) {
	identifier := r.URL.Query().Get("model")
	if strings.TrimSpace(identifier) == "" {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	resolution, err := h.service.Resolve(r.Context(), identifier)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resolution": resolution})
}

func targetInputs(values []targetRequest) []TargetInput {
	result := make([]TargetInput, len(values))
	for index, value := range values {
		enabled := true
		if value.Enabled != nil {
			enabled = *value.Enabled
		}
		result[index] = TargetInput{
			ProviderModelID:  value.ProviderModelID,
			CredentialPoolID: value.CredentialPoolID,
			Priority:         value.Priority,
			Weight:           value.Weight,
			CommercialPolicy: value.CommercialPolicy,
			Enabled:          enabled,
			Metadata:         value.Metadata,
		}
	}
	return result
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
	if err != nil || len(decoded) > 128 {
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
		h.logger.Error("route_request_error", "request_id", httpapi.RequestID(r.Context()), "error", err)
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
		return http.StatusBadRequest, "invalid_request", "invalid_request", "Route 参数不符合要求。"
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNoRoute):
		return http.StatusNotFound, "not_found", "route_not_found", "Route 不存在或尚未配置。"
	case errors.Is(err, ErrRouteDisabled):
		return http.StatusConflict, "route_error", "route_disabled", "Route 当前已禁用。"
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, "invalid_request", "route_conflict", "Route 版本或目标配置冲突。"
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
