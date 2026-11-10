package rules

import (
	"cmp"
	"fmt"
	"math/big"
	"slices"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Basis is what an allocation is proportional to (T02 §6, §13.4).
type Basis string

const (
	BasisQuantity Basis = "quantity" // normalized demand quantities
	BasisWeight   Basis = "weight"   // verified net weight of single-line packages
	BasisVolume   Basis = "volume"   // verified outer volume of single-line packages
	BasisAmount   Basis = "amount"   // the lines' purchase component amounts, fixed before common charges
	BasisExplicit Basis = "explicit" // non-negative integer relative weights, at least one positive
)

// LineValue is an exact non-negative value of one target line.
type LineValue struct {
	LineID string
	Value  kernel.Decimal
}

// Weight is an integer relative weight of one target line (the snapshot's base_integer).
type Weight struct {
	LineID string
	Base   *big.Int
}

// Share is what a line receives of a component: its integer weight and its amount in minor units.
type Share struct {
	LineID string
	Basis  Basis
	Base   *big.Int
	Amount kernel.Money
}

// maxScale bounds the fractional digits of a value IntegerWeights accepts: a volume is the product
// of three 6-digit dimensions.
const maxScale = 3 * QuantityScale

var digitLimit = new(big.Int).Exp(big.NewInt(10), big.NewInt(256), nil)

// IntegerWeights turns exact values into integer weights with the same ratios: each value times the
// least common multiple of the values' reduced denominators, so 1:1:1 stays 1:1:1 and 120:1.5 is
// 240:3 (T02 §13.4, Python base_integer). Values are never truncated to a fixed number of digits.
func IntegerWeights(path string, values []LineValue) ([]Weight, error) {
	type scaled struct {
		num *big.Int // value × 10^scale
		den *big.Int // reduced denominator of the value
	}
	parts := make([]scaled, len(values))
	lcm := big.NewInt(1)
	for i, v := range values {
		if v.Value.Sign() < 0 {
			return nil, fail(CodeAllocationInvalid, path, fmt.Sprintf("The basis of line %s is negative", v.LineID))
		}
		s := 0
		for s <= maxScale && !fits(v.Value, s) {
			s++
		}
		if s > maxScale {
			return nil, fail(CodeCalculationLimitExceeded, path, "An allocation basis has too many fractional digits")
		}
		num := v.Value.ScaledHalfUp(s) // exact: the value fits scale s
		pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(s)), nil)
		g := new(big.Int).GCD(nil, nil, num, pow) // gcd(0, 10^s) = 10^s: a zero weight has denominator 1
		den := new(big.Int).Quo(pow, g)
		parts[i] = scaled{num: new(big.Int).Quo(num, g), den: den}
		gcd := new(big.Int).GCD(nil, nil, lcm, den)
		lcm.Mul(lcm, new(big.Int).Quo(den, gcd))
	}
	out := make([]Weight, len(values))
	for i, v := range values {
		w := new(big.Int).Mul(parts[i].num, new(big.Int).Quo(lcm, parts[i].den))
		if w.CmpAbs(digitLimit) >= 0 {
			return nil, fail(CodeCalculationLimitExceeded, path, "An integer allocation weight has more than 256 digits")
		}
		out[i] = Weight{LineID: v.LineID, Base: w}
	}
	return out, nil
}

// Allocate applies CST-INV-004: total, a non-negative amount in minor units, is split over the
// weights by the largest remainder method. Each line first gets the floor of its exact share; the
// minor units left go one each to the largest remainders, ties to the smaller line ID in byte
// (ASCII) order. The shares sum to total exactly and come back in ascending line ID order. A line
// with weight 0 gets nothing; no weights, a negative weight, a repeated line or a zero total weight
// is ALLOCATION_INVALID: nothing is split evenly by default. 0.05 USD over three equal weights is
// 0.02, 0.02, 0.01.
func Allocate(path string, total kernel.Money, basis Basis, weights []Weight) ([]Share, error) {
	if total.Sign() < 0 {
		return nil, fail(kernel.CodeInvalidKnownValue, path, "Negative costs are not supported")
	}
	if len(weights) == 0 {
		return nil, fail(CodeAllocationInvalid, path, "An allocation has at least one target line")
	}
	sorted := slices.Clone(weights)
	slices.SortFunc(sorted, func(a, b Weight) int { return cmp.Compare(a.LineID, b.LineID) })
	sum := new(big.Int)
	shares := make([]kernel.Share, len(sorted))
	for i, w := range sorted {
		if i > 0 && sorted[i-1].LineID == w.LineID {
			return nil, fail(kernel.CodeDuplicateLineID, path, fmt.Sprintf("Line %s is a target twice", w.LineID))
		}
		if w.Base == nil || w.Base.Sign() < 0 {
			return nil, fail(CodeAllocationInvalid, path, fmt.Sprintf("The weight of line %s is negative", w.LineID))
		}
		if new(big.Int).Mul(total.Minor(), w.Base).CmpAbs(digitLimit) >= 0 {
			return nil, fail(CodeCalculationLimitExceeded, path, "An allocation product has more than 256 digits")
		}
		sum.Add(sum, w.Base)
		shares[i] = kernel.Share{ID: w.LineID, Weight: kernel.DecimalFromBigInt(w.Base)}
	}
	if sum.Sign() == 0 {
		return nil, fail(CodeAllocationInvalid, path, "The weights sum to zero; nothing is split evenly by default")
	}
	parts, err := total.Allocate(shares)
	if err != nil {
		return nil, fail(CodeAllocationInvalid, path, "")
	}
	out := make([]Share, len(sorted))
	for i, w := range sorted {
		out[i] = Share{LineID: w.LineID, Basis: basis, Base: new(big.Int).Set(w.Base), Amount: parts[i]}
	}
	return out, nil
}

