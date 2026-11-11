package api

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Error catalog V0.1: FORMAL_DOCUMENT_MISSING, FORMAL_DOCUMENT_INTEGRITY_FAILURE and
// DOCUMENT_BINDING_MISSING list TRD and answer 422. TRD declares each code once: none of them is
// QuoteRoot's or the calculation's, and the other codes a formal document answers with are declared
// there with the catalog's categories.
func TestDocumentCodesFollowTheErrorCatalog(t *testing.T) {
	rows := []string{"FORMAL_DOCUMENT_MISSING", "FORMAL_DOCUMENT_INTEGRITY_FAILURE", "DOCUMENT_BINDING_MISSING"}
	if len(documentCodes) != len(rows) {
		t.Fatalf("%d codes declared, the catalog has %d rows", len(documentCodes), len(rows))
	}
	for _, code := range rows {
		category, ok := DocumentCategory(code)
		if !ok || category != kernel.CategoryInvalid {
			t.Errorf("%s: %q (declared %v)", code, category, ok)
		}
		if _, clash := codes[code]; clash {
			t.Errorf("%s is also a QuoteRoot code", code)
		}
		if _, clash := calcCodes[code]; clash {
			t.Errorf("%s is also a calculation code", code)
		}
		r := RejectDocument(code, "/document")
		if r.Code != code || r.Category != kernel.CategoryInvalid || r.Fields[0].Path != "/document" {
			t.Errorf("RejectDocument(%s) = %+v", code, r)
		}
	}
	for code, want := range map[string]kernel.Category{CodeDependencyNotSupported: kernel.CategoryInvalid,
		CodeCalculationLimitExceeded: kernel.CategoryInvalid, CodeBusinessHashMismatch: kernel.CategoryInvalid,
		CodeReferenceStale: kernel.CategoryConflict} {
		if got, ok := Category(code); !ok || got != want {
			t.Errorf("%s: %q", code, got)
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("an undeclared code must panic")
		}
	}()
	_ = RejectDocument(CodeBusinessHashMismatch, "/")
}
