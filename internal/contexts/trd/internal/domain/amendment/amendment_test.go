package amendment

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// BRC-001's order at revision 1 (orderrev's fixture reads the same revision from a sealed quote's
// snapshot): L1 red S 24, L2 red M 36, L3 blue S 12, L4 blue M 48 PCS at 8 USD, 960.00 paid 30/70.
func revisionOne() *orderrev.Revision {
	ref := func(kind, id string) quote.Ref { return quote.Ref{Kind: kind, ID: id, Revision: 1} }
	tax := orderrev.Tax{Application: ref(quote.KindTaxApplication, "tax.sale"), Profile: ref(quote.KindTaxProfile, "tax.zero"),
		PriceBasis: "exclusive", Rate: "0"}
	r := &orderrev.Revision{Contract: orderrev.SnapshotContract, Scope: orderrev.Scope{Tenant: "tenant.1", Company: "company.live"},
		OrderID: "order.1", Number: 1, Hash: quote.Hash([]byte("order.1 snapshot")),
		Source: orderrev.Source{QuoteID: "quote.1", Revision: 1, BusinessHash: quote.Hash([]byte("quote.1 r1")), Number: "QT-2026-00001",
			DocumentSHA256: quote.Hash([]byte("QT-2026-00001.txt")), AcceptanceID: "acceptance.1"},
		Customer: quote.PartyRef{PartyID: "customer.1", Revision: 2}, Contact: quote.ContactRef{ContactID: "contact.1", RowVersion: 2},
		Currency: ref(quote.KindCurrency, "USD"),
		Terms: orderrev.Terms{TradeTerms: ref(quote.KindTradeTerms, "terms.fca"), NamedPlace: quote.BusinessRef{Kind: "location", ID: "DEHAM", Revision: 1},
			PaymentTerms: ref(quote.KindPaymentTerms, "payment.30-70"), Delivery: quote.Delivery{Kind: "fixed", Date: "2027-01-31", Milestone: "handover",
				Place: quote.BusinessRef{Kind: "location", ID: "DEHAM", Revision: 1}, TimeZone: "Asia/Shanghai"}},
		Amounts: orderrev.Amounts{Charges: []quote.AmountLine{}, Net: "96000", Tax: "0", Payable: "96000",
			Payments: []quote.Payment{{StageID: "P1", Percent: "30", Trigger: "order_confirmed", AmountMinor: "28800"},
				{StageID: "P2", Percent: "70", Trigger: "after_shipment", Days: 30, AmountMinor: "67200"}}},
		Charges: []orderrev.Charge{}}
	for _, l := range []struct{ id, sku, qty, net string }{{"L1", "ST01.red.S", "24", "19200"}, {"L2", "ST01.red.M", "36", "28800"},
		{"L3", "ST01.blue.S", "12", "9600"}, {"L4", "ST01.blue.M", "48", "38400"}} {
		r.Lines = append(r.Lines, orderrev.Line{LineID: l.id, RfqLineID: l.id, Sku: quote.SkuRef{SkuID: l.sku, SpecRevision: 1}, Quantity: l.qty,
			Unit: ref(quote.KindUnit, "PCS"), NormalizedQuantity: l.qty, CostUnit: "PCS", UnitPrice: "8", PriceBasisQuantity: "1", DiscountRatio: "0",
			Tax: tax})
		r.Amounts.Lines = append(r.Amounts.Lines, quote.AmountLine{ID: l.id, NetMinor: l.net, TaxMinor: "0", PayableMinor: l.net})
	}
	return r
}

var (
	preparedAt = time.Date(2027, 1, 10, 9, 0, 0, 0, time.UTC)
	clerk      = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "clerk"}
	agent      = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"}
	merch      = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "merchandiser"}
	reviewer   = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "order.reviewer"}
	applier    = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "order.reviewer.2"}
	proof      = []quote.Evidence{{SourceID: "source.7", Locator: "客户邮件 2027-01-09：蓝 M 由 48 改为 44"}}
	qualified  = func(string) (quote.Standing, error) {
		return quote.Standing{Human: true, Permissions: []string{PermissionDecide, PermissionRead}, SeesRoot: true}, nil
	}
)

