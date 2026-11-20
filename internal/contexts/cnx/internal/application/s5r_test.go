package application

import (
	"encoding/json"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	cert "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/certificate"
	dcl "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/declaration"
	pin "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/purchaseinvoice"
	tr "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/taxrefund"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5R-D23: humanOnlyCommands and the tax_specialist template name tax_refund_case.rebind_policy: a human only, under its
// own permission. Its request hash leaves out the key and the expected version, as every case command's.
func TestS5RRebindIsHumanOnly(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	r, err := policy.Rule(api.CommandRebindPolicy)
	if err != nil || r.Permission != api.CommandRebindPolicy || !r.Admits(kernel.ActorHuman) || r.Admits(kernel.ActorAI) || r.Admits(kernel.ActorSystem) {
		t.Fatalf("%+v %v", r, err)
	}
	name, key, args := request(api.RebindPolicy{IdempotencyKey: "k", CaseID: "trc.1", ExpectedVersion: 3, Revision: 2, Reason: "x"})
	if name != api.CommandRebindPolicy || key != "k" || args.(api.RebindPolicy).ExpectedVersion != 0 {
		t.Fatalf("%s %s %+v", name, key, args)
	}
}

// The S5R members are omitted when empty, so the request hash of a command stored before S5R replays unchanged
// (record_result, record_submission, certificate.register, purchase_invoice.verify without them).
func TestS5RMembersKeepTheRequestHashOfStoredCommands(t *testing.T) {
	for _, cmd := range []api.Command{
		api.RecordTaxResult{IdempotencyKey: "k", CaseID: "trc.1", ExpectedVersion: 2, Stage: api.TaxSubmitted, ExternalReference: "SB-1",
			EvidenceIDs: []string{"e1"}},
		api.RecordSubmission{IdempotencyKey: "k", DeclarationID: "d", ExpectedVersion: 2, OperationReference: "op",
			Submitter: api.Submitter{Kind: api.SubmitterOwnClerk, Name: "x"}, SubmittedAt: "2026-11-12T08:00:00Z"},
		api.RegisterCertificate{IdempotencyKey: "k", CertificateKind: api.CertificateOfOrigin, Number: "CO-1"},
	} {
		_, _, args := request(cmd)
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range []string{"PurchaseInvoiceID", "ServiceRole", "AgreementCode", "OriginForm", "IssuingBody"} {
			if strings.Contains(string(raw), `"`+member+`"`) {
				t.Errorf("%T hashes %s while empty: %s", cmd, member, raw)
			}
		}
	}
}

// The S5R names of the domains are the contract's (S5R-D12, D13, D14, D20).
func TestS5RDomainsAreTheContract(t *testing.T) {
	for domain, contract := range map[string]string{dcl.NotReceived: api.DeclarationNotReceived, tr.ConvertedToExemption: api.TaxConvertedToExemption,
		tr.EvidenceRolledBack: api.TaxEvidenceRolledBack, pin.Reversal: api.MatchReversal, cert.KindPreferentialOrigin: api.CertificatePreferentialOrigin,
		pin.CodeMatchExceeded: api.CodePurchaseInvoiceMatchExceeded} {
		if domain != contract {
			t.Errorf("domain %q, api %q", domain, contract)
		}
	}
}
