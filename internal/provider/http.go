package provider

import (
	"bytes"
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
	"github.com/starhui-dev/bablo/internal/model"
)

const (
	adminProvidersPath      = "/api/v1/admin/providers"
	adminProviderModelsPath = "/api/v1/admin/provider-models"
	maximumJSONBody         = 1 << 20
)

type handler struct {
	service *Service
	logger  *slog.Logger
}

// NewHandler creates the administrator provider catalog HTTP surface.
func NewHandler(service *Service, logger *slog.Logger) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("provider HTTP handler requires a service")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &handler{service: service, logger: logger}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case r.URL.Path == adminProvidersPath:
		switch r.Method {
		case http.MethodGet:
			h.listProviders(w, r)
		case http.MethodPost:
			h.createProvider(w, r)
		default:
			h.methodNotAllowed(w, r, "GET, POST")
		}
	case strings.HasPrefix(r.URL.Path, adminProvidersPath+"/"):
		h.serveProviderResource(w, r)
	case r.URL.Path == adminProviderModelsPath:
		switch r.Method {
		case http.MethodGet:
			h.listProviderModels(w, r)
		case http.MethodPost:
			h.createProviderModel(w, r)
		default:
			h.methodNotAllowed(w, r, "GET, POST")
		}
	case strings.HasPrefix(r.URL.Path, adminProviderModelsPath+"/"):
		providerModelID, ok := parseResourceID(r.URL.Path, adminProviderModelsPath)
		if !ok {
			h.writeError(w, r, ErrNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			h.getProviderModel(w, r, providerModelID)
		case http.MethodPatch:
			h.updateProviderModel(w, r, providerModelID)
		default:
			h.methodNotAllowed(w, r, "GET, PATCH")
		}
	default:
		h.writeError(w, r, ErrNotFound)
	}
}

type createProviderRequest struct {
	Slug              string `json:"slug"`
	DisplayName       string `json:"display_name"`
	ResourceType      string `json:"resource_type"`
	CommercialAllowed bool   `json:"commercial_allowed"`
	Enabled           *bool  `json:"enabled"`
}

type updateProviderRequest struct {
	Slug              *string `json:"slug"`
	DisplayName       *string `json:"display_name"`
	ResourceType      *string `json:"resource_type"`
	CommercialAllowed *bool   `json:"commercial_allowed"`
	Enabled           *bool   `json:"enabled"`
}

type createProviderModelRequest struct {
	ProviderID      uuid.UUID          `json:"provider_id"`
	ModelID         uuid.UUID          `json:"model_id"`
	UpstreamModelID string             `json:"upstream_model_id"`
	Protocol        string             `json:"protocol"`
	Capabilities    model.Capabilities `json:"capabilities"`
	Enabled         *bool              `json:"enabled"`
}

type optionalUUIDRequest struct {
	Set   bool
	Value *uuid.UUID
}

func (value *optionalUUIDRequest) UnmarshalJSON(data []byte) error {
	value.Set = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		value.Value = nil
		return nil
	}
	var parsed uuid.UUID
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	value.Value = &parsed
	return nil
}

type updateProviderModelRequest struct {
	ModelID      optionalUUIDRequest `json:"model_id"`
	Protocol     *string             `json:"protocol"`
	Capabilities *model.Capabilities `json:"capabilities"`
	Enabled      *bool               `json:"enabled"`
	ReviewStatus *string             `json:"review_status"`
}

type reconcileRequest struct {
	Models []Discovery `json:"models"`
}

func (h *handler) listProviders(w http.ResponseWriter, r *http.Request) {
	cursor, err := decodeCursor(r.URL.Query().Get("cursor"), 63)
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	page, err := h.service.List(r.Context(), cursor, limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": page.Providers, "next_cursor": encodeCursor(page.NextCursor)})
}

func (h *handler) createProvider(w http.ResponseWriter, r *http.Request) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, auth.ErrSessionInvalid)
		return
	}
	var payload createProviderRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	enabled := true
	if payload.Enabled != nil {
		enabled = *payload.Enabled
	}
	created, err := h.service.Create(r.Context(), session.UserID, CreateInput{
		Slug:              payload.Slug,
		DisplayName:       payload.DisplayName,
		ResourceType:      payload.ResourceType,
		CommercialAllowed: payload.CommercialAllowed,
		Enabled:           enabled,
	}, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"provider": created})
}

