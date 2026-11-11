package rules

import (
	"fmt"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// PriceBasis says whether an amount includes its tax.
type PriceBasis string

const (
	TaxInclusive     PriceBasis = "inclusive"      // the amount is the gross
	TaxExclusive     PriceBasis = "exclusive"      // the amount is the net
	TaxNotApplicable PriceBasis = "not_applicable" // no tax applies; the rate is 0
)

// Recovery is how much of the tax paid is recovered and therefore not a cost (CST-INV-007).
type Recovery string

const (
	// RecoveryUnknown blocks the calculation: it is never assumed recoverable or nonrecoverable.
	RecoveryUnknown Recovery = "unknown"
	// Recoverable: the cost is the net amount.
	Recoverable Recovery = "recoverable"
	// Nonrecoverable: the cost is the gross amount.
	Nonrecoverable Recovery = "nonrecoverable"
	// PartiallyRecoverable: the cost is the gross amount less the recoverable tax, the net amount
	// times the recoverable rate rounded HALF_UP to the currency's minor units; the recoverable
	// rate is not above the tax rate. It covers, for instance, an exporter's refund rate below the
	// levy rate. 113 CNY inclusive at 0.13 with a recoverable rate of 0.09 costs 104 CNY.
	PartiallyRecoverable Recovery = "partial"
)

// TaxTreatment is the tax an amount is subject to: rate, price basis and recovery treatment from a
// verified tax profile revision, and for a partial recovery the recoverable rate from the verified
// tax application (CN-D02). Nothing here is inferred from a product name or from the tax rate.
type TaxTreatment struct {
	Path            string
	Rate            kernel.Decimal
	PriceBasis      PriceBasis
	Recovery        Recovery
	RecoverableRate *kernel.Decimal // partial only
}

// TaxedCost is an amount split into net, tax and gross (Net + Tax = Gross), the tax expected to be
// recovered and the cost (Gross − Recovered). An expected recovery is not profit; it only keeps
// the recovered tax out of the cost.
type TaxedCost struct {
	Net, Tax, Gross kernel.Money
	Recovered       kernel.Money
	Cost            kernel.Money
}

// Cost applies the tax treatment to amount, an exact non-negative amount of currency c (T02 §13.3
// steps 2–3): an inclusive amount is rounded as the gross, net = gross ÷ (1 + rate) rounded and
// tax = gross − net, so inclusive tax is removed once; an exclusive amount is rounded as the net
// and tax = net × rate rounded; every rounding is HALF_UP to c's minor units. The cost is then the
// net (recoverable), the gross (nonrecoverable) or the gross less round(net × recoverable rate)
// (partial; never more than the tax itself). An unknown treatment is TAX_TREATMENT_UNKNOWN; a
// partial treatment without a recoverable rate, or with one above the tax rate, and a recoverable
// rate on any other treatment are TAX_RECOVERY_RATE_INVALID.
func (t TaxTreatment) Cost(c kernel.Currency, amount kernel.Decimal) (TaxedCost, error) {
	switch t.Recovery {
	case Recoverable, Nonrecoverable, PartiallyRecoverable:
	case RecoveryUnknown:
		return TaxedCost{}, fail(CodeTaxTreatmentUnknown, t.Path+"/recovery_treatment",
			"The tax recovery treatment is not verified; it is never assumed")
	default:
		return TaxedCost{}, fail(CodeDependencyNotSupported, t.Path+"/recovery_treatment", fmt.Sprintf("Recovery treatment %q is not supported", t.Recovery))
	}
	if !fits(t.Rate, FactorScale) || t.Rate.Sign() < 0 || t.Rate.Cmp(one) > 0 {
		return TaxedCost{}, fail(kernel.CodeInvalidKnownValue, t.Path+"/rate", "A tax rate is between 0 and 1")
	}
	if t.Recovery == PartiallyRecoverable {
		rr := t.RecoverableRate
		switch {
		case rr == nil:
			return TaxedCost{}, fail(CodeTaxRecoveryRateInvalid, t.Path+"/recoverable_rate", "A partial recovery needs a verified recoverable rate")
		case !fits(*rr, FactorScale) || rr.Sign() < 0 || rr.Cmp(t.Rate) > 0:
			return TaxedCost{}, fail(CodeTaxRecoveryRateInvalid, t.Path+"/recoverable_rate",
				fmt.Sprintf("The recoverable rate %s is not between 0 and the tax rate %s", rr, t.Rate))
		}
	} else if t.RecoverableRate != nil {
		return TaxedCost{}, fail(CodeTaxRecoveryRateInvalid, t.Path+"/recoverable_rate", "Only a partial recovery carries a recoverable rate")
	}
	if amount.Sign() < 0 {
		return TaxedCost{}, fail(kernel.CodeInvalidKnownValue, t.Path, "Negative costs are not supported")
	}
	var split kernel.TaxSplit
	var err error
	switch t.PriceBasis {
	case TaxInclusive:
		split, err = kernel.TaxInclusive(c, amount, t.Rate)
	case TaxExclusive:
		split, err = kernel.TaxExclusive(c, amount, t.Rate)
	case TaxNotApplicable:
		if !t.Rate.IsZero() {
			return TaxedCost{}, fail(kernel.CodeInvalidKnownValue, t.Path+"/rate", "Where no tax applies the rate is 0")
		}
		split, err = kernel.TaxExclusive(c, amount, t.Rate)
	default:
		return TaxedCost{}, fail(kernel.CodeSchemaInvalid, t.Path+"/price_basis", "")
	}
	if err != nil {
		return TaxedCost{}, fail(CodeCalculationLimitExceeded, t.Path, "The taxed amount is out of range")
	}
	out := TaxedCost{Net: split.Net, Tax: split.Tax, Gross: split.Gross}
	switch t.Recovery {
	case Recoverable:
		out.Recovered = split.Tax
	case Nonrecoverable:
		out.Recovered, _ = kernel.NewMoney(c, 0)
	default:
		if out.Recovered, err = split.Net.MulHalfUp(*t.RecoverableRate); err != nil {
			return TaxedCost{}, fail(CodeCalculationLimitExceeded, t.Path, "The recoverable tax is out of range")
		}
		if over, _ := out.Recovered.Cmp(split.Tax); over > 0 {
			out.Recovered = split.Tax // a rounding of an inclusive split never recovers more than was paid
		}
	}
	out.Cost, _ = split.Gross.Sub(out.Recovered)
	return out, nil
}

// CostKind is what a tax application covers: goods bought from a supplier or a service charged by a
// service provider. One never stands in for the other.
type CostKind string

const (
	Goods   CostKind = "goods"
	Service CostKind = "service"
)

// BusinessModelCNM01 is the only business model tax applications are verified for (V1.0).
const BusinessModelCNM01 = "CN-M01"

// TaxApplication is a verified tax_application revision: for which business model, cost kind,
// counterparty and subject (SKU profile for goods, fee type for services) a tax profile revision
// applies. Identities are opaque strings the rules compare.
type TaxApplication struct {
	Path          string
	BusinessModel string
	Kind          CostKind
	Counterparty  string
	Subject       string
	TaxProfile    string
}

// Applies reports, as REFERENCE_INVALID at the application's path, whether the application does not
// cover a cost of kind from counterparty for subject under taxProfile; a goods application never
// serves a service charge and the other way round.
func (a TaxApplication) Applies(kind CostKind, counterparty, subject, taxProfile string) error {
	switch {
	case a.BusinessModel != BusinessModelCNM01:
		return fail(kernel.CodeReferenceInvalid, a.Path, "The tax application is for another business model")
	case a.Kind != kind:
		return fail(kernel.CodeReferenceInvalid, a.Path, fmt.Sprintf("A %s tax application does not apply to %s", a.Kind, kind))
	case a.Counterparty != counterparty:
		return fail(kernel.CodeReferenceInvalid, a.Path, "The tax application is for another counterparty")
	case a.Subject != subject:
		return fail(kernel.CodeReferenceInvalid, a.Path, "The tax application is for another subject")
	case a.TaxProfile != taxProfile:
		return fail(kernel.CodeReferenceInvalid, a.Path, "The tax application binds another tax profile revision")
	}
	return nil
}
