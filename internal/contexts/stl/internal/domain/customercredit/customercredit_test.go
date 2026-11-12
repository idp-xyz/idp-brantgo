package customercredit

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Python V0.50 has no STL: these tests cite the design (tactical model, S5 plan D-4 to D-7, owner
// decision E-2), never a Python vector.

var (
	usd = kernel.MustCurrency("USD", 2)
	cny = kernel.MustCurrency("CNY", 2)
	t0  = time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
)

func m(s string) kernel.Money { return mc(usd, s) }

func mc(c kernel.Currency, s string) kernel.Money {
	x, err := kernel.ParseAmount(c, s)
	if err != nil {
		panic(err)
	}
	return x
}

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

// e2 is the owner's synthetic credit policy (decision E-2): USD, default limit 0, tiers
// credit_officer 1,000 and credit_manager 5,000, two approvers above, 30 days overdue tolerance,
// document-controlled payments secured, reservations valid 30 days, decisions 7 days.
func e2() Policy {
	return Policy{Use: ConfigUse{ID: "credit", Revision: 1, Hash: "h"}, Currency: usd, DefaultLimit: m("0"),
		LimitSources: []string{SourceInternalAssessment}, Tiers: []Tier{{"credit_officer", m("1000")}, {"credit_manager", m("5000")}},
		OverdueToleranceDays: 30, DocumentsSecured: true, ReservationValidityDays: 30, DecisionValidityDays: 7, TwoApproversAboveTop: true}
}

var (
	officer  = Approver{Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "officer.1"}, Templates: []string{"credit_officer"}}
	manager1 = Approver{Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "manager.1"}, Templates: []string{"credit_manager"}}
	manager2 = Approver{Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "manager.2"}, Templates: []string{"credit_manager", "treasury"}}
	sales    = Approver{Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "sales.1"}, Templates: []string{"sales_clerk"}}
	system   = kernel.ActorRef{Kind: kernel.ActorSystem, ID: "system:inbox.stl"}
)

// TRD-INV-022, WF-08 and D-5: the credit portion is what falls due after shipment or invoice; a
// document-controlled stage is secured when the policy says so. BRC-001's 30 % on order
// confirmation and 70 % 30 days after shipment of 960.00 reserve 672.00 (S5 plan §5 step 3).
func TestPortionIsWhatFallsDueAfterShipment(t *testing.T) {
	brc := []Stage{{StageID: "P1", Trigger: TriggerOrderConfirmed, Amount: m("288.00")}, {StageID: "P2", Trigger: TriggerAfterShipment, Amount: m("672.00")}}
	p, s, err := Portion(usd, brc, true)
	if err != nil || p.MinorString() != "67200" || !s.IsZero() {
		t.Fatalf("BRC-001: %s %s %v", p, s, err)
	}
	mixed := []Stage{{StageID: "A", Trigger: TriggerBeforeShipment, Amount: m("100")}, {StageID: "B", Trigger: TriggerAfterInvoice, Amount: m("200")},
		{StageID: "C", Trigger: TriggerAfterShipment, ReleaseControl: ReleaseDocuments, Amount: m("300")}}
	if p, s, _ := Portion(usd, mixed, true); p.Amount() != "200.00" || s.Amount() != "300.00" {
		t.Fatalf("documents secured: %s %s", p, s)
	}
	if p, s, _ := Portion(usd, mixed, false); p.Amount() != "500.00" || !s.IsZero() {
		t.Fatalf("documents not secured: %s %s", p, s)
	}
	if _, _, err := Portion(usd, []Stage{{StageID: "X", Trigger: TriggerAfterShipment, Amount: mc(cny, "1")}}, true); err == nil {
		t.Fatal("a stage in another currency")
	}
}

