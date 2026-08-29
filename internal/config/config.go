// Package config loads non-secret Bablo process configuration from the environment.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Config contains process configuration. Secret-bearing URLs are never logged.
type Config struct {
	Environment       string
	HTTPAddr          string
	DatabaseURL       string
	RedisURL          string
	LogLevel          slog.Level
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

// Load reads BABLO_* environment variables and applies safe local defaults.
func Load() (Config, error) {
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

	return Config{
		Environment:       envOr("BABLO_ENV", "development"),
		HTTPAddr:          envOr("BABLO_HTTP_ADDR", ":8080"),
		DatabaseURL:       os.Getenv("BABLO_DATABASE_URL"),
		RedisURL:          os.Getenv("BABLO_REDIS_URL"),
		LogLevel:          level,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
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
