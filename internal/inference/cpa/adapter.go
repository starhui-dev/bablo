package cpa

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	clipexec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"

	"github.com/starhui-dev/bablo/internal/inference"
)

const sdkVersion = "v7.2.145"

// Options configures a manager-backed adapter without exposing CPA types.
type Options struct {
	Providers    []string
	Capabilities inference.Capabilities
}

// ServiceOptions configures an adapter that embeds the CPA Service.
type ServiceOptions struct {
	ConfigPath   string
	Providers    []string
	Capabilities inference.Capabilities
}

type readinessState struct {
	mu  sync.Mutex
	err error
}

func (s *readinessState) setError(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

func (s *readinessState) error() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Adapter translates Bablo inference values to the pinned CPA SDK.
type Adapter struct {
	manager      *coreauth.Manager
	service      *cliproxy.Service
	providers    []string
	capabilities inference.Capabilities

	mu            sync.Mutex
	started       bool
	closed        bool
	cancel        context.CancelFunc
	runDone       chan struct{}
	runErr        error
	ready         chan struct{}
	startup       *readinessState
	startupCancel context.CancelFunc
}

// New constructs a manager-backed adapter. Service construction is available via NewService.
func New(opts Options) *Adapter {
	return newWithManager(coreauth.NewManager(nil, nil, nil), nil, opts.Providers, opts.Capabilities)
}

// NewService constructs an adapter and CPA Service from a CPA config file.
// The embedded CPA HTTP endpoint is lifecycle-internal; Bablo remains the
// public HTTP surface and PostgreSQL remains the credential source of truth.
func NewService(opts ServiceOptions) (*Adapter, error) {
	path := strings.TrimSpace(opts.ConfigPath)
	if path == "" {
		return nil, errors.New("cpa adapter: config path is required")
	}
	cfg, err := sdkconfig.LoadConfig(path)
	if err != nil {
		return nil, fmt.Errorf("cpa adapter: load config: %w", err)
	}
	if err := validateEmbeddedConfig(cfg); err != nil {
		return nil, err
	}
	if err := validateCredentialSourceConfig(cfg); err != nil {
		return nil, err
	}
	if err := ensureEmbeddedPort(cfg); err != nil {
		return nil, err
	}
	tokenStore := auth.GetTokenStore()
	if setter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
		setter.SetBaseDir(cfg.AuthDir)
	}
	// A nil CPA Store is intentional. Bablo registers decrypted runtime
	// credentials from PostgreSQL and must not let CPA Load overwrite them from
	// auth files or persist refreshed secrets outside Bablo's encrypted store.
	manager := coreauth.NewManager(nil, nil, nil)
	startupCtx, startupCancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	startup := &readinessState{}
	var readyOnce sync.Once
	startupResult := make(chan error, 1)
	service, err := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(path).
		WithCoreAuthManager(manager).
		// The callback is synchronous and runs before CPA creates its watcher.
		// It establishes a real HTTP readiness barrier before runtime config
		// synchronization touches the embedded Server.
		WithWatcherFactory(func(_ string, _ string, reload func(*sdkconfig.Config)) (*cliproxy.WatcherWrapper, error) {
			startupErr := <-startupResult
			if startupErr != nil {
				startup.setError(startupErr)
			} else {
				// This public callback is CPA's supported runtime synchronization
				// boundary. It also binds built-in executors for current Auths.
				reload(cfg)
			}
			readyOnce.Do(func() { close(ready) })
			return nil, nil
		}).
		WithHooks(cliproxy.Hooks{OnAfterStart: func(*cliproxy.Service) {
			startupResult <- waitForEmbeddedServer(startupCtx, cfg)
		}}).
		Build()
	if err != nil {
		startupCancel()
		return nil, fmt.Errorf("cpa adapter: build service: %w", err)
	}
	adapter := newWithManager(manager, service, opts.Providers, opts.Capabilities)
	adapter.ready = ready
	adapter.startup = startup
	adapter.startupCancel = startupCancel
	return adapter, nil
}

