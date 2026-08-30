package model

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
	userModelsPath  = "/api/v1/models"
	adminModelsPath = "/api/v1/admin/models"
	maximumJSONBody = 64 << 10
)

type handler struct {
	service *Service
	logger  *slog.Logger
}

// NewHandler creates the user and administrator model catalog HTTP surface.
func NewHandler(service *Service, logger *slog.Logger) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("model HTTP handler requires a service")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &handler{service: service, logger: logger}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case r.URL.Path == userModelsPath:
		if r.Method != http.MethodGet {
			h.methodNotAllowed(w, r, "GET")
			return
		}
		h.list(w, r, true)
	case r.URL.Path == adminModelsPath:
		switch r.Method {
		case http.MethodGet:
			h.list(w, r, false)
		case http.MethodPost:
			h.create(w, r)
		default:
			h.methodNotAllowed(w, r, "GET, POST")
		}
	case strings.HasPrefix(r.URL.Path, adminModelsPath+"/"):
		modelID, ok := parseResourceID(r.URL.Path, adminModelsPath)
		if !ok {
			h.writeError(w, r, ErrNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			h.get(w, r, modelID)
		case http.MethodPatch:
			h.update(w, r, modelID)
		default:
			h.methodNotAllowed(w, r, "GET, PATCH")
		}
	default:
		h.writeError(w, r, ErrNotFound)
	}
}

type createRequest struct {
	PublicID     string       `json:"public_model_id"`
	Aliases      []string     `json:"aliases"`
	DisplayName  string       `json:"display_name"`
	Visibility   string       `json:"visibility"`
	BillingClass string       `json:"billing_class"`
	Capabilities Capabilities `json:"capabilities"`
	Enabled      *bool        `json:"enabled"`
}

type updateRequest struct {
	PublicID     *string       `json:"public_model_id"`
	Aliases      *[]string     `json:"aliases"`
	DisplayName  *string       `json:"display_name"`
	Visibility   *string       `json:"visibility"`
	BillingClass *string       `json:"billing_class"`
	Capabilities *Capabilities `json:"capabilities"`
	Enabled      *bool         `json:"enabled"`
}

func (h *handler) list(w http.ResponseWriter, r *http.Request, publicOnly bool) {
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
	var page Page
	if publicOnly {
		page, err = h.service.ListPublic(r.Context(), cursor, limit)
	} else {
		page, err = h.service.ListAdmin(r.Context(), cursor, limit)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":        page.Models,
		"next_cursor": encodeCursor(page.NextCursor),
	})
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
	if payload.Visibility == "" {
		payload.Visibility = VisibilityPublic
	}
	if payload.BillingClass == "" {
		payload.BillingClass = BillingToken
	}
	created, err := h.service.Create(r.Context(), session.UserID, CreateInput{
		PublicID:     payload.PublicID,
		Aliases:      payload.Aliases,
		DisplayName:  payload.DisplayName,
		Visibility:   payload.Visibility,
		BillingClass: payload.BillingClass,
		Capabilities: payload.Capabilities,
		Enabled:      enabled,
	}, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"model": created})
}

func (h *handler) get(w http.ResponseWriter, r *http.Request, modelID uuid.UUID) {
	value, err := h.service.Get(r.Context(), modelID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": value})
}

func (h *handler) update(w http.ResponseWriter, r *http.Request, modelID uuid.UUID) {
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
	updated, err := h.service.Update(r.Context(), session.UserID, modelID, UpdateInput{
		PublicID:     payload.PublicID,
		Aliases:      payload.Aliases,
		DisplayName:  payload.DisplayName,
		Visibility:   payload.Visibility,
		BillingClass: payload.BillingClass,
		Capabilities: payload.Capabilities,
		Enabled:      payload.Enabled,
	}, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": updated})
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
	limit, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
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
		h.logger.Error("model_request_error", "request_id", httpapi.RequestID(r.Context()), "error", err)
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
		return http.StatusBadRequest, "invalid_request", "invalid_request", "模型目录参数不符合要求。"
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "not_found", "model_not_found", "模型不存在。"
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, "invalid_request", "model_conflict", "模型标识或别名已存在。"
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
