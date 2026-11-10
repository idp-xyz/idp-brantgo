package kernel

import (
	"errors"
	"fmt"
	"math/big"
	"math/rand/v2"
	"testing"
)

func shares(pairs ...string) []Share {
	out := make([]Share, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, Share{ID: pairs[i], Weight: MustDecimal(pairs[i+1])})
	}
	return out
}

func allocate(t *testing.T, total int64, s []Share) string {
	t.Helper()
	parts, err := AllocateLargestRemainder(big.NewInt(total), s)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(parts)
}

// Python T03 test_payment_tail_uses_remainder_order_and_zero_stage_is_rejected.
func TestPaymentTailFollowsRemainderOrder(t *testing.T) {
	stages := shares("P1", "33.33", "P2", "33.33", "P3", "33.34")
	if got := allocate(t, 5, stages); got != "[2 1 2]" {
		t.Fatalf("0.05 over 33.33/33.33/33.34 = %s, want [2 1 2]", got)
	}
	if got := allocate(t, 1, stages); got != "[0 0 1]" {
		t.Fatalf("0.01 over 33.33/33.33/33.34 = %s, want [0 0 1] (the caller rejects the zero stage)", got)
	}
}

func TestTiesGoToTheSmallerID(t *testing.T) {
	if got := allocate(t, 5, shares("P1", "1", "P2", "1", "P3", "1")); got != "[2 2 1]" {
		t.Fatalf("0.05 in thirds = %s, want [2 2 1]", got)
	}
	if got := allocate(t, 5, shares("P3", "1", "P2", "1", "P1", "1")); got != "[1 2 2]" {
		t.Fatalf("input order must not decide ties: %s, want [1 2 2]", got)
	}
}

func TestAllocationRejectsInvalidInput(t *testing.T) {
	for name, tc := range map[string]struct {
		total int64
		s     []Share
	}{
		"negative total":  {-1, shares("A", "1")},
		"no shares":       {1, nil},
		"repeated ID":     {1, shares("A", "1", "A", "1")},
		"empty ID":        {1, shares("", "1")},
		"negative weight": {1, []Share{{ID: "A", Weight: MustDecimal("-1")}, {ID: "B", Weight: MustDecimal("2")}}},
		"zero weights":    {1, shares("A", "0", "B", "0")},
	} {
		if _, err := AllocateLargestRemainder(big.NewInt(tc.total), tc.s); !errors.Is(err, ErrInvalidAllocation) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// Every part is the floor or the ceiling of its exact share, the parts add up to the total, and
// the result per ID does not depend on the input order.
func TestAllocationProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(20261106, 1))
	for round := range 2000 {
		n := 1 + rng.IntN(8)
		s := make([]Share, n)
		for i := range s {
			s[i] = Share{ID: fmt.Sprintf("S%02d", rng.IntN(100)*10+i), Weight: DecimalFromMinor(big.NewInt(rng.Int64N(10000)), 2)}
		}
		s[rng.IntN(n)].Weight = DecimalFromInt(1 + rng.Int64N(100))
		total := big.NewInt(rng.Int64N(1_000_000))
		parts, err := AllocateLargestRemainder(total, s)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		sum, weights := new(big.Int), Decimal{}
		for _, x := range s {
			weights = weights.Add(x.Weight)
		}
		byID := map[string]string{}
		for i, p := range parts {
			sum.Add(sum, p)
			exact, _ := DecimalFromBigInt(total).Mul(s[i].Weight).Quo(weights)
			floor := exact.Floor()
			if p.Cmp(floor) < 0 || p.Cmp(new(big.Int).Add(floor, big.NewInt(1))) > 0 {
				t.Fatalf("round %d: part %s outside [%s, %s+1]", round, p, floor, floor)
			}
			byID[s[i].ID] = p.String()
		}
		if sum.Cmp(total) != 0 {
			t.Fatalf("round %d: parts sum to %s, want %s", round, sum, total)
		}
		reversed := make([]Share, n)
		for i := range s {
			reversed[n-1-i] = s[i]
		}
		again, _ := AllocateLargestRemainder(total, reversed)
		for i, p := range again {
			if byID[reversed[i].ID] != p.String() {
				t.Fatalf("round %d: %s got %s, then %s after reordering", round, reversed[i].ID, byID[reversed[i].ID], p)
			}
		}
	}
}
