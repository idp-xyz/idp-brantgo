package config

import (
	"testing"
	"time"
)

func terms(stages ...PaymentStage) Content {
	return Content{Kind: KindPaymentTerms, ValidFrom: start, PaymentTerms: &PaymentTerms{Name: "30/70 CAD", Stages: stages}}
}

// S5 D-5 (G0 F-04, TRD-INV-022): a stage is paid against the shipping documents only when it is due
// after shipment — "70% 见提单副本付款且正本提单由我方控制" (SIM-01 step 7).
func TestReleaseControlOnlyAfterShipment(t *testing.T) {
	deposit := PaymentStage{StageID: "deposit", Percent: "30", Trigger: "order_confirmed"}
	for _, c := range []struct {
		trigger, control string
		want             string
	}{
		{"after_shipment", "documents", "OK"},
		{"after_shipment", "", "OK"},
		{"before_shipment", "", "OK"},
		{"before_shipment", "documents", "INVALID_KNOWN_VALUE /args/data/stages/1/release_control"},
		{"after_invoice", "documents", "INVALID_KNOWN_VALUE /args/data/stages/1/release_control"},
		{"order_confirmed", "documents", "INVALID_KNOWN_VALUE /args/data/stages/1/release_control"},
	} {
		balance := PaymentStage{StageID: "balance", Percent: "70", Trigger: c.trigger, ReleaseControl: c.control}
		if got := code(Validate("terms", terms(deposit, balance))); got != c.want {
			t.Errorf("%s with %q: %s, want %s", c.trigger, c.control, got, c.want)
		}
	}
	// POL-INV-004 is still judged first.
	short := PaymentStage{StageID: "balance", Percent: "60", Trigger: "before_shipment", ReleaseControl: "documents"}
	if got := code(Validate("terms", terms(deposit, short))); got != "PAYMENT_TOTAL_MISMATCH /args/data/stages" {
		t.Fatal(got)
	}
}

// CN-D02 (CN-AT12): an export tax policy is published only on its professional review, which the
// publication cites, so the publisher can read it (R63).
func TestExportTaxPolicyCitesItsProfessionalReview(t *testing.T) {
	policy := Content{Kind: KindExportTaxPolicy, ValidFrom: start, ExportTaxPolicy: &ExportTaxPolicy{Name: "synthetic", RefundMethod: "exempt_refund",
		PolicyBasis: "synthetic basis", ProfessionalReviewEvidenceID: "tax.review"}}
	r := New(KindExportTaxPolicy, "export.tax")
	mustSave(t, r, policy, editor, 1, start)
	if _, err := r.Publish(1, Facts{}, reviewer, 1, []string{"evidence"}, clock); code(err) != "REFERENCE_INVALID /args/evidence_ids" {
		t.Fatalf("the professional review must be cited: %s", code(err))
	}
	if _, err := r.Publish(1, Facts{}, reviewer, 1, []string{"evidence", "tax.review"}, clock); err != nil {
		t.Fatal(err)
	}
	if r.Active == nil || r.Active.Content.ExportTaxPolicy.RefundMethod != "exempt_refund" || len(r.Active.Content.References()) != 0 {
		t.Fatalf("active %+v", r.Active)
	}
	// A later revision supersedes it; the superseded revision keeps its method (CN-R08: 适用方法变化不
	// 追溯覆盖历史), and was in force while it was active.
	next := policy
	next.ExportTaxPolicy = &ExportTaxPolicy{Name: "synthetic", RefundMethod: "exempt_credit_refund", PolicyBasis: "changed basis",
		ProfessionalReviewEvidenceID: "tax.review.2"}
	mustSave(t, r, next, editor, 1, clock)
	later := clock.Add(time.Hour)
	superseded, err := r.Publish(2, Facts{}, reviewer, 1, []string{"tax.review.2"}, later)
	if err != nil {
		t.Fatal(err)
	}
	if superseded.Content.ExportTaxPolicy.RefundMethod != "exempt_refund" || InForce(superseded, clock) != nil ||
		code(InForce(superseded, later)) != "CONFIG_NOT_ACTIVE /" || InForce(r.Active, later) != nil {
		t.Fatalf("superseded %+v", superseded)
	}
}

