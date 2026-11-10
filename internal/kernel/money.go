package kernel

import (
	"errors"
	"fmt"
	"math/big"
)

var (
	ErrInvalidCurrency  = errors.New("kernel: invalid currency")
	ErrCurrencyMismatch = errors.New("kernel: currency mismatch")
	ErrInvalidAmount    = errors.New("kernel: invalid amount")
	ErrAmountOutOfRange = errors.New("kernel: amount out of range")
)

// MaxMinorDigits bounds every amount: |minor| < 10^24, the CALCULATION_LIMIT_EXCEEDED boundary of
// the Python contracts.
const MaxMinorDigits = 24

var minorLimit = pow10(MaxMinorDigits)

// Currency is an ISO 4217 alphabetic code with the number of minor-unit digits that its POL
// revision assigns. The kernel checks only the form; whether a code is in use belongs to
// reference data and POL.
type Currency struct {
	code       string
	minorUnits int
}

// NewCurrency returns the currency code with minorUnits fractional digits (0–4).
func NewCurrency(code string, minorUnits int) (Currency, error) {
	if len(code) != 3 || minorUnits < 0 || minorUnits > 4 {
		return Currency{}, fmt.Errorf("%w: %q/%d", ErrInvalidCurrency, code, minorUnits)
	}
	for i := 0; i < 3; i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return Currency{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, code)
		}
	}
	return Currency{code: code, minorUnits: minorUnits}, nil
}

// MustCurrency is NewCurrency that panics on error, for constants and tests.
func MustCurrency(code string, minorUnits int) Currency {
	c, err := NewCurrency(code, minorUnits)
	if err != nil {
		panic(err)
	}
	return c
}

// Code returns the ISO 4217 code.
func (c Currency) Code() string { return c.code }

// MinorUnits returns the number of minor-unit digits.
func (c Currency) MinorUnits() int { return c.minorUnits }

// IsZero reports whether c is the zero Currency, which no amount may use.
func (c Currency) IsZero() bool { return c.code == "" }

func (c Currency) String() string { return c.code }

// Money is an amount in integer minor units of one currency, such as cents of USD. Sums and
// differences stay in integers; turning a Decimal into Money is an explicit HALF_UP rounding
// point. Amounts of different currencies never combine.
type Money struct {
	currency Currency
	minor    *big.Int
}

// MoneyFromMinor returns minor units of c.
func MoneyFromMinor(c Currency, minor *big.Int) (Money, error) {
	if c.IsZero() {
		return Money{}, ErrInvalidCurrency
	}
	if new(big.Int).Abs(minor).Cmp(minorLimit) >= 0 {
		return Money{}, fmt.Errorf("%w: %s minor units", ErrAmountOutOfRange, minor)
	}
	return Money{currency: c, minor: new(big.Int).Set(minor)}, nil
}

// NewMoney returns minor units of c.
func NewMoney(c Currency, minor int64) (Money, error) { return MoneyFromMinor(c, big.NewInt(minor)) }

// ParseMinor parses a minor-unit string of the contracts: "0", or an integer of at most 24 digits
// without leading zeros and with an optional '-'.
func ParseMinor(c Currency, s string) (Money, error) {
	body := s
	if len(body) > 1 && body[0] == '-' {
		body = body[1:]
	}
	if body == "" || len(body) > MaxMinorDigits || !digits(body) || (body[0] == '0' && (len(body) > 1 || body != s)) {
		return Money{}, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	v, _ := new(big.Int).SetString(s, 10)
	return MoneyFromMinor(c, v)
}

// ParseAmount parses an unsigned decimal amount, such as "960.00", that has at most the
// currency's minor-unit digits. It never rounds.
func ParseAmount(c Currency, s string) (Money, error) {
	d, err := ParseDecimal(s, DecimalFormat{IntDigits: MaxMinorDigits - c.minorUnits, Scale: c.minorUnits})
	if err != nil {
		return Money{}, fmt.Errorf("%w: %q for %s", ErrInvalidAmount, s, c)
	}
	return MoneyFromMinor(c, d.ScaledHalfUp(c.minorUnits))
}

// RoundMoney rounds an exact amount of c HALF_UP to c's minor units.
func RoundMoney(c Currency, amount Decimal) (Money, error) {
	if c.IsZero() {
		return Money{}, ErrInvalidCurrency
	}
	return MoneyFromMinor(c, amount.ScaledHalfUp(c.minorUnits))
}

// Currency returns the currency.
func (m Money) Currency() Currency { return m.currency }

func (m Money) value() *big.Int {
	if m.minor == nil {
		return new(big.Int)
	}
	return m.minor
}

// Minor returns a copy of the amount in minor units.
func (m Money) Minor() *big.Int { return new(big.Int).Set(m.value()) }

// MinorString returns the minor units as the contracts' integer string.
func (m Money) MinorString() string { return m.value().String() }

// Decimal returns the amount in major units, exactly.
func (m Money) Decimal() Decimal { return DecimalFromMinor(m.value(), m.currency.minorUnits) }

// Amount returns the amount with exactly the currency's minor-unit digits, such as "960.00".
func (m Money) Amount() string { return formatScaled(m.value(), m.currency.minorUnits) }

func (m Money) String() string { return m.Amount() + " " + m.currency.code }

// Sign returns -1, 0 or +1.
func (m Money) Sign() int { return m.value().Sign() }

// IsZero reports whether the amount is 0.
func (m Money) IsZero() bool { return m.Sign() == 0 }

// Equal reports whether m and o have the same currency and amount.
func (m Money) Equal(o Money) bool { return m.currency == o.currency && m.value().Cmp(o.value()) == 0 }

// Cmp compares two amounts of the same currency.
func (m Money) Cmp(o Money) (int, error) {
	if m.currency != o.currency {
		return 0, ErrCurrencyMismatch
	}
	return m.value().Cmp(o.value()), nil
}

// Add returns m + o.
func (m Money) Add(o Money) (Money, error) {
	if m.currency != o.currency {
		return Money{}, ErrCurrencyMismatch
	}
	return MoneyFromMinor(m.currency, new(big.Int).Add(m.value(), o.value()))
}

// Sub returns m − o.
func (m Money) Sub(o Money) (Money, error) {
	if m.currency != o.currency {
		return Money{}, ErrCurrencyMismatch
	}
	return MoneyFromMinor(m.currency, new(big.Int).Sub(m.value(), o.value()))
}

// Neg returns −m.
func (m Money) Neg() Money { return Money{currency: m.currency, minor: new(big.Int).Neg(m.value())} }

// MulHalfUp returns m × factor rounded HALF_UP to minor units.
func (m Money) MulHalfUp(factor Decimal) (Money, error) {
	return RoundMoney(m.currency, m.Decimal().Mul(factor))
}

// Sum adds amounts of c; it returns zero of c for no amounts.
func Sum(c Currency, amounts ...Money) (Money, error) {
	total, err := MoneyFromMinor(c, new(big.Int))
	if err != nil {
		return Money{}, err
	}
	for _, a := range amounts {
		if total, err = total.Add(a); err != nil {
			return Money{}, err
		}
	}
	return total, nil
}

// Allocate splits m by weight with AllocateLargestRemainder. m must not be negative.
func (m Money) Allocate(shares []Share) ([]Money, error) {
	parts, err := AllocateLargestRemainder(m.value(), shares)
	if err != nil {
		return nil, err
	}
	out := make([]Money, len(parts))
	for i, p := range parts {
		out[i] = Money{currency: m.currency, minor: p}
	}
	return out, nil
}
