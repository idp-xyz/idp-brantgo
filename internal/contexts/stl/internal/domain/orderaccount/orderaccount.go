// Package orderaccount holds STL's OrderAccount rules (tactical model STL-AGG-OrderAccount,
// STL-INV-003; UL STL-PaymentConditionStatus, UL-INV-28; S5 plan D-4, D-5; Context Map R45).
//
// An order account is one root per order: the payment stages of the order revision it was opened
// at (TRD OrderCommercialSnapshot v1, R32) and, derived from the applications that stand, what is
// applied to each. Whether an order's payment condition is met for a scope is derived from it:
// scope shipment needs every stage due on order confirmation or before shipment fully applied;
// scope documents needs those and every post-shipment stage under document control (D-5). The
// payment plan is not money received (MD-37): only applied receipts count, and an unexplained
// difference never does, because applications cannot use it (fundrecord.Available).
// PaymentConditionGuard v1 is the SQL form of Condition (migrations/stl/000002).
package orderaccount

import (
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Stage triggers (POL PaymentStage) and release controls.
const (
	TriggerOrderConfirmed = "order_confirmed"
	TriggerBeforeShipment = "before_shipment"
	TriggerAfterShipment  = "after_shipment"
	TriggerAfterInvoice   = "after_invoice"
	ReleaseDocuments      = "documents"
)

// Scopes of a payment condition (S5-07; FUL CommercialRelease scopes).
const (
	ScopeShipment  = "shipment"
	ScopeDocuments = "documents"
)

// Verdicts and their reasons.
const (
	Satisfied         = "satisfied"
	Blocked           = "blocked"
	ReasonNoAccount   = "no_account"
	ReasonRevision    = "revision_mismatch"
	ReasonStageUnpaid = "stage_unpaid"
	StatusOpen        = "open"
)

// Stage is one payment stage of the account's order revision.
type Stage struct {
	StageID        string
	Seq            int
	Trigger        string
	Days           int
	Percent        string
	Amount         kernel.Money
	ReleaseControl string
}

// Account is the root of STL-AGG-OrderAccount.
type Account struct {
	OrderID          string
	OrderRevision    int64
	OrderHash        string
	PartyID          string
	CurrencyRevision int64
	Status           string
	Version          int64
	Stages           []Stage
	OwnerActor       string
	OwnerTeam        string
	Opened           fundrecord.Act
}

// Currency is the account's currency.
func (a *Account) Currency() kernel.Currency {
	if len(a.Stages) == 0 {
		return kernel.Currency{}
	}
	return a.Stages[0].Amount.Currency()
}

// Stage returns the stage with id, or nil.
func (a *Account) Stage(id string) *Stage {
	for i := range a.Stages {
		if a.Stages[i].StageID == id {
			return &a.Stages[i]
		}
	}
	return nil
}

// Triggers TRD's payment plans use.
func validTrigger(t string) bool {
	switch t {
	case TriggerOrderConfirmed, TriggerBeforeShipment, TriggerAfterShipment, TriggerAfterInvoice:
		return true
	}
	return false
}

// Open is order_account.open's rule: an account of an order revision with at least one stage, each
// with a known trigger and release control, a non-negative amount in the order currency and an ID
// of its own.
func Open(orderID string, revision int64, hash, party string, currencyRevision int64, stages []Stage, by fundrecord.Act, team string) (*Account, error) {
	if err := checkStages(stages); err != nil {
		return nil, err
	}
	return &Account{OrderID: orderID, OrderRevision: revision, OrderHash: hash, PartyID: party, CurrencyRevision: currencyRevision,
		Status: StatusOpen, Version: 1, Stages: stages, OwnerActor: by.By.ID, OwnerTeam: team, Opened: by}, nil
}

// checkStages checks an order revision's stages as Open takes them and numbers them in plan order.
func checkStages(stages []Stage) error {
	if len(stages) == 0 {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "TRD's order revision has no payment stages")
	}
	seen := map[string]bool{}
	c := stages[0].Amount.Currency()
	for i, s := range stages {
		switch {
		case seen[s.StageID] || !kernel.ValidIdentifier(s.StageID):
			return kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "TRD's payment stage IDs are not distinct identifiers")
		case !validTrigger(s.Trigger) || (s.ReleaseControl != "" && s.ReleaseControl != ReleaseDocuments) || s.Days < 0:
			return kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "TRD's payment stage "+s.StageID+" is not one STL knows")
		case s.Amount.Currency() != c || s.Amount.Sign() < 0:
			return kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "TRD's payment stage "+s.StageID+" has no amount")
		}
		seen[s.StageID] = true
		stages[i].Seq = i + 1
	}
	return nil
}

// Required reports whether scope needs stage s paid (STL-INV-003, D-5).
func Required(scope string, s Stage) bool {
	switch s.Trigger {
	case TriggerOrderConfirmed, TriggerBeforeShipment:
		return true
	case TriggerAfterShipment, TriggerAfterInvoice:
		return scope == ScopeDocuments && s.ReleaseControl == ReleaseDocuments
	}
	return false
}

// OpenOf returns what is still open of stage s given applied, the applications standing on it.
func OpenOf(s Stage, applied kernel.Money) kernel.Money {
	m, err := s.Amount.Sub(applied)
	if err != nil {
		panic("stl: stage amounts in one currency: " + err.Error())
	}
	return m
}

// Verdict is the payment condition of an order revision for a scope: Status satisfied or blocked,
// Reason, the totals of the required stages and, when blocked by a stage, the first unmet one.
type Verdict struct {
	Scope         string
	Status        string
	Reason        string
	OrderRevision int64
	OrderHash     string
	Required      kernel.Money
	Applied       kernel.Money
	Stage         *Stage
	StageApplied  kernel.Money
	Shortfall     kernel.Money
	CheckedAt     time.Time
}

// Condition judges a's payment condition for scope at revision, applied holding the amount standing
// on each stage (absent: none); a nil account is blocked (no_account).
func Condition(a *Account, revision int64, scope string, applied map[string]kernel.Money, at time.Time) Verdict {
	v := Verdict{Scope: scope, Status: Blocked, CheckedAt: at}
	if a == nil {
		v.Reason = ReasonNoAccount
		return v
	}
	v.OrderRevision, v.OrderHash = a.OrderRevision, a.OrderHash
	if a.OrderRevision != revision {
		v.Reason = ReasonRevision
		return v
	}
	c := a.Currency()
	v.Required, _ = kernel.NewMoney(c, 0)
	v.Applied = v.Required
	for i := range a.Stages {
		s := a.Stages[i]
		if !Required(scope, s) {
			continue
		}
		got, ok := applied[s.StageID]
		if !ok {
			got, _ = kernel.NewMoney(c, 0)
		}
		v.Required, _ = v.Required.Add(s.Amount)
		counted := got
		if c, _ := got.Cmp(s.Amount); c > 0 {
			// A stage counts at most its due: a payment plan review may shrink a stage below what
			// stands on it, and the excess must not hide another stage's shortfall (C1's finding).
			counted = s.Amount
		}
		v.Applied, _ = v.Applied.Add(counted)
		if short := OpenOf(s, got); v.Stage == nil && short.Sign() > 0 {
			v.Stage, v.StageApplied, v.Shortfall = &a.Stages[i], got, short
		}
	}
	if v.Stage != nil {
		v.Reason = ReasonStageUnpaid
		return v
	}
	v.Status = Satisfied
	return v
}
