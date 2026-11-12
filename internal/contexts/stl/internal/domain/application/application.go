// Package application holds STL's Application rules (tactical model STL-AGG-Application,
// SM-STL-APPLICATION, STL-INV-002, STL-INV-003, STL-INV-006; UL STL-Application, UL-INV-22; S5 plan
// D-3, D-4; baseline 4.2, 4.3, 4.6 S-A).
//
// An application allocates part of a receipt's available balance to a target of the same currency:
// in S5-04a a payment stage of an order account (a prepayment, before any receivable exists; D-4).
// B2b adds receivables of confirmed statements and moves stage applications to them. An application
// never exceeds what the receipt has available (APPLICATION_EXCEEDS_AVAILABLE, or
// FUND_DIFFERENCE_UNEXPLAINED when only the unexplained difference would cover it) nor what is open
// on the target (APPLICATION_EXCEEDS_OPEN); the payer must be the order's customer
// (PARTY_ENTITY_MISMATCH: pending verification, CN-AT08); other currencies are STL-INV-007's
// cross-currency write-off (unit C8). Unapplying writes a new record and keeps the fund fact. This
// is commercial allocation, never the administrative 出口收汇核销 (CN-R11).
package application

import (
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderaccount"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Target kinds and states (SM-STL-APPLICATION).
const (
	TargetOrderStage = "order_stage"
	StatusApplied    = "applied"
	StatusUnapplied  = "unapplied"
)

// Application is the root of STL-AGG-Application.
type Application struct {
	ID            string
	FundRecordID  string
	TargetKind    string
	OrderID       string
	OrderRevision int64
	StageID       string
	Amount        kernel.Money
	Status        string
	Version       int64
	OwnerActor    string
	OwnerTeam     string
	Applied       fundrecord.Act
	Unapplied     *fundrecord.Act
}

// Stands reports whether the application counts.
func (a *Application) Stands() bool { return a.Status == StatusApplied }

// ApplyInput is an application.apply request as the rules see it, with the fund record and the
// order account read under their locks: FundEntries are the fund's entries, StageApplied what
// stands on the stage now.
type ApplyInput struct {
	ID           string
	Fund         *fundrecord.FundRecord
	FundEntries  fundrecord.Entries
	Account      *orderaccount.Account
	StageID      string
	StageApplied kernel.Money
	Amount       kernel.Money
	By           fundrecord.Act
	Team         string
}

// Apply applies application.apply's rules and returns the new application.
func Apply(in ApplyInput) (*Application, error) {
	f, acc := in.Fund, in.Account
	switch {
	case !f.Applicable():
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/fund_record_id",
			"A "+f.Kind+" is not applied to orders: only receipts are (non-goods money waits for verification)")
	case f.Currency() != acc.Currency():
		return nil, fundrecord.Reject(fundrecord.CodeFXScopeMismatch, "/args/fund_record_id",
			"The receipt is in "+f.Currency().Code()+", the order in "+acc.Currency().Code()+": cross-currency write-off needs a settlement rate")
	case f.CounterpartyID != acc.PartyID:
		return nil, fundrecord.Reject(fundrecord.CodePartyEntityMismatch, "/args/fund_record_id",
			"The payer "+f.CounterpartyID+" is not the order's customer "+acc.PartyID+": verify the payment basis first")
	}
	stage := acc.Stage(in.StageID)
	if stage == nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/target/stage_id", "The order account has no such payment stage")
	}
	if in.Amount.Currency() != f.Currency() || in.Amount.Sign() <= 0 {
		return nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/amount", "The amount is above zero")
	}
	available := f.Available(in.FundEntries)
	if c, _ := in.Amount.Cmp(available); c > 0 {
		covered, err := available.Add(f.Unexplained())
		if c2, _ := in.Amount.Cmp(covered); err == nil && c2 <= 0 {
			return nil, fundrecord.Reject(fundrecord.CodeFundDifferenceUnexplained, "/args/amount",
				"Only "+available.String()+" is available: "+f.Unexplained().String()+" of the difference is unexplained")
		}
		return nil, fundrecord.Reject(fundrecord.CodeApplicationExceedsAvailable, "/args/amount", "Only "+available.String()+" is available")
	}
	open := orderaccount.OpenOf(*stage, in.StageApplied)
	if c, _ := in.Amount.Cmp(open); c > 0 {
		return nil, fundrecord.Reject(fundrecord.CodeApplicationExceedsOpen, "/args/amount", "Only "+open.String()+" of stage "+stage.StageID+" is open")
	}
	return &Application{ID: in.ID, FundRecordID: f.ID, TargetKind: TargetOrderStage, OrderID: acc.OrderID, OrderRevision: acc.OrderRevision,
		StageID: stage.StageID, Amount: in.Amount, Status: StatusApplied, Version: 1, OwnerActor: in.By.By.ID, OwnerTeam: in.Team, Applied: in.By}, nil
}

// Unapply undoes a standing application (SM-STL-APPLICATION: authorised person and reason).
func (a *Application) Unapply(by fundrecord.Act) error {
	if a.Status != StatusApplied {
		return kernel.Reject(kernel.CodeInvalidState, "/args/application_id", "The application is already unapplied")
	}
	a.Status, a.Version, a.Unapplied = StatusUnapplied, a.Version+1, &by
	return nil
}
