package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp/totp"

	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/migrations"
)

func TestAuthHTTPSessionFixationCSRFPasswordAndLogout(t *testing.T) {
	service, _ := testAuthService(t)
	user, err := service.CreateLocalUser(context.Background(), "member@example.test", "initial password value", false, "test_create")
	if err != nil {
		t.Fatalf("CreateLocalUser() error = %v", err)
	}
	if len(user.Roles) != 1 || user.Roles[0] != "user" {
		t.Fatalf("roles = %v, want user only", user.Roles)
	}

	handler, err := NewHandler(service, slog.New(slog.NewTextHandler(io.Discard, nil)), HandlerConfig{
		AllowedOrigin: "https://console.example",
		CookieSecure:  true,
		SessionTTL:    12 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	originlessBody := strings.NewReader(`{"email":"member@example.test","password":"initial password value"}`)
	originlessRequest := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", originlessBody)
	originlessRecorder := httptest.NewRecorder()
	handler.ServeHTTP(originlessRecorder, originlessRequest)
	if originlessRecorder.Code != http.StatusForbidden {
		t.Fatalf("originless login status = %d, want 403", originlessRecorder.Code)
	}
	wrongMethod := authRequest(t, handler, http.MethodGet, "/api/v1/auth/login", nil, nil, "")
	if wrongMethod.Code != http.StatusMethodNotAllowed || !strings.Contains(wrongMethod.Body.String(), `"code":"method_not_allowed"`) {
		t.Fatalf("wrong method response = %d %s", wrongMethod.Code, wrongMethod.Body.String())
	}

	first := authRequest(t, handler, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "Member@Example.Test", "password": "initial password value",
	}, nil, "")
	if first.Code != http.StatusOK {
		t.Fatalf("first login status = %d, body = %s", first.Code, first.Body.String())
	}
	firstSession, firstCSRF := authCookies(t, first.Result())
	if !firstSession.HttpOnly || !firstSession.Secure || firstSession.SameSite != http.SameSiteLaxMode || firstSession.Path != "/api/v1" {
		t.Fatalf("session cookie security flags = %+v", firstSession)
	}
	if firstCSRF.HttpOnly || !firstCSRF.Secure || firstCSRF.Path != "/" {
		t.Fatalf("csrf cookie security flags = %+v", firstCSRF)
	}

	second := authRequest(t, handler, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "member@example.test", "password": "initial password value",
	}, []*http.Cookie{firstSession}, "")
	if second.Code != http.StatusOK {
		t.Fatalf("second login status = %d, body = %s", second.Code, second.Body.String())
	}
	secondSession, secondCSRF := authCookies(t, second.Result())

	oldSession := authRequest(t, handler, http.MethodGet, "/api/v1/auth/session", nil, []*http.Cookie{firstSession}, "")
	if oldSession.Code != http.StatusUnauthorized {
		t.Fatalf("replaced session status = %d, want 401", oldSession.Code)
	}
	current := authRequest(t, handler, http.MethodGet, "/api/v1/auth/session", nil, []*http.Cookie{secondSession}, "")
	if current.Code != http.StatusOK {
		t.Fatalf("current session status = %d, body = %s", current.Code, current.Body.String())
	}

	missingCSRF := authRequest(t, handler, http.MethodPost, "/api/v1/auth/password", map[string]string{
		"current_password": "initial password value", "new_password": "replacement password value",
	}, []*http.Cookie{secondSession, secondCSRF}, "")
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatalf("password change without CSRF status = %d, want 403", missingCSRF.Code)
	}

	changed := authRequest(t, handler, http.MethodPost, "/api/v1/auth/password", map[string]string{
		"current_password": "initial password value", "new_password": "replacement password value",
	}, []*http.Cookie{secondSession, secondCSRF}, secondCSRF.Value)
	if changed.Code != http.StatusOK {
		t.Fatalf("password change status = %d, body = %s", changed.Code, changed.Body.String())
	}
	if stale := authRequest(t, handler, http.MethodGet, "/api/v1/auth/session", nil, []*http.Cookie{secondSession}, ""); stale.Code != http.StatusUnauthorized {
		t.Fatalf("session after password change status = %d, want 401", stale.Code)
	}

	oldPassword := authRequest(t, handler, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "member@example.test", "password": "initial password value",
	}, nil, "")
	if oldPassword.Code != http.StatusUnauthorized {
		t.Fatalf("old password login status = %d, want 401", oldPassword.Code)
	}
	newPassword := authRequest(t, handler, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"email": "member@example.test", "password": "replacement password value",
	}, nil, "")
	if newPassword.Code != http.StatusOK {
		t.Fatalf("new password login status = %d, body = %s", newPassword.Code, newPassword.Body.String())
	}
	newSession, newCSRF := authCookies(t, newPassword.Result())
	loggedOut := authRequest(t, handler, http.MethodPost, "/api/v1/auth/logout", map[string]string{}, []*http.Cookie{newSession, newCSRF}, newCSRF.Value)
	if loggedOut.Code != http.StatusOK {
		t.Fatalf("logout status = %d, body = %s", loggedOut.Code, loggedOut.Body.String())
	}
	if stale := authRequest(t, handler, http.MethodGet, "/api/v1/auth/session", nil, []*http.Cookie{newSession}, ""); stale.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out session status = %d, want 401", stale.Code)
	}
}

