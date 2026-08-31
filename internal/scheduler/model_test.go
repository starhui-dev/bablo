package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/model"
	"github.com/starhui-dev/bablo/internal/route"
)

func TestSupportsRequiredCapabilities(t *testing.T) {
	available := model.Capabilities{Chat: true, Stream: true, Tools: true}
	if !supportsRequired(available, model.Capabilities{Chat: true, Stream: true}) {
		t.Fatal("available capabilities rejected a supported request")
	}
	if supportsRequired(available, model.Capabilities{Chat: true, Reasoning: true}) {
		t.Fatal("available capabilities accepted an unsupported request")
	}
}

func TestWeightedStartUsesDeterministicSlots(t *testing.T) {
	values := []*candidate{
		{combinedWeight: 1},
		{combinedWeight: 2},
	}
	for _, test := range []struct {
		slot int64
		want int
	}{
		{slot: 0, want: 0},
		{slot: 1, want: 1},
		{slot: 2, want: 1},
	} {
		if got := weightedStart(values, test.slot); got != test.want {
			t.Fatalf("weightedStart(%d) = %d, want %d", test.slot, got, test.want)
		}
	}
}

func TestOrderCandidatesRoundRobinAndAffinity(t *testing.T) {
	coordinator := NewMemoryCoordinator()
	t.Cleanup(func() { _ = coordinator.Close() })
	service := &Service{coordinator: coordinator, options: Options{CursorTTL: time.Minute}}
	versionID := uuid.New()
	targetID := uuid.New()
	values := []*candidate{
		{target: route.Target{ID: targetID, RouteVersionID: versionID, TargetNo: 0, Priority: 0}, member: poolMember{CredentialID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), MemberPriority: 0}},
		{target: route.Target{ID: targetID, RouteVersionID: versionID, TargetNo: 0, Priority: 0}, member: poolMember{CredentialID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), MemberPriority: 0}},
	}
	request := Request{
		RequestID:  "request-order",
		Resolution: route.Resolution{Route: route.Route{ID: uuid.New()}, Version: route.Version{ID: versionID}},
		Strategy:   StrategyRoundRobin,
		Now:        time.Now().UTC(),
	}
	first, affinityUsed, err := service.orderCandidates(context.Background(), request, append([]*candidate(nil), values...), uuid.Nil, false)
	if err != nil || affinityUsed || first[0].member.CredentialID != values[0].member.CredentialID {
		t.Fatalf("first round-robin order = %#v, affinity=%v, err=%v", first, affinityUsed, err)
	}
	second, _, err := service.orderCandidates(context.Background(), request, append([]*candidate(nil), values...), uuid.Nil, false)
	if err != nil || second[0].member.CredentialID != values[1].member.CredentialID {
		t.Fatalf("second round-robin order = %#v, err=%v", second, err)
	}
	affinity, affinityUsed, err := service.orderCandidates(context.Background(), request, append([]*candidate(nil), values...), values[0].member.CredentialID, true)
	if err != nil || !affinityUsed || affinity[0].member.CredentialID != values[0].member.CredentialID {
		t.Fatalf("affinity order = %#v, used=%v, err=%v", affinity, affinityUsed, err)
	}
}

