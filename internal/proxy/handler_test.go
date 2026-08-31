package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/apikey"
	"github.com/starhui-dev/bablo/internal/billing"
	"github.com/starhui-dev/bablo/internal/credential"
	"github.com/starhui-dev/bablo/internal/inference"
	catalogmodel "github.com/starhui-dev/bablo/internal/model"
	"github.com/starhui-dev/bablo/internal/pricing"
	"github.com/starhui-dev/bablo/internal/provider"
	"github.com/starhui-dev/bablo/internal/route"
	"github.com/starhui-dev/bablo/internal/scheduler"
	"github.com/starhui-dev/bablo/internal/usage"
)

type proxyAuthorizeCall struct {
	principal apikey.Principal
	model     string
	tokens    int64
}

type fakeProxyKeys struct {
	principal    apikey.Principal
	authorized   []string
	authorizeErr error
	listErr      error

	mu              sync.Mutex
	authorizeCalls  []proxyAuthorizeCall
	listCalls       []apikey.Principal
	middlewareCalls int
}

func (f *fakeProxyKeys) IdentityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.middlewareCalls++
		f.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

func (f *fakeProxyKeys) Authorize(_ context.Context, principal apikey.Principal, model string, tokens int64) error {
	f.mu.Lock()
	f.authorizeCalls = append(f.authorizeCalls, proxyAuthorizeCall{principal: principal, model: model, tokens: tokens})
	f.mu.Unlock()
	return f.authorizeErr
}

func (f *fakeProxyKeys) ListAuthorizedModels(_ context.Context, principal apikey.Principal) ([]string, error) {
	f.mu.Lock()
	f.listCalls = append(f.listCalls, principal)
	f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]string(nil), f.authorized...), nil
}

type fakeProxyCatalog struct {
	models map[string]catalogmodel.Model
	pages  map[string]catalogmodel.Page
	err    error

	mu          sync.Mutex
	resolveArgs []string
	listArgs    []string
}

func (f *fakeProxyCatalog) ResolvePublic(_ context.Context, identifier string) (catalogmodel.Model, error) {
	f.mu.Lock()
	f.resolveArgs = append(f.resolveArgs, identifier)
	f.mu.Unlock()
	if f.err != nil {
		return catalogmodel.Model{}, f.err
	}
	model, ok := f.models[strings.ToLower(strings.TrimSpace(identifier))]
	if !ok {
		return catalogmodel.Model{}, catalogmodel.ErrNotFound
	}
	return model, nil
}

func (f *fakeProxyCatalog) ListPublic(_ context.Context, cursor string, _ int) (catalogmodel.Page, error) {
	f.mu.Lock()
	f.listArgs = append(f.listArgs, cursor)
	f.mu.Unlock()
	if f.err != nil {
		return catalogmodel.Page{}, f.err
	}
	if page, ok := f.pages[cursor]; ok {
		return page, nil
	}
	return catalogmodel.Page{}, nil
}

type fakeProxyRoutes struct {
	resolution route.Resolution
	err        error

	mu    sync.Mutex
	calls []string
}

func (f *fakeProxyRoutes) Resolve(_ context.Context, identifier string) (route.Resolution, error) {
	f.mu.Lock()
	f.calls = append(f.calls, identifier)
	f.mu.Unlock()
	if f.err != nil {
		return route.Resolution{}, f.err
	}
	return f.resolution, nil
}

type fakeProxyScheduler struct {
	selection scheduler.Selection
	err       error

	mu    sync.Mutex
	calls []scheduler.Request
}

func (f *fakeProxyScheduler) Select(_ context.Context, request scheduler.Request) (scheduler.Selection, error) {
	f.mu.Lock()
	f.calls = append(f.calls, request)
	f.mu.Unlock()
	if f.err != nil {
		return scheduler.Selection{}, f.err
	}
	return f.selection, nil
}

type fakeProxyEngine struct {
	result     inference.ExecutionResult
	executeErr error
	stream     inference.Stream
	streamErr  error

	mu           sync.Mutex
	executeCalls []inference.Request
	streamCalls  []inference.Request
}

func (f *fakeProxyEngine) Execute(_ context.Context, request inference.Request) (inference.ExecutionResult, error) {
	f.mu.Lock()
	f.executeCalls = append(f.executeCalls, cloneInferenceRequest(request))
	f.mu.Unlock()
	return f.result, f.executeErr
}

func (f *fakeProxyEngine) ExecuteStream(_ context.Context, request inference.Request) (inference.Stream, error) {
	f.mu.Lock()
	f.streamCalls = append(f.streamCalls, cloneInferenceRequest(request))
	f.mu.Unlock()
	return f.stream, f.streamErr
}

func (f *fakeProxyEngine) Capabilities(context.Context) (inference.Capabilities, error) {
	return inference.Capabilities{Streaming: true, Tools: true, Reasoning: true}, nil
}

func (f *fakeProxyEngine) Shutdown(context.Context) error { return nil }

func cloneInferenceRequest(request inference.Request) inference.Request {
	request.Body = append([]byte(nil), request.Body...)
	request.Headers = cloneHeaderValues(request.Headers)
	if request.Metadata != nil {
		request.Metadata = mapsClone(request.Metadata)
	}
	return request
}

