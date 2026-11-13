//go:build integration

package bootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TRD's downstream reads (S5 plan unit A2, prerequisite of S5-03/04/05/12/13): OrderCommercialSnapshot
// v1 (Context Map R32 TRD→STL), SalesOrderRef v1 and SalesContractDocumentRef v1 (R46 TRD→CNX) as
// one lookup, ResolveOrderSnapshots, and the Terms block ActiveOrderRef v1 gains (R27 TRD→FUL), on the
// order world: customer.1 r2 with its buyer address hq, FCA (Incoterms 2020) at DEHAM, payment terms
// "payment" (100 % before shipment), 120 PCS of sku.1 at 8.00 USD, 960.00 USD without tax, compared
// in USD (policy.1). Python has none of this; the tests cite the design IDs.

// snapshots resolves order ids through OrderSnapshotLookups as ctx.
func (q *trdWorld) snapshots(ctx context.Context, ids ...string) map[string]trdapi.OrderSnapshotFact {
	q.t.Helper()
	facts, err := q.c.TRD.ResolveOrderSnapshots(ctx, ids)
	q.must(err)
	return facts
}

// fixtureTerms are the order world's terms as an order of the fixture quote binds them: FCA 2020
// at DEHAM, the buyer address hq, the payment terms paid 100 % before shipment (the plan's one
// stage, 960.00 USD), nothing on credit.
func (q *trdWorld) fixtureTerms() trdapi.OrderTerms {
	return trdapi.OrderTerms{
		TradeTerms: trdapi.TradeTerms{Ref: tref(q.trade), Incoterm: "FCA", Edition: polapi.IncotermsEdition2020, NamedPlace: dehamPlace},
		Addresses: []trdapi.OrderAddress{{Role: ptyapi.AddressBuyer, PartyID: "customer.1", PartyRevision: 2, AddressID: "hq",
			Address: "合成买方地址\n上海"}},
		PaymentTerms:       tref(q.payment),
		Stages:             []trdapi.PaymentStage{{StageID: "all", Trigger: trdapi.TriggerBeforeShipment, Percent: "100", AmountMinor: "96000"}},
		CreditPortionMinor: "0"}
}

