//go:build integration

package bootstrap_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// WBS S5R-03 on PostgreSQL 16: TRD's revision reads and sealed terms (simulated decisions S5R-D35,
// S5R-D40, S5R-D33, E0, 2026-11-13; tactical model TRD-AGG-SalesOrder notes, UL TRD-SalesOrder;
// Context Map R27, R32, R46 OrderRevisionRef v1). A new order revision — revision 1 of order.prepare,
// a later one of order.amendment.apply — seals the Incoterm and edition of its trade terms, its
// customer addresses' countries and the notify party it binds; OrderRevisionRef v1 reads any
// committed revision as it was, with the lines' prices and costing cost; a revision written before
// S5R keeps its bytes and order hash, and its Incoterm and countries are derived from the bound POL
// and PTY revisions (Derived). Python V0.50 has none of it: the tests cite the design.

// notifyParty confirms party notify.1 with a notify address and a consignee address, both in the
// Netherlands, and returns its notify address (AddressRoleRef v1 at revision 1).
func (q *trdWorld) notifyParty() trdapi.AddressRoleRef {
	q.t.Helper()
	_, err := q.c.PTY.SaveParty(q.author, ptyapi.SaveParty{IdempotencyKey: q.key(), PartyID: "notify.1", Name: "notify.1",
		Roles: []string{ptyapi.RoleCustomer}, Addresses: []ptyapi.Address{
			{AddressID: "rotterdam", Role: ptyapi.AddressNotify, Address: "合成通知方地址\n鹿特丹", Country: "NL"},
			{AddressID: "dock", Role: ptyapi.AddressConsignee, Address: "合成收货地址", Country: "NL"}}})
	q.must(err)
	_, err = q.c.PTY.ConfirmParty(q.author, ptyapi.ConfirmParty{IdempotencyKey: q.key(), PartyID: "notify.1", ExpectedVersion: 1,
		EvidenceIDs: []string{"evidence"}})
	q.must(err)
	return trdapi.AddressRoleRef{PartyID: "notify.1", PartyRevision: 1, AddressID: "rotterdam"}
}

// locatedData confirms customer.1 in revision 3 with its buyer address hq in Germany (the billing
// address ap states no country), publishes trading profile revision 2 on it, and returns the
// fixture's quote data on that revision, binding both addresses.
func (q *trdWorld) locatedData() trdapi.QuoteData {
	q.t.Helper()
	ed, rv := q.author, q.reviewer
	v, err := q.c.PTY.GetParty(ed, "customer.1")
	q.must(err)
	_, err = q.c.PTY.ReviseParty(ed, ptyapi.ReviseParty{IdempotencyKey: q.key(), PartyID: "customer.1", ExpectedVersion: v.RowVersion,
		Reason: "补录地址所在国"})
	q.must(err)
	_, err = q.c.PTY.SaveParty(ed, ptyapi.SaveParty{IdempotencyKey: q.key(), PartyID: "customer.1", ExpectedVersion: v.RowVersion + 1,
		Name: "customer.1", Roles: []string{ptyapi.RoleCustomer}, Addresses: []ptyapi.Address{
			{AddressID: "hq", Role: ptyapi.AddressBuyer, Address: "合成买方地址\n汉堡", Country: "DE"},
			{AddressID: "ap", Role: ptyapi.AddressBilling, Address: "合成账单地址"}}})
	q.must(err)
	_, err = q.c.PTY.ConfirmParty(ed, ptyapi.ConfirmParty{IdempotencyKey: q.key(), PartyID: "customer.1", ExpectedVersion: v.RowVersion + 2,
		EvidenceIDs: []string{"evidence"}})
	q.must(err)
	p, err := q.c.PTY.GetTradingProfile(ed, "customer.1")
	q.must(err)
	saved, err := q.saveProfile(ed, "customer.1", p.RootVersion, 3, terms(q.payment), terms(q.trade),
		ptyapi.PinnedContact{ContactID: "contact.1", RowVersion: 2})
	q.must(err)
	_, err = q.publishProfile(rv, "customer.1", saved.RowVersion, 2)
	q.must(err)
	// A new party revision is screened anew (PartyScreeningGuard v1, TRD-INV-021).
	sv, err := q.c.PTY.GetScreening(ed, "customer.1")
	q.must(err)
	_, err = q.c.PTY.RecordScreening(ed, ptyapi.RecordScreening{IdempotencyKey: q.key(), PartyID: "customer.1", ExpectedVersion: sv.RowVersion,
		RegulationID: "screening", RegulationRevision: 1})
	q.must(err)
	d := q.data()
	d.CustomerRef = &trdapi.PartyRef{PartyID: "customer.1", Revision: 3}
	d.TradingProfileRef = &trdapi.TradingProfileRef{PartyID: "customer.1", Revision: 2}
	d.CustomerAddressRefs = []trdapi.AddressRoleRef{{PartyID: "customer.1", PartyRevision: 3, AddressID: "hq"},
		{PartyID: "customer.1", PartyRevision: 3, AddressID: "ap"}}
	return d
}