func mapsClone(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneHeaderValues(source map[string][]string) map[string][]string {
	if source == nil {
		return nil
	}
	result := make(map[string][]string, len(source))
	for key, values := range source {
		result[key] = append([]string(nil), values...)
	}
	return result
}

type fakeProxyLease struct {
	mu           sync.Mutex
	renewCalls   int
	releaseCalls int
	renewErr     error
	releaseErr   error
}

func (f *fakeProxyLease) Renew(context.Context, time.Duration) error {
	f.mu.Lock()
	f.renewCalls++
	f.mu.Unlock()
	return f.renewErr
}

func (f *fakeProxyLease) Release(context.Context) error {
	f.mu.Lock()
	f.releaseCalls++
	f.mu.Unlock()
	return f.releaseErr
}

type fakeProxyHealth struct {
	mu     sync.Mutex
	ids    []uuid.UUID
	inputs []credential.HealthInput
	err    error
}

func (f *fakeProxyHealth) RecordHealth(_ context.Context, id uuid.UUID, input credential.HealthInput) error {
	f.mu.Lock()
	f.ids = append(f.ids, id)
	f.inputs = append(f.inputs, input)
	f.mu.Unlock()
	return f.err
}

type fakeProxyRuntime struct {
	mu      sync.Mutex
	results []inference.CredentialResult
}

func (f *fakeProxyRuntime) MarkCredentialResult(_ context.Context, result inference.CredentialResult) {
	f.mu.Lock()
	f.results = append(f.results, result)
	f.mu.Unlock()
}

type fakeProxyUsage struct {
	mu            sync.Mutex
	handle        usage.RequestHandle
	beginErr      error
	finalizeErr   error
	begins        []usage.StartInput
	finalizations []usage.FinalizeInput
}

func (f *fakeProxyUsage) BeginRequest(_ context.Context, input usage.StartInput) (usage.RequestHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.begins = append(f.begins, input)
	if f.beginErr != nil {
		return usage.RequestHandle{}, f.beginErr
	}
	if f.handle.RecordID == uuid.Nil {
		f.handle = usage.RequestHandle{
			RecordID:       uuid.MustParse("aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa"),
			RequestID:      input.RequestID,
			UserID:         input.UserID,
			APIKeyID:       input.APIKeyID,
			Endpoint:       input.Endpoint,
			RequestedModel: input.RequestedModel,
			Stream:         input.Stream,
			StartedAt:      input.StartedAt,
		}
	}
	return f.handle, nil
}

func (f *fakeProxyUsage) Finalize(_ context.Context, handle usage.RequestHandle, input usage.FinalizeInput) (usage.Event, error) {
	f.mu.Lock()
	f.finalizations = append(f.finalizations, input)
	f.mu.Unlock()
	if f.finalizeErr != nil {
		return usage.Event{}, f.finalizeErr
	}
	return usage.Event{
		ID:              uuid.MustParse("bbbbbbbb-bbbb-7bbb-8bbb-bbbbbbbbbbbb"),
		RequestRecordID: handle.RecordIDPointer(),
		RequestID:       handle.RequestID,
		UserID:          uuidPointer(handle.UserID),
		APIKeyID:        uuidPointer(handle.APIKeyID),
		PriceVersionID:  cloneUUID(input.PriceVersionID),
		WalletID:        cloneUUID(input.WalletID),
		Usage:           input.Usage,
		AmountMinor:     input.AmountMinor,
		Currency:        input.Currency,
		Estimated:       input.Estimated,
		TerminalStatus:  input.TerminalStatus,
	}, nil
}

func (f *fakeProxyUsage) RecordReconciliation(_ context.Context, _ usage.ReconciliationInput) (usage.Reconciliation, error) {
	return usage.Reconciliation{}, nil
}

type fakeProxyPrices struct {
	snapshot  pricing.Snapshot
	err       error
	mu        sync.Mutex
	modelIDs  []uuid.UUID
	targetIDs []*uuid.UUID
}

func (f *fakeProxyPrices) ResolveSnapshot(_ context.Context, modelID uuid.UUID, providerModelID *uuid.UUID, _ time.Time) (pricing.Snapshot, error) {
	f.mu.Lock()
	f.modelIDs = append(f.modelIDs, modelID)
	if providerModelID == nil {
		f.targetIDs = append(f.targetIDs, nil)
	} else {
		value := *providerModelID
		f.targetIDs = append(f.targetIDs, &value)
	}
	f.mu.Unlock()
	if f.err != nil {
		return pricing.Snapshot{}, f.err
	}
	return f.snapshot, nil
}

type fakeProxyBilling struct {
	quote       billing.Quote
	quoteErr    error
	reservation billing.Reservation
	reserveErr  error
	settleErr   error

	mu           sync.Mutex
	quoteCalls   []usage.TokenUsage
	reserveCalls []billing.ReserveInput
	settleCalls  []billing.SettleInput
	releaseCalls []billing.ReleaseInput
}

func (f *fakeProxyBilling) Quote(snapshot pricing.Snapshot, observed usage.TokenUsage) (billing.Quote, error) {
	f.mu.Lock()
	f.quoteCalls = append(f.quoteCalls, observed)
	f.mu.Unlock()
	if f.quoteErr != nil {
		return billing.Quote{}, f.quoteErr
	}
	quote := f.quote
	if quote.Currency == "" {
		quote.Currency = snapshot.Currency
	}
	return quote, nil
}

func (f *fakeProxyBilling) Reserve(_ context.Context, input billing.ReserveInput) (billing.Reservation, error) {
	f.mu.Lock()
	f.reserveCalls = append(f.reserveCalls, input)
	f.mu.Unlock()
	if f.reserveErr != nil {
		return billing.Reservation{}, f.reserveErr
	}
	return f.reservation, nil
}

func (f *fakeProxyBilling) Settle(_ context.Context, input billing.SettleInput) (billing.Settlement, error) {
	f.mu.Lock()
	f.settleCalls = append(f.settleCalls, input)
	f.mu.Unlock()
	return billing.Settlement{}, f.settleErr
}

func (f *fakeProxyBilling) Release(_ context.Context, input billing.ReleaseInput) error {
	f.mu.Lock()
	f.releaseCalls = append(f.releaseCalls, input)
	f.mu.Unlock()
	return nil
}

type proxyStreamStep struct {
	event inference.StreamEvent
	err   error
}

type fakeProxyStream struct {
	steps         []proxyStreamStep
	headers       map[string][]string
	ignoreContext bool

	mu         sync.Mutex
	index      int
	closeCalls int
}

func (f *fakeProxyStream) Next(ctx context.Context) (inference.StreamEvent, error) {
	if !f.ignoreContext {
		if err := ctx.Err(); err != nil {
			return inference.StreamEvent{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.index >= len(f.steps) {
		return inference.StreamEvent{Done: true}, io.EOF
	}
	step := f.steps[f.index]
	f.index++
	return step.event, step.err
}

func (f *fakeProxyStream) Headers() map[string][]string { return cloneHeaderValues(f.headers) }

func (f *fakeProxyStream) Close() error {
	f.mu.Lock()
	f.closeCalls++
	f.mu.Unlock()
	return nil
}

func newProxyFixture(t *testing.T) (*Handler, *fakeProxyKeys, *fakeProxyCatalog, *fakeProxyRoutes, *fakeProxyScheduler, *fakeProxyEngine, *fakeProxyLease, *fakeProxyHealth, *fakeProxyRuntime) {
	t.Helper()
	userID := uuid.MustParse("11111111-1111-7111-8111-111111111111")
	keyID := uuid.MustParse("22222222-2222-7222-8222-222222222222")
	modelID := uuid.MustParse("33333333-3333-7333-8333-333333333333")
	routeID := uuid.MustParse("44444444-4444-7444-8444-444444444444")
	versionID := uuid.MustParse("55555555-5555-7555-8555-555555555555")
	targetID := uuid.MustParse("66666666-6666-7666-8666-666666666666")
	providerID := uuid.MustParse("77777777-7777-7777-8777-777777777777")
	poolID := uuid.MustParse("88888888-8888-7888-8888-888888888888")
	credentialID := uuid.MustParse("99999999-9999-7999-8999-999999999999")
	createdAt := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	capabilities := catalogmodel.Capabilities{Chat: true, Responses: true, Stream: true, Tools: true, Reasoning: true}
	publicModel := catalogmodel.Model{
		ID: modelID, PublicID: "bablo-chat", Aliases: []string{"chat-latest"},
		DisplayName: "Bablo Chat", Visibility: "public", BillingClass: "token",
		Capabilities: capabilities, Enabled: true, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	target := route.Target{
		ID: targetID, RouteVersionID: versionID, TargetNo: 0, ProviderModelID: uuid.New(),
		CredentialPoolID: poolID, ProviderID: providerID, ProviderSlug: "official-openai",
		ProviderResourceType: provider.ResourceOfficialAPI, ProviderCommercialAllowed: true,
		UpstreamModelID: "upstream-chat", Protocol: provider.ProtocolOpenAIChat,
		Capabilities: capabilities, ProviderModelEnabled: true, ReviewStatus: provider.ReviewApproved,
		PoolEnabled: true, Priority: 0, Weight: 1, EffectiveCommercialAllowed: true, Enabled: true,
	}
	resolution := route.Resolution{
		RequestedModel: "chat-latest", ModelID: modelID, ModelPublicID: publicModel.PublicID,
		Route:      route.Route{ID: routeID, ModelID: modelID, ModelPublicID: publicModel.PublicID, Enabled: true},
		Version:    route.Version{ID: versionID, RouteID: routeID, VersionNo: 1, Targets: []route.Target{target}},
		Candidates: []route.Target{target},
	}
	keys := &fakeProxyKeys{
		principal:  apikey.Principal{UserID: userID, APIKeyID: keyID, KeyPrefix: "bablo_sk_test", SecretVersion: 1},
		authorized: []string{publicModel.PublicID},
	}
	catalog := &fakeProxyCatalog{
		models: map[string]catalogmodel.Model{
			publicModel.PublicID: publicModel,
			"chat-latest":        publicModel,
		},
		pages: map[string]catalogmodel.Page{"": {Models: []catalogmodel.Model{publicModel}, NextCursor: "page-2"}, "page-2": {Models: []catalogmodel.Model{{PublicID: "private-model", Enabled: true}}, NextCursor: ""}},
	}
	routes := &fakeProxyRoutes{resolution: resolution}
	lease := &fakeProxyLease{}
	schedulerService := &fakeProxyScheduler{selection: scheduler.Selection{Target: target, CredentialID: credentialID, Lease: lease}}
	engine := &fakeProxyEngine{}
	health := &fakeProxyHealth{}
	runtime := &fakeProxyRuntime{}
	handler := &Handler{
		keys:         keys,
		models:       catalog,
		routes:       routes,
		scheduler:    schedulerService,
		engine:       engine,
		health:       health,
		runtime:      runtime,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		maxBodyBytes: defaultMaxBodyBytes,
		leaseTTL:     time.Hour,
		strategy:     scheduler.StrategyRoundRobin,
		now:          func() time.Time { return createdAt },
		principal:    func(context.Context) (apikey.Principal, bool) { return keys.principal, true },
	}
	return handler, keys, catalog, routes, schedulerService, engine, lease, health, runtime
}

func TestHandlerChatJSONRunsAuthenticatedRouteSchedulerEnginePipeline(t *testing.T) {
	handler, keys, _, routes, schedulerService, engine, lease, health, runtime := newProxyFixture(t)
	engine.result = inference.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers: map[string][]string{
			"Content-Type":               {"text/plain"},
			"Cache-Control":              {"public, max-age=60"},
			"X-RateLimit-Limit-Requests": {"10"},
			"Set-Cookie":                 {"secret=must-not-forward"},
			"X-Upstream-Secret":          {"must-not-forward"},
		},
		Body: []byte(`{"id":"chatcmpl-test","object":"chat.completion"}`),
	}
	body := `{"model":"chat-latest","stream":false,"tools":[{"type":"function"}],"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(body))
	request.Header.Set("X-Request-ID", "req-chat-json")
	request.Header.Set("Authorization", "Bearer should-not-forward")
	request.Header.Set("X-Forwarded-For", "198.51.100.10")
	request.Header.Set("X-Client-Trace", "trace-1")
	request.Header["x-request-id"] = []string{"attacker-controlled"}
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Request-ID"); got != "req-chat-json" {
		t.Fatalf("request id = %q, want req-chat-json", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	if values := recorder.Header().Values("Cache-Control"); len(values) != 1 || values[0] != "no-store" {
		t.Fatalf("cache-control = %#v, want [no-store]", values)
	}
	if got := recorder.Header().Get("X-RateLimit-Limit-Requests"); got != "10" {
		t.Fatalf("safe upstream header = %q, want 10", got)
	}
	if recorder.Header().Get("Set-Cookie") != "" || recorder.Header().Get("X-Upstream-Secret") != "" {
		t.Fatalf("unsafe upstream headers leaked: %v", recorder.Header())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response["object"] != "chat.completion" {
		t.Fatalf("response = %s, decode error = %v", recorder.Body.String(), err)
	}

	if len(keys.authorizeCalls) != 1 || keys.authorizeCalls[0].model != "bablo-chat" || keys.authorizeCalls[0].principal.APIKeyID == uuid.Nil {
		t.Fatalf("authorize calls = %#v", keys.authorizeCalls)
	}
	if len(keys.authorizeCalls) != 1 || keys.authorizeCalls[0].tokens != estimateTokens([]byte(body)) {
		t.Fatalf("authorize token estimate = %#v, want %d", keys.authorizeCalls, estimateTokens([]byte(body)))
	}
	if len(routes.calls) != 1 || routes.calls[0] != "bablo-chat" {
		t.Fatalf("route calls = %#v", routes.calls)
	}
	if len(schedulerService.calls) != 1 {
		t.Fatalf("scheduler calls = %d, want 1", len(schedulerService.calls))
	}
	selectionRequest := schedulerService.calls[0]
	if selectionRequest.RequestID != "req-chat-json" || selectionRequest.Protocol != provider.ProtocolOpenAIChat || !selectionRequest.RequiredCapabilities.Chat {
		t.Fatalf("scheduler request = %#v", selectionRequest)
	}
	if !selectionRequest.RequiredCapabilities.Tools || !selectionRequest.RequiredCapabilities.Reasoning || selectionRequest.RequiredCapabilities.Stream {
		t.Fatalf("scheduler capabilities = %#v", selectionRequest.RequiredCapabilities)
	}
	if len(engine.executeCalls) != 1 || len(engine.streamCalls) != 0 {
		t.Fatalf("engine calls = execute:%d stream:%d", len(engine.executeCalls), len(engine.streamCalls))
	}
	engineRequest := engine.executeCalls[0]
	if engineRequest.RequestID != "req-chat-json" || engineRequest.ResolvedRoute.RequestedModel != "chat-latest" || engineRequest.ResolvedRoute.ResolvedModel != "upstream-chat" || engineRequest.ResolvedRoute.ProviderID != "official-openai" || engineRequest.ResolvedRoute.CredentialID == "" {
		t.Fatalf("engine route = %#v", engineRequest.ResolvedRoute)
	}
	if _, ok := engineRequest.Headers["Authorization"]; ok {
		t.Fatalf("authorization header forwarded: %#v", engineRequest.Headers)
	}
	if _, ok := engineRequest.Headers["X-Forwarded-For"]; ok {
		t.Fatalf("forwarded header forwarded: %#v", engineRequest.Headers)
	}
	if _, ok := engineRequest.Headers["X-Client-Trace"]; ok {
		t.Fatalf("arbitrary client header forwarded: %#v", engineRequest.Headers)
	}
	if got := engineRequest.Headers["X-Request-ID"]; len(got) != 1 || got[0] != "req-chat-json" {
		t.Fatalf("engine request id header = %#v", got)
	}
	if _, ok := engineRequest.Headers["x-request-id"]; ok {
		t.Fatalf("lowercase request id header forwarded: %#v", engineRequest.Headers)
	}
	if lease.releaseCalls != 1 {
		t.Fatalf("lease releases = %d, want 1", lease.releaseCalls)
	}
	if len(runtime.results) != 1 || !runtime.results[0].Succeeded || len(health.inputs) != 1 || !health.inputs[0].Succeeded {
		t.Fatalf("health/runtime success = %#v / %#v", health.inputs, runtime.results)
	}
}
func TestHandlerRecordsResolvedUsageFact(t *testing.T) {
	handler, _, _, _, schedulerService, engine, _, _, _ := newProxyFixture(t)
	recorder := &fakeProxyUsage{}
	handler.usage = recorder
	engine.result = inference.ExecutionResult{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"id":"chatcmpl-usage","usage":{"prompt_tokens":12,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":3}}}`),
	}
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat"}`))
	request.Header.Set("X-Request-ID", "req-usage-json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.begins) != 1 || recorder.begins[0].RequestID != "req-usage-json" {
		t.Fatalf("usage begins = %+v", recorder.begins)
	}
	if len(recorder.finalizations) != 1 {
		t.Fatalf("usage finalizations = %d, want 1", len(recorder.finalizations))
	}
	finalization := recorder.finalizations[0]
	if finalization.TerminalStatus != usage.StatusSucceeded || finalization.Usage != (usage.TokenUsage{InputTokens: 12, OutputTokens: 8, CacheReadTokens: 3}) {
		t.Fatalf("usage finalization = %+v", finalization)
	}
	if finalization.ResolvedModelID == nil || *finalization.ResolvedModelID != uuid.MustParse("33333333-3333-7333-8333-333333333333") || finalization.CredentialID == nil {
		t.Fatalf("resolved route in usage = %+v", finalization)
	}
	if len(schedulerService.calls) != 1 || schedulerService.calls[0].RequestRecordID == nil || *schedulerService.calls[0].RequestRecordID != recorder.handle.RecordID {
		t.Fatalf("scheduler request record id = %+v", schedulerService.calls)
	}
}
func TestHandlerMarksPartialUsageForReconciliation(t *testing.T) {
	handler, _, _, _, _, engine, _, _, _ := newProxyFixture(t)
	recorder := &fakeProxyUsage{}
	handler.usage = recorder
	engine.result = inference.ExecutionResult{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"id":"chatcmpl-partial","usage":{"completion_tokens":8}}`),
	}
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat"}`))
	request.Header.Set("X-Request-ID", "req-partial-usage")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.finalizations) != 1 {
		t.Fatalf("finalizations = %d, want 1", len(recorder.finalizations))
	}
	finalization := recorder.finalizations[0]
	if finalization.TerminalStatus != usage.StatusReconcileNeeded || finalization.Provenance != usage.ProvenanceMissingUsage || finalization.Usage != (usage.TokenUsage{}) {
		t.Fatalf("partial usage finalization = %+v", finalization)
	}
}
func TestHandlerBindsResolvedPriceSnapshotAndSettlesUsage(t *testing.T) {
	handler, _, _, _, schedulerService, engine, _, _, _ := newProxyFixture(t)
	recorder := &fakeProxyUsage{}
	handler.usage = recorder
	priceID := uuid.MustParse("aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaab")
	walletID := uuid.MustParse("cccccccc-cccc-7ccc-8ccc-cccccccccccc")
	reservationID := uuid.MustParse("dddddddd-dddd-7ddd-8ddd-dddddddddddd")
	prices := &fakeProxyPrices{snapshot: pricing.Snapshot{
		VersionID: priceID,
		Scope:     pricing.ScopeProviderModel,
		Currency:  "USD",
		Prices:    map[string]string{pricing.DimensionInputToken: "1"},
	}}
	coordinator := &fakeProxyBilling{
		quote: billing.Quote{AmountMinor: 7, Currency: "USD"},
		reservation: billing.Reservation{
			ID: reservationID, WalletID: walletID, AmountMinor: 9,
			Currency: "USD", Status: billing.ReservationReserved,
		},
	}
	handler.prices = prices
	handler.billing = coordinator
	engine.result = inference.ExecutionResult{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"id":"chatcmpl-price","usage":{"prompt_tokens":2,"completion_tokens":1}}`),
	}
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat","max_completion_tokens":16}`))
	request.Header.Set("X-Request-ID", "req-price-snapshot")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	prices.mu.Lock()
	if len(prices.modelIDs) != 1 || prices.modelIDs[0] != uuid.MustParse("33333333-3333-7333-8333-333333333333") {
		t.Fatalf("price model calls = %#v", prices.modelIDs)
	}
	if len(prices.targetIDs) != 1 || prices.targetIDs[0] == nil || *prices.targetIDs[0] != schedulerService.selection.Target.ProviderModelID {
		t.Fatalf("price target calls = %#v", prices.targetIDs)
	}
	prices.mu.Unlock()
	recorder.mu.Lock()
	if len(recorder.finalizations) != 1 {
		t.Fatalf("finalizations = %d, want 1", len(recorder.finalizations))
	}
	finalization := recorder.finalizations[0]
	recorder.mu.Unlock()
	if finalization.PriceVersionID == nil || *finalization.PriceVersionID != priceID || finalization.WalletID == nil || *finalization.WalletID != walletID || finalization.AmountMinor == nil || *finalization.AmountMinor != 7 || finalization.Currency != "USD" {
		t.Fatalf("priced usage finalization = %+v", finalization)
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if len(coordinator.reserveCalls) != 1 || coordinator.reserveCalls[0].EstimatedUsage.OutputTokens != 16 || coordinator.reserveCalls[0].Price.VersionID != priceID {
		t.Fatalf("reserve calls = %+v", coordinator.reserveCalls)
	}
	if len(coordinator.settleCalls) != 1 || coordinator.settleCalls[0].ReservationID != reservationID || coordinator.settleCalls[0].Event.AmountMinor == nil || *coordinator.settleCalls[0].Event.AmountMinor != 7 {
		t.Fatalf("settle calls = %+v", coordinator.settleCalls)
	}
}