// R32 (草稿与生效订单), R46, R27 and S3-D153: the lookup answers the draft order of quote.1 and the
// active order of quote.2 at revision 1 — binding, state, source, customer, currency, the sealed
// terms with the buyer address and the payment plan (nothing on credit), the delivery, lines and
// totals, no documents yet, and the analysis rate (USD compared in USD: no rate) — and leaves an
// unknown order out. ActiveOrderRef v1 carries the same terms and lines. A cancelled draft stays
// readable with its end named. An active grant without trd.read suffices for the lookup, while the
// audited read order.snapshot.get needs trd.read and answers what the lookup answers; an own-scope
// reader sees neither order, a caller without a grant is refused, malformed IDs are SCHEMA_INVALID.
func TestOrderSnapshotsServeDraftsAndActiveOrders(t *testing.T) {
	q := newOrderWorld(t)
	_, draft := q.prepared("quote.1")
	active := q.activeOrder("quote.2")
	facts := q.snapshots(q.author, draft.ObjectID, active.ObjectID, "order.none")
	if len(facts) != 2 {
		t.Fatalf("the lookup %+v", facts)
	}

	v := q.getOrder(draft.ObjectID)
	d := facts[draft.ObjectID]
	if d.Check("/x") != nil || d.Status != trdapi.OrderDraft || d.Ended != "" || d.RowVersion != v.RowVersion ||
		d.Binding != (trdapi.OrderBinding{OrderID: draft.ObjectID, OrderRevision: 1, OrderHash: v.OrderHash}) || d.QuoteID != "quote.1" ||
		d.BusinessRevision != v.BusinessRevision || d.BusinessHash != v.BusinessHash || d.AcceptanceID != v.AcceptanceID {
		t.Fatalf("the draft's head %+v", d)
	}
	if d.CustomerRef != (trdapi.PartyRef{PartyID: "customer.1", Revision: 2}) || d.CurrencyRef != tref(q.usd) ||
		!reflect.DeepEqual(d.Delivery, handover) || d.NetMinor != "96000" || d.TaxMinor != "0" || d.PayableMinor != "96000" || d.Charges != nil ||
		d.DocumentRefs != nil || d.AnalysisFX != (trdapi.AnalysisFX{OrderCurrency: "USD", ComparisonCurrencyRef: tref(q.usd)}) {
		t.Fatalf("the draft's facts %+v", d)
	}
	if !reflect.DeepEqual(d.OrderTerms, q.fixtureTerms()) {
		t.Fatalf("the draft's terms\n%+v\nwant\n%+v", d.OrderTerms, q.fixtureTerms())
	}
	if l := d.Lines; len(l) != 1 || l[0].LineID != "L1" || l[0].Sku.SkuID != "sku.1" || l[0].UnitRef != tref(q.pcs) || l[0].NetMinor != "96000" ||
		l[0].PayableMinor != "96000" || l[0].CostUnit != "PCS" {
		t.Fatalf("the draft's lines %+v", l)
	}
	if got := d.Address(ptyapi.AddressBuyer); len(got) != 1 || got[0].AddressID != "hq" {
		t.Fatalf("the buyer %+v", got)
	}

	a := facts[active.ObjectID]
	if a.Check("/x") != nil || a.Status != trdapi.OrderActive || a.QuoteID != "quote.2" || a.Binding.OrderHash != active.OrderHash ||
		!reflect.DeepEqual(a.OrderTerms, q.fixtureTerms()) {
		t.Fatalf("the active order %+v", a)
	}
	refs, err := q.c.TRD.ResolveActiveOrders(q.author, []string{draft.ObjectID, active.ObjectID})
	q.must(err)
	for id, f := range facts {
		// S5R-D33: the line's costing cost (the 660.00 USD snapshot's L1) is OrderCommercialSnapshot v1's,
		// not ActiveOrderRef v1's; the lines are otherwise the same.
		lines := slices.Clone(f.Lines)
		for i := range lines {
			if lines[i].CostMinor != "66000" {
				t.Errorf("the costing cost of %s %s: %q", id, lines[i].LineID, lines[i].CostMinor)
			}
			lines[i].CostMinor = ""
		}
		if r := refs[id]; !reflect.DeepEqual(r.Terms, f.OrderTerms) || !reflect.DeepEqual(r.Lines, lines) || r.Binding != f.Binding {
			t.Errorf("ActiveOrderRef v1 of %s: %+v", id, r)
		}
	}
	if refs[draft.ObjectID].Code != trdapi.CodeOrderNotActive || refs[active.ObjectID].Check("/x") != nil {
		t.Fatalf("ActiveOrderRef v1 verdicts %+v", refs)
	}

	// The audited read answers what the lookup answers, with trd.read.
	got, err := q.c.TRD.GetOrderSnapshot(q.author, draft.ObjectID)
	q.must(err)
	if !reflect.DeepEqual(got, d) {
		t.Fatalf("order.snapshot.get\n%+v\nlookup\n%+v", got, d)
	}
	if audit, err := q.c.TRD.Audit(q.author); err != nil || !hasAudit(audit, "order.snapshot.get") {
		t.Fatalf("audit %v", err)
	}
	_, err = q.c.TRD.GetOrderSnapshot(q.author, "order.none")
	q.want(err, kernel.CodeNotFound)

	// S3-D153: an active grant, the caller's data scope.
	bare := q.regrant("lookup.only", "company", []string{})
	if found := q.snapshots(bare, draft.ObjectID, active.ObjectID); len(found) != 2 || !reflect.DeepEqual(found[draft.ObjectID], d) {
		t.Fatalf("an active grant without permissions: %+v", found)
	}
	_, err = q.c.TRD.GetOrderSnapshot(bare, draft.ObjectID)
	q.want(err, kernel.CodePermissionDenied)
	own := q.regrant("outsider", "own", allPermissions)
	if found := q.snapshots(own, draft.ObjectID, active.ObjectID); len(found) != 0 {
		t.Fatalf("an own-scope lookup %+v", found)
	}
	_, err = q.c.TRD.GetOrderSnapshot(own, active.ObjectID)
	q.want(err, kernel.CodeNotFound)
	nobody, err := identity.WithPrincipal(context.Background(), identity.Principal{Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "nobody"},
		Scope: q.scope("company.A")})
	q.must(err)
	_, err = q.c.TRD.ResolveOrderSnapshots(nobody, []string{draft.ObjectID})
	q.want(err, kernel.CodePermissionDenied)
	_, err = q.c.TRD.ResolveOrderSnapshots(q.author, []string{"bad id"})
	if code(err) != kernel.CodeSchemaInvalid || path(err) != "/order_ids/0" {
		t.Fatalf("a malformed ID: %v", err)
	}
	_, err = q.c.TRD.ResolveOrderSnapshots(q.author, make([]string, trdapi.MaxOrderRefs+1))
	q.want(err, kernel.CodeSchemaInvalid)

	// A cancelled draft: its state and end are the answer, the sealed facts stay.
	_, err = q.c.TRD.CancelOrder(q.author, trdapi.CancelOrder{IdempotencyKey: q.key(), OrderID: draft.ObjectID, ExpectedVersion: v.RowVersion,
		Reason: "客户撤回", Evidence: q.proof})
	q.must(err)
	c := q.snapshots(q.author, draft.ObjectID)[draft.ObjectID]
	if c.Check("/x") != nil || c.Status != trdapi.OrderCancelled || c.Ended != trdapi.CommandCancelOrder || c.RowVersion != v.RowVersion+1 ||
		!reflect.DeepEqual(c.OrderTerms, d.OrderTerms) {
		t.Fatalf("the cancelled draft %+v", c)
	}
}