func facts() orderrev.Facts {
	return orderrev.Facts{Currency: kernel.MustCurrency("USD", 2), Units: map[string]calc.Unit{"PCS": {Code: "PCS"}}}
}

func at(minutes int) time.Time { return preparedAt.Add(time.Duration(minutes) * time.Minute) }

// impact is a complete assessment: purchasing affected (the purchase draft for blue M is voided in
// part, SIM-02 step 7), payment plan affected, the rest unaffected but booking, which is open.
func impact() []Assessment {
	out := []Assessment{}
	for _, area := range Areas() {
		a := Assessment{Area: area, Status: Unaffected, Evidence: []quote.Evidence{}}
		switch area {
		case AreaPurchasing:
			a.Status, a.Note = Affected, "采购草稿 pd.4 的蓝 M 分配由 48 减为 44，余量作废（purchase.draft.void）"
		case AreaPaymentPlan:
			a.Status, a.Note = Affected, "应付由 960.00 减为 928.00：278.40 + 649.60"
		case AreaBooking:
			a.Status, a.Note = Open, "订舱体积待货代确认"
		}
		out = append(out, a)
	}
	return out
}

// toFortyFour is ES-09's change: blue M (L4) from 48 to 44, from customer response response.7.
func toFortyFour(r *orderrev.Revision, assessed []Assessment) Preparation {
	p := orderrev.ProposalOf(r)
	p.Lines[3].Quantity = "44"
	return Preparation{AmendmentID: "amendment.1", OrderID: "order.1", Base: r.Link(),
		Source: Source{Kind: SourceCustomerResponse, ResponseID: "response.7", Evidence: proof}, Proposal: p, Impact: assessed}
}

func activeAt(r *orderrev.Revision) Order { return Order{Status: orderrev.Active, Current: r} }

func shipped(lines ...string) orderrev.Fulfilment {
	f := orderrev.Fulfilment{Source: "ful.shipment_confirmation.v1", Shipped: map[string]kernel.Decimal{}}
	for i := 0; i+1 < len(lines); i += 2 {
		f.Shipped[lines[i]] = kernel.MustDecimal(lines[i+1])
	}
	return f
}

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code + " at " + rej.Fields[0].Path
	}
	if err != nil {
		return "ERROR " + err.Error()
	}
	return "OK"
}

// approved runs ES-09 steps 2–3 on revision r: the AI agent prepares the change with an incomplete
// assessment, the clerk prepares it again complete and submits it, and the reviewer approves it.
func approved(t *testing.T, r *orderrev.Revision, f orderrev.Fulfilment) *Amendment {
	t.Helper()
	a, _, _, err := Prepare(nil, toFortyFour(r, impact()[:2]), activeAt(r), f, facts(), agent, 1, at(0))
	if err != nil {
		t.Fatal(err)
	}
	if a, _, _, err = Prepare(a, func() Preparation { p := toFortyFour(r, impact()); p.ExpectedVersion = 1; return p }(), activeAt(r), f, facts(),
		clerk, 1, at(1)); err != nil {
		t.Fatal(err)
	}
	if err = a.Submit(2, activeAt(r), clerk, 1, at(2)); err != nil {
		t.Fatal(err)
	}
	if err = a.Decide(3, Decision{Hash: a.Hash, Approve: true, Reason: "客户书面确认减量"}, activeAt(r), reviewer, 1, at(3)); err != nil {
		t.Fatal(err)
	}
	return a
}

