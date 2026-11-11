package api

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Error catalog V0.1 (06_ENGINEERING_CONTRACTS/02_CONTRACT_CATALOG/03_Error_Catalog_V0.1.csv): each
// of these rows lists TRD among its contexts and answers 422.
var catalogRows = []string{
	"CALCULATION_LIMIT_EXCEEDED", "DEPENDENCY_NOT_SUPPORTED", "COMPARISON_BASIS_MISMATCH", "BASE_QUANTITY_MISMATCH",
	"COST_SNAPSHOT_NOT_ELIGIBLE", "CHARGE_DOUBLE_COUNT", "CHARGE_SCOPE_UNRESOLVED", "TAX_POLICY_UNCONFIRMED",
	"FX_SCOPE_MISMATCH", "FX_STALE", "PAYMENT_TOTAL_MISMATCH", "MARGIN_POLICY_REQUIRES_APPROVAL",
}

func TestCalcCodesFollowTheErrorCatalog(t *testing.T) {
	if len(calcCodes) != len(catalogRows) {
		t.Fatalf("%d codes declared, the catalog has %d rows", len(calcCodes), len(catalogRows))
	}
	for _, code := range catalogRows {
		category, ok := CalcCategory(code)
		if !ok || category != kernel.CategoryInvalid {
			t.Errorf("%s: %q (declared %v), want invalid", code, category, ok)
		}
		r := RejectCalc(code, "/args/data/lines/0")
		if r.Code != code || r.Category != kernel.CategoryInvalid || r.Fields[0].Path != "/args/data/lines/0" {
			t.Errorf("RejectCalc(%s) = %+v", code, r)
		}
	}
	for _, shared := range []string{kernel.CodeSchemaInvalid, kernel.CodeInvalidKnownValue, kernel.CodeReferenceInvalid,
		kernel.CodeDuplicateLineID, kernel.CodeDraftNotReady} {
		if _, ok := CalcCategory(shared); ok {
			t.Errorf("%s is a shared kernel code, not TRD's to declare", shared)
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("an undeclared code must panic")
		}
	}()
	_ = RejectCalc("NOT_A_TRD_CODE", "/")
}
