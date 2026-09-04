package quota

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type quotaTestPersistence struct {
	mu           sync.Mutex
	observations []Observation
	states       map[uuid.UUID]ProbeState
	refs         map[uuid.UUID]ProbeRequest
	snapshots    map[uuid.UUID][]Snapshot
	due          []DueCredential
	persistErr   error
	upsertErr    error
	getStateErr  error
	listDueErr   error
	getRefErr    error
	listSnapErr  error
}

func newQuotaTestPersistence() *quotaTestPersistence {
	return &quotaTestPersistence{
		states:    make(map[uuid.UUID]ProbeState),
		refs:      make(map[uuid.UUID]ProbeRequest),
		snapshots: make(map[uuid.UUID][]Snapshot),
	}
}

func (p *quotaTestPersistence) PersistObservation(_ context.Context, request ProbeRequest, observation Observation, state ProbeState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.persistErr != nil {
		return p.persistErr
	}
	p.observations = append(p.observations, cloneObservationForTest(observation))
	p.states[state.CredentialID] = state
	return nil
}

func (p *quotaTestPersistence) UpsertProbeState(_ context.Context, state ProbeState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.upsertErr != nil {
		return p.upsertErr
	}
	p.states[state.CredentialID] = state
	return nil
}

func (p *quotaTestPersistence) GetProbeState(_ context.Context, credentialID uuid.UUID) (ProbeState, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.getStateErr != nil {
		return ProbeState{}, false, p.getStateErr
	}
	state, ok := p.states[credentialID]
	return state, ok, nil
}

func (p *quotaTestPersistence) ListSnapshots(_ context.Context, credentialID uuid.UUID, _ string, limit int) ([]Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listSnapErr != nil {
		return nil, p.listSnapErr
	}
	values := append([]Snapshot(nil), p.snapshots[credentialID]...)
	if len(values) > limit {
		values = values[:limit]
	}
	return values, nil
}

func (p *quotaTestPersistence) GetCredentialRef(_ context.Context, credentialID uuid.UUID) (ProbeRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.getRefErr != nil {
		return ProbeRequest{}, p.getRefErr
	}
	request, ok := p.refs[credentialID]
	if !ok {
		return ProbeRequest{}, ErrNotFound
	}
	return request, nil
}

func (p *quotaTestPersistence) ListDue(_ context.Context, _ time.Time, _ int) ([]DueCredential, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listDueErr != nil {
		return nil, p.listDueErr
	}
	return append([]DueCredential(nil), p.due...), nil
}

func cloneObservationForTest(value Observation) Observation {
	value.Metadata = cloneStringMap(value.Metadata)
	originalWindows := value.Windows
	value.Windows = make([]Window, len(originalWindows))
	for index, window := range originalWindows {
		value.Windows[index] = cloneWindow(window)
	}
	return value
}

func cloneHeadersForTest(source map[string][]string) map[string][]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string][]string, len(source))
	for key, values := range source {
		result[key] = append([]string(nil), values...)
	}
	return result
}

type quotaTestProbe struct {
	name     string
	provider string
	probe    func(context.Context, ProbeRequest) (Observation, error)
}

func (p *quotaTestProbe) Name() string { return p.name }
func (p *quotaTestProbe) Supports(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), p.provider)
}
func (p *quotaTestProbe) Probe(ctx context.Context, request ProbeRequest) (Observation, error) {
	return p.probe(ctx, request)
}

type quotaTestObserver struct {
	name     string
	provider string
	observe  func(context.Context, ProbeRequest, map[string][]string, time.Time) (Observation, error)
}

func (o *quotaTestObserver) Name() string { return o.name }
func (o *quotaTestObserver) Supports(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), o.provider)
}
func (o *quotaTestObserver) ObserveResponse(ctx context.Context, request ProbeRequest, headers map[string][]string, observedAt time.Time) (Observation, error) {
	return o.observe(ctx, request, headers, observedAt)
}

type quotaTestHealthProbe struct {
	name     string
	provider string
	probe    func(context.Context, ProbeRequest) error
}

func (p *quotaTestHealthProbe) Name() string { return p.name }
func (p *quotaTestHealthProbe) Supports(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), p.provider)
}
func (p *quotaTestHealthProbe) Probe(ctx context.Context, request ProbeRequest) error {
	return p.probe(ctx, request)
}