func ensureEmbeddedPort(cfg *sdkconfig.Config) error {
	if cfg == nil {
		return errors.New("cpa adapter: service config is nil")
	}
	if cfg.Port > 0 {
		return nil
	}
	if cfg.Port < 0 {
		return errors.New("cpa adapter: embedded service port is invalid")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(strings.TrimSpace(cfg.Host), "0"))
	if err != nil {
		return fmt.Errorf("cpa adapter: allocate embedded service port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return fmt.Errorf("cpa adapter: release embedded service port: %w", err)
	}
	cfg.Port = port
	return nil
}

func waitForEmbeddedServer(ctx context.Context, cfg *sdkconfig.Config) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil {
		return errors.New("cpa adapter: service config is nil")
	}
	address := net.JoinHostPort(strings.TrimSpace(cfg.Host), strconv.Itoa(cfg.Port))
	endpoint := "http://" + address + "/healthz"
	deadline := time.Now().Add(5 * time.Second)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err == nil {
			response, requestErr := client.Do(request)
			if requestErr == nil {
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cpa adapter: embedded service did not become ready at %s", endpoint)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func validateEmbeddedConfig(cfg *sdkconfig.Config) error {
	if cfg == nil {
		return errors.New("cpa adapter: service config is nil")
	}
	switch strings.TrimSpace(cfg.Host) {
	case "127.0.0.1", "::1":
	default:
		return errors.New("cpa adapter: embedded service host must be loopback")
	}
	if strings.TrimSpace(cfg.RemoteManagement.SecretKey) != "" {
		return errors.New("cpa adapter: CPA management secret must be disabled")
	}
	if cfg.RemoteManagement.AllowRemote {
		return errors.New("cpa adapter: remote management must be disabled")
	}
	return nil
}

func validateCredentialSourceConfig(cfg *sdkconfig.Config) error {
	if cfg == nil {
		return errors.New("cpa adapter: service config is nil")
	}
	if len(cfg.APIKeys) > 0 || len(cfg.GeminiKey) > 0 || len(cfg.InteractionsKey) > 0 || len(cfg.CodexKey) > 0 || len(cfg.XAIKey) > 0 || len(cfg.ClaudeKey) > 0 || len(cfg.VertexCompatAPIKey) > 0 {
		return errors.New("cpa adapter: upstream credentials must be managed by Bablo PostgreSQL, not CPA config")
	}
	for index := range cfg.OpenAICompatibility {
		if len(cfg.OpenAICompatibility[index].APIKeyEntries) > 0 {
			return errors.New("cpa adapter: OpenAI-compatible credentials must be managed by Bablo PostgreSQL, not CPA config")
		}
	}
	return nil
}

func newWithManager(manager *coreauth.Manager, service *cliproxy.Service, providers []string, capabilities inference.Capabilities) *Adapter {
	ready := make(chan struct{})
	if service == nil {
		close(ready)
	}
	return &Adapter{
		manager:      manager,
		service:      service,
		providers:    append([]string(nil), providers...),
		capabilities: cloneCapabilities(capabilities),
		ready:        ready,
	}
}

// SDKVersion reports the exact CPA version compiled into this adapter.
func (a *Adapter) SDKVersion() string { return sdkVersion }

// registerExecutor registers a CPA provider executor inside the adapter boundary.
func (a *Adapter) registerExecutor(executor coreauth.ProviderExecutor) {
	if a == nil || executor == nil {
		return
	}
	a.manager.RegisterExecutor(executor)
}

// registerAuth registers a CPA auth record inside the adapter boundary.
func (a *Adapter) registerAuth(ctx context.Context, authRecord *coreauth.Auth) error {
	if a == nil || a.manager == nil {
		return errors.New("cpa adapter: manager is unavailable")
	}
	if authRecord == nil {
		return errors.New("cpa adapter: auth record is nil")
	}
	_, err := a.manager.Register(ctx, authRecord)
	return err
}

// Start starts the embedded CPA Service when one was configured.
func (a *Adapter) Start(ctx context.Context) error {
	if a == nil {
		return errors.New("cpa adapter: adapter is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return errors.New("cpa adapter: adapter is closed")
	}
	if a.started {
		return nil
	}
	a.started = true
	if a.service == nil {
		return nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	a.runDone = make(chan struct{})
	done := a.runDone
	service := a.service
	go func() {
		err := service.Run(runCtx)
		a.mu.Lock()
		a.runErr = err
		close(done)
		a.mu.Unlock()
	}()
	return nil
}

// Shutdown stops the embedded CPA Service and auto-refresh resources.
func (a *Adapter) Shutdown(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	cancel := a.cancel
	startupCancel := a.startupCancel
	runDone := a.runDone
	service := a.service
	a.cancel = nil
	a.startupCancel = nil
	a.mu.Unlock()

	if startupCancel != nil {
		startupCancel()
	}
	if cancel != nil {
		cancel()
	}

	var shutdownErr error
	if runDone != nil {
		// CPA's public OnAfterStart hook fires before its watcher and refresh
		// setup completes. Cancelling Run and waiting for it to unwind keeps
		// the SDK's own startup/shutdown sequence serialized.
		select {
		case <-runDone:
			a.mu.Lock()
			runErr := a.runErr
			a.mu.Unlock()
			if runErr != nil && !errors.Is(runErr, context.Canceled) && shutdownErr == nil {
				shutdownErr = runErr
			}
		case <-ctx.Done():
			shutdownErr = ctx.Err()
		}
	} else if service != nil {
		// Start was never called, so no SDK Run goroutine can race this call.
		shutdownErr = service.Shutdown(ctx)
	}
	if a.manager != nil {
		a.manager.StopAutoRefresh()
	}
	return shutdownErr
}

// WaitReady waits until the embedded CPA lifecycle reaches its public
// OnAfterStart hook. Manager-only adapters are ready immediately.
func (a *Adapter) WaitReady(ctx context.Context) error {
	if a == nil {
		return errors.New("cpa adapter: adapter is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	service := a.service
	started := a.started
	ready := a.ready
	runDone := a.runDone
	startup := a.startup
	a.mu.Unlock()
	if service == nil {
		return nil
	}
	if !started || ready == nil || runDone == nil {
		return errors.New("cpa adapter: service has not started")
	}
	select {
	case <-ready:
		if startupErr := startup.error(); startupErr != nil {
			return fmt.Errorf("cpa adapter: service startup failed: %w", startupErr)
		}
		return nil
	case <-runDone:
		a.mu.Lock()
		runErr := a.runErr
		a.mu.Unlock()
		if runErr == nil {
			return errors.New("cpa adapter: service stopped before readiness")
		}
		return fmt.Errorf("cpa adapter: service stopped before readiness: %w", runErr)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Execute executes one non-streaming request through CPA core auth Manager.
func (a *Adapter) Execute(ctx context.Context, request inference.Request) (inference.ExecutionResult, error) {
	if err := a.validateRequest(request); err != nil {
		return inference.ExecutionResult{}, err
	}
	providers := a.providersFor(request)
	if len(providers) == 0 {
		return inference.ExecutionResult{}, &inference.UpstreamError{Class: "provider_not_configured", HTTPStatus: http.StatusBadRequest}
	}

	sourceFormat := formatFromName(request.SourceFormat)
	responseFormat := formatFromName(request.ResponseFormat)
	payload := cloneBytes(request.Body)
	metadata := cloneMetadata(request.Metadata)
	if request.RequestID != "" {
		metadata["request_id"] = request.RequestID
	}
	if request.ResolvedRoute.CredentialID != "" {
		metadata[clipexec.PinnedAuthMetadataKey] = request.ResolvedRoute.CredentialID
	}

	cpaRequest := clipexec.Request{
		Model:    requestModel(request),
		Payload:  payload,
		Format:   sourceFormat,
		Metadata: metadata,
	}
	options := clipexec.Options{
		Stream:          false,
		Headers:         requestHeaders(request),
		OriginalRequest: cloneBytes(payload),
		SourceFormat:    sourceFormat,
		ResponseFormat:  responseFormat,
		Metadata:        metadata,
	}
	response, err := a.manager.Execute(ctx, providers, cpaRequest, options)
	if err != nil {
		return inference.ExecutionResult{}, mapError(err)
	}
	return inference.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    fromHTTPHeader(response.Headers),
		Body:       cloneBytes(response.Payload),
	}, nil
}

// ExecuteStream executes a streaming request and adapts CPA chunks to Bablo events.
func (a *Adapter) ExecuteStream(ctx context.Context, request inference.Request) (inference.Stream, error) {
	if err := a.validateRequest(request); err != nil {
		return nil, err
	}
	providers := a.providersFor(request)
	if len(providers) == 0 {
		return nil, &inference.UpstreamError{Class: "provider_not_configured", HTTPStatus: http.StatusBadRequest}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	sourceFormat := formatFromName(request.SourceFormat)
	responseFormat := formatFromName(request.ResponseFormat)
	payload := cloneBytes(request.Body)
	metadata := cloneMetadata(request.Metadata)
	if request.RequestID != "" {
		metadata["request_id"] = request.RequestID
	}
	if request.ResolvedRoute.CredentialID != "" {
		metadata[clipexec.PinnedAuthMetadataKey] = request.ResolvedRoute.CredentialID
	}
	cpaRequest := clipexec.Request{
		Model:    requestModel(request),
		Payload:  payload,
		Format:   sourceFormat,
		Metadata: metadata,
	}
	options := clipexec.Options{
		Stream:          true,
		Headers:         requestHeaders(request),
		OriginalRequest: cloneBytes(payload),
		SourceFormat:    sourceFormat,
		ResponseFormat:  responseFormat,
		Metadata:        metadata,
	}

	streamCtx, cancel := context.WithCancel(ctx)
	result, err := a.manager.ExecuteStream(streamCtx, providers, cpaRequest, options)
	if err != nil {
		cancel()
		return nil, mapError(err)
	}
	if result == nil || result.Chunks == nil {
		cancel()
		return nil, &inference.UpstreamError{Class: "empty_stream", Retryable: true}
	}
	return newStream(result, cancel), nil
}

// Capabilities returns the adapter's configured capability snapshot.
func (a *Adapter) Capabilities(context.Context) (inference.Capabilities, error) {
	if a == nil {
		return inference.Capabilities{}, errors.New("cpa adapter: adapter is nil")
	}
	return cloneCapabilities(a.capabilities), nil
}

func (a *Adapter) validateRequest(request inference.Request) error {
	_ = request
	if a == nil || a.manager == nil {
		return errors.New("cpa adapter: manager is unavailable")
	}
	return nil
}
func (a *Adapter) providersFor(request inference.Request) []string {
	if provider := strings.TrimSpace(request.ResolvedRoute.ProviderID); provider != "" {
		return []string{provider}
	}
	return append([]string(nil), a.providers...)
}

func requestModel(request inference.Request) string {
	if model := strings.TrimSpace(request.ResolvedRoute.ResolvedModel); model != "" {
		return model
	}
	return strings.TrimSpace(request.ResolvedRoute.RequestedModel)
}

func formatFromName(value string) sdktranslator.Format {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "openai", "openai.chat", "openai-compatible":
		return sdktranslator.FormatOpenAI
	case "openai-response", "openai.responses", "responses":
		return sdktranslator.FormatOpenAIResponse
	case "claude", "anthropic", "messages":
		return sdktranslator.FormatClaude
	case "gemini":
		return sdktranslator.FormatGemini
	case "codex":
		return sdktranslator.FormatCodex
	default:
		return sdktranslator.FromString(value)
	}
}

func cloneMetadata(source map[string]any) map[string]any {
	result := make(map[string]any, len(source)+2)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneBytes(source []byte) []byte {
	if source == nil {
		return nil
	}
	return append([]byte(nil), source...)
}

func toHTTPHeader(source map[string][]string) http.Header {
	if len(source) == 0 {
		return nil
	}
	result := make(http.Header, len(source))
	for key, values := range source {
		result[key] = append([]string(nil), values...)
	}
	return result
}

func requestHeaders(request inference.Request) http.Header {
	headers := toHTTPHeader(request.Headers)
	if request.RequestID == "" {
		return headers
	}
	if headers == nil {
		headers = make(http.Header)
	}
	if headers.Get("X-Request-ID") == "" {
		headers.Set("X-Request-ID", request.RequestID)
	}
	return headers
}

func fromHTTPHeader(source http.Header) map[string][]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string][]string, len(source))
	for key, values := range source {
		result[key] = append([]string(nil), values...)
	}
	return result
}

func cloneCapabilities(source inference.Capabilities) inference.Capabilities {
	result := source
	result.Formats = append([]string(nil), source.Formats...)
	result.ModelIDs = append([]string(nil), source.ModelIDs...)
	return result
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	status := 0
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) && statusErr != nil {
		status = statusErr.StatusCode()
	}
	var authErr *coreauth.Error
	if errors.As(err, &authErr) && authErr != nil {
		if authErr.HTTPStatus > 0 {
			status = authErr.HTTPStatus
		}
		class := classifyAuthCode(authErr.Code)
		if class != "" {
			return &inference.UpstreamError{Class: class, HTTPStatus: status, Retryable: retryableStatus(status)}
		}
	}
	return &inference.UpstreamError{
		Class:      classifyStatus(status),
		HTTPStatus: status,
		Retryable:  retryableStatus(status),
	}
}

func classifyAuthCode(code string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "provider_not_found", "executor_not_found", "provider_not_configured":
		return "provider_not_configured"
	case "auth_not_found", "auth_unavailable":
		return "credential_unavailable"
	default:
		return ""
	}
}

func classifyStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication"
	case status == http.StatusForbidden:
		return "permission"
	case status == http.StatusRequestTimeout:
		return "timeout"
	case status == http.StatusTooManyRequests:
		return "rate_limit"
	case status >= http.StatusInternalServerError:
		return "server"
	case status >= http.StatusBadRequest:
		return "client"
	default:
		return "network"
	}
}

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}