// acceptedOn drafts quote id with data d, freezes it, has it approved and activated, and the
// customer's full acceptance verified as "acceptance.<id>".
func (q *trdWorld) acceptedOn(id string, d trdapi.QuoteData) trdapi.AcceptanceReceipt {
	q.t.Helper()
	_, err := q.saveQuote(q.author, id, 0, d)
	q.must(err)
	f, err := q.freezeQuote(q.author, id, 1, 1)
	q.must(err)
	_, err = q.submit(q.author, id, f.RowVersion, 1, "review."+id)
	q.must(err)
	dec, err := q.c.TRD.DecideReview(q.reviewer, q.approval(id, f.RowVersion+1, "review."+id, f.BusinessHash, "approval."+id))
	q.must(err)
	act, err := q.c.TRD.Activate(q.author, q.activateCmd(id, dec.RowVersion, 1, "approval."+id))
	q.must(err)
	_, err = q.c.TRD.RecordResponse(q.author, q.full("response."+id, id, 1, act.BusinessHash))
	q.must(err)
	verify := q.verification("response."+id, "acceptance."+id, 1, act.BusinessHash)
	verify.CustomerRef = *d.CustomerRef
	v, err := q.c.TRD.VerifyResponse(q.reviewer, verify)
	q.must(err)
	return v
}

// revisions resolves OrderRevisionRef v1 keys as ctx.
func (q *trdWorld) revisions(ctx context.Context, keys ...trdapi.OrderRevisionKey) map[trdapi.OrderRevisionKey]trdapi.OrderRevisionFact {
	q.t.Helper()
	got, err := q.c.TRD.ResolveOrderRevisions(ctx, keys)
	q.must(err)
	return got
}

// incoterm is what POL's trade terms revision of the world states.
func (q *trdWorld) incoterm() (string, string) {
	q.t.Helper()
	got, err := q.c.POL.ResolveConfigs(q.author, []polapi.ConfigRef{q.trade}, time.Now())
	q.must(err)
	t := got[q.trade].Data.TradeTerms
	if t == nil || t.Incoterm == "" {
		q.t.Fatalf("the world's trade terms state no Incoterm: %+v", got)
	}
	return t.Incoterm, t.Edition
}

// applied has the author prepare amendment cmd, submit it, the reviewer approve it, credit be
// reserved for what it proposes, and the author apply it.
func (q *trdWorld) applied(cmd trdapi.PrepareAmendment) trdapi.AmendmentReceipt {
	q.t.Helper()
	p, err := q.c.TRD.PrepareAmendment(q.author, cmd)
	q.must(err)
	d := q.decidedAmendment(cmd.OrderID, cmd.AmendmentID, p.RowVersion)
	q.readyToApply(cmd.OrderID, cmd.AmendmentID)
	a, err := q.c.TRD.ApplyAmendment(q.author, q.applyCmd(cmd.OrderID, cmd.AmendmentID, d.RowVersion))
	q.must(err)
	return a
}

