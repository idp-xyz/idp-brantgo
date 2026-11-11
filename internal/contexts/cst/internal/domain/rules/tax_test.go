package rules

import (
	"math/big"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func treatment(rate string, basis PriceBasis, recovery Recovery, rr *kernel.Decimal) TaxTreatment {
	return TaxTreatment{Path: "/lines/0/tax_profile_ref", Rate: dec(rate), PriceBasis: basis, Recovery: recovery, RecoverableRate: rr}
}

// CST-INV-007 and WBS S2-08 (tactical test TD-AT07): 113 CNY inclusive of 0.13 splits into 100 net
// and 13 tax; recoverable costs 100, nonrecoverable 113, partial at a recoverable rate of 0.09 costs
// 113 − 100 × 0.09 = 104.
func TestTaxRecoveryTreatments(t *testing.T) {
	for _, c := range []struct {
		recovery        Recovery
		rr              *kernel.Decimal
		cost, recovered string
	}{
		{Recoverable, nil, "100.00", "13.00"},
		{Nonrecoverable, nil, "113.00", "0.00"},
		{PartiallyRecoverable, decp("0.09"), "104.00", "9.00"},
		{PartiallyRecoverable, decp("0.13"), "100.00", "13.00"},
		{PartiallyRecoverable, decp("0"), "113.00", "0.00"},
	} {
		got, err := treatment("0.13", TaxInclusive, c.recovery, c.rr).Cost(cny, dec("113"))
		ok(t, err)
		if got.Net.Amount() != "100.00" || got.Tax.Amount() != "13.00" || got.Gross.Amount() != "113.00" ||
			got.Cost.Amount() != c.cost || got.Recovered.Amount() != c.recovered {
			t.Errorf("%s %v: %+v, want cost %s recovered %s", c.recovery, c.rr, got, c.cost, c.recovered)
		}
	}
	// 0.04 CNY inclusive of 0.13 splits into 0.04 net and 0.00 tax; round(0.04 × 0.13) = 0.01 would
	// recover more than was paid and cost less than the net, so the recovery stops at the tax.
	got, err := treatment("0.13", TaxInclusive, PartiallyRecoverable, decp("0.13")).Cost(cny, dec("0.04"))
	ok(t, err)
	if got.Tax.Amount() != "0.00" || got.Recovered.Amount() != "0.00" || got.Cost.Amount() != "0.04" {
		t.Errorf("0.04 CNY: %+v", got)
	}
}

// CST-INV-007 negatives: a partial recovery without a rate, with a rate above the tax rate (TD-AT07:
// 0.15 over 0.13) or a recoverable rate on another treatment is TAX_RECOVERY_RATE_INVALID; an
// unknown treatment is TAX_TREATMENT_UNKNOWN (Python test_unknown_tax_recovery_cannot_be_assumed_
// nonrecoverable).
func TestTaxRefusals(t *testing.T) {
	for _, c := range []struct {
		name       string
		t          TaxTreatment
		code, path string
	}{
		{"partial without rate", treatment("0.13", TaxInclusive, PartiallyRecoverable, nil), CodeTaxRecoveryRateInvalid, "/lines/0/tax_profile_ref/recoverable_rate"},
		{"partial above tax rate", treatment("0.13", TaxInclusive, PartiallyRecoverable, decp("0.15")), CodeTaxRecoveryRateInvalid, "/lines/0/tax_profile_ref/recoverable_rate"},
		{"negative recoverable rate", treatment("0.13", TaxInclusive, PartiallyRecoverable, decp("-0.01")), CodeTaxRecoveryRateInvalid, "/lines/0/tax_profile_ref/recoverable_rate"},
		{"rate on a recoverable treatment", treatment("0.13", TaxInclusive, Recoverable, decp("0.09")), CodeTaxRecoveryRateInvalid, "/lines/0/tax_profile_ref/recoverable_rate"},
		{"unknown treatment", treatment("0.13", TaxInclusive, RecoveryUnknown, nil), CodeTaxTreatmentUnknown, "/lines/0/tax_profile_ref/recovery_treatment"},
		{"unsupported treatment", treatment("0.13", TaxInclusive, "refund_estimate", nil), CodeDependencyNotSupported, "/lines/0/tax_profile_ref/recovery_treatment"},
		{"rate above 1", treatment("1.01", TaxExclusive, Nonrecoverable, nil), kernel.CodeInvalidKnownValue, "/lines/0/tax_profile_ref/rate"},
		{"not applicable with a rate", treatment("0.13", TaxNotApplicable, Nonrecoverable, nil), kernel.CodeInvalidKnownValue, "/lines/0/tax_profile_ref/rate"},
		{"unknown price basis", treatment("0.13", "maybe", Nonrecoverable, nil), kernel.CodeSchemaInvalid, "/lines/0/tax_profile_ref/price_basis"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.t.Cost(cny, dec("113"))
			rejected(t, err, c.code, c.path)
		})
	}
	_, err := treatment("0.13", TaxExclusive, Nonrecoverable, nil).Cost(cny, dec("-1"))
	rejected(t, err, kernel.CodeInvalidKnownValue, "/lines/0/tax_profile_ref")
}