// STL-INV-004 with the exposure of D-4: open receivables + Σ max(0, live reservation − receivables
// formed for its order) + this portion ≤ limit; the earliest overdue receivable within tolerance.
func TestAssessAppliesTheLimitAndTheOverdueTolerance(t *testing.T) {
	limit := m("10000")
	held := func(order string, rev int64, amount string) Held {
		return Held{OrderID: order, OrderRevision: rev, Exposure: m(amount)}
	}
	for _, c := range []struct {
		name               string
		a                  Assessment
		exposure, excess   string
		reasons            []string
		overdueDays, cover string
	}{
		// STL-INV-004 positive: limit 10,000, open 5,000, reservation 4,000 succeeds.
		{"positive", Assessment{Limit: limit, Receivables: Receivables{Open: m("5000")}, OrderID: "o.new", Portion: m("4000")}, "5000.00", "0.00", nil, "", "0.00"},
		// TD-AT11: open 7,000, portion 4,000 → decision CREDIT_LIMIT_EXCEEDED, excess 1,000.
		{"TD-AT11", Assessment{Limit: limit, Receivables: Receivables{Open: m("7000")}, OrderID: "o.new", Portion: m("4000")}, "7000.00", "1000.00",
			[]string{CodeCreditLimitExceeded}, "", "1000.00"},
		// The same 7,000 held as another order's reservation counts the same.
		{"reserved elsewhere", Assessment{Limit: limit, Held: []Held{held("o.1", 1, "7000")}, OrderID: "o.new", Portion: m("4000")}, "7000.00",
			"1000.00", []string{CodeCreditLimitExceeded}, "", "1000.00"},
		// D-4, S5 plan §5 step 19: the consumed 672.00 of an order whose 960.00 receivable formed
		// counts max(0, 672 − 960) = 0 beside the open 672.00 — no double count.
		{"no double count", Assessment{Limit: limit, Held: []Held{held("o.1", 1, "672")}, Receivables: Receivables{Open: m("672"),
			FormedBy: map[string]kernel.Money{"o.1": m("960")}}, OrderID: "o.new", Portion: m("0")}, "672.00", "0.00", nil, "", "0.00"},
		// Only an order's latest revision counts; the order reserved now replaces its own.
		{"latest revision", Assessment{Limit: limit, Held: []Held{held("o.1", 1, "3000"), held("o.1", 2, "5000"), held("o.new", 1, "9000")},
			OrderID: "o.new", Portion: m("6000")}, "5000.00", "1000.00", []string{CodeCreditLimitExceeded}, "", "1000.00"},
		// SIM-02 step 11: the earliest overdue receivable 45 days past due, tolerance 30.
		{"overdue 45 > 30", Assessment{Limit: limit, Receivables: Receivables{Open: m("1000"), EarliestDue: t0.Add(-45 * 24 * time.Hour)},
			OrderID: "o.new", Portion: m("500"), Now: t0, ToleranceDays: 30}, "1000.00", "0.00", []string{CodeCreditOverdueExceeded}, "45", "500.00"},
		{"overdue 30 = 30", Assessment{Limit: limit, Receivables: Receivables{Open: m("1000"), EarliestDue: t0.Add(-30 * 24 * time.Hour)},
			OrderID: "o.new", Portion: m("500"), Now: t0, ToleranceDays: 30}, "1000.00", "0.00", nil, "30", "0.00"},
	} {
		v, err := Assess(c.a)
		if err != nil || v.Exposure.Amount() != c.exposure || v.Excess.Amount() != c.excess || !slices.Equal(v.Reasons, c.reasons) ||
			v.Cover().Amount() != c.cover || (c.overdueDays != "" && strconv.Itoa(v.OverdueDays) != c.overdueDays) {
			t.Errorf("%s: %+v cover %s, %v", c.name, v, v.Cover(), err)
		}
	}
	// STL-INV-004 negative: another currency without a credit rate.
	if _, err := Assess(Assessment{Limit: limit, OrderID: "o", Portion: mc(cny, "1")}); code(err) != CodeFXScopeMismatch {
		t.Fatalf("another currency: %v", err)
	}
}

