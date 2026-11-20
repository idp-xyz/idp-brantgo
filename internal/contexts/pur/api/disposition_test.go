package api

import "testing"

// The closed contracts of the disposition commands (S5R-D05; SM-PUR-COMMITMENT-DISPOSITION): a known
// kind, a positive quantity in QuantityFormat, a reason, and evidence — at least one source for a return
// to the supplier ("退回工厂，附证据"), none required otherwise; a rejection states its reason.
func TestDispositionContracts(t *testing.T) {
	ev := []Evidence{{SourceID: "rma", Locator: "p1"}}
	dispose := func(edit func(c *DisposeCommitment)) error {
		c := DisposeCommitment{IdempotencyKey: "k", DispositionID: "disp.1", CommitmentID: "commitment.1", OrderLineID: "L1",
			Disposition: DispositionReturnToSupplier, Quantity: "40", Reason: "合成：退回工厂", Evidence: ev}
		edit(&c)
		return c.Validate()
	}
	for _, tc := range []struct {
		edit func(c *DisposeCommitment)
		want string
	}{
		{func(*DisposeCommitment) {}, "OK"},
		{func(c *DisposeCommitment) { c.Disposition, c.Evidence = DispositionWriteOff, nil }, "OK"},
		{func(c *DisposeCommitment) { c.Disposition, c.Evidence = DispositionHoldForReuse, nil }, "OK"},
		{func(c *DisposeCommitment) { c.Disposition = DispositionWriteOff }, "OK"},
		{func(c *DisposeCommitment) { c.Evidence = nil }, "SCHEMA_INVALID /args/evidence"},
		{func(c *DisposeCommitment) {
			c.Disposition, c.Evidence = DispositionWriteOff, []Evidence{{SourceID: "", Locator: "x"}}
		},
			"SCHEMA_INVALID /args/evidence/0/source_id"},
		{func(c *DisposeCommitment) { c.Disposition = "scrap" }, "SCHEMA_INVALID /args/disposition"},
		{func(c *DisposeCommitment) { c.DispositionID = "" }, "SCHEMA_INVALID /args/disposition_id"},
		{func(c *DisposeCommitment) { c.OrderLineID = "" }, "SCHEMA_INVALID /args/order_line_id"},
		{func(c *DisposeCommitment) { c.Quantity = "0" }, "SCHEMA_INVALID /args/quantity"},
		{func(c *DisposeCommitment) { c.Quantity = "1.2345678" }, "SCHEMA_INVALID /args/quantity"},
		{func(c *DisposeCommitment) { c.Reason = "" }, "SCHEMA_INVALID /args/reason"},
	} {
		if got := pathOf(dispose(tc.edit)); got != tc.want {
			t.Errorf("%s, want %s", got, tc.want)
		}
	}
	for _, tc := range []struct {
		c    DecideDisposition
		want string
	}{
		{DecideDisposition{IdempotencyKey: "k", DispositionID: "x", ExpectedVersion: 1, Decision: DecisionApprove}, "OK"},
		{DecideDisposition{IdempotencyKey: "k", DispositionID: "x", ExpectedVersion: 1, Decision: DecisionReject, Reason: "r"}, "OK"},
		{DecideDisposition{IdempotencyKey: "k", DispositionID: "x", ExpectedVersion: 1, Decision: DecisionReject}, "SCHEMA_INVALID /args/reason"},
		{DecideDisposition{IdempotencyKey: "k", DispositionID: "x", ExpectedVersion: 0, Decision: DecisionApprove}, "SCHEMA_INVALID /args/expected_version"},
		{DecideDisposition{IdempotencyKey: "k", DispositionID: "x", ExpectedVersion: 1, Decision: "maybe"}, "SCHEMA_INVALID /args/decision"},
	} {
		if got := pathOf(tc.c.Validate()); got != tc.want {
			t.Errorf("%s, want %s", got, tc.want)
		}
	}
}
