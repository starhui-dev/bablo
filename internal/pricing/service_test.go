package pricing

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestValidateCreateKeepsExactDecimalAndRejectsDuplicateDimensions(t *testing.T) {
	modelID := uuid.New()
	validated, err := validateCreate(CreateInput{
		Scope:         ScopeModel,
		Currency:      " usd ",
		EffectiveFrom: time.Now(),
		Prices: []EntryInput{
			{ModelID: &modelID, Dimension: DimensionInputToken, UnitPrice: "0.000002000000"},
			{ModelID: &modelID, Dimension: DimensionOutputToken, UnitPrice: "0.000008"},
		},
	})
	if err != nil {
		t.Fatalf("validateCreate() error = %v", err)
	}
	if validated.Currency != "USD" || validated.Prices[0].UnitPrice != "0.000002" {
		t.Fatalf("validated price = %+v", validated)
	}

	_, err = validateCreate(CreateInput{
		Scope:         ScopeModel,
		Currency:      "USD",
		EffectiveFrom: time.Now(),
		Prices: []EntryInput{
			{ModelID: &modelID, Dimension: DimensionRequest, UnitPrice: "1"},
			{ModelID: &modelID, Dimension: DimensionRequest, UnitPrice: "2"},
		},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate dimension error = %v, want ErrInvalidInput", err)
	}
}

func TestPublishableRequiresCompleteBillingShapePerTarget(t *testing.T) {
	modelID := uuid.New()
	if publishable(Version{Prices: []Entry{{ModelID: &modelID, Dimension: DimensionInputToken}}}) {
		t.Fatal("input-only token price was publishable")
	}
	if !publishable(Version{Prices: []Entry{
		{ModelID: &modelID, Dimension: DimensionInputToken},
		{ModelID: &modelID, Dimension: DimensionOutputToken},
	}}) {
		t.Fatal("complete token price was not publishable")
	}
	if !publishable(Version{Prices: []Entry{{ModelID: &modelID, Dimension: DimensionRequest}}}) {
		t.Fatal("request price was not publishable")
	}
}