func quotaTestService(t *testing.T, persistence *quotaTestPersistence, now time.Time) *Service {
	t.Helper()
	service, err := NewService(persistence, Options{
		Locker:             NewMemoryLocker(),
		PollInterval:       time.Minute,
		ProbeTimeout:       time.Second,
		LeaseTTL:           10 * time.Second,
		MaxBackoff:         10 * time.Minute,
		AuthBackoff:        time.Hour,
		UnsupportedBackoff: time.Hour,
		SnapshotMaxAge:     2 * time.Minute,
		BatchSize:          4,
		JitterRatio:        0,
		Now:                func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

func TestRecordObservationNormalizesAndAdvancesState(t *testing.T) {
	persistence := newQuotaTestPersistence()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	service := quotaTestService(t, persistence, now)
	credentialID := uuid.New()
	used := int64(12)
	if err := service.RecordObservation(context.Background(), ProbeRequest{
		CredentialID: credentialID, ProviderSlug: " CODEX ", Model: "gpt-5", ObservationKey: "request-1",
	}, Observation{
		Source:   "test",
		Windows:  []Window{{Kind: "MINUTE", UsedTokens: &used, Metadata: map[string]string{"signal": "ok"}}},
		Metadata: map[string]string{"source": "fixture"},
	}); err != nil {
		t.Fatalf("RecordObservation() error = %v", err)
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.observations) != 1 {
		t.Fatalf("persisted observations = %d, want 1", len(persistence.observations))
	}
	observation := persistence.observations[0]
	if observation.Confidence != ConfidenceUnknown || !observation.ObservedAt.Equal(now) || observation.ObservationKey != "request-1" {
		t.Fatalf("normalized observation = %+v", observation)
	}
	if observation.Windows[0].Kind != WindowMinute || observation.Windows[0].UsedTokens == nil || *observation.Windows[0].UsedTokens != used {
		t.Fatalf("normalized window = %+v", observation.Windows[0])
	}
	state := persistence.states[credentialID]
	if state.Status != ProbeStatusSuccess || state.LastObservedAt == nil || !state.LastObservedAt.Equal(now) || state.NextAttemptAt == nil {
		t.Fatalf("probe state = %+v", state)
	}
}

func TestRecordObservationRejectsUnsafeAndDuplicateFacts(t *testing.T) {
	persistence := newQuotaTestPersistence()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	service := quotaTestService(t, persistence, now)
	request := ProbeRequest{CredentialID: uuid.New(), ProviderSlug: "codex", Model: "gpt-5", ObservationKey: "request-2"}
	unsafe := Observation{Source: "test", ErrorClass: strings.Repeat("x", 81), Windows: []Window{{Kind: WindowMinute}}}
	if err := service.RecordObservation(context.Background(), request, unsafe); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unsafe ErrorClass error = %v, want ErrInvalidInput", err)
	}
	duplicate := Observation{Source: "test", Windows: []Window{{Kind: WindowMinute}, {Kind: WindowMinute}}}
	if err := service.RecordObservation(context.Background(), request, duplicate); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate windows error = %v, want ErrConflict", err)
	}
	if len(persistence.observations) != 0 {
		t.Fatalf("invalid observations were persisted: %d", len(persistence.observations))
	}
}

func TestObserveResponsePersistsSignalsAndSkipsNoObservation(t *testing.T) {
	persistence := newQuotaTestPersistence()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	service := quotaTestService(t, persistence, now)
	var seenHeaders map[string][]string
	observer := &quotaTestObserver{
		name: "codex-headers", provider: "codex",
		observe: func(_ context.Context, _ ProbeRequest, headers map[string][]string, observedAt time.Time) (Observation, error) {
			seenHeaders = cloneHeadersForTest(headers)
			return Observation{ObservedAt: observedAt, Source: "cpa", Confidence: ConfidenceHigh, Windows: []Window{{Kind: WindowProviderSpecific}}}, nil
		},
	}
	if err := service.RegisterResponseObserver(observer); err != nil {
		t.Fatalf("RegisterResponseObserver() error = %v", err)
	}
	credentialID := uuid.New()
	headers := map[string][]string{"X-Codex-Plan-Type": {"pro"}, "Authorization": {"must-not-persist"}}
	if err := service.ObserveResponse(context.Background(), ProbeRequest{CredentialID: credentialID, ProviderSlug: "codex", Model: "gpt-5", ObservationKey: "response-1"}, headers, now); err != nil {
		t.Fatalf("ObserveResponse() error = %v", err)
	}
	if seenHeaders["Authorization"] != nil {
		t.Fatalf("sensitive header reached observer: %#v", seenHeaders)
	}
	if len(seenHeaders["X-Codex-Plan-Type"]) != 1 || seenHeaders["X-Codex-Plan-Type"][0] != "pro" {
		t.Fatalf("safe quota header missing: %#v", seenHeaders)
	}
}

func TestSanitizeObservationHeadersIsBoundedAndDeterministic(t *testing.T) {
	source := map[string][]string{
		"Authorization":   {"must-not-pass"},
		"X-Codex-Limit-2": {"second"},
		"x-codex-limit-2": {"first"},
		"X-Codex-Limit-3": {"bad\nvalue"},
	}
	for index := 0; index < maxObservationHeaderCount+10; index++ {
		source["X-Codex-Limit-"+strconv.Itoa(index+10)] = []string{"ok"}
	}
	first := sanitizeObservationHeaders("codex", source)
	second := sanitizeObservationHeaders("codex", source)
	if len(first) != maxObservationHeaderCount {
		t.Fatalf("sanitized header count = %d, want %d", len(first), maxObservationHeaderCount)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("sanitized headers are nondeterministic: first=%#v second=%#v", first, second)
	}
	if first["Authorization"] != nil || first["X-Codex-Limit-3"] != nil {
		t.Fatalf("unsafe headers reached observer: %#v", first)
	}
	if got := first["X-Codex-Limit-2"]; !reflect.DeepEqual(got, []string{"second", "first"}) {
		t.Fatalf("canonical duplicate values = %#v, want deterministic order", got)
	}
}

func TestProbeCycleHonorsParentDeadlineBeforeHealthProbe(t *testing.T) {
	persistence := newQuotaTestPersistence()
	service := quotaTestService(t, persistence, time.Now().UTC())
	var healthCalled atomic.Bool
	quotaProbe := &quotaTestProbe{
		name: "deadline-quota", provider: "codex",
		probe: func(ctx context.Context, _ ProbeRequest) (Observation, error) {
			<-ctx.Done()
			return Observation{}, ctx.Err()
		},
	}
	healthProbe := &quotaTestHealthProbe{
		name: "deadline-health", provider: "codex",
		probe: func(context.Context, ProbeRequest) error {
			healthCalled.Store(true)
			return nil
		},
	}
	if err := service.RegisterQuotaProbe(quotaProbe); err != nil {
		t.Fatalf("RegisterQuotaProbe() error = %v", err)
	}
	if err := service.RegisterHealthProbe(healthProbe); err != nil {
		t.Fatalf("RegisterHealthProbe() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := service.ProbeRequest(ctx, ProbeRequest{CredentialID: uuid.New(), ProviderSlug: "codex"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ProbeRequest() error = %v, want context deadline", err)
	}
	if healthCalled.Load() {
		t.Fatal("health probe ran after the probe cycle context expired")
	}
}

func TestProbeLeasePreventsConcurrentCredentialProbes(t *testing.T) {
	persistence := newQuotaTestPersistence()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	service := quotaTestService(t, persistence, now)
	started := make(chan struct{})
	release := make(chan struct{})
	probe := &quotaTestProbe{
		name: "codex-probe", provider: "codex",
		probe: func(ctx context.Context, request ProbeRequest) (Observation, error) {
			close(started)
			select {
			case <-release:
				return Observation{Source: "probe", Confidence: ConfidenceHigh, ObservationKey: request.ObservationKey, Windows: []Window{{Kind: WindowMinute}}}, nil
			case <-ctx.Done():
				return Observation{}, ctx.Err()
			}
		},
	}
	if err := service.RegisterQuotaProbe(probe); err != nil {
		t.Fatalf("RegisterQuotaProbe() error = %v", err)
	}
	request := ProbeRequest{CredentialID: uuid.New(), ProviderSlug: "codex", Model: "gpt-5", ObservationKey: "probe-1"}
	firstDone := make(chan error, 1)
	go func() { firstDone <- service.ProbeRequest(context.Background(), request) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first probe did not start")
	}
	if err := service.ProbeRequest(context.Background(), request); !errors.Is(err, ErrProbeBusy) {
		t.Fatalf("concurrent probe error = %v, want ErrProbeBusy", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first probe error = %v", err)
	}
	if len(persistence.observations) != 1 {
		t.Fatalf("persisted observations = %d, want 1", len(persistence.observations))
	}
}

func TestViewMarksExpiredAndResetSnapshotsStale(t *testing.T) {
	persistence := newQuotaTestPersistence()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	service := quotaTestService(t, persistence, now)
	credentialID := uuid.New()
	persistence.refs[credentialID] = ProbeRequest{CredentialID: credentialID, ProviderSlug: "codex"}
	oldObserved := now.Add(-3 * time.Minute)
	reset := now.Add(time.Hour)
	persistence.snapshots[credentialID] = []Snapshot{{CredentialID: credentialID, ProviderSlug: "codex", WindowKind: WindowMinute, ObservedAt: oldObserved, ResetAt: &reset}}
	view, err := service.View(context.Background(), credentialID, WindowMinute, 10)
	if err != nil {
		t.Fatalf("View() error = %v", err)
	}
	if len(view.Snapshots) != 1 || !view.Snapshots[0].Stale {
		t.Fatalf("stale view = %+v", view)
	}
	reset = now
	persistence.snapshots[credentialID][0].ObservedAt = now
	persistence.snapshots[credentialID][0].ResetAt = &reset
	view, err = service.View(context.Background(), credentialID, WindowMinute, 10)
	if err != nil {
		t.Fatalf("View() reset error = %v", err)
	}
	if !view.Snapshots[0].Stale {
		t.Fatal("snapshot with elapsed reset was not stale")
	}
}

func TestSupportedProbeNamesDeduplicatesRegistrations(t *testing.T) {
	persistence := newQuotaTestPersistence()
	service := quotaTestService(t, persistence, time.Now().UTC())
	probe := &quotaTestProbe{name: "codex-shared", provider: "codex", probe: func(context.Context, ProbeRequest) (Observation, error) { return Observation{}, ErrNoObservation }}
	observer := &quotaTestObserver{name: "codex-shared", provider: "codex", observe: func(context.Context, ProbeRequest, map[string][]string, time.Time) (Observation, error) {
		return Observation{}, ErrNoObservation
	}}
	if err := service.RegisterQuotaProbe(probe); err != nil {
		t.Fatalf("RegisterQuotaProbe() error = %v", err)
	}
	if err := service.RegisterResponseObserver(observer); err != nil {
		t.Fatalf("RegisterResponseObserver() error = %v", err)
	}
	names := service.SupportedProbeNames("CODEX")
	if len(names) != 1 || names[0] != "codex-shared" {
		t.Fatalf("supported probe names = %#v", names)
	}
}

type quotaTestHealthReporter struct {
	mu     sync.Mutex
	inputs []HealthInput
}

func (r *quotaTestHealthReporter) RecordHealth(_ context.Context, _ uuid.UUID, input HealthInput) error {
	r.mu.Lock()
	r.inputs = append(r.inputs, input)
	r.mu.Unlock()
	return nil
}

func TestRecordObservationDerivesStableKeyAndRejectsOversizedMetadata(t *testing.T) {
	persistence := newQuotaTestPersistence()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	service := quotaTestService(t, persistence, now)
	request := ProbeRequest{CredentialID: uuid.New(), ProviderSlug: "codex", Model: "gpt-5"}
	observation := Observation{ObservedAt: now, Source: "test", Windows: []Window{{Kind: WindowMinute}}}
	if err := service.RecordObservation(context.Background(), request, observation); err != nil {
		t.Fatalf("first RecordObservation() error = %v", err)
	}
	if err := service.RecordObservation(context.Background(), request, observation); err != nil {
		t.Fatalf("second RecordObservation() error = %v", err)
	}
	if len(persistence.observations) != 2 || persistence.observations[0].ObservationKey == "" || persistence.observations[0].ObservationKey != persistence.observations[1].ObservationKey {
		t.Fatalf("derived observation keys = %#v", persistence.observations)
	}
	oversized := Observation{Source: "test", Metadata: map[string]string{"payload": strings.Repeat("x", 500)}, Windows: []Window{{Kind: WindowMinute}}}
	for index := 0; index < 20; index++ {
		oversized.Metadata["field-"+string(rune('a'+index))] = strings.Repeat("y", 500)
	}
	if err := service.RecordObservation(context.Background(), request, oversized); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized metadata error = %v, want ErrInvalidInput", err)
	}
	duplicateKey := Observation{Source: "test", Metadata: map[string]string{"signal": "one", " signal ": "two"}, Windows: []Window{{Kind: WindowMinute}}}
	if err := service.RecordObservation(context.Background(), request, duplicateKey); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("normalized duplicate metadata error = %v, want ErrInvalidInput", err)
	}
}

