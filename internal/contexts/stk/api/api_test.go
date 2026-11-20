package api

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func TestDirectShipIsNotAWarehouseReceipt(t *testing.T) {
	// STK-INV-002: a person cannot post a supplier delivery, or a shipment line, as stock.
	cmd := ReceiveStock{MovementID: "mv.1", SkuID: "sku.1", LocationID: "loc.1", BatchID: "batch.1",
		CustodyKind: CustodyOwnWarehouse, Quantity: "10", UnitID: "PCS", UnitRevision: 1, Currency: "USD", MinorUnits: 2,
		UnitCostMinor: "1000", PriceBasisQty: "1", SourceKind: SourceDelivery, SourceID: "del.1", SourceLineID: "L1"}
	err := cmd.Validate()
	rej, ok := kernel.AsRejection(err)
	if !ok || rej.Code != kernel.CodeInvalidKnownValue || rej.Fields[0].Path != "/args/source_kind" {
		t.Fatalf("%v", err)
	}
	issue := IssueStock{MovementID: "mv.2", SkuID: "sku.1", LocationID: "loc.1", BatchID: "batch.1",
		CustodyKind: CustodyOwnWarehouse, Quantity: "1", UnitID: "PCS", UnitRevision: 1, SourceKind: SourceShipmentLine,
		SourceID: "ship.1", SourceLineID: "L1"}
	err = issue.Validate()
	rej, ok = kernel.AsRejection(err)
	if !ok || rej.Code != kernel.CodeInvalidKnownValue {
		t.Fatalf("issue %v", err)
	}
}

func TestAdjustNeedsAReasonAndADirection(t *testing.T) {
	cmd := AdjustStock{MovementID: "mv.1", Direction: AdjustOut, SkuID: "sku.1", LocationID: "loc.1", BatchID: "batch.1",
		CustodyKind: CustodyOwnWarehouse, Quantity: "1", UnitID: "PCS", UnitRevision: 1}
	if err := cmd.Validate(); err == nil {
		t.Fatal("missing reason")
	}
	cmd.Reason = "count"
	if err := cmd.Validate(); err != nil {
		t.Fatal(err)
	}
	cmd.Direction = "scrap"
	if rej, _ := kernel.AsRejection(cmd.Validate()); rej == nil || rej.Code != kernel.CodeInvalidKnownValue {
		t.Fatal(cmd.Validate())
	}
}

func TestHandoverKindsDoNotMix(t *testing.T) {
	base := RecordHandover{MovementID: "mv.1", Kind: HandoverScrap, SkuID: "sku.1", LocationID: "loc.1", BatchID: "b.1",
		CustodyKind: CustodyExternalFactoryMaterial, Quantity: "1", UnitID: "PCS", UnitRevision: 1, Reason: "broken"}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	base.Kind = HandoverTransfer
	if err := base.Validate(); err == nil {
		t.Fatal("transfer without a destination")
	}
	base.CounterMovementID = "mv.1.in"
	base.ToLocationID = "loc.2"
	base.ToBatchID = "b.1"
	base.ToCustodyKind = CustodyOwnWarehouse
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestHalfUpCostIsAnIntegerMinorAmount(t *testing.T) {
	// The receive contract accepts the minor string the FIFO rule rounds from, and refuses a fraction.
	ok := ReceiveStock{MovementID: "mv.1", SkuID: "sku.1", LocationID: "loc.1", BatchID: "b.1",
		CustodyKind: CustodyOwnWarehouse, Quantity: "2", UnitID: "PCS", UnitRevision: 1, Currency: "USD", MinorUnits: 2,
		UnitCostMinor: "10", PriceBasisQty: "3"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	ok.UnitCostMinor = "10.5"
	if err := ok.Validate(); err == nil {
		t.Fatal("a fractional minor unit was accepted")
	}
}
