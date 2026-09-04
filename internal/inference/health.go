package inference

import "time"

// CredentialResult is a provider-neutral runtime health observation. The
// credential service persists the authoritative copy; adapters may use it for
// local cooldown behavior.
type CredentialResult struct {
	CredentialID  string
	Provider      string
	Model         string
	RouteModel    string
	Succeeded     bool
	ErrorClass    string
	HTTPStatus    int
	CooldownUntil *time.Time
	ObservedAt    time.Time
}
