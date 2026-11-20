package execution

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/codes"
	"go.idp.xyz/idp-brant/internal/ai/gateway"
	"go.idp.xyz/idp-brant/internal/ai/proposal"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// What a send came to decides what becomes of its proposal (S4 G7b): committed is sent; a target that
// did not answer, or any error that is no rejection, gives the claim back so the same request is sent
// again with the same key; a rejection after the admission is the owning context's refusal; a
// rejection before it (delegation, grant, task) voids the proposal, whose grant was not used.
func TestOutcomeOfASend(t *testing.T) {
	rcpt := Receipt{ActionID: "act.1"}
	conflict := kernel.Reject(kernel.CodeInvalidKnownValue, "/args/notify_party_ref", "")
	for name, c := range map[string]struct {
		err      error
		admitted bool
		want     proposal.Outcome
		release  bool
	}{
		"committed":                          {nil, true, proposal.Outcome{Status: proposal.StatusSent, ActionID: "act.1"}, false},
		"refused by the owning context":      {conflict, true, proposal.Outcome{Status: proposal.StatusRefused, Code: kernel.CodeInvalidKnownValue, Path: "/args/notify_party_ref"}, false},
		"refused before the admission":       {kernel.Reject(kernel.CodePermissionDenied, "/delegation", ""), false, proposal.Outcome{Status: proposal.StatusVoid, Code: kernel.CodePermissionDenied, Path: "/delegation"}, false},
		"the grant expired":                  {codes.Reject(codes.GrantExpired, "/grant"), false, proposal.Outcome{Status: proposal.StatusVoid, Code: codes.GrantExpired, Path: "/grant"}, false},
		"a refusal at the root":              {kernel.Reject(kernel.CodeVersionConflict, "/", ""), true, proposal.Outcome{Status: proposal.StatusRefused, Code: kernel.CodeVersionConflict, Path: "/"}, false},
		"the target did not answer":          {model.ErrUnavailable, true, proposal.Outcome{}, true},
		"the target did not answer early":    {fmt.Errorf("send: %w", model.ErrUnavailable), false, proposal.Outcome{}, true},
		"an error that is no rejection":      {errors.New("connection reset"), true, proposal.Outcome{}, true},
		"a rejection that wraps unavailable": {errors.Join(model.ErrUnavailable, conflict), true, proposal.Outcome{}, true},
	} {
		got, release := outcomeOf(rcpt, c.err, c.admitted)
		if got != c.want || release != c.release {
			t.Errorf("%s: %+v release %v, want %+v release %v", name, got, release, c.want, c.release)
		}
	}
}

// codeAndPath is the code and the request member a refusal points at, only where the path is one the
// ledger keeps; a proposal that no longer decodes is SCHEMA_INVALID at its arguments.
func TestCodeAndPathOfARefusal(t *testing.T) {
	for name, c := range map[string]struct {
		err        error
		code, path string
	}{
		"a refusal with a path":           {kernel.Reject("INVALID_KNOWN_VALUE", "/args/notify_party_ref", ""), "INVALID_KNOWN_VALUE", "/args/notify_party_ref"},
		"a refusal at the root":           {kernel.Reject(kernel.CodeVersionConflict, "/", ""), kernel.CodeVersionConflict, "/"},
		"a path the ledger does not keep": {kernel.Reject(kernel.CodeSchemaInvalid, "/args/a b", ""), kernel.CodeSchemaInvalid, ""},
		"a proposal that changed":         {gateway.ErrProposalChanged, kernel.CodeSchemaInvalid, "/args"},
		"another error":                   {errors.New("boom"), "INTERNAL", ""},
	} {
		if code, path := codeAndPath(c.err); code != c.code || path != c.path {
			t.Errorf("%s: %s at %q, want %s at %q", name, code, path, c.code, c.path)
		}
	}
}

// The grant of a request binds command, hash, object and version; a send's key is the intent's, so a
// grant a send of this request used is accepted again only when the proposal was sent before.
func TestMatchOfACall(t *testing.T) {
	tr := &turn{view: tskapi.TaskView{TaskID: "task.1"}}
	hash := strings.Repeat("a", 64)
	call := gateway.Call{Capability: capability.Capability{Spec: capability.Spec{Command: "order.prepare"}}, RequestHash: hash,
		Object: capability.ObjectRef{Type: "trd.quote", ID: "quote.1", Version: 3}}
	m := tr.matchOf(call, false)
	if m.Command != "order.prepare" || m.RequestHash != hash || m.ObjectType != "trd.quote" || m.ObjectID != "quote.1" || m.ObjectVersion != 3 || m.ResendKey != "" {
		t.Fatalf("match %+v", m)
	}
	m = tr.matchOf(call, true)
	if want := tskapi.ActionIDOf(IntentOf("task.1", "order.prepare", "quote.1", hash)); m.ResendKey != want || want == "" {
		t.Fatalf("resend key %q, want %q", m.ResendKey, want)
	}
}