// TD-AT16 and STL-INV-005 with D-6: the approver's tier is the highest credit_policy tier among its
// role templates; a credit officer (1,000) may not release an excess of 3,000, a credit manager
// (5,000) may; above the top tier two distinct top-tier approvers decide; the requester and a sales
// clerk never decide.
func TestApprovalTiers(t *testing.T) {
	p := e2()
	for _, c := range []struct {
		name     string
		cover    string
		a        Approver
		prior    []Approval
		want     string
		required int
	}{
		{"officer within its tier", "1000", officer, nil, "OK", 1},
		{"officer above its tier (TD-AT16)", "3000", officer, nil, CodeCreditDecisionNotAuthorized, 0},
		{"manager within its tier (TD-AT16)", "3000", manager1, nil, "OK", 1},
		{"above the top tier: first manager", "6200", manager1, nil, "OK", 2},
		{"above the top tier: the same manager again", "6200", manager1, []Approval{{Approver: manager1.Actor}}, CodeCreditDecisionNotAuthorized, 0},
		{"above the top tier: second manager", "6200", manager2, []Approval{{Approver: manager1.Actor}}, "OK", 2},
		{"above the top tier: officer", "6200", officer, nil, CodeCreditDecisionNotAuthorized, 0},
		{"the requester", "100", Approver{Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "requester"}, Templates: []string{"credit_manager"}}, nil,
			CodeCreditDecisionNotAuthorized, 0},
		{"a sales clerk (STL-INV-005)", "100", sales, nil, CodeCreditDecisionNotAuthorized, 0},
		{"no credit template", "100", Approver{Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "treasury.1"}, Templates: []string{"treasury"}}, nil,
			CodeCreditDecisionNotAuthorized, 0},
	} {
		ap, required, err := p.Approve(m(c.cover), c.a, "requester", c.prior, "/args/decision_id")
		if code(err) != c.want || required != c.required || (err == nil && ap.Approver != c.a.Actor) {
			t.Errorf("%s: %+v %d %v", c.name, ap, required, err)
		}
	}
	one := e2()
	one.TwoApproversAboveTop = false
	if _, _, err := one.Approve(m("6200"), manager1, "requester", nil, "/x"); code(err) != CodeCreditDecisionNotAuthorized {
		t.Fatalf("no two-approver rule: %v", err)
	}
	if tier, ok := p.TierOf([]string{"credit_officer", "credit_manager"}); !ok || tier.Level != "credit_manager" {
		t.Fatalf("highest tier %+v", tier)
	}
}

func binding(order string, rev int64) Binding {
	return Binding{OrderID: order, OrderRevision: rev, OrderHash: "0000000000000000000000000000000000000000000000000000000000000000"[:62] + "a" + string(rune('0'+rev))}
}

func by(id string, kind kernel.ActorKind, at time.Time) Act {
	return Act{By: kernel.ActorRef{Kind: kind, ID: id}, Epoch: 1, At: at}
}

func afterShipment(amount string) []Stage {
	return []Stage{{StageID: "P2", Trigger: TriggerAfterShipment, Amount: m(amount)}}
}

