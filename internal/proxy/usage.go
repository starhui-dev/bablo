package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/apikey"
	"github.com/starhui-dev/bablo/internal/inference"
	"github.com/starhui-dev/bablo/internal/pricing"
	"github.com/starhui-dev/bablo/internal/route"
	"github.com/starhui-dev/bablo/internal/scheduler"
	"github.com/starhui-dev/bablo/internal/usage"
)

var errUsageUnavailable = errors.New("usage persistence is unavailable")

type requestUsageState struct {
	recorder UsageRecorder
	handle   usage.RequestHandle
	started  time.Time

	resolvedModelID *uuid.UUID
	routeVersionID  *uuid.UUID
	providerID      *uuid.UUID
	providerModelID *uuid.UUID
	credentialID    *uuid.UUID
	selected        bool

	priceVersionID *uuid.UUID
	currency       string
	done           bool
}

type usageCompletion struct {
	err        error
	cancelled  bool
	observed   usage.TokenUsage
	usageFound bool
	latency    time.Duration
	ttft       *time.Duration
}

func (h *Handler) beginUsage(ctx context.Context, r *http.Request, principal apikey.Principal, endpoint endpoint, parsed parsedRequest) (*requestUsageState, error) {
	if h == nil || h.usage == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now().UTC()
	input := usage.StartInput{
		RequestID:      requestID(r),
		UserID:         principal.UserID,
		APIKeyID:       principal.APIKeyID,
		Endpoint:       endpoint.path(),
		RequestedModel: parsed.Model,
		Stream:         parsed.Stream,
		StartedAt:      started,
	}
	handle, err := h.usage.BeginRequest(context.WithoutCancel(ctx), input)
	if err != nil {
		return nil, fmt.Errorf("%w: begin request record: %v", errUsageUnavailable, err)
	}
	if handle.TerminalStatus != "" {
		return nil, fmt.Errorf("%w: request %s is already %s", usage.ErrRequestAlreadyClosed, handle.RequestID, handle.TerminalStatus)
	}
	return &requestUsageState{recorder: h.usage, handle: handle, started: started}, nil
}

func (s *requestUsageState) requestRecordID() *uuid.UUID {
	if s == nil {
		return nil
	}
	return s.handle.RecordIDPointer()
}

func (s *requestUsageState) setResolution(resolution route.Resolution) {
	if s == nil {
		return
	}
	s.resolvedModelID = uuidPointer(resolution.ModelID)
	s.routeVersionID = uuidPointer(resolution.Version.ID)
}

func (s *requestUsageState) setSelection(selection scheduler.Selection) {
	if s == nil {
		return
	}
	s.selected = selection.CredentialID != uuid.Nil
	if selection.Target.ProviderID != uuid.Nil {
		s.providerID = uuidPointer(selection.Target.ProviderID)
	}
	if selection.Target.ProviderModelID != uuid.Nil {
		s.providerModelID = uuidPointer(selection.Target.ProviderModelID)
	}
	if selection.CredentialID != uuid.Nil {
		s.credentialID = uuidPointer(selection.CredentialID)
	}
	if selection.Target.RouteVersionID != uuid.Nil {
		s.routeVersionID = uuidPointer(selection.Target.RouteVersionID)
	}
}

func (s *requestUsageState) setPrice(snapshot pricing.Snapshot) {
	if s == nil {
		return
	}
	if snapshot.VersionID != uuid.Nil {
		s.priceVersionID = uuidPointer(snapshot.VersionID)
	}
	s.currency = snapshot.Currency
}

func (s *requestUsageState) finalize(ctx context.Context, completion usageCompletion) error {
	if s == nil || s.recorder == nil || s.done {
		return nil
	}
	if completion.latency <= 0 {
		completion.latency = time.Since(s.started)
		if completion.latency < 0 {
			completion.latency = 0
		}
	}
	status := usage.StatusSucceeded
	if completion.cancelled || errors.Is(completion.err, errClientCancelled) || (completion.err != nil && errors.Is(completion.err, context.Canceled) && ctx != nil && ctx.Err() != nil) {
		status = usage.StatusCancelled
	} else if completion.err != nil {
		status = usage.StatusFailed
	} else if !completion.usageFound {
		status = usage.StatusReconcileNeeded
	}
	provenance := usage.ProvenanceAdapter
	if !completion.usageFound {
		provenance = usage.ProvenanceMissingUsage
	}
	var upstreamStatus *int
	if completion.err != nil {
		var upstream *inference.UpstreamError
		if errors.As(completion.err, &upstream) && upstream != nil && upstream.HTTPStatus >= 100 && upstream.HTTPStatus <= 599 {
			value := upstream.HTTPStatus
			upstreamStatus = &value
		}
	}
	var attempt *usage.AttemptInput
	if s.selected {
		attempt = &usage.AttemptInput{
			AttemptNo:       0,
			RouteVersionID:  cloneUUID(s.routeVersionID),
			ProviderID:      cloneUUID(s.providerID),
			ProviderModelID: cloneUUID(s.providerModelID),
			CredentialID:    cloneUUID(s.credentialID),
			StartedAt:       s.started,
		}
	}
	input := usage.FinalizeInput{
		Attempt:         attempt,
		ResolvedModelID: cloneUUID(s.resolvedModelID),
		ProviderID:      cloneUUID(s.providerID),
		ProviderModelID: cloneUUID(s.providerModelID),
		RouteVersionID:  cloneUUID(s.routeVersionID),
		CredentialID:    cloneUUID(s.credentialID),
		PriceVersionID:  cloneUUID(s.priceVersionID),
		Usage:           completion.observed,
		Currency:        s.currency,
		Provenance:      provenance,
		TerminalStatus:  status,
		UpstreamStatus:  upstreamStatus,
		ErrorClass:      completionErrorClass(completion.err),
		Latency:         completion.latency,
		TTFT:            cloneDuration(completion.ttft),
		FinishedAt:      time.Now().UTC(),
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_, err := s.recorder.Finalize(context.WithoutCancel(ctx), s.handle, input)
	if err != nil {
		return fmt.Errorf("%w: finalize usage event: %v", errUsageUnavailable, err)
	}
	s.done = true
	return nil
}
func finalizeUsage(r *http.Request, state *requestUsageState, completion usageCompletion) error {
	if state == nil {
		return nil
	}
	ctx := context.Background()
	if r != nil && r.Context() != nil {
		ctx = r.Context()
	}
	return state.finalize(ctx, completion)
}

func finalizeUsageFailure(r *http.Request, state *requestUsageState, err error) error {
	cancelled := r != nil && r.Context() != nil && r.Context().Err() != nil
	return finalizeUsage(r, state, usageCompletion{err: err, cancelled: cancelled})
}
func usageRecordID(state *requestUsageState) *uuid.UUID {
	if state == nil {
		return nil
	}
	return state.requestRecordID()
}

func completionErrorClass(err error) string {
	if err == nil {
		return ""
	}
	return errorClass(err)
}

func uuidPointer(value uuid.UUID) *uuid.UUID {
	if value == uuid.Nil {
		return nil
	}
	copy := value
	return &copy
}

func cloneUUID(value *uuid.UUID) *uuid.UUID {
	if value == nil || *value == uuid.Nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneDuration(value *time.Duration) *time.Duration {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
