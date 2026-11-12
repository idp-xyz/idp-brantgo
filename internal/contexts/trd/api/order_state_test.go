package api

import (
	"errors"
	"testing"
)

// The order-state read (S5 unit B4, A6 gap 1): ScanOrderState reads the function's row, NULLs of
// an unknown order included, and ActiveFrom says whether the order is active at a revision or a
// later one (S5 D-9: a later revision that asked no more credit stands on the earlier
// reservation).
func TestScanOrderStateAndActiveFrom(t *testing.T) {
	row := func(known bool, status *string, revision *int64, hash *string, version *int64) func(dest ...any) error {
		return func(dest ...any) error {
			*dest[0].(*bool) = known
			*dest[1].(**string) = status
			*dest[2].(**int64) = revision
			*dest[3].(**string) = hash
			*dest[4].(**int64) = version
			return nil
		}
	}
	status, hash := OrderActive, "h2"
	revision, version := int64(2), int64(6)
	s, err := ScanOrderState("order.1", row(true, &status, &revision, &hash, &version))
	if err != nil || s != (OrderState{OrderID: "order.1", Known: true, Status: OrderActive, OrderRevision: 2, OrderHash: "h2", RowVersion: 6}) {
		t.Fatalf("an active order %+v %v", s, err)
	}
	for rev, want := range map[int64]bool{1: true, 2: true, 3: false} {
		if s.ActiveFrom(rev) != want {
			t.Errorf("active from revision %d: %v", rev, !want)
		}
	}
	draft := OrderDraft
	if s, _ := ScanOrderState("order.2", row(true, &draft, &revision, &hash, &version)); s.ActiveFrom(1) {
		t.Fatalf("a draft %+v", s)
	}
	if s, err := ScanOrderState("order.none", row(false, nil, nil, nil, nil)); err != nil || s.Known || s.Status != "" || s.ActiveFrom(0) {
		t.Fatalf("an unknown order %+v %v", s, err)
	}
	broken := errors.New("scan")
	if _, err := ScanOrderState("order.1", func(...any) error { return broken }); !errors.Is(err, broken) {
		t.Fatalf("scan error %v", err)
	}
}

// ProposedAt finds the proposed revision with a binding, only one TRD derived (S5 D-9).
func TestProposedAt(t *testing.T) {
	b := OrderBinding{OrderID: "order.1", OrderRevision: 3, OrderHash: "h3"}
	f := OrderSnapshotFact{Proposed: []ProposedRevision{{AmendmentID: "a.stale", Code: "REFERENCE_STALE"},
		{AmendmentID: "a.more", Binding: b, CreditPortionMinor: "61600"}}}
	if p, ok := f.ProposedAt(b); !ok || p.AmendmentID != "a.more" {
		t.Fatalf("proposed %+v %v", p, ok)
	}
	if _, ok := f.ProposedAt(OrderBinding{OrderID: "order.1", OrderRevision: 3, OrderHash: "other"}); ok {
		t.Fatal("another hash")
	}
	if _, ok := f.ProposedAt(OrderBinding{}); ok {
		t.Fatal("the zero binding of the underived amendment")
	}
}
