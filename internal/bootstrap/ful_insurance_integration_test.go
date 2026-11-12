//go:build integration

package bootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// WBS S5-16 on PostgreSQL 16: the CIF/CIP insurance policy record of FUL's shipment document set
// (tactical model FUL-AGG-ShipmentDocumentSet entity InsurancePolicyRecord; FUL-INV-005; simulated
// walkthrough SIM-02 step 8, finding F-03; Context Map R44, R74, R78; S5 decision D-13). Python
// V0.50 has no fulfilment context: test names cite the design.
//
// The world is the document world (newFulDocWorld: templates, sequences, the documentation clerk
// docs.1) with PTY's supplier party insurer.1 holding a verified insurer service role "cargo", and
// a second costing of the CST fixture: the same goods and freight plus a 12.00 USD cargo insurance
// fee (POL fee category insurance, category insurance) on logistics offer log.ins, frozen as its own
// snapshot (672.00 USD). An order whose quote adopted it has a costing insurance component of 12.00.

type fulInsWorld struct {
	*fulDocWorld
	insurer fulapi.ServiceRoleRef
	insured cstapi.SnapshotReceipt
}

func newFulInsWorld(t *testing.T) *fulInsWorld {
	t.Helper()
	w := &fulInsWorld{fulDocWorld: newFulDocWorld(t)}
	ed, rv := w.author, w.reviewer
	w.staffParty(ed, "insurer.1", []string{ptyapi.RoleSupplier})
	from := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	_, err := w.c.PTY.VerifyServiceRole(ed, ptyapi.VerifyServiceRole{IdempotencyKey: w.key(), PartyID: "insurer.1", ExpectedVersion: 2,
		ServiceRoleID: "cargo", Role: ptyapi.ServiceInsurer, ServiceCode: "cargo.all-risks", Origin: "CNSHA", Destination: "DEHAM",
		ValidFrom: kernel.FormatTimestamp(from), ValidUntil: kernel.FormatTimestamp(from.Add(60 * 24 * time.Hour)), EvidenceIDs: []string{"evidence"}})
	w.must(err)
	w.insurer = fulapi.ServiceRoleRef{PartyID: "insurer.1", ServiceRoleID: "cargo", Revision: 1}

	insurance := w.configure(ed, rv, polapi.KindFeeCategory, "insurance", 0, w.win.from, time.Time{}, polapi.ConfigData{FeeCategory: &polapi.FeeCategory{
		Name: "货运保险", Category: "insurance", Side: polapi.FeeSidePurchaseCost, Inclusion: polapi.InclusionSeparate,
		AllocationBasis: polapi.AllocationQuantity, PolicyBasis: "synthetic"}}, true)
	app := w.configure(ed, rv, polapi.KindTaxApplication, "tax.insurance", 0, w.win.from, time.Time{}, polapi.ConfigData{TaxApplication: &polapi.TaxApplication{
		Name: "insurance", Side: polapi.TaxSidePurchase, SubjectKind: polapi.SubjectFeeCategory, SubjectID: "insurance", PartyID: "provider.1",
		TaxProfile: w.tax, Jurisdiction: "CN", BasisNote: "synthetic"}}, true)
	l := w.logistics()
	fee := w.fee()
	fee.ID, fee.ObligationID, fee.FeeTypeRef, fee.TaxApplicationRef = "F2", "insurance.1", cref(insurance), cref(app)
	fee.Billing = &supapi.Billing{Kind: supapi.BillingFixed, Amount: "12"}
	l.Fees = append(l.Fees, fee)
	w.verified(supapi.KindLogisticsOffer, "log.ins", l)
	w.insured = w.frozen(w.scenario("offer.1", "log.ins"))
	if w.insured.TotalMinor != "67200" {
		w.t.Fatalf("the insured costing %+v", w.insured)
	}
	return w
}

// insuredOrder activates order "order.<quote>" of a quote that adopted the insured costing.
func (w *fulInsWorld) insuredOrder(quote string) trdapi.OrderReceipt {
	w.t.Helper()
	saved := w.snap
	w.snap = w.insured
	defer func() { w.snap = saved }()
	return w.activeOrder(quote)
}

