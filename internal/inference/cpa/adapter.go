package cpa

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

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

// Adapter translates Bablo inference values to the pinned CPA SDK.
type Adapter struct {
	manager      *coreauth.Manager
	service      *cliproxy.Service
	providers    []string
	capabilities inference.Capabilities

	mu      sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	runDone chan error
}

// New constructs a manager-backed adapter. Service construction is available via NewService.
func New(opts Options) *Adapter {
	return newWithManager(coreauth.NewManager(nil, nil, nil), nil, opts.Providers, opts.Capabilities)
}

// NewService constructs an adapter and CPA Service from a CPA config file.
// The config path is required by the pinned CPA Builder API.
func NewService(opts ServiceOptions) (*Adapter, error) {
	path := strings.TrimSpace(opts.ConfigPath)
	if path == "" {
		return nil, errors.New("cpa adapter: config path is required")
	}
	cfg, err := sdkconfig.LoadConfig(path)
	if err != nil {
		return nil, fmt.Errorf("cpa adapter: load config: %w", err)
	}

	tokenStore := auth.GetTokenStore()
	if setter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
		setter.SetBaseDir(cfg.AuthDir)
	}
	manager := coreauth.NewManager(tokenStore, nil, nil)
	service, err := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(path).
		WithCoreAuthManager(manager).
		Build()
	if err != nil {
		return nil, fmt.Errorf("cpa adapter: build service: %w", err)
	}
	return newWithManager(manager, service, opts.Providers, opts.Capabilities), nil
}

func newWithManager(manager *coreauth.Manager, service *cliproxy.Service, providers []string, capabilities inference.Capabilities) *Adapter {
	if manager == nil {
		manager = coreauth.NewManager(nil, nil, nil)
	}
	return &Adapter{
		manager:      manager,
		service:      service,
		providers:    append([]string(nil), providers...),
		capabilities: cloneCapabilities(capabilities),
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
	a.runDone = make(chan error, 1)
	done := a.runDone
	service := a.service
	go func() {
		err := service.Run(runCtx)
		done <- err
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
	runDone := a.runDone
	service := a.service
	a.cancel = nil
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	a.manager.StopAutoRefresh()

	var shutdownErr error
	if service != nil {
		shutdownErr = service.Shutdown(ctx)
	}
	if runDone != nil {
		select {
		case err := <-runDone:
			if err != nil && !errors.Is(err, context.Canceled) && shutdownErr == nil {
				shutdownErr = err
			}
		case <-ctx.Done():
			if shutdownErr == nil {
				shutdownErr = ctx.Err()
			}
		}
	}
	return shutdownErr
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
