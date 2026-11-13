package settlement

import (
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Payable adjustments (S5R unit R8; simulated sign-off E0 of 2026-11-13: S5R-D20; tactical model
// STL-AGG-SettlementStatement entity PayableAdjustment, STL-INV-013; Context Map R55 CNX→STL PurchaseInvoiceRef v1).
//
// A payable changes only by PUR's delivery correction (its quantity: the purchase statement is reversed and settled
// again, S5R-D46(4)) or by a payable adjustment: a price allowance the supplier granted or an amount the supplier
// invoiced less (under-invoicing), each referencing the CNX purchase invoice that evidences it — a red invoice for an
// allowance. Treasury requests it on a confirmed purchase statement, a credit manager other than the requester
// confirms or rejects it (INDEPENDENT_REVIEW_REQUIRED). A confirmed adjustment credits the purchase side like a credit
// note the sales side (STL-INV-008): it reduces what is open on the payable at once and, for its net part, the
// purchase cost of the order result. A re-issued invoice of the same amount changes nothing; no consumer of CNX's
// invoice events adjusts a payable: the cause of a red invoice needs a person's judgement.

// Kinds, decisions and states of a payable adjustment.
const (
	AdjustmentPriceAllowance = "price_allowance"
	AdjustmentUnderInvoiced  = "under_invoiced"
	AdjustmentRequested      = "requested"
	AdjustmentConfirmed      = "confirmed"
	AdjustmentRejected       = "rejected"
)

// PayableAdjustment is a payable adjustment of a purchase statement (entity of STL-AGG-SettlementStatement).
type PayableAdjustment struct {
	ID                string
	StatementID       string
	Kind              string
	Status            string
	Version           int64
	Amount            kernel.Money
	PurchaseInvoiceID string
	Basis             string
	EvidenceIDs       []string
	OwnerActor        string
	OwnerTeam         string
	Requested         fundrecord.Act
	Decided           *fundrecord.Act // with the decision's reason
}

// Effective reports whether the adjustment counts against the payable: confirmed, on a statement that stands.
func (a *PayableAdjustment) Effective(st *Statement) bool {
	return a.Status == AdjustmentConfirmed && st.Status == StatusConfirmed
}

// Split splits the adjustment into its net and tax parts by the statement's net and tax (S5R-D46(2)).
func (a *PayableAdjustment) Split(st *Statement) (net, tax kernel.Money) {
	return SplitTax(a.Amount, st)
}

// AdjustmentInput is a payable_adjustment.request as the rules see it, with the statement read under its lock and
// what stands on its installment — payments and confirmed adjustments (applied).
type AdjustmentInput struct {
	ID                string
	Statement         *Statement
	Kind              string
	Amount            kernel.Money
	PurchaseInvoiceID string
	Basis             string
	EvidenceIDs       []string
	Applied           map[int]kernel.Money
	By                fundrecord.Act
	Team              string
}

func exceedsPayable(amount, open kernel.Money) error {
	if c, _ := amount.Cmp(open); c > 0 {
		o := open
		if o.Sign() < 0 {
			o = must(kernel.NewMoney(open.Currency(), 0))
		}
		return fundrecord.Reject(fundrecord.CodeApplicationExceedsOpen, "/args/amount", "Only "+o.String()+" of the payable is open")
	}
	return nil
}

// RequestAdjustment is payable_adjustment.request's rule (STL-INV-013): an adjustment of a confirmed purchase
// statement, in its currency, above zero and within what is open on the payable (APPLICATION_EXCEEDS_OPEN).
func RequestAdjustment(in AdjustmentInput) (*PayableAdjustment, error) {
	st := in.Statement
	switch {
	case st.Kind != KindPurchase:
		return nil, state("/args/statement_id", "A payable adjustment adjusts a payable: "+st.ID+" is a sales statement")
	case st.Status != StatusConfirmed:
		return nil, state("/args/statement_id", "The statement is "+st.Status)
	case in.Kind != AdjustmentPriceAllowance && in.Kind != AdjustmentUnderInvoiced:
		return nil, invalid("/args/kind", "A payable adjustment is a price_allowance or under_invoiced")
	case in.Amount.Currency() != st.Currency() || in.Amount.Sign() <= 0:
		return nil, invalid("/args/amount", "The amount is above zero")
	}
	if err := exceedsPayable(in.Amount, st.Open(in.Applied)); err != nil {
		return nil, err
	}
	return &PayableAdjustment{ID: in.ID, StatementID: st.ID, Kind: in.Kind, Status: AdjustmentRequested, Version: 1, Amount: in.Amount,
		PurchaseInvoiceID: in.PurchaseInvoiceID, Basis: in.Basis, EvidenceIDs: in.EvidenceIDs, OwnerActor: in.By.By.ID, OwnerTeam: in.Team,
		Requested: in.By}, nil
}

// Decide is payable_adjustment.confirm's rule: a requested adjustment is confirmed or rejected once, with a reason, by a
// person other than its requester (INDEPENDENT_REVIEW_REQUIRED; independence row payable_adjustment.confirm
// independentOf 调整申请人); confirming needs its statement to stand and the amount within open, what is open on the
// payable now.
func (a *PayableAdjustment) Decide(decision string, st *Statement, open kernel.Money, by fundrecord.Act) error {
	switch {
	case a.Status != AdjustmentRequested:
		return state("/args/adjustment_id", "The payable adjustment is already "+a.Status)
	case by.By.ID == a.Requested.By.ID:
		return fundrecord.Reject(fundrecord.CodeIndependentReviewRequired, "/args/adjustment_id",
			"The requester of the payable adjustment does not decide it")
	case decision != DecisionConfirm && decision != DecisionReject:
		return invalid("/args/decision", "The decision is confirm or reject")
	}
	if decision == DecisionConfirm {
		if st.Status != StatusConfirmed {
			return state("/args/adjustment_id", "Statement "+st.ID+" is "+st.Status)
		}
		if err := exceedsPayable(a.Amount, open); err != nil {
			return err
		}
		a.Status = AdjustmentConfirmed
	} else {
		a.Status = AdjustmentRejected
	}
	a.Version, a.Decided = a.Version+1, &by
	return nil
}
