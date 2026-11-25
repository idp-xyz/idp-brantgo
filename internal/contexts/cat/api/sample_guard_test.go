package api

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func TestSampleGuardCheck(t *testing.T) {
	ref := SampleRef{SkuID: "sku.1", SpecRevision: 2, Stage: StagePPSample}
	if err := (SampleGuardVerdict{Ref: ref, Known: true, Approved: true}).Check("/x"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		v    SampleGuardVerdict
		code string
	}{
		{SampleGuardVerdict{Ref: ref, Known: true, Code: CodeSampleApprovalRequired}, CodeSampleApprovalRequired},
		{SampleGuardVerdict{Ref: ref, Known: true, Code: CodeSampleSpecMismatch, ApprovedSpec: 1}, CodeSampleSpecMismatch},
		{SampleGuardVerdict{Ref: ref, Known: false, Code: kernel.CodeReferenceInvalid}, kernel.CodeReferenceInvalid},
		{SampleGuardVerdict{Ref: ref, Known: true, Code: kernel.CodeReferenceInvalid}, kernel.CodeReferenceInvalid},
	} {
		rej, ok := kernel.AsRejection(c.v.Check("/args/data/lines/0/sample_stage"))
		if !ok || rej.Code != c.code || rej.Fields[0].Path != "/args/data/lines/0/sample_stage" {
			t.Fatalf("%s: %+v", c.code, rej)
		}
		if want, declared := Category(c.code); c.code != kernel.CodeReferenceInvalid && (!declared || rej.Category != want) {
			t.Errorf("%s category %s declared %v %s", c.code, rej.Category, declared, want)
		}
	}
}

func TestScanSampleGuard(t *testing.T) {
	id, code := "sample.1", CodeSampleSpecMismatch
	spec := int64(1)
	v, err := ScanSampleGuard(SampleRef{SkuID: "sku.1", SpecRevision: 2, Stage: StagePPSample}, func(dest ...any) error {
		*(dest[0].(*bool)) = true
		*(dest[1].(*bool)) = false
		*(dest[2].(**string)) = &id
		*(dest[3].(**int64)) = &spec
		*(dest[4].(**string)) = &code
		return nil
	})
	if err != nil || !v.Known || v.Approved || v.ApprovalID != id || v.ApprovedSpec != 1 || v.Code != code {
		t.Fatalf("%+v %v", v, err)
	}
}