// ES-09 and SIM-02 step 7, with TD-AT01's shipments: blue M shipped 40; the amendment to 44 is
// prepared (by an AI agent, then a human), submitted, decided by an independent reviewer and
// applied; the order's revision 2 is the preview the reviewer saw, and the commit evidence names
// every step.
func TestAmendmentLifecycle(t *testing.T) {
	r1 := revisionOne()
	f := shipped("L4", "40")
	a, preview, d, err := Prepare(nil, toFortyFour(r1, impact()[:2]), activeAt(r1), f, facts(), agent, 1, at(0))
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != Draft || a.Version != 1 || !a.Fresh || a.Hash == "" || preview.Number != 2 || preview.Amendment.Hash != a.Hash ||
		len(d.Lines) != 1 || d.Lines[0].LineID != "L4" || preview.Amounts.Net != "92800" {
		t.Fatalf("prepared: %+v %+v %+v", a, preview, d)
	}
	first := a.Hash
	if err := a.Submit(1, activeAt(r1), clerk, 1, at(1)); code(err) != "DRAFT_NOT_READY at /impact/operations" {
		t.Fatalf("submit with two areas assessed: %s", code(err))
	}
	p := toFortyFour(r1, impact())
	p.ExpectedVersion = 1
	if a, preview, _, err = Prepare(a, p, activeAt(r1), f, facts(), clerk, 2, at(1)); err != nil {
		t.Fatal(err)
	}
	if a.Version != 2 || a.Hash == first || !slices.Equal(a.Preparers(), []kernel.ActorRef{agent, clerk}) ||
		!slices.Equal(a.OpenItems(), []string{AreaBooking}) {
		t.Fatalf("prepared again: %+v", a)
	}
	if err = a.Submit(2, activeAt(r1), merch, 2, at(2)); err != nil || a.Status != PendingApproval || a.Version != 3 {
		t.Fatalf("submit: %v %+v", err, a)
	}
	for _, by := range []kernel.ActorRef{clerk, merch} {
		if err := a.Decide(3, Decision{Hash: a.Hash, Approve: true}, activeAt(r1), by, 1, at(3)); code(err) != "REVIEWER_NOT_INDEPENDENT at /args/amendment_id" {
			t.Fatalf("decided by %s: %s", by.ID, code(err))
		}
	}
	if err := a.Decide(3, Decision{Hash: first, Approve: true}, activeAt(r1), reviewer, 1, at(3)); code(err) != "ORDER_REVIEW_MISMATCH at /args/amendment_hash" {
		t.Fatalf("decided on the first draft's hash: %s", code(err))
	}
	if err = a.Decide(3, Decision{Hash: a.Hash, Approve: true, Reason: "客户书面确认减量"}, activeAt(r1), reviewer, 4, at(3)); err != nil ||
		a.Status != Approved || a.Version != 4 || a.Decided.By != reviewer {
		t.Fatalf("approve: %v %+v", err, a)
	}
	r2, d, commit, err := a.Apply(Applying{ExpectedVersion: 4, IdempotencyKey: "key.apply", Fulfilment: f, Facts: facts(),
		Upstream:     []quote.Entry{{Name: "stl.credit_guard.v1", Value: orderrev.NotWired}, {Name: "cnx.trade_compliance_guard.v1", Value: orderrev.NotWired}},
		ControlEpoch: 0, RowVersion: 7}, activeAt(r1), qualified, applier, 5, at(4))
	if err != nil {
		t.Fatal(err)
	}
	if r2.Hash != preview.Hash || !bytes.Equal(r2.Document, preview.Document) || a.Status != Applied || a.Version != 5 ||
		a.Applied.From != r1.Link() || a.Applied.To != r2.Link() || a.Applied.Act.By != applier || len(d.Lines) != 1 {
		t.Fatalf("applied: %+v %+v", a, r2)
	}
	var names, values []string
	for _, e := range commit.Entries {
		names, values = append(names, e.Name), append(values, e.Value)
	}
	if !slices.Equal(names, []string{"command", "idempotency_key", "order_id", "amendment_id", "amendment_hash", "from_order_revision",
		"from_order_hash", "to_order_revision", "to_order_hash", "source", "prepared_by", "submitted_by", "decided_by", "decided_at", "open_items",
		"fulfilment_source", "shipped", "stl.credit_guard.v1", "cnx.trade_compliance_guard.v1", "authority_epoch", "control_epoch", "row_version",
		"committed_by", "committed_at"}) {
		t.Fatalf("entries: %q", names)
	}
	want := map[string]string{"command": CommandApply, "to_order_revision": "2", "to_order_hash": r2.Hash, "from_order_hash": r1.Hash,
		"source": "customer_response:response.7", "prepared_by": "ai:agent.1,human:clerk", "submitted_by": "human:merchandiser",
		"decided_by": "human:order.reviewer", "open_items": "booking", "shipped": "L4=40", "fulfilment_source": "ful.shipment_confirmation.v1",
		"stl.credit_guard.v1": "not_wired", "authority_epoch": "5", "row_version": "7", "committed_at": "2027-01-10T09:04:00.000000Z"}
	for i, n := range names {
		if w, ok := want[n]; ok && values[i] != w {
			t.Errorf("entry %s = %q, want %q", n, values[i], w)
		}
	}
	// Applied is final.
	if _, _, _, err := a.Apply(Applying{ExpectedVersion: 5, Facts: facts()}, activeAt(r2), qualified, applier, 5, at(5)); code(err) != "INVALID_STATE at /args/amendment_id" {
		t.Fatalf("applied twice: %s", code(err))
	}
}

