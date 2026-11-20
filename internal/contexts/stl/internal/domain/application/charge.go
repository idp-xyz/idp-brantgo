package application

import (
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/charge"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Charge payments and the settlement rate of a receipt in the order's currency (S5R unit R8; simulated sign-off E0 of
// 2026-11-13: S5R-D19; tactical model STL-AGG-Application "目标种类 charge，出账资金记录核销到费用", STL-INV-002,
// STL-INV-010 "费用的已付状态由核销派生", STL-INV-011).
//
// An outgoing payment to the source party of a confirmed charge may be applied to the charge, at most what is open on
// it (its amount less what stands applied to it): the charge's paid state derives from these applications and the
// order result counts them in its net cash. An internal charge has no payee: it is not paid through STL. A payment in
// another currency than the charge's converts at a POL settlement rate like a payable's (no order analysis).
//
// A receipt in the order's currency applied to an order analysed in another currency may record the POL settlement
// rate in force from the order's currency into the analysis currency when it is applied: the order result values what
// of the receipt its fx_conversion records have not converted yet at that rate (unrealised), what they converted at
// their bank rates (realised).

// SettlementRate is the POL fx_rate of purpose settlement a receipt in the order's currency recorded when it was
// applied: one unit of the order's currency is Value units of Currency, the order's analysis currency (S5R-D19).
type SettlementRate struct {
	Use      RateUse
	Value    kernel.Decimal
	Currency kernel.Currency
}

// SettlementInput is the settlement rate an application of an order's target names (S5R-D19) and the order's analysis
// currency (its code; "" when TRD states none).
type SettlementInput struct {
	FX       *FXInput
	Analysis string
}

// settlementRate checks the settlement rate an application of an order's target names (nil: none): it is only for a
// fund in the target's currency (conv nil) of an order analysed in another currency, from the target's currency into
// the analysis currency (FX_SCOPE_MISMATCH at /args/settlement_fx_rate otherwise).
func settlementRate(conv *Conversion, target kernel.Currency, in *SettlementInput) (*SettlementRate, error) {
	if in == nil || in.FX == nil {
		return nil, nil
	}
	fx := in.FX
	refuse := func(reason string) (*SettlementRate, error) {
		return nil, fundrecord.Reject(fundrecord.CodeFXScopeMismatch, "/args/settlement_fx_rate", reason)
	}
	switch {
	case conv != nil:
		return refuse("The receipt converts at its own rate: a settlement rate into the analysis currency is for a receipt in the order's currency")
	case in.Analysis == "" || in.Analysis == target.Code():
		return refuse("The order is analysed in its own currency " + target.Code() + ": no settlement rate applies")
	case fx.Rate.From() != target || fx.Rate.To().Code() != in.Analysis:
		return refuse("The rate converts " + fx.Rate.From().Code() + " into " + fx.Rate.To().Code() + ", not " + target.Code() + " into the analysis currency " +
			in.Analysis)
	}
	return &SettlementRate{Use: fx.Use, Value: fx.Rate.Rate(), Currency: fx.Rate.To()}, nil
}

// ChargeInput is an application.apply to a charge as the rules see it, with the fund record and the charge read under
// their locks: FundEntries are the fund's entries, Applied what stands applied to the charge.
type ChargeInput struct {
	ID          string
	Fund        *fundrecord.FundRecord
	FundEntries fundrecord.Entries
	Charge      *charge.Charge
	Applied     kernel.Money
	Amount      kernel.Money
	FX          *FXInput
	By          fundrecord.Act
	Team        string
}

// ChargeOpen is what of charge c is still to be paid given applied, what stands applied to it: never below zero (a
// negative adjustment is not paid).
func ChargeOpen(c *charge.Charge, applied kernel.Money) kernel.Money {
	open := must(c.Amount.Sub(applied))
	if open.Sign() < 0 {
		return zero(c.Currency())
	}
	return open
}

// ApplyToCharge applies application.apply's rules to a charge (S5R-D19): a payment that stands, to the source party
// of a confirmed charge that has one, in the charge's currency or converted at a settlement rate, within what the fund
// has available (APPLICATION_EXCEEDS_AVAILABLE) and what is open on the charge (APPLICATION_EXCEEDS_OPEN).
func ApplyToCharge(in ChargeInput) (*Application, error) {
	f, c := in.Fund, in.Charge
	path := "/args/target/charge_id"
	switch {
	case f.Reversed():
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/fund_record_id", "The fund record is reversed (S5R-D22)")
	case !f.Pays():
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/fund_record_id", "A "+f.Kind+" does not pay a charge: only payments do")
	case c.Status != charge.StatusConfirmed:
		return nil, kernel.Reject(kernel.CodeInvalidState, path, "The charge is "+c.Status+": only a confirmed charge is paid")
	case c.Source.PartyID == "":
		return nil, kernel.Reject(kernel.CodeInvalidState, path, "An internal charge has no payee: it is not paid through STL")
	case f.CounterpartyID != c.Source.PartyID:
		return nil, fundrecord.Reject(fundrecord.CodePartyEntityMismatch, "/args/fund_record_id",
			"The payment's party "+f.CounterpartyID+" is not the charge's "+c.Source.PartyID)
	case f.Currency() != c.Currency() && in.FX == nil:
		return nil, fundrecord.Reject(fundrecord.CodeFXScopeMismatch, "/args/fx_rate",
			"The payment is in "+f.Currency().Code()+", the charge in "+c.Currency().Code()+": name a settlement rate")
	case in.Amount.Currency() != f.Currency() || in.Amount.Sign() <= 0:
		return nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/amount", "The amount is above zero")
	}
	fx := in.FX
	if fx != nil {
		fx = &FXInput{Rate: fx.Rate, Use: fx.Use} // a charge has no order analysis rate
	}
	amount, conv, err := convert(in.Amount, c.Currency(), fx)
	if err != nil {
		return nil, err
	}
	if err = withinAvailable(in.Amount, f, in.FundEntries); err != nil {
		return nil, err
	}
	if open := ChargeOpen(c, in.Applied); exceeds(amount, open) {
		return nil, fundrecord.Reject(fundrecord.CodeApplicationExceedsOpen, "/args/amount", "Only "+open.String()+" of charge "+c.ID+" is open")
	}
	return &Application{ID: in.ID, FundRecordID: f.ID, TargetKind: TargetCharge, ChargeID: c.ID, Amount: amount, Conversion: conv,
		Status: StatusApplied, Version: 1, OwnerActor: in.By.By.ID, OwnerTeam: in.Team, Applied: in.By, PayerBasis: PayerCustomer}, nil
}
