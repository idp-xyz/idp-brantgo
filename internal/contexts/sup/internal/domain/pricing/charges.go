package pricing

import (
	"fmt"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Party is who bears a charge under the export sale: the exporter (seller) or the customer (buyer).
type Party string

const (
	Seller Party = "seller"
	Buyer  Party = "buyer"
)

// ChargeableKind is what a logistics charge is billed on (T02 §6).
type ChargeableKind string

const (
	ChargeFixed               ChargeableKind = "fixed"
	ChargeActualWeight        ChargeableKind = "actual_weight"
	ChargeVolume              ChargeableKind = "volume"
	ChargeMaxActualVolumetric ChargeableKind = "max_actual_volumetric"
)

// Rounding is how a chargeable quantity is rounded to the billing increment.
type Rounding string

const (
	RoundCeil   Rounding = "ceil"
	RoundHalfUp Rounding = "half_up"
)

// RoundingScope is where the rounding (and, for max_actual_volumetric, the maximum) applies.
type RoundingScope string

const (
	PerPackage RoundingScope = "per_package"
	Shipment   RoundingScope = "shipment"
)

// ChargeableRule is how the chargeable quantity of a charge follows from a measurement. Measured
// says whether the charge names the measurement it is taken from; SUP stores the reference, CST
// owns the measurement.
type ChargeableRule struct {
	Path             string
	Kind             ChargeableKind
	BillingIncrement kernel.Decimal
	Rounding         Rounding
	RoundingScope    RoundingScope
	MaxScope         RoundingScope
	DivisorM3PerKG   kernel.Decimal
	Measured         bool
}

// Check applies the verify-time rules of a chargeable rule exactly as CST's ChargeableRule.Check.
func (r ChargeableRule) Check() error {
	switch r.Kind {
	case ChargeFixed:
		return nil
	case ChargeActualWeight, ChargeVolume, ChargeMaxActualVolumetric:
	default:
		return Fail(CodeUnsupportedPricingMode, r.Path+"/kind", fmt.Sprintf("Chargeable basis %q is not supported", r.Kind))
	}
	if r.BillingIncrement.Sign() <= 0 || !fits(r.BillingIncrement, QuantityScale) {
		return Fail(kernel.CodeInvalidKnownValue, r.Path+"/billing_increment", "A billing increment is positive with at most 6 fractional digits")
	}
	switch r.Rounding {
	case RoundCeil, RoundHalfUp:
	default:
		return Fail(kernel.CodeSchemaInvalid, r.Path+"/rounding_mode", "")
	}
	switch r.RoundingScope {
	case PerPackage, Shipment:
	default:
		return Fail(kernel.CodeSchemaInvalid, r.Path+"/rounding_scope", "")
	}
	if r.Kind == ChargeMaxActualVolumetric {
		if r.MaxScope != r.RoundingScope {
			return Fail(CodeUnsupportedPricingMode, r.Path+"/max_scope", "The maximum and the rounding apply at the same level")
		}
		if r.DivisorM3PerKG.Sign() <= 0 || !fits(r.DivisorM3PerKG, FactorScale) {
			return Fail(kernel.CodeInvalidKnownValue, r.Path+"/divisor_m3_per_kg", "A volumetric divisor is positive and explicit")
		}
	}
	return nil
}

// CheckMeasured refuses a measured charge without the measurement it is taken from, with the code
// and path CST's ChargeableRule.Quantity gives one (COST_INPUT_NOT_READY): a charge that cannot be
// measured is never verified.
func (r ChargeableRule) CheckMeasured() error {
	if r.Kind != ChargeFixed && !r.Measured {
		return Fail(CodeCostInputNotReady, r.Path+"/measurement_ref", "A measured charge needs a verified measurement")
	}
	return nil
}

// BillingKind is how a charge's amount follows from its chargeable quantity.
type BillingKind string

const (
	BillFixed    BillingKind = "fixed"
	BillPerBasis BillingKind = "per_basis"
	BillTiered   BillingKind = "tiered"
)

// Billing is a charge's tariff, in Unit (KG, or M3 for a volume basis) for a measured charge.
type Billing struct {
	Path            string
	Kind            BillingKind
	Amount          kernel.Decimal
	Rate            kernel.Decimal
	BasisQuantity   kernel.Decimal
	Minimum         kernel.Decimal
	Unit            kernel.UnitCode
	Tiers           Schedule // tiered: mode and tiers; its basis is BasisQuantity
	ZeroPriceReason string
}

// Check applies the verify-time rules of a tariff together with its chargeable rule exactly as
// CST's Billing.Check: a fixed chargeable basis only with a fixed tariff, a measured one only with
// a per-basis or tiered tariff in the unit it measures; amounts of the price format; a zero fixed
// amount or rate explained.
func (b Billing) Check(rule ChargeableRule) error {
	if err := rule.Check(); err != nil {
		return err
	}
	switch b.Kind {
	case BillFixed:
		if rule.Kind != ChargeFixed {
			return Fail(CodeUnsupportedPricingMode, b.Path+"/kind", "A measured chargeable basis is billed per basis or by tiers")
		}
		if err := checkAmount(b.Amount, b.Path+"/amount"); err != nil {
			return err
		}
		if b.Amount.IsZero() && b.ZeroPriceReason == "" {
			return Fail(CodeCostInputNotReady, b.Path+"/zero_price_reason", "A zero charge needs a verified reason")
		}
		return nil
	case BillPerBasis, BillTiered:
	default:
		return Fail(CodeUnsupportedPricingMode, b.Path+"/kind", fmt.Sprintf("Billing %q is not supported", b.Kind))
	}
	if rule.Kind == ChargeFixed {
		return Fail(CodeUnsupportedPricingMode, b.Path+"/kind", "A fixed chargeable basis takes a fixed amount")
	}
	want := kernel.UnitCode("KG")
	if rule.Kind == ChargeVolume {
		want = "M3"
	}
	if b.Unit != want {
		return Fail(kernel.CodeReferenceInvalid, b.Path+"/unit_ref", fmt.Sprintf("The chargeable quantity is in %s", want))
	}
	if err := CheckQuantity(b.BasisQuantity, b.Path+"/basis_quantity"); err != nil {
		return err
	}
	if err := checkAmount(b.Minimum, b.Path+"/minimum_amount"); err != nil {
		return err
	}
	if b.Kind == BillTiered {
		s := b.Tiers
		s.Path, s.BasisQuantity, s.Scope = b.Path, b.BasisQuantity, TierScopeSKU
		return s.Check()
	}
	if err := checkAmount(b.Rate, b.Path+"/rate"); err != nil {
		return err
	}
	if b.Rate.IsZero() && b.ZeroPriceReason == "" {
		return Fail(CodeCostInputNotReady, b.Path+"/zero_price_reason", "A zero rate needs a verified reason")
	}
	return nil
}

// Basis is what an allocation is proportional to (T02 §6, §13.4).
type Basis string

const (
	BasisQuantity Basis = "quantity"
	BasisWeight   Basis = "weight"
	BasisVolume   Basis = "volume"
	BasisAmount   Basis = "amount"
	BasisExplicit Basis = "explicit"
)

// ExplicitWeight is an explicit relative weight: an integer 0–10^9, not a ratio.
type ExplicitWeight struct {
	LineID string
	Weight int64
}

// AllocationRule is how a charge is allocated: its basis and targets, or its explicit weights.
// Measured says whether it names the measurement weight and volume allocation is taken from.
type AllocationRule struct {
	Path     string
	Basis    Basis
	Targets  []string
	Weights  []ExplicitWeight
	Measured bool
}

// Check applies the verify-time rules of an allocation rule exactly as CST's AllocationRule.Check:
// a known basis; explicit weights per line once, each 0–10^9, at least one positive
// (ALLOCATION_INVALID); targets once; weight and volume name a measurement (COST_INPUT_NOT_READY).
func (r AllocationRule) Check() error {
	switch r.Basis {
	case BasisExplicit:
		if len(r.Weights) == 0 {
			return Fail(CodeAllocationInvalid, r.Path+"/weights", "Explicit allocation lists at least one weight")
		}
		ids := make([]string, len(r.Weights))
		positive := false
		for i, w := range r.Weights {
			ids[i] = w.LineID
			if w.Weight < 0 || w.Weight > MaxExplicitWeight {
				return Fail(CodeAllocationInvalid, index(r.Path, "weights", i)+"/weight", "An explicit weight is an integer from 0 to 1000000000")
			}
			positive = positive || w.Weight > 0
		}
		if i := repeated(ids); i >= 0 {
			return Fail(kernel.CodeDuplicateLineID, index(r.Path, "weights", i)+"/line_id", "")
		}
		if !positive {
			return Fail(CodeAllocationInvalid, r.Path+"/weights", "At least one explicit weight is positive")
		}
		return nil
	case BasisQuantity, BasisAmount, BasisWeight, BasisVolume:
	default:
		return Fail(kernel.CodeSchemaInvalid, r.Path+"/basis", "")
	}
	if len(r.Targets) == 0 {
		return Fail(CodeAllocationInvalid, r.Path+"/target_line_ids", "An allocation has at least one target line")
	}
	if err := Unique(r.Targets, r.Path+"/target_line_ids"); err != nil {
		return err
	}
	if (r.Basis == BasisWeight || r.Basis == BasisVolume) && !r.Measured {
		return Fail(CodeCostInputNotReady, r.Path+"/measurement_ref", "Weight and volume allocation need a verified measurement")
	}
	return nil
}

// Fee is one charge of a logistics offer revision as the verify-time rules need it.
type Fee struct {
	Path         string
	ID           string
	ObligationID string
	Party        Party
	Chargeable   ChargeableRule
	Billing      Billing
	Allocation   AllocationRule
}

// CheckFees applies the verify-time rules of one logistics offer's fees exactly as CST's
// CheckFees: IDs once; an obligation once, whatever ID carries it (CHARGE_DOUBLE_COUNT); a known
// party; each tariff with its chargeable rule and each allocation rule.
func CheckFees(fees []Fee) error {
	if len(fees) > MaxComponents {
		return Fail(CodeCalculationLimitExceeded, fees[MaxComponents].Path, "An offer has at most 500 charges")
	}
	ids := make([]string, len(fees))
	obligations := make([]string, len(fees))
	for i, f := range fees {
		ids[i], obligations[i] = f.ID, f.ObligationID
	}
	if i := repeated(ids); i >= 0 {
		return Fail(kernel.CodeDuplicateLineID, fees[i].Path+"/id", "")
	}
	if i := repeated(obligations); i >= 0 {
		return Fail(CodeChargeDoubleCount, fees[i].Path+"/obligation_id", "The obligation is already charged under another ID")
	}
	for _, f := range fees {
		if f.Party != Seller && f.Party != Buyer {
			return Fail(kernel.CodeSchemaInvalid, f.Path+"/responsible_party", "")
		}
		if err := f.Billing.Check(f.Chargeable); err != nil {
			return err
		}
		if err := f.Allocation.Check(); err != nil {
			return err
		}
	}
	return nil
}
