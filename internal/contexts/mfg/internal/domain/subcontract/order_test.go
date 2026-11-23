package subcontract_test

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/mfg/internal/domain/subcontract"
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

func issued() *subcontract.Order {
	return &subcontract.Order{ID: "sub.1", Status: subcontract.StatusIssued, Version: 1, PlanID: "plan.1",
		OperationID: "op.1", Quantity: qty(&testing.T{}, "10"), UnitID: "PCS", OwnerActor: "purchaser.1"}
}

func TestGateAndProgressDoNotInventStock(t *testing.T) {
	err := subcontract.Gate(false)
	rej, ok := kernel.AsRejection(err)
	if !ok || rej.Code != subcontract.CodeDependencyNotSupported {
		t.Fatalf("gate %v", err)
	}
	o := issued()
	if err = o.Advance(subcontract.Progress{ID: "prg.1", Operation: "op.1", Kind: subcontract.StatusInProgress,
		Quantity: qty(t, "4"), UnitID: "PCS", Hash: "h", ActorID: "merch.1"}); err != nil {
		t.Fatal(err)
	}
	if o.Status != subcontract.StatusInProgress || len(o.Claims) != 1 {
		t.Fatalf("claim %+v", o)
	}
	if subcontract.MovesStock() || subcontract.ReleasesQuality() {
		t.Fatal("a progress claim created stock or a quality release")
	}
	// The same claim is a replay and adds nothing.
	if err = o.Advance(subcontract.Progress{ID: "prg.1", Operation: "op.1", Kind: subcontract.StatusInProgress,
		Quantity: qty(t, "4"), UnitID: "PCS", Hash: "h", ActorID: "merch.1"}); err != nil || len(o.Claims) != 1 {
		t.Fatalf("replay %v %+v", err, o)
	}
}

func TestSubcontractStateMachine(t *testing.T) {
	o := issued()
	if err := o.Advance(subcontract.Progress{ID: "prg.1", Operation: "op.1", Kind: subcontract.StatusCompleted,
		Quantity: qty(t, "1"), UnitID: "PCS", ActorID: "merch.1"}); code(err) != kernel.CodeInvalidState {
		t.Fatalf("skip %v", err)
	}
	if err := o.Advance(subcontract.Progress{ID: "prg.1", Operation: "op.1", Kind: subcontract.StatusInProgress,
		Quantity: qty(t, "1"), UnitID: "PCS", ActorID: "merch.1"}); err != nil {
		t.Fatal(err)
	}
	if err := o.Advance(subcontract.Progress{ID: "prg.2", Operation: "op.1", Kind: subcontract.StatusCompleted,
		Quantity: qty(t, "1"), UnitID: "PCS", ActorID: "merch.1"}); err != nil || !o.Terminal() {
		t.Fatalf("complete %v %+v", err, o)
	}
	cancel := issued()
	if err := cancel.Advance(subcontract.Progress{ID: "prg.1", Operation: "op.1", Kind: subcontract.StatusCancelled,
		Quantity: qty(t, "1"), UnitID: "PCS", ActorID: "merch.1"}); err != nil || cancel.Status != subcontract.StatusCancelled {
		t.Fatalf("cancel %v %+v", err, cancel)
	}
}

func TestHandoverKindsStayDistinct(t *testing.T) {
	for _, kind := range []string{subcontract.HandoverTransfer, subcontract.HandoverConsume, subcontract.HandoverReturn, subcontract.HandoverScrap} {
		if subcontract.HandoverKind(kind) != nil {
			t.Fatalf("kind %s", kind)
		}
	}
	// Scrap is not accepted under a transfer spelling (MFG-INV-002).
	err := subcontract.HandoverKind("handover_transfer_scrap")
	if code(err) != kernel.CodeInvalidKnownValue {
		t.Fatalf("aliased scrap %v", err)
	}
	if subcontract.HandoverTransfer == subcontract.HandoverScrap {
		t.Fatal("transfer and scrap are one kind")
	}
}

func code(err error) string {
	rej, ok := kernel.AsRejection(err)
	if !ok {
		return ""
	}
	return rej.Code
}
