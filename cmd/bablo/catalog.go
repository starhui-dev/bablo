package main

import (
	"log/slog"
	"net/http"

	"github.com/starhui-dev/bablo/internal/auth"
	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/httpapi"
	"github.com/starhui-dev/bablo/internal/model"
	"github.com/starhui-dev/bablo/internal/pricing"
	"github.com/starhui-dev/bablo/internal/provider"
)

func catalogServerOptions(store *data.Store, authHandler *auth.Handler, logger *slog.Logger) ([]httpapi.Option, error) {
	if authHandler == nil {
		return nil, nil
	}
	modelRepository, err := model.NewRepository(store)
	if err != nil {
		return nil, err
	}
	modelService, err := model.NewService(modelRepository)
	if err != nil {
		return nil, err
	}
	modelHandler, err := model.NewHandler(modelService, logger)
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
	providerHandler, err := provider.NewHandler(providerService, logger)
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
	pricingHandler, err := pricing.NewHandler(pricingService, logger)
	if err != nil {
		return nil, err
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

	return []httpapi.Option{
		httpapi.WithModelHandler(authHandler.Protect(modelHandler)),
		httpapi.WithAdminCatalogHandler(authHandler.ProtectRole(adminCatalog, "admin")),
	}, nil
}
