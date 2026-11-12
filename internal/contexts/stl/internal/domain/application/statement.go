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
// StageID picks a receivable's installment ("" the first one still open, in due order).
type StatementInput struct {
	ID          string
	Fund        *fundrecord.FundRecord
	FundEntries fundrecord.Entries
	Statement   *settlement.Statement
	StageID     string
	Applied     map[int]kernel.Money
	Amount      kernel.Money
	By          fundrecord.Act
	Team        string
}

func zero(c kernel.Currency) kernel.Money {
	z, _ := kernel.NewMoney(c, 0)
	return z
}

// ApplyToStatement applies application.apply's rules to a receivable or payable: a receipt of the
// statement's customer to a receivable, a payment to its supplier to a payable, in its currency,
// within what the fund has available and what is open on the installment.
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
	case f.Currency() != st.Currency():
		return nil, fundrecord.Reject(fundrecord.CodeFXScopeMismatch, "/args/fund_record_id",
			"The fund is in "+f.Currency().Code()+", the statement in "+st.Currency().Code()+": cross-currency write-off needs a settlement rate")
	case f.CounterpartyID != st.CounterpartyID:
		return nil, fundrecord.Reject(fundrecord.CodePartyEntityMismatch, "/args/fund_record_id",
			"The fund's party "+f.CounterpartyID+" is not the statement's "+st.CounterpartyID)
	}
	applied := func(i *settlement.Installment) kernel.Money {
		if a, ok := in.Applied[i.Seq]; ok {
			return a
		}
		return zero(st.Currency())
	}
	var inst *settlement.Installment
	if in.StageID != "" {
		if inst = st.InstallmentOf(in.StageID); inst == nil {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/target/stage_id", "The receivable has no installment of this payment stage")
		}
	} else {
		for i := range st.Installments {
			if x := &st.Installments[i]; settlement.OpenOf(*x, applied(x)).Sign() > 0 {
				inst = x
				break
			}
		}
		if inst == nil {
			return nil, fundrecord.Reject(fundrecord.CodeApplicationExceedsOpen, "/args/amount", "Nothing of "+st.ID+" is open")
		}
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
	open := settlement.OpenOf(*inst, applied(inst))
	if c, _ := in.Amount.Cmp(open); c > 0 {
		what := "the payable"
		if inst.StageID != "" {
			what = "the installment of stage " + inst.StageID
		} else if target == TargetReceivable {
			what = "the installment beyond the payment plan"
		}
		return nil, fundrecord.Reject(fundrecord.CodeApplicationExceedsOpen, "/args/amount", "Only "+open.String()+" of "+what+" is open")
	}
	x := &Application{ID: in.ID, FundRecordID: f.ID, TargetKind: target, StatementID: st.ID, InstallmentSeq: inst.Seq, Amount: in.Amount,
		Status: StatusApplied, Version: 1, OwnerActor: in.By.By.ID, OwnerTeam: in.Team, Applied: in.By}
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
			m.Replacement = &Application{ID: newID(), FundRecordID: from.FundRecordID, TargetKind: TargetReceivable, OrderID: st.OrderID,
				OrderRevision: st.OrderRevision, StageID: inst.StageID, StatementID: st.ID, InstallmentSeq: inst.Seq, MovedFrom: from.ID,
				Amount: part, Status: StatusApplied, Version: 1, OwnerActor: by.By.ID, OwnerTeam: team, Applied: by}
			if rest, _ := from.Amount.Sub(part); rest.Sign() > 0 {
				m.Remainder = &Application{ID: newID(), FundRecordID: from.FundRecordID, TargetKind: TargetOrderStage, OrderID: from.OrderID,
					OrderRevision: from.OrderRevision, StageID: from.StageID, MovedFrom: from.ID, Amount: rest, Status: StatusApplied, Version: 1,
					OwnerActor: by.By.ID, OwnerTeam: team, Applied: by}
			}
			from.Status, from.Version = StatusMoved, from.Version+1
			out = append(out, m)
		}
	}
	return out
}