func TestAdminMFARecoveryAndRBAC(t *testing.T) {
	service, now := testAuthService(t)
	ctx := context.Background()
	admin, err := service.CreateLocalUser(ctx, "admin@example.test", "admin password value", true, "test_admin")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	target, err := service.CreateLocalUser(ctx, "target@example.test", "target password value", false, "test_target")
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	normal, err := service.CreateLocalUser(ctx, "normal@example.test", "normal password value", false, "test_normal")
	if err != nil {
		t.Fatalf("create normal: %v", err)
	}

	adminLogin, err := service.Login(ctx, admin.Email, "admin password value", "", LoginMetadata{RemoteAddr: "192.0.2.10", RequestID: "login_admin"})
	if err != nil {
		t.Fatalf("admin Login() error = %v", err)
	}
	if err := service.AdminResetPassword(ctx, adminLogin.Session, target.ID, "blocked reset password", "reset_blocked"); !errors.Is(err, ErrMFARequired) {
		t.Fatalf("admin reset before MFA error = %v, want ErrMFARequired", err)
	}

	binding, err := service.BeginTOTP(ctx, adminLogin.Session, "mfa_begin")
	if err != nil {
		t.Fatalf("BeginTOTP() error = %v", err)
	}
	passcode, err := totp.GenerateCode(binding.Secret, now)
	if err != nil {
		t.Fatalf("totp.GenerateCode() error = %v", err)
	}
	verifiedAdmin, recoveryCodes, err := service.ConfirmTOTP(ctx, adminLogin.Session, passcode, LoginMetadata{RemoteAddr: "192.0.2.10", RequestID: "mfa_confirm"})
	if err != nil {
		t.Fatalf("ConfirmTOTP() error = %v", err)
	}
	if len(recoveryCodes) != recoveryCodeCount || !verifiedAdmin.Session.MFAVerified {
		t.Fatalf("confirmed MFA session = %+v, recovery codes = %d", verifiedAdmin.Session, len(recoveryCodes))
	}
	if _, err := service.Authenticate(ctx, adminLogin.SessionToken); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("pre-MFA session after rotation error = %v, want ErrSessionInvalid", err)
	}
	if err := service.AdminResetPassword(ctx, verifiedAdmin.Session, target.ID, "target replacement value", "reset_success"); err != nil {
		t.Fatalf("AdminResetPassword() error = %v", err)
	}
	if _, err := service.Login(ctx, target.Email, "target password value", "", LoginMetadata{RemoteAddr: "192.0.2.20"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("target old password Login() error = %v, want ErrInvalidCredentials", err)
	}
	if _, err := service.Login(ctx, target.Email, "target replacement value", "", LoginMetadata{RemoteAddr: "192.0.2.20"}); err != nil {
		t.Fatalf("target replacement password Login() error = %v", err)
	}

	partial, err := service.Login(ctx, admin.Email, "admin password value", verifiedAdmin.SessionToken, LoginMetadata{RemoteAddr: "192.0.2.10", RequestID: "login_partial"})
	if err != nil || !partial.Session.MFARequired() {
		t.Fatalf("MFA Login() session = %+v, error = %v", partial.Session, err)
	}
	recovered, err := service.VerifyMFA(ctx, partial.Session, recoveryCodes[0], LoginMetadata{RemoteAddr: "192.0.2.10", RequestID: "recovery_ok"})
	if err != nil || !recovered.Session.MFAVerified {
		t.Fatalf("VerifyMFA(recovery) session = %+v, error = %v", recovered.Session, err)
	}
	secondPartial, err := service.Login(ctx, admin.Email, "admin password value", recovered.SessionToken, LoginMetadata{RemoteAddr: "192.0.2.10", RequestID: "login_partial_2"})
	if err != nil {
		t.Fatalf("second MFA Login() error = %v", err)
	}
	if _, err := service.VerifyMFA(ctx, secondPartial.Session, recoveryCodes[0], LoginMetadata{RemoteAddr: "192.0.2.10", RequestID: "recovery_replay"}); !errors.Is(err, ErrMFAInvalid) {
		t.Fatalf("reused recovery code error = %v, want ErrMFAInvalid", err)
	}

	normalLogin, err := service.Login(ctx, normal.Email, "normal password value", "", LoginMetadata{RemoteAddr: "192.0.2.30"})
	if err != nil {
		t.Fatalf("normal Login() error = %v", err)
	}
	if err := service.AdminResetPassword(ctx, normalLogin.Session, target.ID, "forbidden reset value", "reset_denied"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("non-admin reset error = %v, want ErrPermissionDenied", err)
	}

	finalAdmin, err := service.VerifyMFA(ctx, secondPartial.Session, recoveryCodes[1], LoginMetadata{RemoteAddr: "192.0.2.10", RequestID: "recovery_admin_surface"})
	if err != nil {
		t.Fatalf("VerifyMFA(admin surface) error = %v", err)
	}
	handler, err := NewHandler(service, slog.New(slog.NewTextHandler(io.Discard, nil)), HandlerConfig{
		AllowedOrigin: "https://console.example",
		CookieSecure:  true,
		SessionTTL:    12 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	protected := handler.ProtectRole(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), "admin")
	assertProtectedStatus := func(token string, want int) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/models", nil)
		request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
		recorder := httptest.NewRecorder()
		protected.ServeHTTP(recorder, request)
		if recorder.Code != want {
			t.Fatalf("ProtectRole() status = %d, body = %s, want %d", recorder.Code, recorder.Body.String(), want)
		}
	}
	assertProtectedStatus(finalAdmin.SessionToken, http.StatusNoContent)
	assertProtectedStatus(normalLogin.SessionToken, http.StatusForbidden)
}