func (h *handler) serveProviderResource(w http.ResponseWriter, r *http.Request) {
	value := strings.Trim(strings.TrimPrefix(r.URL.Path, adminProvidersPath), "/")
	parts := strings.Split(value, "/")
	if len(parts) < 1 || len(parts) > 2 {
		h.writeError(w, r, ErrNotFound)
		return
	}
	providerID, err := uuid.Parse(parts[0])
	if err != nil {
		h.writeError(w, r, ErrNotFound)
		return
	}
	if len(parts) == 2 {
		if parts[1] != "reconcile" || r.Method != http.MethodPost {
			h.writeError(w, r, ErrNotFound)
			return
		}
		h.reconcile(w, r, providerID)
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.getProvider(w, r, providerID)
	case http.MethodPatch:
		h.updateProvider(w, r, providerID)
	default:
		h.methodNotAllowed(w, r, "GET, PATCH")
	}
}

func (h *handler) getProvider(w http.ResponseWriter, r *http.Request, providerID uuid.UUID) {
	value, err := h.service.Get(r.Context(), providerID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": value})
}

func (h *handler) updateProvider(w http.ResponseWriter, r *http.Request, providerID uuid.UUID) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, auth.ErrSessionInvalid)
		return
	}
	var payload updateProviderRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	updated, err := h.service.Update(r.Context(), session.UserID, providerID, UpdateInput{
		Slug:              payload.Slug,
		DisplayName:       payload.DisplayName,
		ResourceType:      payload.ResourceType,
		CommercialAllowed: payload.CommercialAllowed,
		Enabled:           payload.Enabled,
	}, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": updated})
}

func (h *handler) listProviderModels(w http.ResponseWriter, r *http.Request) {
	providerID, err := uuid.Parse(r.URL.Query().Get("provider_id"))
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	cursor, err := decodeCursor(r.URL.Query().Get("cursor"), 255)
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	page, err := h.service.ListModels(r.Context(), providerID, cursor, limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": page.Models, "next_cursor": encodeCursor(page.NextCursor)})
}

func (h *handler) createProviderModel(w http.ResponseWriter, r *http.Request) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, auth.ErrSessionInvalid)
		return
	}
	var payload createProviderModelRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	enabled := true
	if payload.Enabled != nil {
		enabled = *payload.Enabled
	}
	created, err := h.service.CreateModel(r.Context(), session.UserID, CreateModelInput{
		ProviderID:      payload.ProviderID,
		ModelID:         payload.ModelID,
		UpstreamModelID: payload.UpstreamModelID,
		Protocol:        payload.Protocol,
		Capabilities:    payload.Capabilities,
		Enabled:         enabled,
	}, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"provider_model": created})
}

func (h *handler) getProviderModel(w http.ResponseWriter, r *http.Request, providerModelID uuid.UUID) {
	value, err := h.service.GetModel(r.Context(), providerModelID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider_model": value})
}

func (h *handler) updateProviderModel(w http.ResponseWriter, r *http.Request, providerModelID uuid.UUID) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, auth.ErrSessionInvalid)
		return
	}
	var payload updateProviderModelRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	updated, err := h.service.UpdateModel(r.Context(), session.UserID, providerModelID, UpdateModelInput{
		ModelID:      OptionalUUID{Set: payload.ModelID.Set, Value: payload.ModelID.Value},
		Protocol:     payload.Protocol,
		Capabilities: payload.Capabilities,
		Enabled:      payload.Enabled,
		ReviewStatus: payload.ReviewStatus,
	}, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider_model": updated})
}

func (h *handler) reconcile(w http.ResponseWriter, r *http.Request, providerID uuid.UUID) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, auth.ErrSessionInvalid)
		return
	}
	var payload reconcileRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	result, err := h.service.Reconcile(r.Context(), session.UserID, providerID, payload.Models, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

func parseResourceID(path, prefix string) (uuid.UUID, bool) {
	value := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	if value == "" || strings.Contains(value, "/") {
		return uuid.Nil, false
	}
	parsed, err := uuid.Parse(value)
	return parsed, err == nil
}

func parseLimit(value string) (int, error) {
	if value == "" {
		return 50, nil
	}
	return strconv.Atoi(value)
}

func encodeCursor(value string) string {
	if value == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeCursor(value string, maximum int) (string, error) {
	if value == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) > maximum {
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
		h.logger.Error("provider_request_error", "request_id", httpapi.RequestID(r.Context()), "error", err)
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
		return http.StatusBadRequest, "invalid_request", "invalid_request", "Provider 目录参数不符合要求。"
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "not_found", "provider_not_found", "Provider 或上游模型不存在。"
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, "invalid_request", "provider_conflict", "Provider 标识或上游模型已存在。"
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