// The S5 members are stored when set, read back and hash as stored.
func TestS5ContentRoundTrips(t *testing.T) {
	cad := terms(PaymentStage{StageID: "deposit", Percent: "30", Trigger: "order_confirmed"},
		PaymentStage{StageID: "balance", Percent: "70", Trigger: "after_shipment", ReleaseControl: "documents"})
	e2 := Content{Kind: KindCreditPolicy, ValidFrom: start, CreditPolicy: &CreditPolicy{Name: "E-2", Currency: "USD", DefaultLimit: "0",
		LimitSources: []string{"internal_assessment"}, ApprovalTiers: []CreditApprovalTier{{Level: "credit_officer", MaxAmount: "1000.00"},
			{Level: "credit_manager", MaxAmount: "5000.00"}}, OverdueToleranceDays: 30, DocumentsSecured: true, ReservationValidityDays: 30,
		DecisionValidityDays: 7, TwoApproversAboveTop: true}}
	ci := Content{Kind: KindShipmentDocumentTemplate, ValidFrom: start, ShipmentDocumentTemplate: &ShipmentDocumentTemplate{Name: "ci",
		DocumentKind: "commercial_invoice", Language: "zh-CN", RendererVersion: "shipment-text/0.1.0", Title: "商业发票",
		Sections: []string{"seller", "buyer", "identity", "shipment", "lines", "totals", "terms", "notes"}, Clauses: []string{}, FooterNote: ""}}
	tax := Content{Kind: KindExportTaxPolicy, ValidFrom: start, ExportTaxPolicy: &ExportTaxPolicy{Name: "n", RefundMethod: "not_applicable",
		PolicyBasis: "b", ProfessionalReviewEvidenceID: "e"}}
	for c, want := range map[*Content]string{
		&cad: `{"name":"30/70 CAD","stages":[{"stage_id":"deposit","percent":"30","trigger":"order_confirmed","days":0},` +
			`{"stage_id":"balance","percent":"70","trigger":"after_shipment","days":0,"release_control":"documents"}]}`,
		&e2: `{"name":"E-2","currency":"USD","default_limit":"0","limit_sources":["internal_assessment"],"approval_tiers":[` +
			`{"level":"credit_officer","max_amount":"1000.00"},{"level":"credit_manager","max_amount":"5000.00"}],"overdue_tolerance_days":30,` +
			`"documents_secured":true,"reservation_validity_days":30,"decision_validity_days":7,"two_approvers_above_top":true}`,
		&ci: `{"name":"ci","document_kind":"commercial_invoice","language":"zh-CN","renderer_version":"shipment-text/0.1.0","title":"商业发票",` +
			`"sections":["seller","buyer","identity","shipment","lines","totals","terms","notes"],"clauses":[],"footer_note":""}`,
		&tax: `{"name":"n","refund_method":"not_applicable","policy_basis":"b","professional_review_evidence_id":"e"}`,
	} {
		raw, err := c.DataJSON()
		if err != nil || string(raw) != want {
			t.Fatalf("%s stores %s, %v", c.Kind, raw, err)
		}
		back := Content{Kind: c.Kind, ValidFrom: start}
		if err = back.DecodeData(raw); err != nil || back.Data() == nil {
			t.Fatal(c.Kind, err)
		}
		a, _ := c.Hash()
		b, _ := back.Hash()
		if a != b || len(a) != 64 {
			t.Errorf("%s does not round-trip", c.Kind)
		}
	}
	if err := Validate("credit", e2); err != nil {
		t.Fatal(err)
	}
	if got := code(CheckReferences("credit", e2, facts(currencyFact("USD", 2)))); got != "OK" {
		t.Fatal(got)
	}
	if len(ci.References()) != 0 || len(tax.References()) != 0 {
		t.Fatal("the S5 kinds refer to no configuration")
	}
}
