package api

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func outcome(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	if err != nil {
		return err.Error()
	}
	return "OK "
}

var draftHash = strings.Repeat("ab", 32)

// The closed contracts of the commitment commands (S5-01, api/commitment.go): identifiers, the
// restated draft hash, versions, the decision's shape (an approval with its validity and
// acknowledgements of known kinds with reasons, once each; a rejection with a reason only), the issue's
// sequence, sending time and evidence, and the confirmation's result with its deviation.
func TestCommitmentContracts(t *testing.T) {
	ev := []Evidence{{SourceID: "mail.1", Locator: "body"}}
	run := func(name string, err error, want string) {
		t.Helper()
		if got := outcome(err); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
	req := RequestCommitment{IdempotencyKey: "k", CommitmentID: "c.1", DraftID: "d.1", DraftHash: draftHash}
	run("request", req.Validate(), "OK ")
	bad := req
	bad.DraftHash = strings.ToUpper(draftHash)
	run("request hash", bad.Validate(), "SCHEMA_INVALID /args/draft_hash")

	approve := DecideCommitment{IdempotencyKey: "k", CommitmentID: "c.1", ExpectedVersion: 1, DraftHash: draftHash, Decision: DecisionApprove,
		ValidUntil: "2026-11-13T00:00:00Z", Acknowledgements: []Acknowledgement{{OrderLineID: "L1", Kind: DeviationOverbuy, Reason: "MOQ"}}}
	run("approve", approve.Validate(), "OK ")
	for want, edit := range map[string]func(c *DecideCommitment){
		"SCHEMA_INVALID /args/decision":                         func(c *DecideCommitment) { c.Decision = "maybe" },
		"SCHEMA_INVALID /args/valid_until":                      func(c *DecideCommitment) { c.ValidUntil = "2026-11-13T08:00:00+08:00" },
		"SCHEMA_INVALID /args/expected_version":                 func(c *DecideCommitment) { c.ExpectedVersion = 0 },
		"SCHEMA_INVALID /args/acknowledgements/0/kind":          func(c *DecideCommitment) { c.Acknowledgements[0].Kind = "price" },
		"SCHEMA_INVALID /args/acknowledgements/0/reason":        func(c *DecideCommitment) { c.Acknowledgements[0].Reason = " " },
		"SCHEMA_INVALID /args/acknowledgements/1":               func(c *DecideCommitment) { c.Acknowledgements = append(c.Acknowledgements, c.Acknowledgements[0]) },
		"SCHEMA_INVALID /args/acknowledgements/0/order_line_id": func(c *DecideCommitment) { c.Acknowledgements[0].OrderLineID = "L 1" },
	} {
		c := approve
		c.Acknowledgements = append([]Acknowledgement{}, approve.Acknowledgements...)
		edit(&c)
		run(want, c.Validate(), want)
	}
	reject := DecideCommitment{IdempotencyKey: "k", CommitmentID: "c.1", ExpectedVersion: 1, DraftHash: draftHash, Decision: DecisionReject,
		Reason: "Price above the costing"}
	run("reject", reject.Validate(), "OK ")
	for want, edit := range map[string]func(c *DecideCommitment){
		"SCHEMA_INVALID /args/reason":           func(c *DecideCommitment) { c.Reason = "" },
		"SCHEMA_INVALID /args/valid_until":      func(c *DecideCommitment) { c.ValidUntil = "2026-11-13T00:00:00Z" },
		"SCHEMA_INVALID /args/acknowledgements": func(c *DecideCommitment) { c.Acknowledgements = []Acknowledgement{} },
	} {
		c := reject
		edit(&c)
		run(want, c.Validate(), want)
	}

	run("revoke", RevokeCommitment{IdempotencyKey: "k", CommitmentID: "c.1", ExpectedVersion: 2, Reason: "r"}.Validate(), "OK ")
	run("revoke reason", RevokeCommitment{IdempotencyKey: "k", CommitmentID: "c.1", ExpectedVersion: 2}.Validate(), "SCHEMA_INVALID /args/reason")

	issue := IssueCommitment{IdempotencyKey: "k", CommitmentID: "c.1", ExpectedVersion: 2, SequenceRef: SequenceRef{ID: "po.seq", Revision: 1},
		SentAt: "2026-11-12T09:00:00Z", Evidence: ev}
	run("issue", issue.Validate(), "OK ")
	for want, edit := range map[string]func(c *IssueCommitment){
		"SCHEMA_INVALID /args/sequence_ref/revision": func(c *IssueCommitment) { c.SequenceRef.Revision = 0 },
		"SCHEMA_INVALID /args/sent_at":               func(c *IssueCommitment) { c.SentAt = "yesterday" },
		"SCHEMA_INVALID /args/evidence":              func(c *IssueCommitment) { c.Evidence = nil },
		"SCHEMA_INVALID /args/evidence/1":            func(c *IssueCommitment) { c.Evidence = append(append([]Evidence{}, ev...), ev...) },
		"SCHEMA_INVALID /args/evidence/0/locator":    func(c *IssueCommitment) { c.Evidence = []Evidence{{SourceID: "mail.1"}} },
		"SCHEMA_INVALID /args/file_id":               func(c *IssueCommitment) { c.FileID = "file 1" },
	} {
		c := issue
		edit(&c)
		run(want, c.Validate(), want)
	}

	conf := RecordSupplierConfirmation{IdempotencyKey: "k", CommitmentID: "c.1", ExpectedVersion: 3, Result: ConfirmationAsIssued,
		ReceivedAt: "2026-11-12T10:00:00Z", Evidence: ev, SupplierReference: "SO-88"}
	run("confirm", conf.Validate(), "OK ")
	for want, edit := range map[string]func(c *RecordSupplierConfirmation){
		"SCHEMA_INVALID /args/result":             func(c *RecordSupplierConfirmation) { c.Result = "accepted" },
		"SCHEMA_INVALID /args/deviation":          func(c *RecordSupplierConfirmation) { c.Deviation = "later" },
		"SCHEMA_INVALID /args/supplier_reference": func(c *RecordSupplierConfirmation) { c.SupplierReference = strings.Repeat("x", 65) },
	} {
		c := conf
		edit(&c)
		run(want, c.Validate(), want)
	}
	dev := conf
	dev.Result = ConfirmationWithDeviations
	run("deviation required", dev.Validate(), "SCHEMA_INVALID /args/deviation")
	dev.Deviation = "Delivery 5 days later"
	run("deviation", dev.Validate(), "OK ")
}

// DownstreamCommitments v1 (R71): a row scans with its NULL members empty; InspectionSubject v1 (R24):
// a line not issued is INVALID_STATE, refs are bounded and checked.
func TestDownstreamCommitmentsAndInspectionSubjects(t *testing.T) {
	line := "L1"
	row := []any{DownstreamKindAllocation, "L1", nil, &line, int64(1), DownstreamStateAllocated, &line, nil}
	got, err := ScanDownstreamCommitment(func(dest ...any) error {
		for i, d := range dest {
			switch p := d.(type) {
			case *string:
				*p = row[i].(string)
			case *int64:
				*p = row[i].(int64)
			case **string:
				if s, ok := row[i].(*string); ok {
					*p = s
				}
			}
		}
		return nil
	})
	if err != nil || got != (DownstreamCommitment{Kind: DownstreamKindAllocation, ObjectID: "L1", OrderLineID: "L1", OrderRevision: 1,
		State: DownstreamStateAllocated, Quantity: "L1"}) || len(DownstreamCommitmentsArgs("o")) != 1 {
		t.Fatalf("%+v, %v", got, err)
	}
	if (CommitmentLine{Code: ""}).Check("/p") != nil || outcome(CommitmentLine{Status: "approved", Code: kernel.CodeInvalidState}.Check("/p")) != "INVALID_STATE /p" {
		t.Fatal("subject")
	}
	if outcome(ValidateCommitmentLineRefs([]CommitmentLineRef{{CommitmentID: "c", OrderLineID: "L 1"}})) != "SCHEMA_INVALID /refs/0/order_line_id" ||
		outcome(ValidateCommitmentLineRefs(make([]CommitmentLineRef, MaxCommitmentLineRefs+1))) != "SCHEMA_INVALID /refs" {
		t.Fatal("refs")
	}
	for code := range map[string]bool{CodePurchaseDraftStale: true, CodePurchaseCommitmentNotApproved: true} {
		if c, ok := Category(code); !ok || c != kernel.CategoryConflict {
			t.Errorf("%s: %s", code, c)
		}
	}
}
