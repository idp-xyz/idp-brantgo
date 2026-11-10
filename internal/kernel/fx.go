package kernel

import (
	"errors"
	"fmt"
)

var ErrInvalidExchangeRate = errors.New("kernel: invalid exchange rate")

// ExchangeRate converts amounts of From into To: one unit of From is Rate units of To, so
// "7.123456 CNY/USD" has From USD and To CNY. The purpose (cost estimate, sales, settlement),
// source and validity belong to the POL revision that publishes the rate, not to this value.
type ExchangeRate struct {
	from, to Currency
	rate     Decimal
}

// NewExchangeRate returns a positive rate from one currency to another. A rate from a currency to
// itself must be exactly 1.
func NewExchangeRate(from, to Currency, rate Decimal) (ExchangeRate, error) {
	if from.IsZero() || to.IsZero() {
		return ExchangeRate{}, ErrInvalidCurrency
	}
	if rate.Sign() <= 0 || (from == to && !rate.Equal(DecimalFromInt(1))) {
		return ExchangeRate{}, fmt.Errorf("%w: %s %s/%s", ErrInvalidExchangeRate, rate, to, from)
	}
	return ExchangeRate{from: from, to: to, rate: rate}, nil
}

// ParseExchangeRate parses the rate in FactorFormat (at most 12 fractional digits).
func ParseExchangeRate(from, to Currency, s string) (ExchangeRate, error) {
	rate, err := ParseDecimal(s, FactorFormat)
	if err != nil {
		return ExchangeRate{}, fmt.Errorf("%w: %w", ErrInvalidExchangeRate, err)
	}
	return NewExchangeRate(from, to, rate)
}

// From returns the source currency.
func (x ExchangeRate) From() Currency { return x.from }

// To returns the target currency.
func (x ExchangeRate) To() Currency { return x.to }

// Rate returns units of To per unit of From.
func (x ExchangeRate) Rate() Decimal { return x.rate }

// Convert returns m in To, rounded HALF_UP to To's minor units once, from the exact product.
func (x ExchangeRate) Convert(m Money) (Money, error) {
	if m.Currency() != x.from {
		return Money{}, fmt.Errorf("%w: rate converts %s, amount is %s", ErrCurrencyMismatch, x.from, m.Currency())
	}
	return RoundMoney(x.to, m.Decimal().Mul(x.rate))
}
