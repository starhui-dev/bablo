package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

import (
	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/apikey"
	"github.com/starhui-dev/bablo/internal/credential"
	"github.com/starhui-dev/bablo/internal/httpapi"
	"github.com/starhui-dev/bablo/internal/inference"
	catalogmodel "github.com/starhui-dev/bablo/internal/model"
	"github.com/starhui-dev/bablo/internal/pricing"
	"github.com/starhui-dev/bablo/internal/provider"
	"github.com/starhui-dev/bablo/internal/route"
	"github.com/starhui-dev/bablo/internal/scheduler"
	"github.com/starhui-dev/bablo/internal/usage"
)

var (
	errInferenceUnavailable = errors.New("inference service is unavailable")
	errStreamingUnsupported = errors.New("streaming response is unavailable")
	errStreamIncomplete     = errors.New("upstream stream ended without a terminal event")
	errEmptyStream          = errors.New("upstream stream returned no payload")
	errMalformedResponse    = errors.New("upstream returned an invalid JSON response")
	errClientCancelled      = errors.New("client cancelled the request")
	errInvalidRequest       = errors.New("invalid proxy request")
	errRequestTooLarge      = errors.New("proxy request body is too large")
	errMethodNotAllowed     = errors.New("method not allowed")
	errProxyNotFound        = errors.New("proxy endpoint not found")
)

// ServeHTTP dispatches the three P0 inference endpoints.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if httpapi.RequestID(r.Context()) == "" {
		if r.Header == nil {
			r.Header = make(http.Header)
		}
		r.Header.Set("X-Request-ID", requestID(r))
	}
	if id := requestID(r); w.Header().Get("X-Request-ID") == "" {
		w.Header().Set("X-Request-ID", id)
	}
	switch r.URL.Path {
	case modelsPath:
		if r.Method != http.MethodGet {
			h.methodNotAllowed(w, r, http.MethodGet)
			return
		}
		h.listModels(w, r)
	case chatCompletionsPath:
		if r.Method != http.MethodPost {
			h.methodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.complete(w, r, endpointChat)
	case responsesPath:
		if r.Method != http.MethodPost {
			h.methodNotAllowed(w, r, http.MethodPost)
			return
		}
		h.complete(w, r, endpointResponses)
	default:
		writeProxyError(w, r, errProxyNotFound)
	}

}

type endpoint uint8

const (
	endpointChat endpoint = iota + 1
	endpointResponses
)

func (e endpoint) path() string {
	if e == endpointResponses {
		return responsesPath
	}
	return chatCompletionsPath
}

func (e endpoint) protocol() string {
	if e == endpointResponses {
		return provider.ProtocolOpenAIResponses
	}
	return provider.ProtocolOpenAIChat
}

func (e endpoint) format() string {
	if e == endpointResponses {
		return "openai-response"
	}
	return "openai"
}

type parsedRequest struct {
	Model     string
	Stream    bool
	Tools     bool
	Reasoning bool
}

func (h *Handler) resolvePrincipal(ctx context.Context) (apikey.Principal, bool) {
	if h != nil && h.principal != nil {
		return h.principal(ctx)
	}
	return apikey.PrincipalFromContext(ctx)
}

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.resolvePrincipal(r.Context())
	if !ok {
		writeProxyError(w, r, apikey.ErrInvalidKey)
		return
	}
	lister, ok := h.keys.(AuthorizedModelLister)
	if !ok {
		writeProxyError(w, r, errInferenceUnavailable)
		return
	}
	authorized, err := lister.ListAuthorizedModels(r.Context(), principal)
	if err != nil {
		writeProxyError(w, r, err)
		return
	}
	allowed := make(map[string]struct{}, len(authorized))
	for _, value := range authorized {
		value = strings.TrimSpace(value)
		if value != "" {
			allowed[value] = struct{}{}
		}
	}
	models := make([]openAIModel, 0, len(allowed))
	if len(allowed) > 0 {
		cursor := ""
		seenCursors := make(map[string]struct{})
		for {
			page, pageErr := h.models.ListPublic(r.Context(), cursor, 100)
			if pageErr != nil {
				writeProxyError(w, r, pageErr)
				return
			}
			for _, value := range page.Models {
				if _, exists := allowed[value.PublicID]; !exists {
					continue
				}
				models = append(models, openAIModel{
					ID:      value.PublicID,
					Object:  "model",
					Created: value.CreatedAt.Unix(),
					OwnedBy: "bablo",
				})
			}
			if page.NextCursor == "" {
				break
			}
			if _, exists := seenCursors[page.NextCursor]; exists {
				writeProxyError(w, r, errMalformedResponse)
				return
			}
			seenCursors[page.NextCursor] = struct{}{}
			cursor = page.NextCursor
			if len(seenCursors) > 1000 {
				writeProxyError(w, r, errMalformedResponse)
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   models,
	})
}

type openAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (h *Handler) complete(w http.ResponseWriter, r *http.Request, e endpoint) {
	principal, ok := h.resolvePrincipal(r.Context())
	if !ok {
		writeProxyError(w, r, apikey.ErrInvalidKey)
		return
	}
	body, parsed, err := h.readRequest(w, r)
	if err != nil {
		writeProxyError(w, r, err)
		return
	}
	publicModel, err := h.models.ResolvePublic(r.Context(), parsed.Model)
	if err != nil {
		writeProxyError(w, r, err)
		return
	}
	if err := h.keys.Authorize(r.Context(), principal, publicModel.PublicID, estimateTokens(body)); err != nil {
		writeProxyError(w, r, err)
		return
	}
	usageState, err := h.beginUsage(r.Context(), r, principal, e, parsed)
	if err != nil {
		writeProxyError(w, r, err)
		return
	}
	if h.routes == nil || h.scheduler == nil || h.engine == nil {
		if finalizeErr := finalizeUsageFailure(r, usageState, errInferenceUnavailable); finalizeErr != nil {
			writeProxyError(w, r, finalizeErr)
			return
		}
		writeProxyError(w, r, errInferenceUnavailable)
		return
	}
	resolution, err := h.routes.Resolve(r.Context(), publicModel.PublicID)
	if err != nil {
		if finalizeErr := finalizeUsageFailure(r, usageState, err); finalizeErr != nil {
			writeProxyError(w, r, finalizeErr)
			return
		}
		writeProxyError(w, r, err)
		return
	}
	if usageState != nil {
		usageState.setResolution(resolution)
	}
	required := requiredCapabilities(e, parsed)
	selection, err := h.scheduler.Select(r.Context(), scheduler.Request{
		RequestID:            requestID(r),
		RequestRecordID:      usageRecordID(usageState),
		Resolution:           resolution,
		Protocol:             e.protocol(),
		RequiredCapabilities: required,
		Strategy:             h.strategy,
		AffinityKey:          principal.APIKeyID.String() + ":" + publicModel.PublicID,
		Now:                  h.now().UTC(),
	})
	if err != nil {
		if finalizeErr := finalizeUsageFailure(r, usageState, err); finalizeErr != nil {
			writeProxyError(w, r, finalizeErr)
			return
		}
		writeProxyError(w, r, err)
		return
	}
	if usageState != nil {
		usageState.setSelection(selection)
	}
	keeper := newLeaseKeeper(&selection, h.leaseTTL, h.logger)
	keeper.Start()
	defer func() {
		keeper.Stop()
		if releaseErr := selection.Release(context.WithoutCancel(r.Context())); releaseErr != nil {
			h.logger.Error("proxy_lease_release_error", "request_id", requestID(r), "error_class", errorClass(releaseErr))
		}
	}()
	if h.prices != nil {
		providerModelID := uuidPointer(selection.Target.ProviderModelID)
		snapshot, priceErr := h.prices.ResolveSnapshot(r.Context(), resolution.ModelID, providerModelID, h.now().UTC())
		if priceErr != nil {
			if finalizeErr := finalizeUsageFailure(r, usageState, priceErr); finalizeErr != nil {
				writeProxyError(w, r, finalizeErr)
				return
			}
			writeProxyError(w, r, priceErr)
			return
		}
		if usageState != nil {
			usageState.setPrice(snapshot)
		}
	}
	inferenceRequest := inference.Request{
		RequestID: requestID(r),
		ResolvedRoute: inference.ResolvedRoute{
			RouteID:        resolution.Route.ID.String(),
			RouteVersionID: resolution.Version.ID.String(),
			ProviderID:     selectedProvider(selection.Target),
			CredentialID:   selection.CredentialID.String(),
			RequestedModel: parsed.Model,
			ResolvedModel:  selection.Target.UpstreamModelID,
		},
		SourceFormat:   e.format(),
		ResponseFormat: e.format(),
		Headers:        filteredRequestHeaders(r, requestID(r)),
		Metadata: map[string]any{
			"requested_model": parsed.Model,
			"request_path":    e.path(),
			"stream":          parsed.Stream,
		},
		Body:   body,
		Stream: parsed.Stream,
	}
	if parsed.Stream {
		h.executeStream(w, r, e, principal, selection, inferenceRequest, usageState)
		return
	}
	h.executeJSON(w, r, principal, selection, inferenceRequest, usageState)
}
func (h *Handler) readRequest(w http.ResponseWriter, r *http.Request) ([]byte, parsedRequest, error) {
	if r.Body == nil {
		return nil, parsedRequest{}, errInvalidRequest
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, parsedRequest{}, errRequestTooLarge
		}
		return nil, parsedRequest{}, errInvalidRequest
	}
	if len(body) == 0 || !json.Valid(body) {
		return nil, parsedRequest{}, errInvalidRequest
	}
	var raw struct {
		Model           json.RawMessage `json:"model"`
		Stream          json.RawMessage `json:"stream"`
		Tools           json.RawMessage `json:"tools"`
		Reasoning       json.RawMessage `json:"reasoning"`
		ReasoningEffort json.RawMessage `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, parsedRequest{}, errInvalidRequest
	}
	if len(raw.Model) == 0 || bytes.Equal(bytes.TrimSpace(raw.Model), []byte("null")) {
		return nil, parsedRequest{}, errInvalidRequest
	}
	var modelName string
	if err := json.Unmarshal(raw.Model, &modelName); err != nil || strings.TrimSpace(modelName) == "" {
		return nil, parsedRequest{}, errInvalidRequest
	}
	parsed := parsedRequest{Model: strings.TrimSpace(modelName)}
	if len(raw.Stream) > 0 && !bytes.Equal(bytes.TrimSpace(raw.Stream), []byte("null")) {
		if err := json.Unmarshal(raw.Stream, &parsed.Stream); err != nil {
			return nil, parsedRequest{}, errInvalidRequest
		}
	}
	parsed.Tools = presentJSON(raw.Tools)
	parsed.Reasoning = presentJSON(raw.Reasoning) || reasoningRequested(raw.ReasoningEffort)
	return body, parsed, nil
}

func presentJSON(value json.RawMessage) bool {
	value = bytes.TrimSpace(value)
	return len(value) > 0 && !bytes.Equal(value, []byte("null"))
}

func reasoningRequested(value json.RawMessage) bool {
	if !presentJSON(value) {
		return false
	}
	var text string
	if json.Unmarshal(value, &text) == nil && strings.EqualFold(strings.TrimSpace(text), "none") {
		return false
	}
	return true
}

func requiredCapabilities(e endpoint, request parsedRequest) catalogmodel.Capabilities {
	result := catalogmodel.Capabilities{Stream: request.Stream, Tools: request.Tools, Reasoning: request.Reasoning}
	if e == endpointResponses {
		result.Responses = true
	} else {
		result.Chat = true
	}
	return result
}

func selectedProvider(target route.Target) string {
	if value := strings.TrimSpace(target.ProviderSlug); value != "" {
		return value
	}
	return target.ProviderID.String()
}

func estimateTokens(body []byte) int64 {
	if len(body) == 0 {
		return 0
	}
	value := int64((len(body) + 3) / 4)
	if value < 1 {
		return 1
	}
	return value
}

func (h *Handler) executeJSON(w http.ResponseWriter, r *http.Request, _ apikey.Principal, selection scheduler.Selection, request inference.Request, usageState *requestUsageState) {
	result, err := h.engine.Execute(r.Context(), request)
	if err != nil {
		finalizeErr := finalizeUsageFailure(r, usageState, err)
		h.reportResult(r.Context(), selection, err)
		if finalizeErr != nil {
			writeProxyError(w, r, finalizeErr)
			return
		}
		writeProxyError(w, r, err)
		return
	}
	status := result.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	observedUsage, usageFound := usage.ExtractJSON(result.Body)
	if status < 200 || status >= 300 || len(bytes.TrimSpace(result.Body)) == 0 || !json.Valid(result.Body) {
		if status < 200 || status >= 300 {
			err = &inference.UpstreamError{Class: "upstream_response", HTTPStatus: status}
		} else {
			err = errMalformedResponse
		}
		finalizeErr := finalizeUsage(r, usageState, usageCompletion{err: err, observed: observedUsage, usageFound: usageFound})
		h.reportResult(r.Context(), selection, err)
		if finalizeErr != nil {
			writeProxyError(w, r, finalizeErr)
			return
		}
		writeProxyError(w, r, err)
		return
	}
	finalizeErr := finalizeUsage(r, usageState, usageCompletion{observed: observedUsage, usageFound: usageFound})
	h.reportResult(r.Context(), selection, nil)
	if finalizeErr != nil {
		writeProxyError(w, r, finalizeErr)
		return
	}
	writeSafeResponseHeaders(w, result.Headers)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(result.Body)
}

func (h *Handler) executeStream(w http.ResponseWriter, r *http.Request, e endpoint, _ apikey.Principal, selection scheduler.Selection, request inference.Request, usageState *requestUsageState) {
	stream, err := h.engine.ExecuteStream(r.Context(), request)
	if err != nil {
		finalizeErr := finalizeUsageFailure(r, usageState, err)
		h.reportResult(r.Context(), selection, err)
		if finalizeErr != nil {
			writeProxyError(w, r, finalizeErr)
			return
		}
		writeProxyError(w, r, err)
		return
	}
	outcome := h.writeStream(w, r, e, stream)
	finalizeErr := finalizeUsage(r, usageState, usageCompletion{
		err:        outcome.err,
		cancelled:  outcome.cancelled,
		observed:   outcome.observed,
		usageFound: outcome.usageFound,
		ttft:       outcome.ttft,
	})
	if outcome.cancelled {
		if finalizeErr != nil {
			h.logger.Error("proxy_usage_finalize_error", "request_id", requestID(r), "error_class", errorClass(finalizeErr))
		}
		return
	}
	if outcome.err != nil {
		h.reportResult(r.Context(), selection, outcome.err)
		if finalizeErr != nil {
			if outcome.wroteHeaders {
				writeSSEError(w, r, finalizeErr)
			} else {
				writeProxyError(w, r, finalizeErr)
			}
			return
		}
		if !outcome.wroteHeaders {
			writeProxyError(w, r, outcome.err)
		}
		return
	}
	h.reportResult(r.Context(), selection, nil)
	if finalizeErr != nil {
		if outcome.wroteHeaders {
			writeSSEError(w, r, finalizeErr)
		} else {
			writeProxyError(w, r, finalizeErr)
		}
	}
}

type streamOutcome struct {
	err          error
	cancelled    bool
	wroteHeaders bool
	observed     usage.TokenUsage
	usageFound   bool
	ttft         *time.Duration
}

func (h *Handler) writeStream(w http.ResponseWriter, r *http.Request, e endpoint, stream inference.Stream) (outcome streamOutcome) {
	streamStarted := time.Now()
	var accumulator usage.Accumulator
	var firstPayloadAt time.Time
	defer func() {
		outcome.observed, outcome.usageFound = accumulator.Value()
		if !firstPayloadAt.IsZero() {
			ttft := firstPayloadAt.Sub(streamStarted)
			if ttft < 0 {
				ttft = 0
			}
			outcome.ttft = &ttft
		}
	}()
	if stream == nil {
		return streamOutcome{err: errEmptyStream}
	}
	defer stream.Close()
	flusher, ok := w.(http.Flusher)
	if !ok {
		return streamOutcome{err: errStreamingUnsupported}
	}
	wroteHeaders := false
	chatDone := false
	responseTerminal := false
	terminalFailure := false
	hadPayload := false
	for {
		event, err := stream.Next(r.Context())
		if len(event.Payload) > 0 {
			accumulator.Observe(event.Payload)
			if firstPayloadAt.IsZero() {
				firstPayloadAt = time.Now()
			}
			if !wroteHeaders {
				writeSafeResponseHeaders(w, stream.Headers())
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("Connection", "keep-alive")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(http.StatusOK)
				wroteHeaders = true
			}
			if writeErr := writeSSEPayload(w, event.Payload); writeErr != nil {
				return streamOutcome{cancelled: true, wroteHeaders: wroteHeaders}
			}
			hadPayload = true
			if e == endpointChat && sseDonePayload(event.Payload) {
				chatDone = true
			}
			if e == endpointResponses {
				switch responseEventType(event.Payload) {
				case "response.completed":
					responseTerminal = true
				case "response.failed", "response.incomplete", "error":
					responseTerminal = true
					terminalFailure = true
				}
			}
			flusher.Flush()
		}
		if err == nil && (chatDone || responseTerminal) {
			if terminalFailure {
				return streamOutcome{err: &inference.UpstreamError{Class: "stream_failed", HTTPStatus: http.StatusBadGateway}, wroteHeaders: wroteHeaders}
			}
			return streamOutcome{wroteHeaders: wroteHeaders}
		}
		if err == nil && !event.Done {
			continue
		}
		if r.Context().Err() != nil {
			if !wroteHeaders {
				writeProxyError(w, r, errClientCancelled)
			}
			return streamOutcome{cancelled: true, wroteHeaders: wroteHeaders}
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if r.Context().Err() != nil {
				if !wroteHeaders {
					writeProxyError(w, r, errClientCancelled)
				}
				return streamOutcome{cancelled: true, wroteHeaders: wroteHeaders}
			}
			if wroteHeaders {
				writeSSEError(w, r, err)
				if e == endpointChat {
					_ = writeSSEDone(w)
				}
			}
			return streamOutcome{err: err, wroteHeaders: wroteHeaders}
		}
		if err != nil && !errors.Is(err, io.EOF) {
			if !wroteHeaders {
				return streamOutcome{err: err}
			}
			writeSSEError(w, r, err)
			if e == endpointChat && !sseDonePayload(event.Payload) {
				_ = writeSSEDone(w)
			}
			return streamOutcome{err: err, wroteHeaders: wroteHeaders}
		}
		if !wroteHeaders {
			return streamOutcome{err: errEmptyStream}
		}
		if e == endpointChat {
			if !chatDone {
				writeSSEError(w, r, errStreamIncomplete)
				_ = writeSSEDone(w)
				return streamOutcome{err: errStreamIncomplete, wroteHeaders: wroteHeaders}
			}
			return streamOutcome{wroteHeaders: wroteHeaders}
		}
		if responseTerminal {
			if terminalFailure {
				return streamOutcome{err: &inference.UpstreamError{Class: "stream_failed", HTTPStatus: http.StatusBadGateway}, wroteHeaders: wroteHeaders}
			}
			return streamOutcome{wroteHeaders: wroteHeaders}
		}
		if hadPayload {
			writeSSEError(w, r, errStreamIncomplete)
		}
		return streamOutcome{err: errStreamIncomplete, wroteHeaders: wroteHeaders}
	}
}

func (h *Handler) reportResult(ctx context.Context, selection scheduler.Selection, err error) {
	if selection.CredentialID == uuid.Nil {
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, errClientCancelled) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	observedAt := h.now().UTC()
	result := inference.CredentialResult{
		CredentialID: selection.CredentialID.String(),
		Succeeded:    err == nil,
		ObservedAt:   observedAt,
	}
	if err != nil {
		result.ErrorClass = errorClass(err)
		if status := errorStatus(err); status == http.StatusTooManyRequests || status >= 500 {
			cooldown := observedAt.Add(30 * time.Second)
			if status == http.StatusTooManyRequests {
				cooldown = observedAt.Add(time.Minute)
			}
			result.CooldownUntil = &cooldown
		}
	}
	safeCtx := context.WithoutCancel(ctx)
	if h.runtime != nil {
		h.runtime.MarkCredentialResult(safeCtx, result)
	}
	if h.health == nil {
		return
	}
	credentialID := selection.CredentialID
	input := credential.HealthInput{
		Succeeded:     result.Succeeded,
		ErrorClass:    result.ErrorClass,
		CooldownUntil: result.CooldownUntil,
		ObservedAt:    observedAt,
	}
	if reportErr := h.health.RecordHealth(safeCtx, credentialID, input); reportErr != nil {
		h.logger.Error("proxy_health_record_error", "request_id", httpapi.RequestID(ctx), "error_class", errorClass(reportErr))
	}
}
func (h *Handler) methodNotAllowed(w http.ResponseWriter, r *http.Request, method string) {
	w.Header().Set("Allow", method)
	writeProxyError(w, r, errMethodNotAllowed)
}

func requestID(r *http.Request) string {
	if r != nil {
		if value := httpapi.RequestID(r.Context()); value != "" {
			return value
		}
		if value := normalizeRequestID(r.Header.Get("X-Request-ID")); value != "" {
			return value
		}
	}
	return "req_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func normalizeRequestID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return ""
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return ""
	}
	return value
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type mappedError struct {
	status     int
	typeName   string
	code       string
	message    string
	retryAfter string
}

func writeProxyError(w http.ResponseWriter, r *http.Request, err error) {
	mapped := mapPublicError(err)
	if mapped.retryAfter != "" {
		w.Header().Set("Retry-After", mapped.retryAfter)
	}
	w.Header().Set("Cache-Control", "no-store")
	id := requestID(r)
	if w.Header().Get("X-Request-ID") == "" {
		w.Header().Set("X-Request-ID", id)
	}
	if mapped.status >= 500 {
		slog.Default().Error("proxy_request_error", "request_id", id, "error_class", mapped.code, "status", mapped.status)
	}
	writeJSON(w, mapped.status, map[string]any{"error": map[string]string{
		"type":       mapped.typeName,
		"code":       mapped.code,
		"message":    mapped.message,
		"request_id": id,
	}})
}

func mapPublicError(err error) mappedError {
	if err == nil {
		return mappedError{status: http.StatusInternalServerError, typeName: "api_error", code: "internal_error", message: "The request could not be completed."}
	}
	switch {
	case errors.Is(err, errInferenceUnavailable):
		return mappedError{status: http.StatusServiceUnavailable, typeName: "api_error", code: "inference_unavailable", message: "Inference service is not configured."}
	case errors.Is(err, errUsageUnavailable):
		return mappedError{status: http.StatusServiceUnavailable, typeName: "api_error", code: "usage_unavailable", message: "Usage settlement is temporarily unavailable."}
	case errors.Is(err, usage.ErrRequestAlreadyClosed):
		return mappedError{status: http.StatusConflict, typeName: "invalid_request_error", code: "duplicate_request", message: "The request id has already been finalized."}
	case errors.Is(err, pricing.ErrPriceMissing), errors.Is(err, pricing.ErrBillingDisabled):
		return mappedError{status: http.StatusServiceUnavailable, typeName: "api_error", code: "price_unavailable", message: "The model price is not currently available."}
	case errors.Is(err, errStreamingUnsupported):
		return mappedError{status: http.StatusNotImplemented, typeName: "api_error", code: "streaming_unavailable", message: "Streaming is not available."}
	case errors.Is(err, errClientCancelled):
		return mappedError{status: 499, typeName: "api_error", code: "client_cancelled", message: "The client cancelled the request."}
	case errors.Is(err, apikey.ErrInvalidKey):
		return mappedError{status: http.StatusUnauthorized, typeName: "authentication_error", code: "invalid_api_key", message: "The API key is invalid."}
	case errors.Is(err, apikey.ErrIPDenied):
		return mappedError{status: http.StatusForbidden, typeName: "permission_error", code: "ip_not_allowed", message: "The source IP is not allowed."}
	case errors.Is(err, apikey.ErrModelDenied):
		return mappedError{status: http.StatusForbidden, typeName: "permission_error", code: "model_not_allowed", message: "The API key cannot access this model."}
	case errors.Is(err, apikey.ErrRateLimited):
		return mappedError{status: http.StatusTooManyRequests, typeName: "rate_limit_error", code: "rate_limited", message: "The API key rate limit was exceeded.", retryAfter: "60"}
	case errors.Is(err, apikey.ErrRateLimitUnavailable):
		return mappedError{status: http.StatusServiceUnavailable, typeName: "api_error", code: "rate_limit_unavailable", message: "Rate limiting is temporarily unavailable."}
	case errors.Is(err, apikey.ErrInvalidInput):
		return mappedError{status: http.StatusBadRequest, typeName: "invalid_request_error", code: "invalid_request", message: "The request is invalid."}
	case errors.Is(err, catalogmodel.ErrNotFound):
		return mappedError{status: http.StatusNotFound, typeName: "not_found", code: "model_not_found", message: "The requested model does not exist."}
	case errors.Is(err, catalogmodel.ErrInvalidInput):
		return mappedError{status: http.StatusBadRequest, typeName: "invalid_request_error", code: "invalid_model", message: "The requested model is invalid."}
	case errors.Is(err, route.ErrNoRoute):
		return mappedError{status: http.StatusServiceUnavailable, typeName: "upstream_error", code: "route_unavailable", message: "No route is available for this model."}
	case errors.Is(err, route.ErrRouteDisabled):
		return mappedError{status: http.StatusServiceUnavailable, typeName: "upstream_error", code: "route_disabled", message: "The route is currently disabled."}
	case errors.Is(err, route.ErrTargetUnavailable), errors.Is(err, scheduler.ErrNoEligible):
		return mappedError{status: http.StatusServiceUnavailable, typeName: "upstream_error", code: "no_eligible_credential", message: "No eligible upstream credential is available."}
	case errors.Is(err, errRequestTooLarge):
		return mappedError{status: http.StatusRequestEntityTooLarge, typeName: "invalid_request_error", code: "request_too_large", message: "The request body is too large."}
	case errors.Is(err, scheduler.ErrStateUnavailable):
		return mappedError{status: http.StatusServiceUnavailable, typeName: "api_error", code: "scheduler_unavailable", message: "Scheduler coordination is temporarily unavailable."}
	case errors.Is(err, scheduler.ErrInvalidInput):
		return mappedError{status: http.StatusBadRequest, typeName: "invalid_request_error", code: "invalid_request", message: "The request is invalid."}
	case errors.Is(err, scheduler.ErrLeaseBusy):
		return mappedError{status: http.StatusServiceUnavailable, typeName: "upstream_error", code: "no_eligible_credential", message: "No eligible upstream credential is available."}
	case errors.Is(err, errMalformedResponse), errors.Is(err, errEmptyStream), errors.Is(err, errStreamIncomplete):
		return mappedError{status: http.StatusBadGateway, typeName: "upstream_error", code: "upstream_protocol_error", message: "The upstream response was incomplete or invalid."}
	case errors.Is(err, errInvalidRequest):
		return mappedError{status: http.StatusBadRequest, typeName: "invalid_request_error", code: "invalid_request", message: "The request is invalid."}
	case errors.Is(err, errMethodNotAllowed):
		return mappedError{status: http.StatusMethodNotAllowed, typeName: "invalid_request_error", code: "method_not_allowed", message: "Method not allowed."}
	case errors.Is(err, errProxyNotFound):
		return mappedError{status: http.StatusNotFound, typeName: "invalid_request_error", code: "not_found", message: "The requested endpoint does not exist."}
	}
	var upstream *inference.UpstreamError
	if errors.As(err, &upstream) && upstream != nil {
		return mapUpstreamError(upstream)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return mappedError{status: http.StatusGatewayTimeout, typeName: "upstream_error", code: "upstream_timeout", message: "The upstream request timed out."}
	}
	if errors.Is(err, context.Canceled) {
		return mappedError{status: 499, typeName: "api_error", code: "client_cancelled", message: "The client cancelled the request."}
	}
	return mappedError{status: http.StatusInternalServerError, typeName: "api_error", code: "internal_error", message: "The request could not be completed."}
}

func mapUpstreamError(err *inference.UpstreamError) mappedError {
	class := strings.ToLower(strings.TrimSpace(err.Class))
	switch class {
	case "provider_not_configured", "credential_unavailable", "credential_source_unsupported":
		return mappedError{status: http.StatusServiceUnavailable, typeName: "upstream_error", code: class, message: "The upstream credential is unavailable."}
	case "rate_limit":
		return mappedError{status: http.StatusTooManyRequests, typeName: "rate_limit_error", code: "upstream_rate_limit", message: "The upstream rate limit was exceeded.", retryAfter: "60"}
	case "authentication":
		return mappedError{status: http.StatusBadGateway, typeName: "upstream_error", code: "upstream_authentication", message: "The upstream authentication failed."}
	case "permission":
		return mappedError{status: http.StatusBadGateway, typeName: "upstream_error", code: "upstream_permission", message: "The upstream rejected the request."}
	case "timeout":
		return mappedError{status: http.StatusGatewayTimeout, typeName: "upstream_error", code: "upstream_timeout", message: "The upstream request timed out."}
	}
	if err.HTTPStatus == http.StatusTooManyRequests {
		return mappedError{status: http.StatusTooManyRequests, typeName: "rate_limit_error", code: "upstream_rate_limit", message: "The upstream rate limit was exceeded.", retryAfter: "60"}
	}
	return mappedError{status: http.StatusBadGateway, typeName: "upstream_error", code: "upstream_error", message: "The upstream request failed."}
}

func errorStatus(err error) int {
	mapped := mapPublicError(err)
	return mapped.status
}

func errorClass(err error) string {
	if err == nil {
		return ""
	}
	var upstream *inference.UpstreamError
	if errors.As(err, &upstream) && upstream != nil && upstream.Class != "" {
		return upstream.Class
	}
	return mapPublicError(err).code
}

func safeHeaderValue(value string) bool {
	return !strings.ContainsAny(value, "\r\n\x00")
}

var allowedRequestHeaders = map[string]struct{}{
	"accept":            {},
	"anthropic-beta":    {},
	"anthropic-version": {},
	"baggage":           {},
	"content-type":      {},
	"openai-beta":       {},
	"traceparent":       {},
	"tracestate":        {},
	"user-agent":        {},
}

// filteredRequestHeaders forwards only protocol/observability headers that are
// safe for the CPA executor. Authentication, routing, proxy and arbitrary
// client headers must never cross the Bablo-to-upstream trust boundary.
func filteredRequestHeaders(r *http.Request, id string) map[string][]string {
	result := make(map[string][]string)
	if r != nil {
		for key, values := range r.Header {
			lower := strings.ToLower(strings.TrimSpace(key))
			if _, allowed := allowedRequestHeaders[lower]; !allowed {
				continue
			}
			canonical := http.CanonicalHeaderKey(key)
			for _, value := range values {
				if safeHeaderValue(value) {
					result[canonical] = append(result[canonical], value)
				}
			}
		}
	}
	result["X-Request-ID"] = []string{id}
	return result
}

var safeResponseHeaders = map[string]struct{}{
	"content-type":                   {},
	"retry-after":                    {},
	"x-ratelimit-limit-requests":     {},
	"x-ratelimit-remaining-requests": {},
	"x-ratelimit-reset-requests":     {},
	"x-ratelimit-limit-tokens":       {},
	"x-ratelimit-remaining-tokens":   {},
	"x-ratelimit-reset-tokens":       {},
}

func writeSafeResponseHeaders(w http.ResponseWriter, source map[string][]string) {
	for key, values := range source {
		lower := strings.ToLower(strings.TrimSpace(key))
		if _, allowed := safeResponseHeaders[lower]; !allowed {
			continue
		}
		for _, value := range values {
			if safeHeaderValue(value) {
				w.Header().Add(http.CanonicalHeaderKey(key), value)
			}
		}
	}
}

func writeSSEPayload(w http.ResponseWriter, payload []byte) error {
	framed := frameSSEPayload(payload)
	_, err := w.Write(framed)
	return err
}

func frameSSEPayload(payload []byte) []byte {
	payload = bytes.ReplaceAll(payload, []byte("\r\n"), []byte("\n"))
	payload = bytes.ReplaceAll(payload, []byte("\r"), []byte("\n"))
	if isSSEPayload(payload) {
		payload = append([]byte(nil), payload...)
		payload = bytes.TrimRight(payload, "\n")
		return append(payload, '\n', '\n')
	}
	lines := bytes.Split(payload, []byte("\n"))
	var framed bytes.Buffer
	for _, line := range lines {
		framed.WriteString("data: ")
		framed.Write(line)
		framed.WriteByte('\n')
	}
	framed.WriteByte('\n')
	return framed.Bytes()
}

func isSSEPayload(payload []byte) bool {
	for _, line := range bytes.Split(payload, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		return bytes.HasPrefix(line, []byte("data:")) || bytes.HasPrefix(line, []byte("event:")) || bytes.HasPrefix(line, []byte("id:")) || bytes.HasPrefix(line, []byte("retry:")) || bytes.HasPrefix(line, []byte(":"))
	}
	return false
}

func sseDonePayload(payload []byte) bool {
	for _, line := range bytes.Split(bytes.ReplaceAll(payload, []byte("\r"), nil), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		}
		if bytes.Equal(line, []byte("[DONE]")) {
			return true
		}
	}
	return false
}

func writeSSEDone(w http.ResponseWriter) error {
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return err
}

func writeSSEError(w http.ResponseWriter, r *http.Request, err error) {
	mapped := mapPublicError(err)
	payload := map[string]any{"error": map[string]string{
		"type":       mapped.typeName,
		"code":       mapped.code,
		"message":    mapped.message,
		"request_id": requestID(r),
	}}
	encoded, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return
	}
	if _, ok := w.(http.Flusher); ok && strings.Contains(r.URL.Path, "/responses") {
		_, _ = io.WriteString(w, "event: error\n")
		_, _ = io.WriteString(w, "data: ")
		_, _ = w.Write(encoded)
		_, _ = io.WriteString(w, "\n\n")
	} else {
		_ = writeSSEPayload(w, encoded)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func responseEventType(payload []byte) string {
	eventName := ""
	for _, line := range bytes.Split(bytes.ReplaceAll(payload, []byte("\r"), nil), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			eventName = strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:"))))
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		}
		if bytes.Equal(line, []byte("[DONE]")) {
			continue
		}
		var value struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &value) == nil && strings.TrimSpace(value.Type) != "" {
			return strings.TrimSpace(value.Type)
		}
	}
	return eventName
}

type leaseKeeper struct {
	selection *scheduler.Selection
	ttl       time.Duration
	interval  time.Duration
	logger    *slog.Logger
	cancel    context.CancelFunc
	wait      sync.WaitGroup
}

func newLeaseKeeper(selection *scheduler.Selection, ttl time.Duration, logger *slog.Logger) *leaseKeeper {
	if logger == nil {
		logger = slog.Default()
	}
	interval := ttl / 3
	if interval < time.Second {
		interval = time.Second
	}
	return &leaseKeeper{selection: selection, ttl: ttl, interval: interval, logger: logger}
}

func (k *leaseKeeper) Start() {
	if k == nil || k.selection == nil || k.selection.Lease == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	k.cancel = cancel
	k.wait.Add(1)
	go func() {
		defer k.wait.Done()
		ticker := time.NewTicker(k.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := k.selection.Renew(context.Background(), k.ttl); err != nil {
					k.logger.Warn("proxy_lease_renew_error", "error_class", errorClass(err))
				}
			}
		}
	}()
}

func (k *leaseKeeper) Stop() {
	if k == nil {
		return
	}
	if k.cancel != nil {
		k.cancel()
	}
	k.wait.Wait()
}

var _ http.Handler = (*Handler)(nil)
