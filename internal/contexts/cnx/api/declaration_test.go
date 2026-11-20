package api

import (
	"testing"
)

func validPrepare() PrepareDeclaration {
	return PrepareDeclaration{IdempotencyKey: "k", ExportLotID: "lot.1", CodeTableVersion: "CN-2026", Items: []DeclarationItem{
		{ItemNo: 1, Lines: []DeclarationLine{{LineID: "E1"}, {LineID: "E2"}}},
		{ItemNo: 2, Lines: []DeclarationLine{{LineID: "E3", UnitConversion: &UnitConversionRef{ID: "dozen.pcs", Revision: 1}}}}}}
}

func validReceipt() RecordReceipt {
	return RecordReceipt{IdempotencyKey: "k", DeclarationID: "dcl-1", ExpectedVersion: 4, ReceiptID: "r1", Outcome: DeclarationReleased,
		External:       &ExternalReceipt{Sequence: 3, StatusCode: "R", OriginalText: "放行", ReceivedAt: "2026-11-12T08:00:00Z"},
		MappingVersion: "syn.v1", DeclarationNumber: "SYN-1", ExportedAt: "2026-11-11T16:00:00Z", EvidenceIDs: []string{"receipt"}}
}

// The closed contracts of the declaration commands (CNX-AGG-DeclarationCase): a new case names no
// declaration and expected version 0, items are numbered 1, 2, … and declare each line once
// (DUPLICATE_LINE_ID); a submission names our own clerk or a broker with its party; a receipt keeps
// the original text, the external status, the mapping version, the external sequence and the
// declaration number (CN-R05) — none of them for unknown, which needs a reason — and states the
// export date with a release (optional for an amendment, never otherwise).
func TestDeclarationContracts(t *testing.T) {
	for _, c := range []struct {
		cmd  interface{ Validate() error }
		want string
	}{
		{validPrepare(), "OK"},
		{func() PrepareDeclaration { c := validPrepare(); c.DeclarationID = "dcl-1"; return c }(), "SCHEMA_INVALID /args/expected_version"},
		{func() PrepareDeclaration {
			c := validPrepare()
			c.DeclarationID, c.ExpectedVersion = "dcl-1", 2
			return c
		}(), "OK"},
		{func() PrepareDeclaration { c := validPrepare(); c.CodeTableVersion = ""; return c }(), "SCHEMA_INVALID /args/code_table_version"},
		{func() PrepareDeclaration { c := validPrepare(); c.Items[1].ItemNo = 3; return c }(), "SCHEMA_INVALID /args/items/1/item_no"},
		{func() PrepareDeclaration { c := validPrepare(); c.Items[1].Lines[0].LineID = "E1"; return c }(), "DUPLICATE_LINE_ID /args/items/1/lines/0/line_id"},
		{func() PrepareDeclaration {
			c := validPrepare()
			c.Items[1].Lines[0].UnitConversion.Revision = 0
			return c
		}(),
			"SCHEMA_INVALID /args/items/1/lines/0/unit_conversion/revision"},
		{func() PrepareDeclaration { c := validPrepare(); c.Items = nil; return c }(), "SCHEMA_INVALID /args/items"},
		{ConfirmDeclaration{IdempotencyKey: "k", DeclarationID: "d", ExpectedVersion: 1, Version: 1}, "OK"},
		{ConfirmDeclaration{IdempotencyKey: "k", DeclarationID: "d", ExpectedVersion: 1}, "SCHEMA_INVALID /args/version"},
		{ConfirmDeclaration{IdempotencyKey: "k", DeclarationID: "d", ExpectedVersion: 1, Version: 1, EvidenceIDs: []string{"a", "a"}},
			"SCHEMA_INVALID /args/evidence_ids/1"},
		// S5R-D36: a broker's submission names its PTY service role (customs_broker); our own clerk's names none.
		{RecordSubmission{IdempotencyKey: "k", DeclarationID: "d", ExpectedVersion: 2, OperationReference: "op.1",
			Submitter:   Submitter{Kind: SubmitterBroker, PartyID: "broker.1", ServiceRole: &ServiceRoleRef{ID: "customs", Revision: 1}, Name: "合成报关行"},
			SubmittedAt: "2026-11-12T08:00:00Z"}, "OK"},
		{RecordSubmission{IdempotencyKey: "k", DeclarationID: "d", ExpectedVersion: 2, OperationReference: "op.1",
			Submitter: Submitter{Kind: SubmitterBroker, PartyID: "broker.1", Name: "合成报关行"}, SubmittedAt: "2026-11-12T08:00:00Z"},
			"SCHEMA_INVALID /args/submitter/service_role"},
		{RecordSubmission{IdempotencyKey: "k", DeclarationID: "d", ExpectedVersion: 2, OperationReference: "op.1",
			Submitter:   Submitter{Kind: SubmitterBroker, PartyID: "broker.1", ServiceRole: &ServiceRoleRef{ID: "customs"}, Name: "合成报关行"},
			SubmittedAt: "2026-11-12T08:00:00Z"}, "SCHEMA_INVALID /args/submitter/service_role/revision"},
		{RecordSubmission{IdempotencyKey: "k", DeclarationID: "d", ExpectedVersion: 2, OperationReference: "op.1",
			Submitter:   Submitter{Kind: SubmitterOwnClerk, ServiceRole: &ServiceRoleRef{ID: "customs", Revision: 1}, Name: "x"},
			SubmittedAt: "2026-11-12T08:00:00Z"}, "SCHEMA_INVALID /args/submitter/service_role"},
		{RecordSubmission{IdempotencyKey: "k", DeclarationID: "d", ExpectedVersion: 2, OperationReference: "op.1",
			Submitter: Submitter{Kind: SubmitterBroker, Name: "合成报关行"}, SubmittedAt: "2026-11-12T08:00:00Z"}, "SCHEMA_INVALID /args/submitter/party_id"},
		{RecordSubmission{IdempotencyKey: "k", DeclarationID: "d", ExpectedVersion: 2, OperationReference: "op.1",
			Submitter: Submitter{Kind: SubmitterOwnClerk, PartyID: "p", Name: "x"}, SubmittedAt: "2026-11-12T08:00:00Z"}, "SCHEMA_INVALID /args/submitter/party_id"},
		{RecordSubmission{IdempotencyKey: "k", DeclarationID: "d", ExpectedVersion: 2, Submitter: Submitter{Kind: SubmitterOwnClerk, Name: "x"},
			SubmittedAt: "2026-11-12T08:00:00Z"}, "SCHEMA_INVALID /args/operation_reference"},
		{validReceipt(), "OK"},
		{func() RecordReceipt { c := validReceipt(); c.ExportedAt = ""; return c }(), "SCHEMA_INVALID /args/exported_at"},
		{func() RecordReceipt { c := validReceipt(); c.Outcome = DeclarationAccepted; return c }(), "SCHEMA_INVALID /args/exported_at"},
		{func() RecordReceipt { c := validReceipt(); c.Outcome, c.ExportedAt = DeclarationAmended, ""; return c }(), "OK"},
		{func() RecordReceipt { c := validReceipt(); c.External.Sequence = 0; return c }(), "SCHEMA_INVALID /args/external/sequence"},
		{func() RecordReceipt { c := validReceipt(); c.External.OriginalText = ""; return c }(), "SCHEMA_INVALID /args/external/original_text"},
		{func() RecordReceipt { c := validReceipt(); c.MappingVersion = ""; return c }(), "SCHEMA_INVALID /args/mapping_version"},
		{func() RecordReceipt { c := validReceipt(); c.DeclarationNumber = ""; return c }(), "SCHEMA_INVALID /args/declaration_number"},
		{func() RecordReceipt { c := validReceipt(); c.EvidenceIDs = nil; return c }(), "SCHEMA_INVALID /args/evidence_ids"},
		{func() RecordReceipt { c := validReceipt(); c.Outcome = "lost"; return c }(), "SCHEMA_INVALID /args/outcome"},
		{func() RecordReceipt {
			c := validReceipt()
			c.AmendedItems = []AmendedItem{{ItemNo: 1, Quantity: "5"}}
			return c
		}(), "SCHEMA_INVALID /args/amended_items"},
		{func() RecordReceipt {
			c := validReceipt()
			c.Outcome, c.ExportedAt, c.AmendedItems = DeclarationAmended, "", []AmendedItem{{ItemNo: 1, Quantity: "5"}, {ItemNo: 1, Quantity: "6"}}
			return c
		}(), "SCHEMA_INVALID /args/amended_items/1/item_no"},
		{RecordReceipt{IdempotencyKey: "k", DeclarationID: "d", ExpectedVersion: 3, ReceiptID: "r", Outcome: DeclarationUnknown, Reason: "回执超时未到"}, "OK"},
		{RecordReceipt{IdempotencyKey: "k", DeclarationID: "d", ExpectedVersion: 3, ReceiptID: "r", Outcome: DeclarationUnknown}, "SCHEMA_INVALID /args/reason"},
		{RecordReceipt{IdempotencyKey: "k", DeclarationID: "d", ExpectedVersion: 3, ReceiptID: "r", Outcome: DeclarationUnknown, Reason: "x",
			MappingVersion: "v1"}, "SCHEMA_INVALID /args/mapping_version"},
		{RecordReceipt{IdempotencyKey: "k", DeclarationID: "d", ExpectedVersion: 3, ReceiptID: "r", Outcome: DeclarationUnknown, Reason: "x",
			External: &ExternalReceipt{Sequence: 1}}, "SCHEMA_INVALID /args/external"},
	} {
		if got := verdict(c.cmd.Validate()); got != c.want {
			t.Errorf("%+v: want %s, got %s", c.cmd, c.want, got)
		}
	}
}