// The facts are what TRD sealed, never POL's current revision (S5 plan unit A2), and they follow the
// order's current revision (S3-D124; TRD-INV-022's positive case). POL publishes revision 2 of the
// trade terms (CPT) and of the payment terms (30 % on order confirmation, 70 % 30 days after
// shipment): the active order still states FCA and its one stage paid before shipment. The customer
// moves to those payment terms by an amendment: revision 2's plan is 288.00 + 672.00, of which
// 672.00 is on credit; the trade terms stay revision 1's (FCA). ActiveOrderRef v1 states the same.
// Before it is applied the approved amendment's revision 2 is stated as proposed (S5 D-9, B4), and
// apply needs a credit reservation of it (CREDIT_RESERVATION_REQUIRED).
func TestOrderSnapshotsAreWhatTRDSealed(t *testing.T) {
	q := newOrderWorld(t)
	o := q.activeOrder("quote.1")
	v1 := q.getOrder(o.ObjectID)
	before := q.snapshots(q.author, o.ObjectID)[o.ObjectID]

	q.configure(q.author, q.reviewer, polapi.KindTradeTerms, q.trade.ConfigID, q.rootVersion(polapi.KindTradeTerms, q.trade.ConfigID), q.win.from,
		time.Time{}, polapi.ConfigData{TradeTerms: &polapi.TradeTerms{Incoterm: "CPT", Edition: polapi.IncotermsEdition2020,
			NamedPlace: "TEST NAMED PLACE", ResponsibilityNote: "synthetic evidence only",
			Delivery: polapi.Delivery{Kind: polapi.DeliveryFixedDate, Date: "2027-01-31"}}}, true)
	pay2 := q.configure(q.author, q.reviewer, polapi.KindPaymentTerms, q.payment.ConfigID, q.rootVersion(polapi.KindPaymentTerms,
		q.payment.ConfigID), q.win.from, time.Time{}, polapi.ConfigData{PaymentTerms: &polapi.PaymentTerms{
		Name: "30% on order confirmation, 70% 30 days after shipment", Stages: []polapi.PaymentStage{
			{StageID: "P1", Percent: "30", Trigger: polapi.TriggerOrderConfirmed},
			{StageID: "P2", Percent: "70", Trigger: polapi.TriggerAfterShipment, Days: 30}}}}, true)
	if pay2.Revision != 2 {
		t.Fatalf("payment terms %+v", pay2)
	}
	if after := q.snapshots(q.author, o.ObjectID)[o.ObjectID]; !reflect.DeepEqual(after, before) || after.TradeTerms.Incoterm != "FCA" ||
		len(after.Stages) != 1 || after.PaymentTerms.Revision != 1 {
		t.Fatalf("after POL moved on\n%+v\nbefore\n%+v", after, before)
	}

	cmd := q.amendCmd(o.ObjectID, "amendment.pay", 0, 1, v1.OrderHash, "120")
	cmd.Terms.PaymentTermsRef = tref(pay2)
	p, err := q.c.TRD.PrepareAmendment(q.author, cmd)
	q.must(err)
	s, err := q.c.TRD.SubmitAmendment(q.author, trdapi.SubmitAmendment{IdempotencyKey: q.key(), OrderID: o.ObjectID, AmendmentID: "amendment.pay",
		ExpectedVersion: p.RowVersion})
	q.must(err)
	dec, err := q.c.TRD.DecideAmendment(q.reviewer, trdapi.DecideAmendment{IdempotencyKey: q.key(), OrderID: o.ObjectID,
		AmendmentID: "amendment.pay", ExpectedVersion: s.RowVersion, AmendmentHash: s.AmendmentHash, Decision: trdapi.DecisionApprove,
		Reason: "客户书面确认新付款条件"})
	q.must(err)
	// S5 D-9 (unit B4): the approved amendment proposes revision 2 with 672.00 on credit; apply
	// refuses until STL holds a reservation of that revision, and then produces exactly it.
	want := []trdapi.PaymentStage{
		{StageID: "P1", Trigger: trdapi.TriggerOrderConfirmed, Percent: "30", AmountMinor: "28800"},
		{StageID: "P2", Trigger: trdapi.TriggerAfterShipment, Days: 30, Percent: "70", AmountMinor: "67200", OnCredit: true}}
	proposed := q.proposed(o.ObjectID, "amendment.pay")
	if proposed.Binding.OrderRevision != 2 || proposed.AmendmentHash != s.AmendmentHash || proposed.AmendmentStatus != trdapi.AmendmentApproved ||
		!reflect.DeepEqual(proposed.Stages, want) || proposed.CreditPortionMinor != "67200" || proposed.PayableMinor != "96000" {
		t.Fatalf("the proposed revision %+v", proposed)
	}
	_, err = q.c.TRD.ApplyAmendment(q.reviewer, q.applyCmd(o.ObjectID, "amendment.pay", dec.RowVersion))
	if code(err) != stlapi.CodeCreditReservationRequired || path(err) != "/args/amendment_id" {
		t.Fatalf("apply without a reservation: %v", err)
	}
	q.reserveCredit(proposed.Binding)
	_, err = q.c.TRD.ApplyAmendment(q.reviewer, q.applyCmd(o.ObjectID, "amendment.pay", dec.RowVersion))
	q.must(err)

	v2 := q.getOrder(o.ObjectID)
	f := q.snapshots(q.author, o.ObjectID)[o.ObjectID]
	if f.Binding != proposed.Binding || len(f.Proposed) != 0 {
		t.Fatalf("revision 2 %+v is not the one proposed %+v", f.Binding, proposed.Binding)
	}
	if f.Check("/x") != nil || f.Binding != (trdapi.OrderBinding{OrderID: o.ObjectID, OrderRevision: 2, OrderHash: v2.OrderHash}) ||
		f.RowVersion != v2.RowVersion || f.PaymentTerms != tref(pay2) || !reflect.DeepEqual(f.Stages, want) || f.CreditPortionMinor != "67200" ||
		f.TradeTerms != before.TradeTerms || f.PayableMinor != "96000" || !reflect.DeepEqual(f.Addresses, before.Addresses) ||
		f.AnalysisFX != before.AnalysisFX || f.QuoteID != before.QuoteID || f.BusinessHash != before.BusinessHash {
		t.Fatalf("revision 2\n%+v", f)
	}
	r, err := q.c.TRD.ResolveActiveOrders(q.author, []string{o.ObjectID})
	q.must(err)
	if got := r[o.ObjectID]; got.Check("/x") != nil || !reflect.DeepEqual(got.Terms, f.OrderTerms) || got.Binding != f.Binding {
		t.Fatalf("ActiveOrderRef v1 at revision 2: %+v", got)
	}
}