func TestNewServiceRejectsLeaseShorterThanProbeCycle(t *testing.T) {
	_, err := NewService(newQuotaTestPersistence(), Options{ProbeTimeout: 10 * time.Second, LeaseTTL: 24 * time.Second})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("NewService() error = %v, want ErrInvalidInput", err)
	}
}

func TestHealthProbeReportsThroughRuntimeBoundReporter(t *testing.T) {
	persistence := newQuotaTestPersistence()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	service := quotaTestService(t, persistence, now)
	reporter := &quotaTestHealthReporter{}
	service.SetHealthReporter(reporter)
	probe := &quotaTestHealthProbe{name: "runtime-health", provider: "codex", probe: func(context.Context, ProbeRequest) error { return nil }}
	if err := service.RegisterHealthProbe(probe); err != nil {
		t.Fatalf("RegisterHealthProbe() error = %v", err)
	}
	if err := service.ProbeRequest(context.Background(), ProbeRequest{CredentialID: uuid.New(), ProviderSlug: "codex"}); err != nil {
		t.Fatalf("ProbeRequest() error = %v", err)
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if len(reporter.inputs) != 1 || !reporter.inputs[0].Succeeded || !reporter.inputs[0].ObservedAt.Equal(now) {
		t.Fatalf("health reports = %#v", reporter.inputs)
	}
}

func TestFailureDelayHonorsRetryAfterAndCapsBackoff(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	service := quotaTestService(t, newQuotaTestPersistence(), now)
	credentialID := uuid.New()
	if got := service.failureDelay(credentialID, 1, "rate_limit", http.StatusTooManyRequests, 2*time.Minute); got != 2*time.Minute {
		t.Fatalf("retry-after delay = %v, want 2m", got)
	}
	if got := service.failureDelay(credentialID, 1, "server", http.StatusBadGateway, 2*time.Hour); got != service.options.MaxBackoff {
		t.Fatalf("capped retry-after delay = %v, want %v", got, service.options.MaxBackoff)
	}
	if got := service.failureDelay(credentialID, 1, "authentication", http.StatusUnauthorized, 0); got != service.options.AuthBackoff {
		t.Fatalf("authentication delay = %v, want %v", got, service.options.AuthBackoff)
	}
}

func TestHealthProbeReportsAuthenticationAndRateLimitCooldowns(t *testing.T) {
	persistence := newQuotaTestPersistence()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	service := quotaTestService(t, persistence, now)
	reporter := &quotaTestHealthReporter{}
	service.SetHealthReporter(reporter)
	status := http.StatusUnauthorized
	probe := &quotaTestHealthProbe{name: "health-errors", provider: "codex", probe: func(context.Context, ProbeRequest) error {
		return &ProbeError{Class: "authentication", HTTPStatus: status}
	}}
	if err := service.RegisterHealthProbe(probe); err != nil {
		t.Fatalf("RegisterHealthProbe() error = %v", err)
	}
	credentialID := uuid.New()
	err := service.ProbeRequest(context.Background(), ProbeRequest{CredentialID: credentialID, ProviderSlug: "codex"})
	if err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("authentication ProbeRequest() error = %v", err)
	}
	reporter.mu.Lock()
	if len(reporter.inputs) != 1 || reporter.inputs[0].Succeeded || reporter.inputs[0].CooldownUntil == nil || !reporter.inputs[0].CooldownUntil.Equal(now.Add(time.Hour)) {
		t.Fatalf("authentication health report = %#v", reporter.inputs)
	}
	reporter.mu.Unlock()

	status = http.StatusTooManyRequests
	service2 := quotaTestService(t, newQuotaTestPersistence(), now)
	reporter2 := &quotaTestHealthReporter{}
	service2.SetHealthReporter(reporter2)
	if err := service2.RegisterHealthProbe(&quotaTestHealthProbe{name: "rate-health", provider: "codex", probe: func(context.Context, ProbeRequest) error {
		return &ProbeError{Class: "rate_limit", HTTPStatus: status, RetryAfter: 2 * time.Minute}
	}}); err != nil {
		t.Fatalf("RegisterHealthProbe() rate error = %v", err)
	}
	_ = service2.ProbeRequest(context.Background(), ProbeRequest{CredentialID: uuid.New(), ProviderSlug: "codex"})
	reporter2.mu.Lock()
	if len(reporter2.inputs) != 1 || reporter2.inputs[0].CooldownUntil == nil || !reporter2.inputs[0].CooldownUntil.After(now) {
		t.Fatalf("rate health report = %#v", reporter2.inputs)
	}
	reporter2.mu.Unlock()
}
