package calc

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Error catalog V0.1: every code the calculation declares is a row whose contexts include TRD and
// that answers 422; package api pins the same rows (calc_codes_test.go).
func TestCodesFollowTheErrorCatalog(t *testing.T) {
	rows := []string{
		"CALCULATION_LIMIT_EXCEEDED", "DEPENDENCY_NOT_SUPPORTED", "COMPARISON_BASIS_MISMATCH", "BASE_QUANTITY_MISMATCH",
		"COST_SNAPSHOT_NOT_ELIGIBLE", "CHARGE_DOUBLE_COUNT", "CHARGE_SCOPE_UNRESOLVED", "TAX_POLICY_UNCONFIRMED",
		"FX_SCOPE_MISMATCH", "FX_STALE", "PAYMENT_TOTAL_MISMATCH", "MARGIN_POLICY_REQUIRES_APPROVAL",
	}
	if len(Categories) != len(rows) {
		t.Fatalf("%d codes, %d catalog rows", len(Categories), len(rows))
	}
	for _, code := range rows {
		if Categories[code] != kernel.CategoryInvalid {
			t.Errorf("%s is %q", code, Categories[code])
		}
		rejected(t, fail(code, "/args/data", ""), code, "/args/data")
	}
	rejected(t, fail(kernel.CodeDuplicateLineID, "/args/data/lines/1/line_id", ""), kernel.CodeDuplicateLineID, "/args/data/lines/1/line_id")
}
