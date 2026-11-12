package application

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The guards of CNX and STL in order.activate and order.amendment.apply (S5 unit B4; WBS S5-12b,
// S5-13b; TRD-INV-022, TRD-INV-023; Context Map R57, R58; WF-08, ES-11, SC-09, TD-AT11, TD-AT12; S5
// decisions D-2, D-5, D-9). Both are guard functions their owners publish (ADR-BR-004 option B):
// TRD calls them with its own executor inside its own transaction, after its roots are locked FOR
// UPDATE and the authority guard FOR SHARE, at their places in context code order (ADR-BR-011), and
// each locks its row FOR SHARE until TRD commits — a compliance decision or a credit release
// either committed first, and the guard says so, or waits until TRD's transaction ended.
//
// TRD decides what it asks and what the answer means for its command; the owner decides the
// answer. A refusal is the owner's rejection (TRADE_COMPLIANCE_NOT_CLEAR,
// CREDIT_RESERVATION_REQUIRED) and never a risk: no order approval acknowledges it away.

// OrderRevisionRef is an order at an order revision and its order hash, as the guards are asked.
type OrderRevisionRef struct {
	OrderID       string
	OrderRevision int64
	OrderHash     string
}

// TradeComplianceGuard is CNX's TradeComplianceGuard v1 (R58) as TRD calls it: is the order
// revision o covered by a clear, valid trade compliance conclusion at at? It locks CNX's check row
// FOR SHARE in exec's transaction. path is where a refusal points in TRD's command.
type TradeComplianceGuard func(ctx context.Context, exec bentopg.Executor, o OrderRevisionRef, at time.Time, path string) (ComplianceAnswer, error)

// ComplianceAnswer is CNX's verdict: Evidence is the value CNX names for a caller's commit evidence
// (status/reason/check/decision/licence); the screening regulation revision and the validity are
// the clear conclusion's. Refusal is nil when the conclusion is clear and valid and CNX's
// TRADE_COMPLIANCE_NOT_CLEAR otherwise.
type ComplianceAnswer struct {
	Evidence           string
	RegulationID       string
	RegulationRevision int64
	ValidUntil         time.Time
	Refusal            error
}

// CreditGuard is STL's CreditGuard v1 (R57) as TRD calls it: does a live reservation of the order
// revision cover q's portion? It locks STL's reservation row FOR SHARE in exec's transaction.
type CreditGuard func(ctx context.Context, exec bentopg.Executor, q CreditQuery) (CreditAnswer, error)

// CreditQuery asks for the order revision Order (the reservation must bind its hash) a reservation
// covering PortionMinor, minor units of the order currency Currency, at At. Path is where a
// refusal points in TRD's command.
type CreditQuery struct {
	Order        OrderRevisionRef
	PortionMinor string
	Currency     string
	At           time.Time
	Path         string
}

// CreditAnswer is STL's verdict: its status (covered or documents_secured when satisfied) and the
// reservation it found — its ID, amount and documents-secured amount in minor units of the order
// currency, basis (within_limit, credit_decision or documents_secured) and the credit decision it
// was reserved under. Refusal is nil when the portion is covered or secured for the order hash
// asked and STL's CREDIT_RESERVATION_REQUIRED otherwise.
type CreditAnswer struct {
	Status        string
	ReservationID string
	ReservedMinor string
	SecuredMinor  string
	Basis         string
	DecisionID    string
	Refusal       error
}

// OrderGuards are the guards of CNX and STL that order.activate and order.amendment.apply call.
// The composed process wires both (trd.New refuses to assemble TRD without them). A Service built
// without one records the gap — the commit evidence says "not_wired" and the event lists the guard
// in unchecked_guards — never a pass.
type OrderGuards struct {
	// Credit is STL's CreditGuard v1 (Context Map R57, TRD-INV-022: a credit reservation covering the
	// part of the payment terms due after shipment; CREDIT_RESERVATION_REQUIRED), called after PTY.
	Credit CreditGuard
	// TradeCompliance is CNX's TradeComplianceGuard v1 (R58, TRD-INV-023: a clear trade compliance
	// conclusion covering the order revision; TRADE_COMPLIANCE_NOT_CLEAR), called after CAT.
	TradeCompliance TradeComplianceGuard
}

// notWired is the evidence value of a guard this Service was built without.
const notWired = api.NotWired

// guardOutcome is what a guard call left for the command: the commit evidence entries, the guard's
// name when it is not wired, and the owner's refusal.
type guardOutcome struct {
	entries   []quote.Entry
	unchecked string
	refusal   error
}

// entryValue returns the value of the first entry called name, "" for none.
func entryValue(entries []quote.Entry, name string) string {
	for _, e := range entries {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// compliance asks CNX whether order revision o is clear at now (TRD-INV-023).
func (s Service) compliance(ctx context.Context, exec bentopg.Executor, o OrderRevisionRef, now time.Time, path string) (guardOutcome, error) {
	if s.Orders.TradeCompliance == nil {
		return guardOutcome{entries: []quote.Entry{{Name: api.GuardCNXTradeCompliance, Value: notWired}}, unchecked: api.GuardCNXTradeCompliance}, nil
	}
	a, err := s.Orders.TradeCompliance(ctx, exec, o, now, path)
	if err != nil {
		return guardOutcome{}, err
	}
	out := guardOutcome{entries: []quote.Entry{{Name: api.GuardCNXTradeCompliance, Value: a.Evidence}}, refusal: a.Refusal}
	regulation, until := "", ""
	if a.RegulationID != "" {
		regulation = a.RegulationID + "@" + itoa(a.RegulationRevision)
	}
	if !a.ValidUntil.IsZero() {
		until = kernel.FormatTimestamp(a.ValidUntil)
	}
	out.entries = append(out.entries, quote.Entry{Name: api.EvidenceComplianceRegulation, Value: regulation},
		quote.Entry{Name: api.EvidenceComplianceValidUntil, Value: until})
	return out, nil
}

// amendedCredit asks STL, in order.amendment.apply, for a reservation of revision next that o's
// current revision amends to (D-9): only when next asks more credit than the current revision.
// A refusal points at /args/amendment_id.
func (s Service) amendedCredit(ctx context.Context, exec bentopg.Executor, o *order.Order, next *orderrev.Revision,
	now time.Time) (guardOutcome, error) {
	cur, err := o.Revision()
	if err != nil {
		return guardOutcome{}, err
	}
	from, err := orderrev.NeedOf(cur.Amounts.Payments)
	if err != nil {
		return guardOutcome{}, err
	}
	to, err := orderrev.NeedOf(next.Amounts.Payments)
	if err != nil {
		return guardOutcome{}, err
	}
	ref := OrderRevisionRef{OrderID: o.ID, OrderRevision: next.Number, OrderHash: next.Hash}
	return s.credit(ctx, exec, ref, to, &from, next.Currency.ID, now, "/args/amendment_id")
}

// credit asks STL for a reservation covering what plan need asks, for order revision o in currency
// (TRD-INV-022). previous is the need of the revision o amends, nil at order.activate: an activation
// asks whenever the plan has a stage due after shipment or invoice (api.GuardNotRequired
// otherwise); an amendment asks only when the amended revision asks more than the one it amends
// (D-9; api.GuardNotIncreased otherwise).
func (s Service) credit(ctx context.Context, exec bentopg.Executor, o OrderRevisionRef, need orderrev.CreditNeed, previous *orderrev.CreditNeed,
	currency string, now time.Time, path string) (guardOutcome, error) {
	amounts := []quote.Entry{{Name: api.EvidenceCreditPostShipment, Value: need.PostShipmentMinor},
		{Name: api.EvidenceCreditPortion, Value: need.PortionMinor}, {Name: api.EvidenceCreditCurrency, Value: currency}}
	if previous != nil {
		amounts = append(amounts, quote.Entry{Name: api.EvidenceCreditPreviousPost, Value: previous.PostShipmentMinor},
			quote.Entry{Name: api.EvidenceCreditPreviousPortion, Value: previous.PortionMinor})
	}
	named := func(value string) []quote.Entry {
		return append([]quote.Entry{{Name: api.GuardSTLCredit, Value: value}}, amounts...)
	}
	switch {
	case !need.Required():
		return guardOutcome{entries: named(api.GuardNotRequired)}, nil
	case previous != nil && !need.Grows(*previous):
		return guardOutcome{entries: named(api.GuardNotIncreased)}, nil
	case s.Orders.Credit == nil:
		return guardOutcome{entries: named(notWired), unchecked: api.GuardSTLCredit}, nil
	}
	a, err := s.Orders.Credit(ctx, exec, CreditQuery{Order: o, PortionMinor: need.PortionMinor, Currency: currency, At: now, Path: path})
	if err != nil {
		return guardOutcome{}, err
	}
	out := guardOutcome{entries: named(a.Status), refusal: a.Refusal}
	out.entries = append(out.entries, quote.Entry{Name: api.EvidenceCreditReservation, Value: a.ReservationID},
		quote.Entry{Name: api.EvidenceCreditReserved, Value: a.ReservedMinor}, quote.Entry{Name: api.EvidenceCreditSecured, Value: a.SecuredMinor},
		quote.Entry{Name: api.EvidenceCreditBasis, Value: a.Basis}, quote.Entry{Name: api.EvidenceCreditDecision, Value: a.DecisionID})
	return out, nil
}
