package upstream

import (
	"context"
	"testing"

	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/application"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
)

type lookups struct {
	purapi.DeliveryLookups
	lines map[purapi.DeliveryLineRef]purapi.DeliveryLine
	asked []purapi.DeliveryLineRef
}

func (l *lookups) ResolveDeliveryLines(_ context.Context, refs []purapi.DeliveryLineRef) (map[purapi.DeliveryLineRef]purapi.DeliveryLine, error) {
	l.asked = refs
	return l.lines, nil
}

// R59 before FUL's transaction: PUR's delivery line with the commitment line it is delivered against
// becomes FUL's DeliveryLine; a line PUR does not answer is absent; PUR's code is passed on.
func TestPURDeliveryLines(t *testing.T) {
	verified := purapi.DeliveryLineRef{DeliveryID: "delivery.1", LineID: "L1", Version: 2}
	claim := purapi.DeliveryLineRef{DeliveryID: "delivery.2", LineID: "L1", Version: 1}
	l := &lookups{lines: map[purapi.DeliveryLineRef]purapi.DeliveryLine{
		verified: {Ref: verified, Status: "verified", VerifiedVersion: 2, VerifiedQuantity: "100", Line: purapi.CommitmentLine{
			Ref: purapi.CommitmentLineRef{CommitmentID: "commitment.1", OrderLineID: "L1"}, OrderID: "order.1", SupplierID: "supplier.1", SkuID: "sku.1",
			SpecRevision: 2, UnitID: "pcs", UnitRevision: 1}},
		claim: {Ref: claim, Status: "recorded", Code: purapi.CodeSupplierDeliveryUnverified},
	}}
	refs := []fulapi.DeliveryLineRef{{DeliveryID: "delivery.1", LineID: "L1", Version: 2}, {DeliveryID: "delivery.2", LineID: "L1", Version: 1},
		{DeliveryID: "delivery.9", LineID: "L1", Version: 1}}
	got, err := PUR{Deliveries: l}.ResolveDeliveryLines(context.Background(), refs)
	if err != nil || len(got) != 2 || len(l.asked) != 3 || l.asked[0] != verified {
		t.Fatalf("%+v %v asked %+v", got, err, l.asked)
	}
	want := application.DeliveryLine{Ref: refs[0], Known: true, Status: "verified", VerifiedVersion: 2, CommitmentID: "commitment.1",
		SupplierID: "supplier.1", OrderID: "order.1", OrderLineID: "L1", SkuID: "sku.1", SpecRevision: 2, UnitID: "pcs", UnitRevision: 1,
		VerifiedQuantity: "100"}
	if got[refs[0]] != want || got[refs[1]].Code != fulapi.CodeSupplierDeliveryUnverified || !got[refs[1]].Known {
		t.Fatalf("%+v", got)
	}
	if out, err := (PUR{Deliveries: l}).ResolveDeliveryLines(context.Background(), nil); err != nil || len(out) != 0 {
		t.Fatal("no refs, no lookup")
	}
}
