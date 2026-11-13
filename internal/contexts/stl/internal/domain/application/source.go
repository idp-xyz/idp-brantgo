package application

import (
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Sources other than a fund in the target's currency (S5 unit C8, WBS S5-18; tactical model STL-AGG-Application
// "核销来源为资金记录或已确认贷项…跨币种按 STL-INV-007"; STL-INV-007, STL-INV-008; UL-INV-22; MD-10; baseline 4.5).
//
// A fund in another currency than its target is converted at a POL fx_rate of purpose settlement for exactly
// that direction (no inverse, no cross): the application keeps what the fund gives (Conversion.Source, which its
// available balance answers for) and what the target receives (Amount: Source × rate, HALF_UP at the target's
// minor units, which the open amount answers for), the rate, and — for an order's target — the order's analysis
// rate with the business settlement FX difference against it, in the analysis currency: never a receipt or
// revenue, an item of the order result (unit C9). A confirmed credit note is a source for the receivable of its
// statement, in its currency.

// RateUse is the POL fx_rate revision a conversion adopted, with its content hash.
type RateUse struct {
	ID       string
	Revision int64
	Hash     string
}

// AnalysisBasis is the order's analysis rate as TRD sealed it (AnalysisFX): one unit of the order currency is
// Rate units of Currency, the comparison currency (exactly 1 when they are the same).
type AnalysisBasis struct {
	OrderCurrency string
	Currency      kernel.Currency
	Rate          kernel.Decimal
}

// Analysis is the order's analysis basis a conversion compared with, and the business settlement FX
// difference in Currency: what the money is worth in the analysis currency less what the target is worth at
// the analysis rate. Difference is nil when neither the source nor the target is in the analysis currency
// (no rate known to STL relates them; not invented).
type Analysis struct {
	Currency   kernel.Currency
	Rate       kernel.Decimal
	Difference *kernel.Money
}

// Conversion is a cross-currency application's conversion (STL-INV-007).
type Conversion struct {
	Source   kernel.Money
	Rate     RateUse
	Value    kernel.Decimal // units of the target's currency per unit of the source's
	Analysis *Analysis      // nil for a payable: no order analysis rate applies
}

// FXInput is the settlement rate application.apply names, as POL answered it, and the order's analysis basis
// (nil: none).
type FXInput struct {
	Rate  kernel.ExchangeRate
	Use   RateUse
	Basis *AnalysisBasis
}

// Taken is what the application takes from its source, in the source's currency: the converted source
// amount, or the amount.
func (a *Application) Taken() kernel.Money {
	if a.Conversion != nil {
		return a.Conversion.Source
	}
	return a.Amount
}

// convert returns what amount, in the fund's currency, gives a target in currency target: amount itself in
// the same currency, which takes no rate; converted at fx otherwise (FX_SCOPE_MISMATCH without one).
func convert(amount kernel.Money, target kernel.Currency, fx *FXInput) (kernel.Money, *Conversion, error) {
	source := amount.Currency()
	if source == target {
		if fx != nil {
			return kernel.Money{}, nil, fundrecord.Reject(fundrecord.CodeFXScopeMismatch, "/args/fx_rate",
				"The fund and the target are both in "+source.Code()+": no rate applies")
		}
		return amount, nil, nil
	}
	if fx == nil {
		return kernel.Money{}, nil, fundrecord.Reject(fundrecord.CodeFXScopeMismatch, "/args/fx_rate",
			"The fund is in "+source.Code()+", the target in "+target.Code()+": name a POL fx_rate of purpose settlement from "+source.Code()+
				" to "+target.Code())
	}
	if fx.Rate.From() != source || fx.Rate.To() != target {
		return kernel.Money{}, nil, fundrecord.Reject(fundrecord.CodeFXScopeMismatch, "/args/fx_rate",
			"The rate converts "+fx.Rate.From().Code()+" into "+fx.Rate.To().Code()+", not "+source.Code()+" into "+target.Code())
	}
	got, err := fx.Rate.Convert(amount)
	if err != nil {
		return kernel.Money{}, nil, fundrecord.Reject(fundrecord.CodeFXScopeMismatch, "/args/fx_rate", err.Error())
	}
	if got.Sign() <= 0 {
		return kernel.Money{}, nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/amount",
			"The amount converts to nothing in "+target.Code())
	}
	c := &Conversion{Source: amount, Rate: fx.Use, Value: fx.Rate.Rate()}
	if b := fx.Basis; b != nil && b.OrderCurrency == target.Code() {
		c.Analysis = analyse(amount, got, *b)
	}
	return got, c, nil
}