func TestHandlerRejectsInsufficientFundsBeforeUpstream(t *testing.T) {
	handler, _, _, _, _, engine, lease, _, _ := newProxyFixture(t)
	recorder := &fakeProxyUsage{}
	priceID := uuid.MustParse("aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaac")
	handler.usage = recorder
	handler.prices = &fakeProxyPrices{snapshot: pricing.Snapshot{
		VersionID: priceID,
		Currency:  "USD",
		Prices:    map[string]string{pricing.DimensionInputToken: "1"},
	}}
	handler.billing = &fakeProxyBilling{
		quote:      billing.Quote{Currency: "USD"},
		reserveErr: billing.ErrInsufficientFunds,
	}
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat"}`))
	request.Header.Set("X-Request-ID", "req-no-funds")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusPaymentRequired || !strings.Contains(response.Body.String(), `"code":"insufficient_funds"`) {
		t.Fatalf("insufficient funds response = %d body=%s", response.Code, response.Body.String())
	}
	if len(engine.executeCalls) != 0 || len(engine.streamCalls) != 0 {
		t.Fatalf("insufficient funds reached upstream: execute=%d stream=%d", len(engine.executeCalls), len(engine.streamCalls))
	}
	if lease.releaseCalls != 1 {
		t.Fatalf("scheduler lease releases = %d, want 1", lease.releaseCalls)
	}
}