// R46 SalesContractDocumentRef v1 (TRD-INV-020, SIM-02 step 6): the draft order's PI with the
// customer's countersigned copy is a document reference of revision 1, current; after the
// amendment to 100 PCS the PI stays, no longer current, and revision 2's S/C is the current one.
// References come oldest revision first; the plan follows the revision (800.00 before shipment).
func TestOrderSnapshotsReferenceTheOrdersDocuments(t *testing.T) {
	d := newDocWorld(t, true)
	ed, rv := d.author, d.reviewer
	o := d.legalDraftOrder("quote.d1")
	id := o.ObjectID
	v1 := d.getOrder(id)
	pi, err := d.c.TRD.GenerateOrderDocument(ed, d.generateCmd(id, 1, v1.OrderHash, trdapi.DocumentProformaInvoice, d.pi, d.piSeq))
	d.must(err)
	d.scan("file.pi.back", "countersigned "+pi.Number)
	_, err = d.c.TRD.RecordSignedOrderDocument(ed, d.signCmd(id, 1, trdapi.DocumentProformaInvoice, trdapi.SignatureCountersigned, "file.pi.back"))
	d.must(err)
	piRef := trdapi.OrderDocumentRef{DocumentKind: trdapi.DocumentProformaInvoice, Number: pi.Number, OrderRevision: 1, OrderHash: v1.OrderHash,
		DocumentSHA256: pi.DocumentSHA256, Current: true, Signatures: []trdapi.SignedCopyRef{{SignatureKind: trdapi.SignatureCountersigned,
			FileID: "file.pi.back", FileSHA256: sha256Hex("countersigned " + pi.Number)}}}
	if f := d.snapshots(ed, id)[id]; f.Status != trdapi.OrderDraft || !reflect.DeepEqual(f.DocumentRefs, []trdapi.OrderDocumentRef{piRef}) {
		t.Fatalf("the draft's documents %+v", f.DocumentRefs)
	}

	rvw := d.reviewed(o, "review.quote.d1")
	_, err = d.c.TRD.ActivateOrder(ed, d.activateOrderCmd(id, rvw.RowVersion, "review.quote.d1"))
	d.must(err)
	a := d.approvedAmendment(id, "amendment.1", 1, v1.OrderHash, "100")
	_, err = d.c.TRD.ApplyAmendment(rv, d.applyCmd(id, "amendment.1", a.RowVersion))
	d.must(err)
	v2 := d.getOrder(id)
	sc, err := d.c.TRD.GenerateOrderDocument(ed, d.generateCmd(id, 2, v2.OrderHash, trdapi.DocumentSalesContract, d.sc, d.scSeq))
	d.must(err)

	f := d.snapshots(ed, id)[id]
	piRef.Current = false
	scRef := trdapi.OrderDocumentRef{DocumentKind: trdapi.DocumentSalesContract, Number: sc.Number, OrderRevision: 2, OrderHash: v2.OrderHash,
		DocumentSHA256: sc.DocumentSHA256, Current: true}
	if f.Check("/x") != nil || f.Binding.OrderRevision != 2 || !reflect.DeepEqual(f.DocumentRefs, []trdapi.OrderDocumentRef{piRef, scRef}) ||
		len(f.Stages) != 1 || f.Stages[0].AmountMinor != "80000" || f.PayableMinor != "80000" || f.Lines[0].NetMinor != "80000" {
		t.Fatalf("revision 2's documents and plan %+v", f)
	}
}

