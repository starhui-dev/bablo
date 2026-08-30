package provider

import (
	"errors"
	"testing"

	"github.com/starhui-dev/bablo/internal/model"
)

func TestValidateCreateRejectsCommercialSubscription(t *testing.T) {
	_, err := validateCreate(CreateInput{
		Slug:              "consumer-subscription",
		DisplayName:       "Consumer",
		ResourceType:      ResourceSubscription,
		CommercialAllowed: true,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("subscription validation error = %v, want ErrInvalidInput", err)
	}
}

func TestCapabilitiesMatchProtocol(t *testing.T) {
	if !capabilitiesMatchProtocol(ProtocolOpenAIChat, model.Capabilities{Chat: true}) {
		t.Fatal("chat protocol rejected chat capability")
	}
	if capabilitiesMatchProtocol(ProtocolOpenAIChat, model.Capabilities{Responses: true}) {
		t.Fatal("chat protocol accepted responses-only capability")
	}
	if !capabilitiesMatchProtocol(ProtocolCustom, model.Capabilities{Messages: true}) {
		t.Fatal("custom protocol rejected a valid capability declaration")
	}
}
