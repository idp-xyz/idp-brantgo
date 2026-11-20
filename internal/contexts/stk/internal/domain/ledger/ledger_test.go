package ledger

import (
	"errors"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func dec(s string) kernel.Decimal { return kernel.MustDecimal(s) }

func layer(id string, cost int64, basis, remaining, at string) Layer {
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		panic(err)
	}
	return Layer{ID: id, Currency: "USD", MinorUnits: 2, UnitCostMinor: cost, Basis: dec(basis),
		Remaining: dec(remaining), ReceivedAt: t}
}

// STK-INV-003: one HALF_UP point. 10 minor × 2 / 3 = 6.6… rounds to 7, and the minor-unit scale
// does not change the integer (USD's 2 digits and a 0-digit currency agree).
func TestAmountRoundsHalfUpOnce(t *testing.T) {
	base := Layer{ID: "L", Currency: "USD", MinorUnits: 2, UnitCostMinor: 10, Basis: dec("3")}
	got, err := Amount(base, dec("2"))
	if err != nil || got != 7 {
		t.Fatalf("amount = %d, %v", got, err)
	}
	base.MinorUnits = 0
	base.Currency = "JPY"
	got, err = Amount(base, dec("2"))
	if err != nil || got != 7 {
		t.Fatalf("zero-scale amount = %d, %v", got, err)
	}
	// An exact half rounds away from zero: 1 × 1 / 2 = 0.5 → 1.
	half := Layer{ID: "H", Currency: "USD", MinorUnits: 2, UnitCostMinor: 1, Basis: dec("2")}
	got, err = Amount(half, dec("1"))
	if err != nil || got != 1 {
		t.Fatalf("half = %d, %v", got, err)
	}
}

// Two layers, the older one first even when the slice is not sorted. 40 at 1000 then 10 at 2500.
func TestAllocateFIFOSplitsAcrossLayers(t *testing.T) {
	layers := []Layer{
		layer("newer", 2500, "1", "60", "2026-02-01T00:00:00.000000Z"),
		layer("older", 1000, "1", "40", "2026-01-01T00:00:00.000000Z"),
	}
	takes, err := AllocateFIFO(layers, dec("50"))
	if err != nil {
		t.Fatal(err)
	}
	if len(takes) != 2 || takes[0].LayerID != "older" || takes[0].Quantity.Cmp(dec("40")) != 0 || takes[0].AmountMinor != 40000 ||
		takes[1].LayerID != "newer" || takes[1].Quantity.Cmp(dec("10")) != 0 || takes[1].AmountMinor != 25000 ||
		TotalMinor(takes) != 65000 {
		t.Fatalf("%+v total %d", takes, TotalMinor(takes))
	}
	if layers[1].Remaining.Cmp(dec("40")) != 0 {
		t.Fatal("allocate changed the caller's layer")
	}
	if _, err = AllocateFIFO(layers, dec("101")); !errors.Is(err, ErrLayers) {
		t.Fatalf("short layers: %v", err)
	}
}

func TestAvailableDoesNotCountReservedTwice(t *testing.T) {
	// On hand 100, reserved 60: an unreserved 50 is short, a reserved 60 is covered.
	if Available(dec("100"), dec("60"), dec("50")) {
		t.Fatal("unreserved 50 of 40 available was accepted")
	}
	if !Available(dec("100"), dec("60"), dec("40")) {
		t.Fatal("the unreserved 40 was refused")
	}
	on, reserved, remaining, err := ReservedIssue(dec("100"), dec("60"), dec("60"), dec("60"))
	if err != nil || on.Cmp(dec("40")) != 0 || reserved.Sign() != 0 || remaining.Sign() != 0 {
		t.Fatalf("reserved issue %s %s %s %v", on, reserved, remaining, err)
	}
	if _, err = UnreservedIssue(dec("100"), dec("60"), dec("50")); !errors.Is(err, ErrShort) {
		t.Fatalf("unreserved 50: %v", err)
	}
	if _, err = Place(dec("100"), dec("60"), dec("50")); !errors.Is(err, ErrShort) {
		t.Fatalf("place 50: %v", err)
	}
	got, err := Place(dec("100"), dec("0"), dec("60"))
	if err != nil || got.Cmp(dec("60")) != 0 {
		t.Fatalf("place %s %v", got, err)
	}
}

func TestReceiptSourceRefusesDirectShip(t *testing.T) {
	if err := ReceiptSource(SourceSupplierDelivery, false, false); !errors.Is(err, ErrDirect) {
		t.Fatalf("delivery: %v", err)
	}
	if err := ReceiptSource(SourceDeliveryLine, false, false); !errors.Is(err, ErrDirect) {
		t.Fatalf("delivery line: %v", err)
	}
	if err := ReceiptSource(SourceCommitment, true, false); !errors.Is(err, ErrDirect) {
		t.Fatalf("commitment of a direct ship: %v", err)
	}
	if err := ReceiptSource(SourceCommitment, false, false); err != nil {
		t.Fatal(err)
	}
	if err := ReceiptSource(SourceHoldForReuse, true, false); !errors.Is(err, ErrHold) {
		t.Fatalf("hold without approval: %v", err)
	}
	if err := ReceiptSource(SourceHoldForReuse, true, true); err != nil {
		t.Fatalf("approved hold of direct-ship goods is a receipt: %v", err)
	}
	if err := IssueSource(SourceShipmentLine); !errors.Is(err, ErrShipment) {
		t.Fatalf("issue of a shipment: %v", err)
	}
	if err := IssueReservation("order.1"); !errors.Is(err, ErrOrderHold) {
		t.Fatalf("order reservation: %v", err)
	}
	if err := IssueReservation(""); err != nil {
		t.Fatal(err)
	}
}

func TestHandoverKindsStayDistinct(t *testing.T) {
	out, in, err := HandoverMoves(KindHandoverScrap)
	if err != nil || !out || in {
		t.Fatalf("scrap %v %v %v", out, in, err)
	}
	out, in, err = HandoverMoves(KindHandoverTransfer)
	if err != nil || !out || !in {
		t.Fatalf("transfer %v %v %v", out, in, err)
	}
	if _, _, err = HandoverMoves(KindIssue); !errors.Is(err, ErrSource) {
		t.Fatalf("issue is not a handover: %v", err)
	}
}

func TestReceiveKeepsTheUnit(t *testing.T) {
	b := Balance{OnHand: dec("1"), UnitID: "PCS", UnitRevision: 1, Currency: "USD", MinorUnits: 2, RowVersion: 1}
	if err := Receive(&b, dec("1"), "KG", 1, "USD", 2); !errors.Is(err, ErrUnit) {
		t.Fatal(err)
	}
	if err := Receive(&b, dec("1"), "PCS", 1, "EUR", 2); !errors.Is(err, ErrCurrency) {
		t.Fatal(err)
	}
}
