package cpa

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"

	"github.com/starhui-dev/bablo/internal/credential"
	"github.com/starhui-dev/bablo/internal/inference"
)

type fakeExecutor struct {
	provider   string
	status     int
	streamBody [][]byte
	waitStream bool
	mu         sync.Mutex
	requestID  string
	headerID   string
	authID     string
}

func (f *fakeExecutor) Identifier() string { return f.provider }

func (f *fakeExecutor) Execute(ctx context.Context, auth *coreauth.Auth, _ coreexec.Request, opts coreexec.Options) (coreexec.Response, error) {
	f.captureRequest(auth, opts)
	if f.status != 0 {
		return coreexec.Response{}, statusError{status: f.status}
	}
	select {
	case <-ctx.Done():
		return coreexec.Response{}, ctx.Err()
	default:
	}
	return coreexec.Response{Payload: []byte(`{"ok":true}`), Headers: http.Header{"X-Fake": {"yes"}}}, nil
}

func (f *fakeExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, _ coreexec.Request, opts coreexec.Options) (*coreexec.StreamResult, error) {
	f.captureRequest(auth, opts)
	if f.status != 0 {
		return nil, statusError{status: f.status}
	}
	chunks := make(chan coreexec.StreamChunk)
	go func() {
		defer close(chunks)
		for _, payload := range f.streamBody {
			select {
			case chunks <- coreexec.StreamChunk{Payload: payload}:
			case <-ctx.Done():
				return
			}
		}
		if f.waitStream {
			<-ctx.Done()
		}
	}()
	return &coreexec.StreamResult{Headers: http.Header{"X-Stream": {"yes"}}, Chunks: chunks}, nil
}

func (f *fakeExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, ctx.Err()
}

func (f *fakeExecutor) CountTokens(context.Context, *coreauth.Auth, coreexec.Request, coreexec.Options) (coreexec.Response, error) {
	return coreexec.Response{Payload: []byte(`{"count":1}`)}, nil
}

func (f *fakeExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("fake executor does not expose HTTP requests")
}

func (f *fakeExecutor) captureRequest(auth *coreauth.Auth, opts coreexec.Options) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestID, _ = opts.Metadata["request_id"].(string)
	f.headerID = opts.Headers.Get("X-Request-ID")
	if auth != nil {
		f.authID = auth.ID
	}
}

func (f *fakeExecutor) capturedRequest() (string, string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requestID, f.headerID, f.authID
}

type statusError struct{ status int }

func (e statusError) Error() string   { return "fake upstream failure" }
func (e statusError) StatusCode() int { return e.status }

func testAdapter(t *testing.T, executor *fakeExecutor) *Adapter {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 1)
	adapter := newWithManager(manager, nil, []string{executor.provider}, inference.Capabilities{Streaming: true})
	adapter.registerExecutor(executor)
	if err := adapter.registerAuth(context.Background(), &coreauth.Auth{ID: "auth-1", Provider: executor.provider, Status: coreauth.StatusActive}); err != nil {
		t.Fatalf("registerAuth() error = %v", err)
	}
	t.Cleanup(func() {
		if err := adapter.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
	})
	return adapter
}

func request() inference.Request {
	return inference.Request{
		RequestID: "req-cpa-test",
		ResolvedRoute: inference.ResolvedRoute{
			ProviderID:   "fake",
			CredentialID: "auth-1",
		},
		SourceFormat: "openai",
		Headers:      map[string][]string{"X-Request": {"yes"}},
		Body:         []byte(`{"model":"public-model","messages":[]}`),
	}
}

func TestAdapterExecuteMapsResponseAndPropagatesRequestID(t *testing.T) {
	executor := &fakeExecutor{provider: "fake"}
	adapter := testAdapter(t, executor)

	result, err := adapter.Execute(context.Background(), request())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.StatusCode != http.StatusOK || string(result.Body) != `{"ok":true}` {
		t.Fatalf("result = %#v, want successful response", result)
	}
	if got := result.Headers["X-Fake"][0]; got != "yes" {
		t.Fatalf("response header = %q, want yes", got)
	}
	metadataID, headerID, authID := executor.capturedRequest()
	if metadataID != "req-cpa-test" || headerID != "req-cpa-test" || authID != "auth-1" {
		t.Fatalf("request identity = metadata %q, header %q, auth %q", metadataID, headerID, authID)
	}
}

func TestAdapterExecuteMapsUpstreamStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			adapter := testAdapter(t, &fakeExecutor{provider: "fake", status: status})
			_, err := adapter.Execute(context.Background(), request())
			if err == nil {
				t.Fatal("Execute() error = nil, want upstream error")
			}
			var upstreamErr *inference.UpstreamError
			if !errors.As(err, &upstreamErr) {
				t.Fatalf("error = %T %v, want UpstreamError", err, err)
			}
			if upstreamErr.HTTPStatus != status {
				t.Fatalf("status = %d, want %d", upstreamErr.HTTPStatus, status)
			}
		})
	}
}

