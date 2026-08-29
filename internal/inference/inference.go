// Package inference defines the stable Bablo-side inference contract.
package inference

import "context"

// Engine executes requests without exposing an upstream SDK type.
type Engine interface {
	Execute(context.Context, Request) (ExecutionResult, error)
	ExecuteStream(context.Context, Request) (Stream, error)
	Capabilities(context.Context) (Capabilities, error)
	Shutdown(context.Context) error
}

// Request is the normalized request sent to an inference engine.
type Request struct {
	RequestID      string
	ResolvedRoute  ResolvedRoute
	SourceFormat   string
	ResponseFormat string
	Headers        map[string]string
	Body           []byte
	Stream         bool
}

// ResolvedRoute identifies the route snapshot selected for one request.
type ResolvedRoute struct {
	RouteID        string
	RouteVersionID string
	ProviderID     string
	CredentialID   string
	RequestedModel string
	ResolvedModel  string
}

// ExecutionResult is the non-streaming engine result.
type ExecutionResult struct {
	StatusCode int
	Headers    map[string]string
	Body       []byte
}

// Stream is the Bablo-side streaming contract.
type Stream interface {
	Next(context.Context) (StreamEvent, error)
	Close() error
}

// StreamEvent is a protocol-neutral event from the engine.
type StreamEvent struct {
	Payload []byte
	Done    bool
}

// Capabilities describes capabilities discovered by the adapter.
type Capabilities struct {
	Formats   []string
	Streaming bool
	Tools     bool
	Reasoning bool
	Vision    bool
	ModelIDs  []string
}
