package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/httpapi"
)

const (
	sessionCookieName = "bablo_session"
	csrfCookieName    = "bablo_csrf"
	maximumJSONBytes  = 32 << 10
)

// HandlerConfig controls browser-facing authentication transport behavior.
type HandlerConfig struct {
	AllowedOrigin string
	CookieDomain  string
	CookieSecure  bool
	SessionTTL    time.Duration
}

// Handler exposes the Bablo Web Session authentication API.
type Handler struct {
	service *Service
	logger  *slog.Logger
	config  HandlerConfig
	mux     *http.ServeMux
}

// NewHandler constructs the authentication HTTP handler.
func NewHandler(service *Service, logger *slog.Logger, cfg HandlerConfig) (*Handler, error) {
	if service == nil {
		return nil, errors.New("auth HTTP handler requires a service")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.SessionTTL <= 0 {
		return nil, errors.New("auth HTTP handler requires a positive session TTL")
	}
	cfg.AllowedOrigin = strings.TrimSuffix(strings.TrimSpace(cfg.AllowedOrigin), "/")

	handler := &Handler{service: service, logger: logger, config: cfg, mux: http.NewServeMux()}
	handler.mux.HandleFunc("POST /api/v1/auth/login", handler.login)
	handler.mux.HandleFunc("GET /api/v1/auth/session", handler.currentSession)
	handler.mux.HandleFunc("POST /api/v1/auth/logout", handler.logout)
	handler.mux.HandleFunc("POST /api/v1/auth/logout-all", handler.logoutAll)
	handler.mux.HandleFunc("POST /api/v1/auth/password", handler.changePassword)
	handler.mux.HandleFunc("POST /api/v1/auth/mfa/verify", handler.verifyMFA)
	handler.mux.HandleFunc("POST /api/v1/auth/mfa/totp/bind", handler.beginTOTP)
	handler.mux.HandleFunc("POST /api/v1/auth/mfa/totp/confirm", handler.confirmTOTP)
	handler.mux.HandleFunc("POST /api/v1/auth/mfa/recovery/regenerate", handler.regenerateRecoveryCodes)
	handler.mux.HandleFunc("POST /api/v1/admin/users/{user_id}/password", handler.adminResetPassword)
	return handler, nil
}

type sessionContextKey struct{}

// SessionFromContext returns the full Web Session established by Protect.
func SessionFromContext(ctx context.Context) (Session, bool) {
	session, ok := ctx.Value(sessionContextKey{}).(Session)
	return session, ok
}

// Protect authenticates a full Web Session around a user control-plane
// handler. Unsafe methods also require the existing Origin and CSRF checks.
func (h *Handler) Protect(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var (
			session Session
			err     error
		)
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			session, err = h.authenticate(r)
		default:
			session, err = h.authenticatedMutation(r)
		}
		if err == nil {
			err = h.service.RequireFullSession(session)
		}
		if err != nil {
			h.writeError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, session)))
	})
}

// ServeHTTP dispatches authentication routes with the same JSON error envelope
// used by the rest of the control plane.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method, ok := authRouteMethod(r.URL.Path)
	if !ok {
		h.writeTransportError(w, r, http.StatusNotFound, "not_found", "接口不存在。")
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		h.writeTransportError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "请求方法不受支持。")
		return
	}
	h.mux.ServeHTTP(w, r)
}