// TD-AT01 at the amendment: with 40 of blue M shipped, an amendment to 36 is never prepared; one
// prepared to 38 while 30 had shipped is refused at apply once 40 have (QUANTITY_BELOW_SHIPPED):
// the shipments confirmed when it applies decide.
func TestTDAT01QuantityNeverBelowShipped(t *testing.T) {
	r1 := revisionOne()
	p := toFortyFour(r1, impact())
	p.Proposal.Lines[3].Quantity = "36"
	if _, _, _, err := Prepare(nil, p, activeAt(r1), shipped("L4", "40"), facts(), clerk, 1, at(0)); code(err) != "QUANTITY_BELOW_SHIPPED at /args/proposal/lines/3/quantity" {
		t.Fatalf("36 after 40: %s", code(err))
	}
	p.Proposal.Lines[3].Quantity = "38"
	a, _, _, err := Prepare(nil, p, activeAt(r1), shipped("L4", "30"), facts(), clerk, 1, at(0))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Submit(1, activeAt(r1), clerk, 1, at(1)); err != nil {
		t.Fatal(err)
	}
	if err = a.Decide(2, Decision{Hash: a.Hash, Approve: true}, activeAt(r1), reviewer, 1, at(2)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := a.Apply(Applying{ExpectedVersion: 3, Fulfilment: shipped("L4", "40"), Facts: facts()}, activeAt(r1), qualified, applier, 1,
		at(3)); code(err) != "QUANTITY_BELOW_SHIPPED at /proposal/lines/3/quantity" {
		t.Fatalf("38 applied after 40 shipped: %s", code(err))
	}
	if a.Status != Approved || a.Version != 3 {
		t.Fatalf("a refused apply changed the amendment: %+v", a)
	}
}

// TRD-INV-018: two amendments of one revision, both approved; the first applied wins, the second is
// ORDER_AMENDMENT_STALE (it binds the old revision) and stays approved, stale; a draft bound to the
// old revision is prepared again on the new one.
func TestTwoAmendmentsOfOneRevisionHaveOneWinner(t *testing.T) {
	r1 := revisionOne()
	first := approved(t, r1, orderrev.Fulfilment{})
	p := toFortyFour(r1, impact())
	p.AmendmentID = "amendment.2"
	p.Proposal.Lines[0].Quantity = "30"
	second, _, _, err := Prepare(nil, p, activeAt(r1), orderrev.Fulfilment{}, facts(), clerk, 1, at(0))
	if err != nil {
		t.Fatal(err)
	}
	if err = second.Submit(1, activeAt(r1), clerk, 1, at(1)); err != nil {
		t.Fatal(err)
	}
	if err = second.Decide(2, Decision{Hash: second.Hash, Approve: true}, activeAt(r1), reviewer, 1, at(2)); err != nil {
		t.Fatal(err)
	}
	r2, _, _, err := first.Apply(Applying{ExpectedVersion: first.Version, Facts: facts()}, activeAt(r1), qualified, applier, 1, at(5))
	if err != nil {
		t.Fatal(err)
	}
	if !second.Stale(activeAt(r2)) || first.Stale(activeAt(r1)) {
		t.Fatal("staleness")
	}
	if _, _, _, err := second.Apply(Applying{ExpectedVersion: 3, Facts: facts()}, activeAt(r2), qualified, applier, 1, at(6)); code(err) != "ORDER_AMENDMENT_STALE at /args/amendment_id" {
		t.Fatalf("second apply: %s", code(err))
	}
	if second.Status != Approved {
		t.Fatalf("second: %s", second.Status)
	}
	// A draft bound to revision 1 cannot be submitted; prepared again on revision 2 it can.
	p.AmendmentID = "amendment.3"
	third, _, _, err := Prepare(nil, p, activeAt(r1), orderrev.Fulfilment{}, facts(), clerk, 1, at(0))
	if err != nil {
		t.Fatal(err)
	}
	if err := third.Submit(1, activeAt(r2), clerk, 1, at(7)); code(err) != "ORDER_AMENDMENT_STALE at /args/amendment_id" {
		t.Fatalf("stale submit: %s", code(err))
	}
	if _, _, _, err := Prepare(third, p, activeAt(r2), orderrev.Fulfilment{}, facts(), clerk, 1, at(7)); code(err) != "VERSION_CONFLICT at /args/expected_version" {
		t.Fatalf("prepare at version 0: %s", code(err))
	}
	p.ExpectedVersion = 1
	if _, _, _, err := Prepare(third, p, activeAt(r2), orderrev.Fulfilment{}, facts(), clerk, 1, at(7)); code(err) != "ORDER_AMENDMENT_STALE at /args/expected_order_revision" {
		t.Fatalf("prepare on the old revision: %s", code(err))
	}
	next := toFortyFour(r2, impact())
	next.AmendmentID, next.ExpectedVersion = "amendment.3", 1
	next.Proposal.Lines[0].Quantity = "30"
	if third, _, _, err = Prepare(third, next, activeAt(r2), orderrev.Fulfilment{}, facts(), clerk, 1, at(8)); err != nil || third.Content.Base != r2.Link() {
		t.Fatalf("prepared on revision 2: %v", err)
	}
}

// What prepare refuses.
func TestPrepareRefusals(t *testing.T) {
	r1 := revisionOne()
	for name, c := range map[string]struct {
		edit  func(p *Preparation)
		order Order
		want  string
	}{
		"a draft order":      {func(*Preparation) {}, Order{Status: orderrev.Draft, Current: r1}, "ORDER_NOT_ACTIVE at /args/order_id"},
		"a closed order":     {func(*Preparation) {}, Order{Status: orderrev.Closed, Current: r1}, "ORDER_NOT_ACTIVE at /args/order_id"},
		"an older revision":  {func(p *Preparation) { p.Base.Hash = strings.Repeat("0", 64) }, activeAt(r1), "ORDER_AMENDMENT_STALE at /args/expected_order_revision"},
		"another order":      {func(p *Preparation) { p.OrderID = "order.2" }, activeAt(r1), "REFERENCE_INVALID at /args/order_id"},
		"a version of a new": {func(p *Preparation) { p.ExpectedVersion = 1 }, activeAt(r1), "VERSION_CONFLICT at /args/expected_version"},
		"a malformed ID":     {func(p *Preparation) { p.AmendmentID = "a b" }, activeAt(r1), "SCHEMA_INVALID at /args/amendment_id"},
		"an unknown source":  {func(p *Preparation) { p.Source.Kind = "phone_call" }, activeAt(r1), "SCHEMA_INVALID at /args/source/kind"},
		"a response without its ID": {func(p *Preparation) { p.Source.ResponseID = "" }, activeAt(r1),
			"SCHEMA_INVALID at /args/source/response_id"},
		"a PO revision without the PO": {func(p *Preparation) { p.Source = Source{Kind: SourceCustomerPORevision} }, activeAt(r1),
			"INVALID_KNOWN_VALUE at /args/source/evidence"},
		"a PO revision naming a response": {func(p *Preparation) {
			p.Source = Source{Kind: SourceCustomerPORevision, ResponseID: "response.7", Evidence: proof}
		}, activeAt(r1), "SCHEMA_INVALID at /args/source/response_id"},
		"evidence without a locator": {func(p *Preparation) { p.Source.Evidence = []quote.Evidence{{SourceID: "source.7"}} }, activeAt(r1),
			"SCHEMA_INVALID at /args/source/evidence/0"},
		"an unknown area":     {func(p *Preparation) { p.Impact[0].Area = "legal" }, activeAt(r1), "SCHEMA_INVALID at /args/impact/0/area"},
		"an area twice":       {func(p *Preparation) { p.Impact[1].Area = AreaPurchasing }, activeAt(r1), "INVALID_KNOWN_VALUE at /args/impact/1/area"},
		"an unknown status":   {func(p *Preparation) { p.Impact[0].Status = "maybe" }, activeAt(r1), "SCHEMA_INVALID at /args/impact/0/status"},
		"an open item unsaid": {func(p *Preparation) { p.Impact[5].Note = " " }, activeAt(r1), "INVALID_KNOWN_VALUE at /args/impact/5/note"},
		"no change": {func(p *Preparation) { p.Proposal.Lines[3].Quantity = "48.000000" }, activeAt(r1),
			"INVALID_KNOWN_VALUE at /args/proposal"},
		"a bad proposal": {func(p *Preparation) { p.Proposal.Lines[3].Quantity = "-1" }, activeAt(r1),
			"SCHEMA_INVALID at /args/proposal/lines/3/quantity"},
	} {
		p := toFortyFour(r1, impact())
		c.edit(&p)
		if _, _, _, err := Prepare(nil, p, c.order, orderrev.Fulfilment{}, facts(), clerk, 1, at(0)); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
	// Only a draft is prepared again.
	a := approved(t, r1, orderrev.Fulfilment{})
	p := toFortyFour(r1, impact())
	p.ExpectedVersion = a.Version
	if _, _, _, err := Prepare(a, p, activeAt(r1), orderrev.Fulfilment{}, facts(), clerk, 1, at(9)); code(err) != "INVALID_STATE at /args/amendment_id" {
		t.Fatalf("prepare an approved amendment: %s", code(err))
	}
}

// What decide, apply and withdraw refuse, and what withdraw does.
func TestDecideApplyWithdrawRefusals(t *testing.T) {
	r1 := revisionOne()
	pending := func() *Amendment {
		a, _, _, err := Prepare(nil, toFortyFour(r1, impact()), activeAt(r1), orderrev.Fulfilment{}, facts(), clerk, 1, at(0))
		if err != nil {
			t.Fatal(err)
		}
		if err = a.Submit(1, activeAt(r1), clerk, 1, at(1)); err != nil {
			t.Fatal(err)
		}
		return a
	}
	a := pending()
	ai := kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.2"}
	other := revisionOne()
	other.Hash = strings.Repeat("1", 64)
	for name, c := range map[string]struct {
		run  func() error
		want string
	}{
		"another version": {func() error {
			return a.Decide(1, Decision{Hash: a.Hash, Approve: true}, activeAt(r1), reviewer, 1, at(2))
		}, "VERSION_CONFLICT at /args/expected_version"},
		"the order moved": {func() error {
			return a.Decide(2, Decision{Hash: a.Hash, Approve: true}, activeAt(other), reviewer, 1, at(2))
		}, "ORDER_AMENDMENT_STALE at /args/amendment_id"},
		"the order was cancelled": {func() error {
			return a.Decide(2, Decision{Hash: a.Hash}, Order{Status: orderrev.Cancelled, Current: r1}, reviewer, 1, at(2))
		}, "ORDER_NOT_ACTIVE at /args/order_id"},
		"a rejection without why": {func() error { return a.Decide(2, Decision{Hash: a.Hash}, activeAt(r1), reviewer, 1, at(2)) }, "SCHEMA_INVALID at /args/reason"},
		"an AI decider":           {func() error { return a.Decide(2, Decision{Hash: a.Hash, Approve: true}, activeAt(r1), ai, 1, at(2)) }, "PERMISSION_DENIED at /"},
		"apply before the decision": {func() error {
			_, _, _, err := a.Apply(Applying{ExpectedVersion: 2}, activeAt(r1), qualified, applier, 1, at(2))
			return err
		}, "ORDER_APPROVAL_REQUIRED at /args/amendment_id"},
		"withdraw without a reason": {func() error { return a.Withdraw(2, quote.Act{By: clerk, At: at(2)}) }, "SCHEMA_INVALID at /args/reason"},
		"withdraw by an AI agent":   {func() error { return a.Withdraw(2, quote.Act{By: ai, At: at(2), Reason: "x"}) }, "PERMISSION_DENIED at /"},
		"submit a pending one":      {func() error { return a.Submit(2, activeAt(r1), clerk, 1, at(2)) }, "INVALID_STATE at /args/amendment_id"},
	} {
		if err := c.run(); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
	if err := a.Decide(2, Decision{Hash: a.Hash, Reason: "客户未书面确认"}, activeAt(r1), reviewer, 1, at(2)); err != nil || a.Status != Rejected {
		t.Fatalf("reject: %v %s", err, a.Status)
	}
	if err := a.Decide(3, Decision{Hash: a.Hash, Approve: true}, activeAt(r1), reviewer, 1, at(3)); code(err) != "ORDER_REVIEW_IMMUTABLE at /args/amendment_id" {
		t.Fatalf("decided twice: %s", code(err))
	}
	if err := a.Withdraw(3, quote.Act{By: clerk, At: at(3), Reason: "x"}); code(err) != "INVALID_STATE at /args/amendment_id" {
		t.Fatalf("withdraw a rejected one: %s", code(err))
	}

	// The decider's qualification is read again when the amendment applies.
	b := approved(t, r1, orderrev.Fulfilment{})
	revoked := func(string) (quote.Standing, error) {
		return quote.Standing{Human: true, Permissions: []string{PermissionRead}, SeesRoot: true}, nil
	}
	unseen := func(string) (quote.Standing, error) {
		return quote.Standing{Human: true, Permissions: []string{PermissionDecide, PermissionRead}}, nil
	}
	broken := func(string) (quote.Standing, error) { return quote.Standing{}, errors.New("storage") }
	for name, s := range map[string]quote.Standings{"decide revoked": revoked, "order out of scope": unseen} {
		if _, _, _, err := b.Apply(Applying{ExpectedVersion: b.Version, Facts: facts()}, activeAt(r1), s, applier, 1, at(4)); code(err) != "ORDER_APPROVAL_REQUIRED at /args/amendment_id" {
			t.Errorf("%s: %s", name, code(err))
		}
	}
	if _, _, _, err := b.Apply(Applying{ExpectedVersion: b.Version, Facts: facts()}, activeAt(r1), broken, applier, 1, at(4)); err == nil || err.Error() != "storage" {
		t.Errorf("standings failure: %v", err)
	}
	if _, _, _, err := b.Apply(Applying{ExpectedVersion: b.Version, Facts: facts()}, Order{Status: orderrev.Closed, Current: r1}, qualified, applier, 1,
		at(4)); code(err) != "ORDER_NOT_ACTIVE at /args/order_id" {
		t.Errorf("apply on a closed order: %s", code(err))
	}
	if _, _, _, err := b.Apply(Applying{ExpectedVersion: 1, Facts: facts()}, activeAt(r1), qualified, applier, 1, at(4)); code(err) != "VERSION_CONFLICT at /args/expected_version" {
		t.Errorf("apply at another version: %s", code(err))
	}
	if _, _, _, err := b.Apply(Applying{ExpectedVersion: b.Version, Facts: facts()}, activeAt(r1), qualified, ai, 1, at(4)); code(err) != "PERMISSION_DENIED at /" {
		t.Errorf("apply by an AI agent: %s", code(err))
	}

	// Withdrawing a draft or a pending amendment.
	c := pending()
	if err := c.Withdraw(2, quote.Act{By: merch, At: at(3), Reason: "客户撤回改单"}); err != nil || c.Status != Withdrawn || c.Version != 3 ||
		c.Withdrawn.By != merch {
		t.Fatalf("withdraw pending: %v %+v", err, c)
	}
	d, _, _, err := Prepare(nil, toFortyFour(r1, nil), activeAt(r1), orderrev.Fulfilment{}, facts(), agent, 1, at(0))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Withdraw(1, quote.Act{By: clerk, At: at(1), Reason: "重复"}); err != nil || d.Status != Withdrawn {
		t.Fatalf("withdraw draft: %v", err)
	}
}

// The amendment hash covers the normalized content: equivalent decimals and line order hash alike;
// the golden vector testdata/amendment-es09.json is ES-09's amendment, complete.
func TestAmendmentHashIsTheCanonicalContent(t *testing.T) {
	r1 := revisionOne()
	a, _, _, err := Prepare(nil, toFortyFour(r1, impact()), activeAt(r1), orderrev.Fulfilment{}, facts(), clerk, 1, at(0))
	if err != nil {
		t.Fatal(err)
	}
	p := toFortyFour(r1, impact())
	p.Proposal.Lines[3].Quantity = "44.000000"
	p.Proposal.Lines[0].UnitPrice = "8.00"
	slices.Reverse(p.Proposal.Lines)
	slices.Reverse(p.Impact)
	b, _, _, err := Prepare(nil, p, activeAt(r1), orderrev.Fulfilment{}, facts(), clerk, 1, at(0))
	if err != nil || b.Hash != a.Hash {
		t.Fatalf("equivalent content: %v %s %s", err, b.Hash, a.Hash)
	}
	doc, err := a.Document()
	if err != nil || quote.Hash(doc) != a.Hash {
		t.Fatalf("document: %v", err)
	}
	path := filepath.Join("testdata", "amendment-es09.json")
	if os.Getenv("AMENDMENT_WRITE_VECTORS") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, doc, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(doc, want) {
		t.Fatalf("golden: %v\n got %s\nwant %s", err, doc, want)
	}
	if a.Hash != es09AmendmentHash {
		t.Fatalf("hash %s, want %s", a.Hash, es09AmendmentHash)
	}
}

// es09AmendmentHash is the SHA-256 of testdata/amendment-es09.json, whose bytes Python's
// json.dumps(sort_keys=True, separators=(",", ":"), ensure_ascii=False) reproduces.
const es09AmendmentHash = "5b5c4b57b378da430011461b03ff47dfced449d705e6aaa596c26f9869c34463"
