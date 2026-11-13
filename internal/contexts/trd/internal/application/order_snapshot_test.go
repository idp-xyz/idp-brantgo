package application

import (
	"testing"

	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// OrderCommercialSnapshot v1 fails closed (S5 plan unit A2): an order whose snapshot does not hash
// to its order hash keeps only its head — binding, state, end, row version and source — with Code
// REFERENCE_INVALID, never a partial fact a downstream could reserve credit or ship on. The
// database refuses such a change (migrations/trd/000005's triggers), so only a unit test reaches it.
func TestABrokenSnapshotKeepsOnlyTheHead(t *testing.T) {
	o := &order.Order{ID: "order.1", Status: order.Active, OrderRevision: 1, OrderHash: "h", RowVersion: 4, Snapshot: []byte(`{}`),
		SnapshotHash: "h", Source: order.Source{QuoteID: "quote.1", Revision: 1, BusinessHash: "b", AcceptanceID: "acceptance.1"}}
	f := orderSnapshotFact(o, kernel.Scope{Tenant: "t", Company: "c"})
	want := api.OrderSnapshotFact{Binding: api.OrderBinding{OrderID: "order.1", OrderRevision: 1, OrderHash: "h"}, Status: api.OrderActive,
		RowVersion: 4, QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: "b", AcceptanceID: "acceptance.1", Code: kernel.CodeReferenceInvalid}
	if f.Check("/x") == nil || f.Binding != want.Binding || f.Status != want.Status || f.RowVersion != want.RowVersion || f.QuoteID != want.QuoteID ||
		f.AcceptanceID != want.AcceptanceID || f.Stages != nil || f.Lines != nil || f.CreditPortionMinor != "" {
		t.Fatalf("a broken snapshot %+v", f)
	}
	if a := activeOrderFact(o, kernel.Scope{Tenant: "t", Company: "c"}); a.Code != kernel.CodeReferenceInvalid || a.Terms.Stages != nil {
		t.Fatalf("ActiveOrderRef v1 of a broken snapshot %+v", a)
	}
}

// The Incoterm and edition are those of the trade_terms revision the order binds; one POL does not
// know, or a reference of another kind, states nothing (the caller turns the fact into
// REFERENCE_INVALID rather than leave the Incoterm empty, so no CIF or CIP order passes as one
// without insurance).
func TestTradeTermsAreTheBoundRevisions(t *testing.T) {
	fca := api.ConfigRef{Kind: "trade_terms", ID: "FCA", Revision: 1}
	known := map[api.ConfigRef]polapi.TradeTerms{fca: {Incoterm: "FCA", Edition: "2020"},
		{Kind: "payment_terms", ID: "FCA", Revision: 1}: {Incoterm: "CIF"}}
	tt := api.TradeTerms{Ref: fca}
	if !stateTradeTerms(&tt, known) || tt.Incoterm != "FCA" || tt.Edition != "2020" {
		t.Fatalf("known %+v", tt)
	}
	for _, r := range []api.ConfigRef{{Kind: "trade_terms", ID: "FCA", Revision: 2}, {Kind: "payment_terms", ID: "FCA", Revision: 1}} {
		tt := api.TradeTerms{Ref: r}
		if stateTradeTerms(&tt, known) || tt.Incoterm != "" {
			t.Errorf("%+v stated %+v", r, tt)
		}
	}
}

// order.snapshot.get is an audited read under trd.read any actor kind may run, as order.get is.
func TestOrderSnapshotReadNeedsTrdRead(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	r, err := policy.Rule(CommandGetOrderSnapshot)
	if err != nil || r.Permission != PermissionRead || len(r.ActorKinds) != 3 {
		t.Fatalf("%+v %v", r, err)
	}
}
