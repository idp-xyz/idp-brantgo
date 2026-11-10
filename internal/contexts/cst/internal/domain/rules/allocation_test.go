package rules

import (
	"fmt"
	"math/big"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func equalWeights(ids ...string) []Weight {
	out := make([]Weight, len(ids))
	for i, id := range ids {
		out[i] = Weight{LineID: id, Base: big.NewInt(1)}
	}
	return out
}

func amounts(shares []Share) string {
	parts := make([]string, len(shares))
	for i, s := range shares {
		parts[i] = s.LineID + "=" + s.Amount.Amount()
	}
	return strings.Join(parts, " ")
}

// CST-INV-004 and T02 §11 费用尾差 / §13.4: 0.05 USD over three equal weights is 0.02, 0.02, 0.01
// in stable ID order, summing to 0.05; Python test_three_line_one_cent_allocation_has_deterministic_
// remainder: 0.01 over L3, L2, L1 (given in that order) goes to L1.
func TestLargestRemainder(t *testing.T) {
	shares, err := Allocate("/fees/0/allocation", money(t, usd, 5), BasisQuantity, equalWeights("L1", "L2", "L3"))
	ok(t, err)
	if got := amounts(shares); got != "L1=0.02 L2=0.02 L3=0.01" {
		t.Fatalf("0.05: %s", got)
	}
	shares, err = Allocate("/fees/0/allocation", money(t, usd, 1), BasisQuantity, equalWeights("L3", "L2", "L1"))
	ok(t, err)
	if got := amounts(shares); got != "L1=0.01 L2=0.00 L3=0.00" {
		t.Fatalf("0.01: %s", got)
	}
}

// T02 §16.5: a volume weight ratio of 1 : 10^18 is legal and splits 60.00 USD into 0.00 and 60.00.
func TestHugeWeightRatio(t *testing.T) {
	huge := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	shares, err := Allocate("/fees/0/allocation", money(t, usd, 6000), BasisVolume,
		[]Weight{{LineID: "L1", Base: big.NewInt(1)}, {LineID: "L2", Base: huge}})
	ok(t, err)
	if got := amounts(shares); got != "L1=0.00 L2=60.00" {
		t.Fatalf("got %s", got)
	}
	limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(256), nil)
	_, err = Allocate("/fees/0/allocation", money(t, usd, 6000), BasisVolume, []Weight{{LineID: "L1", Base: limit}})
	rejected(t, err, CodeCalculationLimitExceeded, "/fees/0/allocation")
}

// CST-INV-004 negative: a zero total weight is ALLOCATION_INVALID; nothing is split evenly by
// default (Python test_zero_explicit_allocation_weights_are_rejected for the explicit rule below).
func TestAllocationRefusals(t *testing.T) {
	_, err := Allocate("/a", money(t, usd, 100), BasisExplicit, []Weight{{LineID: "L1", Base: big.NewInt(0)}})
	rejected(t, err, CodeAllocationInvalid, "/a")
	_, err = Allocate("/a", money(t, usd, 100), BasisExplicit, nil)
	rejected(t, err, CodeAllocationInvalid, "/a")
	_, err = Allocate("/a", money(t, usd, 100), BasisExplicit, []Weight{{LineID: "L1", Base: big.NewInt(-1)}, {LineID: "L2", Base: big.NewInt(2)}})
	rejected(t, err, CodeAllocationInvalid, "/a")
	_, err = Allocate("/a", money(t, usd, 100), BasisExplicit, equalWeights("L1", "L1"))
	rejected(t, err, kernel.CodeDuplicateLineID, "/a")
	_, err = Allocate("/a", money(t, usd, -1), BasisExplicit, equalWeights("L1"))
	rejected(t, err, kernel.CodeInvalidKnownValue, "/a")
}