// credit.reserve as SM-STL-CREDIT-RESERVATION, SM-STL-CREDIT-DECISION and WF-08 have it.
func TestReserveWithinTheLimitOrOpenADecision(t *testing.T) {
	limit := []LimitVersion{{Version: 1, Currency: usd, Amount: m("10000"), ValidFrom: t0.Add(-time.Hour), ValidUntil: t0.Add(365 * 24 * time.Hour)}}
	in := func(id string, b Binding, stages []Stage, reservations []Reservation, decisions []Decision, at time.Time) ReserveInput {
		return ReserveInput{ID: id, PartyID: "customer.1", Binding: b, OrderCurrency: usd, Stages: stages, Policy: e2(), Versions: limit,
			Reservations: reservations, Decisions: decisions, Receivables: Receivables{Open: m("0")}, By: by("sales.1", kernel.ActorHuman, at),
			NewDecisionID: "cdec-1"}
	}
	first, err := Reserve(in("r.1", binding("o.1", 1), afterShipment("7000"), nil, nil, t0))
	r1 := first.Reservation
	if err != nil || r1 == nil || r1.Basis != BasisWithinLimit || r1.Amount.Amount() != "7000.00" || !r1.ExpiresAt.Equal(t0.Add(30*24*time.Hour)) {
		t.Fatalf("within the limit: %+v %v", first, err)
	}
	held := []Reservation{*r1}
	// TD-AT11: 7,000 held, 4,000 more → a decision opens with CREDIT_LIMIT_EXCEEDED, nothing is reserved.
	second, err := Reserve(in("r.2", binding("o.2", 1), afterShipment("4000"), held, nil, t0))
	d := second.Decision
	if err != nil || second.Reservation != nil || d == nil || !second.NewDecision || d.Excess.Amount() != "1000.00" ||
		!slices.Equal(d.Reasons, []string{CodeCreditLimitExceeded}) || !d.ExpiresAt.Equal(t0.Add(7*24*time.Hour)) || d.Cover.Amount() != "1000.00" {
		t.Fatalf("over the limit: %+v %v", second, err)
	}
	// Asking again while it is open answers the open decision.
	again, _ := Reserve(in("r.2", binding("o.2", 1), afterShipment("4000"), held, []Decision{*d}, t0.Add(time.Hour)))
	if again.Decision == nil || again.NewDecision || again.Decision.ID != d.ID {
		t.Fatalf("open decision: %+v", again)
	}
	// The officer releases it within its tier; the next reservation succeeds on it.
	if done, _, err := d.Release(e2(), officer, by("officer.1", kernel.ActorHuman, t0.Add(time.Hour))); !done || err != nil || d.Status != DecisionReleased {
		t.Fatalf("release %v %v", done, err)
	}
	third, err := Reserve(in("r.2", binding("o.2", 1), afterShipment("4000"), held, []Decision{*d}, t0.Add(2*time.Hour)))
	if err != nil || third.Reservation == nil || third.Reservation.Basis != BasisCreditDecision || third.Reservation.DecisionID != d.ID {
		t.Fatalf("on the released decision: %+v %v", third, err)
	}
	// A released decision of another binding, an expired one, or a larger excess opens a new one.
	if out, _ := Reserve(in("r.3", binding("o.3", 1), afterShipment("4000"), held, []Decision{*d}, t0.Add(2*time.Hour))); !out.NewDecision {
		t.Fatalf("another binding: %+v", out)
	}
	if out, _ := Reserve(in("r.2", binding("o.2", 1), afterShipment("4000"), held, []Decision{*d}, t0.Add(8*24*time.Hour))); out.Decision == nil {
		t.Fatalf("expired decision (TD-AT16: re-request): %+v", out)
	}
	bigger := append(slices.Clone(held), Reservation{ID: "r.9", Binding: binding("o.9", 1), Amount: m("500"), Exposure: m("500"),
		Status: ReservationReserved, ExpiresAt: t0.Add(24 * time.Hour), Basis: BasisWithinLimit})
	if out, _ := Reserve(in("r.2", binding("o.2", 1), afterShipment("4000"), bigger, []Decision{*d}, t0.Add(2*time.Hour))); !out.NewDecision {
		t.Fatalf("larger excess: %+v", out)
	}

	// WF-08: one live reservation per order revision; the same ID answers it; an expired one is
	// released and reserved again.
	if out, _ := Reserve(in("r.1", binding("o.1", 1), afterShipment("7000"), held, nil, t0)); out.Existing == nil {
		t.Fatalf("same reservation: %+v", out)
	}
	if _, err := Reserve(in("r.1x", binding("o.1", 1), afterShipment("7000"), held, nil, t0)); code(err) != kernel.CodeInvalidState {
		t.Fatalf("second reservation of a revision: %v", err)
	}
	if _, err := Reserve(in("r.1", binding("o.4", 1), afterShipment("1"), held, nil, t0)); code(err) != kernel.CodeInvalidState {
		t.Fatalf("an ID taken by another binding: %v", err)
	}
	late := t0.Add(31 * 24 * time.Hour)
	if out, err := Reserve(in("r.1b", binding("o.1", 1), afterShipment("7000"), held, nil, late)); err != nil || out.Expired == nil ||
		out.Expired.ID != "r.1" || out.Reservation == nil {
		t.Fatalf("expired reservation: %+v %v", out, err)
	}
	// D-5: only document-controlled stages after shipment → documents_secured, amount 0.
	docs := []Stage{{StageID: "D", Trigger: TriggerAfterShipment, ReleaseControl: ReleaseDocuments, Amount: m("900")}}
	if out, err := Reserve(in("r.5", binding("o.5", 1), docs, held, nil, t0)); err != nil || out.Reservation.Basis != BasisDocumentsSecured ||
		!out.Reservation.Amount.IsZero() || out.Reservation.Secured.Amount() != "900.00" {
		t.Fatalf("documents secured: %+v %v", out, err)
	}
	// Nothing due after shipment: no reservation is needed.
	prepaid := []Stage{{StageID: "A", Trigger: TriggerBeforeShipment, Amount: m("900")}}
	if _, err := Reserve(in("r.6", binding("o.6", 1), prepaid, held, nil, t0)); code(err) != kernel.CodeInvalidState {
		t.Fatalf("prepaid: %v", err)
	}
	// A new customer has the policy's default limit 0 (ADR-BR-016): any portion opens a decision.
	fresh := in("r.7", binding("o.7", 1), afterShipment("1"), nil, nil, t0)
	fresh.Versions = nil
	if out, _ := Reserve(fresh); !out.NewDecision || out.Limit.Amount() != "0.00" {
		t.Fatalf("default limit: %+v", out)
	}
	// STL-INV-004: an order in CNY needs a credit rate into the policy's USD.
	cnyOrder := in("r.8", binding("o.8", 1), nil, nil, nil, t0)
	cnyOrder.OrderCurrency, cnyOrder.Stages = cny, []Stage{{StageID: "P", Trigger: TriggerAfterShipment, Amount: mc(cny, "100")}}
	if _, err := Reserve(cnyOrder); code(err) != CodeFXScopeMismatch {
		t.Fatalf("no credit rate: %v", err)
	}
	cnyOrder.Convert = func(x kernel.Money) (kernel.Money, error) {
		return kernel.RoundMoney(usd, x.Decimal().Mul(kernel.MustDecimal("0.140449")))
	}
	if out, err := Reserve(cnyOrder); err != nil || out.Reservation.Exposure.Amount() != "14.04" || out.Reservation.Amount.Currency() != cny {
		t.Fatalf("converted: %+v %v", out, err)
	}
}

