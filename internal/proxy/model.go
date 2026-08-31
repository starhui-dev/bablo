// Package proxy owns Bablo's authenticated OpenAI-compatible inference surface.
// It coordinates domain services but never exposes CPA SDK types.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/apikey"
	"github.com/starhui-dev/bablo/internal/credential"
	"github.com/starhui-dev/bablo/internal/inference"
	catalogmodel "github.com/starhui-dev/bablo/internal/model"
	"github.com/starhui-dev/bablo/internal/pricing"
	"github.com/starhui-dev/bablo/internal/route"
	"github.com/starhui-dev/bablo/internal/scheduler"
	"github.com/starhui-dev/bablo/internal/usage"
)

const (
	modelsPath          = "/v1/models"
	chatCompletionsPath = "/v1/chat/completions"
	responsesPath       = "/v1/responses"
	defaultMaxBodyBytes = 4 << 20
	defaultLeaseTTL     = 30 * time.Second
)

// KeyAuthorizer authenticates the inference bearer key and enforces its policy.
// The concrete implementation is internal/apikey; this interface keeps Proxy
// unit-testable and prevents transport code from depending on repository types.
type KeyAuthorizer interface {
	IdentityMiddleware(http.Handler) http.Handler
	Authorize(context.Context, apikey.Principal, string, int64) error
}

// AuthorizedModelLister provides the fresh model entitlement view used by
// /v1/models. It must validate the current key secret version and status.
type AuthorizedModelLister interface {
	ListAuthorizedModels(context.Context, apikey.Principal) ([]string, error)
}

// ModelCatalog resolves aliases and lists public model metadata.
type ModelCatalog interface {
	ResolvePublic(context.Context, string) (catalogmodel.Model, error)
	ListPublic(context.Context, string, int) (catalogmodel.Page, error)
}

// RouteResolver resolves one requested public model to a fixed route snapshot.
type RouteResolver interface {
	Resolve(context.Context, string) (route.Resolution, error)
}

// SchedulerSelector chooses one credential and acquires its concurrency lease.
type SchedulerSelector interface {
	Select(context.Context, scheduler.Request) (scheduler.Selection, error)
}

// HealthReporter persists the safe provider observation in PostgreSQL.
type HealthReporter interface {
	RecordHealth(context.Context, uuid.UUID, credential.HealthInput) error
}

// RuntimeReporter updates adapter-local cooldown state. It is optional and is
// intentionally expressed in Bablo's inference vocabulary.
type RuntimeReporter interface {
	MarkCredentialResult(context.Context, inference.CredentialResult)
}

// UsageRecorder persists request and usage facts. It is optional during bootstrap
// but should be configured for every production inference handler.
type UsageRecorder interface {
	usage.Recorder
}

// PriceSnapshotResolver binds the exact active price version to a resolved
// provider model before an upstream request is executed.
type PriceSnapshotResolver interface {
	ResolveSnapshot(context.Context, uuid.UUID, *uuid.UUID, time.Time) (pricing.Snapshot, error)
}

var (
	_ KeyAuthorizer         = (*apikey.Service)(nil)
	_ AuthorizedModelLister = (*apikey.Service)(nil)
	_ ModelCatalog          = (*catalogmodel.Service)(nil)
	_ RouteResolver         = (*route.Service)(nil)
	_ SchedulerSelector     = (*scheduler.Service)(nil)
	_ HealthReporter        = (*credential.Service)(nil)
)

// Options wires the Proxy orchestration boundary.
type Options struct {
	APIKeys         KeyAuthorizer
	Models          ModelCatalog
	Routes          RouteResolver
	Scheduler       SchedulerSelector
	Engine          inference.Engine
	HealthReporter  HealthReporter
	RuntimeReporter RuntimeReporter
	UsageRecorder   UsageRecorder
	PriceResolver   PriceSnapshotResolver
	Logger          *slog.Logger
	MaxBodyBytes    int64
	LeaseTTL        time.Duration
	Strategy        scheduler.Strategy
	Now             func() time.Time
}

type Handler struct {
	keys         KeyAuthorizer
	models       ModelCatalog
	routes       RouteResolver
	scheduler    SchedulerSelector
	engine       inference.Engine
	health       HealthReporter
	runtime      RuntimeReporter
	usage        UsageRecorder
	prices       PriceSnapshotResolver
	logger       *slog.Logger
	maxBodyBytes int64
	leaseTTL     time.Duration
	strategy     scheduler.Strategy
	now          func() time.Time
	principal    func(context.Context) (apikey.Principal, bool)
}

// NewHandler constructs the authenticated inference surface. Route, Scheduler,
// and Engine may be nil while a process is being bootstrapped; requests that
// need them fail closed with a stable 503 instead of panicking.
func NewHandler(options Options) (http.Handler, error) {
	if options.APIKeys == nil {
		return nil, errors.New("proxy handler requires API key authorizer")
	}
	if options.Models == nil {
		return nil, errors.New("proxy handler requires model catalog")
	}
	if options.Engine != nil && options.UsageRecorder == nil {
		return nil, errors.New("proxy handler requires usage recorder when inference engine is configured")
	}
	if options.Engine != nil && options.PriceResolver == nil {
		return nil, errors.New("proxy handler requires price resolver when inference engine is configured")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.MaxBodyBytes == 0 {
		options.MaxBodyBytes = defaultMaxBodyBytes
	}
	if options.MaxBodyBytes < 1 || options.MaxBodyBytes > 32<<20 {
		return nil, errors.New("proxy handler body limit is invalid")
	}
	if options.LeaseTTL == 0 {
		options.LeaseTTL = defaultLeaseTTL
	}
	if options.LeaseTTL <= 0 {
		return nil, errors.New("proxy handler lease TTL is invalid")
	}
	if options.Strategy == "" {
		options.Strategy = scheduler.StrategyRoundRobin
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	handler := &Handler{
		keys:         options.APIKeys,
		models:       options.Models,
		routes:       options.Routes,
		scheduler:    options.Scheduler,
		engine:       options.Engine,
		health:       options.HealthReporter,
		runtime:      options.RuntimeReporter,
		usage:        options.UsageRecorder,
		prices:       options.PriceResolver,
		logger:       options.Logger,
		maxBodyBytes: options.MaxBodyBytes,
		leaseTTL:     options.LeaseTTL,
		strategy:     options.Strategy,
		now:          options.Now,
		principal:    apikey.PrincipalFromContext,
	}
	return options.APIKeys.IdentityMiddleware(handler), nil
}

// NewUnavailableHandler returns a fail-closed data-plane handler for a process
// whose database or CPA runtime has not been configured. If keys is provided,
// valid bearer authentication is still required before returning 503.
func NewUnavailableHandler(keys KeyAuthorizer) http.Handler {
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeProxyError(w, r, errInferenceUnavailable)
	})
	if keys == nil {
		return base
	}
	return keys.IdentityMiddleware(base)
}
