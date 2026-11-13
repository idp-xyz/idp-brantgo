package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	tcc "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/tradecompliancecheck"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The domain decides with codes the api declares, with the same categories; the api's states,
// conclusions, causes and outcomes are the domain's.
func TestDomainCodesAndNamesAreTheContracts(t *testing.T) {
	for code, category := range tcc.Categories {
		if got, ok := api.Category(code); !ok || got != category {
			t.Errorf("%s: api says %q (declared %v), domain says %q", code, got, ok, category)
		}
	}
	for domain, contract := range map[string]string{string(tcc.Pending): api.StatusPending, string(tcc.Clear): api.StatusClear,
		string(tcc.Blocked): api.StatusBlocked, string(tcc.Clear) + "c": api.ConclusionClear + "c", string(tcc.Blocked) + "c": api.ConclusionBlocked + "c",
		tcc.CauseRegulationRevised: api.CauseRegulationRevised, tcc.CauseExpired: api.CauseExpired, tcc.CauseOrderAmended: api.CauseOrderAmended,
		tcc.CauseProgrammeUpdated: api.CauseProgrammeUpdated,
		string(tcc.NoMatch):       api.OutcomeNoMatch, string(tcc.Match): api.OutcomeMatch, string(tcc.Incomplete): api.OutcomeIncomplete} {
		if domain != contract {
			t.Errorf("domain %q, api %q", domain, contract)
		}
	}
}

// humanOnlyCommands names trade_compliance.decide; SM-CNX-COMPLIANCE gives evaluate to human|ai
// (and the S5 plan to the system) and reopen to the system, which compliance also holds: an AI
// agent never reopens. Reads need cnx.read, the trail cnx.audit; each command needs its own name
// as permission (role templates: compliance).
func TestRules(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	for command, kinds := range map[string][]kernel.ActorKind{
		api.CommandEvaluate: {kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem},
		api.CommandDecide:   {kernel.ActorHuman},
		api.CommandReopen:   {kernel.ActorHuman, kernel.ActorSystem},
	} {
		r, err := policy.Rule(command)
		if err != nil || r.Permission != command || len(r.ActorKinds) != len(kinds) {
			t.Fatalf("%s: %+v %v", command, r, err)
		}
		for _, k := range kinds {
			if !r.Admits(k) {
				t.Errorf("%s does not admit %s", command, k)
			}
		}
	}
	for command, permission := range map[string]string{api.CommandGetCheck: api.PermissionRead, api.CommandOrderChecks: api.PermissionRead,
		api.CommandChecksByRegulation: api.PermissionRead, api.CommandChecksByList: api.PermissionRead, api.CommandAudit: api.PermissionAudit} {
		if r, err := policy.Rule(command); err != nil || r.Permission != permission || !r.Admits(kernel.ActorAI) {
			t.Errorf("%s: %+v %v", command, r, err)
		}
	}
}

// The request hash covers the business arguments, not the key or the CAS value.
func TestRequestHashIgnoresKeyAndVersion(t *testing.T) {
	yes := true
	for _, pair := range [][2]api.Command{
		{api.Evaluate{IdempotencyKey: "k1", OrderID: "o", OrderRevision: 1, ExpectedVersion: 0}, api.Evaluate{IdempotencyKey: "k2", OrderID: "o", OrderRevision: 1, ExpectedVersion: 3}},
		{api.Decide{IdempotencyKey: "k1", CheckID: "c", ExpectedVersion: 1, LicenseRequired: &yes}, api.Decide{IdempotencyKey: "k2", CheckID: "c", ExpectedVersion: 2, LicenseRequired: &yes}},
		{api.Reopen{IdempotencyKey: "k1", Cause: api.CauseExpired, CheckID: "c", ExpectedVersion: 2}, api.Reopen{IdempotencyKey: "k9", Cause: api.CauseExpired, CheckID: "c", ExpectedVersion: 5}},
	} {
		n1, k1, a := request(pair[0])
		n2, k2, b := request(pair[1])
		ha, errA := action.RequestHash(a)
		hb, errB := action.RequestHash(b)
		if n1 != n2 || k1 == k2 || errA != nil || errB != nil || ha != hb {
			t.Errorf("%T: %v %v", pair[0], a, b)
		}
	}
	if name, _, _ := request(nil); name != "" {
		t.Fatal("unknown command")
	}
	// The code table version is a business argument: another table is another request.
	_, _, a := request(api.Evaluate{IdempotencyKey: "k1", OrderID: "o", OrderRevision: 1, CodeTableVersion: "CN-2026"})
	_, _, b := request(api.Evaluate{IdempotencyKey: "k1", OrderID: "o", OrderRevision: 1, CodeTableVersion: "CN-2027"})
	ha, _ := action.RequestHash(a)
	hb, _ := action.RequestHash(b)
	if ha == hb {
		t.Fatal("the request hash covers code_table_version")
	}
}

// A regulation revision is a screening_regulation POL resolved in force.
func TestRegulationOf(t *testing.T) {
	reg := &polapi.ScreeningRegulation{RescreenIntervalDays: 30, Lists: []polapi.ScreeningList{{ListCode: "cn.embargo", Jurisdiction: "CN"},
		{ListCode: "un.sc", Jurisdiction: "UN"}}}
	ref := polapi.ConfigRef{Kind: polapi.KindScreeningRegulation, ConfigID: "screening", Revision: 1}
	r := &polapi.ResolvedConfig{Ref: ref, Data: polapi.ConfigData{ScreeningRegulation: reg}}
	if got, err := regulationOf(r, "/args/regulation_id"); err != nil || got != reg {
		t.Fatal(err)
	}
	expired := *r
	expired.Code = polapi.CodeConfigExpired
	for _, c := range []struct {
		r    *polapi.ResolvedConfig
		want string
	}{
		{nil, kernel.CodeReferenceInvalid},
		{&polapi.ResolvedConfig{Ref: polapi.ConfigRef{Kind: polapi.KindCreditPolicy, ConfigID: "c", Revision: 1}}, kernel.CodeReferenceInvalid},
		{&expired, polapi.CodeConfigExpired},
	} {
		_, err := regulationOf(c.r, "/args/regulation_id")
		if rej, ok := kernel.AsRejection(err); !ok || rej.Code != c.want {
			t.Errorf("%+v: %v", c.r, err)
		}
	}
	if (Upstream{}).Complete() {
		t.Fatal("an empty upstream is incomplete")
	}
}