// SM-STL-CREDIT-DECISION and STL-INV-005: decided once; expired decisions are re-requested; the
// requester does not reject its own.
func TestDecisionLifecycle(t *testing.T) {
	open := func() *Decision {
		return &Decision{ID: "d", Binding: binding("o", 1), Portion: m("4000"), Excess: m("3000"), Cover: m("3000"), Status: DecisionOpen, Version: 1,
			ExpiresAt: t0.Add(7 * 24 * time.Hour), Opened: by("sales.1", kernel.ActorHuman, t0), OverdueDays: -1}
	}
	d := open()
	if _, _, err := d.Release(e2(), officer, by("officer.1", kernel.ActorHuman, t0)); code(err) != CodeCreditDecisionNotAuthorized || d.Version != 1 {
		t.Fatalf("officer for 3,000: %v", err)
	}
	if done, _, err := d.Release(e2(), manager1, by("manager.1", kernel.ActorHuman, t0)); !done || err != nil || d.Version != 2 {
		t.Fatalf("manager for 3,000: %v", err)
	}
	if _, _, err := d.Release(e2(), manager2, by("manager.2", kernel.ActorHuman, t0)); code(err) != kernel.CodeInvalidState {
		t.Fatalf("decided twice (STL-INV-005 concurrency): %v", err)
	}
	if d.State(t0.Add(8*24*time.Hour)) != StateExpired {
		t.Fatal("a released decision unused past its validity is expired")
	}
	d.ReservationID = "r"
	if d.State(t0.Add(8*24*time.Hour)) != DecisionReleased {
		t.Fatal("a used decision stays released")
	}
	late := open()
	if _, _, err := late.Release(e2(), manager1, by("manager.1", kernel.ActorHuman, t0.Add(7*24*time.Hour))); code(err) != CodeApprovalExpired {
		t.Fatalf("expired: %v", err)
	}
	if err := open().Reject(Approver{Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "sales.1"}, Templates: []string{"credit_manager"}},
		by("sales.1", kernel.ActorHuman, t0)); code(err) != CodeCreditDecisionNotAuthorized {
		t.Fatalf("requester rejects: %v", err)
	}
	r := open()
	if err := r.Reject(officer, by("officer.1", kernel.ActorHuman, t0)); err != nil || r.Status != DecisionRejected || r.Version != 2 {
		t.Fatalf("reject: %v", err)
	}
	// Above the top tier: two managers, one after the other.
	big := open()
	big.Cover = m("6200")
	if done, n, err := big.Release(e2(), manager1, by("manager.1", kernel.ActorHuman, t0)); done || n != 2 || err != nil || big.Status != DecisionOpen {
		t.Fatalf("first of two: %v %d %v", done, n, err)
	}
	if done, _, err := big.Release(e2(), manager2, by("manager.2", kernel.ActorHuman, t0)); !done || err != nil || big.Version != 3 {
		t.Fatalf("second of two: %v %v", done, err)
	}
}

