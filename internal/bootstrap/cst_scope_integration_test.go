//go:build integration

package bootstrap_test

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Python test_costing.py test_cross_company_cannot_read_or_replay_same_named_objects, on a costing
// context: the same request in another company is a new command on a new object, never a replay
// of the first company's action, and neither company reads the other's object.
func TestCrossCompanyCannotReadOrReplaySameNamedContexts(t *testing.T) {
	c := newCostingWorld(t)
	request := cstapi.SaveContext{IdempotencyKey: "same.key", ContextID: "same", Data: cstapi.ContextData{}}
	first, err := c.c.CST.SaveContext(c.author, request)
	c.must(err)
	outsider := c.provision(bootstrap.Grant{Scope: c.scope("company.B"), Actor: actorRef("human", "author"), Permissions: allPermissions,
		DataScope: "company"})
	_, err = c.c.CST.GetContext(outsider, "same")
	c.want(err, kernel.CodeNotFound)
	other, err := c.c.CST.SaveContext(outsider, request)
	c.must(err)
	if other.Replayed || other.ActionID == first.ActionID || other.RowVersion != 1 {
		t.Fatalf("first %+v, other %+v", first, other)
	}
	again, err := c.c.CST.SaveContext(c.author, request)
	if err != nil || !again.Replayed || again.ActionID != first.ActionID {
		t.Fatalf("replay in the first company %+v, %v", again, err)
	}
	rec, err := c.c.CST.Reconcile(outsider, request)
	if err != nil || rec.State != kernel.ReconcileCommitted || rec.ActionID != other.ActionID {
		t.Fatalf("reconcile in company.B %+v, %v", rec, err)
	}
}

// Python test_costing.py test_authorization_and_missing_history_rejections_are_audited, on the
// costing context read: a reader without cst.read and a reader of a missing context are both
// refused, and both refusals are in the audit trail.
func TestContextReadRejectionsAreAudited(t *testing.T) {
	c := newCostingWorld(t)
	reader := c.provision(bootstrap.Grant{Scope: c.scope("company.A"), Actor: actorRef("human", "reader"), Permissions: []string{"inq.read"},
		DataScope: "company"})
	_, err := c.c.CST.GetContext(reader, "missing")
	c.want(err, kernel.CodePermissionDenied)
	_, err = c.c.CST.GetContext(c.checker, "missing")
	c.want(err, kernel.CodeNotFound)
	_, err = c.c.CST.GetMeasurement(c.checker, "missing")
	c.want(err, kernel.CodeNotFound)
	audit, err := c.c.CST.Audit(c.checker)
	c.must(err)
	var denied []string
	for _, e := range audit {
		if e.Outcome == "rejected" {
			denied = append(denied, e.Command+" "+e.Code+" "+e.Actor.ID)
		}
	}
	want := []string{"costing_context.get PERMISSION_DENIED reader", "costing_context.get NOT_FOUND checker", "measurement.get NOT_FOUND checker"}
	if len(denied) != len(want) {
		t.Fatalf("audited rejections %v", denied)
	}
	for i := range want {
		if denied[i] != want[i] {
			t.Fatalf("audited rejections %v, want %v", denied, want)
		}
	}
	// A refused command is audited too, before any business check.
	_, err = c.c.CST.VerifyContext(c.checker, cstapi.VerifyContext{IdempotencyKey: c.key(), ContextID: "missing", ExpectedVersion: 1,
		WorkingRevision: 1, Evidence: proof})
	c.want(err, kernel.CodeNotFound)
	_, err = c.c.CST.SaveContext(c.author, cstapi.SaveContext{IdempotencyKey: c.key(), ContextID: "bad id"})
	c.want(err, kernel.CodeSchemaInvalid)
	audit, err = c.c.CST.Audit(c.checker)
	c.must(err)
	if n := len(audit); n < 5 || audit[n-2].Command != cstapi.CommandVerifyContext || audit[n-2].Code != kernel.CodeNotFound ||
		audit[n-1].Command != cstapi.CommandSaveContext || audit[n-1].Code != kernel.CodeSchemaInvalid || audit[n-1].ObjectID != "" {
		t.Fatalf("audit %+v", audit)
	}
}
