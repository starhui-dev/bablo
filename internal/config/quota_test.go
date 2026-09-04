package config

import (
	"strings"
	"testing"
	"time"
)

func clearQuotaEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"BABLO_QUOTA_ENABLED",
		"BABLO_QUOTA_POLL_INTERVAL",
		"BABLO_QUOTA_PROBE_TIMEOUT",
		"BABLO_QUOTA_LEASE_TTL",
		"BABLO_QUOTA_MAX_BACKOFF",
		"BABLO_QUOTA_AUTH_BACKOFF",
		"BABLO_QUOTA_UNSUPPORTED_BACKOFF",
		"BABLO_QUOTA_MAX_AGE",
		"BABLO_QUOTA_BATCH_SIZE",
		"BABLO_QUOTA_JITTER_RATIO",
		"BABLO_QUOTA_SCHEDULER_ENABLED",
		"BABLO_QUOTA_SCHEDULER_WINDOW_KIND",
		"BABLO_QUOTA_SCHEDULER_REQUIRE_FRESH",
	} {
		t.Setenv(key, "")
	}
}

func TestLoadQuotaDefaultsToPassiveObservationOnly(t *testing.T) {
	clearQuotaEnv(t)
	cfg, err := LoadQuota("development")
	if err != nil {
		t.Fatalf("LoadQuota() error = %v", err)
	}
	if !cfg.Enabled || cfg.SchedulerQuotaEnabled || cfg.SchedulerWindowKind != "" || cfg.SchedulerRequireFresh {
		t.Fatalf("LoadQuota() defaults = %+v", cfg)
	}
	if cfg.PollInterval != time.Minute || cfg.ProbeTimeout != 10*time.Second || cfg.LeaseTTL != 30*time.Second || cfg.SnapshotMaxAge != 2*time.Minute {
		t.Fatalf("LoadQuota() timing defaults = %+v", cfg)
	}
}

func TestLoadQuotaParsesSchedulerPolicy(t *testing.T) {
	clearQuotaEnv(t)
	t.Setenv("BABLO_QUOTA_SCHEDULER_ENABLED", "true")
	t.Setenv("BABLO_QUOTA_SCHEDULER_WINDOW_KIND", " provider_specific ")
	t.Setenv("BABLO_QUOTA_SCHEDULER_REQUIRE_FRESH", "true")
	cfg, err := LoadQuota("production")
	if err != nil {
		t.Fatalf("LoadQuota() error = %v", err)
	}
	if !cfg.SchedulerQuotaEnabled || cfg.SchedulerWindowKind != "provider_specific" || !cfg.SchedulerRequireFresh {
		t.Fatalf("scheduler policy = %+v", cfg)
	}
}

func TestLoadQuotaRejectsSchedulerWithoutObservationWorker(t *testing.T) {
	clearQuotaEnv(t)
	t.Setenv("BABLO_QUOTA_ENABLED", "false")
	t.Setenv("BABLO_QUOTA_SCHEDULER_ENABLED", "true")
	if _, err := LoadQuota("development"); err == nil || !strings.Contains(err.Error(), "requires BABLO_QUOTA_ENABLED") {
		t.Fatalf("LoadQuota() error = %v, want worker dependency error", err)
	}
}

func TestLoadQuotaRejectsUnknownSchedulerWindow(t *testing.T) {
	clearQuotaEnv(t)
	t.Setenv("BABLO_QUOTA_SCHEDULER_ENABLED", "true")
	t.Setenv("BABLO_QUOTA_SCHEDULER_WINDOW_KIND", "rolling")
	if _, err := LoadQuota("development"); err == nil || !strings.Contains(err.Error(), "unsupported window") {
		t.Fatalf("LoadQuota() error = %v, want window validation", err)
	}
}

func TestLoadQuotaRejectsLeaseShorterThanProbeCycle(t *testing.T) {
	clearQuotaEnv(t)
	t.Setenv("BABLO_QUOTA_PROBE_TIMEOUT", "10s")
	t.Setenv("BABLO_QUOTA_LEASE_TTL", "20s")
	if _, err := LoadQuota("production"); err == nil || !strings.Contains(err.Error(), "invalid worker limits") {
		t.Fatalf("LoadQuota() error = %v, want lease-cycle validation", err)
	}
}