// SM-STL-CREDIT-LIMIT and ADR-BR-016: a limit in the policy's currency from accepted sources, the
// lower of both; published by an independent approver whose tier covers it (two above the top).
func TestLimitRequests(t *testing.T) {
	p := e2()
	p.LimitSources = []string{SourceInternalAssessment, SourceSinosureBuyerLimit}
	in := LimitInput{ID: "l", PartyID: "customer.1", Currency: usd, CurrencyRevision: 1, ValidFrom: t0, ValidUntil: t0.Add(365 * 24 * time.Hour),
		Sources: map[string]SourceAmount{SourceInternalAssessment: {Amount: m("12000"), EvidenceIDs: []string{"e1"}},
			SourceSinosureBuyerLimit: {Amount: m("10000"), EvidenceIDs: []string{"e2"}}}, By: by("sales.1", kernel.ActorHuman, t0)}
	r, err := NewLimitRequest(in, p)
	if err != nil || r.Amount.Amount() != "10000.00" || r.Source() != SourceLowerOfBoth {
		t.Fatalf("lower of both: %+v %v", r, err)
	}
	if _, err := NewLimitRequest(in, e2()); code(err) != kernel.CodeInvalidKnownValue {
		t.Fatalf("a source the policy does not accept: %v", err)
	}
	cnyIn := in
	cnyIn.Currency = cny
	if _, err := NewLimitRequest(cnyIn, p); code(err) != CodeFXScopeMismatch {
		t.Fatalf("another currency: %v", err)
	}
	if _, _, err := r.Publish(p, officer, by("officer.1", kernel.ActorHuman, t0), 1); code(err) != CodeCreditDecisionNotAuthorized {
		t.Fatalf("officer publishes 10,000: %v", err)
	}
	v, n, err := r.Publish(p, manager1, by("manager.1", kernel.ActorHuman, t0), 1)
	if v != nil || n != 2 || err != nil || r.Status != RequestRequested || r.Version != 2 {
		t.Fatalf("first of two: %+v %d %v", v, n, err)
	}
	v, _, err = r.Publish(p, manager2, by("manager.2", kernel.ActorHuman, t0), 1)
	if err != nil || v == nil || v.Amount.Amount() != "10000.00" || r.Status != RequestPublished || r.Version != 3 || r.LimitVersion != 1 {
		t.Fatalf("published: %+v %v", v, err)
	}
	if !v.InForce(t0) || LimitInForce([]LimitVersion{*v}, t0.Add(-time.Second)) != nil {
		t.Fatal("validity")
	}
	small, _ := NewLimitRequest(LimitInput{ID: "s", PartyID: "c", Currency: usd, ValidFrom: t0, ValidUntil: t0.Add(time.Hour),
		Sources: map[string]SourceAmount{SourceInternalAssessment: {Amount: m("4000")}}, By: by("sales.1", kernel.ActorHuman, t0)}, p)
	if err := small.Reject(sales, by("sales.1", kernel.ActorHuman, t0)); code(err) != CodeCreditDecisionNotAuthorized {
		t.Fatalf("the requester rejects: %v", err)
	}
	if v, _, err := small.Publish(p, manager1, by("manager.1", kernel.ActorHuman, t0), 2); err != nil || v == nil || small.Version != 2 {
		t.Fatalf("one manager publishes 4,000: %v", err)
	}
}

