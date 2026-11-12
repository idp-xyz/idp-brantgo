package documentbundle

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func verdict(err error) string {
	if err == nil {
		return "OK"
	}
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code + " " + rej.Fields[0].Path
	}
	return err.Error()
}

func checklist() []Requirement {
	owner := Owner{Kind: "actor", ID: "doc.1"}
	return []Requirement{
		{RequirementID: "ci", Kind: KindCommercialInvoice, Version: "v1", Status: Present, Owner: owner, DueOn: "2026-11-20",
			Evidence: []Evidence{{SourceID: "ci.scan"}}},
		{RequirementID: "pl", Kind: KindPackingList, Version: "v1", Status: Missing, Owner: Owner{Kind: "party", ID: "supplier.1"}, DueOn: "2026-11-18"},
		{RequirementID: "decl", Kind: KindCustomsDeclaration, Version: "1", Status: Verified, Owner: owner, Evidence: []Evidence{{SourceID: "receipt"}},
			Declaration: &DeclarationRef{DeclarationID: "dcl-1", Status: "released", Exported: true}, Basis: "放行回执与报关单核对一致"},
		{RequirementID: "fx", Kind: KindFundRecord, Version: "1", Status: Verified, Owner: owner, Evidence: []Evidence{{SourceID: "bank"}},
			FundRecords: []FundRef{{FundRecordID: "fr-1", Label: CommercialAllocation}}, Basis: "银行回单核对"},
	}
}

// CN-AT14 bundle half and CNX-INV-008: a missing requirement is listed with its owner and due date,
// an attached one is present and does not count as met; a human verifies; the bundle is complete only
// when every requirement is verified; stating it complete with a present or missing requirement is
// DOCUMENT_REQUIREMENT_UNVERIFIED; an AI agent never verifies (PERMISSION_DENIED); a customs
// declaration customs has not released is not verified.
func TestPresenceIsNotValidityCNAT14(t *testing.T) {
	n, err := Check(CheckInput{Requirements: checklist(), Human: true})
	if err != nil || n.Missing != 1 || n.Present != 1 || n.Verified != 2 || n.Status() != Incomplete || n.MissingIDs[0] != "pl" || n.PresentIDs[0] != "ci" {
		t.Fatalf("%v %+v", err, n)
	}
	if _, err := Check(CheckInput{Requirements: checklist(), Human: true, ExpectComplete: true}); verdict(err) !=
		"DOCUMENT_REQUIREMENT_UNVERIFIED /args/requirements/0/status" {
		t.Fatalf("an attachment is not a verification: %s", verdict(err))
	}
	if _, err := Check(CheckInput{Requirements: checklist(), Human: false}); verdict(err) != "PERMISSION_DENIED /args/requirements/2/status" {
		t.Fatalf("an AI agent verifies nothing: %s", verdict(err))
	}
	pending := checklist()
	pending[2].Declaration.Exported, pending[2].Declaration.Status = false, "submitted"
	if _, err := Check(CheckInput{Requirements: pending, Human: true}); verdict(err) != "DOCUMENT_REQUIREMENT_UNVERIFIED /args/requirements/2/status" {
		t.Fatalf("a declaration not released: %s", verdict(err))
	}
	all := checklist()
	for i := range all {
		all[i].Status, all[i].Basis = Verified, "核实"
		all[i].Evidence = []Evidence{{SourceID: "x"}}
	}
	n, err = Check(CheckInput{Requirements: all, Human: true, ExpectComplete: true})
	if err != nil || n.Status() != Complete || n.Verified != 4 {
		t.Fatalf("complete %v %+v", err, n)
	}
	s, err := Seal(CheckDocument{BundleID: "dbn-1", CheckNo: 1, Requirements: checklist(), Status: Incomplete})
	if err != nil || s.Contract != CheckContract || s.Requirements[1].Evidence == nil || s.Requirements[0].FundRecords == nil {
		t.Fatalf("%v %+v", err, s)
	}
	back, ok, err := Unseal(s.Bytes, s.SHA256)
	if err != nil || !ok || back.Requirements[3].FundRecords[0].Label != CommercialAllocation || back.Requirements[1].DueOn != "2026-11-18" {
		t.Fatalf("%v %v %+v", err, ok, back)
	}
	if _, ok, _ := Unseal(append(s.Bytes, ' '), s.SHA256); ok {
		t.Fatal("a changed check is not intact")
	}
}

// ExternalReconciliation: the difference is external − internal exactly (declared value vs CI value,
// SIM-01 step 24); an approved adjustment needs a difference.
func TestReconciliationDifference(t *testing.T) {
	r := &Reconciliation{External: kernel.MustDecimal("960.00"), Internal: kernel.MustDecimal("958.5"), Adjustment: AdjustmentApproved}
	if err := Reconcile(r); err != nil || r.Difference.String() != "1.5" {
		t.Fatalf("%v %s", err, r.Difference)
	}
	r = &Reconciliation{External: kernel.MustDecimal("100"), Internal: kernel.MustDecimal("120"), Adjustment: AdjustmentNone}
	if err := Reconcile(r); err != nil || r.Difference.String() != "-20" {
		t.Fatalf("%v %s", err, r.Difference)
	}
	r = &Reconciliation{External: kernel.MustDecimal("5"), Internal: kernel.MustDecimal("5"), Adjustment: AdjustmentApproved}
	if got := verdict(Reconcile(r)); got != "INVALID_KNOWN_VALUE /args/adjustment/kind" {
		t.Fatal(got)
	}
	if err := CheckVersion(nil, 0); err != nil || verdict(CheckVersion(nil, 1)) != "VERSION_CONFLICT /args/expected_version" ||
		verdict(CheckVersion(&Bundle{RowVersion: 2}, 1)) != "VERSION_CONFLICT /args/expected_version" {
		t.Fatal("bundle versions")
	}
}
