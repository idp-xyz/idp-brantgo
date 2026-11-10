package kernel

import (
	"errors"
	"fmt"
)

var ErrInvalidTaxRate = errors.New("kernel: invalid tax rate")

// TaxSplit is an amount split into its net, tax and gross parts; Net + Tax = Gross.
type TaxSplit struct {
	Net, Tax, Gross Money
}

// TaxExclusive treats base as the net amount: net = round(base), tax = round(net × rate),
// gross = net + tax. Every rounding is HALF_UP to c's minor units, as in the Python contracts.
func TaxExclusive(c Currency, base, rate Decimal) (TaxSplit, error) {
	if err := checkTax(base, rate); err != nil {
		return TaxSplit{}, err
	}
	net, err := RoundMoney(c, base)
	if err != nil {
		return TaxSplit{}, err
	}
	tax, err := net.MulHalfUp(rate)
	if err != nil {
		return TaxSplit{}, err
	}
	gross, err := net.Add(tax)
	if err != nil {
		return TaxSplit{}, err
	}
	return TaxSplit{Net: net, Tax: tax, Gross: gross}, nil
}

// TaxInclusive treats base as the gross amount: gross = round(base), net = round(gross ÷ (1 +
// rate)), tax = gross − net.
func TaxInclusive(c Currency, base, rate Decimal) (TaxSplit, error) {
	if err := checkTax(base, rate); err != nil {
		return TaxSplit{}, err
	}
	gross, err := RoundMoney(c, base)
	if err != nil {
		return TaxSplit{}, err
	}
	netValue, err := gross.Decimal().Quo(DecimalFromInt(1).Add(rate))
	if err != nil {
		return TaxSplit{}, err
	}
	net, err := RoundMoney(c, netValue)
	if err != nil {
		return TaxSplit{}, err
	}
	tax, err := gross.Sub(net)
	if err != nil {
		return TaxSplit{}, err
	}
	return TaxSplit{Net: net, Tax: tax, Gross: gross}, nil
}

func checkTax(base, rate Decimal) error {
	if base.Sign() < 0 {
		return fmt.Errorf("%w: negative base %s", ErrInvalidAmount, base)
	}
	if rate.Sign() < 0 || rate.Cmp(DecimalFromInt(1)) > 0 {
		return fmt.Errorf("%w: %s is outside [0, 1]", ErrInvalidTaxRate, rate)
	}
	return nil
}
