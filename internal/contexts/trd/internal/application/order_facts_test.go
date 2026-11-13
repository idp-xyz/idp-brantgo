package application

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/amendment"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
)

// S5R-D05 in the amendment pre-fill (WF-06 step 1; R71 as PUR publishes it since S5R): an approved
// disposition of delivered goods is listed apart, as settled, with its kind, quantity and line;
// the allocation PUR states net of it is the open item, as PUR states it.
func TestS5RD05ThePrefillListsDispositionsApart(t *testing.T) {
	f := impactFacts{read: true, revision: 1, shipments: api.GuardFULShipments, downstream: api.GuardDownstream,
		commitments: []DownstreamItem{
			{Kind: "purchase_commitment", ObjectID: "commitment.1", State: "cancelled", OrderRevision: 1},
			{Kind: "demand_allocation", ObjectID: "L1", OrderLineID: "L1", State: "allocated", Quantity: "20"},
			{Kind: "commitment_disposition", ObjectID: "disp.return", OrderLineID: "L1", State: "approved", Quantity: "40",
				Disposition: "return_to_supplier"},
		}, terms: quote.Ref{Kind: "payment_terms", ID: "pay", Revision: 1}}
	note := f.notes(quote.Ref{Kind: "payment_terms", ID: "pay", Revision: 1})[amendment.AreaPurchasing]
	want := "pur.downstream_commitments.v1: purchase_commitment commitment.1 cancelled at order revision 1; demand_allocation L1 20; " +
		"disposed of with approval: disp.return return_to_supplier 40 of line L1"
	if note != want {
		t.Fatalf("purchasing note\n got %q\nwant %q", note, want)
	}
	f.commitments = f.commitments[2:]
	if note := f.notes(f.terms)[amendment.AreaPurchasing]; !strings.HasPrefix(note, "pur.downstream_commitments.v1: nothing open against the order; disposed") {
		t.Fatalf("only dispositions: %q", note)
	}
}
