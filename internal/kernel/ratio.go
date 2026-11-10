package kernel

import (
	"errors"
	"fmt"
)

var ErrInvalidRatio = errors.New("kernel: invalid ratio")

// Ratio is an exact value in [0, 1], such as a discount, a tax rate or a minimum margin.
type Ratio struct {
	value Decimal
}

// NewRatio returns d as a Ratio.
func NewRatio(d Decimal) (Ratio, error) {
	if d.Sign() < 0 || d.Cmp(DecimalFromInt(1)) > 0 {
		return Ratio{}, fmt.Errorf("%w: %s is outside [0, 1]", ErrInvalidRatio, d)
	}
	return Ratio{value: d}, nil
}

// ParseRatio parses a ratio with at most 8 fractional digits, as the contracts' discount and
// margin fields carry it.
func ParseRatio(s string) (Ratio, error) {
	d, err := ParseDecimal(s, DecimalFormat{IntDigits: 1, Scale: 8})
	if err != nil {
		return Ratio{}, fmt.Errorf("%w: %w", ErrInvalidRatio, err)
	}
	return NewRatio(d)
}

// Decimal returns the ratio as a Decimal.
func (r Ratio) Decimal() Decimal { return r.value }

// Complement returns 1 − r.
func (r Ratio) Complement() Ratio { return Ratio{value: DecimalFromInt(1).Sub(r.value)} }

// Percent returns r × 100.
func (r Ratio) Percent() Decimal { return r.value.Mul(DecimalFromInt(100)) }

func (r Ratio) String() string { return r.value.String() }

// MeetsShare reports whether part ≥ whole × r, compared exactly; it is how a margin is checked
// against a minimum without rounding the margin rate.
func (r Ratio) MeetsShare(part, whole Decimal) bool { return part.Cmp(whole.Mul(r.value)) >= 0 }