// T02 §13.4: exact values become integer weights by the least common multiple of their
// denominators, never truncated: 120 : 1.5 is 240 : 3, 0.000001 : 1 is 1 : 1000000.
func TestIntegerWeights(t *testing.T) {
	micro := dec("0.000001")
	cube := micro.Mul(micro).Mul(micro) // a volume of 0.000001 m per axis
	for _, c := range []struct {
		values []kernel.Decimal
		want   string
	}{
		{[]kernel.Decimal{dec("120"), dec("1.5")}, "240 3"},
		{[]kernel.Decimal{micro, dec("1")}, "1 1000000"},
		{[]kernel.Decimal{dec("1"), dec("1"), dec("1")}, "1 1 1"},
		{[]kernel.Decimal{dec("0"), dec("2.5")}, "0 5"},
		{[]kernel.Decimal{cube, dec("1")}, "1 1000000000000000000"},
	} {
		values := make([]LineValue, len(c.values))
		for i, v := range c.values {
			values[i] = LineValue{LineID: fmt.Sprint(i), Value: v}
		}
		w, err := IntegerWeights("/a", values)
		ok(t, err)
		got := make([]string, len(w))
		for i, x := range w {
			got[i] = x.Base.String()
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%v: %v, want %s", c.values, got, c.want)
		}
	}
}

// fixtureBase is the scenario's purchased lines: L1–L3 of 1 PCS each, 5.00 USD of goods each.
func fixtureBase(t *testing.T) []BaseLine {
	var out []BaseLine
	for _, id := range []string{"L1", "L2", "L3"} {
		out = append(out, BaseLine{LineID: id, Quantity: dec("1"), Purchase: money(t, usd, 500)})
	}
	return out
}

// Allocation rules of a fee: Python test_zero_explicit_allocation_weights_are_rejected,
// test_weight_allocation_requires_measurement and test_allocation_cannot_target_an_unpurchased_line.
func TestAllocationRules(t *testing.T) {
	base := fixtureBase(t)
	zero := AllocationRule{Path: "/fees/0/allocation", Basis: BasisExplicit, Weights: []ExplicitWeight{{LineID: "L1", Weight: 0}}}
	rejected(t, zero.Check(), CodeAllocationInvalid, "/fees/0/allocation/weights")
	weight := AllocationRule{Path: "/fees/0/allocation", Basis: BasisWeight, Targets: []string{"L1"}}
	rejected(t, weight.Check(), CodeCostInputNotReady, "/fees/0/allocation/measurement_ref")
	unpurchased := AllocationRule{Path: "/fees/0/allocation", Basis: BasisExplicit, Weights: []ExplicitWeight{{LineID: "not.purchased", Weight: 1}}}
	ok(t, unpurchased.Check())
	_, err := unpurchased.Distribute(money(t, usd, 6000), base)
	rejected(t, err, kernel.CodeReferenceInvalid, "/fees/0/allocation/weights/0/line_id")
	target := AllocationRule{Path: "/fees/0/allocation", Basis: BasisQuantity, Targets: []string{"L1", "L9"}}
	_, err = target.Distribute(money(t, usd, 6000), base)
	rejected(t, err, kernel.CodeReferenceInvalid, "/fees/0/allocation/target_line_ids/1")
	tooHeavy := AllocationRule{Path: "/fees/0/allocation", Basis: BasisExplicit, Weights: []ExplicitWeight{{LineID: "L1", Weight: MaxExplicitWeight + 1}}}
	rejected(t, tooHeavy.Check(), CodeAllocationInvalid, "/fees/0/allocation/weights/0/weight")
	twice := AllocationRule{Path: "/fees/0/allocation", Basis: BasisExplicit, Weights: []ExplicitWeight{{LineID: "L1", Weight: 1}, {LineID: "L1", Weight: 2}}}
	rejected(t, twice.Check(), kernel.CodeDuplicateLineID, "/fees/0/allocation/weights/1/line_id")
	unknown := AllocationRule{Path: "/fees/0/allocation", Basis: "revenue", Targets: []string{"L1"}}
	rejected(t, unknown.Check(), kernel.CodeSchemaInvalid, "/fees/0/allocation/basis")

	explicit := AllocationRule{Path: "/fees/0/allocation", Basis: BasisExplicit,
		Weights: []ExplicitWeight{{LineID: "L1", Weight: 1}, {LineID: "L2", Weight: 3}, {LineID: "L3", Weight: 0}}}
	shares, err := explicit.Distribute(money(t, usd, 6000), base)
	ok(t, err)
	if got := amounts(shares); got != "L1=15.00 L2=45.00 L3=0.00" {
		t.Fatalf("explicit: %s", got)
	}
	amount := AllocationRule{Path: "/fees/0/allocation", Basis: BasisAmount, Targets: []string{"L1", "L2", "L3"}}
	base[0].Purchase = money(t, usd, 1000)
	shares, err = amount.Distribute(money(t, usd, 6000), base)
	ok(t, err)
	if got := amounts(shares); got != "L1=30.00 L2=15.00 L3=15.00" || shares[0].Base.String() != "1000" {
		t.Fatalf("amount: %s", got)
	}
}

