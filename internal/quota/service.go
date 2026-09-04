package quota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	defaultPollInterval         = time.Minute
	defaultProbeTimeout         = 10 * time.Second
	defaultProbeLeaseTTL        = 30 * time.Second
	defaultMaxBackoff           = 30 * time.Minute
	defaultAuthBackoff          = 24 * time.Hour
	defaultUnsupportedBackoff   = time.Hour
	defaultSnapshotMaxAge       = 2 * time.Minute
	defaultProbeBatchSize       = 32
	maxObservationWindows       = 8
	maxObservationFields        = 64
	maxObservationKeyBytes      = 128
	maxObservationValueBytes    = 512
	maxObservationMetadataBytes = 8192
	maxObservationHeaderValues  = 8
	maxObservationHeaderCount   = 128
	maxProviderSlugBytes        = 120
	probeLeaseSafetyMargin      = 5 * time.Second
)

// Options controls durable observation freshness and transient worker state.
type Options struct {
	Locker             Locker
	HealthReporter     HealthReporter
	PollInterval       time.Duration
	ProbeTimeout       time.Duration
	LeaseTTL           time.Duration
	MaxBackoff         time.Duration
	AuthBackoff        time.Duration
	UnsupportedBackoff time.Duration
	SnapshotMaxAge     time.Duration
	BatchSize          int
	JitterRatio        float64
	Now                func() time.Time
}

func (o Options) withDefaults() (Options, error) {
	if o.PollInterval == 0 {
		o.PollInterval = defaultPollInterval
	}
	if o.ProbeTimeout == 0 {
		o.ProbeTimeout = defaultProbeTimeout
	}
	if o.LeaseTTL == 0 {
		o.LeaseTTL = defaultProbeLeaseTTL
	}
	if o.MaxBackoff == 0 {
		o.MaxBackoff = defaultMaxBackoff
	}
	if o.AuthBackoff == 0 {
		o.AuthBackoff = defaultAuthBackoff
	}
	if o.UnsupportedBackoff == 0 {
		o.UnsupportedBackoff = defaultUnsupportedBackoff
	}
	if o.SnapshotMaxAge == 0 {
		o.SnapshotMaxAge = defaultSnapshotMaxAge
	}
	if o.BatchSize == 0 {
		o.BatchSize = defaultProbeBatchSize
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Locker == nil {
		o.Locker = NewMemoryLocker()
	}
	if o.PollInterval <= 0 || o.ProbeTimeout <= 0 || o.LeaseTTL <= 0 ||
		o.MaxBackoff <= 0 || o.AuthBackoff <= 0 || o.UnsupportedBackoff <= 0 ||
		o.SnapshotMaxAge <= 0 || o.BatchSize < 1 || o.BatchSize > 1000 ||
		o.JitterRatio < 0 || o.JitterRatio > 1 || o.LeaseTTL < minimumProbeLeaseTTL(o.ProbeTimeout) {
		return Options{}, ErrInvalidInput
	}
	return o, nil
}

func minimumProbeLeaseTTL(probeTimeout time.Duration) time.Duration {
	const maxDuration = time.Duration(1<<63 - 1)
	if probeTimeout <= 0 {
		return 0
	}
	if probeTimeout > (maxDuration-probeLeaseSafetyMargin)/2 {
		return maxDuration
	}
	return probeTimeout*2 + probeLeaseSafetyMargin
}

// Service coordinates provider-specific probes, passive response observers,
// freshness calculation, and durable probe state.
type Service struct {
	repository Persistence
	options    Options

	mu           sync.RWMutex
	quotaProbes  map[string]QuotaProbe
	healthProbes map[string]HealthProbe
	observers    map[string]ResponseObserver
}

// NewService constructs a quota observation service.
func NewService(repository Persistence, options Options) (*Service, error) {
	if repository == nil {
		return nil, ErrInvalidInput
	}
	options, err := options.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Service{
		repository:   repository,
		options:      options,
		quotaProbes:  make(map[string]QuotaProbe),
		healthProbes: make(map[string]HealthProbe),
		observers:    make(map[string]ResponseObserver),
	}, nil
}

// Close releases the transient locker. PostgreSQL remains owned by the caller.
func (s *Service) Close() error {
	if s == nil || s.options.Locker == nil {
		return nil
	}
	return s.options.Locker.Close()
}

// SetHealthReporter updates the optional credential-health sink after service construction.
// The quota worker may be built before the credential service because both are runtime dependencies.
func (s *Service) SetHealthReporter(reporter HealthReporter) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.options.HealthReporter = reporter
	s.mu.Unlock()
}