func authRouteMethod(path string) (string, bool) {
	switch path {
	case "/api/v1/auth/session":
		return http.MethodGet, true
	case "/api/v1/auth/login",
		"/api/v1/auth/logout",
		"/api/v1/auth/logout-all",
		"/api/v1/auth/password",
		"/api/v1/auth/mfa/verify",
		"/api/v1/auth/mfa/totp/bind",
		"/api/v1/auth/mfa/totp/confirm",
		"/api/v1/auth/mfa/recovery/regenerate":
		return http.MethodPost, true
	}
	const prefix = "/api/v1/admin/users/"
	const suffix = "/password"
	if strings.HasPrefix(path, prefix) && strings.HasSuffix(path, suffix) {
		userID := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
		if userID != "" && !strings.Contains(userID, "/") {
			return http.MethodPost, true
		}
	}
	return "", false
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type codeRequest struct {
	Code string `json:"code"`
}

type passwordChangeRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

type passwordResetRequest struct {
	NewPassword string `json:"new_password"`
}

type sessionResponse struct {
	UserID      string   `json:"user_id"`
	Email       string   `json:"email"`
	Roles       []string `json:"roles"`
	ExpiresAt   string   `json:"expires_at"`
	MFAEnabled  bool     `json:"mfa_enabled"`
	MFARequired bool     `json:"mfa_required"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if err := h.validateOrigin(r); err != nil {
		h.writeError(w, r, err)
		return
	}
	var payload loginRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	previousToken := ""
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		previousToken = cookie.Value
	}
	bundle, err := h.service.Login(r.Context(), payload.Email, payload.Password, previousToken, requestMetadata(r))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.setSessionCookies(w, bundle)
	writeAuthJSON(w, http.StatusOK, map[string]any{"session": sessionView(bundle.Session)})
}

func (h *Handler) currentSession(w http.ResponseWriter, r *http.Request) {
	session, err := h.authenticate(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeAuthJSON(w, http.StatusOK, map[string]any{"session": sessionView(session)})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	session, err := h.authenticatedMutation(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if err := h.service.Logout(r.Context(), session, httpapi.RequestID(r.Context())); err != nil {
		h.writeError(w, r, err)
		return
	}
	h.clearSessionCookies(w)
	writeAuthJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) logoutAll(w http.ResponseWriter, r *http.Request) {
	session, err := h.authenticatedMutation(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if err := h.service.LogoutAll(r.Context(), session, httpapi.RequestID(r.Context())); err != nil {
		h.writeError(w, r, err)
		return
	}
	h.clearSessionCookies(w)
	writeAuthJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	session, err := h.authenticatedMutation(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var payload passwordChangeRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	if err := h.service.ChangePassword(r.Context(), session, payload.CurrentPassword, payload.NewPassword, httpapi.RequestID(r.Context())); err != nil {
		h.writeError(w, r, err)
		return
	}
	h.clearSessionCookies(w)
	writeAuthJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) verifyMFA(w http.ResponseWriter, r *http.Request) {
	session, err := h.authenticatedMutation(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var payload codeRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	bundle, err := h.service.VerifyMFA(r.Context(), session, payload.Code, requestMetadata(r))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.setSessionCookies(w, bundle)
	writeAuthJSON(w, http.StatusOK, map[string]any{"session": sessionView(bundle.Session)})
}

func (h *Handler) beginTOTP(w http.ResponseWriter, r *http.Request) {
	session, err := h.authenticatedMutation(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	binding, err := h.service.BeginTOTP(r.Context(), session, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeAuthJSON(w, http.StatusOK, map[string]string{
		"secret":           binding.Secret,
		"provisioning_url": binding.ProvisionURL,
	})
}

func (h *Handler) confirmTOTP(w http.ResponseWriter, r *http.Request) {
	session, err := h.authenticatedMutation(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var payload codeRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	bundle, recoveryCodes, err := h.service.ConfirmTOTP(r.Context(), session, payload.Code, requestMetadata(r))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.setSessionCookies(w, bundle)
	writeAuthJSON(w, http.StatusOK, map[string]any{
		"session":        sessionView(bundle.Session),
		"recovery_codes": recoveryCodes,
	})
}

func (h *Handler) regenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	session, err := h.authenticatedMutation(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	codes, err := h.service.RegenerateRecoveryCodes(r.Context(), session, httpapi.RequestID(r.Context()))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeAuthJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

func (h *Handler) adminResetPassword(w http.ResponseWriter, r *http.Request) {
	session, err := h.authenticatedMutation(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	targetID, err := uuid.Parse(r.PathValue("user_id"))
	if err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	var payload passwordResetRequest
	if err := decodeJSON(w, r, &payload); err != nil {
		h.writeError(w, r, ErrInvalidInput)
		return
	}
	if err := h.service.AdminResetPassword(r.Context(), session, targetID, payload.NewPassword, httpapi.RequestID(r.Context())); err != nil {
		h.writeError(w, r, err)
		return
	}
	writeAuthJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) authenticate(r *http.Request) (Session, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return Session{}, ErrSessionInvalid
	}
	return h.service.Authenticate(r.Context(), cookie.Value)
}

func (h *Handler) authenticatedMutation(r *http.Request) (Session, error) {
	if err := h.validateOrigin(r); err != nil {
		return Session{}, err
	}
	session, err := h.authenticate(r)
	if err != nil {
		return Session{}, err
	}
	csrfCookie, err := r.Cookie(csrfCookieName)
	if err != nil {
		return Session{}, ErrCSRFInvalid
	}
	headerToken := r.Header.Get("X-CSRF-Token")
	if len(headerToken) != len(csrfCookie.Value) || subtle.ConstantTimeCompare([]byte(headerToken), []byte(csrfCookie.Value)) != 1 {
		return Session{}, ErrCSRFInvalid
	}
	if err := h.service.ValidateCSRF(session, headerToken); err != nil {
		return Session{}, err
	}
	return session, nil
}

func (h *Handler) validateOrigin(r *http.Request) error {
	origin := strings.TrimSuffix(strings.TrimSpace(r.Header.Get("Origin")), "/")
	if h.config.AllowedOrigin != "" {
		if origin == "" || origin != h.config.AllowedOrigin {
			return ErrCSRFInvalid
		}
		return nil
	}
	if origin == "" {
		return nil
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host != r.Host {
		return ErrCSRFInvalid
	}
	if (r.TLS == nil && parsed.Scheme != "http") || (r.TLS != nil && parsed.Scheme != "https") {
		return ErrCSRFInvalid
	}
	return nil
}

func (h *Handler) setSessionCookies(w http.ResponseWriter, bundle SessionBundle) {
	maxAge := int(h.config.SessionTTL.Seconds())
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    bundle.SessionToken,
		Path:     "/api/v1",
		Domain:   h.config.CookieDomain,
		MaxAge:   maxAge,
		Expires:  bundle.Session.ExpiresAt,
		HttpOnly: true,
		Secure:   h.config.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    bundle.CSRFToken,
		Path:     "/",
		Domain:   h.config.CookieDomain,
		MaxAge:   maxAge,
		Expires:  bundle.Session.ExpiresAt,
		HttpOnly: false,
		Secure:   h.config.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) clearSessionCookies(w http.ResponseWriter) {
	for _, cookie := range []struct {
		name     string
		path     string
		httpOnly bool
	}{
		{name: sessionCookieName, path: "/api/v1", httpOnly: true},
		{name: csrfCookieName, path: "/"},
	} {
		http.SetCookie(w, &http.Cookie{
			Name:     cookie.name,
			Value:    "",
			Path:     cookie.path,
			Domain:   h.config.CookieDomain,
			MaxAge:   -1,
			Expires:  time.Unix(1, 0),
			HttpOnly: cookie.httpOnly,
			Secure:   h.config.CookieSecure,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := publicAuthError(err)
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.Itoa(5*60))
	}
	if status >= http.StatusInternalServerError {
		h.logger.Error("auth_request_error", "request_id", httpapi.RequestID(r.Context()), "error", err)
	}
	writeAuthJSON(w, status, map[string]any{
		"error": map[string]string{
			"type":       "authentication_error",
			"code":       code,
			"message":    message,
			"request_id": httpapi.RequestID(r.Context()),
		},
	})
}
func (h *Handler) writeTransportError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeAuthJSON(w, status, map[string]any{
		"error": map[string]string{
			"type":       "invalid_request",
			"code":       code,
			"message":    message,
			"request_id": httpapi.RequestID(r.Context()),
		},
	})
}

func publicAuthError(err error) (int, string, string) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest, "invalid_request", "请求参数不符合要求。"
	case errors.Is(err, ErrInvalidCredentials):
		return http.StatusUnauthorized, "invalid_credentials", "邮箱或密码不正确。"
	case errors.Is(err, ErrSessionInvalid):
		return http.StatusUnauthorized, "invalid_session", "登录状态无效或已过期。"
	case errors.Is(err, ErrCSRFInvalid):
		return http.StatusForbidden, "csrf_failed", "请求来源或 CSRF 校验失败。"
	case errors.Is(err, ErrMFARequired):
		return http.StatusForbidden, "mfa_required", "需要完成多因素认证。"
	case errors.Is(err, ErrMFAInvalid):
		return http.StatusUnauthorized, "invalid_mfa_code", "多因素认证码无效。"
	case errors.Is(err, ErrMFAUnavailable):
		return http.StatusConflict, "mfa_unavailable", "多因素认证尚未配置。"
	case errors.Is(err, ErrPermissionDenied):
		return http.StatusForbidden, "permission_denied", "没有执行此操作的权限。"
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, "conflict", "当前状态不允许此操作。"
	case errors.Is(err, ErrRateLimited):
		return http.StatusTooManyRequests, "rate_limited", "尝试次数过多，请稍后重试。"
	case errors.Is(err, ErrUserNotFound):
		return http.StatusNotFound, "not_found", "用户不存在。"
	default:
		return http.StatusInternalServerError, "internal_error", "服务暂时无法处理请求。"
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maximumJSONBytes)
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

func requestMetadata(r *http.Request) LoginMetadata {
	remoteAddr := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		remoteAddr = host
	}
	if net.ParseIP(remoteAddr) == nil {
		remoteAddr = ""
	}
	userAgent := r.UserAgent()
	if len(userAgent) > 512 {
		userAgent = userAgent[:512]
	}
	return LoginMetadata{
		UserAgent:  userAgent,
		RemoteAddr: remoteAddr,
		RequestID:  httpapi.RequestID(r.Context()),
	}
}

func sessionView(session Session) sessionResponse {
	return sessionResponse{
		UserID:      session.UserID.String(),
		Email:       session.Email,
		Roles:       append([]string(nil), session.Roles...),
		ExpiresAt:   session.ExpiresAt.UTC().Format(time.RFC3339),
		MFAEnabled:  session.MFAEnabled,
		MFARequired: session.MFARequired(),
	}
}

func writeAuthJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
