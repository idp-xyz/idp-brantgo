package subjects

import (
	"context"
	"testing"

	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/api"
)

type lines struct {
	asked []purapi.CommitmentLineRef
	have  map[purapi.CommitmentLineRef]purapi.CommitmentLine
}

func (l *lines) ResolveCommitmentLines(_ context.Context, refs []purapi.CommitmentLineRef) (map[purapi.CommitmentLineRef]purapi.CommitmentLine, error) {
	l.asked = refs
	out := map[purapi.CommitmentLineRef]purapi.CommitmentLine{}
	for _, r := range refs {
		if v, ok := l.have[r]; ok {
			out[r] = v
		}
	}
	return out, nil
}

// QLT's subject is the PUR commitment line as InspectionSubject v1 states it (R24): the quantity is
// what was bought, the content hash the committed draft's; other subject kinds are never asked.
func TestPURSubjectsMapCommitmentLines(t *testing.T) {
	ref := purapi.CommitmentLineRef{CommitmentID: "commitment.1", OrderLineID: "L1"}
	l := &lines{have: map[purapi.CommitmentLineRef]purapi.CommitmentLine{ref: {Ref: ref, Status: "issued", RowVersion: 3, DraftHash: "h",
		OrderID: "order.1", OrderRevision: 2, OrderHash: "o", SupplierID: "supplier.1", SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS",
		UnitRevision: 1, AllocatedQuantity: "120", PurchaseQuantity: "130"}}}
	want := api.SubjectRef{Kind: api.SubjectCommitmentLine, ID: "commitment.1", LineID: "L1"}
	got, err := PUR{Lines: l}.ResolveSubjects(context.Background(), []api.SubjectRef{want, {Kind: "other", ID: "x", LineID: "L1"},
		{Kind: api.SubjectCommitmentLine, ID: "commitment.none", LineID: "L1"}})
	if err != nil || len(got) != 1 || len(l.asked) != 2 {
		t.Fatalf("%v, %v, asked %v", got, err, l.asked)
	}
	s := got[want]
	if s.Ref != want || s.Revision != 3 || s.Hash != "h" || s.Status != "issued" || s.Code != "" || s.OrderID != "order.1" || s.OrderRevision != 2 ||
		s.OrderHash != "o" || s.OrderLineID != "L1" || s.SupplierID != "supplier.1" || s.SkuID != "sku.1" || s.UnitID != "PCS" || s.Quantity != "130" {
		t.Fatalf("%+v", s)
	}
	if got, err := (PUR{Lines: &lines{}}).ResolveSubjects(context.Background(), []api.SubjectRef{{Kind: "other"}}); err != nil || len(got) != 0 {
		t.Fatalf("%v, %v", got, err)
	}
}
