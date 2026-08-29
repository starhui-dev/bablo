package inference

import "fmt"

// UpstreamError is a safe, provider-neutral error classification.
// It intentionally omits upstream response bodies and credentials.
type UpstreamError struct {
	Class      string
	HTTPStatus int
	Retryable  bool
}

func (e *UpstreamError) Error() string {
	if e == nil {
		return "upstream error"
	}
	if e.HTTPStatus > 0 {
		return fmt.Sprintf("upstream %s (status %d)", e.Class, e.HTTPStatus)
	}
	return "upstream " + e.Class
}

// StatusCode exposes an HTTP-like status for the HTTP error mapper.
func (e *UpstreamError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.HTTPStatus
}
