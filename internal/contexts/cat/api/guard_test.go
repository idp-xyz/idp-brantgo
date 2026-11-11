package api

import (
	"errors"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func TestSkuGuardVerdict(t *testing.T) {
	ref := SpecRef{SkuID: "sku.1", SpecRevision: 1}
	if args := SkuGuardArgs(ref); len(args) != 2 || args[0] != "sku.1" || args[1] != int64(1) {
		t.Fatalf("args %v", args)
	}
	row := func(known bool, status *string, current *int64, code *string) func(dest ...any) error {
		return func(dest ...any) error {
			if len(dest) != 4 {
				return errors.New("one destination per column")
			}
			*dest[0].(*bool) = known
			*dest[1].(**string) = status
			*dest[2].(**int64) = current
			*dest[3].(**string) = code
			return nil
		}
	}
	str := func(s string) *string { return &s }
	num := func(n int64) *int64 { return &n }
	for _, c := range []struct {
		scan    func(dest ...any) error
		want    string
		status  string
		current int64
	}{
		{row(true, str(StatusConfirmed), num(1), nil), "OK", StatusConfirmed, 1},
		{row(true, str(StatusDraft), num(0), str(CodeReferenceStale)), "REFERENCE_STALE /args/sku", StatusDraft, 0},
		{row(true, str(StatusConfirmed), num(2), str(CodeReferenceStale)), "REFERENCE_STALE /args/sku", StatusConfirmed, 2},
		{row(true, str(StatusInactive), num(0), str(kernel.CodeReferenceInvalid)), "REFERENCE_INVALID /args/sku", StatusInactive, 0},
		{row(false, str(StatusConfirmed), num(1), str(kernel.CodeReferenceInvalid)), "REFERENCE_INVALID /args/sku", StatusConfirmed, 1},
		{row(false, nil, nil, str(kernel.CodeReferenceInvalid)), "REFERENCE_INVALID /args/sku", "", 0},
	} {
		v, err := ScanSkuGuard(ref, c.scan)
		if err != nil {
			t.Fatal(err)
		}
		err = v.Check("/args/sku")
		if got := code(err); got != c.want || v.Current() != (c.want == "OK") || v.Ref != ref || v.Status != c.status || v.CurrentRevision != c.current {
			t.Errorf("%+v: %s, want %s", v, got, c.want)
		}
		if rej, ok := kernel.AsRejection(err); ok && rej.Code == CodeReferenceStale && rej.Category != kernel.CategoryConflict {
			t.Errorf("stale is a conflict: %+v", rej)
		}
	}
	if (SkuGuardVerdict{}).Check("/x") == nil {
		t.Fatal("a verdict the guard did not give is REFERENCE_INVALID")
	}
	broken := errors.New("connection lost")
	if _, err := ScanSkuGuard(ref, func(...any) error { return broken }); !errors.Is(err, broken) {
		t.Fatal(err)
	}
}
