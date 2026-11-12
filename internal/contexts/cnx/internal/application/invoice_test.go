package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	pin "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/purchaseinvoice"
	tr "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/taxrefund"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// humanOnlyCommands name purchase_invoice.verify and tax_refund_case.record_result (D-17); an
// import and an allocation admit humans and AI agents on the same command path (CN-AT18 "候选须核实
// 后进入相同业务命令"), never a system actor; each command needs its own name as permission (role
// templates purchaser and tax specialist); the reads need cnx.read.
func TestInvoiceAndTaxCaseRules(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	for command, kinds := range map[string][]kernel.ActorKind{
		api.CommandImportInvoice:       {kernel.ActorHuman, kernel.ActorAI},
		api.CommandVerifyInvoice:       {kernel.ActorHuman},
		api.CommandAllocateTaxEvidence: {kernel.ActorHuman, kernel.ActorAI},
		api.CommandRecordTaxResult:     {kernel.ActorHuman},
	} {
		r, err := policy.Rule(command)
		if err != nil || r.Permission != command || len(r.ActorKinds) != len(kinds) {
			t.Fatalf("%s: %+v %v", command, r, err)
		}
		for _, k := range kinds {
			if !r.Admits(k) {
				t.Errorf("%s does not admit %s", command, k)
			}
		}
		if r.Admits(kernel.ActorSystem) {
			t.Errorf("%s admits the system", command)
		}
	}
	for _, command := range []string{api.CommandGetInvoice, api.CommandInvoices, api.CommandGetTaxCase, api.CommandTaxCases} {
		if r, err := policy.Rule(command); err != nil || r.Permission != api.PermissionRead || !r.Admits(kernel.ActorAI) {
			t.Errorf("%s: %+v %v", command, r, err)
		}
	}
}

// The domains' codes, states, colours, purposes and allocation states are the contract's.
func TestInvoiceAndTaxCaseDomainsAreTheContract(t *testing.T) {
	for _, categories := range []map[string]kernel.Category{pin.Categories, tr.Categories} {
		for code, category := range categories {
			if got, ok := api.Category(code); !ok || got != category {
				t.Errorf("%s: api says %q (declared %v), domain says %q", code, got, ok, category)
			}
		}
	}
	for _, p := range [][2]string{{pin.Imported, api.InvoiceImported}, {pin.Verified, api.InvoiceVerified},
		{pin.Rejected, api.InvoiceRejected}, {pin.Reversed, api.InvoiceReversed}, {pin.Blue, api.ColourBlue}, {pin.Red, api.ColourRed},
		{pin.Verify, api.DecisionVerify}, {pin.Reject, api.DecisionReject}, {pin.Standing, api.MatchStanding}, {pin.Review, api.MatchReview},
		{tr.Expected, api.TaxExpected}, {tr.Planned, api.TaxPlanned}, {tr.Submitted, api.TaxSubmitted}, {tr.Approved, api.TaxApproved},
		{tr.Received, api.TaxReceived}, {tr.Rejected, api.TaxRejected}, {tr.ExportRefund, api.PurposeExportRefund},
		{tr.ExportExemption, api.PurposeExportExemption}, {tr.Released, api.EvidenceReleased}, {tr.Replaced, api.EvidenceReplaced},
		{tr.Standing, api.EvidenceStanding}, {tr.Review, api.EvidenceReview}, {tr.Review, pin.Review}} {
		if p[0] != p[1] {
			t.Errorf("domain %q, api %q", p[0], p[1])
		}
	}
}

// The request hash covers the business arguments, not the key or the CAS value.
func TestInvoiceAndTaxCaseRequestHashIgnoresKeyAndVersion(t *testing.T) {
	facts := &api.InvoiceFacts{Kind: api.InvoiceSpecialVAT, Number: "1"}
	cur := &api.CurrencyRef{ID: "CNY", Revision: 1}
	for _, pair := range [][2]api.Command{
		{api.ImportInvoice{IdempotencyKey: "k1", FileID: "f"}, api.ImportInvoice{IdempotencyKey: "k2", FileID: "f"}},
		{api.VerifyInvoice{IdempotencyKey: "k1", PurchaseInvoiceID: "p", ExpectedVersion: 1, Decision: "verify", Facts: facts},
			api.VerifyInvoice{IdempotencyKey: "k2", PurchaseInvoiceID: "p", ExpectedVersion: 4, Decision: "verify", Facts: facts}},
		{api.AllocateTaxEvidence{IdempotencyKey: "k1", ExportLotID: "lot"}, api.AllocateTaxEvidence{IdempotencyKey: "k2", ExportLotID: "lot"}},
		{api.RecordTaxResult{IdempotencyKey: "k1", CaseID: "c", ExpectedVersion: 2, Stage: "expected", Currency: cur},
			api.RecordTaxResult{IdempotencyKey: "k2", CaseID: "c", ExpectedVersion: 3, Stage: "expected", Currency: cur}},
	} {
		n1, k1, a := request(pair[0])
		n2, k2, b := request(pair[1])
		ha, errA := action.RequestHash(a)
		hb, errB := action.RequestHash(b)
		if n1 == "" || n1 != n2 || k1 == k2 || errA != nil || errB != nil || ha != hb {
			t.Errorf("%T: %v %v", pair[0], a, b)
		}
	}
	_, _, a := request(api.AllocateTaxEvidence{IdempotencyKey: "k1", ExportLotID: "lot", Allocations: []api.EvidenceAllocation{{Quantity: "600"}}})
	_, _, b := request(api.AllocateTaxEvidence{IdempotencyKey: "k1", ExportLotID: "lot", Allocations: []api.EvidenceAllocation{{Quantity: "601"}}})
	ha, _ := action.RequestHash(a)
	hb, _ := action.RequestHash(b)
	if ha == hb {
		t.Fatal("the request hash covers the allocations")
	}
}
