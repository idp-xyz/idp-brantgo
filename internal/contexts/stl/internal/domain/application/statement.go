package application

import (
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Applications to business receivables and payables (S5-04b; STL-INV-002, STL-INV-003; tactical
// model STL-AGG-Application "目标为订单付款阶段（预收）或结算单应收应付"; S5 plan D-4).
//
// A receivable application targets one installment of a confirmed sales statement and keeps the
// stage that installment is due for, so it counts for that stage as well (PaymentConditionGuard v1
// sums the order's standing applications by stage): what the customer pays after shipment on a
// document-controlled stage still opens the documents. A payable application targets the one
// installment of a confirmed purchase statement, from a payment to its supplier.

// TargetOf is the application target kind of a statement's kind.
func TargetOf(kind string) string {
	if kind == settlement.KindPurchase {
		return TargetPayable
	}
	return TargetReceivable
}

// StatementInput is an application.apply to a statement as the rules see it, with the fund record,
// the statement and what stands on each of its installments (by seq) read under their locks.
// StageID picks a receivable's installment ("" the first one still open, in due order). FX is the
// settlement rate named for a fund in another currency than the statement's (STL-INV-007; nil: none).
type StatementInput struct {
	ID          string
	Fund        *fundrecord.FundRecord
	FundEntries fundrecord.Entries
	Statement   *settlement.Statement
	StageID     string
	Applied     map[int]kernel.Money
	Amount      kernel.Money
	FX          *FXInput
	Payers      Payers // S5R-D21: what lets another payer than a receivable's customer pay
	By          fundrecord.Act
	Team        string
}

func zero(c kernel.Currency) kernel.Money {
	z, _ := kernel.NewMoney(c, 0)
	return z
}

func appliedOn(st *settlement.Statement, i *settlement.Installment, applied map[int]kernel.Money) kernel.Money {
	if a, ok := applied[i.Seq]; ok {
		return a
	}
	return zero(st.Currency())
}

// installment picks the installment of statement st an application targets: of stage, or the first one
// still open ("" stage).
func installment(st *settlement.Statement, stage string, applied map[int]kernel.Money) (*settlement.Installment, error) {
	if stage != "" {
		inst := st.InstallmentOf(stage)
		if inst == nil {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/target/stage_id", "The receivable has no installment of this payment stage")
		}
		return inst, nil
	}
	for i := range st.Installments {
		if x := &st.Installments[i]; settlement.OpenOf(*x, appliedOn(st, x, applied)).Sign() > 0 {
			return x, nil
		}
	}
	return nil, fundrecord.Reject(fundrecord.CodeApplicationExceedsOpen, "/args/amount", "Nothing of "+st.ID+" is open")
}

// withinOpen refuses target more than is open on installment inst (APPLICATION_EXCEEDS_OPEN).
func withinOpen(target kernel.Money, st *settlement.Statement, inst *settlement.Installment, applied map[int]kernel.Money) error {
	open := settlement.OpenOf(*inst, appliedOn(st, inst, applied))
	if c, _ := target.Cmp(open); c > 0 {
		what := "the payable"
		if inst.StageID != "" {
			what = "the installment of stage " + inst.StageID
		} else if TargetOf(st.Kind) == TargetReceivable {
			what = "the installment beyond the payment plan"
		}
		return fundrecord.Reject(fundrecord.CodeApplicationExceedsOpen, "/args/amount", "Only "+open.String()+" of "+what+" is open")
	}
	return nil
}

// withinAvailable refuses more than fund f has available (APPLICATION_EXCEEDS_AVAILABLE, or
// FUND_DIFFERENCE_UNEXPLAINED when only its unexplained difference would cover it).
func withinAvailable(amount kernel.Money, f *fundrecord.FundRecord, e fundrecord.Entries) error {
	available := f.Available(e)
	if c, _ := amount.Cmp(available); c > 0 {
		covered, err := available.Add(f.Unexplained())
		if c2, _ := amount.Cmp(covered); err == nil && c2 <= 0 {
			return fundrecord.Reject(fundrecord.CodeFundDifferenceUnexplained, "/args/amount",
				"Only "+available.String()+" is available: "+f.Unexplained().String()+" of the difference is unexplained")
		}
		return fundrecord.Reject(fundrecord.CodeApplicationExceedsAvailable, "/args/amount", "Only "+available.String()+" is available")
	}
	return nil
}

// ApplyToStatement applies application.apply's rules to a receivable or payable: a receipt of the
// statement's customer to a receivable, a payment to its supplier to a payable, in its currency — or,
// at a settlement rate, in another (STL-INV-007) — within what the fund has available and what is open
// on the installment.
func ApplyToStatement(in StatementInput) (*Application, error) {
	f, st := in.Fund, in.Statement
	target := TargetOf(st.Kind)
	switch {
	case st.Status != settlement.StatusConfirmed:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/target/statement_id", "The statement is "+st.Status)
	case target == TargetReceivable && !f.Applicable():
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/fund_record_id",
			"A "+f.Kind+" is not applied to receivables: only receipts are (non-goods money waits for verification)")
	case target == TargetPayable && f.Kind != fundrecord.KindPayment:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/fund_record_id", "A "+f.Kind+" is not applied to payables: only payments are")
	case f.Currency() != st.Currency() && in.FX == nil:
		return nil, fundrecord.Reject(fundrecord.CodeFXScopeMismatch, "/args/fx_rate",
			"The fund is in "+f.Currency().Code()+", the statement in "+st.Currency().Code()+": cross-currency write-off needs a settlement rate")
	case target == TargetPayable && f.CounterpartyID != st.CounterpartyID:
		return nil, fundrecord.Reject(fundrecord.CodePartyEntityMismatch, "/args/fund_record_id",
			"The fund's party "+f.CounterpartyID+" is not the statement's "+st.CounterpartyID)
	}
	payer, profile := PayerCustomer, int64(0)
	if target == TargetReceivable {
		var err error
		if payer, profile, err = payerOf(f, st.CounterpartyID, in.Payers); err != nil {
			return nil, err
		}
	}
	inst, err := installment(st, in.StageID, in.Applied)
	if err != nil {
		return nil, err
	}
	if in.Amount.Currency() != f.Currency() || in.Amount.Sign() <= 0 {
		return nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/amount", "The amount is above zero")
	}
	fx := in.FX
	if fx != nil && target == TargetPayable {
		fx = &FXInput{Rate: fx.Rate, Use: fx.Use} // a payable has no order analysis rate
	}
	amount, conv, err := convert(in.Amount, st.Currency(), fx)
	if err != nil {
		return nil, err
	}
	if err = withinAvailable(in.Amount, f, in.FundEntries); err != nil {
		return nil, err
	}
	if err = withinOpen(amount, st, inst, in.Applied); err != nil {
		return nil, err
	}
	x := &Application{ID: in.ID, FundRecordID: f.ID, TargetKind: target, StatementID: st.ID, InstallmentSeq: inst.Seq, Amount: amount,
		Conversion: conv, Status: StatusApplied, Version: 1, OwnerActor: in.By.By.ID, OwnerTeam: in.Team, Applied: in.By, PayerBasis: payer,
		PayerProfileRevision: profile}
	if target == TargetReceivable {
		x.OrderID, x.OrderRevision, x.StageID = st.OrderID, st.OrderRevision, inst.StageID
	}
	return x, nil
}

// Move is what confirming a sales statement does to one stage application (D-4): From, now moved,
// is replaced by Replacement on the receivable's installment of its stage and, when only part of it
// fits there, by Remainder on the stage.
type Move struct {
	From        *Application
	Replacement *Application
	Remainder   *Application
}

// MoveToReceivable moves the standing stage applications of a new sales statement's order to its
// receivable (D-4): for each installment of a stage, in plan order, the stage's standing applications
// in the order they were made move until the installment is covered; the last one that fits only in
// part leaves its remainder on the stage at the revision it was made at. standing are the order's
// stage applications that stand, newID names the new applications. Funds, stages and amounts are
// kept, so the fund's available balance and what stands on each stage do not change.
func MoveToReceivable(st *settlement.Statement, standing []Application, newID func() string, by fundrecord.Act, team string) []Move {
	var out []Move
	used := map[string]bool{}
	for _, inst := range st.Installments {
		if inst.StageID == "" {
			continue
		}
		need := inst.Amount
		for i := range standing {
			from := &standing[i]
			if need.Sign() <= 0 {
				break
			}
			if used[from.ID] || from.Status != StatusApplied || from.TargetKind != TargetOrderStage || from.OrderID != st.OrderID ||
				from.StageID != inst.StageID {
				continue
			}
			used[from.ID] = true
			part := from.Amount
			if c, _ := part.Cmp(need); c > 0 {
				part = need
			}
			need, _ = need.Sub(part)
			m := Move{From: from}
			var moved, kept *Conversion // a cross-currency prepayment moves its conversion in proportion (C8)
			if from.Conversion != nil {
				moved, kept = from.Conversion.part(part, from.Amount)
			}
			m.Replacement = &Application{ID: newID(), FundRecordID: from.FundRecordID, TargetKind: TargetReceivable, OrderID: st.OrderID,
				OrderRevision: st.OrderRevision, StageID: inst.StageID, StatementID: st.ID, InstallmentSeq: inst.Seq, MovedFrom: from.ID,
				Amount: part, Conversion: moved, Status: StatusApplied, Version: 1, OwnerActor: by.By.ID, OwnerTeam: team, Applied: by,
				PayerBasis: from.PayerBasis, PayerProfileRevision: from.PayerProfileRevision} // S5R-D21: the payer keeps its basis
			if rest, _ := from.Amount.Sub(part); rest.Sign() > 0 {
				m.Remainder = &Application{ID: newID(), FundRecordID: from.FundRecordID, TargetKind: TargetOrderStage, OrderID: from.OrderID,
					OrderRevision: from.OrderRevision, StageID: from.StageID, MovedFrom: from.ID, Amount: rest, Conversion: kept, Status: StatusApplied,
					Version: 1, OwnerActor: by.By.ID, OwnerTeam: team, Applied: by, PayerBasis: from.PayerBasis,
					PayerProfileRevision: from.PayerProfileRevision}
			}
			from.Status, from.Version = StatusMoved, from.Version+1
			out = append(out, m)
		}
	}
	return out
}