// SM-STL-CREDIT-RESERVATION: consumed by the system for its binding while valid; released before
// activation, a consumed one only by the system.
func TestReservationTransitions(t *testing.T) {
	reserved := func() *Reservation {
		return &Reservation{ID: "r", Binding: binding("o", 1), Status: ReservationReserved, Version: 1, Reserved: by("sales.1", kernel.ActorHuman, t0),
			ExpiresAt: t0.Add(30 * 24 * time.Hour)}
	}
	now := by("system:inbox.stl", kernel.ActorSystem, t0.Add(time.Hour))
	for _, c := range []struct {
		name string
		b    Binding
		at   time.Time
		want string
	}{
		{"another order", binding("o.x", 1), t0.Add(time.Minute), kernel.CodeReferenceInvalid},
		{"another revision", binding("o", 2), t0.Add(time.Minute), CodeReferenceStale},
		{"activated before the reservation", binding("o", 1), t0.Add(-time.Minute), kernel.CodeInvalidKnownValue},
		{"activated after now", binding("o", 1), t0.Add(2 * time.Hour), kernel.CodeInvalidKnownValue},
		{"activated", binding("o", 1), t0.Add(time.Minute), "OK"},
	} {
		if err := reserved().Consume(c.b, c.at, now); code(err) != c.want {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	expiredAtActivation := reserved()
	expiredAtActivation.ExpiresAt = t0.Add(time.Minute)
	if err := expiredAtActivation.Consume(binding("o", 1), t0.Add(2*time.Minute), by("s", kernel.ActorSystem, t0.Add(time.Hour))); code(err) != kernel.CodeInvalidState {
		t.Fatalf("expired when activated: %v", err)
	}
	r := reserved()
	if err := r.Consume(binding("o", 1), t0.Add(time.Minute), now); err != nil || r.Status != ReservationConsumed || r.Version != 2 || !r.Live(t0.Add(365*24*time.Hour)) {
		t.Fatalf("consumed: %+v %v", r, err)
	}
	if err := r.Release(by("officer.1", kernel.ActorHuman, t0.Add(2*time.Hour))); code(err) != kernel.CodeInvalidState {
		t.Fatalf("a person releases a consumed reservation: %v", err)
	}
	if err := r.Release(Act{By: system, Epoch: 1, At: t0.Add(2 * time.Hour), Reason: "order closed"}); err != nil || r.Status != ReservationReleased || r.Version != 3 {
		t.Fatalf("the system releases it on the order's end: %v", err)
	}
	if err := r.Release(Act{By: system, Epoch: 1, At: t0.Add(3 * time.Hour)}); code(err) != kernel.CodeInvalidState {
		t.Fatalf("released twice: %v", err)
	}
	if x := reserved(); x.State(t0.Add(30*24*time.Hour)) != StateExpired || x.Live(t0.Add(30*24*time.Hour)) {
		t.Fatal("a reserved reservation expires")
	}
}
