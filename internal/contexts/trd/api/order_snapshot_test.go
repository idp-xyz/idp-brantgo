package api

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// OrderCommercialSnapshot v1 (S5 plan unit A2; R27, R32, R46): a fact whose sealed revision is
// intact checks clear whatever the order's state (the caller decides on Status); one that is not
// is REFERENCE_INVALID at the caller's path. The terms answer the addresses of a role in their
// bound order, and the Terms block of ActiveOrderRef v1 is the same structure. The trigger and
// release-control names are POL's and D-5's.
func TestOrderSnapshotFact(t *testing.T) {
	cancelled := OrderSnapshotFact{Status: OrderCancelled, Ended: "order.cancel"}
	if err := cancelled.Check("/x"); err != nil {
		t.Fatalf("an intact, cancelled order: %v", err)
	}
	if err := (OrderSnapshotFact{Code: kernel.CodeReferenceInvalid}).Check("/args/order_ref"); pathOf(err) != "REFERENCE_INVALID /args/order_ref" {
		t.Fatalf("a broken snapshot: %s", pathOf(err))
	}
	terms := OrderTerms{Addresses: []OrderAddress{{Role: "buyer", AddressID: "hq"}, {Role: "consignee", AddressID: "dock"},
		{Role: "consignee", AddressID: "yard"}}}
	f := OrderSnapshotFact{OrderTerms: terms}
	if got := f.Address("consignee"); len(got) != 2 || got[0].AddressID != "dock" || got[1].AddressID != "yard" {
		t.Fatalf("consignees %+v", got)
	}
	if got := (ActiveOrderFact{Terms: terms}).Terms.Address("payer"); got != nil {
		t.Fatalf("no payer: %+v", got)
	}
	if TriggerOrderConfirmed != "order_confirmed" || TriggerBeforeShipment != "before_shipment" || TriggerAfterShipment != "after_shipment" ||
		TriggerAfterInvoice != "after_invoice" || ReleaseControlDocuments != "documents" {
		t.Fatal("the stage vocabulary moved")
	}
}
