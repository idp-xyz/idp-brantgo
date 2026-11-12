package application

import (
	"maps"
	"testing"

	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	fr "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The api declares exactly the codes the rules give, with the same categories.
func TestAPICodesAreTheRulesCodes(t *testing.T) {
	rules := maps.Clone(cc.Codes)
	for c, cat := range settlement.Codes {
		rules[c] = cat
	}
	for c, cat := range fr.Codes {
		if have, ok := rules[c]; ok && have != cat {
			t.Fatalf("%s is %v in customer credit and %v in funds", c, have, cat)
		}
		rules[c] = cat
	}
	if !maps.Equal(rules, api.Codes()) {
		t.Fatalf("rules %v\napi %v", rules, api.Codes())
	}
}

// The tactical model's humanOnlyCommands name credit_limit.publish and credit_exception.decide;
// credit.consume is the system's (SM-STL-CREDIT-RESERVATION); the command catalog admits humans and
// AI to request limits and to reserve and release credit, and the state machine the system.
func TestActorKindsOfTheCreditCommands(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	for c, kinds := range map[string][]kernel.ActorKind{
		api.CommandPublishLimit:    {kernel.ActorHuman},
		api.CommandDecideException: {kernel.ActorHuman},
		api.CommandConsume:         {kernel.ActorSystem},
		api.CommandRequestLimit:    {kernel.ActorHuman, kernel.ActorAI},
		api.CommandReserve:         {kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem},
		api.CommandRelease:         {kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem},
	} {
		r, err := policy.Rule(c)
		if err != nil || r.Permission != c || len(r.ActorKinds) != len(kinds) {
			t.Errorf("%s: %+v, %v", c, r, err)
			continue
		}
		for _, k := range kinds {
			if !r.Admits(k) {
				t.Errorf("%s does not admit %s", c, k)
			}
		}
	}
	for _, c := range []string{api.CommandGetCredit, api.CommandGetReservation, api.CommandGetDecision} {
		if r, err := policy.Rule(c); err != nil || r.Permission != api.PermissionRead {
			t.Errorf("%s: %+v, %v", c, r, err)
		}
	}
	// Every permission a rule names is held by a role template (BC-D02).
	held := map[string]bool{}
	for _, tpl := range authorization.Templates() {
		for _, p := range tpl.Permissions {
			held[p] = true
		}
	}
	for _, r := range Rules() {
		if !held[r.Permission] {
			t.Errorf("no role template holds %s", r.Permission)
		}
	}
}

// The request hash covers the business arguments, not the key or the CAS value.
func TestRequestHashIgnoresKeyAndVersion(t *testing.T) {
	a := api.DecideCreditException{IdempotencyKey: "k1", DecisionID: "d", ExpectedVersion: 1, Decision: api.DecisionRelease, Reason: "r"}
	b := a
	b.IdempotencyKey, b.ExpectedVersion = "k2", 2
	_, _, x := request(a)
	_, _, y := request(b)
	if x != y {
		t.Fatal("hash args differ")
	}
	for _, c := range []api.Command{api.RequestCreditLimit{}, api.PublishCreditLimit{}, api.ReserveCredit{}, api.ReleaseCredit{}, api.ConsumeCredit{}} {
		if name, _, _ := request(c); name == "" {
			t.Errorf("%T has no name", c)
		}
	}
	if name, _, _ := request(nil); name != "" {
		t.Fatal("unknown command")
	}
}

// A credit_policy revision as STL reads it: E-2's tiers in the policy currency's minor units and the
// S5 members (POL A3a); a revision saved before S5 states no validities, and STL fails closed.
func TestPolicyOf(t *testing.T) {
	usd := kernel.MustCurrency("USD", 2)
	rc := polapi.ResolvedConfig{Ref: polapi.ConfigRef{Kind: polapi.KindCreditPolicy, ConfigID: "credit.usd", Revision: 3}, ContentHash: "h",
		Data: polapi.ConfigData{CreditPolicy: &polapi.CreditPolicy{Name: "E-2", Currency: "USD", DefaultLimit: "0",
			LimitSources: []string{polapi.LimitSourceInternalAssessment}, OverdueToleranceDays: 30, DocumentsSecured: true,
			ReservationValidityDays: 30, DecisionValidityDays: 7, TwoApproversAboveTop: true,
			ApprovalTiers: []polapi.CreditApprovalTier{{Level: "credit_officer", MaxAmount: "1000"}, {Level: "credit_manager", MaxAmount: "5000.00"}}}}}
	p, err := policyOf(rc, usd)
	if err != nil || p.Use.Revision != 3 || p.Top().Level != "credit_manager" || p.Top().Max.MinorString() != "500000" || !p.DocumentsSecured ||
		p.ReservationValidityDays != 30 || p.DecisionValidityDays != 7 || !p.TwoApproversAboveTop || p.OverdueToleranceDays != 30 {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := policyOf(rc, kernel.MustCurrency("CNY", 2)); err == nil {
		t.Fatal("another currency")
	}
	unset := *rc.Data.CreditPolicy
	unset.ReservationValidityDays, unset.DecisionValidityDays = 0, 0
	old := rc
	old.Data.CreditPolicy = &unset
	if _, err := policyOf(old, usd); err == nil {
		t.Fatal("a policy without validities")
	}
	rc.Data.CreditPolicy.ApprovalTiers[0].MaxAmount = "1000.123"
	if _, err := policyOf(rc, usd); err == nil {
		t.Fatal("more digits than USD")
	}
}

// C8: credit_note.request is the sales clerk's, by a person or AI (S5-18 "销售经办; human|ai");
// credit.confirm and settlement.reverse are humanOnlyCommands held by the credit manager; credit_note.get
// is a read. Their request hashes leave the key and the expected version out.
func TestActorKindsOfCreditNotesAndReversal(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	for c, kinds := range map[string][]kernel.ActorKind{
		api.CommandRequestCreditNote: {kernel.ActorHuman, kernel.ActorAI},
		api.CommandConfirmCredit:     {kernel.ActorHuman},
		api.CommandReverseSettlement: {kernel.ActorHuman},
	} {
		r, err := policy.Rule(c)
		if err != nil || r.Permission != c || len(r.ActorKinds) != len(kinds) {
			t.Errorf("%s: %+v, %v", c, r, err)
			continue
		}
		for _, k := range kinds {
			if !r.Admits(k) {
				t.Errorf("%s does not admit %s", c, k)
			}
		}
	}
	if r, err := policy.Rule(api.CommandGetCreditNote); err != nil || r.Permission != api.PermissionRead {
		t.Errorf("credit_note.get: %+v, %v", r, err)
	}
	a := api.ConfirmCredit{IdempotencyKey: "k1", CreditNoteID: "cn.1", ExpectedVersion: 1, Decision: api.DecisionConfirm, Reason: "r"}
	b := a
	b.IdempotencyKey, b.ExpectedVersion = "k2", 3
	if _, _, x := request(a); x != func() any { _, _, y := request(b); return y }() {
		t.Fatal("confirm hash args differ")
	}
	for _, c := range []api.Command{api.RequestCreditNote{}, api.ConfirmCredit{}, api.ReverseSettlement{}} {
		if name, _, _ := request(c); name == "" {
			t.Errorf("%T has no name", c)
		}
	}
}

// The analysis basis of a conversion (STL-INV-007, baseline 4.5): TRD's AnalysisFX when the analysis currency is
// the fund's (its rate) or the order's own (exactly 1); none for a third currency, another target currency, no
// AnalysisFX or an unreadable rate.
func TestBasisOfAConversion(t *testing.T) {
	usd, cny, eur := kernel.MustCurrency("USD", 2), kernel.MustCurrency("CNY", 2), kernel.MustCurrency("EUR", 2)
	o := &Order{Analysis: &OrderAnalysis{OrderCurrency: "USD", Currency: "CNY", Rate: "7.0"}}
	if b := basisOf(o, cny, usd); b == nil || b.Currency != cny || b.Rate.String() != "7" {
		t.Fatalf("in the fund's currency %+v", b)
	}
	own := &Order{Analysis: &OrderAnalysis{OrderCurrency: "USD", Currency: "USD"}}
	if b := basisOf(own, cny, usd); b == nil || b.Currency != usd || b.Rate.String() != "1" {
		t.Fatalf("in the order's currency %+v", b)
	}
	for name, c := range map[string]struct {
		o              *Order
		source, target kernel.Currency
	}{
		"third currency": {o, eur, usd},
		"other target":   {o, cny, eur},
		"no analysis":    {&Order{}, cny, usd},
		"no order":       {nil, cny, usd},
		"bad rate":       {&Order{Analysis: &OrderAnalysis{OrderCurrency: "USD", Currency: "CNY", Rate: "7,0"}}, cny, usd},
	} {
		if b := basisOf(c.o, c.source, c.target); b != nil {
			t.Errorf("%s: %+v", name, b)
		}
	}
}