func validCheck() CheckBundle {
	owner := RequirementOwner{Kind: OwnerActor, ID: "doc.1"}
	return CheckBundle{IdempotencyKey: "k", ExportLotID: "lot.1", Requirements: []Requirement{
		{RequirementID: "pl", Kind: RequirementPackingList, Version: "v1", Status: RequirementMissing, Owner: owner, DueOn: "2026-11-20"},
		{RequirementID: "ci", Kind: RequirementCommercialInvoice, Version: "v1", Status: RequirementPresent, Owner: owner, DueOn: "2026-11-20",
			EvidenceIDs: []string{"ci.scan"}},
		{RequirementID: "decl", Kind: RequirementCustomsDeclaration, Version: "1", Status: RequirementVerified, Owner: owner,
			EvidenceIDs: []string{"receipt"}, DeclarationID: "dcl-1", Basis: "放行回执核对"},
		{RequirementID: "fx", Kind: RequirementFundRecord, Version: "1", Status: RequirementPresent, Owner: ownerRef(OwnerRole, "treasury"),
			DueOn: "2026-12-31", EvidenceIDs: []string{"bank"}, FundRecordIDs: []string{"fr-1"}}}}
}

// ownerRef is a requirement owner.
func ownerRef(kind, id string) RequirementOwner { return RequirementOwner{Kind: kind, ID: id} }

