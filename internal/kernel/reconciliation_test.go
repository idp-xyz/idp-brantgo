package kernel

import (
	"strings"
	"testing"
)

// A request hash is the SHA-256 the action ledgers store (action_result.request_hash CHECK
// ^[0-9a-f]{64}$): ActionReconciliation v1 takes nothing else.
func TestValidRequestHash(t *testing.T) {
	for h, want := range map[string]bool{
		strings.Repeat("0", 64):       true,
		strings.Repeat("af09", 16):    true,
		strings.Repeat("A", 64):       false,
		strings.Repeat("0", 63):       false,
		strings.Repeat("0", 65):       false,
		strings.Repeat("0", 63) + "g": false,
		"":                            false,
		strings.Repeat("0", 62) + "é": false,
	} {
		if got := ValidRequestHash(h); got != want {
			t.Errorf("%q: %v, want %v", h, got, want)
		}
	}
}
