// Package config loads non-secret Bablo process configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"strings"
	"time"
)

// Config contains non-secret Bablo process configuration. Secret-bearing URLs are never logged.
type Config struct {
	Environment       string
	HTTPAddr          string
	DatabaseURL       string
	RedisURL          string
	CPAConfigPath     string
	LogLevel          slog.Level
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	TrustedProxyCIDRs []netip.Prefix
	ShutdownTimeout   time.Duration
}

// Load reads BABLO_* environment variables and applies safe local defaults.
func Load() (Config, error) {
	environment := strings.ToLower(strings.TrimSpace(envOr("BABLO_ENV", "development")))
	switch environment {
	case "development", "test", "staging", "production":
	default:
		return Config{}, fmt.Errorf("BABLO_ENV: unsupported environment %q", environment)
	}

	level, err := parseLogLevel(envOr("BABLO_LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}

	readHeaderTimeout, err := durationEnv("BABLO_READ_HEADER_TIMEOUT", 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	readTimeout, err := durationEnv("BABLO_READ_TIMEOUT", 0)
	if err != nil {
		return Config{}, err
	}
	writeTimeout, err := durationEnv("BABLO_WRITE_TIMEOUT", 0)
	if err != nil {
		return Config{}, err
	}
	idleTimeout, err := durationEnv("BABLO_IDLE_TIMEOUT", 60*time.Second)
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := durationEnv("BABLO_SHUTDOWN_TIMEOUT", 15*time.Second)
	if err != nil {
		return Config{}, err
	}
	trustedProxyCIDRs, err := prefixListEnv("BABLO_TRUSTED_PROXY_CIDRS")
	if err != nil {
		return Config{}, err
	}
	redisURL := strings.TrimSpace(os.Getenv("BABLO_REDIS_URL"))
	if environment == "production" && redisURL == "" {
		return Config{}, errors.New("BABLO_REDIS_URL is required in production")
	}
	return Config{
		Environment:       environment,
		HTTPAddr:          envOr("BABLO_HTTP_ADDR", ":8080"),
		DatabaseURL:       os.Getenv("BABLO_DATABASE_URL"),
		RedisURL:          redisURL,
		CPAConfigPath:     strings.TrimSpace(os.Getenv("BABLO_CPA_CONFIG_PATH")),
		LogLevel:          level,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		TrustedProxyCIDRs: trustedProxyCIDRs,
		ShutdownTimeout:   shutdownTimeout,
	}, nil
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", key, value, err)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s: duration must not be negative", key)
	}
	return parsed, nil
}

func prefixListEnv(key string) ([]netip.Prefix, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil, nil
	}
	values := strings.Split(raw, ",")
	result := make([]netip.Prefix, 0, len(values))
	seen := make(map[netip.Prefix]struct{}, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("%s: invalid CIDR %q: %w", key, value, err)
		}
		prefix = prefix.Masked()
		if _, exists := seen[prefix]; exists {
			continue
		}
		seen[prefix] = struct{}{}
		result = append(result, prefix)
	}
	return result, nil
}

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("BABLO_LOG_LEVEL: unsupported level %q", value)
	}
}