// tradeTerms publishes the next revision of the fixture's trade terms on incoterm and returns it.
// Quotes after it would bind a superseded revision: activate every order first.
func (w *fulInsWorld) tradeTerms(incoterm string) polapi.ConfigRef {
	w.t.Helper()
	return w.configure(w.author, w.reviewer, polapi.KindTradeTerms, w.trade.ConfigID, w.rootVersion(polapi.KindTradeTerms, w.trade.ConfigID),
		w.win.from, time.Time{}, polapi.ConfigData{TradeTerms: &polapi.TradeTerms{Incoterm: incoterm, Edition: polapi.IncotermsEdition2020,
			NamedPlace: "HAMBURG", ResponsibilityNote: "synthetic evidence only", Delivery: polapi.Delivery{Kind: polapi.DeliveryFixedDate, Date: "2027-01-31"}}},
		true)
}

// amended moves order o by amendment id — the author prepares and submits, the reviewer decides and
// applies — to qty of L1 at price on trade terms terms, and returns o at its new revision.
func (w *fulInsWorld) amended(o trdapi.OrderReceipt, id, qty, price string, terms polapi.ConfigRef) trdapi.OrderReceipt {
	w.t.Helper()
	v := w.getOrder(o.ObjectID)
	cmd := w.amendCmd(o.ObjectID, id, 0, v.OrderRevision, v.OrderHash, qty)
	cmd.Lines[0].UnitPrice, cmd.Terms.TradeTermsRef = price, tref(terms)
	p, err := w.c.TRD.PrepareAmendment(w.author, cmd)
	w.must(err)
	s, err := w.c.TRD.SubmitAmendment(w.author, trdapi.SubmitAmendment{IdempotencyKey: w.key(), OrderID: o.ObjectID, AmendmentID: id,
		ExpectedVersion: p.RowVersion})
	w.must(err)
	d, err := w.c.TRD.DecideAmendment(w.reviewer, trdapi.DecideAmendment{IdempotencyKey: w.key(), OrderID: o.ObjectID, AmendmentID: id,
		ExpectedVersion: s.RowVersion, AmendmentHash: s.AmendmentHash, Decision: trdapi.DecisionApprove, Reason: "客户书面确认（合成）"})
	w.must(err)
	_, err = w.c.TRD.ApplyAmendment(w.reviewer, w.applyCmd(o.ObjectID, id, d.RowVersion))
	w.must(err)
	v = w.getOrder(o.ObjectID)
	o.OrderRevision, o.OrderHash = v.OrderRevision, v.OrderHash
	return o
}

// releasedOf is released for a purchase commitment line of committed PCS: release
// "rel.<commitment>" of quantity on the world's current order.
func (w *fulInsWorld) releasedOf(commitment, committed, quantity string) string {
	w.t.Helper()
	ref := qltapi.SubjectRef{Kind: qltapi.SubjectCommitmentLine, ID: commitment, LineID: "L1"}
	w.subjects.put(qlt.Subject{Ref: ref, Revision: 1, Hash: hex64("commitment " + commitment), Status: "issued", SupplierID: "supplier.1",
		OrderID: w.order.ObjectID, OrderRevision: w.order.OrderRevision, OrderHash: w.order.OrderHash, OrderLineID: "L1", SkuID: w.line.Sku.SkuID,
		SpecRevision: w.line.Sku.SpecRevision, UnitID: w.line.UnitRef.ID, UnitRevision: w.line.UnitRef.Revision, Quantity: committed})
	_, err := w.qlt.RecordInspection(w.qc, qltapi.RecordInspection{IdempotencyKey: w.key(), InspectionID: "insp." + commitment, Subject: ref,
		InspectedQuantity: quantity, ConformingQuantity: quantity, NonconformingQuantity: "0", Sampling: qltapi.Sampling{Method: qltapi.SamplingFull},
		Standard: qltapi.StandardRef{SourceID: "standard"}, Inspector: qltapi.Inspector{Kind: qltapi.InspectorInternal}, ReportFileID: "report",
		InspectedAt: kernel.FormatTimestamp(time.Now().Add(-time.Hour))})
	w.must(err)
	_, err = w.qlt.GrantRelease(w.qc, qltapi.GrantRelease{IdempotencyKey: w.key(), ReleaseID: "rel." + commitment, Subject: ref, Quantity: quantity,
		ValidUntil: kernel.FormatTimestamp(time.Now().Add(30 * 24 * time.Hour))})
	w.must(err)
	return "rel." + commitment
}