func validResolve() ResolveReconciliation {
	return ResolveReconciliation{IdempotencyKey: "k", BundleID: "dbn-1", ExpectedVersion: 2, ReconciliationID: "rec.1",
		Subject: ReconciliationSubject{Kind: SubjectDeclaration, ID: "dcl-1"}, Measure: MeasureAmount, Currency: &CurrencyRef{ID: "USD", Revision: 1},
		External: ReconciledValue{Value: "960.00", Source: "报关单申报总价", EvidenceIDs: []string{"receipt"}},
		Internal: ReconciledValue{Value: "958.50", Source: "商业发票金额"}, Responsibility: Responsibility{Kind: "internal", Note: "运保费口径"},
		Adjustment: Adjustment{Kind: AdjustmentApproved, Note: "按报关口径桥接"}, Reason: "成交价与申报价口径差异"}
}

// The closed contracts of the bundle commands (CNX-AGG-DocumentBundle, CNX-INV-008): a missing
// requirement has no attachment and a due date; a present or verified one at least one attachment; a
// verified one a basis and needs no due date; a customs declaration names its declaration once it is
// attached, a fund record requirement its fund records; a reconciliation names its subject, the
// measure with its currency or unit, both values with their sources (evidence for the external
// one), the responsibility and the adjustment (a note when approved), and a reason.
func TestBundleContracts(t *testing.T) {
	for _, c := range []struct {
		cmd  interface{ Validate() error }
		want string
	}{
		{validCheck(), "OK"},
		{func() CheckBundle { c := validCheck(); c.Requirements[0].EvidenceIDs = []string{"x"}; return c }(), "SCHEMA_INVALID /args/requirements/0/evidence_ids"},
		{func() CheckBundle { c := validCheck(); c.Requirements[0].DueOn = ""; return c }(), "SCHEMA_INVALID /args/requirements/0/due_on"},
		{func() CheckBundle { c := validCheck(); c.Requirements[1].EvidenceIDs = nil; return c }(), "SCHEMA_INVALID /args/requirements/1/evidence_ids"},
		{func() CheckBundle { c := validCheck(); c.Requirements[2].Basis = ""; return c }(), "SCHEMA_INVALID /args/requirements/2/basis"},
		{func() CheckBundle { c := validCheck(); c.Requirements[2].DeclarationID = ""; return c }(), "SCHEMA_INVALID /args/requirements/2/declaration_id"},
		{func() CheckBundle { c := validCheck(); c.Requirements[1].DeclarationID = "dcl-1"; return c }(), "SCHEMA_INVALID /args/requirements/1/declaration_id"},
		{func() CheckBundle { c := validCheck(); c.Requirements[3].FundRecordIDs = nil; return c }(), "SCHEMA_INVALID /args/requirements/3/fund_record_ids"},
		{func() CheckBundle { c := validCheck(); c.Requirements[3].RequirementID = "ci"; return c }(), "SCHEMA_INVALID /args/requirements/3/requirement_id"},
		{func() CheckBundle { c := validCheck(); c.Requirements[0].Kind = "visa"; return c }(), "SCHEMA_INVALID /args/requirements/0/kind"},
		{func() CheckBundle { c := validCheck(); c.Requirements[0].DueOn = "2026-13-01"; return c }(), "SCHEMA_INVALID /args/requirements/0/due_on"},
		{func() CheckBundle { c := validCheck(); c.Requirements[0].Owner.Kind = "team"; return c }(), "SCHEMA_INVALID /args/requirements/0/owner/kind"},
		{validResolve(), "OK"},
		{func() ResolveReconciliation { c := validResolve(); c.Currency = nil; return c }(), "SCHEMA_INVALID /args/currency"},
		{func() ResolveReconciliation {
			c := validResolve()
			c.Measure, c.Currency, c.Unit = MeasureQuantity, nil, "PCS"
			return c
		}(), "OK"},
		{func() ResolveReconciliation { c := validResolve(); c.Unit = "PCS"; return c }(), "SCHEMA_INVALID /args/unit"},
		{func() ResolveReconciliation { c := validResolve(); c.External.EvidenceIDs = nil; return c }(), "SCHEMA_INVALID /args/external/evidence_ids"},
		{func() ResolveReconciliation { c := validResolve(); c.Internal.EvidenceIDs = []string{"x"}; return c }(), "SCHEMA_INVALID /args/internal/evidence_ids"},
		{func() ResolveReconciliation { c := validResolve(); c.Adjustment.Note = ""; return c }(), "SCHEMA_INVALID /args/adjustment/note"},
		{func() ResolveReconciliation { c := validResolve(); c.Responsibility.Kind = "god"; return c }(), "SCHEMA_INVALID /args/responsibility/kind"},
		{func() ResolveReconciliation { c := validResolve(); c.External.Value = "-1"; return c }(), "SCHEMA_INVALID /args/external/value"},
		{func() ResolveReconciliation { c := validResolve(); c.Subject.Kind = "invoice"; return c }(), "SCHEMA_INVALID /args/subject/kind"},
		{func() ResolveReconciliation { c := validResolve(); c.Reason = ""; return c }(), "SCHEMA_INVALID /args/reason"},
	} {
		if got := verdict(c.cmd.Validate()); got != c.want {
			t.Errorf("%+v: want %s, got %s", c.cmd, c.want, got)
		}
	}
}

