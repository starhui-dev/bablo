package route

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeCreateUsesExactMatchAndStableTargetOrder(t *testing.T) {
	modelID := uuid.New()
	providerModelID := uuid.New()
	poolID := uuid.New()
	validated, err := normalizeCreate(CreateInput{
		ModelID:    modelID,
		MatchValue: "  MODEL-LATEST ",
		Targets: []TargetInput{{
			ProviderModelID:  providerModelID,
			CredentialPoolID: poolID,
			Weight:           2,
			Enabled:          true,
		}},
	})
	if err != nil {
		t.Fatalf("normalizeCreate() error = %v", err)
	}
	if validated.MatchType != MatchExact || validated.MatchValue != "model-latest" {
		t.Fatalf("normalized match = %q/%q", validated.MatchType, validated.MatchValue)
	}
	if validated.Targets[0].CommercialPolicy != CommercialInherit || validated.Targets[0].Weight != 2 {
		t.Fatalf("normalized target = %+v", validated.Targets[0])
	}
}

func TestNormalizeTargetsRejectsDuplicateAndAllDisabledTargets(t *testing.T) {
	providerModelID := uuid.New()
	poolID := uuid.New()
	input := []TargetInput{{ProviderModelID: providerModelID, CredentialPoolID: poolID, Enabled: true}, {ProviderModelID: providerModelID, CredentialPoolID: poolID, Enabled: true}}
	if _, err := normalizeTargets(input); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate target error = %v, want ErrConflict", err)
	}
	if _, err := normalizeTargets([]TargetInput{{ProviderModelID: uuid.New(), CredentialPoolID: uuid.New(), Enabled: false}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("all disabled target error = %v, want ErrInvalidInput", err)
	}
}

func TestNormalizeCreateRejectsNonExactMatch(t *testing.T) {
	_, err := normalizeCreate(CreateInput{
		ModelID:    uuid.New(),
		MatchType:  "regex",
		MatchValue: "model.*",
		Targets: []TargetInput{{
			ProviderModelID:  uuid.New(),
			CredentialPoolID: uuid.New(),
			Enabled:          true,
		}},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("non-exact match error = %v, want ErrInvalidInput", err)
	}
}