// documented prepares shipment id of n PCS on order o (commitment of committed PCS) and its set's
// CI and PL in en and zh-CN, the PL of cartons of 10 PCS.
func (w *fulInsWorld) documented(o trdapi.OrderReceipt, id, set, commitment, committed string, n int64) {
	w.t.Helper()
	restore := w.onOrder(o)
	defer restore()
	q := kernel.DecimalFromInt(n).String()
	_, err := w.prepare(w.merch, id, q, w.delivered(commitment, q), w.releasedOf(commitment, committed, q))
	w.must(err)
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.ciCmd(set, id, 0))
	w.must(err)
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.plCmd(set, id, 1, fulapi.Package{Cartons: n / 10, CartonFrom: 1,
		Contents: []fulapi.PackageContent{{LineID: "S1", QuantityPerCarton: "10"}}, GrossWeightKg: "5", NetWeightKg: "4.5", VolumeCbm: "0.05"}))
	w.must(err)
}

// policyCmd records the policy of set on shipment with the given insured amount and premium in USD,
// its file registered by the documentation clerk.
func (w *fulInsWorld) policyCmd(set, shipment string, expected int64, insured, premium string) fulapi.RecordInsurance {
	w.t.Helper()
	file := "file.policy." + set
	if _, err := w.c.EVD.GetFile(w.docs, file); err != nil {
		_, err = w.file(w.docs, file, "policy-"+set+".txt", evdapi.MimeText, "SYNTHETIC CARGO POLICY OF "+set, "")
		w.must(err)
	}
	return fulapi.RecordInsurance{IdempotencyKey: w.key(), DocumentSetID: set, ShipmentID: shipment, ExpectedVersion: expected, Insurer: w.insurer,
		PolicyNumber: "PIC-" + set, Clauses: []string{"Institute Cargo Clauses (A)", "Institute War Clauses (Cargo)"}, IssuedOn: "2026-12-18",
		Currency: ref(w.usd), InsuredAmount: insured, Premium: premium, FileID: file, EvidenceIDs: []string{"evidence"}}
}

func (w *fulInsWorld) insurance(set string) fulapi.InsuranceView {
	w.t.Helper()
	v, err := w.ful.GetInsurance(w.docs, set)
	w.must(err)
	return v
}

