//go:build integration

package bootstrap_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CN-AT11, CNX half (出口后折让或退运: 销售、库存、报关及税务影响分别形成受控案件，不因贷项自动完成退税调整; S5 unit C10,
// left by C8; STL-INV-008 "贷项不自动调整退税资料"; CN-R09; CN-E01) on PostgreSQL 16, in C3a's declaration world: TRD's
// real active order, FUL's shipment of 60 PCS confirmed and its export lot lot.N (line E1 of shipment line S1) declared
// and released by customs, an STL running on that FUL (STL's port to FUL, R30), CNX on the real FUL adapter (R36) and
// STL's lookups (R38: FundRecordRef v1 and CreditNoteRef v1).
//
//   - The sales statement of the shipment line is 480.00 USD; the sales clerk requests a price allowance of 50.00.
//     Requested but not confirmed, its impact is not recorded (INVALID_STATE).
//   - The lot has a tax refund case: the tax specialist allocated invoice SYNCN11's 60 PCS to E1.
//   - The credit manager confirms the credit note. The tax specialist records its impact on the lot's bundle through
//     external_reconciliation.resolve with the subject credit_note: CNX reads CreditNoteRef v1, ties it to lot.N by
//     its statement's shipment line, and keeps what STL stated (kind, version 2, statement, order, 5000 USD minor,
//     lot line E1). The external value states what customs and the tax office show (0.00: no amended declaration,
//     no adjusted refund), the internal value is STL's 50.00 — another amount, another currency, an AI agent, or a
//     bundle of a lot the statement does not settle are refused.
//   - Nothing is recomputed: the declaration, the tax refund case (version, state, allocations, results), FUL's
//     shipment (60 PCS) and the statement's line stay as they were; the receivable stays 480.00 open (C8: a
//     confirmed credit note is applied separately).
func TestCNAT11CreditNoteAfterExportIsReflectedInCNX(t *testing.T) {
	w := newDeclWorld(t)
	for _, s := range []struct{ id, commitment string }{{"shipment.N", "commitment.1"}, {"shipment.M", "commitment.2"}} {
		w.shipment(s.id, "60", s.commitment)
		w.granted(s.id)
		_, err := w.confirm(w.merch, s.id, 1)
		w.must(err)
	}
	_, err := w.registerLot(w.merch, "lot.N", "shipment.N", lotLine("E1", "S1", "60"))
	w.must(err)
	_, err = w.registerLot(w.merch, "lot.M", "shipment.M", lotLine("E1", "S1", "60"))
	w.must(err)
	cny := w.currency(w.author, w.reviewer, "CNY", w.win)
	review := w.source(w.author, "tax.review.c10", "合成专业复核结论：免退（仅供测试，非税务结论）")
	_, err = w.saveConfig(w.author, polapi.KindExportTaxPolicy, "export.tax", 0, w.win.from, time.Time{}, exportTax(polapi.RefundExemptRefund, review))
	w.must(err)
	_, err = w.c.POL.Publish(w.reviewer, polapi.Publish{IdempotencyKey: w.key(), Kind: polapi.KindExportTaxPolicy, ConfigID: "export.tax", ExpectedVersion: 1,
		Revision: 1, EvidenceIDs: []string{review}, Reason: "核实合成退税政策"})
	w.must(err)
	declared := w.d.released("lot.N", kernel.Instant(time.Now()), "E1")

	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	ledger, err := stl.New(db, stl.Upstream{Orders: stl.TRDOrders(w.c.TRD), Configs: w.c.POL, Parties: w.c.PTY, Sources: w.c.EVD,
		Shipments: stl.FULShipmentLines(func() fulapi.ShipmentLineLookups { return w.ful }), Deliveries: stlDeliveries(w.c)})
	w.must(err)
	// The FUL world paid the order's stage in full before shipment; that prepayment is unapplied so the receivable
	// stays open and has room for the credit note (C8's credit room).
	for _, s := range w.account(w.order.ObjectID).Stages {
		_, err = w.c.STL.Unapply(w.treasury, stlapi.Unapply{IdempotencyKey: w.key(), ApplicationID: "app." + w.order.ObjectID + "." + s.StageID,
			ExpectedVersion: 1, Reason: "合成：撤销夹具的全额预收"})
		w.must(err)
	}
	treasury, clerk, manager := w.treasury, w.assign("sales.n", "sales_clerk"), w.assign("manager.n", "credit_manager")
	st, err := ledger.ConfirmSettlement(treasury, salesCmd(w.key(), "st.N", "shipment.N"))
	w.must(err)
	if st.AmountMinor != "48000" || st.OpenMinor != "48000" || st.OrderID != w.order.ObjectID {
		t.Fatalf("statement %+v", st)
	}
	claim := w.source(clerk, "claim.n", "合成客户折让函：到港后色差，协商折让 50.00 USD（测试数据）")
	_, err = ledger.RequestCreditNote(clerk, stlapi.RequestCreditNote{IdempotencyKey: w.key(), CreditNoteID: "cn.N", CreditKind: stlapi.CreditPriceAllowance,
		OrderID: w.order.ObjectID, StatementID: "st.N", Amount: "50.00", Basis: "到港色差，协商折让", EvidenceIDs: []string{claim}})
	w.must(err)

	// The lot's tax refund case before the credit note: invoice SYNCN11 (60 PCS) allocated to E1.
	tax := w.assign("tax.n", "tax_specialist")
	_, err = w.file(w.cx.doc1, "invoice.c10", "invoice.pdf.txt", evdapi.MimeText, "合成增值税专用发票（测试数据）", "")
	w.must(err)
	facts := cnxapi.InvoiceFacts{Kind: cnxapi.InvoiceSpecialVAT, Colour: cnxapi.ColourBlue, Number: "SYNCN11", IssuedOn: "2026-11-10",
		Supplier: cnxapi.InvoiceParty{PartyID: "supplier.1", Name: "合成供应商（测试）", TaxID: "SUPPLIER1"},
		Buyer:    cnxapi.InvoiceParty{Name: "合成企业", TaxID: "SYNTHETIC-NOT-REAL"}, Currency: cnxapi.CurrencyRef{ID: cny.ConfigID, Revision: cny.Revision},
		Lines: []cnxapi.InvoiceLine{{LineNo: 1, Description: "合成针织衫（测试）", Quantity: "60", Unit: "PCS", NetAmount: "300.00", TaxRate: "0.13",
			TaxAmount: "39.00"}}, NetTotal: "300.00", TaxTotal: "39.00", GrossTotal: "339.00"}
	imported, err := w.cnx.ImportInvoice(tax, cnxapi.ImportInvoice{IdempotencyKey: w.key(), FileID: "invoice.c10", Candidate: facts})
	w.must(err)
	inv, err := w.cnx.VerifyInvoice(tax, cnxapi.VerifyInvoice{IdempotencyKey: w.key(), PurchaseInvoiceID: imported.PurchaseInvoiceID,
		ExpectedVersion: imported.RowVersion, Decision: cnxapi.DecisionVerify, Facts: &facts})
	w.must(err)
	taxCase, err := w.cnx.AllocateTaxEvidence(tax, cnxapi.AllocateTaxEvidence{IdempotencyKey: w.key(), ExportLotID: "lot.N", PolicyID: "export.tax",
		Allocations: []cnxapi.EvidenceAllocation{{AllocationID: "tn1", ExportLineID: "E1", PurchaseInvoiceID: inv.PurchaseInvoiceID, LineNo: 1,
			Purpose: cnxapi.PurposeExportRefund, Quantity: "60"}}})
	w.must(err)
	caseBefore, err := w.cnx.GetTaxCase(tax, taxCase.CaseID)
	w.must(err)
	declBefore, err := w.cnx.GetDeclaration(tax, declared.DeclarationID)
	w.must(err)

	evidence := w.source(w.cx.doc1, "ci.n", "合成商业发票（测试数据）")
	bundles := map[string]cnxapi.BundleReceipt{}
	for _, lot := range []string{"lot.N", "lot.M"} {
		b, err := w.cnx.CheckBundle(w.cx.doc1, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: lot, Requirements: []cnxapi.Requirement{
			bundleRequirement("ci", cnxapi.RequirementCommercialInvoice, cnxapi.RequirementPresent, evidence)}})
		w.must(err)
		bundles[lot] = b
	}
	noChange := w.source(w.cx.doc1, "customs.tax.n", "合成：报关单未改单，退税申报未调整（截至记录时，测试数据）")
	resolve := func(bundle string, version int64, edit func(*cnxapi.ResolveReconciliation)) (cnxapi.BundleReceipt, error) {
		c := cnxapi.ResolveReconciliation{IdempotencyKey: w.key(), BundleID: bundle, ExpectedVersion: version, ReconciliationID: "rec.cn." + w.key(),
			Subject: cnxapi.ReconciliationSubject{Kind: cnxapi.SubjectCreditNote, ID: "cn.N"}, Measure: cnxapi.MeasureAmount,
			Currency:       &cnxapi.CurrencyRef{ID: w.usd.ConfigID, Revision: w.usd.Revision},
			External:       cnxapi.ReconciledValue{Value: "0.00", Source: "报关单 " + declBefore.DeclarationNumber + " 未改单；退税未申报调整", EvidenceIDs: []string{noChange}},
			Internal:       cnxapi.ReconciledValue{Value: "50.00", Source: "STL 贷项 cn.N（价格折让）"},
			Responsibility: cnxapi.Responsibility{Kind: "internal", Note: "税务专员评估是否需改单或调整退税申报"}, Adjustment: cnxapi.Adjustment{Kind: cnxapi.AdjustmentNone},
			Reason: "出口后折让：记录报关与退税影响，不重算退税"}
		if edit != nil {
			edit(&c)
		}
		return w.cnx.ResolveReconciliation(tax, c)
	}
	n := bundles["lot.N"]
	if _, err = resolve(n.BundleID, n.RowVersion, nil); code(err) != kernel.CodeInvalidState || path(err) != "/args/subject/id" {
		t.Fatalf("a requested credit note: %s %s", code(err), path(err))
	}
	_, err = ledger.ConfirmCredit(manager, stlapi.ConfirmCredit{IdempotencyKey: w.key(), CreditNoteID: "cn.N", ExpectedVersion: 1,
		Decision: stlapi.DecisionConfirm, Reason: "核准色差折让"})
	w.must(err)
	for name, c := range map[string]struct {
		bundle  cnxapi.BundleReceipt
		edit    func(*cnxapi.ResolveReconciliation)
		code, p string
	}{
		"another amount": {n, func(c *cnxapi.ResolveReconciliation) { c.Internal.Value = "49.00" }, kernel.CodeInvalidKnownValue, "/args/internal/value"},
		"another currency": {n, func(c *cnxapi.ResolveReconciliation) {
			c.Currency = &cnxapi.CurrencyRef{ID: cny.ConfigID, Revision: cny.Revision}
		}, kernel.CodeInvalidKnownValue, "/args/currency"},
		"another lot":  {bundles["lot.M"], nil, kernel.CodeReferenceInvalid, "/args/subject/id"},
		"no such note": {n, func(c *cnxapi.ResolveReconciliation) { c.Subject.ID = "cn.none" }, kernel.CodeReferenceInvalid, "/args/subject/id"},
	} {
		if _, err := resolve(c.bundle.BundleID, c.bundle.RowVersion, c.edit); code(err) != c.code || path(err) != c.p {
			t.Errorf("%s: %s %s", name, code(err), path(err))
		}
	}
	agent := w.declarationAgent("agent.cn")
	ai := cnxapi.ResolveReconciliation{IdempotencyKey: w.key(), BundleID: n.BundleID, ExpectedVersion: n.RowVersion, ReconciliationID: "rec.ai",
		Subject: cnxapi.ReconciliationSubject{Kind: cnxapi.SubjectCreditNote, ID: "cn.N"}, Measure: cnxapi.MeasureAmount,
		Currency: &cnxapi.CurrencyRef{ID: w.usd.ConfigID, Revision: w.usd.Revision},
		External: cnxapi.ReconciledValue{Value: "0.00", Source: "x", EvidenceIDs: []string{noChange}}, Internal: cnxapi.ReconciledValue{Value: "50.00", Source: "x"},
		Responsibility: cnxapi.Responsibility{Kind: "internal", Note: "x"}, Adjustment: cnxapi.Adjustment{Kind: cnxapi.AdjustmentNone}, Reason: "x"}
	if _, err = w.cnx.ResolveReconciliation(agent, ai); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("AI agent: %s", code(err))
	}
	recorded, err := resolve(n.BundleID, n.RowVersion, nil)
	w.must(err)
	if recorded.RowVersion != n.RowVersion+1 || recorded.ReconciliationID == "" {
		t.Fatalf("reconciliation %+v", recorded)
	}
	bv, err := w.cnx.GetBundle(tax, n.BundleID)
	w.must(err)
	if len(bv.Reconciliations) != 1 {
		t.Fatalf("reconciliations %+v", bv.Reconciliations)
	}
	r := bv.Reconciliations[0]
	if l := r.CreditNote; r.Subject.Kind != cnxapi.SubjectCreditNote || r.Subject.ID != "cn.N" || l == nil || l.CreditKind != stlapi.CreditPriceAllowance ||
		l.Version != 2 || l.StatementID != "st.N" || l.OrderID != w.order.ObjectID || l.Currency != "USD" || l.AmountMinor != "5000" ||
		!slices.Equal(l.LotLineIDs, []string{"E1"}) || !strings.HasPrefix(r.Difference, "-50") || r.Adjustment.Kind != cnxapi.AdjustmentNone {
		t.Fatalf("the credit note's impact %+v %+v", r, r.CreditNote)
	}

	caseAfter, err := w.cnx.GetTaxCase(tax, taxCase.CaseID)
	w.must(err)
	if caseAfter.RowVersion != caseBefore.RowVersion || caseAfter.Status != caseBefore.Status || len(caseAfter.Allocations) != 1 ||
		caseAfter.Allocations[0].Quantity != "60" || caseAfter.Allocations[0].Status != cnxapi.EvidenceStanding || len(caseAfter.Results) != len(caseBefore.Results) {
		t.Fatalf("the tax refund case moved: before %+v after %+v", caseBefore, caseAfter)
	}
	declAfter, err := w.cnx.GetDeclaration(tax, declared.DeclarationID)
	w.must(err)
	if declAfter.RowVersion != declBefore.RowVersion || declAfter.Status != declBefore.Status || !declAfter.ExportedAt.Equal(declBefore.ExportedAt) {
		t.Fatalf("the declaration moved: %+v", declAfter)
	}
	if s := w.fulWorld.shipment("shipment.N"); len(s.Lines) != 1 || s.Lines[0].Quantity != "60" {
		t.Fatalf("FUL's shipment %+v", s.Lines)
	}
	sv, err := ledger.GetStatement(treasury, "st.N")
	w.must(err)
	if len(sv.Lines) != 1 || sv.Lines[0].Quantity != "60" || sv.CreditedMinor != "5000" || sv.OpenMinor != "48000" {
		t.Fatalf("the statement %+v", sv)
	}
}
