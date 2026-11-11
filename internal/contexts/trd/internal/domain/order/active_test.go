package order

import (
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/amendment"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// activeOrder is the suite's order activated, with its revision 1 as the revision value the
// snapshot of a sealed calculation yields (package orderrev reads it from such a snapshot; this
// fixture's commercial document has no calculation bindings): L1 and L2, 60 PCS each at 8 USD.
func activeOrder(t *testing.T) *Order {
	t.Helper()
	decided := prepareOn.Add(time.Hour)
	o, r := approvedOrder(t, decided, decided.Add(48*time.Hour))
	activateOn := decided.Add(time.Hour)
	if _, err := o.Activate(Activation{Review: r, IdempotencyKey: "k.activate", ApprovalID: "approval.1"}, clerk, 3, activateOn); err != nil {
		t.Fatal(err)
	}
	ref := func(kind, id string) quote.Ref { return quote.Ref{Kind: kind, ID: id, Revision: 1} }
	tax := orderrev.Tax{Application: ref(quote.KindTaxApplication, "tax.sale"), Profile: ref(quote.KindTaxProfile, "tax.zero"),
		PriceBasis: "exclusive", Rate: "0"}
	place := quote.BusinessRef{Kind: "location", ID: "DEHAM", Revision: 1}
	r1 := &orderrev.Revision{Contract: orderrev.SnapshotContract, OrderID: o.ID, Number: 1, Hash: o.OrderHash, Customer: customer, Contact: contact,
		Currency: ref(quote.KindCurrency, "USD"), Terms: orderrev.Terms{TradeTerms: ref(quote.KindTradeTerms, "fob"), NamedPlace: place,
			PaymentTerms: ref(quote.KindPaymentTerms, "pay.1"), Delivery: quote.Delivery{Kind: "fixed", Date: "2027-01-31", Milestone: "handover",
				Place: place, TimeZone: "Asia/Shanghai"}},
		Charges: []orderrev.Charge{}, Amounts: orderrev.Amounts{Charges: []quote.AmountLine{}, Net: "96000", Tax: "0", Payable: "96000",
			Payments: []quote.Payment{{StageID: "all", Percent: "100", Trigger: "before_shipment", AmountMinor: "96000"}}}}
	for _, id := range []string{"L1", "L2"} {
		r1.Lines = append(r1.Lines, orderrev.Line{LineID: id, RfqLineID: id, Sku: quote.SkuRef{SkuID: "sku.1", SpecRevision: 1}, Quantity: "60",
			Unit: ref(quote.KindUnit, "PCS"), NormalizedQuantity: "60", CostUnit: "PCS", UnitPrice: "8", PriceBasisQuantity: "1", DiscountRatio: "0",
			Tax: tax})
		r1.Amounts.Lines = append(r1.Amounts.Lines, quote.AmountLine{ID: id, NetMinor: "48000", TaxMinor: "0", PayableMinor: "48000"})
	}
	o.SetCurrent(r1)
	return o
}

// The active order's amendments move it to its next revision once applied; its end records the
// revision it ended at (S3-15).
func TestActiveOrderAmendsAndEnds(t *testing.T) {
	o := activeOrder(t)
	r1, err := o.Revision()
	if err != nil {
		t.Fatal(err)
	}
	p := orderrev.ProposalOf(r1)
	p.Lines[0].Quantity = "50"
	facts := orderrev.Facts{Currency: kernel.MustCurrency("USD", 2), Units: map[string]calc.Unit{"PCS": {Code: "PCS"}}}
	prep := amendment.Preparation{AmendmentID: "amendment.1", OrderID: o.ID, Base: r1.Link(),
		Source: amendment.Source{Kind: amendment.SourceCustomerResponse, ResponseID: "response.9"}, Proposal: p}
	for _, area := range amendment.Areas() {
		prep.Impact = append(prep.Impact, amendment.Assessment{Area: area, Status: amendment.Unaffected})
	}
	at := prepareOn.Add(3 * time.Hour)
	rowVersion := o.RowVersion
	a, preview, _, err := o.PrepareAmendment(prep, orderrev.Fulfilment{}, facts, clerk, 3, at)
	if err != nil || o.Amendment("amendment.1") != a || o.RowVersion != rowVersion || preview.Number != 2 {
		t.Fatalf("prepare: %v %+v", err, a)
	}
	if _, err = o.SubmitAmendment("amendment.1", 1, clerk, 3, at); err != nil {
		t.Fatal(err)
	}
	if _, err = o.DecideAmendment("amendment.1", 2, amendment.Decision{Hash: a.Hash, Approve: true}, reviewer, 3, at); err != nil {
		t.Fatal(err)
	}
	standings := func(string) (quote.Standing, error) {
		return quote.Standing{Human: true, Permissions: []string{amendment.PermissionDecide, amendment.PermissionRead}, SeesRoot: true}, nil
	}
	if _, _, _, err = o.ApplyAmendment("amendment.none", amendment.Applying{ExpectedVersion: 3, Facts: facts}, standings, reviewer, 3, at); code(err) != "NOT_FOUND /args/amendment_id" {
		t.Fatalf("an unknown amendment: %s", code(err))
	}
	_, next, d, err := o.ApplyAmendment("amendment.1", amendment.Applying{ExpectedVersion: 3, IdempotencyKey: "k.apply", Facts: facts}, standings,
		reviewer, 3, at)
	if err != nil {
		t.Fatal(err)
	}
	c := o.AmendmentCommits[0]
	if o.OrderRevision != 2 || o.OrderHash != next.Hash || o.RowVersion != rowVersion+1 || o.NewRevision != next || !o.Changed ||
		len(o.Revisions) != 1 || o.Revisions[0].PreviousHash != r1.Hash || o.Revisions[0].RowVersion != o.RowVersion || !c.Fresh ||
		c.RowVersion != o.RowVersion || c.FromHash != r1.Hash || c.OrderHash != next.Hash || len(d.Lines) != 1 {
		t.Fatalf("applied: %+v", o)
	}
	var rv string
	for _, e := range c.Entries {
		if e.Name == "row_version" {
			rv = e.Value
		}
	}
	if rv != "5" {
		t.Fatalf("the commit's row version entry %s", rv)
	}
	if cur, err := o.Revision(); err != nil || cur != next {
		t.Fatalf("current %v", err)
	}
	// The snapshot still verifies at revision 2: it is revision 1, with its own hash.
	if _, err = o.Verify(scope, "/id"); err != nil {
		t.Fatalf("verify at revision 2: %v", err)
	}

	e, err := o.EndActive(orderrev.CommandCancel, orderrev.Fulfilment{Source: orderrev.NotWired}, orderrev.Downstream{Source: orderrev.NotWired}, nil,
		quote.Act{By: clerk, Epoch: 3, At: at, Reason: "客户撤单"})
	if err != nil || o.Status != Cancelled || e.OrderRevision != 2 || e.OrderHash != next.Hash || e.RowVersion != o.RowVersion || !e.Fresh ||
		e.Lines[0].Ordered != "50" {
		t.Fatalf("cancelled: %v %+v", err, e)
	}
	if _, err = o.EndActive(orderrev.CommandClose, orderrev.Fulfilment{}, orderrev.Downstream{}, nil, quote.Act{By: clerk, At: at}); code(err) != "ORDER_NOT_ACTIVE /args/order_id" {
		t.Fatalf("closing a cancelled order: %s", code(err))
	}
	if _, err = o.EndActive("order.reopen", orderrev.Fulfilment{}, orderrev.Downstream{}, nil, quote.Act{By: clerk, At: at}); code(err) != "INVALID_STATE /command" {
		t.Fatalf("an unknown end: %s", code(err))
	}
	if _, _, _, err = o.PrepareAmendment(prep, orderrev.Fulfilment{}, facts, clerk, 3, at); code(err) != "ORDER_NOT_ACTIVE /args/order_id" {
		t.Fatalf("amending a cancelled order: %s", code(err))
	}
}

// A draft order has no amendments, and its current revision is the snapshot's; a revision that is
// not the order's is never taken as current.
func TestDraftOrderHasNoAmendments(t *testing.T) {
	o, _ := prepared(t)
	if _, _, _, err := o.PrepareAmendment(amendment.Preparation{AmendmentID: "amendment.1", OrderID: o.ID}, orderrev.Fulfilment{},
		orderrev.Facts{}, clerk, 1, prepareOn); code(err) != "ORDER_NOT_ACTIVE /args/order_id" {
		t.Fatalf("amending a draft: %s", code(err))
	}
	if _, err := o.SubmitAmendment("amendment.1", 1, clerk, 1, prepareOn); code(err) != "NOT_FOUND /args/amendment_id" {
		t.Fatalf("submitting an unknown amendment: %s", code(err))
	}
	o.SetCurrent(&orderrev.Revision{OrderID: o.ID, Number: 1, Hash: strings.Repeat("0", 64)})
	if _, err := o.Revision(); code(err) != "REFERENCE_INVALID /order" {
		t.Fatalf("a foreign current revision: %s", code(err))
	}
}
