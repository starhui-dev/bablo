package scheduler

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/credential"
	"github.com/starhui-dev/bablo/internal/model"
	"github.com/starhui-dev/bablo/internal/provider"
	"github.com/starhui-dev/bablo/internal/route"
)

// Service owns scheduler eligibility, ordering, lease acquisition, and
// decision persistence. It never decrypts or receives credential secrets.
type Service struct {
	repository  *Repository
	coordinator Coordinator
	options     Options
}

// Options controls Scheduler transient state behavior.
type Options struct {
	LeaseTTL    time.Duration
	AffinityTTL time.Duration
	CursorTTL   time.Duration
	QuotaMaxAge time.Duration
}

func (o Options) withDefaults() (Options, error) {
	if o.LeaseTTL == 0 {
		o.LeaseTTL = defaultLeaseTTL
	}
	if o.AffinityTTL == 0 {
		o.AffinityTTL = defaultAffinityTTL
	}
	if o.CursorTTL == 0 {
		o.CursorTTL = defaultCursorTTL
	}
	if o.QuotaMaxAge == 0 {
		o.QuotaMaxAge = defaultQuotaMaxAge
	}
	if o.LeaseTTL <= 0 || o.AffinityTTL <= 0 || o.CursorTTL <= 0 || o.QuotaMaxAge <= 0 {
		return Options{}, ErrInvalidInput
	}
	return o, nil
}

// NewService constructs a Scheduler service.
func NewService(repository *Repository, coordinator Coordinator, options Options) (*Service, error) {
	if repository == nil || coordinator == nil {
		return nil, ErrInvalidInput
	}
	options, err := options.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Service{repository: repository, coordinator: coordinator, options: options}, nil
}