func TestHandlerSettlesReservedEstimateWhenUsageIsMissing(t *testing.T) {
	handler, _, _, _, _, engine, _, _, _ := newProxyFixture(t)
	recorder := &fakeProxyUsage{}
	priceID := uuid.MustParse("aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaad")
	walletID := uuid.MustParse("cccccccc-cccc-7ccc-8ccc-cccccccccccd")
	reservationID := uuid.MustParse("dddddddd-dddd-7ddd-8ddd-ddddddddddde")
	handler.usage = recorder
	handler.prices = &fakeProxyPrices{snapshot: pricing.Snapshot{
		VersionID: priceID,
		Currency:  "USD",
		Prices:    map[string]string{pricing.DimensionInputToken: "1"},
	}}
	coordinator := &fakeProxyBilling{
		reservation: billing.Reservation{
			ID: reservationID, WalletID: walletID, AmountMinor: 11,
			Currency: "USD", Status: billing.ReservationReserved,
		},
	}
	handler.billing = coordinator
	engine.result = inference.ExecutionResult{StatusCode: http.StatusOK, Body: []byte(`{"id":"chatcmpl-missing"}`)}
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat"}`))
	request.Header.Set("X-Request-ID", "req-estimated-billing")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	recorder.mu.Lock()
	if len(recorder.finalizations) != 1 {
		t.Fatalf("finalizations = %d, want 1", len(recorder.finalizations))
	}
	finalization := recorder.finalizations[0]
	recorder.mu.Unlock()
	if !finalization.Estimated || finalization.AmountMinor == nil || *finalization.AmountMinor != 11 || finalization.TerminalStatus != usage.StatusReconcileNeeded {
		t.Fatalf("estimated finalization = %+v", finalization)
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if len(coordinator.settleCalls) != 1 || coordinator.settleCalls[0].Event.AmountMinor == nil || *coordinator.settleCalls[0].Event.AmountMinor != 11 {
		t.Fatalf("estimated settle calls = %+v", coordinator.settleCalls)
	}
}

func TestHandlerRecordsStreamUsageAndTTFT(t *testing.T) {
	handler, _, _, _, _, engine, _, _, _ := newProxyFixture(t)
	recorder := &fakeProxyUsage{}
	handler.usage = recorder
	engine.stream = &fakeProxyStream{steps: []proxyStreamStep{
		{event: inference.StreamEvent{Payload: []byte(`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}

`)}},
		{event: inference.StreamEvent{Payload: []byte(`data: [DONE]

`), Done: true}},
	}}
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat","stream":true}`))
	request.Header.Set("X-Request-ID", "req-usage-stream")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "[DONE]") {
		t.Fatalf("stream response = %d %q", response.Code, response.Body.String())
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.finalizations) != 1 {
		t.Fatalf("usage finalizations = %d, want 1", len(recorder.finalizations))
	}
	finalization := recorder.finalizations[0]
	if finalization.TerminalStatus != usage.StatusSucceeded || finalization.Usage.InputTokens != 5 || finalization.Usage.OutputTokens != 2 || finalization.TTFT == nil {
		t.Fatalf("stream usage finalization = %+v", finalization)
	}
}

