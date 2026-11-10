package rules

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The vectors of this package's tests come from the T02 contract specification (V0.3.4, §11
// acceptance examples and the fixed examples of §13), the CST invariant register and the Python
// V0.50 reference tests, each cited where it is used. All rates, prices and dates are synthetic.

var (
	usd = kernel.MustCurrency("USD", 2)
	cny = kernel.MustCurrency("CNY", 2)
	eur = kernel.MustCurrency("EUR", 2)
	jpy = kernel.MustCurrency("JPY", 0)
)

func dec(s string) kernel.Decimal { return kernel.MustDecimal(s) }

func decp(s string) *kernel.Decimal {
	d := dec(s)
	return &d
}

func qty(t *testing.T, v string, unit kernel.UnitCode) kernel.Quantity {
	t.Helper()
	q, err := kernel.ParseQuantity(v, unit)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func money(t *testing.T, c kernel.Currency, minor int64) kernel.Money {
	t.Helper()
	m, err := kernel.NewMoney(c, minor)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// rejected fails the test unless err is a rejection with code whose first field is at path (any
// path when path is empty).
func rejected(t *testing.T, err error, code, path string) {
	t.Helper()
	r, ok := kernel.AsRejection(err)
	if !ok {
		t.Fatalf("got %v, want rejection %s at %q", err, code, path)
	}
	if r.Code != code || (path != "" && r.Fields[0].Path != path) {
		t.Fatalf("got %s at %s (%s), want %s at %q", r.Code, r.Fields[0].Path, r.Fields[0].Reason, code, path)
	}
	want := Categories[code]
	if want == "" {
		want = kernel.CategoryInvalid
	}
	if r.Category != want {
		t.Fatalf("%s has category %s", code, r.Category)
	}
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func flat(path, price string) Schedule {
	return Schedule{Path: path, Mode: ModeFlat, Scope: TierScopeSKU, BasisQuantity: dec("1"),
		Tiers: []Tier{{Lower: dec("0"), UnitPrice: dec(price)}}}
}

// twoTiers is T02 §11's [0,100) at 5 and [100,∞) at 4.
func twoTiers(mode PricingMode) Schedule {
	return Schedule{Path: "/lines/0", Mode: mode, Scope: TierScopeSKU, BasisQuantity: dec("1"), Tiers: []Tier{
		{Lower: dec("0"), Upper: dec("100"), Bounded: true, UnitPrice: dec("5")},
		{Lower: dec("100"), UnitPrice: dec("4")},
	}}
}