// Select evaluates all fixed route candidates, acquires one credential lease,
// and appends one immutable decision record even when no credential is usable.
func (s *Service) Select(ctx context.Context, request Request) (Selection, error) {
	request, err := normalizeRequest(request, s.options.QuotaMaxAge)
	if err != nil {
		return Selection{}, err
	}
	decision := Decision{
		StrategyVersion: strategyVersionBase + "/" + string(request.Strategy),
		Candidates:      make([]CandidateDecision, 0),
		FallbackChain:   make([]FallbackEntry, 0),
	}

	eligible := make([]*candidate, 0)
	for _, target := range request.Resolution.Candidates {
		targetReasons := targetEligibilityReasons(target, request.Protocol, request.RequiredCapabilities)
		if len(targetReasons) > 0 {
			decision.Candidates = append(decision.Candidates, CandidateDecision{
				TargetID:       target.ID,
				TargetPriority: target.Priority,
				Weight:         int64(target.Weight),
				Eligible:       false,
				Reasons:        targetReasons,
			})
			continue
		}
		members, err := s.repository.loadPoolMembers(ctx, target.CredentialPoolID, request.Quota)
		if err != nil {
			return Selection{Decision: decision}, err
		}
		if len(members) == 0 {
			decision.Candidates = append(decision.Candidates, CandidateDecision{
				TargetID:       target.ID,
				TargetPriority: target.Priority,
				Weight:         int64(target.Weight),
				Eligible:       false,
				Reasons:        []string{"pool_empty"},
			})
			continue
		}
		for _, member := range members {
			candidateValue := candidate{
				target:   target,
				member:   member,
				leaseKey: member.CredentialID.String(),
			}
			candidateValue.combinedWeight = int64(target.Weight) * int64(member.MemberWeight)
			if candidateValue.combinedWeight < 1 {
				candidateValue.combinedWeight = 1
			}
			candidateValue.decision = CandidateDecision{
				TargetID:       target.ID,
				CredentialID:   uuidPointer(member.CredentialID),
				TargetPriority: target.Priority,
				MemberPriority: member.MemberPriority,
				Weight:         candidateValue.combinedWeight,
				Score:          int64(1_000_000 - target.Priority),
				Eligible:       true,
			}
			candidateValue.decision.Reasons = memberEligibilityReasons(member, request)
			if len(candidateValue.decision.Reasons) > 0 {
				candidateValue.decision.Eligible = false
			}
			if member.Quota.Remaining != nil {
				value := *member.Quota.Remaining
				candidateValue.decision.QuotaRemaining = &value
			}
			if member.Quota.ResetAt != nil {
				value := *member.Quota.ResetAt
				candidateValue.decision.QuotaResetAt = &value
			}
			if member.Quota.ObservedAt != nil {
				value := *member.Quota.ObservedAt
				candidateValue.decision.QuotaObservedAt = &value
			}
			candidateValue.decisionIndex = len(decision.Candidates)
			decision.Candidates = append(decision.Candidates, candidateValue.decision)
			if candidateValue.decision.Eligible {
				eligible = append(eligible, &candidateValue)
			}
		}
	}

	if len(eligible) == 0 {
		if err := s.repository.RecordDecision(ctx, request, decision); err != nil {
			return Selection{Decision: decision}, err
		}
		return Selection{Decision: decision}, ErrNoEligible
	}

	affinityState := scopedAffinityKey(request)
	affinityID, affinityFound, affinityErr := s.readAffinity(ctx, affinityState)
	if affinityErr != nil {
		decision.FallbackChain = append(decision.FallbackChain, FallbackEntry{Reason: "affinity_state_unavailable"})
	}
	ordered, affinityUsed, err := s.orderCandidates(ctx, request, eligible, affinityID, affinityFound)
	if err != nil {
		decision.FallbackChain = append(decision.FallbackChain, FallbackEntry{Reason: "cursor_state_unavailable"})
		if recordErr := s.repository.RecordDecision(ctx, request, decision); recordErr != nil {
			return Selection{Decision: decision}, recordErr
		}
		return Selection{Decision: decision}, err
	}
	if affinityFound && !affinityUsed {
		decision.FallbackChain = append(decision.FallbackChain, FallbackEntry{CredentialID: uuidPointer(affinityID), Reason: "affinity_unavailable"})
	}

	owner := uuid.New().String()
	var selected *candidate
	var selectedLease Lease
	for _, item := range ordered {
		for slot := 0; slot < item.member.MaxConcurrency; slot++ {
			resource := item.leaseKey + ":" + strconv.Itoa(slot)
			lease, acquireErr := s.coordinator.Acquire(ctx, resource, owner, s.options.LeaseTTL)
			if acquireErr == nil {
				selected = item
				selectedLease = lease
				break
			}
			if errors.Is(acquireErr, ErrLeaseBusy) {
				continue
			}
			decision.Candidates[item.decisionIndex].Eligible = false
			decision.Candidates[item.decisionIndex].Reasons = append(decision.Candidates[item.decisionIndex].Reasons, "concurrency_state_unavailable")
			decision.FallbackChain = append(decision.FallbackChain, FallbackEntry{
				TargetID:     uuidPointer(item.target.ID),
				CredentialID: uuidPointer(item.member.CredentialID),
				Reason:       "concurrency_state_unavailable",
			})
			if recordErr := s.repository.RecordDecision(ctx, request, decision); recordErr != nil {
				return Selection{Decision: decision}, recordErr
			}
			return Selection{Decision: decision}, acquireErr
		}
		if selected != nil {
			break
		}
		decision.Candidates[item.decisionIndex].Eligible = false
		decision.Candidates[item.decisionIndex].Reasons = append(decision.Candidates[item.decisionIndex].Reasons, "concurrency_lease_busy")
		decision.FallbackChain = append(decision.FallbackChain, FallbackEntry{
			TargetID:     uuidPointer(item.target.ID),
			CredentialID: uuidPointer(item.member.CredentialID),
			Reason:       "concurrency_lease_busy",
		})
	}
	if selected == nil {
		if err := s.repository.RecordDecision(ctx, request, decision); err != nil {
			return Selection{Decision: decision}, err
		}
		return Selection{Decision: decision}, ErrNoEligible
	}

	decision.SelectedTargetID = uuidPointer(selected.target.ID)
	decision.SelectedProviderID = uuidPointer(selected.target.ProviderID)
	decision.SelectedCredentialID = uuidPointer(selected.member.CredentialID)
	if affinityState != "" {
		if err := s.coordinator.SetAffinity(ctx, affinityState, selected.member.CredentialID, s.options.AffinityTTL); err != nil {
			decision.FallbackChain = append(decision.FallbackChain, FallbackEntry{Reason: "affinity_state_unavailable"})
		}
	}
	if err := s.repository.RecordDecision(ctx, request, decision); err != nil {
		_ = selectedLease.Release(context.WithoutCancel(ctx))
		return Selection{Decision: decision}, err
	}
	return Selection{
		Target:       selected.target,
		CredentialID: selected.member.CredentialID,
		Lease:        selectedLease,
		Decision:     decision,
	}, nil
}