func TestHandlerFinalizesCancelledStreamWithoutUsage(t *testing.T) {
	handler, _, _, _, _, engine, _, _, _ := newProxyFixture(t)
	recorder := &fakeProxyUsage{}
	handler.usage = recorder
	engine.stream = &fakeProxyStream{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat","stream":true}`)).WithContext(ctx)
	request.Header.Set("X-Request-ID", "req-usage-cancel")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.finalizations) != 1 || recorder.finalizations[0].TerminalStatus != usage.StatusCancelled || recorder.finalizations[0].Provenance != usage.ProvenanceMissingUsage {
		t.Fatalf("cancelled usage finalization = %+v", recorder.finalizations)
	}
}

func TestFilteredRequestHeadersUsesExplicitAllowlist(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("User-Agent", "bablo-test-client")
	request.Header.Set("Traceparent", "00-abc-def-01")
	request.Header.Set("OpenAI-Beta", "responses=v1")
	request.Header.Set("X-Client-Trace", "must-not-forward")
	request.Header.Set("X-Forwarded-For", "198.51.100.10")
	request.Header.Set("Authorization", "Bearer must-not-forward")

	got := filteredRequestHeaders(request, "req-allowlist")
	for _, key := range []string{"Content-Type", "Accept", "User-Agent", "Traceparent", "OpenAI-Beta"} {
		key = http.CanonicalHeaderKey(key)
		if len(got[key]) == 0 {
			t.Fatalf("allowed header %q missing from %#v", key, got)
		}
	}
	if len(got["X-Request-ID"]) != 1 || got["X-Request-ID"][0] != "req-allowlist" {
		t.Fatalf("request id header = %#v", got["X-Request-ID"])
	}
	for _, key := range []string{"X-Client-Trace", "X-Forwarded-For", "Authorization"} {
		key = http.CanonicalHeaderKey(key)
		if _, ok := got[key]; ok {
			t.Fatalf("unapproved header %q forwarded: %#v", key, got)
		}
	}
}

