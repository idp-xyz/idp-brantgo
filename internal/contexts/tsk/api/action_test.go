package api

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func attempt() RecordAttempt {
	return RecordAttempt{IdempotencyKey: "k1", TaskID: "task.1", BusinessIntentID: "intent.1",
		Target: TargetCommand{Context: TargetTRD, Command: "order.activate", RequestHash: strings.Repeat("a", 64)}, ControlEpoch: 1,
		Outcome: ActionSubmitted, DelegationID: "del.1", GrantID: "grant.1"}
}

// action.record_attempt's closed contract: a context a task step writes, a command name, a request
// hash of 64 lower-case hexadecimal digits, a known outcome, a code exactly where the outcome needs
// one, references only on a submission.
func TestRecordAttemptContract(t *testing.T) {
	ok := []func(*RecordAttempt){
		func(*RecordAttempt) {},
		func(c *RecordAttempt) { c.DelegationID, c.GrantID, c.ControlEpoch = "", "", 0 },
		func(c *RecordAttempt) {
			c.Outcome, c.Code, c.DelegationID, c.GrantID = ActionRejected, "VERSION_CONFLICT", "", ""
		},
		func(c *RecordAttempt) { c.Outcome, c.DelegationID, c.GrantID = ActionUnknown, "", "" },
		func(c *RecordAttempt) {
			c.Outcome, c.Code, c.DelegationID, c.GrantID = ActionUnknown, "COMMIT_UNCERTAIN", "", ""
		},
		func(c *RecordAttempt) { c.Target.Context, c.Target.Command = TargetCST, "cost_scenario.save_draft" },
		func(c *RecordAttempt) { c.Target.Context, c.Target.Command = TargetSUP, "supplier_offer.save_draft" },
		func(c *RecordAttempt) { c.Target.Context, c.Target.Command = TargetTRD, "quote.metadata.update" },
	}
	for i, edit := range ok {
		c := attempt()
		edit(&c)
		if err := c.Validate(); err != nil {
			t.Errorf("%d: %v", i, err)
		}
	}
	for _, tc := range []struct {
		edit func(*RecordAttempt)
		path string
	}{
		{func(c *RecordAttempt) { c.IdempotencyKey = "" }, "/idempotency_key"},
		{func(c *RecordAttempt) { c.TaskID = "task 1" }, "/args/task_id"},
		{func(c *RecordAttempt) { c.BusinessIntentID = "" }, "/args/business_intent_id"},
		{func(c *RecordAttempt) { c.Target.Context = "pur" }, "/args/target/context"},
		{func(c *RecordAttempt) { c.Target.Context = "TRD" }, "/args/target/context"},
		{func(c *RecordAttempt) { c.Target.Command = "activate" }, "/args/target/command"},
		{func(c *RecordAttempt) { c.Target.Command = "Order.activate" }, "/args/target/command"},
		{func(c *RecordAttempt) { c.Target.Command = "order." }, "/args/target/command"},
		{func(c *RecordAttempt) { c.Target.Command = "order.activate!" }, "/args/target/command"},
		{func(c *RecordAttempt) { c.Target.Command = "a." + strings.Repeat("b", MaxTargetCommand) }, "/args/target/command"},
		{func(c *RecordAttempt) { c.Target.RequestHash = strings.Repeat("A", 64) }, "/args/target/request_hash"},
		{func(c *RecordAttempt) { c.Target.RequestHash = "abc" }, "/args/target/request_hash"},
		{func(c *RecordAttempt) { c.ControlEpoch = -1 }, "/args/control_epoch"},
		{func(c *RecordAttempt) { c.Outcome = "accepted" }, "/args/outcome"},
		{func(c *RecordAttempt) { c.Outcome = "committed" }, "/args/outcome"},
		{func(c *RecordAttempt) { c.Code = "VERSION_CONFLICT" }, "/args/code"},
		{func(c *RecordAttempt) { c.Outcome, c.DelegationID, c.GrantID = ActionRejected, "", "" }, "/args/code"},
		{func(c *RecordAttempt) {
			c.Outcome, c.Code, c.DelegationID, c.GrantID = ActionRejected, "version conflict", "", ""
		}, "/args/code"},
		{func(c *RecordAttempt) { c.Outcome, c.Code, c.GrantID = ActionUnknown, "", "" }, "/args/delegation_id"},
		{func(c *RecordAttempt) { c.Outcome, c.Code, c.DelegationID = ActionRejected, "X_Y", "" }, "/args/grant_id"},
		{func(c *RecordAttempt) { c.DelegationID = "del 1" }, "/args/delegation_id"},
		{func(c *RecordAttempt) { c.GrantID = "grant 1" }, "/args/grant_id"},
	} {
		c := attempt()
		tc.edit(&c)
		rejected(t, c.Validate(), kernel.CodeSchemaInvalid, tc.path)
	}
}

func TestReconcileActionContract(t *testing.T) {
	if err := (ReconcileAction{IdempotencyKey: "k", TaskID: "task.1", ActionID: ActionIDOf("intent.1")}).Validate(); err != nil {
		t.Fatal(err)
	}
	rejected(t, ReconcileAction{IdempotencyKey: "k", TaskID: "task.1"}.Validate(), kernel.CodeSchemaInvalid, "/args/action_id")
	rejected(t, ReconcileAction{TaskID: "task.1", ActionID: "a"}.Validate(), kernel.CodeSchemaInvalid, "/idempotency_key")
	rejected(t, ReconcileAction{IdempotencyKey: "k", ActionID: "a"}.Validate(), kernel.CodeSchemaInvalid, "/args/task_id")
}

func TestNamesAndCodes(t *testing.T) {
	for _, s := range []string{"source.register", "quote.metadata.update", "order.review.decide", "cost_snapshot.freeze", "a.b"} {
		if !CommandName(s) {
			t.Errorf("%q", s)
		}
	}
	for _, s := range []string{"", "quote", ".x", "quote.", "Quote.x", "quote.X", "quote_.1x", "1q.x", "q.x-y", "q..x"} {
		if CommandName(s) {
			t.Errorf("%q accepted", s)
		}
	}
	for _, s := range []string{"VERSION_CONFLICT", "COMMIT_UNCERTAIN", "X1"} {
		if !ErrorCode(s) {
			t.Errorf("%q", s)
		}
	}
	for _, s := range []string{"", "X", "version_conflict", "1X", "X-Y", "X " + strings.Repeat("Y", 70)} {
		if ErrorCode(s) {
			t.Errorf("%q accepted", s)
		}
	}
	id := ActionIDOf("intent.1")
	if !kernel.ValidIdentifier(id) || id == ActionIDOf("intent.2") || id != ActionIDOf("intent.1") {
		t.Fatalf("%q", id)
	}
}
