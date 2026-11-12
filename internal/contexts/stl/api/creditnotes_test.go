package api

import (
	"encoding/json"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The closed contracts of credit_note.request, credit.confirm and settlement.reverse, and application.apply's
// sources (S5 unit C8; STL-INV-007, STL-INV-008, STL-INV-009).
func TestCreditNoteAndReversalContracts(t *testing.T) {
	req := RequestCreditNote{IdempotencyKey: "k", CreditNoteID: "cn.1", CreditKind: CreditPriceAllowance, OrderID: "order.1", StatementID: "st.1",
		Amount: "100.00", Basis: "到港色差，协商折让", EvidenceIDs: []string{"claim.1"}}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		mutate func(*RequestCreditNote)
		path   string
	}{
		"kind":        {func(c *RequestCreditNote) { c.CreditKind = "refund" }, "/args/kind"},
		"order":       {func(c *RequestCreditNote) { c.OrderID = "" }, "/args/order_id"},
		"statement":   {func(c *RequestCreditNote) { c.StatementID = "" }, "/args/statement_id"},
		"amount":      {func(c *RequestCreditNote) { c.Amount = "-1" }, "/args/amount"},
		"basis":       {func(c *RequestCreditNote) { c.Basis = " " }, "/args/basis"},
		"no evidence": {func(c *RequestCreditNote) { c.EvidenceIDs = nil }, "/args/evidence_ids"},
		"id":          {func(c *RequestCreditNote) { c.CreditNoteID = "" }, "/args/credit_note_id"},
	} {
		x := req
		c.mutate(&x)
		if got := pathOf(x.Validate()); got != c.path {
			t.Errorf("request %s: %q, want %q", name, got, c.path)
		}
	}
	confirm := ConfirmCredit{IdempotencyKey: "k", CreditNoteID: "cn.1", ExpectedVersion: 1, Decision: DecisionConfirm, Reason: "核准"}
	if err := confirm.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		cmd  ConfirmCredit
		path string
	}{
		"decision": {ConfirmCredit{IdempotencyKey: "k", CreditNoteID: "cn.1", ExpectedVersion: 1, Decision: "approve", Reason: "r"}, "/args/decision"},
		"reason":   {ConfirmCredit{IdempotencyKey: "k", CreditNoteID: "cn.1", ExpectedVersion: 1, Decision: DecisionReject}, "/args/reason"},
		"version":  {ConfirmCredit{IdempotencyKey: "k", CreditNoteID: "cn.1", Decision: DecisionReject, Reason: "r"}, "/args/expected_version"},
	} {
		if got := pathOf(c.cmd.Validate()); got != c.path {
			t.Errorf("confirm %s: %q, want %q", name, got, c.path)
		}
	}
	rev := ReverseSettlement{IdempotencyKey: "k", StatementID: "st.1", ExpectedVersion: 1, Reason: "单价错误"}
	if err := rev.Validate(); err != nil {
		t.Fatal(err)
	}
	rev.EvidenceIDs = []string{"e", "e"}
	if got := pathOf(rev.Validate()); got != "/args/evidence_ids/1" {
		t.Errorf("reverse evidence: %q", got)
	}
	rev.EvidenceIDs, rev.Reason = nil, ""
	if got := pathOf(rev.Validate()); got != "/args/reason" {
		t.Errorf("reverse reason: %q", got)
	}
	if c, ok := Category(CodeSettlementHasApplications); !ok || c != kernel.CategoryConflict {
		t.Fatalf("SETTLEMENT_HAS_APPLICATIONS is %v", c)
	}
}

func TestApplySources(t *testing.T) {
	recv := ApplicationTarget{Kind: TargetReceivable, StatementID: "st.1"}
	rate := &ConfigRef{Kind: KindFXRate, ConfigID: "cny.usd.settlement", Revision: 1}
	for name, c := range map[string]struct {
		cmd  Apply
		path string
	}{
		"credit note":          {Apply{IdempotencyKey: "k", ApplicationID: "a", CreditNoteID: "cn.1", Target: recv, Amount: "100.00"}, ""},
		"fund with a rate":     {Apply{IdempotencyKey: "k", ApplicationID: "a", FundRecordID: "f", Target: recv, Amount: "2750.00", FXRate: rate}, ""},
		"no source":            {Apply{IdempotencyKey: "k", ApplicationID: "a", Target: recv, Amount: "1"}, "/args/fund_record_id"},
		"both sources":         {Apply{IdempotencyKey: "k", ApplicationID: "a", FundRecordID: "f", CreditNoteID: "cn.1", Target: recv, Amount: "1"}, "/args/fund_record_id"},
		"credit note to stage": {Apply{IdempotencyKey: "k", ApplicationID: "a", CreditNoteID: "cn.1", Target: ApplicationTarget{Kind: TargetOrderStage, OrderID: "o", StageID: "P1"}, Amount: "1"}, "/args/credit_note_id"},
		"credit note at rate":  {Apply{IdempotencyKey: "k", ApplicationID: "a", CreditNoteID: "cn.1", Target: recv, Amount: "1", FXRate: rate}, "/args/credit_note_id"},
		"rate of credit":       {Apply{IdempotencyKey: "k", ApplicationID: "a", FundRecordID: "f", Target: recv, Amount: "1", FXRate: &ConfigRef{Kind: KindCreditPolicy, ConfigID: "x", Revision: 1}}, "/args/fx_rate/kind"},
	} {
		if got := pathOf(c.cmd.Validate()); got != c.path {
			t.Errorf("%s: %q, want %q", name, got, c.path)
		}
	}
	// The members C8 added stay out of the request hash while empty: a request sent before them hashes as
	// it did.
	b, err := json.Marshal(Apply{ApplicationID: "a", FundRecordID: "f", Target: recv, Amount: "1"})
	if err != nil || strings.Contains(string(b), "CreditNoteID") || strings.Contains(string(b), "FXRate") {
		t.Fatalf("%s %v", b, err)
	}
}