// decidedAmendment submits prepared amendment id of order at version and has the reviewer approve
// it.
func (q *trdWorld) decidedAmendment(order, id string, version int64) trdapi.AmendmentReceipt {
	q.t.Helper()
	s, err := q.c.TRD.SubmitAmendment(q.author, trdapi.SubmitAmendment{IdempotencyKey: q.key(), OrderID: order, AmendmentID: id,
		ExpectedVersion: version})
	q.must(err)
	d, err := q.c.TRD.DecideAmendment(q.reviewer, trdapi.DecideAmendment{IdempotencyKey: q.key(), OrderID: order, AmendmentID: id,
		ExpectedVersion: s.RowVersion, AmendmentHash: s.AmendmentHash, Decision: trdapi.DecisionApprove, Reason: "客户书面确认"})
	q.must(err)
	return d
}

// S5R-D35, S5R-D40, S5R-D33 (TRD-AGG-SalesOrder "新修订以 omitempty 字段封存 Incoterm 与地址国别 … 订单可选绑定通知方
// … 订单事实给出行的封存核价成本 CostMinor"; register test "修订 1 的出运在修订 2 生效后，单证仍打印修订 1 的条款"):
//
//   - order.prepare binds notify.1's notify address; another role is INVALID_KNOWN_VALUE, an unknown
//     address REFERENCE_INVALID, both at /args/notify_party_ref. Revision 1 seals the world's Incoterm,
//     hq in DE, ap with no country, and the notify party in NL.
//   - OrderRevisionRef v1, OrderCommercialSnapshot v1 and ActiveOrderRef v1 show them; the snapshot's
//     and the revision's lines carry the costing cost of the 660.00 USD snapshot (66000), ActiveOrderRef
//     none; over HTTP the snapshot read names the members notify_party, country, cost_minor and
//     price_basis_quantity.
//   - An amendment to 100 PCS that drops the notify party yields revision 2: sealed, no notify party,
//     the cost 66000 × 100 ÷ 120 = 55000; revision 1 still reads as it was (120 PCS, the notify party).
//   - An amendment binding the notify party again is approved while its address is current; once
//     notify.1 is revised, apply refuses it (REFERENCE_STALE at /proposal/terms/notify_party_ref).
//   - Revisions the order does not have, and orders of other companies' callers, are absent.
func TestS5RD35D40NewRevisionsSealTheirTermsAndEachRevisionReadsAsItWas(t *testing.T) {
	q := newOrderWorld(t)
	notify := q.notifyParty()
	a := q.acceptedOn("quote.1", q.locatedData())
	incoterm, edition := q.incoterm()
	const id = "order.quote.1"

	cmd := q.prepareCmd(id, a)
	cmd.NotifyPartyRef = &trdapi.AddressRoleRef{PartyID: "notify.1", PartyRevision: 1, AddressID: "dock"}
	_, err := q.c.TRD.PrepareOrder(q.author, cmd)
	wantRefused(t, "a consignee address as the notify party", err, kernel.CodeInvalidKnownValue, "/args/notify_party_ref")
	cmd = q.prepareCmd(id, a)
	cmd.NotifyPartyRef = &trdapi.AddressRoleRef{PartyID: "notify.1", PartyRevision: 1, AddressID: "nowhere"}
	_, err = q.c.TRD.PrepareOrder(q.author, cmd)
	wantRefused(t, "an unknown address", err, kernel.CodeReferenceInvalid, "/args/notify_party_ref")
	cmd = q.prepareCmd(id, a)
	cmd.NotifyPartyRef = &notify
	o, err := q.c.TRD.PrepareOrder(q.author, cmd)
	q.must(err)
	stored := q.getOrder(id).Snapshot
	if !strings.Contains(stored, `"seal":{"addresses":[{"address_id":"ap"},{"address_id":"hq","country":"DE"}],"incoterm":"`+incoterm+`"`) ||
		!strings.Contains(stored, `"notify_party":{"address":"合成通知方地址\n鹿特丹","country":"NL"}`) ||
		!strings.Contains(stored, `"notify_party_ref":{"address_id":"rotterdam","party_id":"notify.1","party_revision":1}`) {
		t.Fatalf("the snapshot seals the terms: %s", stored)
	}

	r1 := trdapi.OrderRevisionKey{OrderID: id, OrderRevision: 1}
	checkRevisionOne := func(f trdapi.OrderRevisionFact, current bool) {
		t.Helper()
		l, ok := f.Line("L1")
		if f.Code != "" || f.Derived || f.Binding != (trdapi.OrderBinding{OrderID: id, OrderRevision: 1, OrderHash: o.OrderHash}) ||
			f.Current != current || f.Previous != nil || f.AmendmentID != "" || f.TradeTerms.Incoterm != incoterm || f.TradeTerms.Edition != edition ||
			f.TradeTerms.Ref != tref(q.trade) || len(f.Addresses) != 2 || f.NotifyParty == nil || !ok || l.Quantity != "120" ||
			l.UnitPrice != "8" || l.PriceBasisQuantity != "1" || l.DiscountRatio != "0" || l.NetMinor != "96000" || l.CostMinor != "66000" ||
			f.NetMinor != "96000" || f.CustomerRef != (trdapi.PartyRef{PartyID: "customer.1", Revision: 3}) {
			t.Fatalf("revision 1 %+v", f)
		}
		for _, x := range f.Addresses {
			if want := map[string]string{"hq": "DE", "ap": ""}[x.AddressID]; x.Country != want || x.PartyRevision != 3 {
				t.Fatalf("address %+v", x)
			}
		}
		if *f.NotifyParty != (trdapi.OrderAddress{Role: ptyapi.AddressNotify, PartyID: "notify.1", PartyRevision: 1, AddressID: "rotterdam",
			Address: "合成通知方地址\n鹿特丹", Country: "NL"}) {
			t.Fatalf("notify party %+v", f.NotifyParty)
		}
	}
	checkRevisionOne(q.revisions(q.author, r1)[r1], true)
	snap := q.snapshots(q.author, id)[id]
	if snap.NotifyParty == nil || snap.NotifyParty.Country != "NL" || snap.Address(ptyapi.AddressBuyer)[0].Country != "DE" ||
		snap.TradeTerms.Incoterm != incoterm || snap.Lines[0].CostMinor != "66000" || snap.Lines[0].PriceBasisQuantity != "1" {
		t.Fatalf("OrderCommercialSnapshot v1 %+v", snap)
	}

	// Over HTTP: the snapshot read's members.
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ed := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "editor", "human")}
	status, body := ed.do("GET", "/api/brant/v1/trd/orders/"+id+"/snapshot", nil)
	np, _ := body["notify_party"].(map[string]any)
	lines, _ := body["lines"].([]any)
	if status != http.StatusOK || np["country"] != "NL" || np["role"] != "notify" || len(lines) != 1 ||
		lines[0].(map[string]any)["cost_minor"] != "66000" || lines[0].(map[string]any)["price_basis_quantity"] != "1" {
		t.Fatalf("order.snapshot.get over HTTP %d %v", status, body)
	}

	// CNX's conclusion on the order: hq's country is PTY's now (DE), so none is entered for it.
	d := q.approvedReview(o, "review.quote.1")
	cw := q.compliance()
	ev := cw.evaluateCmd(id, 1, 0, syntheticClear)
	ev.AddressCountries = nil
	e, err := cw.cnx.Evaluate(cw.compliance, ev)
	q.must(err)
	_, err = cw.cnx.Decide(cw.compliance, cw.decideCmd(e, cnxapi.ConclusionClear, false))
	q.must(err)
	act, err := q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(id, d.RowVersion, "review.quote.1"))
	q.must(err)
	active, err := q.c.TRD.ResolveActiveOrders(q.author, []string{id})
	q.must(err)
	if f := active[id]; f.Code != "" || f.Terms.NotifyParty == nil || f.Lines[0].CostMinor != "" || f.Terms.Addresses[0].Country == "" &&
		f.Terms.Addresses[1].Country == "" {
		t.Fatalf("ActiveOrderRef v1 %+v", f)
	}

	// Revision 2: 100 PCS, the notify party dropped.
	a1 := q.amendCmd(id, "amendment.1", 0, 1, act.OrderHash, "100")
	q.applied(a1)
	r2 := trdapi.OrderRevisionKey{OrderID: id, OrderRevision: 2}
	got := q.revisions(q.author, r1, r2, trdapi.OrderRevisionKey{OrderID: id, OrderRevision: 3},
		trdapi.OrderRevisionKey{OrderID: "order.other", OrderRevision: 1})
	if len(got) != 2 {
		t.Fatalf("only the order's two revisions: %v", got)
	}
	checkRevisionOne(got[r1], false)
	f2 := got[r2]
	l2, _ := f2.Line("L1")
	if f2.Code != "" || f2.Derived || !f2.Current || f2.Previous == nil || *f2.Previous != got[r1].Binding || f2.AmendmentID != "amendment.1" ||
		f2.NotifyParty != nil || f2.TradeTerms.Incoterm != incoterm || l2.Quantity != "100" || l2.NetMinor != "80000" || l2.CostMinor != "55000" ||
		f2.Binding.OrderHash != q.getOrder(id).OrderHash {
		t.Fatalf("revision 2 %+v", f2)
	}
	for _, x := range f2.Addresses {
		if x.AddressID == "hq" && x.Country != "DE" {
			t.Fatalf("revision 2 seals the buyer's country: %+v", x)
		}
	}
	if doc := q.getOrder(id).CurrentRevision; !strings.Contains(doc, `"seal":{"addresses":[{"address_id":"ap"},{"address_id":"hq","country":"DE"}]`) ||
		strings.Contains(doc, "notify_party") {
		t.Fatalf("revision 2's document %s", doc)
	}

	// Binding the notify party again: approved while current, refused at apply once notify.1 is revised.
	b := q.orderBinding(id)
	a2 := q.amendCmd(id, "amendment.2", 0, b.OrderRevision, b.OrderHash, "100")
	a2.Terms.NotifyPartyRef = &notify
	p, err := q.c.TRD.PrepareAmendment(q.author, a2)
	q.must(err)
	dec := q.decidedAmendment(id, "amendment.2", p.RowVersion)
	pv, err := q.c.PTY.GetParty(q.author, "notify.1")
	q.must(err)
	_, err = q.c.PTY.ReviseParty(q.author, ptyapi.ReviseParty{IdempotencyKey: q.key(), PartyID: "notify.1", ExpectedVersion: pv.RowVersion,
		Reason: "通知方地址变更"})
	q.must(err)
	_, err = q.c.TRD.ApplyAmendment(q.author, q.applyCmd(id, "amendment.2", dec.RowVersion))
	wantRefused(t, "a notify party no longer current", err, trdapi.CodeReferenceStale, "/proposal/terms/notify_party_ref")
	if v := q.getOrder(id); v.OrderRevision != 2 {
		t.Fatalf("nothing applied: %d", v.OrderRevision)
	}
}

