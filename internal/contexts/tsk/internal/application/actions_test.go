package application

import (
	"context"
	"slices"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	actions "go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/action"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The contexts TSK reconciles task steps with are exactly those the Context Map makes upstream of
// TSK with ActionReconciliation v1 (R18 TRD, R66 EVD, R67 INQ, R68 SUP, R69 CST; D-11), and
// ActionReconciled is a TSK fact.
func TestReconcilersFollowTheContextMap(t *testing.T) {
	var cmap struct {
		Contexts []struct {
			ID    string   `json:"id"`
			Facts []string `json:"facts"`
		} `json:"contexts"`
		Relationships []struct {
			ID       string `json:"id"`
			From     string `json:"frm"`
			To       string `json:"to"`
			Contract string `json:"contract"`
		} `json:"relationships"`
	}
	load(t, root("docs", "03_STRATEGIC_DDD", "01_IDP_Brant_Strategic_Context_Map_V0.1_Draft.json"), &cmap)
	var upstream []string
	for _, r := range cmap.Relationships {
		if r.To == "TSK" && strings.Contains(r.Contract, "ActionReconciliation v1") {
			upstream = append(upstream, strings.ToLower(r.From))
		}
	}
	got := slices.Clone(actions.Contexts)
	slices.Sort(got)
	slices.Sort(upstream)
	if !slices.Equal(got, upstream) {
		t.Fatalf("TSK reconciles with %v, the Context Map names %v", got, upstream)
	}
	for _, c := range cmap.Contexts {
		if c.ID == "TSK" && !slices.Contains(c.Facts, "ActionReconciled") {
			t.Fatalf("TSK facts %v", c.Facts)
		}
	}
	if api.EventActionReconciled != "idp-brant.tsk.action_reconciled.v1" {
		t.Fatal(api.EventActionReconciled)
	}
}

// The action ID TSK derives is the one its api publishes (ActionIDOf), and the key of the target
// command.
func TestActionIDIsPublished(t *testing.T) {
	for _, intent := range []string{"intent.1", "q01.task.1.order.activate", strings.Repeat("x", 64)} {
		if actions.ID(intent) != api.ActionIDOf(intent) {
			t.Fatalf("%s: %s, api %s", intent, actions.ID(intent), api.ActionIDOf(intent))
		}
	}
}

// ActionExecution's commands are TSK commands: the request hash covers everything but the
// idempotency key (an observation is another request than the submission), and the ledger knows
// their names; the reconciliation of TSK's own commands has its own audit name.
func TestActionRequests(t *testing.T) {
	a := api.RecordAttempt{IdempotencyKey: "k1", TaskID: "task.1", BusinessIntentID: "intent.1",
		Target: api.TargetCommand{Context: api.TargetINQ, Command: "rfq.save_draft", RequestHash: strings.Repeat("c", 64)}, Outcome: api.ActionSubmitted}
	b := a
	b.IdempotencyKey = "k2"
	if requestHash(a) != requestHash(b) {
		t.Fatal("the key changed the hash")
	}
	b.Outcome = api.ActionUnknown
	if requestHash(a) == requestHash(b) {
		t.Fatal("the outcome did not change the hash")
	}
	for _, c := range []api.Command{a, api.ReconcileAction{IdempotencyKey: "k", TaskID: "t", ActionID: "a"}} {
		if name, key, _ := request(c); name == "" || key == "" {
			t.Errorf("%T: %q %q", c, name, key)
		}
	}
	if api.ReconcileAudit == api.CommandReconcileAction {
		t.Fatal("reconciliations of TSK's commands are audited under the command's name")
	}
}

// A target's refusal of the request's form lands at the member of TSK's request that carried it;
// other refusals and errors pass on unchanged.
func TestTargetRejections(t *testing.T) {
	for in, want := range map[string]string{"/args/command": "/args/target/command", "/command": "/args/target/command",
		"/args/request_hash": "/args/target/request_hash", "/args/idempotency_key": "/args/idempotency_key"} {
		err := targetRejection(kernel.Reject(kernel.CodeSchemaInvalid, in, ""))
		if rej, ok := kernel.AsRejection(err); !ok || rej.Fields[0].Path != want {
			t.Errorf("%s: %v", in, err)
		}
	}
	denied := kernel.Reject(kernel.CodePermissionDenied, "/", "")
	if rej, ok := kernel.AsRejection(targetRejection(denied)); !ok || rej != denied {
		t.Fatal("PERMISSION_DENIED changed")
	}
}

type nopReconciler struct{}

func (nopReconciler) ReconcileHash(context.Context, string, string, string) (Answer, error) {
	return Answer{State: kernel.ReconcileNotCommitted}, nil
}

// The module needs a reconciler for every context a task step writes.
func TestUpstreamNeedsEveryReconciler(t *testing.T) {
	all := Reconcilers{}
	for _, c := range actions.Contexts {
		all[c] = nopReconciler{}
	}
	u := Upstream{Control: nil, Readiness: ReadinessNotWired{}, Reconcilers: all}
	if u.Complete() {
		t.Fatal("complete without the object control")
	}
	if !all.Complete() {
		t.Fatal("five reconcilers are not complete")
	}
	for _, c := range actions.Contexts {
		some := Reconcilers{}
		for k, v := range all {
			if k != c {
				some[k] = v
			}
		}
		if some.Complete() {
			t.Fatalf("complete without %s", c)
		}
	}
	if _, err := (Service{Upstream: Upstream{Reconcilers: Reconcilers{}}}).ask(context.Background(), actions.Target{Context: "trd"}, "k"); err == nil {
		t.Fatal("an unwired context answered")
	}
}