// Python test_costing_domains.py tax vectors on the fixture's 600 USD of goods (the 60 USD freight
// is added in TestPythonFixtureTotals): test_nonrecoverable_exclusive_tax_increases_cost 600 ×
// 1.13 = 678.00; test_recoverable_exclusive_tax_does_not_increase_cost 600.00;
// test_inclusive_recoverable_tax_is_removed_once 600 ÷ 1.13 = 530.97 (tax removed once, not twice).
func TestPythonTaxVectors(t *testing.T) {
	for _, c := range []struct {
		basis    PriceBasis
		recovery Recovery
		want     string
	}{
		{TaxExclusive, Nonrecoverable, "678.00"},
		{TaxExclusive, Recoverable, "600.00"},
		{TaxInclusive, Recoverable, "530.97"},
		{TaxNotApplicable, Nonrecoverable, "600.00"},
	} {
		rate := "0.13"
		if c.basis == TaxNotApplicable {
			rate = "0"
		}
		got, err := treatment(rate, c.basis, c.recovery, nil).Cost(usd, dec("600"))
		ok(t, err)
		if got.Cost.Amount() != c.want {
			t.Errorf("%s %s: %s, want %s", c.basis, c.recovery, got.Cost.Amount(), c.want)
		}
	}
}

// Independent arithmetic for CST-INV-007 partial recovery: in integer minor units, recovered =
// floor((2 × net × r + d) / (2 × d)) for r = n/d (HALF_UP), cost = gross − min(recovered, tax), over
// a grid of amounts, tax rates and recoverable rates.
func TestPartialRecoveryAgainstIntegerOracle(t *testing.T) {
	rates := []struct{ rate, rr string }{{"0.13", "0.09"}, {"0.13", "0.13"}, {"0.09", "0.06"}, {"0.06", "0.03"}, {"0.17", "0.165"}}
	for _, r := range rates {
		for minor := int64(0); minor <= 30000; minor += 37 {
			amount := kernel.DecimalFromMinor(big.NewInt(minor), 2)
			for _, basis := range []PriceBasis{TaxInclusive, TaxExclusive} {
				got, err := treatment(r.rate, basis, PartiallyRecoverable, decp(r.rr)).Cost(cny, amount)
				ok(t, err)
				net, tax, gross := got.Net.Minor().Int64(), got.Tax.Minor().Int64(), got.Gross.Minor().Int64()
				if net+tax != gross {
					t.Fatalf("%s: %d + %d != %d", basis, net, tax, gross)
				}
				n, d := ratio(r.rr)
				recovered := (2*net*n + d) / (2 * d)
				recovered = min(recovered, tax)
				if got.Recovered.Minor().Int64() != recovered || got.Cost.Minor().Int64() != gross-recovered {
					t.Fatalf("%s %d at %s/%s: recovered %s cost %s, want %d and %d", basis, minor, r.rate, r.rr,
						got.Recovered.Amount(), got.Cost.Amount(), recovered, gross-recovered)
				}
			}
		}
	}
}

// ratio returns a short decimal string as n / d.
func ratio(s string) (int64, int64) {
	r, _ := new(big.Rat).SetString(s)
	return r.Num().Int64(), r.Denom().Int64()
}

// Python test_goods_cannot_reuse_service_tax_application and test_service_cannot_reuse_goods_tax_
// application (fixture tax.goods: supplier.1, sku.1.profile; tax.service: provider.1, freight).
func TestTaxApplicationsAreNotInterchangeable(t *testing.T) {
	goods := TaxApplication{Path: "/lines/0/tax_application_ref", BusinessModel: BusinessModelCNM01, Kind: Goods,
		Counterparty: "supplier.1", Subject: "sku.1.profile@1", TaxProfile: "tax.zero@1"}
	service := TaxApplication{Path: "/fees/0/tax_application_ref", BusinessModel: BusinessModelCNM01, Kind: Service,
		Counterparty: "provider.1", Subject: "freight@1", TaxProfile: "tax.zero@1"}
	ok(t, goods.Applies(Goods, "supplier.1", "sku.1.profile@1", "tax.zero@1"))
	ok(t, service.Applies(Service, "provider.1", "freight@1", "tax.zero@1"))
	rejected(t, service.Applies(Goods, "supplier.1", "sku.1.profile@1", "tax.zero@1"), kernel.CodeReferenceInvalid, "/fees/0/tax_application_ref")
	rejected(t, goods.Applies(Service, "provider.1", "freight@1", "tax.zero@1"), kernel.CodeReferenceInvalid, "/lines/0/tax_application_ref")
	rejected(t, goods.Applies(Goods, "supplier.2", "sku.1.profile@1", "tax.zero@1"), kernel.CodeReferenceInvalid, "")
	rejected(t, goods.Applies(Goods, "supplier.1", "sku.2.profile@1", "tax.zero@1"), kernel.CodeReferenceInvalid, "")
	rejected(t, goods.Applies(Goods, "supplier.1", "sku.1.profile@1", "tax.13@1"), kernel.CodeReferenceInvalid, "")
	other := goods
	other.BusinessModel = "CN-M02"
	rejected(t, other.Applies(Goods, "supplier.1", "sku.1.profile@1", "tax.zero@1"), kernel.CodeReferenceInvalid, "")
}