// The audited read over HTTP (GET /api/brant/v1/trd/orders/{id}/snapshot, trd.order.snapshot.get):
// the fact in the transport's snake_case form, the embedded terms inline; an unknown order is 404.
func TestOrderSnapshotOverHTTP(t *testing.T) {
	q := newOrderWorld(t)
	_, o := q.prepared("quote.1")
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ed := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "editor", "human")}
	status, out := ed.do("GET", "/api/brant/v1/trd/orders/"+o.ObjectID+"/snapshot", nil)
	binding, _ := out["binding"].(map[string]any)
	trade, _ := out["trade_terms"].(map[string]any)
	stages, _ := out["stages"].([]any)
	addresses, _ := out["addresses"].([]any)
	if status != http.StatusOK || out["ok"] != true || binding["order_id"] != o.ObjectID || binding["order_revision"] != float64(1) ||
		binding["order_hash"] != o.OrderHash || out["status"] != trdapi.OrderDraft || out["credit_portion_minor"] != "0" ||
		trade["incoterm"] != "FCA" || len(stages) != 1 || len(addresses) != 1 || out["payable_minor"] != "96000" {
		t.Fatalf("GET snapshot: %d %v", status, out)
	}
	stage, _ := stages[0].(map[string]any)
	address, _ := addresses[0].(map[string]any)
	payment, _ := out["payment_terms"].(map[string]any)
	if stage["trigger"] != trdapi.TriggerBeforeShipment || stage["amount_minor"] != "96000" || stage["on_credit"] != false ||
		address["role"] != ptyapi.AddressBuyer || payment["id"] != q.payment.ConfigID || !strings.Contains(address["address"].(string), "上海") {
		t.Fatalf("GET snapshot members: %v %v %v", stage, address, payment)
	}
	if status, out = ed.do("GET", "/api/brant/v1/trd/orders/order.none/snapshot", nil); status != http.StatusNotFound || out["ok"] != false {
		t.Fatalf("an unknown order: %d %v", status, out)
	}
}
