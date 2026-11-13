package kernel

import (
	"slices"
	"strings"
	"testing"
)

// ISO 3166-1 alpha-2 (S5 D-8): the 249 officially assigned codes, sorted and unique; reserved,
// user-assigned, deleted and malformed codes are refused.
func TestCountryCodesAreTheAssignedOnes(t *testing.T) {
	codes := strings.Fields(countryCodes)
	if len(codes) != 249 || CountryCodeCount() != 249 || !slices.IsSorted(codes) || len(slices.Compact(slices.Clone(codes))) != 249 {
		t.Fatalf("%d codes (%d distinct), sorted %v", len(codes), CountryCodeCount(), slices.IsSorted(codes))
	}
	for _, c := range codes {
		if len(c) != 2 || c[0] < 'A' || c[0] > 'Z' || c[1] < 'A' || c[1] > 'Z' {
			t.Errorf("malformed code %q", c)
		}
	}
	for _, c := range []string{"CN", "DE", "GB", "US", "HK", "TW", "MO", "AQ", "BV", "HM", "SS", "BQ", "CW", "SX", "AX"} {
		if !ValidCountryCode(c) {
			t.Errorf("%s is assigned", c)
		}
	}
	for _, c := range []string{"", "C", "CHN", "cn", "Cn", "UK", "EU", "UN", "XK", "XA", "ZZ", "AA", "QM", "AN", "YU", "CS", "C1", " CN"} {
		if ValidCountryCode(c) {
			t.Errorf("%q is not an assigned code", c)
		}
	}
}
