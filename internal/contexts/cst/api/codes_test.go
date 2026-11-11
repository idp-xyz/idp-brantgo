package api

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Error catalog V0.1: every CST row is 422 (invalid) except REFERENCE_STALE (409, conflict).
func TestCodesFollowTheErrorCatalog(t *testing.T) {
	for code, category := range codes {
		want := kernel.CategoryInvalid
		if code == CodeReferenceStale {
			want = kernel.CategoryConflict
		}
		if category != want {
			t.Errorf("%s is %q, want %q", code, category, want)
		}
		r := Reject(code, "/args/data")
		if r.Code != code || r.Category != want || r.Fields[0].Path != "/args/data" {
			t.Errorf("Reject(%s) = %+v", code, r)
		}
	}
	if _, ok := Category(kernel.CodeSchemaInvalid); ok {
		t.Error("a shared kernel code is not CST's to declare")
	}
	defer func() {
		if recover() == nil {
			t.Error("an undeclared code must panic")
		}
	}()
	_ = Reject("NOT_A_CST_CODE", "/")
}