// SIM-02 step 8 (F-03) and FUL-INV-005 with D-13: the B2 order adopted the insured costing; the
// customer's PO makes it 1,000 shirts CIF Hamburg at 9.20 USD (9,200.00; insured at least 10,120.00),
// the 10 % reduction 900, and the factory's 850 ship: the CI is 7,820.00. Without a policy the CIF
// set is unreconciled at /insurance_policy, which no acknowledgement covers. A policy insuring
// 8,601.99 is INSURANCE_COVERAGE_INSUFFICIENT; 8,602.00 at a premium of 15.00 is recorded and shows
// the 3.00 difference to the costing's 12.00 insurance component; a new version at 12.00 shows none.
// The issue issues the policy with the CI and PL and supersedes the first version; the database
// keeps the record as recorded and refuses a policy record without its minimum cover.
func TestInsuranceOnTheCIFInvoiceSIM02Step8(t *testing.T) {
	w := newFulInsWorld(t)
	o := w.insuredOrder("quote.b2")
	cif := w.tradeTerms("CIF")
	o = w.amended(o, "amendment.b2.po", "1000", "9.20", cif)
	if f := w.snapshots(w.author, o.ObjectID)[o.ObjectID]; f.PayableMinor != "920000" || f.TradeTerms.Incoterm != "CIF" {
		t.Fatalf("the PO revision %+v", f)
	}
	o = w.amended(o, "amendment.b2.reduction", "900", "9.20", cif)
	w.documented(o, "shipment.b2", "docs.b2", "commitment.b2", "900", 850)

	v := w.set("docs.b2")
	if !slices.Equal(paths(v), []string{"/insurance_policy"}) || v.Reconciliation.Differences[0].Expected != "an insurance policy (CIF)" {
		t.Fatalf("CIF without a policy %v", v.Reconciliation.Differences)
	}
	if ins := w.insurance("docs.b2"); !ins.Required || ins.Incoterm != "CIF" || ins.Current != nil || ins.Coverage != nil || len(ins.Versions) != 0 {
		t.Fatalf("insurance before the policy %+v", ins)
	}
	_, err := w.ful.IssueShipmentDocuments(w.docs, fulapi.IssueShipmentDocuments{IdempotencyKey: w.key(), DocumentSetID: "docs.b2", ExpectedVersion: 2})
	if code(err) != fulapi.CodeShipmentDocumentsUnreconciled || path(err) != "/insurance_policy" {
		t.Fatalf("issue without a policy: %v", err)
	}
	_, err = w.ful.IssueShipmentDocuments(w.docs, fulapi.IssueShipmentDocuments{IdempotencyKey: w.key(), DocumentSetID: "docs.b2", ExpectedVersion: 2,
		Acknowledgements: []fulapi.Acknowledgement{{Path: "/insurance_policy", Reason: "保险后补"}}})
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/acknowledgements/0/path" {
		t.Fatalf("acknowledging the missing policy: %v", err)
	}

	_, err = w.ful.RecordInsurance(w.docs, w.policyCmd("docs.b2", "shipment.b2", 2, "8601.99", "15.00"))
	if rej, ok := kernel.AsRejection(err); !ok || rej.Code != fulapi.CodeInsuranceCoverageInsufficient || rej.Category != kernel.CategoryInvalid ||
		path(err) != "/args/insured_amount" || !strings.Contains(rej.Fields[0].Reason, "8601.99 USD is below 110 % of the CIF commercial invoice") ||
		!strings.Contains(rej.Fields[0].Reason, "of 7820.00 USD: 8602.00 USD") {
		t.Fatalf("8,601.99: %v", err)
	}
	cmd := w.policyCmd("docs.b2", "shipment.b2", 2, "8602.00", "15.00")
	r, err := w.ful.RecordInsurance(w.docs, cmd)
	ci := v.Documents[0]
	if err != nil || r.Status != fulapi.DocumentDraft || r.RowVersion != 3 || r.Version != 1 || r.Coverage == nil ||
		*r.Coverage != (fulapi.InsuranceCoverage{Incoterm: "CIF", InvoiceVersion: 1, InvoiceNumber: ci.Number, InvoiceCurrency: "USD",
			InvoiceAmount: "7820.00", Percent: "110", RequiredAmount: "8602.00", InsuredCurrency: "USD", InsuredAmount: "8602.00", Sufficient: true}) ||
		r.Costing.Status != fulapi.PremiumDifferent || r.Costing.SnapshotID != w.insured.SnapshotID || r.Costing.ComponentAmount != "12.00" ||
		r.Costing.Premium != "15.00" || r.Costing.Difference != "3.00" || r.Costing.Currency != "USD" || len(r.Costing.Components) != 1 {
		t.Fatalf("8,602.00: %+v %v", r, err)
	}
	if again, err := w.ful.RecordInsurance(w.docs, cmd); err != nil || !again.Replayed || again.ActionID != r.ActionID || again.Version != 1 {
		t.Fatalf("the same key %+v %v", again, err)
	}
	r2, err := w.ful.RecordInsurance(w.agentDocsWith(fulapi.CommandRecordInsurance), w.policyCmd("docs.b2", "shipment.b2", 3, "8602.00", "12.00"))
	if err != nil || r2.Version != 2 || r2.RowVersion != 4 || r2.Costing.Status != fulapi.PremiumEqual || r2.Costing.Difference != "0.00" {
		t.Fatalf("an AI agent records the premium of 12.00: %+v %v", r2, err)
	}
	ins := w.insurance("docs.b2")
	if !ins.Required || ins.Current == nil || ins.Current.Version != 2 || ins.Current.Status != fulapi.DocumentDraft || len(ins.Versions) != 2 ||
		ins.Versions[0].Status != fulapi.DocumentSuperseded || ins.Coverage == nil || !ins.Coverage.Sufficient || ins.Coverage.RequiredAmount != "8602.00" ||
		ins.Current.Insurer != w.insurer || ins.Current.PolicyNumber != "PIC-docs.b2" || ins.Current.InsuredAmount != "8602.00" ||
		ins.Current.Premium != "12.00" || ins.Current.CoveragePercent != "110" || ins.Current.FileID != "file.policy.docs.b2" ||
		ins.Current.RecordedBy.Kind != kernel.ActorAI || ins.Versions[0].Costing.Difference != "3.00" {
		t.Fatalf("insurance %+v", ins)
	}
	if v = w.set("docs.b2"); v.Reconciliation.State != fulapi.Reconciled || len(v.Documents) != 4 {
		t.Fatalf("covered %v", paths(v))
	}
	iss, err := w.ful.IssueShipmentDocuments(w.docs, fulapi.IssueShipmentDocuments{IdempotencyKey: w.key(), DocumentSetID: "docs.b2", ExpectedVersion: 4})
	if err != nil || !slices.Contains(iss.Issued, fulapi.DocumentRef{DocumentKind: fulapi.DocumentInsurancePolicy, Version: 2}) || len(iss.Issued) != 3 {
		t.Fatalf("issue %+v %v", iss, err)
	}
	if ins = w.insurance("docs.b2"); ins.Current.Status != fulapi.DocumentIssued || ins.Current.IssueSeq != 1 {
		t.Fatalf("issued %+v", ins.Current)
	}
	var issued []any
	for _, e := range w.fulEventsOf(fulapi.EventShipmentDocumentIssued) {
		if e.Payload["document_set_id"] == "docs.b2" {
			issued = e.Payload["documents"].([]any)
		}
	}
	if len(issued) != 3 || issued[2].(map[string]any)["document_kind"] != fulapi.DocumentInsurancePolicy ||
		issued[2].(map[string]any)["number"] != "PIC-docs.b2" {
		t.Fatalf("ShipmentDocumentIssued %v", issued)
	}

	// The record is kept as recorded; the database refuses a policy record below the minimum cover.
	if err := w.exec("company.A", `UPDATE brant_ful.shipment_document SET content = replace(content, '860200', '1')
		WHERE document_set_id = 'docs.b2' AND document_kind = 'insurance_policy'`); err == nil {
		t.Fatal("the database changed a policy record")
	}
	err = w.exec("company.A", `WITH c AS (SELECT '{"insurance_policy":{"policy_number":"PIC-X","currency":{"code":"USD"},"insured_minor":"1",
		"premium_minor":"0","coverage_percent":"100","clauses":["A"]}}'::text AS content)
		INSERT INTO brant_ful.shipment_document (tenant_id, company_id, document_set_id, document_kind, version, status, set_version, number,
			content, content_sha256, file_id, file_sha256, file_mime_type, sources, evidence_ids, idempotency_key, prepared_by_kind, prepared_by_id,
			prepare_epoch, prepared_at, commit_entries, commit_entries_sha256)
		SELECT current_setting('app.tenant_id'), current_setting('app.company_id'), 'docs.b2', 'insurance_policy', 3, 'draft', 6, 'PIC-X',
			content, encode(sha256(convert_to(content, 'UTF8')), 'hex'), 'file.x', repeat('a', 64), 'text/plain', '[]', '{}', 'k.x', 'human',
			'docs.1', 1, now(), '[]', encode(sha256(convert_to('[]', 'UTF8')), 'hex') FROM c`)
	if sqlCode(err) != "23514" || !strings.Contains(err.Error(), "shipment_document_insurance_policy") {
		t.Fatalf("a policy record covering 100 %%: %v", err)
	}
}

