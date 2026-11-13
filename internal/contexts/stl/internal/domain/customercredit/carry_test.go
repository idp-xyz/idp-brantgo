package customercredit

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// sim02 is SIM-02's reservation after activation (simulated G0 walkthrough SIM-02 step 4, S5 D2b): 9,200.00
// O/A 60 days on a released credit decision, consumed at activation.
func sim02() *Reservation {
	return &Reservation{ID: "res.sim", PartyID: "customer.1", Binding: binding("sim", 1), Amount: m("9200"), Secured: m("0"), Exposure: m("9200"),
		Basis: BasisCreditDecision, DecisionID: "cdec.1", Policy: ConfigUse{ID: "credit", Revision: 1, Hash: "h"}, Status: ReservationConsumed,
		Version: 2, ExpiresAt: t0.Add(30 * 24 * time.Hour), OwnerActor: "sales.1", OwnerTeam: "team.1", Reserved: by("sales.1", kernel.ActorHuman, t0),
		ActivatedAt: t0.Add(time.Hour)}
}

// S5R-D08 (SM-STL-CREDIT-RESERVATION 承接, STL-INV-004 positive "SIM-02：订单减到 8,280.00 后敞口按新信用部分计入"): the
// customer reduces SIM-02 by 10 %; the amendment asks no more credit, so the system carries revision 1's
// 9,200.00 over to revision 2 at its credit portion 8,280.00 — a new reservation consumed at the instant the
// amendment took effect, basis carried_over, keeping the customer, owner, policy and expiry of the one it
// carries; the exposure counts revision 2's 8,280.00. A larger portion is not carried (D-9 reserves it before
// apply), nor one of the same or an earlier revision, of another order, of a released reservation, or a
// revision with nothing due on credit.
func TestCarryOverFollowsTheAmendedRevision(t *testing.T) {
	from := sim02()
	applied, now := t0.Add(48*time.Hour), t0.Add(48*time.Hour+time.Second)
	sys := Act{By: system, Epoch: 3, At: now}
	c := Carry{ID: "carried.1", From: from, Binding: binding("sim", 2), Portion: m("8280"), Secured: m("0"), Exposure: m("8280"),
		AppliedAt: applied, By: sys}
	r, err := CarryOver(c)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != ReservationConsumed || r.Version != 1 || r.Basis != BasisCarriedOver || r.CarriedFrom != "res.sim" || r.DecisionID != "" ||
		r.Amount.Amount() != "8280.00" || r.Exposure.Amount() != "8280.00" || r.PartyID != "customer.1" || r.OwnerActor != "sales.1" ||
		r.OwnerTeam != "team.1" || r.Policy != from.Policy || !r.ExpiresAt.Equal(from.ExpiresAt) || !r.ActivatedAt.Equal(applied) ||
		r.Consumed == nil || r.Consumed.By != system || r.Reserved.By != system || !r.Live(t0.Add(365*24*time.Hour)) {
		t.Fatalf("carried %+v", r)
	}
	v, err := Assess(Assessment{Limit: m("10000"), Held: []Held{HeldOf(r)}, Receivables: Receivables{Open: m("7000")}, OrderID: "o.next",
		Portion: m("0"), Now: now})
	if err != nil || v.Exposure.Amount() != "15280.00" {
		t.Fatalf("SIM-02 exposure after the amendment: %+v %v", v, err)
	}
	for name, mod := range map[string]func(*Carry){
		"a larger portion":       func(c *Carry) { c.Portion, c.Exposure = m("9200.01"), m("9200.01") },
		"the same revision":      func(c *Carry) { c.Binding = binding("sim", 1) },
		"nothing on credit":      func(c *Carry) { c.Portion, c.Exposure = m("0"), m("0") },
		"a released reservation": func(c *Carry) { x := *from; x.Status = ReservationReleased; c.From = &x },
	} {
		x := c
		mod(&x)
		if _, err := CarryOver(x); code(err) != kernel.CodeInvalidState {
			t.Errorf("%s: %v", name, err)
		}
	}
	other := c
	other.Binding = binding("other", 2)
	if _, err := CarryOver(other); code(err) != kernel.CodeReferenceInvalid {
		t.Errorf("another order: %v", err)
	}
	future := c
	future.AppliedAt = now.Add(time.Minute)
	if _, err := CarryOver(future); code(err) != kernel.CodeInvalidKnownValue {
		t.Errorf("an amendment in the future: %v", err)
	}
	// Documents secured under the policy (D-5): a revision whose credit part is all document-controlled carries a
	// secured amount and no exposure.
	secured := c
	secured.Portion, secured.Secured, secured.Exposure = m("0"), m("8280"), m("0")
	if r, err := CarryOver(secured); err != nil || r.Amount.Sign() != 0 || r.Secured.Amount() != "8280.00" || r.Exposure.Sign() != 0 {
		t.Fatalf("secured: %+v %v", r, err)
	}
	// Another currency: the exposure at the rate never carries more than was held (HALF_UP rounding).
	fx := *from
	fx.Amount, fx.Secured, fx.Exposure, fx.FX = mc(cny, "70000"), mc(cny, "0"), m("10000"), &ConfigUse{ID: "fx", Revision: 1, Hash: "f"}
	cross := c
	cross.From, cross.Portion, cross.Secured, cross.Exposure = &fx, mc(cny, "70000"), mc(cny, "0"), m("10000.01")
	if r, err := CarryOver(cross); err != nil || r.Exposure.Amount() != "10000.00" || r.FX == nil {
		t.Fatalf("cross-currency: %+v %v", r, err)
	}
}

// S5R-D08: every release states its cause; the order's end and supersession are the system's, a person or
// agent abandons a reservation or lets an expired one go.
func TestReleaseCauses(t *testing.T) {
	r := sim02()
	if err := r.Release(by("officer.1", kernel.ActorHuman, t0), CauseSuperseded); code(err) != kernel.CodeInvalidState {
		t.Fatalf("a person releases a consumed reservation: %v", err)
	}
	if err := r.Release(Act{By: system, Epoch: 1, At: t0.Add(time.Hour)}, "unknown"); code(err) != kernel.CodeInvalidKnownValue {
		t.Fatalf("an unknown cause: %v", err)
	}
	if err := r.Release(Act{By: system, Epoch: 1, At: t0.Add(time.Hour)}, CauseSuperseded); err != nil || r.ReleaseCause != CauseSuperseded ||
		r.Version != 3 {
		t.Fatalf("superseded: %+v %v", r, err)
	}
	x := sim02()
	x.Status, x.Version = ReservationReserved, 1
	if err := x.Release(by("sales.1", kernel.ActorAI, t0), CauseOrderEnded); code(err) != kernel.CodeInvalidKnownValue {
		t.Fatalf("an agent states the order's end: %v", err)
	}
	if err := x.Release(by("sales.1", kernel.ActorHuman, t0), CauseAbandoned); err != nil || x.ReleaseCause != CauseAbandoned {
		t.Fatalf("abandoned: %+v %v", x, err)
	}
}
