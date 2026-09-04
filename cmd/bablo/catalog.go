package main

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/auth"
	"github.com/starhui-dev/bablo/internal/credential"
	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/httpapi"
	"github.com/starhui-dev/bablo/internal/model"
	"github.com/starhui-dev/bablo/internal/pricing"
	"github.com/starhui-dev/bablo/internal/provider"
	"github.com/starhui-dev/bablo/internal/quota"
	"github.com/starhui-dev/bablo/internal/route"
	"github.com/starhui-dev/bablo/internal/secret"
)

type catalogRuntime struct {
	options        []httpapi.Option
	modelService   *model.Service
	pricingService *pricing.Service

	routeService      *route.Service
	credentialService *credential.Service
}

type quotaHealthReporter struct {
	service *credential.Service
}

func (r quotaHealthReporter) RecordHealth(ctx context.Context, credentialID uuid.UUID, input quota.HealthInput) error {
	if r.service == nil {
		return quota.ErrStateUnavailable
	}
	return r.service.RecordHealth(ctx, credentialID, credential.HealthInput{
		Succeeded:     input.Succeeded,
		ErrorClass:    input.ErrorClass,
		CooldownUntil: input.CooldownUntil,
		ObservedAt:    input.ObservedAt,
		Metadata:      input.Metadata,
	})
}

func newCatalogRuntime(store *data.Store, authHandler *auth.Handler, credentialKeys *secret.Keyring, logger *slog.Logger, quotaViewers ...quota.Viewer) (*catalogRuntime, error) {
	modelRepository, err := model.NewRepository(store)
	if err != nil {
		return nil, err
	}
	modelService, err := model.NewService(modelRepository)
	if err != nil {
		return nil, err
	}

	providerRepository, err := provider.NewRepository(store)
	if err != nil {
		return nil, err
	}
	providerService, err := provider.NewService(providerRepository)
	if err != nil {
		return nil, err
	}

	pricingRepository, err := pricing.NewRepository(store)
	if err != nil {
		return nil, err
	}
	pricingService, err := pricing.NewService(pricingRepository)
	if err != nil {
		return nil, err
	}

	routeRepository, err := route.NewRepository(store)
	if err != nil {
		return nil, err
	}
	routeService, err := route.NewService(routeRepository)
	if err != nil {
		return nil, err
	}

	runtime := &catalogRuntime{
		modelService:   modelService,
		pricingService: pricingService,
		routeService:   routeService,
	}
	if credentialKeys != nil {
		credentialRepository, err := credential.NewRepository(store, credentialKeys)
		if err != nil {
			return nil, err
		}
		credentialService, err := credential.NewService(credentialRepository, credentialKeys)
		if err != nil {
			return nil, err
		}
		runtime.credentialService = credentialService
	}
	if authHandler == nil {
		return runtime, nil
	}

	modelHandler, err := model.NewHandler(modelService, logger)
	if err != nil {
		return nil, err
	}
	providerHandler, err := provider.NewHandler(providerService, logger)
	if err != nil {
		return nil, err
	}
	pricingHandler, err := pricing.NewHandler(pricingService, logger)
	if err != nil {
		return nil, err
	}
	routeHandler, err := route.NewHandler(routeService, logger)
	if err != nil {
		return nil, err
	}

	var credentialHandler http.Handler
	if runtime.credentialService != nil {
		credentialHandler, err = credential.NewHandler(runtime.credentialService, logger, quotaViewers...)
		if err != nil {
			return nil, err
		}
	}

	adminCatalog := http.NewServeMux()
	adminCatalog.Handle("/api/v1/admin/models", modelHandler)
	adminCatalog.Handle("/api/v1/admin/models/", modelHandler)
	adminCatalog.Handle("/api/v1/admin/providers", providerHandler)
	adminCatalog.Handle("/api/v1/admin/providers/", providerHandler)
	adminCatalog.Handle("/api/v1/admin/provider-models", providerHandler)
	adminCatalog.Handle("/api/v1/admin/provider-models/", providerHandler)
	adminCatalog.Handle("/api/v1/admin/prices", pricingHandler)
	adminCatalog.Handle("/api/v1/admin/prices/", pricingHandler)
	adminCatalog.Handle("/api/v1/admin/routes", routeHandler)
	adminCatalog.Handle("/api/v1/admin/routes/", routeHandler)
	if credentialHandler != nil {
		adminCatalog.Handle("/api/v1/admin/credentials", credentialHandler)
		adminCatalog.Handle("/api/v1/admin/credentials/", credentialHandler)
		adminCatalog.Handle("/api/v1/admin/credential-pools", credentialHandler)
		adminCatalog.Handle("/api/v1/admin/credential-pools/", credentialHandler)
	}
	runtime.options = []httpapi.Option{
		httpapi.WithModelHandler(authHandler.Protect(modelHandler)),
		httpapi.WithAdminCatalogHandler(authHandler.ProtectRole(adminCatalog, "admin")),
	}
	return runtime, nil
}

func catalogServerOptions(store *data.Store, authHandler *auth.Handler, credentialKeys *secret.Keyring, logger *slog.Logger) ([]httpapi.Option, error) {
	runtime, err := newCatalogRuntime(store, authHandler, credentialKeys, logger)
	if err != nil {
		return nil, err
	}
	return runtime.options, nil
}