func (s *Service) readAffinity(ctx context.Context, key string) (uuid.UUID, bool, error) {
	if key == "" {
		return uuid.Nil, false, nil
	}
	return s.coordinator.GetAffinity(ctx, key)
}

func targetEligibilityReasons(target route.Target, protocol string, required model.Capabilities) []string {
	reasons := make([]string, 0, 7)
	if !target.Enabled {
		reasons = append(reasons, "target_disabled")
	}
	if !target.ProviderModelEnabled {
		reasons = append(reasons, "provider_model_disabled")
	}
	if target.ReviewStatus != provider.ReviewApproved {
		reasons = append(reasons, "provider_model_unapproved")
	}
	if !target.PoolEnabled {
		reasons = append(reasons, "pool_disabled")
	}
	if target.ProviderResourceType == provider.ResourceSubscription || !target.ProviderCommercialAllowed || !target.EffectiveCommercialAllowed {
		reasons = append(reasons, "resource_policy_denied")
	}
	if protocol != "" && target.Protocol != protocol {
		reasons = append(reasons, "protocol_mismatch")
	}
	if !supportsRequired(target.Capabilities, required) {
		reasons = append(reasons, "capability_mismatch")
	}
	return reasons
}

func memberEligibilityReasons(member poolMember, request Request) []string {
	reasons := make([]string, 0, 7)
	if !member.Enabled {
		reasons = append(reasons, "pool_member_disabled")
	}
	if !member.ProviderEnabled {
		reasons = append(reasons, "provider_disabled")
	}
	if member.Status != credential.StatusActive {
		reasons = append(reasons, "credential_inactive")
	}
	if member.MaxConcurrency < 1 {
		reasons = append(reasons, "credential_capacity_invalid")
	}
	if member.CooldownUntil != nil && member.CooldownUntil.After(request.Now) {
		reasons = append(reasons, "cooldown")
	}
	if request.Region != "" && member.Region != request.Region {
		reasons = append(reasons, "region_mismatch")
	}
	if request.RequireProxy && member.ProxyRef == "" {
		reasons = append(reasons, "proxy_required")
	}
	if request.ProxyRef != "" && member.ProxyRef != request.ProxyRef {
		reasons = append(reasons, "proxy_mismatch")
	}
	if request.Quota.Enabled {
		reasons = append(reasons, quotaEligibilityReasons(member.Quota, request.Quota, request.Now)...)
	}
	return reasons
}

func quotaEligibilityReasons(snapshot quotaSnapshot, policy QuotaPolicy, now time.Time) []string {
	reasons := make([]string, 0, 2)
	if !snapshot.Found || snapshot.ObservedAt == nil {
		if policy.RequireFresh {
			return append(reasons, "quota_missing")
		}
		return reasons
	}
	stale := now.Before(*snapshot.ObservedAt) || now.Sub(*snapshot.ObservedAt) > policy.MaxAge
	if snapshot.ResetAt != nil && !snapshot.ResetAt.After(now) {
		stale = true
	}
	if stale && policy.RequireFresh {
		reasons = append(reasons, "quota_stale")
		return reasons
	}
	if !stale && snapshot.Remaining != nil && *snapshot.Remaining < policy.RequiredTokens {
		reasons = append(reasons, "quota_exhausted")
	}
	return reasons
}

func supportsRequired(available, required model.Capabilities) bool {
	return (!required.Chat || available.Chat) &&
		(!required.Responses || available.Responses) &&
		(!required.Messages || available.Messages) &&
		(!required.Stream || available.Stream) &&
		(!required.Tools || available.Tools) &&
		(!required.Vision || available.Vision) &&
		(!required.Reasoning || available.Reasoning)
}

