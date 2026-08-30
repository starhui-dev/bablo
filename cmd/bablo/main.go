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
		catalogOptions, err := catalogServerOptions(store, authHandler, credentialKeys, logger)
		if err != nil {
			logger.Error("bablo_catalog_error", "error", err)
			return 1
		}
		serverOptions = append(serverOptions, catalogOptions...)
	} else if len(authCfg.EncryptionKey) > 0 || credentialKeys != nil {
		logger.Error("bablo_secret_database_error", "error", "BABLO_DATABASE_URL is required when authentication or Credential storage is configured")
		return 1
	}

	server := httpapi.New(cfg, logger, buildVersion, serverOptions...)
	if store != nil {
		server.SetDependencyReady("postgres", true)
	}
	if redisReady {
		server.SetDependencyReady("redis", true)
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
