package customercredit

import (
	"fmt"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Payment stage triggers (POL payment_terms) and the release control of S5 D-5.
const (
	TriggerOrderConfirmed = "order_confirmed"
	TriggerBeforeShipment = "before_shipment"
	TriggerAfterShipment  = "after_shipment"
	TriggerAfterInvoice   = "after_invoice"
	ReleaseDocuments      = "documents"
)

// Stage is one payment stage of an order revision (TRD OrderCommercialSnapshot v1): its trigger, its
// release control ("" or documents) and its scheduled amount in the order currency.
type Stage struct {
	StageID        string
	Trigger        string
	ReleaseControl string
	Amount         kernel.Money
}

// Portion returns the credit portion of an order revision (TRD-INV-022, WF-08): the scheduled
// amounts due after shipment or after invoice, except stages under document control when the
// policy treats them as secured (D-5), which are returned as secured instead. Stages due on order
// confirmation or before shipment are paid before the goods leave and need no credit.
func Portion(c kernel.Currency, stages []Stage, documentsSecured bool) (portion, secured kernel.Money, err error) {
	portion, _ = kernel.NewMoney(c, 0)
	secured = portion
	for i, s := range stages {
		if s.Amount.Currency() != c {
			return kernel.Money{}, kernel.Money{}, fmt.Errorf("customercredit: stage %d is in %s, the order in %s", i, s.Amount.Currency(), c)
		}
		if s.Amount.Sign() < 0 {
			return kernel.Money{}, kernel.Money{}, fmt.Errorf("customercredit: stage %s has a negative amount", s.StageID)
		}
		if s.Trigger != TriggerAfterShipment && s.Trigger != TriggerAfterInvoice {
			continue
		}
		if s.ReleaseControl == ReleaseDocuments && documentsSecured {
			secured, _ = secured.Add(s.Amount)
			continue
		}
		portion, _ = portion.Add(s.Amount)
	}
	return portion, secured, nil
}

// Held is a live reservation of the customer as the exposure counts it: its order revision, its
// amount in the limit currency, and whether it left document-controlled stages out as secured (D-5:
// then the receivables of those stages do not replace it).
type Held struct {
	OrderID          string
	OrderRevision    int64
	Exposure         kernel.Money
	DocumentsSecured bool
}

// Receivables is what the customer owes on business receivables (STL's settlement statements, B2b,
// and opening balances, C10): the open amount in the limit currency, the receivables each order has
// formed on its credit stages (due after shipment or invoice, whether paid or not; they replace that
// order's reservation, D-4) — FormedDocumentsBy those of document-controlled stages, which replace a
// reservation only where it did not treat them as secured — and the due date of the earliest open
// installment (zero: none open; overdue when before now).
type Receivables struct {
	Open              kernel.Money
	FormedBy          map[string]kernel.Money
	FormedDocumentsBy map[string]kernel.Money
	EarliestDue       time.Time
}

// Assessment is what credit.reserve judges (STL-INV-004): the limit in force, the customer's live
// reservations and receivables, the order being reserved and its portion converted into the limit
// currency, at Now with the policy's overdue tolerance; DocumentsSecured is the policy's D-5 setting
// the portion was computed with.
type Assessment struct {
	Limit            kernel.Money
	Held             []Held
	Receivables      Receivables
	OrderID          string
	Portion          kernel.Money
	Now              time.Time
	ToleranceDays    int
	DocumentsSecured bool
}

// Verdict is the assessment's outcome in the limit currency: the exposure before this order, the
// order's share after its receivables, the exposure with it, the excess over the limit, the days
// the earliest overdue receivable is overdue (-1: none) and the reasons a decision must open with
// (none: within the limit).
type Verdict struct {
	Exposure    kernel.Money
	Share       kernel.Money
	After       kernel.Money
	Excess      kernel.Money
	OverdueDays int
	Reasons     []string
}

// Within reports whether the reservation may be made without a decision.
func (v Verdict) Within() bool { return len(v.Reasons) == 0 }

// Overdue reports whether the earliest overdue receivable exceeds the tolerance.
func (v Verdict) Overdue() bool { return slices.Contains(v.Reasons, CodeCreditOverdueExceeded) }

// Cover is the amount an approver's tier must cover to release a decision on v: the excess over the
// limit, or, when a receivable is overdue beyond the tolerance, the whole new credit (the order's
// share), whichever is larger.
func (v Verdict) Cover() kernel.Money {
	if v.Overdue() {
		if c, _ := v.Share.Cmp(v.Excess); c > 0 {
			return v.Share
		}
	}
	return v.Excess
}

func clampZero(m kernel.Money) kernel.Money {
	if m.Sign() < 0 {
		z, _ := kernel.NewMoney(m.Currency(), 0)
		return z
	}
	return m
}

// Assess applies STL-INV-004 with the exposure of S5 D-4: exposure = open receivables + Σ over the
// customer's other orders of max(0, the live reservation of the order's latest revision − the
// receivables formed for that order); the order being reserved replaces its earlier revisions'
// reservations with max(0, Portion − its receivables). A reservation needs exposure + share ≤ limit
// (otherwise CREDIT_LIMIT_EXCEEDED) and an earliest overdue receivable no more than the tolerance
// days overdue (otherwise CREDIT_OVERDUE_EXCEEDED). Every amount is in the limit's currency.
func Assess(a Assessment) (Verdict, error) {
	c := a.Limit.Currency()
	zero, _ := kernel.NewMoney(c, 0)
	check := func(what string, m kernel.Money) error {
		if m.Currency() != c {
			return Reject(CodeFXScopeMismatch, "/args/credit_policy", fmt.Sprintf("%s is in %s, the limit in %s", what, m.Currency(), c))
		}
		return nil
	}
	open := a.Receivables.Open
	if open.Currency().IsZero() {
		open = zero
	}
	if err := check("The open receivables", open); err != nil {
		return Verdict{}, err
	}
	if err := check("The portion", a.Portion); err != nil {
		return Verdict{}, err
	}
	formed := func(order string, secured bool) (kernel.Money, error) {
		f, ok := a.Receivables.FormedBy[order]
		if !ok {
			f = zero
		}
		if err := check("The receivables of order "+order, f); err != nil {
			return kernel.Money{}, err
		}
		if d, ok := a.Receivables.FormedDocumentsBy[order]; ok && !secured {
			if err := check("The receivables of order "+order, d); err != nil {
				return kernel.Money{}, err
			}
			f, _ = f.Add(d)
		}
		return f, nil
	}
	latest := map[string]Held{}
	for _, h := range a.Held {
		if h.OrderID == a.OrderID {
			continue
		}
		if err := check("The reservation of order "+h.OrderID, h.Exposure); err != nil {
			return Verdict{}, err
		}
		if cur, ok := latest[h.OrderID]; !ok || h.OrderRevision > cur.OrderRevision {
			latest[h.OrderID] = h
		}
	}
	exposure := open
	orders := make([]string, 0, len(latest))
	for o := range latest {
		orders = append(orders, o)
	}
	slices.Sort(orders)
	for _, o := range orders {
		f, err := formed(o, latest[o].DocumentsSecured)
		if err != nil {
			return Verdict{}, err
		}
		share, _ := latest[o].Exposure.Sub(f)
		exposure, _ = exposure.Add(clampZero(share))
	}
	f, err := formed(a.OrderID, a.DocumentsSecured)
	if err != nil {
		return Verdict{}, err
	}
	share, _ := a.Portion.Sub(f)
	share = clampZero(share)
	after, _ := exposure.Add(share)
	excess, _ := after.Sub(a.Limit)
	v := Verdict{Exposure: exposure, Share: share, After: after, Excess: clampZero(excess), OverdueDays: -1}
	if v.Excess.Sign() > 0 {
		v.Reasons = append(v.Reasons, CodeCreditLimitExceeded)
	}
	if due := a.Receivables.EarliestDue; !due.IsZero() && due.Before(a.Now) {
		v.OverdueDays = int(a.Now.Sub(due) / (24 * time.Hour))
		if v.OverdueDays > a.ToleranceDays {
			v.Reasons = append(v.Reasons, CodeCreditOverdueExceeded)
		}
	}
	return v, nil
}
