package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// RfqLinesHash is order independent, sorts line IDs bytewise as SQL's COLLATE "C" does, and
// separates fields and lines so that no two line sets share a canonical text.
func TestRfqLinesHash(t *testing.T) {
	a := []RfqRefLine{{LineID: "L2", SkuID: "sku.2", SkuSpecRevision: 1, Quantity: "30.5", UOM: "PCS"},
		{LineID: "L10", SkuID: "sku.1", SkuSpecRevision: 12, Quantity: "1", UOM: "PCS"}}
	b := []RfqRefLine{a[1], a[0]}
	if RfqLinesHash(a) != RfqLinesHash(b) {
		t.Fatal("line order changes the hash")
	}
	text := "L10\x1fsku.1\x1f12\x1f1\x1fPCS\x1eL2\x1fsku.2\x1f1\x1f30.5\x1fPCS"
	sum := sha256.Sum256([]byte(text))
	if RfqLinesHash(a) != hex.EncodeToString(sum[:]) {
		t.Fatal("canonical text")
	}
	empty := sha256.Sum256(nil)
	if RfqLinesHash(nil) != hex.EncodeToString(empty[:]) {
		t.Fatal("empty line set")
	}
	draft := []RfqRefLine{{LineID: "L1"}}
	if sum := sha256.Sum256([]byte("L1\x1f\x1f\x1f\x1f")); RfqLinesHash(draft) != hex.EncodeToString(sum[:]) {
		t.Fatal("unknown fields are empty, an unset specification revision too")
	}
	partial := a[:1]
	if RfqLinesHash(partial) == RfqLinesHash(a) {
		t.Fatal("a partial line set hashes differently")
	}
}

func TestRfqGuardVerdict(t *testing.T) {
	b := RfqBinding{RfqID: "rfq.1", RowVersion: 2, LinesHash: "h"}
	if got := RfqGuardArgs(b); len(got) != 3 || got[0] != "rfq.1" || got[1] != int64(2) || got[2] != "h" {
		t.Fatalf("args %v", got)
	}
	scan := func(known bool, status *string, version *int64, hash, code *string) func(dest ...any) error {
		return func(dest ...any) error {
			*dest[0].(*bool) = known
			*dest[1].(**string) = status
			*dest[2].(**int64) = version
			*dest[3].(**string) = hash
			*dest[4].(**string) = code
			return nil
		}
	}
	str := func(s string) *string { return &s }
	two := int64(2)
	v, err := ScanRfqGuard(b, scan(true, str(StatusReady), &two, str("h"), nil))
	if err != nil || !v.Ready() || v.Check("/rfq") != nil || v.RowVersion != 2 || v.Binding != b {
		t.Fatalf("ready %+v, %v", v, err)
	}
	v, _ = ScanRfqGuard(b, scan(true, str(StatusClosed), &two, str("h"), str(CodeReferenceStale)))
	if rej, ok := kernel.AsRejection(v.Check("/rfq")); !ok || rej.Code != CodeReferenceStale || rej.Category != kernel.CategoryConflict ||
		rej.Fields[0].Path != "/rfq" || v.Ready() {
		t.Fatalf("stale %+v", v)
	}
	v, _ = ScanRfqGuard(b, scan(false, nil, nil, nil, str(kernel.CodeReferenceInvalid)))
	if rej, ok := kernel.AsRejection(v.Check("/rfq")); !ok || rej.Code != kernel.CodeReferenceInvalid || v.Status != "" {
		t.Fatalf("unknown %+v", v)
	}
	boom := errors.New("scan")
	if _, err = ScanRfqGuard(b, func(...any) error { return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	ref := RfqRef{RfqID: "rfq.1", RowVersion: 2, Status: StatusReady, LinesHash: "h"}
	if !ref.Ready() || ref.Binding() != b {
		t.Fatal("binding")
	}
}
