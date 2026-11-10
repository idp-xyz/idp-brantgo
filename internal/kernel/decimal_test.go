package kernel

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParseDecimalAcceptsContractForms(t *testing.T) {
	for _, s := range []string{"0", "7", "12", "0.005", "960.00", "999999999999.999999", "0.000001"} {
		if _, err := ParseDecimal(s, QuantityFormat); err != nil {
			t.Errorf("ParseDecimal(%q) = %v", s, err)
		}
	}
	if d, err := ParseDecimal("7.123456789012", FactorFormat); err != nil || d.String() != "7.123456789012" {
		t.Errorf("factor = %v, %v", d, err)
	}
}

func TestParseDecimalRejectsEverythingElse(t *testing.T) {
	for _, s := range []string{
		"", "-1", "+1", "01", "00.5", "1.", ".5", "1e3", "1E3", "1e-2", "NaN", "Infinity", "inf",
		" 1", "1 ", "1/3", "0x10", "1_000", "1,5", "١", "1.0000001", "1234567890123", "1.2.3", "−1",
	} {
		if d, err := ParseDecimal(s, QuantityFormat); !errors.Is(err, ErrInvalidDecimal) {
			t.Errorf("ParseDecimal(%q) = %v, %v; want ErrInvalidDecimal", s, d, err)
		}
	}
}

func TestRoundHalfUpIsExact(t *testing.T) {
	for _, tc := range []struct {
		in    Decimal
		scale int
		want  string
	}{
		{MustDecimal("0.005"), 2, "0.01"},
		{MustDecimal("0.004999"), 2, "0.00"},
		{MustDecimal("2.675"), 2, "2.68"}, // 2.67499999… as a float64
		{MustDecimal("1.005"), 2, "1.01"},
		{MustDecimal("0.125"), 2, "0.13"},
		{MustDecimal("712.3456"), 2, "712.35"},
		{MustDecimal("-0.005"), 2, "-0.01"}, // ties away from zero
		{MustDecimal("-0.004"), 2, "0.00"},
		{MustDecimal("8"), 0, "8"},
		{MustDecimal("0.5"), 0, "1"},
		{third(1), 2, "0.33"},
		{third(2), 2, "0.67"},
	} {
		if got := tc.in.StringFixed(tc.scale); got != tc.want {
			t.Errorf("%s rounded to %d = %s, want %s", tc.in, tc.scale, got, tc.want)
		}
	}
}

func third(n int64) Decimal {
	d, err := DecimalFromInt(n).Quo(DecimalFromInt(3))
	if err != nil {
		panic(err)
	}
	return d
}

func TestExactAndString(t *testing.T) {
	if s, ok := third(1).Exact(); ok || s != "" || third(1).String() != "1/3" {
		t.Fatalf("1/3: Exact = %q, %v; String = %s", s, ok, third(1).String())
	}
	for in, want := range map[string]string{"0.3125": "0.3125", "960.00": "960", "0.50": "0.5", "0": "0", "-12.340": "-12.34"} {
		if got := MustDecimal(in).String(); got != want {
			t.Errorf("String(%s) = %s, want %s", in, got, want)
		}
	}
	if got := (Decimal{}).StringFixed(2); got != "0.00" {
		t.Errorf("zero value = %s", got)
	}
	if got := MustDecimal("-0.5").Floor().String(); got != "-1" {
		t.Errorf("Floor(-0.5) = %s", got)
	}
}

func TestQuoByZero(t *testing.T) {
	if _, err := DecimalFromInt(1).Quo(Decimal{}); !errors.Is(err, ErrDivisionByZero) {
		t.Fatalf("err = %v", err)
	}
}

// canonical is an independent oracle for the exact text of a parsed decimal: drop trailing
// fractional zeros and a bare point.
func canonical(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

func FuzzParseDecimal(f *testing.F) {
	for _, s := range []string{"0", "0.005", "960.00", "1e3", "-1", "01", "NaN", "1.", "999999999999.999999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseDecimal(s, QuantityFormat)
		if err != nil {
			return
		}
		exact, ok := d.Exact()
		if !ok || exact != canonical(s) {
			t.Fatalf("ParseDecimal(%q).Exact() = %q, %v; want %q", s, exact, ok, canonical(s))
		}
	})
}

// roundCents is an independent oracle for HALF_UP to two digits on a non-negative decimal string:
// digit arithmetic on the text, no rationals.
func roundCents(whole uint64, frac string) string {
	frac = (frac + "000")[:max(3, len(frac))]
	cents := whole*100 + uint64(frac[0]-'0')*10 + uint64(frac[1]-'0')
	if frac[2] >= '5' {
		cents++
	}
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

func FuzzRoundHalfUpAgainstDigitOracle(f *testing.F) {
	f.Add(uint64(0), uint64(5000000), uint8(7))
	f.Add(uint64(2), uint64(675), uint8(3))
	f.Add(uint64(999999999), uint64(99999999), uint8(8))
	f.Fuzz(func(t *testing.T, whole, fracBits uint64, width uint8) {
		whole %= 1_000_000_000_000
		n := int(width%8) + 1
		frac := fmt.Sprintf("%0*d", n, fracBits%pow10Uint(n))
		s := fmt.Sprintf("%d.%s", whole, frac)
		d, err := ParseDecimal(s, PriceFormat)
		if err != nil {
			t.Fatalf("ParseDecimal(%q): %v", s, err)
		}
		if got, want := d.StringFixed(2), roundCents(whole, frac); got != want {
			t.Fatalf("%s rounded = %s, oracle %s", s, got, want)
		}
	})
}

func pow10Uint(n int) uint64 {
	v := uint64(1)
	for range n {
		v *= 10
	}
	return v
}