// agentDocsWith provisions "agent.ins", an AI agent with the document permissions and permission.
func (w *fulInsWorld) agentDocsWith(permission string) context.Context {
	return w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("ai", "agent.ins"), DataScope: "company",
		Permissions: []string{permission, "ful.read", "evd.read", "pty.read", "pol.read", "trd.read", "cst.read"}})
}

// FUL-INV-005 and F-03: a missing policy blocks the issue of a CIF set and not of an FOB one. Two
// orders: order.quote.1 (the world's costing, which has no insurance component) moved to FOB, and
// order.quote.cif moved to CIF. The FOB set issues without a policy and needs none. The CIF set,
// over HTTP: the issue is refused at /insurance_policy; insurance.record insuring less than 110 % of
// the 50 × 8.00 = 400.00 invoice is refused 422 INSURANCE_COVERAGE_INSUFFICIENT; 440.00 is recorded
// (reconcile route committed); insurance.get shows the cover and the premium against a costing
// without an insurance component; the set issues.
func TestMissingPolicyBlocksCIFNotFOBOverHTTP(t *testing.T) {
	w := newFulInsWorld(t)
	other := w.activeOrder("quote.cif")
	fob := w.tradeTerms("FOB")
	base := w.amended(w.order, "amendment.fob", "120", "8", fob)
	cif := w.tradeTerms("CIF")
	other = w.amended(other, "amendment.cif", "120", "8", cif)

	w.documented(base, "shipment.fob", "docs.fob", "commitment.fob", "120", 50)
	if v := w.set("docs.fob"); v.Reconciliation.State != fulapi.Reconciled {
		t.Fatalf("FOB %v", paths(v))
	}
	if ins := w.insurance("docs.fob"); ins.Required || ins.Incoterm != "FOB" || ins.Current != nil {
		t.Fatalf("FOB insurance %+v", ins)
	}
	if _, err := w.ful.IssueShipmentDocuments(w.docs, fulapi.IssueShipmentDocuments{IdempotencyKey: w.key(), DocumentSetID: "docs.fob",
		ExpectedVersion: 2}); err != nil {
		t.Fatalf("FOB issues without a policy: %v", err)
	}

	w.documented(other, "shipment.cif", "docs.cif", "commitment.cif", "120", 50)
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	docs := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "docs.1", "human"), n: 300}
	status, out := docs.try("ful", fulapi.Contract, fulapi.CommandIssueShipmentDocuments, map[string]any{"document_set_id": "docs.cif",
		"expected_version": 2})
	if status != http.StatusConflict || out["code"] != fulapi.CodeShipmentDocumentsUnreconciled || httpPath(out) != "/insurance_policy" {
		t.Fatalf("CIF issue without a policy %d %v", status, out)
	}
	w.policyCmd("docs.cif", "shipment.cif", 2, "1", "0") // registers the policy file
	args := func(insured string) map[string]any {
		return map[string]any{"document_set_id": "docs.cif", "shipment_id": "shipment.cif", "expected_version": 2,
			"insurer": map[string]any{"party_id": "insurer.1", "service_role_id": "cargo", "revision": 1}, "policy_number": "PIC-HTTP-1",
			"clauses": []any{"Institute Cargo Clauses (A)"}, "issued_on": "2026-12-18",
			"currency": map[string]any{"kind": "currency", "id": w.usd.ConfigID, "revision": w.usd.Revision}, "insured_amount": insured,
			"premium": "1.20", "file_id": "file.policy.docs.cif", "evidence_ids": []any{"evidence"}}
	}
	status, out = docs.try("ful", fulapi.Contract, fulapi.CommandRecordInsurance, args("439.99"))
	if status != http.StatusUnprocessableEntity || out["code"] != fulapi.CodeInsuranceCoverageInsufficient || httpPath(out) != "/args/insured_amount" {
		t.Fatalf("439.99 %d %v", status, out)
	}
	record := map[string]any{"contract": fulapi.Contract, "command": fulapi.CommandRecordInsurance, "idempotency_key": "ful.http.insurance",
		"args": args("440.00")}
	status, rec := docs.do("POST", "/api/brant/v1/ful/commands/"+fulapi.CommandRecordInsurance, record)
	if status != http.StatusOK || rec["status"] != fulapi.DocumentDraft || rec["row_version"] != float64(3) || rec["object_id"] != "docs.cif" {
		t.Fatalf("440.00 %d %v", status, rec)
	}
	status, rc := docs.do("POST", "/api/brant/v1/ful/commands/"+fulapi.CommandRecordInsurance+"/reconcile", record)
	if status != http.StatusOK || rc["state"] != "committed" || rc["action_id"] != rec["action_id"] {
		t.Fatalf("reconcile %d %v", status, rc)
	}
	status, ins := docs.do("GET", "/api/brant/v1/ful/document_sets/docs.cif/insurance", nil)
	cov, _ := ins["coverage"].(map[string]any)
	cur, _ := ins["current"].(map[string]any)
	costing, _ := cur["costing"].(map[string]any)
	if status != http.StatusOK || ins["required"] != true || ins["incoterm"] != "CIF" || cov["invoice_amount"] != "400.00" ||
		cov["required_amount"] != "440.00" || cov["insured_amount"] != "440.00" || cov["sufficient"] != true || cur["policy_number"] != "PIC-HTTP-1" ||
		costing["status"] != fulapi.PremiumNoComponent || costing["premium"] != "1.20" || costing["snapshot_id"] != w.snap.SnapshotID {
		t.Fatalf("insurance %d %v", status, ins)
	}
	issue := docs.command("ful", fulapi.Contract, fulapi.CommandIssueShipmentDocuments, map[string]any{"document_set_id": "docs.cif",
		"expected_version": 3})
	if issue["status"] != fulapi.DocumentIssued || issue["row_version"] != float64(4) {
		t.Fatalf("issue %v", issue)
	}
}

// httpPath is the path of an HTTP error body's first field error.
func httpPath(out map[string]any) string {
	fields, _ := out["field_errors"].([]any)
	if len(fields) == 0 {
		return ""
	}
	f, _ := fields[0].(map[string]any)
	p, _ := f["path"].(string)
	return p
}
