package pricing

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/auth"
	"github.com/starhui-dev/bablo/internal/httpapi"
)

const (
	adminPricesPath = "/api/v1/admin/prices"
	maximumJSONBody = 512 << 10
)

type handler struct {
	service *Service
	logger  *slog.Logger
}

// NewHandler creates the administrator price version HTTP surface.
func NewHandler(service *Service, logger *slog.Logger) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("pricing HTTP handler requires a service")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &handler{service: service, logger: logger}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == adminPricesPath {
		switch r.Method {
		case http.MethodGet:
			h.list(w, r)
		case http.MethodPost:
			h.create(w, r)
		default:
			h.methodNotAllowed(w, r, "GET, POST")
		}
		return
	}
	if !strings.HasPrefix(r.URL.Path, adminPricesPath+"/") {
		h.writeError(w, r, ErrNotFound)
		return
	}
	value := strings.Trim(strings.TrimPrefix(r.URL.Path, adminPricesPath), "/")
	parts := strings.Split(value, "/")
	if len(parts) < 1 || len(parts) > 2 {
		h.writeError(w, r, ErrNotFound)
		return
	}
	versionID, err := uuid.Parse(parts[0])
	if err != nil {
		h.writeError(w, r, ErrNotFound)
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			h.methodNotAllowed(w, r, "GET")
			return
		}
		h.get(w, r, versionID)
		return
	}
	if r.Method != http.MethodPost {
		h.methodNotAllowed(w, r, "POST")
		return
	}
	switch parts[1] {
	case "activate":
		h.activate(w, r, versionID)
	case "retire":
		h.retire(w, r, versionID)
	default:
		h.writeError(w, r, ErrNotFound)
	}
}

type retireRequest struct {
	EffectiveTo *time.Time `json:"effective_to"`
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	cursor, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	limit := 50
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil {
			h.writeError(w, r, ErrInvalidInput)
			return
		}
	}
	page, err := h.service.List(r.Context(), r.URL.Query().Get("scope"), cursor, limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": page.Versions, "next_cursor": encodeCursor(page.NextCursor)})
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, auth.ErrSessionInvalid)
		return
	}
	var payload CreateInput
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	created, err := h.service.Create(r.Context(), session.UserID, payload, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"price_version": created})
}

func (h *handler) get(w http.ResponseWriter, r *http.Request, versionID uuid.UUID) {
	value, err := h.service.Get(r.Context(), versionID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"price_version": value})
}

func (h *handler) activate(w http.ResponseWriter, r *http.Request, versionID uuid.UUID) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, auth.ErrSessionInvalid)
		return
	}
	if err := decodeEmptyJSON(w, r); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	value, err := h.service.Activate(r.Context(), session.UserID, versionID, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"price_version": value})
}

func (h *handler) retire(w http.ResponseWriter, r *http.Request, versionID uuid.UUID) {
	session, ok := auth.SessionFromContext(r.Context())
	if !ok {
		h.writeError(w, r, auth.ErrSessionInvalid)
		return
	}
	var payload retireRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	var effectiveTo time.Time
	if payload.EffectiveTo != nil {
		effectiveTo = *payload.EffectiveTo
	}
	value, err := h.service.Retire(r.Context(), session.UserID, versionID, effectiveTo, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"price_version": value})
}

func encodeCursor(value int64) string {
	if value == 0 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(value, 10)))
}

func decodeCursor(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) > 20 {
		return 0, ErrInvalidInput
	}
	return strconv.ParseInt(string(decoded), 10, 64)
}

func (h *handler) methodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	h.writeError(w, r, errMethodNotAllowed)
}

var errMethodNotAllowed = errors.New("method not allowed")

func (h *handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, errorType, code, message := publicError(err)
	if status >= http.StatusInternalServerError {
		h.logger.Error("pricing_request_error", "request_id", httpapi.RequestID(r.Context()), "error", err)
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
		return http.StatusBadRequest, "invalid_request", "invalid_request", "价格参数不符合要求。"
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "not_found", "price_version_not_found", "价格版本不存在。"
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, "invalid_request", "price_version_conflict", "价格版本状态或生效区间冲突。"
	case errors.Is(err, ErrPriceMissing):
		return http.StatusConflict, "billing_error", "price_missing", "可计费模型缺少完整生效价格。"
	case errors.Is(err, ErrBillingDisabled):
		return http.StatusConflict, "billing_error", "billing_disabled", "模型计费已禁用。"
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
