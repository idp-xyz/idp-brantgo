package shipment

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// disposedInputs is inputs() with delivery.2 line L1 of the same commitment line (200 verified) and a
// second release: commitment.1 line L1 has 1000 verified over both deliveries and 300 disposed of by
// approved dispositions, so 700 may ship of it (S5R-D05).
func disposedInputs() Inputs {
	in := inputs()
	line := CommitmentLineTotals{Verified: dec("1000"), Disposed: dec("300")}
	d := in.Deliveries[ref1]
	d.Line = line
	in.Deliveries[ref1] = d
	in.Deliveries[ref2] = DeliveryFact{Ref: ref2, Version: 1, Status: "verified", Scope: scopeOf("commitment.1"), Verified: dec("200"), Line: line}
	in.Releases["rel.2"] = ReleaseFact{ID: "rel.2", State: "granted", SubjectKind: SubjectCommitmentLine, Scope: scopeOf("commitment.1"),
		Quantity: dec("600")}
	return in
}

// FUL-INV-003 with S5R-D05 at shipment.prepare: what ships of a commitment line over all its deliveries,
// with what FUL's confirmed shipments shipped of it, is at most verified less disposed, though every
// delivery line alone has room.
func TestS5RD05DisposedGoodsNeverShipAtPrepare(t *testing.T) {
	a := RequestLine{ID: "S1", OrderLineID: "L1", Quantity: dec("500"), Delivery: ref1, Version: 1, ReleaseID: "rel.1"}
	b := RequestLine{ID: "S2", OrderLineID: "L1", Quantity: dec("201"), Delivery: ref2, Version: 1, ReleaseID: "rel.2"}
	if _, err := Prepare(request(a, b), disposedInputs()); outcome(err) != "SUPPLIER_DELIVERY_INSUFFICIENT /args/lines/1/quantity" {
		t.Fatal(outcome(err))
	}
	b.Quantity = dec("200")
	s, err := Prepare(request(a, b), disposedInputs())
	if err != nil {
		t.Fatal(err)
	}
	if u := s.CommitmentLines(); len(u) != 1 || u[0].Ref != (CommitmentLine{CommitmentID: "commitment.1", OrderLineID: "L1"}) ||
		u[0].Quantity.String() != "700" || u[0].Line != 0 {
		t.Fatalf("commitment lines %+v", u)
	}
	in := disposedInputs()
	in.Consumed.CommitmentLines = map[CommitmentLine]kernel.Decimal{{CommitmentID: "commitment.1", OrderLineID: "L1"}: dec("100")}
	b.Quantity = dec("101")
	if _, err := Prepare(request(a, b), in); outcome(err) != "SUPPLIER_DELIVERY_INSUFFICIENT /args/lines/1/quantity" {
		t.Fatalf("shipped before counts: %s", outcome(err))
	}
	// Nothing disposed: the commitment line bounds nothing the delivery lines do not.
	in = disposedInputs()
	for _, r := range []DeliveryRef{ref1, ref2} {
		d := in.Deliveries[r]
		d.Line.Disposed = kernel.Decimal{}
		in.Deliveries[r] = d
	}
	b.Quantity = dec("200")
	a.Quantity = dec("600")
	if _, err := Prepare(request(a, b), in); err != nil {
		t.Fatal(err)
	}
	// A line whose totals PUR did not state ships nothing (fail closed).
	in = inputs()
	d := in.Deliveries[ref1]
	d.Line = CommitmentLineTotals{}
	in.Deliveries[ref1] = d
	if _, err := Prepare(request(), in); outcome(err) != "SUPPLIER_DELIVERY_INSUFFICIENT /args/lines/0/quantity" {
		t.Fatal(outcome(err))
	}
}

// FUL-INV-003 with S5R-D05 at confirmation: SupplierDeliveryGuard v2's totals of the commitment line
// are judged with what the order's other confirmed shipments shipped of it, and recorded in the guard
// evidence; a confirmed shipment re-judged leaves out its own lines.
func TestS5RD05TheConfirmationCountsTheDispositions(t *testing.T) {
	a := RequestLine{ID: "S1", OrderLineID: "L1", Quantity: dec("500"), Delivery: ref1, Version: 1, ReleaseID: "rel.1"}
	b := RequestLine{ID: "S2", OrderLineID: "L1", Quantity: dec("200"), Delivery: ref2, Version: 1, ReleaseID: "rel.2"}
	s, err := Prepare(request(a, b), disposedInputs())
	if err != nil {
		t.Fatal(err)
	}
	u := s.CommitmentLines()[0]
	t700 := CommitmentLineTotals{Verified: dec("1000"), Disposed: dec("300")}
	e, err := s.CheckCommitmentLine(u, t700, kernel.Decimal{})
	if err != nil || e.Name != "supplier_commitment_line:commitment.1/L1" || e.Value != "verified=1000/disposed=300" {
		t.Fatalf("%+v %v", e, err)
	}
	// A disposition approved since the preparation: 400 disposed leaves 600.
	if _, err = s.CheckCommitmentLine(u, CommitmentLineTotals{Verified: dec("1000"), Disposed: dec("400")}, kernel.Decimal{}); outcome(err) !=
		"SUPPLIER_DELIVERY_INSUFFICIENT /shipment/lines/0/quantity" {
		t.Fatal(outcome(err))
	}
	rej, _ := kernel.AsRejection(err)
	if want := "Purchase commitment commitment.1 line L1 has 1000 verified and 400 disposed of by approved dispositions (S5R-D05); 0 shipped " +
		"before and 700 in this shipment exceed the 600 that may ship"; rej.Fields[0].Reason != want {
		t.Fatalf("reason %q", rej.Fields[0].Reason)
	}
	if _, err = s.CheckCommitmentLine(u, t700, dec("0.000001")); outcome(err) != "SUPPLIER_DELIVERY_INSUFFICIENT /shipment/lines/0/quantity" {
		t.Fatal(outcome(err))
	}
	s.Status = StatusConfirmed
	key := CommitmentLine{CommitmentID: "commitment.1", OrderLineID: "L1"}
	before := s.Before(Consumed{CommitmentLines: map[CommitmentLine]kernel.Decimal{key: dec("700")}})
	if got := before.CommitmentLines[key]; got.Sign() != 0 || before.OrderLines == nil || before.Deliveries == nil || before.Releases == nil {
		t.Fatalf("before %+v", before)
	}
}
