package calc

import (
	"fmt"
	"math/big"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func stages(pairs ...string) []Stage {
	out := make([]Stage, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Stage{StageID: pairs[i], Percent: dec(pairs[i+1])})
	}
	return out
}

// TRD-INV-008 and T03 §5.3: the payment plan by largest remainder sums to the payable.
func TestAllocatePayments(t *testing.T) {
	for _, c := range []struct {
		name, source string
		payable      int64
		stages       []Stage
		want         string
	}{
		{"960.00 at 30/70", "T03 §5.3; TRD-INV-008", 96000, stages("deposit", "30", "balance", "70"), "28800 67200"},
		{"960.00 at 100", "test_freeze_ignores_client_calculation_and_persists_server_result (PYT-0442)", 96000, stages("P1", "100"), "96000"},
		{"0.05 at 33.33/33.33/33.34: remainders, then stage ID", "test_payment_tail_uses_remainder_order_and_zero_stage_is_rejected (PYT-0452)",
			5, stages("P1", "33.33", "P2", "33.33", "P3", "33.34"), "2 1 2"},
		{"1084.80 at 100", "test_exclusive_sales_tax_uses_dedicated_reviewed_application (PYT-0319)", 108480, stages("P1", "100"), "108480"},
		{"971.30 at 100", "test_foreign_currency_taxed_fee_composition_keeps_payment_and_margin_units (PYT-0347)", 97130, stages("P1", "100"), "97130"},
		{"six-digit percentages", "POL PercentFormat", 1000001, stages("x", "33.333333", "y", "66.666667"), "333334 666667"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := AllocatePayments("/args/data", minor(t, usd, c.payable), c.stages)
			ok(t, err)
			if s := amountsOf(got); s != c.want {
				t.Fatalf("got %s, want %s", s, c.want)
			}
			for i, p := range got {
				if p.StageID != c.stages[i].StageID || !p.Percent.Equal(c.stages[i].Percent) || p.Amount.Currency() != usd {
					t.Fatalf("payment %d: %+v", i, p)
				}
			}
		})
	}
	// T03 §5.3: 6 units at 50/25/25 are 3 + 1.5 + 1.5, and the unit left goes to "B" (0x42) before
	// "a" (0x61), equal remainders taking ASCII stage ID order.
	got, err := AllocatePayments("/args/data", minor(t, usd, 6), stages("b", "50", "a", "25", "B", "25"))
	ok(t, err)
	if s := amountsOf(got); s != "3 1 2" {
		t.Fatalf("got %s, want 3 1 2", s)
	}
}

func amountsOf(ps []Payment) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.Amount.MinorString()
	}
	return strings.Join(parts, " ")
}

func TestAllocatePaymentsRefusals(t *testing.T) {
	many := make([]Stage, MaxStages+1)
	for i := range many {
		many[i] = Stage{StageID: fmt.Sprintf("S%02d", i), Percent: dec("1")}
	}
	for _, c := range []struct {
		name, source string
		payable      int64
		stages       []Stage
	}{
		{"a payable that leaves a stage at 0", "test_payment_tail_uses_remainder_order_and_zero_stage_is_rejected (PYT-0452); TRD-INV-008",
			1, stages("P1", "33.33", "P2", "33.33", "P3", "33.34")},
		{"percentages summing to 99.99", "POL-INV-004; _server_calculation sum == 100", 96000, stages("P1", "33.33", "P2", "66.66")},
		{"percentages summing to 100.01", "POL-INV-004", 96000, stages("P1", "50", "P2", "50.01")},
		{"a stage of 0", "_server_calculation percent > 0", 96000, stages("P1", "0", "P2", "100")},
		{"a negative stage", "_server_calculation percent > 0", 96000, stages("P1", "-10", "P2", "110")},
		{"a repeated stage ID", "_server_calculation distinct stage IDs", 96000, stages("P1", "50", "P1", "50")},
		{"no stage", "_server_calculation 0 < stages", 96000, nil},
		{"more than 20 stages", "_server_calculation stages ≤ 20", 96000, many},
		{"seven fractional digits", "POL PercentFormat", 96000, stages("P1", "33.3333333", "P2", "66.6666667")},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := AllocatePayments("/args/data", minor(t, usd, c.payable), c.stages)
			rejected(t, err, CodePaymentTotalMismatch, "/args/data/payment_terms_ref")
		})
	}
}

// AllocatePayments against an independent integer largest remainder: with P_i the percentages
// scaled by 10^6 (ΣP_i = 10^8), stage i gets ⌊A·P_i ÷ 10^8⌋ and the A − Σ⌊…⌋ units left go to the
// largest remainders A·P_i mod 10^8, ties to the smaller stage ID; the plan sums to A.
func TestAllocatePaymentsMatchesIndependentIntegerArithmetic(t *testing.T) {
	rng := rand.New(rand.NewPCG(20261111, 8))
	total := int64(100_000_000)
	for i := 0; i < 3000; i++ {
		n := 1 + rng.IntN(MaxStages)
		cuts := make([]int64, 0, n+1)
		cuts = append(cuts, 0, total)
		for len(cuts) < n+1 {
			cuts = append(cuts, 1+rng.Int64N(total-1))
		}
		sort.Slice(cuts, func(a, b int) bool { return cuts[a] < cuts[b] })
		scaled := make([]int64, 0, n)
		in := make([]Stage, 0, n)
		for k := 1; k < len(cuts); k++ {
			if p := cuts[k] - cuts[k-1]; p > 0 {
				id := fmt.Sprintf("S%c%d", 'a'+rng.IntN(3), k) // IDs out of input order
				scaled = append(scaled, p)
				in = append(in, Stage{StageID: id, Percent: kernel.DecimalFromMinor(big.NewInt(p), 6)})
			}
		}
		payable := rng.Int64N(1_000_000_000)
		got, err := AllocatePayments("/args/data", minor(t, usd, payable), in)

		floors := make([]int64, len(in))
		rems := make([]int64, len(in))
		left := payable
		for k, p := range scaled {
			prod := new(big.Int).Mul(big.NewInt(payable), big.NewInt(p))
			q, r := new(big.Int).QuoRem(prod, big.NewInt(total), new(big.Int))
			floors[k], rems[k] = q.Int64(), r.Int64()
			left -= floors[k]
		}
		order := make([]int, len(in))
		for k := range order {
			order[k] = k
		}
		sort.SliceStable(order, func(a, b int) bool {
			if rems[order[a]] != rems[order[b]] {
				return rems[order[a]] > rems[order[b]]
			}
			return in[order[a]].StageID < in[order[b]].StageID
		})
		for k := int64(0); k < left; k++ {
			floors[order[k]]++
		}
		zero := false
		sum := int64(0)
		for _, v := range floors {
			zero = zero || v == 0
			sum += v
		}
		if sum != payable {
			t.Fatalf("independent allocation sums to %d, not %d", sum, payable)
		}
		if zero {
			rejected(t, err, CodePaymentTotalMismatch, "/args/data/payment_terms_ref")
			continue
		}
		ok(t, err)
		for k, p := range got {
			if p.Amount.Minor().Int64() != floors[k] {
				t.Fatalf("payable %d, stages %+v: stage %d got %s, want %d", payable, in, k, p.Amount.MinorString(), floors[k])
			}
		}
	}
}