// A licence release on a recorded declaration receipt names receipt_id and nothing the receipt
// already says; without it the stated result stays as S5 B5 built it.
func TestReleaseOnAReceiptContract(t *testing.T) {
	base := ReleaseLicense{IdempotencyKey: "k", LicenseID: "elc-1", ExpectedVersion: 2, AllocationID: "a", ReleaseID: "r", Reason: "撤销回执"}
	for _, c := range []struct {
		result ExternalResult
		want   string
	}{
		{ExternalResult{Kind: ExternalWithdrawn, ReceiptID: "rcpt.1"}, "OK"},
		{ExternalResult{Kind: ExternalWithdrawn, ReceiptID: "rcpt.1", DeclarationNumber: "SYN"}, "SCHEMA_INVALID /args/external_result/declaration_number"},
		{ExternalResult{Kind: ExternalWithdrawn, ReceiptID: "rcpt.1", EvidenceIDs: []string{"e"}}, "SCHEMA_INVALID /args/external_result/evidence_ids"},
		{ExternalResult{Kind: ExternalWithdrawn}, "SCHEMA_INVALID /args/external_result/declaration_number"},
		{ExternalResult{Kind: ExternalWithdrawn, DeclarationNumber: "SYN", ReceiptText: "撤销", ReceivedAt: "2026-11-12T08:00:00Z",
			EvidenceIDs: []string{"e"}}, "OK"},
	} {
		cmd := base
		r := c.result
		cmd.ExternalResult = &r
		if got := verdict(cmd.Validate()); got != c.want {
			t.Errorf("%+v: want %s, got %s", c.result, c.want, got)
		}
	}
}