func testAuthService(t *testing.T) (*Service, time.Time) {
	t.Helper()
	databaseURL := isolatedAuthDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := data.Migrate(ctx, databaseURL, migrations.Files, logger); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	store, err := data.Open(ctx, data.Config{URL: databaseURL, MaxConns: 4})
	if err != nil {
		t.Fatalf("data.Open() error = %v", err)
	}
	t.Cleanup(store.Close)
	repository, err := NewRepository(store)
	if err != nil {
		t.Fatalf("NewRepository() error = %v", err)
	}
	box, err := NewSecretBox(bytes.Repeat([]byte{0x42}, 32), "test-v1")
	if err != nil {
		t.Fatalf("NewSecretBox() error = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service, err := NewService(repository, ServiceConfig{
		SessionTTL:      12 * time.Hour,
		Issuer:          "Bablo Test",
		RequireAdminMFA: true,
		SecretBox:       box,
		Now:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service, now
}

func isolatedAuthDatabaseURL(t *testing.T) string {
	t.Helper()
	baseURL := os.Getenv("BABLO_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("BABLO_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse BABLO_TEST_DATABASE_URL: %v", err)
	}
	schema := "bablo_auth_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		pool.Close()
		t.Fatalf("create test schema: %v", err)
	}
	pool.Close()
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		cleanupPool, err := pgxpool.New(cleanupCtx, baseURL)
		if err != nil {
			t.Errorf("open cleanup database: %v", err)
			return
		}
		defer cleanupPool.Close()
		if _, err := cleanupPool.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})
	return parsed.String()
}

func authRequest(t *testing.T, handler http.Handler, method, path string, payload any, cookies []*http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		body = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("Origin", "https://console.example")
	request.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func authCookies(t *testing.T, response *http.Response) (*http.Cookie, *http.Cookie) {
	t.Helper()
	var sessionCookie, csrfCookie *http.Cookie
	for _, cookie := range response.Cookies() {
		switch cookie.Name {
		case sessionCookieName:
			sessionCookie = cookie
		case csrfCookieName:
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || csrfCookie == nil {
		t.Fatalf("auth cookies missing: %v", response.Cookies())
	}
	return sessionCookie, csrfCookie
}