// Weight and volume allocation use the verified measurement's single-line packages; a mixed package
// is DEPENDENCY_NOT_SUPPORTED and a target without measured goods COST_INPUT_NOT_READY.
func TestMeasuredAllocation(t *testing.T) {
	base := fixtureBase(t)
	m := &Measurement{Path: "/measurement", Kind: Measured, ObservedAt: clock, Packages: []Package{
		{Path: "/measurement/packages/0", ID: "P1", Count: 2, LengthM: dec("1"), WidthM: dec("1"), HeightM: dec("0.5"), NetKG: dec("9"), GrossKG: dec("10"), LineIDs: []string{"L1"}},
		{Path: "/measurement/packages/1", ID: "P2", Count: 1, LengthM: dec("1"), WidthM: dec("1"), HeightM: dec("1"), NetKG: dec("2"), GrossKG: dec("3"), LineIDs: []string{"L2"}},
	}}
	byWeight := AllocationRule{Path: "/fees/0/allocation", Basis: BasisWeight, Targets: []string{"L1", "L2"}, Measurement: m}
	shares, err := byWeight.Distribute(money(t, usd, 6000), base)
	ok(t, err)
	if got := amounts(shares); got != "L1=54.00 L2=6.00" || shares[0].Base.String() != "18" { // 18 kg : 2 kg
		t.Fatalf("weight: %s", got)
	}
	byVolume := byWeight
	byVolume.Basis = BasisVolume
	shares, err = byVolume.Distribute(money(t, usd, 6000), base)
	ok(t, err)
	if got := amounts(shares); got != "L1=30.00 L2=30.00" { // 1 m³ : 1 m³
		t.Fatalf("volume: %s", got)
	}
	uncovered := byWeight
	uncovered.Targets = []string{"L1", "L3"}
	_, err = uncovered.Distribute(money(t, usd, 6000), base)
	rejected(t, err, CodeCostInputNotReady, "/fees/0/allocation/measurement_ref")
	mixed := *m
	mixed.Packages = slices.Clone(m.Packages)
	mixed.Packages[1].LineIDs = []string{"L2", "L3"}
	byWeight.Measurement = &mixed
	_, err = byWeight.Distribute(money(t, usd, 6000), base)
	rejected(t, err, CodeDependencyNotSupported, "/measurement/packages/1/line_ids")
}