func TestHandlerDoesNotMarkCredentialFailureOnClientCancellation(t *testing.T) {
	handler, _, _, _, _, engine, lease, health, runtime := newProxyFixture(t)
	engine.executeErr = context.Canceled
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat"}`))
	request = request.WithContext(request.Context())
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != 499 || !strings.Contains(recorder.Body.String(), `"code":"client_cancelled"`) {
		t.Fatalf("cancelled non-stream response = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if lease.releaseCalls != 1 {
		t.Fatalf("lease releases = %d, want 1", lease.releaseCalls)
	}
	if len(health.inputs) != 0 || len(runtime.results) != 0 {
		t.Fatalf("client cancellation reported as credential failure: health=%#v runtime=%#v", health.inputs, runtime.results)
	}
}

func TestHandlerResponsesJSONUsesResponsesContract(t *testing.T) {
	handler, _, _, _, schedulerService, engine, lease, health, runtime := newProxyFixture(t)
	engine.result = inference.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    map[string][]string{"Content-Type": {"application/json"}},
		Body:       []byte(`{"id":"resp_test","object":"response","output":[]}`),
	}
	request := httptest.NewRequest(http.MethodPost, responsesPath, strings.NewReader(`{"model":"chat-latest","input":"hello"}`))
	request.Header.Set("X-Request-ID", "req-responses-json")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("responses JSON = %d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response["object"] != "response" {
		t.Fatalf("responses JSON body = %s, decode error = %v", recorder.Body.String(), err)
	}
	if len(schedulerService.calls) != 1 {
		t.Fatalf("scheduler calls = %d, want 1", len(schedulerService.calls))
	}
	selectionRequest := schedulerService.calls[0]
	if selectionRequest.Protocol != provider.ProtocolOpenAIResponses || !selectionRequest.RequiredCapabilities.Responses || selectionRequest.RequiredCapabilities.Stream {
		t.Fatalf("responses scheduler request = %#v", selectionRequest)
	}
	if len(engine.executeCalls) != 1 || engine.executeCalls[0].SourceFormat != "openai-response" || engine.executeCalls[0].ResponseFormat != "openai-response" || engine.executeCalls[0].Stream {
		t.Fatalf("responses engine request = %#v", engine.executeCalls)
	}
	if lease.releaseCalls != 1 || len(runtime.results) != 1 || !runtime.results[0].Succeeded || len(health.inputs) != 1 || !health.inputs[0].Succeeded {
		t.Fatalf("responses completion state lease=%d runtime=%#v health=%#v", lease.releaseCalls, runtime.results, health.inputs)
	}
}
func TestHandlerModelsListsOnlyFreshlyAuthorizedModelsAcrossPages(t *testing.T) {

	handler, keys, catalog, _, _, _, _, _, _ := newProxyFixture(t)
	other := catalogmodel.Model{PublicID: "other-model", Enabled: true, CreatedAt: time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)}
	catalog.pages = map[string]catalogmodel.Page{
		"":     {Models: []catalogmodel.Model{other}, NextCursor: "next"},
		"next": {Models: []catalogmodel.Model{{PublicID: "bablo-chat", CreatedAt: time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)}}, NextCursor: ""},
	}
	keys.authorized = []string{"bablo-chat", "missing-from-catalog"}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, modelsPath, nil)
	request.Header.Set("X-Request-ID", "req-models")

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Object string        `json:"object"`
		Data   []openAIModel `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode models = %v; body=%s", err, recorder.Body.String())
	}
	if response.Object != "list" || len(response.Data) != 1 || response.Data[0].ID != "bablo-chat" || response.Data[0].Object != "model" || response.Data[0].OwnedBy != "bablo" {
		t.Fatalf("models response = %#v", response)
	}
	if len(catalog.listArgs) != 2 || catalog.listArgs[0] != "" || catalog.listArgs[1] != "next" {
		t.Fatalf("catalog cursors = %#v", catalog.listArgs)
	}
	if len(keys.listCalls) != 1 || keys.listCalls[0].APIKeyID == uuid.Nil {
		t.Fatalf("authorized model calls = %#v", keys.listCalls)
	}
}

func TestHandlerMapsNonStreamingUpstreamErrorAndReleasesLease(t *testing.T) {
	handler, _, _, _, _, engine, lease, health, runtime := newProxyFixture(t)
	engine.executeErr = &inference.UpstreamError{Class: "rate_limit", HTTPStatus: http.StatusTooManyRequests, Retryable: true}
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat"}`))
	request.Header.Set("X-Request-ID", "req-upstream-rate")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "60" || !strings.Contains(recorder.Body.String(), `"code":"upstream_rate_limit"`) {
		t.Fatalf("upstream error = %d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	if lease.releaseCalls != 1 || len(runtime.results) != 1 || runtime.results[0].Succeeded || runtime.results[0].ErrorClass != "rate_limit" || len(health.inputs) != 1 || health.inputs[0].Succeeded {
		t.Fatalf("upstream failure state lease=%d runtime=%#v health=%#v", lease.releaseCalls, runtime.results, health.inputs)
	}
	if runtime.results[0].CooldownUntil == nil || health.inputs[0].CooldownUntil == nil {
		t.Fatal("rate limit failure did not produce cooldown observation")
	}
}