func (s *Service) orderCandidates(ctx context.Context, request Request, values []*candidate, affinityID uuid.UUID, affinityFound bool) ([]*candidate, bool, error) {
	groups := groupCandidates(values)
	ordered := make([]*candidate, 0, len(values))
	affinityUsed := false
	for groupIndex, group := range groups {
		if affinityFound && groupIndex == 0 {
			for index, item := range group {
				if item.member.CredentialID == affinityID {
					group[0], group[index] = group[index], group[0]
					affinityUsed = true
					break
				}
			}
		}
		if !affinityUsed || groupIndex != 0 {
			switch request.Strategy {
			case StrategyFillFirst:
				// Stable group order is already fill-first.
			case StrategyQuotaAware:
				sort.SliceStable(group, func(i, j int) bool { return quotaCandidateLess(group[i], group[j], request) })
			case StrategyRoundRobin:
				key := cursorKey(request.Resolution.Version.ID, request.Strategy, group[0].target.Priority, group[0].member.MemberPriority)
				start, cursorErr := s.coordinator.Next(ctx, key, int64(len(group)), s.options.CursorTTL)
				if cursorErr != nil {
					return nil, affinityUsed, cursorErr
				}
				rotate(group, int(start))
			case StrategyWeightedRoundRobin:
				key := cursorKey(request.Resolution.Version.ID, request.Strategy, group[0].target.Priority, group[0].member.MemberPriority)
				slot, cursorErr := s.coordinator.Next(ctx, key, totalWeight(group), s.options.CursorTTL)
				if cursorErr != nil {
					return nil, affinityUsed, cursorErr
				}
				rotate(group, weightedStart(group, slot))
			default:
				return nil, affinityUsed, ErrInvalidInput
			}
		}
		ordered = append(ordered, group...)
	}
	return ordered, affinityUsed, nil
}

func groupCandidates(values []*candidate) [][]*candidate {
	sort.SliceStable(values, func(i, j int) bool {
		left, right := values[i], values[j]
		if left.target.Priority != right.target.Priority {
			return left.target.Priority < right.target.Priority
		}
		if left.member.MemberPriority != right.member.MemberPriority {
			return left.member.MemberPriority < right.member.MemberPriority
		}
		if left.target.TargetNo != right.target.TargetNo {
			return left.target.TargetNo < right.target.TargetNo
		}
		if left.target.ID != right.target.ID {
			return left.target.ID.String() < right.target.ID.String()
		}
		return left.member.CredentialID.String() < right.member.CredentialID.String()
	})
	groups := make([][]*candidate, 0)
	for _, value := range values {
		if len(groups) == 0 {
			groups = append(groups, []*candidate{value})
			continue
		}
		last := groups[len(groups)-1]
		if last[0].target.Priority == value.target.Priority && last[0].member.MemberPriority == value.member.MemberPriority {
			groups[len(groups)-1] = append(last, value)
		} else {
			groups = append(groups, []*candidate{value})
		}
	}
	return groups
}

func quotaCandidateLess(left, right *candidate, request Request) bool {
	leftUsable := quotaUsable(left.member.Quota, request.Quota, request.Now)
	rightUsable := quotaUsable(right.member.Quota, request.Quota, request.Now)
	if leftUsable != rightUsable {
		return leftUsable
	}
	if leftUsable && left.member.Quota.Remaining != nil && right.member.Quota.Remaining != nil && *left.member.Quota.Remaining != *right.member.Quota.Remaining {
		return *left.member.Quota.Remaining > *right.member.Quota.Remaining
	}
	if leftUsable && (left.member.Quota.ResetAt != nil || right.member.Quota.ResetAt != nil) {
		if left.member.Quota.ResetAt == nil {
			return false
		}
		if right.member.Quota.ResetAt == nil {
			return true
		}
		if !left.member.Quota.ResetAt.Equal(*right.member.Quota.ResetAt) {
			return left.member.Quota.ResetAt.Before(*right.member.Quota.ResetAt)
		}
	}
	return candidateLess(left, right)
}

func candidateLess(left, right *candidate) bool {
	if left.member.MemberWeight != right.member.MemberWeight {
		return left.member.MemberWeight > right.member.MemberWeight
	}
	return left.member.CredentialID.String() < right.member.CredentialID.String()
}

