package api

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// TaxQuery asks for the tax_profile revision Profile at instant At, applied to amounts in Currency.
type TaxQuery struct {
	Profile  ConfigRef
	Currency string // currency code, configured and in force
	At       time.Time
}

// Validate checks the form of the query.
func (q TaxQuery) Validate() error {
	switch {
	case !currencyCode(q.Currency):
		return schema("/currency")
	case q.At.IsZero():
		return schema("/at")
	}
	return requiredRef(&q.Profile, KindTaxProfile, "/profile")
}

// TaxRate is a tax profile revision a calculation may apply: in force at the instant asked, with a
// recovery treatment that is not unknown.
type TaxRate struct {
	Profile     ConfigRef
	ContentHash string
	Currency    kernel.Currency // with the minor units of its published definition
	Rate        kernel.Decimal
	PriceBasis  string // inclusive, exclusive or not_applicable
	Recovery    string // recoverable or nonrecoverable
}

// Split splits amount, an exact decimal in t.Currency, into net, tax and gross rounded HALF_UP to the
// currency's minor units (Python foundation.tax.preview): an inclusive amount is the gross, from
// which net = gross ÷ (1 + rate) and tax = gross − net; an exclusive amount is the net, and tax =
// net × rate; where tax does not apply the rate is 0. A negative amount is INVALID_KNOWN_VALUE.
func (t TaxRate) Split(amount kernel.Decimal) (kernel.TaxSplit, error) {
	split := kernel.TaxExclusive
	if t.PriceBasis == PriceBasisInclusive {
		split = kernel.TaxInclusive
	}
	s, err := split(t.Currency, amount, t.Rate)
	if err != nil {
		return kernel.TaxSplit{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/amount", "")
	}
	return s, nil
}

// UnitQuery asks for the unit_conversion revision Conversion for SKU SkuID at specification
// revision SpecRevision (the caller reads both from CAT) at instant At.
type UnitQuery struct {
	Conversion   ConfigRef
	SkuID        string
	SpecRevision int64
	At           time.Time
}

// Validate checks the form of the query.
func (q UnitQuery) Validate() error {
	switch {
	case !kernel.ValidIdentifier(q.SkuID):
		return schema("/sku_id")
	case q.SpecRevision < 1 || q.SpecRevision > maxVersion:
		return schema("/spec_revision")
	case q.At.IsZero():
		return schema("/at")
	}
	return requiredRef(&q.Conversion, KindUnitConversion, "/conversion")
}

// QuantityConversion is a unit conversion a calculation may apply: one From is Factor To, and To
// has ToPrecision fractional digits.
type QuantityConversion struct {
	Conversion  ConfigRef
	ContentHash string
	From, To    kernel.UnitCode
	Factor      kernel.Decimal
	ToPrecision int
}

// Convert converts q exactly (Python foundation.unit.convert): q must be a positive quantity of
// From (UNIT_SCOPE_MISMATCH for another unit), and q × Factor must be exact at To's precision. A
// result that would need rounding is INVALID_KNOWN_VALUE, never a rounded quantity.
func (c QuantityConversion) Convert(q kernel.Quantity) (kernel.Quantity, error) {
	if q.Unit() != c.From {
		return kernel.Quantity{}, Reject(CodeUnitScopeMismatch, "/unit")
	}
	if q.Value().Sign() <= 0 {
		return kernel.Quantity{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/quantity", "")
	}
	v := q.Value().Mul(c.Factor)
	if !v.RoundHalfUp(c.ToPrecision).Equal(v) {
		return kernel.Quantity{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/quantity", "Quantity conversion would lose precision")
	}
	return kernel.NewQuantity(v, c.To)
}
