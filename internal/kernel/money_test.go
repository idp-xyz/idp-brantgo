package kernel

import (
	"errors"
	"math/big"
	"strings"
	"testing"
)

var (
	usd = MustCurrency("USD", 2)
	cny = MustCurrency("CNY", 2)
	jpy = MustCurrency("JPY", 0)
)

func money(t *testing.T, c Currency, amount string) Money {
	t.Helper()
	m, err := ParseAmount(c, amount)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCurrencyForm(t *testing.T) {
	for _, tc := range []struct {
		code  string
		minor int
		ok    bool
	}{{"USD", 2, true}, {"JPY", 0, true}, {"CLF", 4, true}, {"usd", 2, false}, {"US", 2, false}, {"USDX", 2, false}, {"USD", 5, false}, {"USD", -1, false}} {
		if _, err := NewCurrency(tc.code, tc.minor); (err == nil) != tc.ok {
			t.Errorf("NewCurrency(%q, %d) = %v", tc.code, tc.minor, err)
		}
	}
}

func TestParseMinorFollowsTheContractPattern(t *testing.T) {
	limit := strings.Repeat("9", 24)
	for _, s := range []string{"0", "1", "-1", "96000", limit, "-" + limit} {
		if m, err := ParseMinor(usd, s); err != nil || m.MinorString() != s {
			t.Errorf("ParseMinor(%q) = %v, %v", s, m, err)
		}
	}
	for _, s := range []string{"", "-", "-0", "00", "01", "+1", "1e2", "1.0", " 1", "1" + limit} {
		if _, err := ParseMinor(usd, s); err == nil {
			t.Errorf("ParseMinor(%q) must be rejected", s)
		}
	}
}

func TestParseAmountNeverRounds(t *testing.T) {
	if m := money(t, usd, "960.00"); m.MinorString() != "96000" || m.Amount() != "960.00" || m.String() != "960.00 USD" {
		t.Fatalf("960.00 USD = %s (%s minor)", m, m.MinorString())
	}
	if m := money(t, usd, "960"); m.MinorString() != "96000" {
		t.Fatalf("960 USD = %s minor", m.MinorString())
	}
	for _, tc := range []struct {
		c Currency
		s string
	}{{usd, "960.001"}, {jpy, "100.5"}, {usd, "-1.00"}, {usd, "1e2"}} {
		if _, err := ParseAmount(tc.c, tc.s); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("ParseAmount(%s, %q) = %v", tc.c, tc.s, err)
		}
	}
}

func TestAmountsOfDifferentCurrenciesNeverCombine(t *testing.T) {
	a, b := money(t, usd, "1.00"), money(t, cny, "1.00")
	if _, err := a.Add(b); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Add = %v", err)
	}
	if _, err := a.Sub(b); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Sub = %v", err)
	}
	if _, err := a.Cmp(b); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Cmp = %v", err)
	}
	if _, err := Sum(usd, a, b); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Sum = %v", err)
	}
}

func TestAmountLimitIsTheCalculationLimit(t *testing.T) {
	below := new(big.Int).Sub(pow10(24), big.NewInt(1))
	m, err := MoneyFromMinor(usd, below)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Add(money(t, usd, "0.01")); !errors.Is(err, ErrAmountOutOfRange) {
		t.Fatalf("10^24 minor units: %v", err)
	}
	if _, err = MoneyFromMinor(usd, new(big.Int).Neg(pow10(24))); !errors.Is(err, ErrAmountOutOfRange) {
		t.Fatalf("-10^24 minor units: %v", err)
	}
	if _, err = RoundMoney(usd, DecimalFromBigInt(pow10(22))); !errors.Is(err, ErrAmountOutOfRange) {
		t.Fatalf("10^22 major units of USD: %v", err)
	}
}

func TestMoneyIsImmutable(t *testing.T) {
	m := money(t, usd, "1.00")
	m.Minor().SetInt64(999)
	if m.MinorString() != "100" {
		t.Fatalf("Minor() exposed internal state: %s", m.MinorString())
	}
	var zero Money
	if !zero.IsZero() || zero.Amount() != "0" {
		t.Fatalf("zero Money = %q", zero.Amount())
	}
}

// Python T03 test_multiple_lines_round_before_aggregation: two lines of 1 × 0.005 each round to
// one cent before they are added, so the total is two cents, not one.
func TestLinesRoundBeforeAggregation(t *testing.T) {
	line := MustDecimal("0.005")
	perLine, err := RoundMoney(usd, line)
	if err != nil {
		t.Fatal(err)
	}
	total, err := Sum(usd, perLine, perLine)
	if err != nil {
		t.Fatal(err)
	}
	if perLine.MinorString() != "1" || total.MinorString() != "2" {
		t.Fatalf("lines %s, total %s; want 1 + 1 = 2 minor units", perLine.MinorString(), total.MinorString())
	}
	summedFirst, _ := RoundMoney(usd, line.Add(line))
	if summedFirst.MinorString() != "1" {
		t.Fatalf("rounding the exact sum gives %s", summedFirst.MinorString())
	}
}
