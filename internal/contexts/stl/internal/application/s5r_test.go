package application

import (
	"errors"
	"slices"
	"testing"
	"time"

	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5R unit R7 (simulated sign-off E0, 2026-11-13). Python V0.50 has no STL: the tests cite the decisions
// and the design.

// S5R-D02: order_account.open is STL's own permission, held by the role template treasury (and the
// consumer system:inbox.stl.order_account, adapters/events), never by a sales template or the AI
// template, and admits a person or the system, never an agent (commandActors order_account.open
// human|system). S5R-D21: fund.verify_payer is a humanOnlyCommand held by credit_manager alone.
func TestS5ROwnPermissionsOfTheNewCommands(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	holders := func(permission string) []string {
		var out []string
		for _, tpl := range authorization.Templates() {
			if slices.Contains(tpl.Permissions, permission) {
				out = append(out, tpl.Code)
			}
		}
		return out
	}
	for c, want := range map[string]struct {
		kinds   []kernel.ActorKind
		holders []string
	}{
		api.CommandOpenOrderAccount: {[]kernel.ActorKind{kernel.ActorHuman, kernel.ActorSystem}, []string{"treasury"}},
		api.CommandVerifyPayer:      {[]kernel.ActorKind{kernel.ActorHuman}, []string{"credit_manager"}},
	} {
		r, err := policy.Rule(c)
		if err != nil || r.Permission != c || len(r.ActorKinds) != len(want.kinds) || r.Admits(kernel.ActorAI) {
			t.Errorf("%s: %+v, %v", c, r, err)
			continue
		}
		for _, k := range want.kinds {
			if !r.Admits(k) {
				t.Errorf("%s does not admit %s", c, k)
			}
		}
		if got := holders(c); !slices.Equal(got, want.holders) {
			t.Errorf("%s is held by %v, want %v", c, got, want.holders)
		}
	}
	if api.PermissionOpenOrderAccount == api.PermissionOpenReview {
		t.Fatal("order_account.open borrows payment_plan_review.open again")
	}
	if name, _, _ := request(api.VerifyPayer{}); name != api.CommandVerifyPayer {
		t.Fatalf("fund.verify_payer has no name: %q", name)
	}
}

// The members R7 adds (credit.reserve exposure_fx_rates, credit.release cause, credit.consume carried_from)
// are left out of the request hash while empty, so a command stored before S5R replays under its key: its
// hash is the hash of the S5 shape.
func TestS5RRequestsKeepTheirHash(t *testing.T) {
	type reserveS5 struct {
		IdempotencyKey string
		ReservationID  string
		OrderID        string
		OrderRevision  int64
		OrderHash      string
		CreditPolicy   api.ConfigRef
		FXRate         *api.ConfigRef
	}
	type releaseS5 struct {
		IdempotencyKey  string
		ReservationID   string
		ExpectedVersion int64
		Reason          string
	}
	type consumeS5 struct {
		IdempotencyKey string
		ReservationID  string
		OrderID        string
		OrderRevision  int64
		OrderHash      string
		ActivatedAt    string
	}
	policy := api.ConfigRef{Kind: api.KindCreditPolicy, ConfigID: "credit", Revision: 1}
	hash := func(v any) string {
		h, err := action.RequestHash(v)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	_, _, reserve := request(api.ReserveCredit{IdempotencyKey: "k", ReservationID: "res.1", OrderID: "o", OrderRevision: 1, OrderHash: "h",
		CreditPolicy: policy})
	_, _, release := request(api.ReleaseCredit{IdempotencyKey: "k", ReservationID: "res.1", ExpectedVersion: 2, Reason: "r"})
	_, _, consume := request(api.ConsumeCredit{IdempotencyKey: "k", ReservationID: "res.1", OrderID: "o", OrderRevision: 1, OrderHash: "h",
		ActivatedAt: "2026-11-13T00:00:00Z"})
	for name, c := range map[string][2]string{
		"credit.reserve": {hash(reserve), hash(reserveS5{ReservationID: "res.1", OrderID: "o", OrderRevision: 1, OrderHash: "h", CreditPolicy: policy})},
		"credit.release": {hash(release), hash(releaseS5{ReservationID: "res.1", Reason: "r"})},
		"credit.consume": {hash(consume), hash(consumeS5{ReservationID: "res.1", OrderID: "o", OrderRevision: 1, OrderHash: "h",
			ActivatedAt: "2026-11-13T00:00:00Z"})},
	} {
		if c[0] != c[1] {
			t.Errorf("%s: the request hash changed for an S5 request", name)
		}
	}
	_, _, cause := request(api.ReleaseCredit{IdempotencyKey: "k", ReservationID: "res.1", ExpectedVersion: 2, Reason: "r", Cause: api.CauseSuperseded})
	if hash(cause) == hash(release) {
		t.Fatal("the cause is not part of the request")
	}
}

// S5R-D08: the reservation the consumer carries over for an OrderAmended has the same ID on every delivery
// of the event, and a different one for another event; it is an identifier.
func TestS5RCarriedIDDerivesFromTheEvent(t *testing.T) {
	a, b := CarriedID("evt-1"), CarriedID("evt-2")
	if a != CarriedID("evt-1") || a == b || !kernel.ValidIdentifier(a) || len(a) > 64 {
		t.Fatalf("%q %q", a, b)
	}
}

// S5R-D08 with D-5: what the amended revision needs on credit is its credit portion under the credit policy
// the carried reservation was made under — a document-controlled stage after shipment is secured when that
// policy says so and on credit otherwise — and its exposure is the portion at that reservation's credit rate,
// read from POL by content hash. A revision TRD does not show active is unknown; POL revisions the basis does
// not hold are retried.
func TestS5RNeedOfTheAmendedRevision(t *testing.T) {
	usd, cny := kernel.MustCurrency("USD", 2), kernel.MustCurrency("CNY", 2)
	m := func(c kernel.Currency, s string) kernel.Money {
		x, err := kernel.ParseAmount(c, s)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	b := cc.Binding{OrderID: "sim", OrderRevision: 2, OrderHash: "h2"}
	order := &Order{Binding: OrderBinding(b), Status: OrderActive, CustomerID: "customer.1", Stages: []OrderStage{
		{StageID: "P1", Trigger: cc.TriggerAfterShipment, AmountMinor: "500000"},
		{StageID: "P2", Trigger: cc.TriggerAfterShipment, ReleaseControl: cc.ReleaseDocuments, AmountMinor: "328000"}}}
	policyRef := polapi.ConfigRef{Kind: polapi.KindCreditPolicy, ConfigID: "credit", Revision: 1}
	configs := func(secured bool) map[polapi.ConfigRef]polapi.ResolvedConfig {
		return map[polapi.ConfigRef]polapi.ResolvedConfig{policyRef: {Ref: policyRef, ContentHash: "ph",
			Data: polapi.ConfigData{CreditPolicy: &polapi.CreditPolicy{Currency: "USD", DocumentsSecured: secured}}}}
	}
	from := &cc.Reservation{ID: "res.1", Binding: cc.Binding{OrderID: "sim", OrderRevision: 1, OrderHash: "h1"}, Amount: m(usd, "9200"),
		Exposure: m(usd, "9200"), Policy: cc.ConfigUse{ID: "credit", Revision: 1, Hash: "ph"}}
	p, s, e, known, err := CreditBasis{Order: order, Configs: configs(true)}.need(from, b)
	if err != nil || !known || p.Amount() != "5000.00" || s.Amount() != "3280.00" || e.Amount() != "5000.00" {
		t.Fatalf("documents secured: %v %v %v %v %v", p, s, e, known, err)
	}
	p, s, _, _, err = CreditBasis{Order: order, Configs: configs(false)}.need(from, b)
	if err != nil || p.Amount() != "8280.00" || s.Sign() != 0 {
		t.Fatalf("documents on credit: %v %v %v", p, s, err)
	}
	if _, _, _, known, err = (CreditBasis{Order: order, Configs: configs(true)}).need(from, cc.Binding{OrderID: "sim", OrderRevision: 3, OrderHash: "h3"}); known || err != nil {
		t.Fatalf("a revision TRD does not show: %v %v", known, err)
	}
	if _, _, _, _, err = (CreditBasis{Order: order}).need(from, b); !errors.Is(err, errBasisChanged) {
		t.Fatalf("a policy the basis does not hold: %v", err)
	}
	stale := configs(true)
	x := stale[policyRef]
	x.ContentHash = "other"
	stale[policyRef] = x
	if _, _, _, _, err = (CreditBasis{Order: order, Configs: stale}).need(from, b); err == nil {
		t.Fatal("a policy of another content hash")
	}
	// The order in CNY against a USD limit: the portion converts at the reservation's credit rate.
	fxRef := polapi.ConfigRef{Kind: polapi.KindFXRate, ConfigID: "fx.credit", Revision: 2}
	withRate := configs(false)
	withRate[fxRef] = polapi.ResolvedConfig{Ref: fxRef, ContentHash: "fh", Data: polapi.ConfigData{FXRate: &polapi.FXRate{FromCurrency: "CNY",
		ToCurrency: "USD", Rate: "0.142857", Purpose: polapi.PurposeCredit}}}
	cnyFrom := &cc.Reservation{ID: "res.c", Binding: from.Binding, Amount: m(cny, "9200"), Exposure: m(usd, "1314.28"),
		Policy: from.Policy, FX: &cc.ConfigUse{ID: "fx.credit", Revision: 2, Hash: "fh"}}
	if p, _, e, _, err = (CreditBasis{Order: order, Configs: withRate}).need(cnyFrom, b); err != nil || p.String() != "8280.00 CNY" ||
		e.String() != "1182.86 USD" {
		t.Fatalf("CNY at the credit rate: %v %v %v", p, e, err)
	}
}

// S5R-D08 (a) and the register's "游离占用若金额大于新修订需要，按需要量消耗、差额释放": the amended revision's own reserved
// reservation is consumed when it is for the amended order hash, still valid when the amendment took effect (or when
// it was reserved, if later), and — when an earlier reservation could be carried over instead — holds exactly what the
// revision needs; a larger (or smaller) one is released and the earlier one carried over at the need. An amendment
// ahead of the consumer's clock is retried.
func TestS5RTheAmendedRevisionsOwnReservation(t *testing.T) {
	usd := kernel.MustCurrency("USD", 2)
	m := func(s string) kernel.Money {
		x, err := kernel.ParseAmount(usd, s)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	now := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	b := cc.Binding{OrderID: "sim", OrderRevision: 2, OrderHash: "h2"}
	policyRef := polapi.ConfigRef{Kind: polapi.KindCreditPolicy, ConfigID: "credit", Revision: 1}
	basis := CreditBasis{Order: &Order{Binding: OrderBinding(b), Status: OrderActive, Stages: []OrderStage{{StageID: "P2",
		Trigger: cc.TriggerAfterShipment, AmountMinor: "828000"}}}, Configs: map[polapi.ConfigRef]polapi.ResolvedConfig{policyRef: {Ref: policyRef,
		ContentHash: "ph", Data: polapi.ConfigData{CreditPolicy: &polapi.CreditPolicy{Currency: "USD"}}}}}
	stray := cc.Reservation{ID: "res.2", Binding: b, Amount: m("8280"), Secured: m("0"), Exposure: m("8280"), Status: cc.ReservationReserved,
		Policy: cc.ConfigUse{ID: "credit", Revision: 1, Hash: "ph"}, ExpiresAt: now.Add(24 * time.Hour), Reserved: cc.Act{At: now.Add(-time.Hour)}}
	e := OrderEvent{OrderID: "sim", OrderRevision: 2, OrderHash: "h2", AppliedAt: now.Add(-time.Minute)}
	s := Service{}
	for name, c := range map[string]struct {
		mod       func(r *cc.Reservation, e *OrderEvent)
		carryable bool
		want      bool
	}{
		"the revision's need":                   {func(*cc.Reservation, *OrderEvent) {}, true, true},
		"larger than the need":                  {func(r *cc.Reservation, _ *OrderEvent) { r.Amount = m("9200") }, true, false},
		"larger, nothing earlier to carry":      {func(r *cc.Reservation, _ *OrderEvent) { r.Amount = m("9200") }, false, true},
		"another order hash":                    {func(r *cc.Reservation, _ *OrderEvent) { r.Binding.OrderHash = "hx" }, true, false},
		"expired when the amendment took place": {func(r *cc.Reservation, _ *OrderEvent) { r.ExpiresAt = now.Add(-2 * time.Minute) }, true, false},
		"reserved after the amendment":          {func(r *cc.Reservation, _ *OrderEvent) { r.Reserved.At = now.Add(-time.Second) }, true, true},
		"another order revision":                {func(_ *cc.Reservation, e *OrderEvent) { e.OrderRevision = 3 }, true, false},
	} {
		r, ev := stray, e
		c.mod(&r, &ev)
		bind := cc.Binding{OrderID: ev.OrderID, OrderRevision: ev.OrderRevision, OrderHash: ev.OrderHash}
		got, err := s.strayFits(r, ev, basis, bind, c.carryable, now)
		if err != nil || got != c.want {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
	moved := basis
	moved.Order = &Order{Binding: OrderBinding{OrderID: "sim", OrderRevision: 3, OrderHash: "h3"}, Status: OrderActive}
	if got, err := s.strayFits(stray, e, moved, b, true, now); err != nil || !got {
		t.Fatalf("TRD amended again since: the revision's own reservation is consumed (no need to compare): %v %v", got, err)
	}
	ahead := e
	ahead.AppliedAt = now.Add(time.Minute)
	if _, err := s.strayFits(stray, ahead, basis, b, true, now); err == nil {
		t.Fatal("an amendment ahead of the clock is retried")
	}
}
