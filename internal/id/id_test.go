package id

import (
	"testing"

	"github.com/google/uuid"
)

func TestNewReturnsUUIDv7(t *testing.T) {
	value, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if value.Version() != 7 {
		t.Fatalf("New() version = %d, want 7", value.Version())
	}
	if value.Variant() != uuid.RFC4122 {
		t.Fatalf("New() variant = %v, want RFC4122", value.Variant())
	}
}