func totalWeight(values []*candidate) int64 {
	var total int64
	for _, value := range values {
		total += value.combinedWeight
	}
	if total < 1 {
		return 1
	}
	return total
}

func weightedStart(values []*candidate, slot int64) int {
	for index, value := range values {
		if slot < value.combinedWeight {
			return index
		}
		slot -= value.combinedWeight
	}
	return 0
}

func rotate(values []*candidate, start int) {
	if len(values) == 0 {
		return
	}
	start %= len(values)
	if start <= 0 {
		return
	}
	rotated := append(append([]*candidate(nil), values[start:]...), values[:start]...)
	copy(values, rotated)
}

func cursorKey(versionID uuid.UUID, strategy Strategy, targetPriority, memberPriority int) string {
	return versionID.String() + ":" + string(strategy) + ":" + strconv.Itoa(targetPriority) + ":" + strconv.Itoa(memberPriority)
}

func quotaUsable(snapshot quotaSnapshot, policy QuotaPolicy, now time.Time) bool {
	if !snapshot.Found || snapshot.ObservedAt == nil {
		return false
	}
	if now.Before(*snapshot.ObservedAt) || now.Sub(*snapshot.ObservedAt) > policy.MaxAge {
		return false
	}
	return snapshot.ResetAt == nil || snapshot.ResetAt.After(now)
}

func scopedAffinityKey(request Request) string {
	if request.AffinityKey == "" {
		return ""
	}
	return request.Resolution.Version.ID.String() + ":" + request.AffinityKey
}

func normalizeRequest(request Request, defaultQuotaMaxAge time.Duration) (Request, error) {
	request.RequestID = strings.TrimSpace(request.RequestID)
	if request.RequestID == "" || len(request.RequestID) > 128 || strings.ContainsAny(request.RequestID, "\r\n\x00") {
		return Request{}, ErrInvalidInput
	}
	if request.AttemptNo < 0 || request.DecisionNo < 0 || request.Resolution.Version.ID == uuid.Nil || request.Resolution.Route.ID == uuid.Nil || len(request.Resolution.Candidates) == 0 {
		return Request{}, ErrInvalidInput
	}
	if request.Strategy == "" {
		request.Strategy = StrategyRoundRobin
	}
	switch request.Strategy {
	case StrategyRoundRobin, StrategyWeightedRoundRobin, StrategyFillFirst, StrategyQuotaAware:
	default:
		return Request{}, ErrInvalidInput
	}
	request.Protocol = strings.TrimSpace(request.Protocol)
	request.Region = strings.TrimSpace(request.Region)
	request.ProxyRef = strings.TrimSpace(request.ProxyRef)
	request.AffinityKey = strings.TrimSpace(request.AffinityKey)
	if len(request.Protocol) > 64 || len(request.Region) > 64 || len(request.ProxyRef) > 128 || len(request.AffinityKey) > 256 || strings.ContainsAny(request.Protocol+request.Region+request.ProxyRef+request.AffinityKey, "\r\n\x00") {
		return Request{}, ErrInvalidInput
	}
	if request.ProxyRef != "" {
		request.RequireProxy = true
	}
	if request.Now.IsZero() {
		request.Now = time.Now().UTC()
	} else {
		request.Now = request.Now.UTC()
	}
	if request.Quota.Enabled {
		switch request.Quota.WindowKind {
		case "minute", "hour", "day", "month", "provider_specific":
		default:
			return Request{}, ErrInvalidInput
		}
		if request.Quota.RequiredTokens < 0 {
			return Request{}, ErrInvalidInput
		}
		if request.Quota.MaxAge == 0 {
			request.Quota.MaxAge = defaultQuotaMaxAge
		}
		if request.Quota.MaxAge <= 0 {
			return Request{}, ErrInvalidInput
		}
	} else if request.Quota.WindowKind != "" || request.Quota.RequiredTokens != 0 || request.Quota.MaxAge != 0 || request.Quota.RequireFresh {
		return Request{}, ErrInvalidInput
	}
	return request, nil
}

func uuidPointer(value uuid.UUID) *uuid.UUID {
	return &value
}
