package kernel

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
)

var ErrInvalidAllocation = errors.New("kernel: invalid allocation")

// Share is one recipient of an allocation. Its ID breaks ties; its Weight is not negative.
type Share struct {
	ID     string
	Weight Decimal
}

// AllocateLargestRemainder splits total, a non-negative number of minor units, in proportion to
// the weights. Each share first receives the floor of its exact portion; the units left over go
// one each to the largest fractional remainders, ties broken by ascending ID in byte order. The
// result keeps the input order and sums to total exactly.
//
// This is the payment-stage rule of the Python contracts (T03 §5.3): exact shares, floors, then
// remainders in descending order with ASCII stage_id ascending on ties. Whether a zero share is
// acceptable is the caller's decision.
func AllocateLargestRemainder(total *big.Int, shares []Share) ([]*big.Int, error) {
	if total.Sign() < 0 {
		return nil, fmt.Errorf("%w: negative total %s", ErrInvalidAllocation, total)
	}
	if len(shares) == 0 {
		return nil, fmt.Errorf("%w: no shares", ErrInvalidAllocation)
	}
	weightSum := new(big.Rat)
	seen := make(map[string]bool, len(shares))
	for _, s := range shares {
		if s.ID == "" || seen[s.ID] {
			return nil, fmt.Errorf("%w: missing or repeated share ID %q", ErrInvalidAllocation, s.ID)
		}
		seen[s.ID] = true
		if s.Weight.Sign() < 0 {
			return nil, fmt.Errorf("%w: negative weight for %q", ErrInvalidAllocation, s.ID)
		}
		weightSum.Add(weightSum, s.Weight.rat())
	}
	if weightSum.Sign() == 0 {
		return nil, fmt.Errorf("%w: weights sum to zero", ErrInvalidAllocation)
	}

	type part struct {
		index     int
		floor     *big.Int
		remainder *big.Rat
	}
	parts := make([]part, len(shares))
	allocated := new(big.Int)
	for i, s := range shares {
		exact := new(big.Rat).SetInt(total)
		exact.Mul(exact, s.Weight.rat()).Quo(exact, weightSum)
		floor := new(big.Int).Quo(exact.Num(), exact.Denom()) // exact ≥ 0
		remainder := new(big.Rat).Sub(exact, new(big.Rat).SetInt(floor))
		parts[i] = part{index: i, floor: floor, remainder: remainder}
		allocated.Add(allocated, floor)
	}
	left := new(big.Int).Sub(total, allocated).Int64() // 0 ≤ left < len(shares)

	order := make([]part, len(parts))
	copy(order, parts)
	sort.SliceStable(order, func(a, b int) bool {
		if c := order[a].remainder.Cmp(order[b].remainder); c != 0 {
			return c > 0
		}
		return shares[order[a].index].ID < shares[order[b].index].ID
	})
	for k := int64(0); k < left; k++ {
		order[k].floor.Add(order[k].floor, big.NewInt(1))
	}

	out := make([]*big.Int, len(parts))
	for i, p := range parts {
		out[i] = p.floor
	}
	return out, nil
}