// canonicalJSON re-encodes v as TRD's canonical JSON (members in code point order, no whitespace,
// no HTML escaping, numbers as written).
func canonicalJSON(t *testing.T, v any) string {
	t.Helper()
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// writtenBeforeS5R rewrites the snapshot of draft order id as order.prepare wrote it before S5R —
// without seal and notify party — with its order hash, as the schema owner with the order's guard
// trigger off (the stored order of a company that prepared it before this change). It returns the
// new order hash.
func (q *trdWorld) writtenBeforeS5R(id string) string {
	q.t.Helper()
	stored := q.getOrder(id).Snapshot
	dec := json.NewDecoder(strings.NewReader(stored))
	dec.UseNumber()
	var m map[string]any
	q.must(dec.Decode(&m))
	if canonicalJSON(q.t, m) != stored {
		q.t.Fatal("the snapshot does not re-encode to its bytes")
	}
	delete(m, "seal")
	delete(m["terms"].(map[string]any), "notify_party_ref")
	legacy := canonicalJSON(q.t, m)
	sum := sha256.Sum256([]byte(legacy))
	hash := hex.EncodeToString(sum[:])
	ctx := context.Background()
	tx, err := pgtest.MigratorPool(q.t).Begin(ctx)
	q.must(err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true), set_config('app.company_id', 'company.A', true)`, q.tenant)
	q.must(err)
	_, err = tx.Exec(ctx, `ALTER TABLE brant_trd.sales_order DISABLE TRIGGER sales_order_guard`)
	q.must(err)
	_, err = tx.Exec(ctx, `UPDATE brant_trd.sales_order SET snapshot = $1, snapshot_hash = $2, order_hash = $2 WHERE tenant_id = $3 AND order_id = $4`, legacy, hash,
		q.tenant, id)
	q.must(err)
	_, err = tx.Exec(ctx, `ALTER TABLE brant_trd.sales_order ENABLE TRIGGER sales_order_guard`)
	q.must(err)
	q.must(tx.Commit(ctx))
	return hash
}

// S5R-D35 "旧修订哈希不变，旧修订从绑定的 POL、PTY 修订推导并标记 derived" (WBS S5R-03 acceptance): an order whose
// revision 1 was written before S5R — its snapshot without a seal — keeps its bytes and order hash;
// it activates as before; OrderRevisionRef v1 derives the Incoterm and edition from the trade terms
// revision it binds and the countries from the party revision it binds (customer.1 revision 2 states
// none), marked Derived; OrderCommercialSnapshot v1 reads the Incoterm from POL as in S5. Its first
// amendment writes a sealed revision 2 — revision 1 stays derived and unchanged.
func TestS5RD35ARevisionWrittenBeforeS5RKeepsItsHashAndIsDerived(t *testing.T) {
	q := newOrderWorld(t)
	incoterm, edition := q.incoterm()
	_, o := q.prepared("quote.1")
	id := o.ObjectID
	legacy := q.writtenBeforeS5R(id)
	if v := q.getOrder(id); v.OrderHash != legacy || strings.Contains(v.Snapshot, `"seal"`) {
		t.Fatalf("the order before S5R: %s", v.OrderHash)
	}
	r1 := trdapi.OrderRevisionKey{OrderID: id, OrderRevision: 1}
	f := q.revisions(q.author, r1)[r1]
	if f.Code != "" || !f.Derived || f.Binding.OrderHash != legacy || f.TradeTerms.Incoterm != incoterm || f.TradeTerms.Edition != edition ||
		len(f.Addresses) != 1 || f.Addresses[0].Country != "" || f.NotifyParty != nil || f.Lines[0].CostMinor != "66000" {
		t.Fatalf("revision 1 written before S5R %+v", f)
	}
	if snap := q.snapshots(q.author, id)[id]; snap.Code != "" || snap.TradeTerms.Incoterm != incoterm || snap.Binding.OrderHash != legacy {
		t.Fatalf("OrderCommercialSnapshot v1 %+v", snap)
	}
	o.OrderHash = legacy
	d := q.reviewed(o, "review.quote.1")
	act, err := q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(id, d.RowVersion, "review.quote.1"))
	q.must(err)
	if act.OrderHash != legacy {
		t.Fatalf("activated at %s", act.OrderHash)
	}
	q.applied(q.amendCmd(id, "amendment.1", 0, 1, legacy, "110"))
	r2 := trdapi.OrderRevisionKey{OrderID: id, OrderRevision: 2}
	got := q.revisions(q.author, r1, r2)
	if got[r1].Binding.OrderHash != legacy || !got[r1].Derived || got[r2].Derived || got[r2].Code != "" ||
		got[r2].TradeTerms.Incoterm != incoterm || got[r2].Lines[0].CostMinor != "60500" {
		t.Fatalf("revisions %+v", got)
	}
	if doc := q.getOrder(id).CurrentRevision; !strings.Contains(doc, `"seal":{"addresses":[{"address_id":"hq"}],"incoterm":"`+incoterm+`"`) {
		t.Fatalf("revision 2 of an order before S5R is sealed: %s", doc)
	}
}

// S5R-D05, TRD side (verified; R4 report "R3 must not subtract the disposition rows from the
// allocation again"; TRD-INV-019; WF-06 step 1): on TRD's real order with commitment.1 cancelled and
// 60 PCS delivered, an approved return of 40 leaves PUR's allocation row at 20 — TRD's cancel names
// 20, not 60 and not nothing, so it never subtracts a disposition again — and the amendment pre-fill
// lists the disposition apart as settled ("disposed of with approval"), the netted allocation as
// the open item.
func TestS5RD05TRDJudgesTheNettedAllocationAndPrefillsTheDisposition(t *testing.T) {
	w := newFulPurWorld(t)
	q := w.trdWorld
	id := w.order.ObjectID
	x := purDispose{w: w.scenWorld, m: func() *pur.Module { return w.c.PUR }}
	w.verified("delivery.1", "60", "60")
	cv, err := w.c.PUR.GetCommitment(w.author, "commitment.1")
	w.must(err)
	_, err = w.c.PUR.CancelCommitment(w.author, purapi.CancelCommitment{IdempotencyKey: w.key(), CommitmentID: "commitment.1",
		ExpectedVersion: cv.RowVersion, Reason: "合成：客户撤单，工厂停止生产", Evidence: purChangeEvidence})
	w.must(err)
	x.approved(w.author, w.reviewer, "disp.return", "commitment.1", purapi.DispositionReturnToSupplier, "40")
	_, err = q.endCancel(id)
	wantRefused(t, "cancel with 20 delivered goods not disposed of", err, trdapi.CodeDownstreamCommitmentOpen, "/args/order_id")
	if r := reasonOf(err); !strings.Contains(r, "demand_allocation L1 allocates 20 of line L1, which shipped 0") || strings.Contains(r, "disp.return") {
		t.Fatalf("the refusal names the netted allocation only: %s", r)
	}

	v := q.getOrder(id)
	cmd := q.amendCmd(id, "amendment.1", 0, v.OrderRevision, v.OrderHash, "100")
	cmd.Impact = []trdapi.ImpactAssessment{{Area: "materials", Status: trdapi.ImpactUnaffected}, {Area: "operations", Status: trdapi.ImpactUnaffected}}
	_, err = q.c.TRD.PrepareAmendment(q.author, cmd)
	q.must(err)
	var content struct {
		Impact []struct {
			Area, Status, Note string
		} `json:"impact"`
	}
	q.must(json.Unmarshal([]byte(q.getOrder(id).Amendments[0].Content), &content))
	for _, a := range content.Impact {
		if a.Area == "purchasing" && (a.Status != "open" || a.Note != "pur.downstream_commitments.v1: demand_allocation L1 20; "+
			"disposed of with approval: disp.return return_to_supplier 40 of line L1") {
			t.Fatalf("the purchasing pre-fill %+v", a)
		}
	}
}