// Independent arithmetic for CST-INV-004 on random vectors: the shares sum to the total, each share
// is the floor or the ceiling of its exact share, exactly total − Σ floors lines get the extra unit,
// and those are the largest remainders with ties to the smaller ID. The oracle uses int64 divmod.
func TestAllocationAgainstOracle(t *testing.T) {
	rng := rand.New(rand.NewPCG(20261110, 4)) // fixed seed: the vectors are reproducible
	for range 2000 {
		n := 1 + rng.IntN(8)
		total := rng.Int64N(100000)
		weights := make([]Weight, n)
		w := make([]int64, n)
		var sum int64
		for i := range n {
			w[i] = rng.Int64N(1000)
			if i == 0 && w[i] == 0 {
				w[i] = 1
			}
			sum += w[i]
			weights[i] = Weight{LineID: fmt.Sprintf("L%02d", rng.IntN(1000)*10+i), Base: big.NewInt(w[i])}
		}
		shares, err := Allocate("/a", money(t, usd, total), BasisExplicit, weights)
		ok(t, err)

		type exact struct {
			id         string
			floor, rem int64
		}
		ex := make([]exact, n)
		var floors int64
		for i := range n {
			q, r := total*w[i]/sum, total*w[i]%sum
			ex[i] = exact{weights[i].LineID, q, r}
			floors += q
		}
		slices.SortFunc(ex, func(a, b exact) int {
			if a.rem != b.rem {
				return int(b.rem - a.rem)
			}
			return strings.Compare(a.id, b.id)
		})
		want := map[string]int64{}
		for i, e := range ex {
			want[e.id] = e.floor
			if int64(i) < total-floors {
				want[e.id]++
			}
		}
		var got int64
		for _, s := range shares {
			got += s.Amount.Minor().Int64()
			if s.Amount.Minor().Int64() != want[s.LineID] {
				t.Fatalf("total %d weights %v: %s got %s, want %d", total, w, s.LineID, s.Amount.MinorString(), want[s.LineID])
			}
		}
		if got != total || !slices.IsSortedFunc(shares, func(a, b Share) int { return strings.Compare(a.LineID, b.LineID) }) {
			t.Fatalf("sum %d of %d, order %v", got, total, shares)
		}
	}
}

// The measurement rules of Python test_costing_domains.py: test_measurement_volume_contradiction_is_
// rejected (declared 2 m³ for a derived 1 m³), test_gross_weight_cannot_be_less_than_net_weight
// (8 kg gross for 9 kg net), test_future_measurement_is_not_a_verified_fact (observed 10:00 at a
// 09:00 clock); a verified estimate needs the policy's allow_verified_estimates.
func TestMeasurementCheck(t *testing.T) {
	base := func() Measurement { // tests/fixtures/costing_inputs.json measurement.record_verified
		return Measurement{Path: "/args/data", Kind: Measured, ObservedAt: time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC),
			DeclaredVolume: dec("1"), HasDeclaredVolume: true, Packages: []Package{{Path: "/args/data/packages/0", ID: "P1", Count: 1,
				LengthM: dec("1"), WidthM: dec("1"), HeightM: dec("1"), NetKG: dec("9"), GrossKG: dec("10"), LineIDs: []string{"L1"}}}}
	}
	ok(t, base().Check(clock))
	for _, c := range []struct {
		name       string
		edit       func(m *Measurement)
		code, path string
	}{
		{"volume contradiction", func(m *Measurement) { m.DeclaredVolume = dec("2") }, CodeMeasurementConflict, "/args/data/declared_volume_m3"},
		{"gross below net", func(m *Measurement) { m.Packages[0].GrossKG = dec("8") }, kernel.CodeInvalidKnownValue, "/args/data/packages/0/gross_kg"},
		{"future observation", func(m *Measurement) { m.ObservedAt = clock.Add(time.Hour) }, kernel.CodeInvalidKnownValue, "/args/data/observed_at"},
		{"axis above 1000 m", func(m *Measurement) { m.Packages[0].LengthM = dec("1000.000001") }, CodeCalculationLimitExceeded, "/args/data/packages/0/length_m"},
		{"zero count", func(m *Measurement) { m.Packages[0].Count = 0 }, kernel.CodeInvalidKnownValue, "/args/data/packages/0/count"},
		{"duplicate package", func(m *Measurement) { m.Packages = append(m.Packages, m.Packages[0]) }, kernel.CodeDuplicateLineID, "/args/data/packages/0/id"},
		{"no packages", func(m *Measurement) { m.Packages = nil }, kernel.CodeInvalidKnownValue, "/args/data/packages"},
		{"seven fractional digits", func(m *Measurement) { m.Packages[0].NetKG = dec("8.0000001") }, kernel.CodeSchemaInvalid, "/args/data/packages/0/net_kg"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := base()
			c.edit(&m)
			rejected(t, m.Check(clock), c.code, c.path)
		})
	}
	estimate := base()
	estimate.Kind = Estimated
	ok(t, estimate.Check(clock))
	rejected(t, estimate.Usable(false), CodeCostInputNotReady, "/args/data/kind")
	ok(t, estimate.Usable(true))
}
