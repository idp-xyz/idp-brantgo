// Package application holds STL's Application rules (tactical model STL-AGG-Application,
// SM-STL-APPLICATION, STL-INV-002, STL-INV-003, STL-INV-006; UL STL-Application, UL-INV-22; S5 plan
// D-3, D-4; baseline 4.2, 4.3, 4.6 S-A).
//
// An application allocates part of a receipt's available balance to a target of the same currency:
// a payment stage of an order account (a prepayment, before its goods are settled; D-4), an
// installment of a business receivable or a payable (statement.go, S5-04b); confirming a sales
// statement moves the order's stage applications to its receivable (Move). An application
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
	TargetReceivable = "receivable"
	TargetPayable    = "payable"
	StatusApplied    = "applied"
	StatusUnapplied  = "unapplied"
	StatusMoved      = "moved"
)

// Application is the root of STL-AGG-Application.
type Application struct {
	ID             string
	FundRecordID   string
	TargetKind     string
	OrderID        string
	OrderRevision  int64
	StageID        string
	StatementID    string // a receivable or payable: its statement and installment
	InstallmentSeq int
	MovedFrom      string       // a replacement or remainder: the moved application it comes from
	CreditNoteID   string       // the source is a confirmed credit note instead of a fund record (C8, source.go)
	Amount         kernel.Money // what the target receives, in the target's currency
	Conversion     *Conversion  // a fund in another currency: what it gives and at which rate (STL-INV-007)
	Status         string
	Version        int64
	OwnerActor     string
	OwnerTeam      string
	Applied        fundrecord.Act
	Unapplied      *fundrecord.Act
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
	FX           *FXInput // the settlement rate for a receipt in another currency than the order's (nil: none)
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
	case f.Currency() != acc.Currency() && in.FX == nil:
		return nil, fundrecord.Reject(fundrecord.CodeFXScopeMismatch, "/args/fx_rate",
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
	amount, conv, err := convert(in.Amount, acc.Currency(), in.FX)
	if err != nil {
		return nil, err
	}
	if err = withinAvailable(in.Amount, f, in.FundEntries); err != nil {
		return nil, err
	}
	open := orderaccount.OpenOf(*stage, in.StageApplied)
	if c, _ := amount.Cmp(open); c > 0 {
		return nil, fundrecord.Reject(fundrecord.CodeApplicationExceedsOpen, "/args/amount", "Only "+open.String()+" of stage "+stage.StageID+" is open")
	}
	return &Application{ID: in.ID, FundRecordID: f.ID, TargetKind: TargetOrderStage, OrderID: acc.OrderID, OrderRevision: acc.OrderRevision,
		StageID: stage.StageID, Amount: amount, Conversion: conv, Status: StatusApplied, Version: 1, OwnerActor: in.By.By.ID, OwnerTeam: in.Team,
		Applied: in.By}, nil
}

// Unapply undoes a standing application (SM-STL-APPLICATION: authorised person and reason). A moved
// application stands no more: its replacement on the receivable is what may be unapplied.
func (a *Application) Unapply(by fundrecord.Act) error {
	if a.Status == StatusMoved {
		return kernel.Reject(kernel.CodeInvalidState, "/args/application_id",
			"The application moved to a receivable when its goods were settled: unapply the receivable's application instead")
	}
	if a.Status != StatusApplied {
		return kernel.Reject(kernel.CodeInvalidState, "/args/application_id", "The application is already unapplied")
	}
	a.Status, a.Version, a.Unapplied = StatusUnapplied, a.Version+1, &by
	return nil
}