func (s *Service) healthReporter() HealthReporter {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	reporter := s.options.HealthReporter
	s.mu.RUnlock()
	return reporter
}

// RegisterQuotaProbe adds one uniquely named provider quota probe.
func (s *Service) RegisterQuotaProbe(probe QuotaProbe) error {
	if s == nil || probe == nil {
		return ErrInvalidInput
	}
	name := normalizeProbeName(probe.Name())
	if name == "" {
		return ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.quotaProbes[name]; exists {
		return ErrConflict
	}
	s.quotaProbes[name] = probe
	return nil
}

// RegisterHealthProbe adds one uniquely named provider health probe.
func (s *Service) RegisterHealthProbe(probe HealthProbe) error {
	if s == nil || probe == nil {
		return ErrInvalidInput
	}
	name := normalizeProbeName(probe.Name())
	if name == "" {
		return ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.healthProbes[name]; exists {
		return ErrConflict
	}
	s.healthProbes[name] = probe
	return nil
}

// RegisterResponseObserver adds one passive response observer.
func (s *Service) RegisterResponseObserver(observer ResponseObserver) error {
	if s == nil || observer == nil {
		return ErrInvalidInput
	}
	name := normalizeProbeName(observer.Name())
	if name == "" {
		return ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.observers[name]; exists {
		return ErrConflict
	}
	s.observers[name] = observer
	return nil
}

// SupportedProbeNames returns deterministic names for the provider.
func (s *Service) SupportedProbeNames(provider string) []string {
	if s == nil {
		return nil
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	s.mu.RLock()
	names := make(map[string]struct{})
	for name, probe := range s.quotaProbes {
		if probe.Supports(provider) {
			names[name] = struct{}{}
		}
	}
	for name, probe := range s.healthProbes {
		if probe.Supports(provider) {
			names[name] = struct{}{}
		}
	}
	for name, observer := range s.observers {
		if observer.Supports(provider) {
			names[name] = struct{}{}
		}
	}
	s.mu.RUnlock()
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// RecordObservation validates and durably appends a passive or active quota
// observation. The associated state transition is in the same SQL transaction.
func (s *Service) RecordObservation(ctx context.Context, request ProbeRequest, observation Observation) error {
	return s.recordObservation(ctx, request, observation, "response_observer")
}

func (s *Service) recordObservation(ctx context.Context, request ProbeRequest, observation Observation, probeName string) error {
	if s == nil || s.repository == nil {
		return ErrInvalidInput
	}
	request, err := normalizeProbeRequest(request)
	if err != nil {
		return err
	}
	now := s.nowUTC()
	observation, err = normalizeObservation(observation, now)
	if err != nil {
		return err
	}
	if observation.ObservationKey == "" {
		observation.ObservationKey = request.ObservationKey
	}
	if observation.ObservationKey == "" {
		observation.ObservationKey = deterministicObservationKey(request, observation)
	}
	probeName = normalizeProbeName(probeName)
	if probeName == "" {
		probeName = "response_observer"
	}
	observedAt := observation.ObservedAt
	nextAttempt := now.Add(s.options.PollInterval)
	state := ProbeState{
		CredentialID:   request.CredentialID,
		ProviderSlug:   request.ProviderSlug,
		ProbeName:      probeName,
		Status:         ProbeStatusSuccess,
		LastAttemptAt:  &now,
		LastObservedAt: &observedAt,
		NextAttemptAt:  &nextAttempt,
		FailureCount:   0,
		UpdatedAt:      now,
	}
	return s.repository.PersistObservation(ctx, request, observation, state)
}

// ObserveResponse dispatches a passive observer. Unsupported/no-signal
// responses are deliberately a no-op and never refresh observed_at.
func (s *Service) ObserveResponse(ctx context.Context, request ProbeRequest, headers map[string][]string, observedAt time.Time) error {
	if s == nil {
		return ErrInvalidInput
	}
	request, err := normalizeProbeRequest(request)
	if err != nil {
		return err
	}
	observers := s.observerList(request.ProviderSlug)
	if len(observers) == 0 {
		return nil
	}
	safeHeaders := sanitizeObservationHeaders(request.ProviderSlug, headers)
	for _, observer := range observers {
		observation, observeErr := observer.ObserveResponse(ctx, request, safeHeaders, observedAt)
		if errors.Is(observeErr, ErrNoObservation) {
			continue
		}
		if observeErr != nil {
			return observeErr
		}
		if len(observation.Windows) == 0 {
			continue
		}
		return s.RecordObservation(ctx, request, observation)
	}
	return nil
}

// View returns bounded snapshots and computes stale from observed_at/reset_at.
func (s *Service) View(ctx context.Context, credentialID uuid.UUID, windowKind string, limit int) (View, error) {
	if s == nil || credentialID == uuid.Nil || limit < 1 || limit > 100 {
		return View{}, ErrInvalidInput
	}
	if windowKind != "" && !validWindowKind(windowKind) {
		return View{}, ErrInvalidInput
	}
	request, err := s.repository.GetCredentialRef(ctx, credentialID)
	if err != nil {
		return View{}, err
	}
	state, stateFound, err := s.repository.GetProbeState(ctx, credentialID)
	if err != nil {
		return View{}, err
	}
	snapshots, err := s.repository.ListSnapshots(ctx, credentialID, windowKind, limit)
	if err != nil {
		return View{}, err
	}
	now := s.nowUTC()
	for index := range snapshots {
		snapshots[index].Stale = snapshotStale(snapshots[index], now, s.options.SnapshotMaxAge)
	}
	return View{
		CredentialID:    credentialID,
		ProviderSlug:    request.ProviderSlug,
		State:           state,
		StateFound:      stateFound,
		Snapshots:       snapshots,
		SupportedProbes: s.SupportedProbeNames(request.ProviderSlug),
	}, nil
}

// ProbeCredential performs one bounded, per-credential probe cycle.
func (s *Service) ProbeCredential(ctx context.Context, credentialID uuid.UUID) error {
	if s == nil || credentialID == uuid.Nil {
		return ErrInvalidInput
	}
	request, err := s.repository.GetCredentialRef(ctx, credentialID)
	if err != nil {
		return err
	}
	return s.probeCredential(ctx, request)
}

// ProbeRequest performs one bounded probe cycle using a PostgreSQL-resolved
// provider identity. It is useful to workers that already loaded ListDue.
func (s *Service) ProbeRequest(ctx context.Context, request ProbeRequest) error {
	if s == nil {
		return ErrInvalidInput
	}
	request, err := normalizeProbeRequest(request)
	if err != nil {
		return err
	}
	return s.probeCredential(ctx, request)
}

func (s *Service) probeCredential(ctx context.Context, request ProbeRequest) error {
	if ctx == nil {
		ctx = context.Background()
	}
	owner := uuid.NewString()
	lease, err := s.options.Locker.Acquire(ctx, request.CredentialID.String(), owner, s.options.LeaseTTL)
	if err != nil {
		return err
	}
	cycleTimeout := s.options.LeaseTTL - probeLeaseSafetyMargin
	cycleCtx, cancelCycle := context.WithTimeout(ctx, cycleTimeout)
	var results []error
	quotaErr := s.runQuotaProbe(cycleCtx, request)
	if quotaErr != nil && !errors.Is(quotaErr, ErrUnsupportedProbe) && !errors.Is(quotaErr, ErrNoObservation) {
		results = append(results, quotaErr)
	}
	var healthErr error
	if cycleErr := cycleCtx.Err(); cycleErr != nil {
		if quotaErr == nil || errors.Is(quotaErr, ErrUnsupportedProbe) || errors.Is(quotaErr, ErrNoObservation) {
			results = append(results, cycleErr)
		}
	} else {
		healthErr = s.runHealthProbe(cycleCtx, request)
		if healthErr != nil && !errors.Is(healthErr, ErrNoObservation) {
			results = append(results, healthErr)
		}
	}
	cancelCycle()
	releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), probeLeaseSafetyMargin)
	if releaseErr := lease.Release(releaseCtx); releaseErr != nil {
		results = append(results, releaseErr)
	}
	cancelRelease()
	if len(results) > 0 {
		return errors.Join(results...)
	}
	if healthErr == nil && (errors.Is(quotaErr, ErrUnsupportedProbe) || errors.Is(quotaErr, ErrNoObservation)) {
		if s.healthProbe(request.ProviderSlug) != nil {
			return nil
		}
	}
	return quotaErr
}

func (s *Service) runQuotaProbe(ctx context.Context, request ProbeRequest) error {
	probe := s.quotaProbe(request.ProviderSlug)
	if probe == nil {
		err := s.recordUnsupported(ctx, request)
		if err != nil {
			return errors.Join(ErrUnsupportedProbe, err)
		}
		return ErrUnsupportedProbe
	}
	probeCtx, cancel := context.WithTimeout(ctx, s.options.ProbeTimeout)
	observation, probeErr := probe.Probe(probeCtx, request)
	cancel()
	if probeErr == nil {
		if len(observation.Windows) == 0 {
			probeErr = ErrNoObservation
		} else {
			if observation.Source == "" {
				observation.Source = "probe:" + normalizeProbeName(probe.Name())
			}
			if observation.Confidence == "" {
				observation.Confidence = ConfidenceMedium
			}
			if err := s.recordObservation(ctx, request, observation, normalizeProbeName(probe.Name())); err != nil {
				return err
			}
			return nil
		}
	}
	if errors.Is(probeErr, ErrNoObservation) {
		if err := s.recordNoObservation(ctx, request, normalizeProbeName(probe.Name())); err != nil {
			return errors.Join(ErrNoObservation, err)
		}
		return ErrNoObservation
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	class, status, retryAfter := classifyProbeError(probeErr, probeCtx.Err())
	stateErr := s.recordFailure(ctx, request, normalizeProbeName(probe.Name()), class, status, retryAfter)
	if stateErr != nil {
		return errors.Join(probeErr, stateErr)
	}
	return probeErr
}

func (s *Service) runHealthProbe(ctx context.Context, request ProbeRequest) error {
	probe := s.healthProbe(request.ProviderSlug)
	if probe == nil {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, s.options.ProbeTimeout)
	probeErr := probe.Probe(probeCtx, request)
	cancel()
	if errors.Is(probeErr, ErrNoObservation) {
		return ErrNoObservation
	}
	now := s.nowUTC()
	if probeErr == nil {
		return s.reportHealth(ctx, request.CredentialID, HealthInput{Succeeded: true, ObservedAt: now})
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	class, status, retryAfter := classifyProbeError(probeErr, probeCtx.Err())
	var cooldown *time.Time
	if status == 401 || status == 403 {
		value := now.Add(s.options.AuthBackoff)
		cooldown = &value
	} else if retryAfter > 0 || status == 429 || status >= 500 {
		delay := s.failureDelay(request.CredentialID, 1, class, status, retryAfter)
		value := now.Add(delay)
		cooldown = &value
	}
	reportErr := s.reportHealth(ctx, request.CredentialID, HealthInput{
		Succeeded:     false,
		ErrorClass:    class,
		CooldownUntil: cooldown,
		ObservedAt:    now,
	})
	if reportErr != nil {
		return errors.Join(probeErr, reportErr)
	}
	return probeErr
}

func (s *Service) reportHealth(ctx context.Context, credentialID uuid.UUID, input HealthInput) error {
	if reporter := s.healthReporter(); reporter != nil {
		input.Metadata = cloneStringMap(input.Metadata)
		return reporter.RecordHealth(ctx, credentialID, input)
	}
	return nil

}

func (s *Service) recordUnsupported(ctx context.Context, request ProbeRequest) error {
	now := s.nowUTC()
	next := now.Add(s.options.UnsupportedBackoff)
	return s.repository.UpsertProbeState(ctx, ProbeState{
		CredentialID:   request.CredentialID,
		ProviderSlug:   request.ProviderSlug,
		ProbeName:      "none",
		Status:         ProbeStatusUnsupported,
		LastAttemptAt:  &now,
		NextAttemptAt:  &next,
		LastErrorClass: "probe_unsupported",
		UpdatedAt:      now,
	})
}

func (s *Service) recordNoObservation(ctx context.Context, request ProbeRequest, probeName string) error {
	now := s.nowUTC()
	next := now.Add(s.options.PollInterval)
	state := ProbeState{
		CredentialID:  request.CredentialID,
		ProviderSlug:  request.ProviderSlug,
		ProbeName:     probeName,
		Status:        ProbeStatusNoObservation,
		LastAttemptAt: &now,
		NextAttemptAt: &next,
		UpdatedAt:     now,
	}
	if previous, found, err := s.repository.GetProbeState(ctx, request.CredentialID); err != nil {
		return err
	} else if found {
		state.LastObservedAt = previous.LastObservedAt
	}
	return s.repository.UpsertProbeState(ctx, state)
}

func (s *Service) recordFailure(ctx context.Context, request ProbeRequest, probeName, class string, status int, retryAfter time.Duration) error {
	now := s.nowUTC()
	previous, found, err := s.repository.GetProbeState(ctx, request.CredentialID)
	if err != nil {
		return err
	}
	failureCount := 1
	lastObserved := (*time.Time)(nil)
	if found {
		failureCount = previous.FailureCount + 1
		lastObserved = previous.LastObservedAt
	}
	if failureCount < 1 {
		failureCount = 1
	}
	delay := s.failureDelay(request.CredentialID, failureCount, class, status, retryAfter)
	next := now.Add(delay)
	return s.repository.UpsertProbeState(ctx, ProbeState{
		CredentialID:   request.CredentialID,
		ProviderSlug:   request.ProviderSlug,
		ProbeName:      probeName,
		Status:         ProbeStatusError,
		LastAttemptAt:  &now,
		LastObservedAt: lastObserved,
		NextAttemptAt:  &next,
		FailureCount:   failureCount,
		LastErrorClass: class,
		LastHTTPStatus: status,
		UpdatedAt:      now,
	})
}

func (s *Service) failureDelay(credentialID uuid.UUID, failureCount int, class string, status int, retryAfter time.Duration) time.Duration {
	if status == 401 || status == 403 || class == "authentication" || class == "permission" {
		return s.options.AuthBackoff
	}
	base := s.options.PollInterval
	if retryAfter > base {
		base = retryAfter
	}
	if retryAfter <= 0 {
		for index := 1; index < failureCount && base < s.options.MaxBackoff; index++ {
			if base > s.options.MaxBackoff/2 {
				base = s.options.MaxBackoff
				break
			}
			base *= 2
		}
	}
	if base > s.options.MaxBackoff {
		base = s.options.MaxBackoff
	}
	if s.options.JitterRatio == 0 {
		return base
	}
	seed := sha256.Sum256([]byte(credentialID.String() + ":" + strconv.Itoa(failureCount)))
	fraction := float64(seed[0]) / 255
	factor := (1 - s.options.JitterRatio) + (2 * s.options.JitterRatio * fraction)
	delay := time.Duration(float64(base) * factor)
	if delay < time.Nanosecond {
		return time.Nanosecond
	}
	if delay > s.options.MaxBackoff && retryAfter <= s.options.MaxBackoff {
		return s.options.MaxBackoff
	}
	return delay
}

// RunOnce probes one bounded batch. A busy credential is normal when another
// instance owns its lease and is not returned as a worker failure.
func (s *Service) RunOnce(ctx context.Context) error {
	if s == nil || s.repository == nil {
		return ErrInvalidInput
	}
	if ctx == nil {
		ctx = context.Background()
	}
	due, err := s.repository.ListDue(ctx, s.nowUTC(), s.options.BatchSize)
	if err != nil {
		return err
	}
	var wait sync.WaitGroup
	var mu sync.Mutex
	failures := make([]error, 0)
	for _, item := range due {
		item := item
		wait.Add(1)
		go func() {
			defer wait.Done()
			probeErr := s.ProbeRequest(ctx, item.Request)
			if probeErr == nil || errors.Is(probeErr, ErrProbeBusy) || errors.Is(probeErr, ErrUnsupportedProbe) || errors.Is(probeErr, ErrNoObservation) {
				return
			}
			mu.Lock()
			failures = append(failures, fmt.Errorf("credential %s: %w", item.Request.CredentialID, probeErr))
			mu.Unlock()
		}()
	}
	wait.Wait()
	return errors.Join(failures...)
}

// Run starts the bounded polling worker and returns when ctx is canceled.
func (s *Service) Run(ctx context.Context, logger *slog.Logger) {
	if s == nil || s.options.PollInterval <= 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}
	run := func() {
		if ctx.Err() != nil {
			return
		}
		if err := s.RunOnce(ctx); err != nil {
			logger.Error("bablo_quota_worker_error", "error", err)
		}
	}
	run()
	ticker := time.NewTicker(s.options.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func (s *Service) nowUTC() time.Time {
	if s == nil || s.options.Now == nil {
		return time.Now().UTC()
	}
	return s.options.Now().UTC()
}

func (s *Service) quotaProbe(provider string) QuotaProbe {
	provider = strings.ToLower(strings.TrimSpace(provider))
	s.mu.RLock()
	defer s.mu.RUnlock()
	var names []string
	for name, probe := range s.quotaProbes {
		if probe.Supports(provider) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return s.quotaProbes[names[0]]
}

func (s *Service) healthProbe(provider string) HealthProbe {
	provider = strings.ToLower(strings.TrimSpace(provider))
	s.mu.RLock()
	defer s.mu.RUnlock()
	var names []string
	for name, probe := range s.healthProbes {
		if probe.Supports(provider) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return s.healthProbes[names[0]]
}

func (s *Service) observerList(provider string) []ResponseObserver {
	provider = strings.ToLower(strings.TrimSpace(provider))
	s.mu.RLock()
	result := make([]ResponseObserver, 0)
	for _, observer := range s.observers {
		if observer.Supports(provider) {
			result = append(result, observer)
		}
	}
	s.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool {
		return normalizeProbeName(result[i].Name()) < normalizeProbeName(result[j].Name())
	})
	return result
}

func normalizeProbeRequest(request ProbeRequest) (ProbeRequest, error) {
	request.ProviderSlug = strings.ToLower(strings.TrimSpace(request.ProviderSlug))
	request.Model = strings.TrimSpace(request.Model)
	request.ObservationKey = strings.TrimSpace(request.ObservationKey)
	if request.CredentialID == uuid.Nil || request.ProviderSlug == "" || len(request.ProviderSlug) > maxProviderSlugBytes || containsControl(request.ProviderSlug) || len(request.Model) > 255 || containsControl(request.Model) || len(request.ObservationKey) > 128 || containsControl(request.ObservationKey) {
		return ProbeRequest{}, ErrInvalidInput
	}
	return request, nil
}

func normalizeObservation(observation Observation, now time.Time) (Observation, error) {
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = now
	} else {
		observation.ObservedAt = observation.ObservedAt.UTC()
	}
	if observation.ObservedAt.After(now.Add(5 * time.Minute)) {
		return Observation{}, ErrInvalidInput
	}
	observation.Source = strings.TrimSpace(observation.Source)
	if observation.Source == "" || len(observation.Source) > 120 || containsControl(observation.Source) {
		return Observation{}, ErrInvalidInput
	}
	observation.Confidence = strings.ToLower(strings.TrimSpace(observation.Confidence))
	if observation.Confidence == "" {
		observation.Confidence = ConfidenceUnknown
	}
	observation.ObservationKey = strings.TrimSpace(observation.ObservationKey)
	if observation.ObservationKey != "" && (len(observation.ObservationKey) > maxObservationKeyBytes || containsControl(observation.ObservationKey)) {
		return Observation{}, ErrInvalidInput
	}
	observation.ErrorClass = strings.TrimSpace(observation.ErrorClass)
	if observation.ErrorClass != "" && (len(observation.ErrorClass) > 80 || containsControl(observation.ErrorClass)) {
		return Observation{}, ErrInvalidInput
	}
	if !validConfidence(observation.Confidence) || len(observation.Windows) == 0 || len(observation.Windows) > maxObservationWindows {
		return Observation{}, ErrInvalidInput
	}
	metadata, err := normalizeObservationMetadata(observation.Metadata)
	if err != nil {
		return Observation{}, err
	}
	observation.Metadata = metadata
	seen := make(map[string]struct{}, len(observation.Windows))
	for index := range observation.Windows {
		window := cloneWindow(observation.Windows[index])
		window.Kind = strings.ToLower(strings.TrimSpace(window.Kind))
		if !validWindowKind(window.Kind) {
			return Observation{}, ErrInvalidInput
		}
		if _, exists := seen[window.Kind]; exists {
			return Observation{}, ErrConflict
		}
		seen[window.Kind] = struct{}{}
		for _, value := range []*int64{window.UsedTokens, window.RemainingTokens, window.LimitTokens} {
			if value != nil && *value < 0 {
				return Observation{}, ErrInvalidInput
			}
		}
		if window.ResetAt != nil {
			reset := window.ResetAt.UTC()
			if reset.After(now.Add(366 * 24 * time.Hour)) {
				return Observation{}, ErrInvalidInput
			}
			window.ResetAt = &reset
		}
		window.Metadata, err = normalizeObservationMetadata(window.Metadata)
		if err != nil {
			return Observation{}, err
		}
		observation.Windows[index] = window
	}
	return observation, nil
}

func normalizeObservationMetadata(source map[string]string) (map[string]string, error) {
	if len(source) > maxObservationFields {
		return nil, ErrInvalidInput
	}
	if len(source) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		key = strings.TrimSpace(key)
		if key == "" || len(key) > maxObservationKeyBytes || containsControl(key) || len(value) > maxObservationValueBytes || containsControl(value) {
			return nil, ErrInvalidInput
		}
		if _, exists := result[key]; exists {
			return nil, ErrInvalidInput
		}
		result[key] = value
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maxObservationMetadataBytes {
		return nil, ErrInvalidInput
	}
	return result, nil

}

func snapshotStale(snapshot Snapshot, now time.Time, maxAge time.Duration) bool {
	return now.Before(snapshot.ObservedAt) || now.Sub(snapshot.ObservedAt) > maxAge || (snapshot.ResetAt != nil && !snapshot.ResetAt.After(now))
}

func normalizeProbeName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) > 120 || containsControl(value) {
		return ""
	}
	return value
}

func containsControl(value string) bool {
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return true
		}
	}
	return false
}

func sanitizeObservationHeaders(provider string, source map[string][]string) map[string][]string {
	if len(source) == 0 {
		return nil
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	normalized := make(map[string][]string)
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values := source[key]
		name := http.CanonicalHeaderKey(strings.TrimSpace(key))
		if name == "" || len(name) > maxObservationKeyBytes || !isQuotaObservationHeader(provider, name) {
			continue
		}
		for _, value := range values {
			if value == "" || len(value) > maxObservationValueBytes || containsControl(value) {
				continue
			}
			entries := normalized[name]
			if len(entries) < maxObservationHeaderValues {
				normalized[name] = append(entries, value)
			}
		}
	}
	if len(normalized) == 0 {
		return nil
	}
	names := make([]string, 0, len(normalized))
	for name := range normalized {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > maxObservationHeaderCount {
		names = names[:maxObservationHeaderCount]
	}
	result := make(map[string][]string, len(names))
	for _, name := range names {
		result[name] = append([]string(nil), normalized[name]...)
	}
	return result
}

func isQuotaObservationHeader(provider, name string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "retry-after" {
		return provider == "claude" || provider == "codex"
	}
	switch provider {
	case "claude":
		return strings.HasPrefix(name, "anthropic-ratelimit-unified-")
	case "codex":
		if strings.HasPrefix(name, "x-ratelimit-") || name == "x-codex-active-limit" || name == "x-codex-plan-type" {
			return true
		}
		if strings.HasPrefix(name, "x-codex-credits-") {
			return true
		}
		for _, marker := range []string{"limit", "remaining", "reset", "window", "rate"} {
			if strings.Contains(name, marker) {
				return strings.HasPrefix(name, "x-codex-")
			}
		}
	}
	return false
}

type observationDigestField struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type observationDigestWindow struct {
	Kind            string                   `json:"kind"`
	UsedTokens      *int64                   `json:"used_tokens,omitempty"`
	RemainingTokens *int64                   `json:"remaining_tokens,omitempty"`
	LimitTokens     *int64                   `json:"limit_tokens,omitempty"`
	ResetAt         string                   `json:"reset_at,omitempty"`
	Metadata        []observationDigestField `json:"metadata,omitempty"`
}

func deterministicObservationKey(request ProbeRequest, observation Observation) string {
	windows := make([]observationDigestWindow, 0, len(observation.Windows))
	for _, window := range observation.Windows {
		windows = append(windows, observationDigestWindow{
			Kind:            window.Kind,
			UsedTokens:      cloneInt64(window.UsedTokens),
			RemainingTokens: cloneInt64(window.RemainingTokens),
			LimitTokens:     cloneInt64(window.LimitTokens),
			ResetAt:         formatDigestTime(window.ResetAt),
			Metadata:        digestMetadata(window.Metadata),
		})
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].Kind < windows[j].Kind })
	payload := struct {
		CredentialID string                    `json:"credential_id"`
		ProviderSlug string                    `json:"provider_slug"`
		Model        string                    `json:"model"`
		ObservedAt   string                    `json:"observed_at"`
		Source       string                    `json:"source"`
		Confidence   string                    `json:"confidence"`
		ErrorClass   string                    `json:"error_class,omitempty"`
		Metadata     []observationDigestField  `json:"metadata,omitempty"`
		Windows      []observationDigestWindow `json:"windows"`
	}{
		CredentialID: request.CredentialID.String(),
		ProviderSlug: request.ProviderSlug,
		Model:        request.Model,
		ObservedAt:   observation.ObservedAt.UTC().Format(time.RFC3339Nano),
		Source:       observation.Source,
		Confidence:   observation.Confidence,
		ErrorClass:   observation.ErrorClass,
		Metadata:     digestMetadata(observation.Metadata),
		Windows:      windows,
	}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return "obs:" + hex.EncodeToString(digest[:])
}

func digestMetadata(source map[string]string) []observationDigestField {
	if len(source) == 0 {
		return nil
	}
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]observationDigestField, 0, len(keys))
	for _, key := range keys {
		result = append(result, observationDigestField{Key: key, Value: source[key]})
	}
	return result
}

func formatDigestTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func classifyProbeError(err error, contextErr error) (string, int, time.Duration) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(contextErr, context.DeadlineExceeded) {
		return "probe_timeout", 0, 0
	}
	if errors.Is(err, context.Canceled) {
		return "probe_cancelled", 0, 0
	}
	var probeErr *ProbeError
	if errors.As(err, &probeErr) && probeErr != nil {
		class := strings.ToLower(strings.TrimSpace(probeErr.Class))
		if class == "" {
			class = classifyHTTPStatus(probeErr.HTTPStatus)
		}
		return class, probeErr.HTTPStatus, nonNegativeDuration(probeErr.RetryAfter)
	}
	status := 0
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) && statusErr != nil {
		status = statusErr.StatusCode()
	}
	return classifyHTTPStatus(status), status, 0
}

func classifyHTTPStatus(status int) string {
	switch {
	case status == 401:
		return "authentication"
	case status == 403:
		return "permission"
	case status == 408:
		return "timeout"
	case status == 429:
		return "rate_limit"
	case status >= 500:
		return "server"
	case status >= 400:
		return "client"
	default:
		return "probe_error"
	}
}

func nonNegativeDuration(value time.Duration) time.Duration {
	if value < 0 {
		return 0
	}
	return value
}