func TestMemoryCoordinatorLeaseRenewAndExpiry(t *testing.T) {
	coordinator := NewMemoryCoordinator()
	t.Cleanup(func() { _ = coordinator.Close() })
	lease, err := coordinator.Acquire(context.Background(), "credential:slot:0", "owner-a", 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if _, err := coordinator.Acquire(context.Background(), "credential:slot:0", "owner-b", time.Second); !errors.Is(err, ErrLeaseBusy) {
		t.Fatalf("second Acquire() error = %v, want ErrLeaseBusy", err)
	}
	if err := lease.Renew(context.Background(), 50*time.Millisecond); err != nil {
		t.Fatalf("Renew() error = %v", err)
	}
	time.Sleep(25 * time.Millisecond)
	if _, err := coordinator.Acquire(context.Background(), "credential:slot:0", "owner-b", time.Second); !errors.Is(err, ErrLeaseBusy) {
		t.Fatalf("Acquire() after renew error = %v, want ErrLeaseBusy", err)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if _, err := coordinator.Acquire(context.Background(), "credential:slot:0", "owner-b", time.Second); err != nil {
		t.Fatalf("Acquire() after release error = %v", err)
	}
}

func TestMemoryCoordinatorCursorAndAffinityTTL(t *testing.T) {
	coordinator := NewMemoryCoordinator()
	t.Cleanup(func() { _ = coordinator.Close() })
	for index, want := range []int64{0, 1, 0} {
		got, err := coordinator.Next(context.Background(), "cursor", 2, time.Second)
		if err != nil || got != want {
			t.Fatalf("Next(%d) = %d, %v; want %d", index, got, err, want)
		}
	}
	credentialID := uuid.New()
	if err := coordinator.SetAffinity(context.Background(), "session-key", credentialID, 20*time.Millisecond); err != nil {
		t.Fatalf("SetAffinity() error = %v", err)
	}
	got, found, err := coordinator.GetAffinity(context.Background(), "session-key")
	if err != nil || !found || got != credentialID {
		t.Fatalf("GetAffinity() = %s, %v, %v", got, found, err)
	}
	time.Sleep(25 * time.Millisecond)
	if _, found, err := coordinator.GetAffinity(context.Background(), "session-key"); err != nil || found {
		t.Fatalf("expired affinity = found:%v err:%v", found, err)
	}
}

func FuzzWeightedStartStaysWithinCandidates(f *testing.F) {
	f.Add([]byte{1, 2}, int64(0))
	f.Add([]byte{3, 1, 4}, int64(7))
	f.Fuzz(func(t *testing.T, raw []byte, slot int64) {
		if len(raw) == 0 || len(raw) > 256 {
			return
		}
		values := make([]*candidate, len(raw))
		var total int64
		for index, rawWeight := range raw {
			weight := int64(rawWeight%100) + 1
			values[index] = &candidate{combinedWeight: weight}
			total += weight
		}
		slot %= total
		if slot < 0 {
			slot += total
		}
		start := weightedStart(values, slot)
		if start < 0 || start >= len(values) {
			t.Fatalf("weightedStart(%d) = %d for %d candidates", slot, start, len(values))
		}
	})
}

func TestIneligibleCredentialReasonsAreExplicit(t *testing.T) {
	now := time.Now().UTC()
	cooldown := now.Add(time.Minute)
	reasons := memberEligibilityReasons(poolMember{
		Enabled:         false,
		ProviderEnabled: false,
		Status:          "revoked",
		MaxConcurrency:  0,
		Region:          "eu",
		ProxyRef:        "proxy-a",
		CooldownUntil:   &cooldown,
	}, Request{Now: now, Region: "us", RequireProxy: true, ProxyRef: "proxy-b"})
	for _, reason := range []string{"pool_member_disabled", "provider_disabled", "credential_inactive", "credential_capacity_invalid", "cooldown", "region_mismatch", "proxy_mismatch"} {
		found := false
		for _, actual := range reasons {
			if actual == reason {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("eligibility reasons = %v, missing %q", reasons, reason)
		}
	}
}

func FuzzUnavailableCredentialIsAlwaysRejected(f *testing.F) {
	f.Add(false, true, "active", int64(1), false)
	f.Add(true, false, "active", int64(1), false)
	f.Add(true, true, "revoked", int64(1), false)
	f.Add(true, true, "active", int64(0), false)
	f.Add(true, true, "active", int64(1), true)
	f.Fuzz(func(t *testing.T, memberEnabled, providerEnabled bool, status string, maxConcurrency int64, coolingDown bool) {
		if maxConcurrency < -10000 || maxConcurrency > 10000 || len(status) > 128 {
			return
		}
		now := time.Now().UTC()
		var cooldownUntil *time.Time
		if coolingDown {
			value := now.Add(time.Minute)
			cooldownUntil = &value
		}
		unavailable := !memberEnabled || !providerEnabled || status != "active" || maxConcurrency < 1 || coolingDown
		if !unavailable {
			return
		}
		reasons := memberEligibilityReasons(poolMember{
			Enabled:         memberEnabled,
			ProviderEnabled: providerEnabled,
			Status:          status,
			MaxConcurrency:  int(maxConcurrency),
			CooldownUntil:   cooldownUntil,
		}, Request{Now: now})
		if len(reasons) == 0 {
			t.Fatalf("unavailable credential passed eligibility: enabled=%v provider=%v status=%q capacity=%d cooldown=%v", memberEnabled, providerEnabled, status, maxConcurrency, coolingDown)
		}
	})
}

func TestQuotaResetBoundaryRejectsExpiredSnapshot(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	resetAt := now
	observedAt := now
	remaining := int64(100)
	reasons := quotaEligibilityReasons(quotaSnapshot{Found: true, Remaining: &remaining, ResetAt: &resetAt, ObservedAt: &observedAt}, QuotaPolicy{Enabled: true, WindowKind: "minute", RequiredTokens: 1, MaxAge: time.Minute, RequireFresh: true}, now)
	if len(reasons) != 1 || reasons[0] != "quota_stale" {
		t.Fatalf("expired quota reasons = %v, want quota_stale", reasons)
	}
}

func TestOrderCandidatesWeightedRoundRobin(t *testing.T) {
	coordinator := NewMemoryCoordinator()
	t.Cleanup(func() { _ = coordinator.Close() })
	service := &Service{coordinator: coordinator, options: Options{CursorTTL: time.Minute}}
	versionID := uuid.New()
	targetID := uuid.New()
	firstID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	secondID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	values := []*candidate{
		{target: route.Target{ID: targetID, RouteVersionID: versionID}, member: poolMember{CredentialID: firstID}, combinedWeight: 1},
		{target: route.Target{ID: targetID, RouteVersionID: versionID}, member: poolMember{CredentialID: secondID}, combinedWeight: 2},
	}
	request := Request{
		Resolution: route.Resolution{Route: route.Route{ID: uuid.New()}, Version: route.Version{ID: versionID}},
		Strategy:   StrategyWeightedRoundRobin,
	}
	for index, want := range []uuid.UUID{firstID, secondID, secondID} {
		ordered, _, err := service.orderCandidates(context.Background(), request, append([]*candidate(nil), values...), uuid.Nil, false)
		if err != nil || ordered[0].member.CredentialID != want {
			t.Fatalf("weighted order %d = %s, err=%v, want %s", index, ordered[0].member.CredentialID, err, want)
		}
	}
}

func TestTargetEligibilityReasonsAreExplicit(t *testing.T) {
	target := route.Target{
		Enabled:                    false,
		ProviderModelEnabled:       false,
		ReviewStatus:               "pending",
		PoolEnabled:                false,
		ProviderResourceType:       "subscription",
		ProviderCommercialAllowed:  false,
		EffectiveCommercialAllowed: false,
		Protocol:                   "openai_chat",
		Capabilities:               model.Capabilities{Chat: true},
	}
	reasons := targetEligibilityReasons(target, "openai_responses", model.Capabilities{Chat: true, Tools: true})
	for _, reason := range []string{"target_disabled", "provider_model_disabled", "provider_model_unapproved", "pool_disabled", "resource_policy_denied", "protocol_mismatch", "capability_mismatch"} {
		found := false
		for _, actual := range reasons {
			if actual == reason {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("target eligibility reasons = %v, missing %q", reasons, reason)
		}
	}
}