// analyse compares a conversion with the order's analysis basis b (baseline 4.5 业务结算汇率差额): the target
// at the analysis rate, HALF_UP at the analysis currency's minor units, against the source when it is in the
// analysis currency, or against the target itself when the order is analysed in its own currency (rate 1,
// difference 0).
func analyse(source, target kernel.Money, b AnalysisBasis) *Analysis {
	a := &Analysis{Currency: b.Currency, Rate: b.Rate}
	if target.Currency() == b.Currency {
		a.Rate = kernel.DecimalFromInt(1)
	}
	value, err := kernel.RoundMoney(b.Currency, target.Decimal().Mul(a.Rate))
	if err != nil {
		return a
	}
	var d kernel.Money
	switch {
	case source.Currency() == b.Currency:
		d, err = source.Sub(value)
	case target.Currency() == b.Currency:
		d, err = target.Sub(value)
	default:
		return a
	}
	if err == nil {
		a.Difference = &d
	}
	return a
}

// part splits conversion c of a whole target amount between part of it and the rest, for a move to a
// receivable (D-4): part takes the source and the difference HALF_UP in proportion, the rest what remains,
// so the two add up to c exactly.
func (c *Conversion) part(part, whole kernel.Money) (*Conversion, *Conversion) {
	if part.Equal(whole) {
		return c, nil
	}
	f, err := part.Decimal().Quo(whole.Decimal())
	if err != nil {
		panic("stl: a conversion of nothing")
	}
	first, rest := *c, *c
	first.Source = must(c.Source.MulHalfUp(f))
	rest.Source = must(c.Source.Sub(first.Source))
	if a := c.Analysis; a != nil && a.Difference != nil {
		x, y := *a, *a
		d1 := must(a.Difference.MulHalfUp(f))
		d2 := must(a.Difference.Sub(d1))
		x.Difference, y.Difference = &d1, &d2
		first.Analysis, rest.Analysis = &x, &y
	}
	return &first, &rest
}

func must(m kernel.Money, err error) kernel.Money {
	if err != nil {
		panic("stl: application amounts in one currency: " + err.Error())
	}
	return m
}

// CreditInput is an application.apply of a credit note as the rules see it, with the statement read under its
// lock, what stands on each installment, the credit note and what stands applied from it.
type CreditInput struct {
	ID          string
	Note        *settlement.CreditNote
	NoteApplied kernel.Money
	Statement   *settlement.Statement
	TargetKind  string
	StageID     string
	Applied     map[int]kernel.Money
	Amount      kernel.Money
	By          fundrecord.Act
	Team        string
}

// ApplyCredit applies application.apply's rules to a confirmed credit note (STL-INV-008): to the receivable
// of its own statement, in its currency, within its unapplied rest (APPLICATION_EXCEEDS_AVAILABLE) and what is
// open on the installment (APPLICATION_EXCEEDS_OPEN). Like any application to a receivable it keeps the
// installment's stage.
func ApplyCredit(in CreditInput) (*Application, error) {
	n, st := in.Note, in.Statement
	switch {
	case n.Status != settlement.CreditConfirmed:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/credit_note_id", "The credit note is "+n.Status+": only a confirmed one is applied")
	case in.TargetKind != TargetReceivable || n.StatementID != st.ID:
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/target/statement_id",
			"Credit note "+n.ID+" credits the receivable of statement "+n.StatementID)
	case st.Status != settlement.StatusConfirmed:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/target/statement_id", "The statement is "+st.Status)
	case in.Amount.Currency() != n.Amount.Currency() || in.Amount.Sign() <= 0:
		return nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/amount", "The amount is above zero")
	}
	inst, err := installment(st, in.StageID, in.Applied)
	if err != nil {
		return nil, err
	}
	if rest := n.Rest(in.NoteApplied); exceeds(in.Amount, rest) {
		return nil, fundrecord.Reject(fundrecord.CodeApplicationExceedsAvailable, "/args/amount",
			"Only "+rest.String()+" of credit note "+n.ID+" is not applied")
	}
	if err = withinOpen(in.Amount, st, inst, in.Applied); err != nil {
		return nil, err
	}
	return &Application{ID: in.ID, CreditNoteID: n.ID, TargetKind: TargetReceivable, OrderID: st.OrderID, OrderRevision: st.OrderRevision,
		StageID: inst.StageID, StatementID: st.ID, InstallmentSeq: inst.Seq, Amount: in.Amount, Status: StatusApplied, Version: 1,
		OwnerActor: in.By.By.ID, OwnerTeam: in.Team, Applied: in.By}, nil
}

func exceeds(a, b kernel.Money) bool {
	c, _ := a.Cmp(b)
	return c > 0
}
