package api

import "testing"

func TestNetUnitCostRoundsOnce(t *testing.T) {
	// 3000 minor for 3, basis 1 → 1000. 10 minor for 3 → 3 (10/3 rounds HALF_UP to 3).
	got, err := NetUnitCost("USD", 2, "3000", "1", "3")
	if err != nil || got != "1000" {
		t.Fatalf("%s %v", got, err)
	}
	got, err = NetUnitCost("USD", 2, "10", "1", "3")
	if err != nil || got != "3" {
		t.Fatalf("half-up %s %v", got, err)
	}
	got, err = NetUnitCost("USD", 0, "10", "3", "2")
	// 10 × 3 / 2 = 15 exactly.
	if err != nil || got != "15" {
		t.Fatalf("basis %s %v", got, err)
	}
}