func TestHandlerResponsesStreamStopsAtCompletedEventAndPreservesSSE(t *testing.T) {
	handler, _, _, _, schedulerService, engine, lease, health, runtime := newProxyFixture(t)
	engine.stream = &fakeProxyStream{steps: []proxyStreamStep{
		{event: inference.StreamEvent{Payload: []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")}},
		{event: inference.StreamEvent{Payload: []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")}},
	}}
	body := `{"model":"bablo-chat","stream":true,"input":"hello"}`
	request := httptest.NewRequest(http.MethodPost, responsesPath, strings.NewReader(body))
	request.Header.Set("X-Request-ID", "req-responses-stream")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q, want text/event-stream", got)
	}
	if values := recorder.Header().Values("Cache-Control"); len(values) != 1 || values[0] != "no-store" {
		t.Fatalf("SSE cache-control = %#v, want [no-store]", values)
	}
	bodyText := recorder.Body.String()
	if !strings.Contains(bodyText, "response.output_text.delta") || !strings.Contains(bodyText, "response.completed") || strings.Contains(bodyText, "upstream_protocol_error") {
		t.Fatalf("responses SSE body = %q", bodyText)
	}
	if len(schedulerService.calls) != 1 {
		t.Fatalf("scheduler calls = %d, want 1", len(schedulerService.calls))
	}
	selectionRequest := schedulerService.calls[0]
	if selectionRequest.Protocol != provider.ProtocolOpenAIResponses || !selectionRequest.RequiredCapabilities.Responses || !selectionRequest.RequiredCapabilities.Stream {
		t.Fatalf("responses scheduler request = %#v", selectionRequest)
	}
	if len(engine.streamCalls) != 1 || engine.streamCalls[0].ResponseFormat != "openai-response" || !engine.streamCalls[0].Stream {
		t.Fatalf("responses engine request = %#v", engine.streamCalls)
	}
	if lease.releaseCalls != 1 || len(runtime.results) != 1 || !runtime.results[0].Succeeded || len(health.inputs) != 1 || !health.inputs[0].Succeeded {
		t.Fatalf("stream completion state lease=%d runtime=%#v health=%#v", lease.releaseCalls, runtime.results, health.inputs)
	}
}

func TestHandlerChatStreamRejectsEOFWithoutDoneMarker(t *testing.T) {
	handler, _, _, _, _, engine, _, health, runtime := newProxyFixture(t)
	engine.stream = &fakeProxyStream{steps: []proxyStreamStep{
		{event: inference.StreamEvent{Payload: []byte(`{"id":"partial"}`)}},
		{err: io.EOF},
	}}
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat","stream":true}`))
	request.Header.Set("X-Request-ID", "req-incomplete")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "upstream_protocol_error") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("incomplete stream body = %q", body)
	}
	if len(runtime.results) != 1 || runtime.results[0].Succeeded || runtime.results[0].ErrorClass != "upstream_protocol_error" || len(health.inputs) != 1 || health.inputs[0].Succeeded {
		t.Fatalf("incomplete result runtime=%#v health=%#v", runtime.results, health.inputs)
	}
}

func TestHandlerStreamMapsPreFirstErrorAndClientCancellation(t *testing.T) {
	t.Run("upstream error before first payload", func(t *testing.T) {
		handler, _, _, _, _, engine, _, _, _ := newProxyFixture(t)
		engine.stream = &fakeProxyStream{steps: []proxyStreamStep{{err: &inference.UpstreamError{Class: "rate_limit", HTTPStatus: http.StatusTooManyRequests}}}}
		request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat","stream":true}`))
		recorder := httptest.NewRecorder()

		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "60" || !strings.Contains(recorder.Body.String(), `"code":"upstream_rate_limit"`) {
			t.Fatalf("pre-first error = %d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
		}
	})

	t.Run("client cancellation before first payload", func(t *testing.T) {
		handler, _, _, _, _, engine, lease, _, _ := newProxyFixture(t)
		engine.stream = &fakeProxyStream{steps: []proxyStreamStep{{err: context.Canceled}}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat","stream":true}`)).WithContext(ctx)
		recorder := httptest.NewRecorder()

		handler.ServeHTTP(recorder, request)

		if recorder.Code != 499 || !strings.Contains(recorder.Body.String(), `"code":"client_cancelled"`) {
			t.Fatalf("cancelled response = %d body=%s", recorder.Code, recorder.Body.String())
		}
		if lease.releaseCalls != 1 {
			t.Fatalf("cancelled lease releases = %d, want 1", lease.releaseCalls)
		}
	})
}

func TestHandlerTreatsCancelledRequestWithEOFAsClientCancellation(t *testing.T) {
	handler, _, _, _, _, engine, lease, _, _ := newProxyFixture(t)
	engine.stream = &fakeProxyStream{ignoreContext: true, steps: []proxyStreamStep{{err: io.EOF}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat","stream":true}`)).WithContext(ctx)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != 499 || !strings.Contains(recorder.Body.String(), `"code":"client_cancelled"`) {
		t.Fatalf("cancelled EOF response = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if lease.releaseCalls != 1 {
		t.Fatalf("cancelled EOF lease releases = %d, want 1", lease.releaseCalls)
	}
}

func TestHandlerPostFirstStreamErrorEmitsStructuredSSEError(t *testing.T) {
	handler, _, _, _, _, engine, _, health, runtime := newProxyFixture(t)
	engine.stream = &fakeProxyStream{steps: []proxyStreamStep{
		{event: inference.StreamEvent{Payload: []byte(`{"choices":[{"delta":{"content":"partial"}}]}`)}},
		{err: &inference.UpstreamError{Class: "server", HTTPStatus: http.StatusBadGateway}},
	}}
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat","stream":true}`))
	request.Header.Set("X-Request-ID", "req-post-first-error")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"code":"upstream_error"`) || !strings.Contains(body, "data: [DONE]") || !strings.Contains(body, "partial") {
		t.Fatalf("post-first error SSE = %q", body)
	}
	if len(runtime.results) != 1 || runtime.results[0].Succeeded || len(health.inputs) != 1 || health.inputs[0].Succeeded {
		t.Fatalf("post-first health/runtime = %#v / %#v", health.inputs, runtime.results)
	}
}

func TestHandlerRejectsInvalidMethodsPathsAndBodies(t *testing.T) {
	handler, _, _, _, _, engine, _, _, _ := newProxyFixture(t)

	t.Run("method", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, modelsPath, nil))
		if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodGet || !strings.Contains(recorder.Body.String(), `"code":"method_not_allowed"`) {
			t.Fatalf("method response = %d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
		}
	})

	t.Run("unknown path", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/unknown", nil))
		if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), `"code":"not_found"`) {
			t.Fatalf("unknown path = %d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("invalid body", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":`)))
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"invalid_request"`) {
			t.Fatalf("invalid body = %d body=%s", recorder.Code, recorder.Body.String())
		}
		if len(engine.executeCalls) != 0 || len(engine.streamCalls) != 0 {
			t.Fatalf("invalid body reached engine: execute=%d stream=%d", len(engine.executeCalls), len(engine.streamCalls))
		}
	})

	t.Run("invalid max output tokens", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat","max_tokens":0}`)))
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"invalid_request"`) {
			t.Fatalf("invalid max tokens = %d body=%s", recorder.Code, recorder.Body.String())
		}
		if len(engine.executeCalls) != 0 || len(engine.streamCalls) != 0 {
			t.Fatalf("invalid max tokens reached engine: execute=%d stream=%d", len(engine.executeCalls), len(engine.streamCalls))
		}
	})
}

func TestNewHandlerRequiresAuthorizerAndInvokesIdentityMiddleware(t *testing.T) {
	keys := &fakeProxyKeys{}
	catalog := &fakeProxyCatalog{}
	handler, err := NewHandler(Options{APIKeys: keys, Models: catalog})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	if handler == nil {
		t.Fatal("NewHandler() returned nil handler")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, modelsPath, nil))
	if keys.middlewareCalls != 1 || recorder.Code != http.StatusUnauthorized {
		t.Fatalf("middleware calls=%d status=%d body=%s", keys.middlewareCalls, recorder.Code, recorder.Body.String())
	}
	if _, err := NewHandler(Options{Models: catalog}); err == nil {
		t.Fatal("NewHandler() accepted missing API key authorizer")
	}
	if _, err := NewHandler(Options{APIKeys: keys}); err == nil {
		t.Fatal("NewHandler() accepted missing model catalog")
	}
	if _, err := NewHandler(Options{APIKeys: keys, Models: catalog, Engine: &fakeProxyEngine{}}); err == nil {
		t.Fatal("NewHandler() accepted inference engine without usage recorder")
	}
	if _, err := NewHandler(Options{APIKeys: keys, Models: catalog, Engine: &fakeProxyEngine{}, UsageRecorder: &fakeProxyUsage{}}); err == nil {
		t.Fatal("NewHandler() accepted inference engine without price resolver")
	}
	if _, err := NewHandler(Options{
		APIKeys: keys, Models: catalog, Engine: &fakeProxyEngine{},
		UsageRecorder: &fakeProxyUsage{}, PriceResolver: &fakeProxyPrices{},
	}); err == nil {
		t.Fatal("NewHandler() accepted inference engine without billing coordinator")
	}
	if _, err := NewHandler(Options{
		APIKeys: keys, Models: catalog, Engine: &fakeProxyEngine{},
		UsageRecorder: &fakeProxyUsage{}, PriceResolver: &fakeProxyPrices{}, Billing: &fakeProxyBilling{},
	}); err != nil {
		t.Fatalf("NewHandler() rejected complete inference accounting dependencies: %v", err)
	}
}

func TestSSEFramingDoesNotMutatePayloadAndParsesDataEventType(t *testing.T) {
	raw := []byte(`{"type":"response.completed"}`)
	before := append([]byte(nil), raw...)
	framed := frameSSEPayload(raw)
	if !bytes.Equal(raw, before) {
		t.Fatalf("frameSSEPayload mutated input: %q", raw)
	}
	if string(framed) != "data: {\"type\":\"response.completed\"}\n\n" {
		t.Fatalf("framed raw JSON = %q", framed)
	}
	sse := []byte("event: message\ndata: {\"type\":\"response.completed\"}\n\n")
	if got := responseEventType(sse); got != "response.completed" {
		t.Fatalf("responseEventType = %q, want response.completed", got)
	}
	if got := string(frameSSEPayload([]byte("data: x\n\n"))); got != "data: x\n\n" {
		t.Fatalf("existing SSE framing = %q", got)
	}
}

func TestMapPublicErrorUsesStableSentinels(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "invalid", err: errInvalidRequest, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "method", err: errMethodNotAllowed, status: http.StatusMethodNotAllowed, code: "method_not_allowed"},
		{name: "not found", err: errProxyNotFound, status: http.StatusNotFound, code: "not_found"},
		{name: "unavailable", err: errInferenceUnavailable, status: http.StatusServiceUnavailable, code: "inference_unavailable"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			mapped := mapPublicError(test.err)
			if mapped.status != test.status || mapped.code != test.code {
				t.Fatalf("mapped = %#v, want status=%d code=%s", mapped, test.status, test.code)
			}
		})
	}
	if errors.Is(errProxyNotFound, errMethodNotAllowed) {
		t.Fatal("proxy sentinels unexpectedly alias each other")
	}
}

func TestHandlerGeneratesOneStableRequestIDWithoutClientHeader(t *testing.T) {
	handler, _, _, _, _, engine, _, _, _ := newProxyFixture(t)
	engine.result = inference.ExecutionResult{StatusCode: http.StatusOK, Body: []byte(`{"ok":true}`)}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat"}`))

	handler.ServeHTTP(recorder, request)

	responseID := recorder.Header().Get("X-Request-ID")
	if responseID == "" || len(engine.executeCalls) != 1 || engine.executeCalls[0].RequestID != responseID {
		t.Fatalf("request IDs response=%q engine=%#v", responseID, engine.executeCalls)
	}
}

func TestHandlerMapsRequestBodyLimitTo413(t *testing.T) {
	handler, _, _, _, _, engine, _, _, _ := newProxyFixture(t)
	handler.maxBodyBytes = 8
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, chatCompletionsPath, strings.NewReader(`{"model":"bablo-chat"}`))

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusRequestEntityTooLarge || !strings.Contains(recorder.Body.String(), `"code":"request_too_large"`) {
		t.Fatalf("oversized request = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(engine.executeCalls) != 0 || len(engine.streamCalls) != 0 {
		t.Fatalf("oversized request reached engine: execute=%d stream=%d", len(engine.executeCalls), len(engine.streamCalls))
	}
}
