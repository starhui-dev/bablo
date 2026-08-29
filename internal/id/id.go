// Package id generates Bablo's time-ordered application identifiers.
package id

import "github.com/google/uuid"

// New returns a UUIDv7 suitable for Bablo PostgreSQL primary keys.
func New() (uuid.UUID, error) {
	return uuid.NewV7()
}
