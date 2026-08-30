package model

import (
	"errors"
	"testing"
)

func TestValidateCreateNormalizesIdentifiersAndRejectsDisabledEnabledModel(t *testing.T) {
	validated, err := validateCreate(CreateInput{
		PublicID:     " Bablo/Chat ",
		Aliases:      []string{"BABLO-LATEST", "bablo-latest"},
		DisplayName:  " Bablo Chat ",
		Visibility:   VisibilityPublic,
		BillingClass: BillingToken,
		Capabilities: Capabilities{Chat: true, Stream: true},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("validateCreate() error = %v", err)
	}
	if validated.PublicID != "bablo/chat" || validated.DisplayName != "Bablo Chat" {
		t.Fatalf("validated model = %+v", validated)
	}
	if len(validated.Aliases) != 1 || validated.Aliases[0] != "bablo-latest" {
		t.Fatalf("aliases = %v", validated.Aliases)
	}

	_, err = validateCreate(CreateInput{
		PublicID:     "disabled-model",
		DisplayName:  "Disabled",
		Visibility:   VisibilityPublic,
		BillingClass: BillingDisabled,
		Capabilities: Capabilities{Chat: true},
		Enabled:      true,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("disabled enabled model error = %v, want ErrInvalidInput", err)
	}
}

func TestCanonicalCapabilitiesContainProviderSubset(t *testing.T) {
	canonical := Capabilities{Chat: true, Responses: true, Stream: true, Tools: true}
	if !canonical.Supports(Capabilities{Chat: true, Stream: true}) {
		t.Fatal("canonical capabilities rejected a valid provider subset")
	}
	if canonical.Supports(Capabilities{Chat: true, Reasoning: true}) {
		t.Fatal("canonical capabilities accepted unsupported reasoning")
	}
}
