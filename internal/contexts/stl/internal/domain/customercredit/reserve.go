package customercredit

import (
	"fmt"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// ReserveInput is what credit.reserve decides on: the reservation asked for (ID, the customer, the
// order binding with its payment stages in the order currency), how the order currency converts
// into the policy's (nil: the same currency) under which fx_rate revision, the policy, and the
// customer's root state — limit versions, every reservation, the decisions of this binding and its
// receivables — at Now.
type ReserveInput struct {
	ID            string
	PartyID       string
	Binding       Binding
	OrderCurrency kernel.Currency
	Stages        []Stage
	Convert       func(kernel.Money) (kernel.Money, error)
	FX            *ConfigUse
	Policy        Policy
	Versions      []LimitVersion
	Reservations  []Reservation
	Decisions     []Decision
	Receivables   Receivables
	By            Act
	Team          string
	NewDecisionID string
}

// ReserveOutcome is what credit.reserve does: answer an existing reservation (Existing, nothing
// changes), make a reservation (Reservation, releasing Expired, the expired reservation of the same
// order revision, and using UsedDecision when a released decision covers it), or open a decision
// (Decision, NewDecision false when an open one of the binding is answered instead).
type ReserveOutcome struct {
	Existing     *Reservation
	Reservation  *Reservation
	Expired      *Reservation
	UsedDecision *Decision
	Decision     *Decision
	NewDecision  bool
	Portion      kernel.Money
	Secured      kernel.Money
	Limit        kernel.Money
	LimitVersion int64
	Verdict      Verdict
}

// Reserve applies STL-INV-004, D-4, D-5 and WF-08 (占用按 (company, order_id, order_revision) 唯一).
func Reserve(in ReserveInput) (ReserveOutcome, error) {
	now := in.By.At
	var out ReserveOutcome
	for i := range in.Reservations {
		r := &in.Reservations[i]
		switch {
		case r.ID == in.ID && r.Binding == in.Binding && r.Live(now):
			out.Existing = r
			return out, nil
		case r.ID == in.ID:
			return out, state("/args/reservation_id", "The reservation ID holds another reservation")
		case r.Binding.OrderID != in.Binding.OrderID || r.Binding.OrderRevision != in.Binding.OrderRevision:
		case r.Live(now):
			return out, state("/args/order_revision", fmt.Sprintf("Order revision %d holds reservation %s", r.Binding.OrderRevision, r.ID))
		case r.Status == ReservationReserved:
			out.Expired = r
		}
	}
	portion, secured, err := Portion(in.OrderCurrency, in.Stages, in.Policy.DocumentsSecured)
	if err != nil {
		return out, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", err.Error())
	}
	out.Portion, out.Secured = portion, secured
	if portion.IsZero() && secured.IsZero() {
		return out, state("/args/order_id", "The order has no payment due after shipment or invoice; it needs no credit reservation")
	}
	res := &Reservation{ID: in.ID, PartyID: in.PartyID, Binding: in.Binding, Amount: portion, Secured: secured, FX: in.FX,
		Policy: in.Policy.Use, Status: ReservationReserved, Version: 1, ExpiresAt: in.Policy.ReservationExpiry(now),
		OwnerActor: in.By.By.ID, OwnerTeam: in.Team, Reserved: in.By}
	if portion.IsZero() {
		res.Basis, res.FX = BasisDocumentsSecured, nil
		res.Exposure, _ = kernel.NewMoney(in.Policy.Currency, 0)
		out.Reservation = res
		return out, nil
	}
	converted := portion
	if in.OrderCurrency != in.Policy.Currency {
		if in.Convert == nil {
			return out, Reject(CodeFXScopeMismatch, "/args/fx_rate", fmt.Sprintf("The order is in %s, the credit policy in %s: name an fx_rate of purpose credit",
				in.OrderCurrency, in.Policy.Currency))
		}
		if converted, err = in.Convert(portion); err != nil {
			return out, err
		}
	} else {
		res.FX = nil
	}
	if converted.Currency() != in.Policy.Currency {
		return out, Reject(CodeFXScopeMismatch, "/args/fx_rate", "The rate converts into "+converted.Currency().Code())
	}
	res.Exposure = converted
	out.Limit = in.Policy.DefaultLimit
	if v := LimitInForce(in.Versions, now); v != nil {
		if v.Currency != in.Policy.Currency {
			return out, Reject(CodeFXScopeMismatch, "/args/credit_policy", fmt.Sprintf("The limit in force is in %s, the credit policy in %s",
				v.Currency, in.Policy.Currency))
		}
		out.Limit, out.LimitVersion = v.Amount, v.Version
	}
	var held []Held
	for i := range in.Reservations {
		if r := &in.Reservations[i]; r.Live(now) && r.Basis != BasisDocumentsSecured {
			held = append(held, Held{OrderID: r.Binding.OrderID, OrderRevision: r.Binding.OrderRevision, Exposure: r.Exposure})
		}
	}
	v, err := Assess(Assessment{Limit: out.Limit, Held: held, Receivables: in.Receivables, OrderID: in.Binding.OrderID, Portion: converted,
		Now: now, ToleranceDays: in.Policy.OverdueToleranceDays})
	if err != nil {
		return out, err
	}
	out.Verdict = v
	if v.Within() {
		res.Basis = BasisWithinLimit
		out.Reservation = res
		return out, nil
	}
	var open *Decision
	for i := range in.Decisions {
		d := &in.Decisions[i]
		if d.Covers(in.Binding, portion, v, now) {
			res.Basis, res.DecisionID = BasisCreditDecision, d.ID
			d.ReservationID = res.ID
			out.Reservation, out.UsedDecision = res, d
			return out, nil
		}
		if d.Binding == in.Binding && d.Status == DecisionOpen && !d.Expired(now) && open == nil {
			open = d
		}
	}
	if open != nil {
		out.Decision = open
		return out, nil
	}
	d := &Decision{ID: in.NewDecisionID, PartyID: in.PartyID, Binding: in.Binding, Portion: portion, Converted: converted, FX: res.FX,
		Limit: out.Limit, LimitVersion: out.LimitVersion, Exposure: v.Exposure, Excess: v.Excess, Cover: v.Cover(), OverdueDays: -1,
		Reasons: v.Reasons, Policy: in.Policy.Use, Status: DecisionOpen, Version: 1, ExpiresAt: in.Policy.DecisionExpiry(now),
		OwnerActor: in.By.By.ID, OwnerTeam: in.Team, Opened: in.By}
	if v.Overdue() {
		d.OverdueDays = v.OverdueDays
	}
	out.Decision, out.NewDecision = d, true
	return out, nil
}
