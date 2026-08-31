package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/starhui-dev/bablo/internal/apikey"
	"github.com/starhui-dev/bablo/internal/auth"
	"github.com/starhui-dev/bablo/internal/config"
	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/httpapi"
	"github.com/starhui-dev/bablo/internal/inference/cpa"
	"github.com/starhui-dev/bablo/internal/proxy"
	"github.com/starhui-dev/bablo/internal/scheduler"
	"github.com/starhui-dev/bablo/internal/secret"
)

var buildVersion = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	if len(arguments) > 0 {
		if arguments[0] == "auth" {
			return runAuthCommand(arguments[1:])
		}
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		logger.Error("bablo_command_error", "error", "unknown command", "command", arguments[0])
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		logger.Error("bablo_config_error", "error", err)
		return 1
	}
	authCfg, err := config.LoadAuth(cfg.Environment)
	if err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		logger.Error("bablo_auth_config_error", "error", err)
		return 1
	}
	credentialCfg, err := config.LoadCredential(cfg.Environment)
	if err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		logger.Error("bablo_credential_config_error", "error", err)
		return 1
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	var store *data.Store
	var authHandler *auth.Handler
	var redisReady bool
	var serverOptions []httpapi.Option
	var credentialKeys *secret.Keyring
	var catalog *catalogRuntime
	var schedulerCoordinator scheduler.Coordinator
	var cpaAdapter *cpa.Adapter
	if len(credentialCfg.Keys) > 0 {
		credentialKeys, err = secret.NewKeyring(credentialCfg.CurrentVersion, credentialCfg.Keys)
		if err != nil {
			logger.Error("bablo_credential_keyring_error", "error", err)
			return 1
		}
	}
	if cfg.DatabaseURL != "" {
		connectCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		store, err = data.Open(connectCtx, data.Config{URL: cfg.DatabaseURL})
		cancel()
		if err != nil {
			logger.Error("bablo_database_error", "error", err)
			return 1
		}
		defer store.Close()

		if len(authCfg.EncryptionKey) > 0 {
			secretBox, err := auth.NewSecretBox(authCfg.EncryptionKey, authCfg.KeyVersion)
			if err != nil {
				logger.Error("bablo_auth_secretbox_error", "error", err)
				return 1
			}
			repository, err := auth.NewRepository(store)
			if err != nil {
				logger.Error("bablo_auth_repository_error", "error", err)
				return 1
			}
			service, err := auth.NewService(repository, auth.ServiceConfig{
				SessionTTL:      authCfg.SessionTTL,
				Issuer:          authCfg.Issuer,
				RequireAdminMFA: authCfg.RequireAdminMFA,
				SecretBox:       secretBox,
			})
			if err != nil {
				logger.Error("bablo_auth_service_error", "error", err)
				return 1
			}
			authHandler, err = auth.NewHandler(service, logger, auth.HandlerConfig{
				AllowedOrigin: authCfg.AllowedOrigin,
				CookieDomain:  authCfg.CookieDomain,
				CookieSecure:  authCfg.CookieSecure,
				SessionTTL:    authCfg.SessionTTL,
			})
			if err != nil {
				logger.Error("bablo_auth_handler_error", "error", err)
				return 1
			}
			serverOptions = append(serverOptions, httpapi.WithAuthHandler(authHandler))
		}

		var limiter apikey.Limiter
		if cfg.RedisURL != "" {
			redisCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			limiter, err = apikey.NewRedisLimiter(redisCtx, cfg.RedisURL)
			cancel()
			if err != nil {
				logger.Error("bablo_redis_error", "error", err)
				return 1
			}
			redisReady = true
		} else {
			limiter = apikey.NewMemoryLimiter()
		}
		defer func() {
			if err := limiter.Close(); err != nil {
				logger.Error("bablo_apikey_limiter_close_error", "error", err)
			}
		}()

		apiKeyRepository, err := apikey.NewRepository(store)
		if err != nil {
			logger.Error("bablo_apikey_repository_error", "error", err)
			return 1
		}
		apiKeyService, err := apikey.NewService(apiKeyRepository, limiter)
		if err != nil {
			logger.Error("bablo_apikey_service_error", "error", err)
			return 1
		}
		apiKeyHandler, err := apikey.NewHandler(apiKeyService, logger)
		if err != nil {
			logger.Error("bablo_apikey_handler_error", "error", err)
			return 1
		}
		if authHandler != nil {
			serverOptions = append(serverOptions, httpapi.WithAPIKeyHandler(authHandler.Protect(apiKeyHandler)))
		}
		catalog, err = newCatalogRuntime(store, authHandler, credentialKeys, logger)
		if err != nil {
			logger.Error("bablo_catalog_error", "error", err)
			return 1
		}
		serverOptions = append(serverOptions, catalog.options...)
		if cfg.CPAConfigPath == "" {
			serverOptions = append(serverOptions, httpapi.WithInferenceHandler(proxy.NewUnavailableHandler(apiKeyService)))
		} else {
			if catalog.credentialService == nil {
				logger.Error("bablo_inference_config_error", "error", "BABLO_CREDENTIAL_ENCRYPTION_KEY is required when BABLO_CPA_CONFIG_PATH is configured")
				return 1
			}
			if cfg.RedisURL != "" {
				coordinatorCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				schedulerCoordinator, err = scheduler.NewRedisCoordinator(coordinatorCtx, cfg.RedisURL, scheduler.CoordinatorOptions{})
				cancel()
				if err != nil {
					logger.Error("bablo_scheduler_coordinator_error", "error", err)
					return 1
				}
			} else {
				schedulerCoordinator = scheduler.NewMemoryCoordinator()
			}
			defer func() {
				if schedulerCoordinator != nil {
					if closeErr := schedulerCoordinator.Close(); closeErr != nil {
						logger.Error("bablo_scheduler_coordinator_close_error", "error", closeErr)
					}
				}
			}()
			schedulerRepository, schedulerErr := scheduler.NewRepository(store)
			if schedulerErr != nil {
				logger.Error("bablo_scheduler_repository_error", "error", schedulerErr)
				return 1
			}
			schedulerService, schedulerErr := scheduler.NewService(schedulerRepository, schedulerCoordinator, scheduler.Options{})
			if schedulerErr != nil {
				logger.Error("bablo_scheduler_service_error", "error", schedulerErr)
				return 1
			}
			cpaAdapter, err = cpa.NewService(cpa.ServiceOptions{ConfigPath: cfg.CPAConfigPath})
			if err != nil {
				logger.Error("bablo_cpa_adapter_error", "error", err)
				return 1
			}
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
				defer cancel()
				if shutdownErr := cpaAdapter.Shutdown(shutdownCtx); shutdownErr != nil {
					logger.Error("bablo_cpa_shutdown_error", "error", shutdownErr)
				}
			}()
			if err := cpaAdapter.ReconcileCredentials(context.Background(), catalog.credentialService); err != nil {
				logger.Error("bablo_cpa_credential_reconcile_error", "error", err)
				return 1
			}
			if err := cpaAdapter.Start(context.Background()); err != nil {
				logger.Error("bablo_cpa_start_error", "error", err)
				return 1
			}
			readyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err = cpaAdapter.WaitReady(readyCtx)
			cancel()
			if err != nil {
				logger.Error("bablo_cpa_ready_error", "error", err)
				return 1
			}
			inferenceHandler, handlerErr := proxy.NewHandler(proxy.Options{
				APIKeys:         apiKeyService,
				Models:          catalog.modelService,
				Routes:          catalog.routeService,
				Scheduler:       schedulerService,
				Engine:          cpaAdapter,
				HealthReporter:  catalog.credentialService,
				RuntimeReporter: cpaAdapter,
				Logger:          logger,
			})
			if handlerErr != nil {
				logger.Error("bablo_proxy_handler_error", "error", handlerErr)
				return 1
			}
			serverOptions = append(serverOptions, httpapi.WithInferenceHandler(inferenceHandler))
		}
	} else if cfg.CPAConfigPath != "" {
		logger.Error("bablo_inference_config_error", "error", "BABLO_DATABASE_URL is required when BABLO_CPA_CONFIG_PATH is configured")
		return 1
	} else if len(authCfg.EncryptionKey) > 0 || credentialKeys != nil {
		logger.Error("bablo_secret_database_error", "error", "BABLO_DATABASE_URL is required when authentication or Credential storage is configured")
		return 1
	}

	if store == nil {
		serverOptions = append(serverOptions, httpapi.WithInferenceHandler(proxy.NewUnavailableHandler(nil)))
	}
	server := httpapi.New(cfg, logger, buildVersion, serverOptions...)
	if store != nil {
		server.SetDependencyReady("postgres", true)
	}
	if redisReady {
		server.SetDependencyReady("redis", true)
	}
	if cpaAdapter != nil {
		server.SetDependencyReady("inference", true)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Start()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("bablo_shutdown_error", "error", err)
			return 1
		}
		logger.Info("bablo_http_stopped")
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("bablo_http_error", "error", err)
			return 1
		}
	}
	return 0
}
