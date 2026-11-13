package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	dcl "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/declaration"
	db "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/documentbundle"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// humanOnlyCommands (D-17) name declaration_case.confirm, .record_submission, .record_receipt and
// external_reconciliation.resolve; declaration_case.prepare and document_bundle.check are a
// candidate's path for humans and AI agents (role templates: documentation holds the declaration
// and bundle commands, tax_specialist external_reconciliation.resolve); no system actor. Reads need
// cnx.read.
func TestDeclarationAndBundleRules(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	human, both := []kernel.ActorKind{kernel.ActorHuman}, []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}
	for command, kinds := range map[string][]kernel.ActorKind{
		api.CommandPrepareDeclaration: both, api.CommandConfirmDeclaration: human, api.CommandRecordSubmission: human,
		api.CommandRecordReceipt: human, api.CommandCheckBundle: both, api.CommandResolveReconciliation: human,
	} {
		r, err := policy.Rule(command)
		if err != nil || r.Permission != command || len(r.ActorKinds) != len(kinds) || r.Admits(kernel.ActorSystem) {
			t.Fatalf("%s: %+v %v", command, r, err)
		}
		for _, k := range kinds {
			if !r.Admits(k) {
				t.Errorf("%s does not admit %s", command, k)
			}
		}
	}
	for _, command := range []string{api.CommandGetDeclaration, api.CommandDeclarations, api.CommandGetBundle, api.CommandBundles} {
		if r, err := policy.Rule(command); err != nil || r.Permission != api.PermissionRead || !r.Admits(kernel.ActorAI) {
			t.Errorf("%s: %+v %v", command, r, err)
		}
	}
}

// The request hash covers the business arguments, not the key or the CAS value.
func TestDeclarationRequestHashIgnoresKeyAndVersion(t *testing.T) {
	for _, pair := range [][2]api.Command{
		{api.PrepareDeclaration{IdempotencyKey: "k1", DeclarationID: "d", ExpectedVersion: 1, ExportLotID: "l"},
			api.PrepareDeclaration{IdempotencyKey: "k2", DeclarationID: "d", ExpectedVersion: 4, ExportLotID: "l"}},
		{api.ConfirmDeclaration{IdempotencyKey: "k1", DeclarationID: "d", ExpectedVersion: 1, Version: 1},
			api.ConfirmDeclaration{IdempotencyKey: "k2", DeclarationID: "d", ExpectedVersion: 2, Version: 1}},
		{api.RecordSubmission{IdempotencyKey: "k1", DeclarationID: "d", ExpectedVersion: 1, OperationReference: "op"},
			api.RecordSubmission{IdempotencyKey: "k2", DeclarationID: "d", ExpectedVersion: 3, OperationReference: "op"}},
		{api.RecordReceipt{IdempotencyKey: "k1", DeclarationID: "d", ExpectedVersion: 1, ReceiptID: "r"},
			api.RecordReceipt{IdempotencyKey: "k2", DeclarationID: "d", ExpectedVersion: 7, ReceiptID: "r"}},
		{api.CheckBundle{IdempotencyKey: "k1", ExportLotID: "l", ExpectedVersion: 0}, api.CheckBundle{IdempotencyKey: "k2", ExportLotID: "l", ExpectedVersion: 2}},
		{api.ResolveReconciliation{IdempotencyKey: "k1", BundleID: "b", ExpectedVersion: 1}, api.ResolveReconciliation{IdempotencyKey: "k2", BundleID: "b", ExpectedVersion: 5}},
	} {
		n1, k1, a := request(pair[0])
		n2, k2, b := request(pair[1])
		ha, errA := action.RequestHash(a)
		hb, errB := action.RequestHash(b)
		if n1 == "" || n1 != n2 || k1 == k2 || errA != nil || errB != nil || ha != hb {
			t.Errorf("%T: %v %v", pair[0], a, b)
		}
	}
}

// The declaration and bundle domains decide with codes the api declares, with the same categories,
// and their names are the contract's.
func TestDeclarationAndBundleDomainsAreTheContract(t *testing.T) {
	for _, categories := range []map[string]kernel.Category{dcl.Categories, db.Categories} {
		for code, category := range categories {
			if got, ok := api.Category(code); !ok || got != category {
				t.Errorf("%s: api says %q (declared %v), domain says %q", code, got, ok, category)
			}
		}
	}
	for domain, contract := range map[string]string{dcl.Draft: api.DeclarationDraft, dcl.Confirmed: api.DeclarationConfirmed,
		dcl.Submitted: api.DeclarationSubmitted, dcl.Unknown: api.DeclarationUnknown, dcl.Received: api.DeclarationReceived,
		dcl.Accepted: api.DeclarationAccepted, dcl.Released: api.DeclarationReleased, dcl.Rejected: api.DeclarationRejected,
		dcl.Amended: api.DeclarationAmended, dcl.Withdrawn: api.DeclarationWithdrawn, dcl.MappingVerified: api.MappingVerified,
		dcl.MappingUnverified + "m": api.MappingUnverified + "m", dcl.SubmitterBroker: api.SubmitterBroker, dcl.SubmitterOwnClerk: api.SubmitterOwnClerk,
		string(dcl.BaseUnit): api.DeclarationBaseUnit, db.Missing: api.RequirementMissing, db.Present: api.RequirementPresent,
		db.Verified + "r": api.RequirementVerified + "r", db.Complete: api.BundleComplete, db.Incomplete: api.BundleIncomplete,
		db.CommercialAllocation: api.CommercialAllocation, db.SubjectDeclaration: api.SubjectDeclaration, db.MeasureAmount: api.MeasureAmount,
		db.AdjustmentApproved: api.AdjustmentApproved, db.KindCustomsDeclaration: api.RequirementCustomsDeclaration, db.KindFundRecord: api.RequirementFundRecord,
		dcl.Amended + "x": api.ExternalAmended + "x", dcl.Withdrawn + "x": api.ExternalWithdrawn + "x"} {
		if domain != contract {
			t.Errorf("domain %q, api %q", domain, contract)
		}
	}
	if len(db.Kinds) != 12 || len(db.Responsibilities) != 9 || len(dcl.Outcomes) != 7 {
		t.Errorf("closed lists %v %v %v", db.Kinds, db.Responsibilities, dcl.Outcomes)
	}
}
