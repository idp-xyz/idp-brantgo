package shipment

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func pdec(s string) kernel.Decimal {
	d, err := kernel.ParseDecimal(s, kernel.PriceFormat)
	if err != nil {
		panic(err)
	}
	return d
}

// The price of a shipment line is the order line's at the bound revision (baseline 4.1: 出运保存价格、
// 币种); the amount of a part is the line's amount × part / whole, rounded HALF_UP once, so the whole
// quantity gives the line's amounts exactly (S-D: 120 PCS × USD 8 = 960.00).
func TestPricesAndShares(t *testing.T) {
	s := &Shipment{ID: "shipment.1", Lines: []Line{{ID: "S1", OrderLineID: "L1", OrderQuantity: pdec("120"), Quantity: pdec("50")},
		{ID: "S2", OrderLineID: "L2", OrderQuantity: pdec("3"), Quantity: pdec("1")}}}
	f := PriceFact{CurrencyID: "USD", CurrencyRevision: 1, Lines: map[string]LinePriceFact{
		"L1": {UnitPrice: "8.0000", NetMinor: "96000", TaxMinor: "0", PayableMinor: "96000"},
		"L2": {UnitPrice: "0.33333333", NetMinor: "100", TaxMinor: "13", PayableMinor: "113"}}}
	prices, err := s.Prices(f)
	if err != nil {
		t.Fatal(err)
	}
	p1, p2 := prices["S1"], prices["S2"]
	if a := p1.Of(pdec("50"), pdec("120")); a.Net.String() != "40000" || a.Tax.String() != "0" || a.Payable.String() != "40000" {
		t.Fatalf("50 of 120 at USD 8: %+v", a)
	}
	if a := p1.Of(pdec("120"), pdec("120")); a.Net.String() != "96000" {
		t.Fatalf("the whole line: %+v", a)
	}
	// 100 × 1 / 3 = 33.33… → 33; 13 / 3 = 4.33… → 4; 100 × 2 / 3 = 66.66… → 67.
	if a := p2.Of(pdec("1"), pdec("3")); a.Net.String() != "33" || a.Tax.String() != "4" || a.Payable.String() != "37" {
		t.Fatalf("a third: %+v", a)
	}
	if a := p2.Of(pdec("2"), pdec("3")); a.Net.String() != "67" || a.Tax.String() != "9" {
		t.Fatalf("two thirds: %+v", a)
	}
	if !p2.UnitPrice.Equal(pdec("0.33333333")) || p1.CurrencyID != "USD" || p1.CurrencyRevision != 1 {
		t.Fatalf("%+v", p1)
	}
	if sum := p1.Of(pdec("1"), pdec("120")).Add(p2.Of(pdec("1"), pdec("3"))); sum.Net.String() != "833" {
		t.Fatalf("sum %+v", sum)
	}

	for name, edit := range map[string]func(*PriceFact){
		"no currency":   func(f *PriceFact) { f.CurrencyID = "" },
		"no line price": func(f *PriceFact) { delete(f.Lines, "L2") },
		"a decimal amount": func(f *PriceFact) {
			f.Lines["L1"] = LinePriceFact{UnitPrice: "8", NetMinor: "960.00", TaxMinor: "0", PayableMinor: "0"}
		},
		"a negative tax": func(f *PriceFact) {
			f.Lines["L1"] = LinePriceFact{UnitPrice: "8", NetMinor: "1", TaxMinor: "-1", PayableMinor: "0"}
		},
		"no unit price": func(f *PriceFact) { f.Lines["L1"] = LinePriceFact{NetMinor: "1", TaxMinor: "0", PayableMinor: "1"} },
	} {
		g := PriceFact{CurrencyID: f.CurrencyID, CurrencyRevision: 1, Lines: map[string]LinePriceFact{"L1": f.Lines["L1"], "L2": f.Lines["L2"]}}
		edit(&g)
		if _, err := s.Prices(g); err == nil {
			t.Errorf("%s accepted", name)
		} else if rej, _ := kernel.AsRejection(err); rej.Code != kernel.CodeReferenceInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	for s, ok := range map[string]bool{"0": true, "-5": true, "960": true, "00": false, "-0": false, "1e3": false, "": false, "9.6": false} {
		if _, got := Minor(s); got != ok {
			t.Errorf("Minor(%q) = %v", s, got)
		}
	}
}
