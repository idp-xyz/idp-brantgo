package plan_test

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/mfg/internal/domain/plan"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func qty(t *testing.T, s string) kernel.Decimal {
	t.Helper()
	d, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func sample() *plan.Plan {
	return &plan.Plan{
		ID: "plan.1", Status: plan.StatusDraft, FactoryID: "factory.1",
		Operations: []plan.Operation{{ID: "op.1", Name: "sew", Sequence: 1, SkuID: "sku.1", SpecRevision: 1,
			Quantity: qty(&testing.T{}, "10"), UnitID: "PCS", UnitRevision: 1}},
		Materials: []plan.Material{{ID: "mat.1", Kind: plan.MaterialOurSupply, SkuID: "sku.1",
			Quantity: qty(&testing.T{}, "10"), UnitID: "PCS", UnitRevision: 1, LossBasis: "cut"}},
	}
}

func TestGateRefusesADisabledCompany(t *testing.T) {
	err := plan.Gate(false)
	rej, ok := kernel.AsRejection(err)
	if !ok || rej.Code != plan.CodeDependencyNotSupported {
		t.Fatalf("gate %v", err)
	}
	if plan.Gate(true) != nil {
		t.Fatal("enabled company is gated")
	}
}

func TestPlanStateMachine(t *testing.T) {
	p := sample()
	if err := p.Transition(plan.StatusPublished); err != nil {
		t.Fatal(err)
	}
	if p.Status != plan.StatusPublished || p.Version != 1 {
		t.Fatalf("publish %+v", p)
	}
	steps := []string{plan.StatusInProgress, plan.StatusPaused, plan.StatusInProgress, plan.StatusCompleted}
	for _, to := range steps {
		if err := p.Transition(to); err != nil {
			t.Fatalf("%s: %v", to, err)
		}
	}
	if p.Status != plan.StatusCompleted || !p.Terminal() {
		t.Fatalf("completed %+v", p)
	}
	if err := p.Transition(plan.StatusCancelled); code(err) != kernel.CodeInvalidState {
		t.Fatalf("terminal %v", err)
	}

	cancelled := sample()
	if err := cancelled.Transition(plan.StatusCancelled); err != nil || cancelled.Status != plan.StatusCancelled {
		t.Fatalf("cancel draft %v %+v", err, cancelled)
	}
	// published does not jump to completed (SM-MFG-PLAN).
	jump := sample()
	if err := jump.Transition(plan.StatusPublished); err != nil {
		t.Fatal(err)
	}
	if err := jump.Transition(plan.StatusCompleted); code(err) != kernel.CodeInvalidState {
		t.Fatalf("jump %v", err)
	}
}

func TestPublishNeedsMaterialAndAnOperation(t *testing.T) {
	p := &plan.Plan{ID: "plan.1", FactoryID: "factory.1"}
	if err := p.Transition(plan.StatusPublished); code(err) != kernel.CodeDraftNotReady {
		t.Fatalf("empty publish %v", err)
	}
	if err := p.HoldDraft(); err != nil || p.Status != plan.StatusDraft || p.Version != 1 {
		t.Fatalf("draft %v %+v", err, p)
	}
}

func code(err error) string {
	rej, ok := kernel.AsRejection(err)
	if !ok {
		return ""
	}
	return rej.Code
}
