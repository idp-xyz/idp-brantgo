// Package kernel holds value objects whose meaning is identical in every Brant bounded context:
// exact decimals, money in minor units, allocations, tax splits, exchange rates, quantities,
// ratios, validity intervals, tenant and company scope, and actor references.
//
// It imports the standard library only, and a change here affects every context. Arithmetic is
// exact (math/big rationals) until an explicit rounding point, which rounds HALF_UP to a
// currency's minor units. Floating point never enters a value.
package kernel

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

var (
	ErrInvalidDecimal = errors.New("kernel: invalid decimal")
	ErrDivisionByZero = errors.New("kernel: division by zero")
)

// DecimalFormat is the lexical form a contract accepts for a decimal string: an unsigned plain
// decimal without exponent or leading zeros, with at most IntDigits integer digits and at most
// Scale fractional digits.
type DecimalFormat struct {
	IntDigits int
	Scale     int
}

// The decimal formats of the Python V0.50 contracts.
var (
	QuantityFormat = DecimalFormat{IntDigits: 12, Scale: 6}  // quantities, weights, dimensions
	PriceFormat    = DecimalFormat{IntDigits: 12, Scale: 8}  // unit prices, amounts, discount ratios
	FactorFormat   = DecimalFormat{IntDigits: 12, Scale: 12} // unit conversion factors, exchange rates
)

// Decimal is an exact rational number. The zero value is 0. A Decimal is immutable; every
// operation returns a new value.
type Decimal struct {
	r *big.Rat
}

