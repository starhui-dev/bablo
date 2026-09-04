package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// QuotaConfig controls passive snapshot persistence, the bounded probe worker,
// and the optional quota policy passed to Scheduler. It contains no secrets;
// provider credentials remain in the Credential keyring.
type QuotaConfig struct {
	Enabled            bool
	PollInterval       time.Duration
	ProbeTimeout       time.Duration
	LeaseTTL           time.Duration
	MaxBackoff         time.Duration
	AuthBackoff        time.Duration
	UnsupportedBackoff time.Duration
	SnapshotMaxAge     time.Duration
	BatchSize          int
	JitterRatio        float64

	// SchedulerQuotaEnabled is deliberately opt-in. A missing or stale
	// snapshot must not take a live route out of service until the operator has
	// verified that the configured provider emits the required signals.
	SchedulerQuotaEnabled bool
	SchedulerWindowKind   string
	SchedulerRequireFresh bool
}

// LoadQuota reads quota worker and optional scheduler settings from
// BABLO_QUOTA_* variables.
func LoadQuota(environment string) (QuotaConfig, error) {
	environment = strings.ToLower(strings.TrimSpace(environment))
	switch environment {
	case "development", "test", "staging", "production":
	default:
		return QuotaConfig{}, fmt.Errorf("quota: unsupported environment %q", environment)
	}
	enabled, err := quotaBoolEnv("BABLO_QUOTA_ENABLED", true)
	if err != nil {
		return QuotaConfig{}, err
	}
	schedulerQuotaEnabled, err := quotaBoolEnv("BABLO_QUOTA_SCHEDULER_ENABLED", false)
	if err != nil {
		return QuotaConfig{}, err
	}
	schedulerWindowKind := ""
	if schedulerQuotaEnabled {
		schedulerWindowKind = strings.ToLower(strings.TrimSpace(os.Getenv("BABLO_QUOTA_SCHEDULER_WINDOW_KIND")))
		if schedulerWindowKind == "" {
			schedulerWindowKind = "provider_specific"
		}
		if !validSchedulerWindowKind(schedulerWindowKind) {
			return QuotaConfig{}, fmt.Errorf("BABLO_QUOTA_SCHEDULER_WINDOW_KIND: unsupported window %q", schedulerWindowKind)
		}
	}
	schedulerRequireFresh, err := quotaBoolEnv("BABLO_QUOTA_SCHEDULER_REQUIRE_FRESH", false)
	if err != nil {
		return QuotaConfig{}, err
	}
	poll, err := durationEnv("BABLO_QUOTA_POLL_INTERVAL", time.Minute)
	if err != nil {
		return QuotaConfig{}, err
	}
	probeTimeout, err := durationEnv("BABLO_QUOTA_PROBE_TIMEOUT", 10*time.Second)
	if err != nil {
		return QuotaConfig{}, err
	}
	leaseTTL, err := durationEnv("BABLO_QUOTA_LEASE_TTL", 30*time.Second)
	if err != nil {
		return QuotaConfig{}, err
	}
	maxBackoff, err := durationEnv("BABLO_QUOTA_MAX_BACKOFF", 30*time.Minute)
	if err != nil {
		return QuotaConfig{}, err
	}
	authBackoff, err := durationEnv("BABLO_QUOTA_AUTH_BACKOFF", 24*time.Hour)
	if err != nil {
		return QuotaConfig{}, err
	}
	unsupportedBackoff, err := durationEnv("BABLO_QUOTA_UNSUPPORTED_BACKOFF", time.Hour)
	if err != nil {
		return QuotaConfig{}, err
	}
	snapshotMaxAge, err := durationEnv("BABLO_QUOTA_MAX_AGE", 2*time.Minute)
	if err != nil {
		return QuotaConfig{}, err
	}
	batchSize, err := quotaIntEnv("BABLO_QUOTA_BATCH_SIZE", 32)
	if err != nil {
		return QuotaConfig{}, err
	}
	jitterRatio, err := quotaFloatEnv("BABLO_QUOTA_JITTER_RATIO", 0.20)
	if err != nil {
		return QuotaConfig{}, err
	}
	if poll <= 0 || probeTimeout <= 0 || leaseTTL < minimumQuotaLeaseTTL(probeTimeout) || maxBackoff <= 0 || authBackoff <= 0 || unsupportedBackoff <= 0 || snapshotMaxAge <= 0 || batchSize < 1 || batchSize > 1000 || jitterRatio < 0 || jitterRatio > 1 {
		return QuotaConfig{}, fmt.Errorf("quota: invalid worker limits")
	}
	if schedulerQuotaEnabled && !enabled {
		return QuotaConfig{}, fmt.Errorf("quota: scheduler enforcement requires BABLO_QUOTA_ENABLED")
	}
	return QuotaConfig{
		Enabled:               enabled,
		PollInterval:          poll,
		ProbeTimeout:          probeTimeout,
		LeaseTTL:              leaseTTL,
		MaxBackoff:            maxBackoff,
		AuthBackoff:           authBackoff,
		UnsupportedBackoff:    unsupportedBackoff,
		SnapshotMaxAge:        snapshotMaxAge,
		BatchSize:             batchSize,
		JitterRatio:           jitterRatio,
		SchedulerQuotaEnabled: schedulerQuotaEnabled,
		SchedulerWindowKind:   schedulerWindowKind,
		SchedulerRequireFresh: schedulerRequireFresh,
	}, nil
}

func minimumQuotaLeaseTTL(probeTimeout time.Duration) time.Duration {
	const maxDuration = time.Duration(1<<63 - 1)
	const safetyMargin = 5 * time.Second
	if probeTimeout <= 0 {
		return 0
	}
	if probeTimeout > (maxDuration-safetyMargin)/2 {
		return maxDuration
	}
	return probeTimeout*2 + safetyMargin
}

func quotaBoolEnv(key string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s: invalid boolean %q", key, value)
	}
	return parsed, nil
}

func quotaIntEnv(key string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q", key, value)
	}
	return parsed, nil
}

func quotaFloatEnv(key string, fallback float64) (float64, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid number %q", key, value)
	}
	return parsed, nil
}

func validSchedulerWindowKind(value string) bool {
	switch value {
	case "minute", "hour", "day", "month", "provider_specific":
		return true
	default:
		return false
	}
}
