//go:build integration

package bootstrap_test

import (
	"context"
	"testing"
	"time"

	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
)

// S5R unit R13 on PostgreSQL 16: CNX refuses a fund record STL reversed (simulated decision S5R-D22, E0, 2026-11-13:
// fund.reverse; Context Map R38 FundRecordRef v1 shows status reversed; the follow-up of the R8 report's gap 10).
// Python V0.50 has none of it. The world is newInvoiceWorld (S5 C3b) with STL's real fund records and a credit
// manager who reverses them (independent of the treasury that recorded them).

// reverseFund has the credit manager reverse fund record id at the version STL shows.
func (w *invoiceWorld) reverseFund(manager context.Context, id string) {
	w.t.Helper()
	v, err := w.c.STL.GetFundRecord(manager, id)
	w.must(err)
	r, err := w.c.STL.ReverseFund(manager, stlapi.ReverseFund{IdempotencyKey: w.key(), FundRecordID: id, ExpectedVersion: v.Version,
		Reason: "合成：银行冲正入账（测试数据）"})
	w.must(err)
	if r.Status != stlapi.FundReversed {
		w.t.Fatalf("reversed %+v", r)
	}
}

// document_bundle.check: a fund_record requirement links fr.kept; once STL reversed fr.gone, linking it is
// REFERENCE_STALE at its path; once fr.kept is reversed too, the check that linked it keeps what STL stated then and
// document_bundle.get shows it reversed, a check that states it again is REFERENCE_STALE and one without it passes.
func TestS5RD22TheBundleRefusesAReversedFundRecord(t *testing.T) {
	w := newInvoiceWorld(t)
	manager := w.assign("manager.r13", "credit_manager")
	d := w.declare
	w.dlot("lot.r13", "x1", "1", "PCS")
	bank := w.source(d.doc, "bank.r13", "合成银行回单（测试数据）")
	kept := w.fund("fr.kept", "1000.00", w.cny, w.bankCNY)
	gone := w.fund("fr.gone", "500.00", w.cny, w.bankCNY)
	fx := bundleRequirement("fx", cnxapi.RequirementFundRecord, cnxapi.RequirementVerified, bank)
	fx.FundRecordIDs = []string{kept}
	b, err := w.cnx.CheckBundle(d.doc, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.r13", Requirements: []cnxapi.Requirement{fx}})
	w.must(err)

	w.reverseFund(manager, gone)
	two := fx
	two.FundRecordIDs = []string{kept, gone}
	_, err = w.cnx.CheckBundle(d.doc, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.r13", ExpectedVersion: 1,
		Requirements: []cnxapi.Requirement{two}})
	if code(err) != cnxapi.CodeReferenceStale || path(err) != "/args/requirements/0/fund_record_ids/1" {
		t.Fatalf("a reversed fund record linked: %s at %s", code(err), path(err))
	}

	w.reverseFund(manager, kept)
	v, err := w.cnx.GetBundle(w.tax, b.BundleID)
	w.must(err)
	f := v.Checks[0].Requirements[0].FundRecords[0]
	if len(v.Checks) != 1 || f.FundRecordID != kept || f.ReceivedMinor != "100000" || f.Label != cnxapi.CommercialAllocation || !f.Reversed ||
		!v.Requirements[0].FundRecords[0].Reversed {
		t.Fatalf("the linked record shown reversed %+v", v.Checks)
	}
	_, err = w.cnx.CheckBundle(d.doc, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.r13", ExpectedVersion: 1,
		Requirements: []cnxapi.Requirement{fx}})
	if code(err) != cnxapi.CodeReferenceStale || path(err) != "/args/requirements/0/fund_record_ids/0" {
		t.Fatalf("stated again: %s at %s", code(err), path(err))
	}
	missing := bundleRequirement("fx", cnxapi.RequirementFundRecord, cnxapi.RequirementMissing)
	if r, err := w.cnx.CheckBundle(d.doc, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.r13", ExpectedVersion: 1,
		Requirements: []cnxapi.Requirement{missing}}); err != nil || r.CheckNo != 2 {
		t.Fatalf("without it %+v %v", r, err)
	}
	if v, err = w.cnx.GetBundle(w.tax, b.BundleID); err != nil || !v.Checks[0].Requirements[0].FundRecords[0].Reversed || len(v.Requirements[0].FundRecords) != 0 {
		t.Fatalf("after the new check %+v %v", v, err)
	}
}

// tax_refund_case.record_result received: the refund rests on an STL fund record (FundRecordRef v1); one STL reversed
// is REFERENCE_STALE at /args/fund_record_id; on a standing one the case is received, and once STL reverses that one
// too the result keeps the fund record it recorded and tax_refund_case.get shows it reversed.
func TestS5RD22TheReceivedRefundRefusesAReversedFundRecord(t *testing.T) {
	w := newInvoiceWorld(t)
	manager := w.assign("manager.r13", "credit_manager")
	inv := w.invoice(w.facts("SYN0R13", "100"))
	w.lot("lot.r13", time.Now().UTC(), "x1", "100")
	c, err := w.allocate(w.tax, "lot.r13", alloc("t1", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "100"))
	w.must(err)
	cny := &cnxapi.CurrencyRef{ID: w.cny.ConfigID, Revision: w.cny.Revision}
	for _, r := range []cnxapi.RecordTaxResult{
		{Stage: cnxapi.TaxExpected, Currency: cny, Amount: "100.00", Reason: "按合成口径估计（非税务结论）"},
		{Stage: cnxapi.TaxPlanned, Currency: cny, Amount: "100.00", TaxPeriod: "2026-11",
			Review: &cnxapi.ProfessionalReview{Reviewer: "合成税务师（测试）", EvidenceIDs: []string{w.review}}},
		{Stage: cnxapi.TaxSubmitted, ExternalReference: "SB-R13", EvidenceIDs: []string{w.receipt}},
		{Stage: cnxapi.TaxApproved, Currency: cny, Amount: "100.00", ResultText: "合成核准通知（测试数据）", EvidenceIDs: []string{w.receipt}},
	} {
		c, err = w.record(w.tax, c, r)
		w.must(err)
	}
	gone := w.fund("fund.r13.gone", "100.00", w.cny, w.bankCNY)
	w.reverseFund(manager, gone)
	_, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxReceived, Currency: cny, Amount: "100.00", FundRecordID: gone})
	if code(err) != cnxapi.CodeReferenceStale || path(err) != "/args/fund_record_id" {
		t.Fatalf("a reversed fund record: %s at %s", code(err), path(err))
	}
	paid := w.fund("fund.r13.paid", "100.00", w.cny, w.bankCNY)
	c, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxReceived, Currency: cny, Amount: "100.00", FundRecordID: paid})
	w.must(err)
	if v := w.taxCase(c.CaseID); v.Status != cnxapi.TaxReceived || v.Results[4].FundRecordID != paid || v.Results[4].FundRecordReversed {
		t.Fatalf("received %+v", v.Results)
	}
	w.reverseFund(manager, paid)
	v := w.taxCase(c.CaseID)
	if v.Status != cnxapi.TaxReceived || v.Results[4].FundRecordID != paid || !v.Results[4].FundRecordReversed || v.Results[3].FundRecordReversed ||
		v.Amounts.ReceivedMinor != "10000" {
		t.Fatalf("the result keeps its record, shown reversed %+v", v.Results)
	}
}