// ExplicitWeight is an explicit relative weight: an integer 0–10^9, not a ratio.
type ExplicitWeight struct {
	LineID string
	Weight int64
}

// AllocationRule is how a charge is allocated: its basis and its targets (target_line_ids), or its
// explicit weights. Weight and volume allocation need the verified measurement it names.
type AllocationRule struct {
	Path        string
	Basis       Basis
	Targets     []string
	Weights     []ExplicitWeight
	Measurement *Measurement
}

// Check applies the verify-time rules of an allocation rule (logistics_offer.verify): a known
// basis; explicit weights per line once, each 0–10^9, at least one positive (ALLOCATION_INVALID);
// targets once; weight and volume name a measurement (COST_INPUT_NOT_READY).
func (r AllocationRule) Check() error {
	switch r.Basis {
	case BasisExplicit:
		if len(r.Weights) == 0 {
			return fail(CodeAllocationInvalid, r.Path+"/weights", "Explicit allocation lists at least one weight")
		}
		ids := make([]string, len(r.Weights))
		positive := false
		for i, w := range r.Weights {
			ids[i] = w.LineID
			if w.Weight < 0 || w.Weight > MaxExplicitWeight {
				return fail(CodeAllocationInvalid, index(r.Path, "weights", i)+"/weight", "An explicit weight is an integer from 0 to 1000000000")
			}
			positive = positive || w.Weight > 0
		}
		if i := repeated(ids); i >= 0 {
			return fail(kernel.CodeDuplicateLineID, index(r.Path, "weights", i)+"/line_id", "")
		}
		if !positive {
			return fail(CodeAllocationInvalid, r.Path+"/weights", "At least one explicit weight is positive")
		}
		return nil
	case BasisQuantity, BasisAmount, BasisWeight, BasisVolume:
	default:
		return fail(kernel.CodeSchemaInvalid, r.Path+"/basis", "")
	}
	if len(r.Targets) == 0 {
		return fail(CodeAllocationInvalid, r.Path+"/target_line_ids", "An allocation has at least one target line")
	}
	if err := unique(r.Targets, r.Path+"/target_line_ids"); err != nil {
		return err
	}
	if (r.Basis == BasisWeight || r.Basis == BasisVolume) && r.Measurement == nil {
		return fail(CodeCostInputNotReady, r.Path+"/measurement_ref", "Weight and volume allocation need a verified measurement")
	}
	return nil
}

// BaseLine is a line a charge may be allocated to: a demand line of the scenario, its normalized
// demand quantity and its purchase amount (the sum of its purchase component shares, calculation
// currency, before any common charge).
type BaseLine struct {
	LineID   string
	Quantity kernel.Decimal
	Purchase kernel.Money
}

// Weigh returns the integer weights of the rule's targets over base, the scenario's purchased
// lines; a target outside base is REFERENCE_INVALID. Weight and volume sum the single-line packages
// of the measurement per target (a mixed package is DEPENDENCY_NOT_SUPPORTED), and every target
// needs a positive measured value (COST_INPUT_NOT_READY).
func (r AllocationRule) Weigh(base []BaseLine) ([]Weight, error) {
	if err := r.Check(); err != nil {
		return nil, err
	}
	lines := make(map[string]BaseLine, len(base))
	for _, b := range base {
		lines[b.LineID] = b
	}
	if r.Basis == BasisExplicit {
		out := make([]Weight, len(r.Weights))
		for i, w := range r.Weights {
			if _, ok := lines[w.LineID]; !ok {
				return nil, fail(kernel.CodeReferenceInvalid, index(r.Path, "weights", i)+"/line_id", "The scenario purchases no such line")
			}
			out[i] = Weight{LineID: w.LineID, Base: big.NewInt(w.Weight)}
		}
		return out, nil
	}
	for i, id := range r.Targets {
		if _, ok := lines[id]; !ok {
			return nil, fail(kernel.CodeReferenceInvalid, index(r.Path, "target_line_ids", i), "The scenario purchases no such line")
		}
	}
	values := make([]LineValue, len(r.Targets))
	switch r.Basis {
	case BasisAmount:
		out := make([]Weight, len(r.Targets))
		for i, id := range r.Targets {
			out[i] = Weight{LineID: id, Base: lines[id].Purchase.Minor()}
		}
		return out, nil
	case BasisQuantity:
		for i, id := range r.Targets {
			values[i] = LineValue{LineID: id, Value: lines[id].Quantity}
		}
	default:
		measured, err := r.Measurement.perLine(r.Basis, lines)
		if err != nil {
			return nil, err
		}
		for i, id := range r.Targets {
			v := measured[id]
			if v.Sign() <= 0 {
				return nil, fail(CodeCostInputNotReady, r.Path+"/measurement_ref", fmt.Sprintf("The measurement does not cover line %s", id))
			}
			values[i] = LineValue{LineID: id, Value: v}
		}
	}
	return IntegerWeights(r.Path, values)
}

// Distribute allocates a charge's component amount by its rule (Weigh, then Allocate).
func (r AllocationRule) Distribute(total kernel.Money, base []BaseLine) ([]Share, error) {
	weights, err := r.Weigh(base)
	if err != nil {
		return nil, err
	}
	return Allocate(r.Path, total, r.Basis, weights)
}
