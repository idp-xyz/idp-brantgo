package api

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func pathOf(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

// The closed contracts of the change and cancellation commands (S5-19; PUR-INV-011).
func TestChangeContracts(t *testing.T) {
	src := SourceRef{Kind: KindSupplierOffer, ID: "offer.1", Revision: 1, ConditionLineID: "OL1"}
	ev := []Evidence{{SourceID: "letter", Locator: "p1"}}
	prep := func(edit func(c *PrepareCommitmentChange)) error {
		c := PrepareCommitmentChange{IdempotencyKey: "k", ChangeID: "change.1", CommitmentID: "commitment.1", Revision: 1,
			Lines: []ChangeLine{{OrderLineID: "L1", Quantity: "110", Source: &src, SupplierDate: "2027-01-20"}}, Reason: "letter", Evidence: ev}
		edit(&c)
		return c.Validate()
	}
	for _, tc := range []struct {
		edit func(c *PrepareCommitmentChange)
		want string
	}{
		{func(*PrepareCommitmentChange) {}, "OK"},
		{func(c *PrepareCommitmentChange) {
			c.Lines[0].Quantity, c.Lines[0].Source, c.Lines[0].SupplierDate = "0", nil, ""
		}, "OK"},
		{func(c *PrepareCommitmentChange) { c.Revision = 0 }, "SCHEMA_INVALID /args/revision"},
		{func(c *PrepareCommitmentChange) { c.Lines = nil }, "SCHEMA_INVALID /args/lines"},
		{func(c *PrepareCommitmentChange) { c.Lines[0].Quantity = "-1" }, "SCHEMA_INVALID /args/lines/0/quantity"},
		{func(c *PrepareCommitmentChange) { c.Lines[0].SupplierDate = "2027-02-30" }, "SCHEMA_INVALID /args/lines/0/supplier_date"},
		{func(c *PrepareCommitmentChange) { c.Lines[0].SupplierDate = "20270120" }, "SCHEMA_INVALID /args/lines/0/supplier_date"},
		{func(c *PrepareCommitmentChange) { c.Lines[0].Source = &SourceRef{Kind: "x"} }, "SCHEMA_INVALID /args/lines/0/source/kind"},
		{func(c *PrepareCommitmentChange) { c.Lines[0].Overbuy = &Overbuy{Quantity: "0"} }, "SCHEMA_INVALID /args/lines/0/overbuy/quantity"},
		{func(c *PrepareCommitmentChange) { c.Lines = append(c.Lines, c.Lines[0]) }, "DUPLICATE_LINE_ID /args/lines/1/order_line_id"},
		{func(c *PrepareCommitmentChange) { c.Reason = " " }, "SCHEMA_INVALID /args/reason"},
		{func(c *PrepareCommitmentChange) { c.Evidence = nil }, "SCHEMA_INVALID /args/evidence"},
	} {
		if got := pathOf(prep(tc.edit)); got != tc.want {
			t.Errorf("%s, want %s", got, tc.want)
		}
	}
	hash := strings.Repeat("a", 64)
	for _, tc := range []struct {
		c    DecideCommitmentChange
		want string
	}{
		{DecideCommitmentChange{IdempotencyKey: "k", ChangeID: "x", ExpectedVersion: 1, ChangeHash: hash, Decision: DecisionApprove,
			Acknowledgements: []Acknowledgement{{OrderLineID: "L1", Kind: DeviationDeliveryLate, Reason: "r"}}}, "OK"},
		{DecideCommitmentChange{IdempotencyKey: "k", ChangeID: "x", ExpectedVersion: 1, ChangeHash: hash, Decision: DecisionApprove,
			Acknowledgements: []Acknowledgement{{OrderLineID: "L1", Kind: "surprise", Reason: "r"}}}, "SCHEMA_INVALID /args/acknowledgements/0/kind"},
		{DecideCommitmentChange{IdempotencyKey: "k", ChangeID: "x", ExpectedVersion: 1, ChangeHash: "short", Decision: DecisionApprove}, "SCHEMA_INVALID /args/change_hash"},
		{DecideCommitmentChange{IdempotencyKey: "k", ChangeID: "x", ExpectedVersion: 1, ChangeHash: hash, Decision: DecisionReject}, "SCHEMA_INVALID /args/reason"},
		{DecideCommitmentChange{IdempotencyKey: "k", ChangeID: "x", ExpectedVersion: 1, ChangeHash: hash, Decision: "maybe"}, "SCHEMA_INVALID /args/decision"},
	} {
		if got := pathOf(tc.c.Validate()); got != tc.want {
			t.Errorf("%s, want %s", got, tc.want)
		}
	}
	if got := pathOf(ApplyCommitmentChange{IdempotencyKey: "k", ChangeID: "x"}.Validate()); got != "SCHEMA_INVALID /args/expected_version" {
		t.Error(got)
	}
	if got := pathOf(CancelCommitment{IdempotencyKey: "k", CommitmentID: "c", ExpectedVersion: 3, Reason: "r"}.Validate()); got != "SCHEMA_INVALID /args/evidence" {
		t.Error(got)
	}
	if got := pathOf(CancelCommitment{IdempotencyKey: "k", CommitmentID: "c", ExpectedVersion: 3, Reason: "r", Evidence: ev}.Validate()); got != "OK" {
		t.Error(got)
	}
	if got := pathOf(RecordSupplierConfirmation{IdempotencyKey: "k", CommitmentID: "c", ExpectedVersion: 3, Revision: -1, Result: ConfirmationAsIssued,
		ReceivedAt: "2026-11-12T09:00:00Z", Evidence: ev}.Validate()); got != "SCHEMA_INVALID /args/revision" {
		t.Error(got)
	}
}