func TestAdapterExecutePreservesCancellation(t *testing.T) {
	executor := &fakeExecutor{provider: "fake"}
	adapter := testAdapter(t, executor)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := adapter.Execute(ctx, request())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestAdapterStreamAndCloseCancelProducer(t *testing.T) {
	executor := &fakeExecutor{provider: "fake", streamBody: [][]byte{[]byte("chunk-1")}, waitStream: true}
	adapter := testAdapter(t, executor)

	stream, err := adapter.ExecuteStream(context.Background(), request())
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	headers := stream.Headers()
	if got := headers["X-Stream"][0]; got != "yes" {
		t.Fatalf("stream header = %q, want yes", got)
	}
	headers["X-Stream"][0] = "mutated"
	if got := stream.Headers()["X-Stream"][0]; got != "yes" {
		t.Fatalf("stream Headers() leaked mutable slice: %q", got)
	}
	event, err := stream.Next(context.Background())
	if err != nil || string(event.Payload) != "chunk-1" {
		t.Fatalf("first event = %#v, error = %v", event, err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	_, err = stream.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Fatalf("next after close error = %v, want io.EOF", err)
	}
}

func TestAdapterCapabilitiesReturnsCopyAndVersion(t *testing.T) {
	adapter := New(Options{
		Providers: []string{"fake"},
		Capabilities: inference.Capabilities{
			Formats:   []string{"openai", "openai-response"},
			Streaming: true,
			ModelIDs:  []string{"model-a"},
		},
	})
	if got := adapter.SDKVersion(); got != "v7.2.149" {
		t.Fatalf("SDKVersion() = %q, want v7.2.149", got)
	}
	capabilities, err := adapter.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities() error = %v", err)
	}
	capabilities.Formats[0] = "mutated"
	capabilities.ModelIDs[0] = "mutated"
	again, err := adapter.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities() second error = %v", err)
	}
	if again.Formats[0] != "openai" || again.ModelIDs[0] != "model-a" {
		t.Fatalf("Capabilities() leaked mutable slices: %#v", again)
	}
}

func TestAdapterDefaultProviderAndLifecycle(t *testing.T) {
	executor := &fakeExecutor{provider: "fake"}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 1)
	adapter := newWithManager(manager, nil, []string{"fake"}, inference.Capabilities{})
	adapter.registerExecutor(executor)
	if err := adapter.registerAuth(context.Background(), &coreauth.Auth{ID: "auth-default", Provider: "fake", Status: coreauth.StatusActive}); err != nil {
		t.Fatalf("registerAuth() error = %v", err)
	}
	if err := adapter.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := adapter.Start(context.Background()); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	if err := adapter.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	req := request()
	req.ResolvedRoute.ProviderID = ""
	req.ResolvedRoute.CredentialID = ""
	if _, err := adapter.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute() with default provider error = %v", err)
	}
	if err := adapter.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := adapter.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
	if err := adapter.Start(context.Background()); err == nil {
		t.Fatal("Start() after Shutdown() error = nil, want closed error")
	}
}

func TestAdapterRejectsMissingProvider(t *testing.T) {
	adapter := New(Options{})
	_, err := adapter.Execute(context.Background(), inference.Request{})
	var upstreamErr *inference.UpstreamError
	if !errors.As(err, &upstreamErr) || upstreamErr.Class != "provider_not_configured" || upstreamErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("error = %#v, want provider_not_configured 400", err)
	}
}

func TestAdapterStreamMapsChunkError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks := make(chan coreexec.StreamChunk, 1)
	chunks <- coreexec.StreamChunk{Err: statusError{status: http.StatusTooManyRequests}}
	close(chunks)
	stream := newStream(&coreexec.StreamResult{Chunks: chunks}, cancel)
	_, err := stream.Next(ctx)
	var upstreamErr *inference.UpstreamError
	if !errors.As(err, &upstreamErr) || upstreamErr.Class != "rate_limit" || upstreamErr.HTTPStatus != http.StatusTooManyRequests || !upstreamErr.Retryable {
		t.Fatalf("stream error = %#v, want retryable rate_limit", err)
	}
}

func TestAdapterStreamNextHonorsCallerCancellation(t *testing.T) {
	producerCtx, producerCancel := context.WithCancel(context.Background())
	chunks := make(chan coreexec.StreamChunk)
	go func() {
		<-producerCtx.Done()
		close(chunks)
	}()
	stream := newStream(&coreexec.StreamResult{Chunks: chunks}, producerCancel)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := stream.Next(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Next() error = %v, want context.Canceled", err)
	}
}

