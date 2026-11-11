package calc

import (
	"errors"
	"fmt"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Base returns the line's exact discounted amount B = quantity × unit_price ÷ price_basis_quantity
// × (1 − discount_ratio) (T03 §5.1), after checking the values: quantity, unit price and price basis
// quantity positive, the discount ratio from 0 up to but excluding 1 (a gift at price 0 is not
// supported), each of its format and below 10^12.
func (l Line) Base() (kernel.Decimal, error) {
	if err := positive(l.Quantity, QuantityScale, l.Path+"/quantity", "A quantity"); err != nil {
		return kernel.Decimal{}, err
	}
	if err := positive(l.UnitPrice, PriceScale, l.Path+"/unit_price", "A unit price"); err != nil {
		return kernel.Decimal{}, err
	}
	if err := positive(l.PriceBasisQuantity, PriceScale, l.Path+"/price_basis_quantity", "A price basis quantity"); err != nil {
		return kernel.Decimal{}, err
	}
	switch d := l.Discount; {
	case !fits(d, PriceScale):
		return kernel.Decimal{}, fail(kernel.CodeSchemaInvalid, l.Path+"/discount_ratio", "A discount ratio has at most 8 fractional digits")
	case d.Sign() < 0 || d.Cmp(one) >= 0:
		return kernel.Decimal{}, fail(kernel.CodeInvalidKnownValue, l.Path+"/discount_ratio", "A discount ratio is at least 0 and below 1")
	}
	perUnit, _ := l.UnitPrice.Quo(l.PriceBasisQuantity) // the price basis quantity is positive
	return l.Quantity.Mul(perUnit).Mul(one.Sub(l.Discount)), nil
}

// Split applies the tax profile to base, an exact non-negative amount of currency c (T03 §5.1). An
// exclusive or not-applicable amount is rounded HALF_UP to c's minor units as the net N, the tax is
// round(N × rate) and the payable N + tax. An inclusive amount is rounded as the payable G, the net
// is round(G ÷ (1 + rate)) and the tax G − N, so the payable is conserved and the tax never rounded
// on its own. 113 inclusive at 0.13 is 100 + 13.
func (t TaxProfile) Split(c kernel.Currency, base kernel.Decimal, path string) (kernel.TaxSplit, error) {
	if err := t.Check(path); err != nil {
		return kernel.TaxSplit{}, err
	}
	if base.Sign() < 0 {
		return kernel.TaxSplit{}, fail(kernel.CodeInvalidKnownValue, path, "A taxed amount is not negative")
	}
	split := kernel.TaxExclusive
	if t.PriceBasis == TaxInclusive {
		split = kernel.TaxInclusive
	}
	s, err := split(c, base, t.Rate)
	switch {
	case errors.Is(err, kernel.ErrAmountOutOfRange):
		return kernel.TaxSplit{}, limited(path, "The amount")
	case err != nil:
		return kernel.TaxSplit{}, fail(kernel.CodeInvalidKnownValue, path, err.Error())
	}
	return s, nil
}

// Amounts is what one line or charge contributes in the quote currency, in integer minor units:
// Net + Tax = Payable.
type Amounts struct {
	ID      string
	Net     kernel.Money
	Tax     kernel.Money
	Payable kernel.Money
}

// ChargeAmounts are a charge's amounts with its obligation; an included charge contributes zero.
type ChargeAmounts struct {
	Amounts
	ObligationKey string
	Included      bool
}

// PriceLine prices one line in the quote currency c: its base B split by its tax profile and
// rounded to c's minor units on its own, before any sum (two lines of 1 × 0.005 USD are 0.01 each,
// never merged into one cent).
func PriceLine(c kernel.Currency, l Line) (Amounts, error) {
	base, err := l.Base()
	if err != nil {
		return Amounts{}, err
	}
	if l.Tax == nil {
		return Amounts{}, fail(CodeTaxPolicyUnconfirmed, l.Path+"/tax_application_ref", "No sales tax application is named; a missing one is never zero tax")
	}
	s, err := l.Tax.Profile.Split(c, base, l.Path+"/tax_application_ref")
	if err != nil {
		if r, ok := kernel.AsRejection(err); ok && r.Code == CodeCalculationLimitExceeded {
			return Amounts{}, limited(l.Path, "The line amount")
		}
		return Amounts{}, err
	}
	return Amounts{ID: l.LineID, Net: s.Net, Tax: s.Tax, Payable: s.Gross}, nil
}

// PriceCharge prices one sales charge in the quote currency c by the same tax algorithm as a line.
// An included charge is part of its line's price and adds nothing: its amounts are zero, and its
// tax applicability was bound all the same (BindTerms).
func PriceCharge(c kernel.Currency, ch Charge) (ChargeAmounts, error) {
	out := ChargeAmounts{Amounts: Amounts{ID: ch.ChargeID}, ObligationKey: ch.ObligationKey, Included: ch.Inclusion == InclusionIncluded}
	if out.Included {
		out.Net, out.Tax, out.Payable = zero(c), zero(c), zero(c)
		return out, nil
	}
	if err := chargeAmount(ch.Amount, ch.Path+"/amount"); err != nil {
		return ChargeAmounts{}, err
	}
	if ch.Tax == nil {
		return ChargeAmounts{}, fail(CodeTaxPolicyUnconfirmed, ch.Path+"/tax_application_ref", "No sales tax application is named; a missing one is never zero tax")
	}
	s, err := ch.Tax.Profile.Split(c, ch.Amount, ch.Path+"/tax_application_ref")
	if err != nil {
		if r, ok := kernel.AsRejection(err); ok && r.Code == CodeCalculationLimitExceeded {
			return ChargeAmounts{}, limited(ch.Path+"/amount", "The charge amount")
		}
		return ChargeAmounts{}, err
	}
	out.Net, out.Tax, out.Payable = s.Net, s.Tax, s.Gross
	return out, nil
}

// Totals are the quote's sums in the quote currency: the net sales (line nets and separate charge
// nets), the tax, and the payable = net + tax.
type Totals struct {
	Net     kernel.Money
	Tax     kernel.Money
	Payable kernel.Money
}

// Total sums the rounded amounts of lines and charges in currency c (TRD-INV-008), at path, the
// quote's. The net sales are positive (INVALID_KNOWN_VALUE otherwise) and every sum is below 10^24
// minor units (CALCULATION_LIMIT_EXCEEDED).
func Total(c kernel.Currency, path string, lines []Amounts, charges []ChargeAmounts) (Totals, error) {
	nets := make([]kernel.Money, 0, len(lines)+len(charges))
	taxes := make([]kernel.Money, 0, len(lines)+len(charges))
	for _, l := range lines {
		nets, taxes = append(nets, l.Net), append(taxes, l.Tax)
	}
	for _, ch := range charges {
		nets, taxes = append(nets, ch.Net), append(taxes, ch.Tax)
	}
	net, err := kernel.Sum(c, nets...)
	if err != nil {
		return Totals{}, sumError(err, path, "The net sales")
	}
	tax, err := kernel.Sum(c, taxes...)
	if err != nil {
		return Totals{}, sumError(err, path, "The tax")
	}
	payable, err := net.Add(tax)
	if err != nil {
		return Totals{}, sumError(err, path, "The payable")
	}
	if net.Sign() <= 0 {
		return Totals{}, fail(kernel.CodeInvalidKnownValue, path+"/lines", "The net sales are positive")
	}
	return Totals{Net: net, Tax: tax, Payable: payable}, nil
}

func sumError(err error, path, what string) error {
	if errors.Is(err, kernel.ErrAmountOutOfRange) {
		return limited(path, what)
	}
	return fail(kernel.CodeInvalidKnownValue, path, fmt.Sprintf("%s: %v", what, err))
}
