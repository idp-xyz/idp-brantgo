package api

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func scenarioData() ScenarioData {
	return ScenarioData{ContextRef: &ContextRef{ContextID: "context.1", Revision: 1},
		Allocations: []ScenarioAllocation{{RfqLineID: "L1", PurchaseQuantity: "120", PurchaseUnitRef: ConfigRef{Kind: KindUnit, ID: "PCS", Revision: 1},
			Source: SupplyLineRef{Kind: SupplySupplierOffer, ID: "offer.1", Revision: 1, ConditionLineID: "OL1"}}},
		LogisticsOfferRefs: []SupplyRef{{Kind: SupplyLogisticsOffer, ID: "log.1", Revision: 1}},
		FXRefs:             []ConfigRef{{Kind: KindFXRate, ID: "fx.1", Revision: 1}}, SelectionReason: "verified"}
}

// The closed contract of cost_scenario.save_draft and cost_scenario.evaluate (Python
// commands.schema.json): a draft may be empty, what it states is well formed.
func TestScenarioCommandsValidate(t *testing.T) {
	if err := (SaveScenario{IdempotencyKey: "k", ScenarioID: "s", Data: ScenarioData{}}).Validate(); err != nil {
		t.Fatalf("empty draft: %v", err)
	}
	if err := (SaveScenario{IdempotencyKey: "k", ScenarioID: "s", Data: scenarioData()}).Validate(); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for _, c := range []struct {
		path string
		edit func(d *ScenarioData)
	}{
		{"/args/data/context_ref/context_id", func(d *ScenarioData) { d.ContextRef.ContextID = "" }},
		{"/args/data/allocations", func(d *ScenarioData) { d.Allocations = []ScenarioAllocation{} }},
		{"/args/data/allocations/0/purchase_quantity", func(d *ScenarioData) { d.Allocations[0].PurchaseQuantity = "1e3" }},
		{"/args/data/allocations/0/purchase_quantity", func(d *ScenarioData) { d.Allocations[0].PurchaseQuantity = "-1" }},
		{"/args/data/allocations/0/purchase_unit_ref/kind", func(d *ScenarioData) { d.Allocations[0].PurchaseUnitRef.Kind = "currency" }},
		{"/args/data/allocations/0/source/kind", func(d *ScenarioData) { d.Allocations[0].Source.Kind = "offer" }},
		{"/args/data/allocations/0/source/condition_line_id", func(d *ScenarioData) { d.Allocations[0].Source.ConditionLineID = "a b" }},
		{"/args/data/logistics_offer_refs/0/kind", func(d *ScenarioData) { d.LogisticsOfferRefs[0].Kind = SupplySupplierOffer }},
		{"/args/data/fx_refs/0/kind", func(d *ScenarioData) { d.FXRefs[0].Kind = "currency" }},
		{"/args/data/fx_refs/0/revision", func(d *ScenarioData) { d.FXRefs[0].Revision = 0 }},
	} {
		d := scenarioData()
		c.edit(&d)
		err := SaveScenario{IdempotencyKey: "k", ScenarioID: "s", Data: d}.Validate()
		if r, ok := kernel.AsRejection(err); !ok || r.Code != kernel.CodeSchemaInvalid || r.Fields[0].Path != c.path {
			t.Errorf("%s: got %v", c.path, err)
		}
	}
	if err := (EvaluateScenario{IdempotencyKey: "k", ScenarioID: "s", ExpectedVersion: 0}).Validate(); err == nil {
		t.Fatal("an evaluation names an existing version")
	}
}

// A freeze takes only a verified, comparable evaluation of the scenario's current version.
func TestBoundEvaluationFreezable(t *testing.T) {
	b := BoundEvaluation{VerificationStatus: VerificationVerified, ComparisonStatus: ComparisonComparable, ScenarioVersion: 2, ScenarioRootVersion: 2}
	if err := b.Freezable("/args"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		code, path string
		edit       func(*BoundEvaluation)
	}{
		{CodeCostInputNotReady, "/args/verification_status", func(b *BoundEvaluation) { b.VerificationStatus = VerificationProvisional }},
		{CodeComparisonBasisMismatch, "/args/comparison_status", func(b *BoundEvaluation) { b.ComparisonStatus = ComparisonNotComparable }},
		{CodeReferenceStale, "/args/scenario_version", func(b *BoundEvaluation) { b.ScenarioRootVersion = 3 }},
	} {
		x := b
		c.edit(&x)
		if r, ok := kernel.AsRejection(x.Freezable("/args")); !ok || r.Code != c.code || r.Fields[0].Path != c.path {
			t.Errorf("%s: %v", c.code, x.Freezable("/args"))
		}
	}
}