func TestNewServiceBuildsAndShutsDown(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configBody := "host: 127.0.0.1\nport: 0\nauth-dir: " + filepath.Join(t.TempDir(), "auth") + "\nremote-management:\n  allow-remote: false\n"
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	adapter, err := NewService(ServiceOptions{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := adapter.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readyCancel()
	if err := adapter.WaitReady(readyCtx); err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	if err := adapter.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestNewServiceExecutesRegisteredOpenAICompatibilityCredential(t *testing.T) {
	var upstreamRequest struct {
		Model string `json:"model"`
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer integration-key" {
			t.Errorf("upstream authorization = %q, want bearer credential", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamRequest); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-integration","object":"chat.completion","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	authDir := filepath.Join(t.TempDir(), "auth")
	configBody := "host: 127.0.0.1\nport: 0\nauth-dir: " + authDir + "\nremote-management:\n  allow-remote: false\nopenai-compatibility:\n  - name: local-provider\n    base-url: " + upstream.URL + "\n    models:\n      - name: upstream-model\n        alias: upstream-model\n"
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	adapter, err := NewService(ServiceOptions{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	credentialID := uuid.New()
	if err := adapter.RegisterCredential(context.Background(), credential.RuntimeCredential{
		CredentialID: credentialID,
		ProviderSlug: "local-provider",
		SourceKind:   credential.SourceAPIKey,
		Metadata:     map[string]string{"base_url": upstream.URL},
		Secrets:      map[string][]byte{credential.SecretAPIKey: []byte("integration-key")},
	}); err != nil {
		t.Fatalf("RegisterCredential() error = %v", err)
	}
	startCtx, startCancel := context.WithCancel(context.Background())
	defer startCancel()
	if err := adapter.Start(startCtx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer readyCancel()
	if err := adapter.WaitReady(readyCtx); err != nil {
		t.Fatalf("WaitReady() error = %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := adapter.Shutdown(shutdownCtx); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
	}()

	result, err := adapter.Execute(context.Background(), inference.Request{
		RequestID: "req-service-integration",
		ResolvedRoute: inference.ResolvedRoute{
			ProviderID:     "local-provider",
			CredentialID:   credentialID.String(),
			RequestedModel: "public-model",
			ResolvedModel:  "upstream-model",
		},
		SourceFormat:   "openai",
		ResponseFormat: "openai",
		Body:           []byte(`{"model":"public-model","messages":[]}`),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.StatusCode != http.StatusOK || !json.Valid(result.Body) {
		t.Fatalf("result = %#v, want valid successful JSON", result)
	}
	if upstreamRequest.Model != "upstream-model" {
		t.Fatalf("upstream model = %q, want upstream-model", upstreamRequest.Model)
	}
}

func TestStreamRejectsNilChunkChannel(t *testing.T) {
	stream := newStream(&coreexec.StreamResult{}, func() {})
	_, err := stream.Next(context.Background())
	var upstreamErr *inference.UpstreamError
	if !errors.As(err, &upstreamErr) || upstreamErr.Class != "empty_stream" || !upstreamErr.Retryable {
		t.Fatalf("error = %#v, want retryable empty_stream", err)
	}
}

func TestNewServiceRejectsMissingConfig(t *testing.T) {
	if _, err := NewService(ServiceOptions{}); err == nil {
		t.Fatal("NewService() error = nil, want config path error")
	}
}

func TestNewServiceRejectsUnsafeEmbeddedNetworkConfig(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "wildcard host", body: "host: 0.0.0.0\nport: 8317\n", want: "loopback"},
		{name: "remote management", body: "host: 127.0.0.1\nport: 8317\nremote-management:\n  allow-remote: true\n", want: "remote management"},
		{name: "management secret", body: "host: 127.0.0.1\nport: 8317\nremote-management:\n  secret-key: local-secret\n", want: "management"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			if _, err := NewService(ServiceOptions{ConfigPath: path}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewService() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestNewServiceRejectsConfigCredentials(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "client api keys", body: "host: 127.0.0.1\nport: 8317\napi-keys:\n  - client-key\n", want: "upstream credentials"},
		{name: "provider api key", body: "host: 127.0.0.1\nport: 8317\ngemini-api-key:\n  - api-key: provider-key\n", want: "upstream credentials"},
		{name: "openai compatibility api key", body: "host: 127.0.0.1\nport: 8317\nopenai-compatibility:\n  - name: provider\n    base-url: https://upstream.example.com/v1\n    api-key-entries:\n      - api-key: provider-key\n", want: "OpenAI-compatible credentials"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(test.body), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			if _, err := NewService(ServiceOptions{ConfigPath: path}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewService() error = %v, want %q", err, test.want)
			}
		})
	}
}
func TestRequestModelUsesResolvedRoute(t *testing.T) {
	req := inference.Request{ResolvedRoute: inference.ResolvedRoute{RequestedModel: "public", ResolvedModel: "upstream"}}
	if got := requestModel(req); got != "upstream" {
		t.Fatalf("requestModel() = %q, want upstream", got)
	}
	req.ResolvedRoute.ResolvedModel = ""
	if got := requestModel(req); got != "public" {
		t.Fatalf("requestModel() fallback = %q, want public", got)
	}
}

func TestFormatFromNameUsesPinnedPublicFormats(t *testing.T) {
	if got := formatFromName("responses"); got != sdktranslator.FormatOpenAIResponse {
		t.Fatalf("responses format = %q, want %q", got, sdktranslator.FormatOpenAIResponse)
	}
	if got := formatFromName("claude"); got != sdktranslator.FormatClaude {
		t.Fatalf("claude format = %q, want %q", got, sdktranslator.FormatClaude)
	}
}