// ParseDecimal parses s in format f. It rejects signs, exponents, fractions, NaN, Infinity,
// whitespace and leading zeros, so a float rendered by a JSON encoder never parses by accident.
func ParseDecimal(s string, f DecimalFormat) (Decimal, error) {
	whole, frac, hasPoint := strings.Cut(s, ".")
	switch {
	case whole == "" || len(whole) > f.IntDigits || !digits(whole) || (len(whole) > 1 && whole[0] == '0'):
		return Decimal{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	case hasPoint && (frac == "" || len(frac) > f.Scale || !digits(frac)):
		return Decimal{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return Decimal{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	return Decimal{r: r}, nil
}

// MustDecimal parses s in FactorFormat, or with a leading '-' for a negative value, and panics on
// error. It is meant for constants and tests.
func MustDecimal(s string) Decimal {
	neg := strings.HasPrefix(s, "-")
	d, err := ParseDecimal(strings.TrimPrefix(s, "-"), FactorFormat)
	if err != nil {
		panic(err)
	}
	if neg {
		return d.Neg()
	}
	return d
}

// DecimalFromInt returns v as a Decimal.
func DecimalFromInt(v int64) Decimal { return Decimal{r: new(big.Rat).SetInt64(v)} }

// DecimalFromBigInt returns v as a Decimal.
func DecimalFromBigInt(v *big.Int) Decimal { return Decimal{r: new(big.Rat).SetInt(v)} }

// DecimalFromMinor returns minor / 10^scale, the decimal value of an amount in minor units.
func DecimalFromMinor(minor *big.Int, scale int) Decimal {
	return Decimal{r: new(big.Rat).SetFrac(minor, pow10(scale))}
}

func (d Decimal) rat() *big.Rat {
	if d.r == nil {
		return new(big.Rat)
	}
	return d.r
}

// Add returns d + o.
func (d Decimal) Add(o Decimal) Decimal { return Decimal{r: new(big.Rat).Add(d.rat(), o.rat())} }

// Sub returns d − o.
func (d Decimal) Sub(o Decimal) Decimal { return Decimal{r: new(big.Rat).Sub(d.rat(), o.rat())} }

// Mul returns d × o.
func (d Decimal) Mul(o Decimal) Decimal { return Decimal{r: new(big.Rat).Mul(d.rat(), o.rat())} }

// Quo returns d ÷ o exactly.
func (d Decimal) Quo(o Decimal) (Decimal, error) {
	if o.Sign() == 0 {
		return Decimal{}, ErrDivisionByZero
	}
	return Decimal{r: new(big.Rat).Quo(d.rat(), o.rat())}, nil
}

// Neg returns −d.
func (d Decimal) Neg() Decimal { return Decimal{r: new(big.Rat).Neg(d.rat())} }

// Cmp compares d and o and returns -1, 0 or +1.
func (d Decimal) Cmp(o Decimal) int { return d.rat().Cmp(o.rat()) }

// Equal reports whether d and o are the same number.
func (d Decimal) Equal(o Decimal) bool { return d.Cmp(o) == 0 }

// Sign returns -1, 0 or +1.
func (d Decimal) Sign() int { return d.rat().Sign() }

// IsZero reports whether d is 0.
func (d Decimal) IsZero() bool { return d.Sign() == 0 }

// IsInteger reports whether d has no fractional part.
func (d Decimal) IsInteger() bool { return d.rat().IsInt() }

// Floor returns the greatest integer not above d.
func (d Decimal) Floor() *big.Int {
	r := d.rat()
	q := new(big.Int)
	q.DivMod(r.Num(), r.Denom(), new(big.Int)) // Euclidean division; the denominator is positive
	return q
}

// ScaledHalfUp returns d × 10^scale rounded to an integer, ties away from zero. For the
// non-negative values the contracts round it equals Python's (2·n + d) // (2·d) on the scaled
// fraction n/d.
func (d Decimal) ScaledHalfUp(scale int) *big.Int {
	r := d.rat()
	num := new(big.Int).Mul(r.Num(), pow10(scale))
	neg := num.Sign() < 0
	num.Abs(num)
	num.Lsh(num, 1).Add(num, r.Denom())
	q := num.Quo(num, new(big.Int).Lsh(r.Denom(), 1))
	if neg {
		q.Neg(q)
	}
	return q
}

// RoundHalfUp returns d rounded to scale fractional digits, ties away from zero.
func (d Decimal) RoundHalfUp(scale int) Decimal {
	return DecimalFromMinor(d.ScaledHalfUp(scale), scale)
}

// StringFixed returns d rounded HALF_UP to exactly scale fractional digits, such as "960.00".
func (d Decimal) StringFixed(scale int) string { return formatScaled(d.ScaledHalfUp(scale), scale) }

// Exact returns the shortest plain decimal string of d and true when d has a terminating decimal
// expansion, and "" and false otherwise (as for 1/3).
func (d Decimal) Exact() (string, bool) {
	r := d.rat()
	den := new(big.Int).Set(r.Denom())
	twos, fives := strip(den, 2), strip(den, 5)
	if den.Cmp(big.NewInt(1)) != 0 {
		return "", false
	}
	scale := max(twos, fives)
	scaled := new(big.Int).Mul(r.Num(), pow10(scale))
	scaled.Quo(scaled, r.Denom())
	return formatScaled(scaled, scale), true
}

// String returns the exact decimal string when there is one and the reduced fraction "p/q"
// otherwise.
func (d Decimal) String() string {
	if s, ok := d.Exact(); ok {
		return s
	}
	return d.rat().RatString()
}

// strip divides n by p as often as it divides evenly and returns how often that was.
func strip(n *big.Int, p int64) int {
	bp := big.NewInt(p)
	var q, m big.Int
	count := 0
	for n.Sign() != 0 {
		q.QuoRem(n, bp, &m)
		if m.Sign() != 0 {
			break
		}
		n.Set(&q)
		count++
	}
	return count
}

func formatScaled(v *big.Int, scale int) string {
	digitsText := new(big.Int).Abs(v).String()
	sign := ""
	if v.Sign() < 0 {
		sign = "-"
	}
	if scale <= 0 {
		return sign + digitsText
	}
	if len(digitsText) <= scale {
		digitsText = strings.Repeat("0", scale-len(digitsText)+1) + digitsText
	}
	cut := len(digitsText) - scale
	return sign + digitsText[:cut] + "." + digitsText[cut:]
}

func pow10(n int) *big.Int {
	if n < 0 {
		panic("kernel: negative scale")
	}
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

func digits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
