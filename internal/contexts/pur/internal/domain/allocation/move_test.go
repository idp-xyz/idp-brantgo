package allocation

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// PUR-INV-002 under commitment changes (S5-19, PUR-INV-011): a change applied or a cancellation moves
// the draft's share to the commitment's next revision under one version of the root — the
// effective entry released, the revision's taken —; an increase is judged against what the other
// drafts hold (PURCHASE_ALLOCATION_EXCEEDED); a share moved to zero only releases.
func TestMoveToACommitmentRevision(t *testing.T) {
	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	key := Key{OrderID: "order.1", LineID: "L1"}
	unit := Unit{ID: "PCS", Revision: 1}
	a := Open(key, 1, unit, kernel.MustDecimal("120"))
	if err := a.Take("draft.1", kernel.MustDecimal("100"), 1, unit, kernel.MustDecimal("120"), "/x"); err != nil {
		t.Fatal(err)
	}
	if err := a.Take("draft.2", kernel.MustDecimal("15"), 1, unit, kernel.MustDecimal("120"), "/x"); err != nil {
		t.Fatal(err)
	}
	a.Stored = a.RowVersion
	if err := a.Move("draft.1", 2, kernel.MustDecimal("106"), 1, unit, kernel.MustDecimal("120"), at, "/change"); code(err) != CodePurchaseAllocationExceeded+" /change" {
		t.Fatalf("beyond the room: %v", err)
	}
	b := Open(key, 1, unit, kernel.MustDecimal("120"))
	_ = b.Take("draft.1", kernel.MustDecimal("100"), 1, unit, kernel.MustDecimal("120"), "/x")
	b.Stored = b.RowVersion
	if err := b.Move("draft.1", 2, kernel.MustDecimal("110"), 2, unit, kernel.MustDecimal("900"), at, "/change"); err != nil {
		t.Fatal(err)
	}
	if b.RowVersion != b.Stored+1 || !b.Allocated.Equal(kernel.MustDecimal("110")) || b.OrderRevision != 2 || !b.Demand.Equal(kernel.MustDecimal("900")) ||
		len(b.Released()) != 1 || len(b.Entries) != 2 || b.Entries[1].Revision != 2 || b.Entries[1].Taken != b.RowVersion ||
		b.Released()[0].Released != b.RowVersion || !b.Consistent() || !b.Holds("draft.1") {
		t.Fatalf("moved %+v", b)
	}
	b.Stored = b.RowVersion
	if err := b.Move("draft.1", 3, kernel.MustDecimal("0"), 2, unit, b.Demand, at, "/cancel"); err != nil {
		t.Fatal(err)
	}
	if b.Holds("draft.1") || !b.Allocated.IsZero() || b.RowVersion != b.Stored+1 || len(b.Entries) != 2 || len(b.Released()) != 1 {
		t.Fatalf("released only %+v", b)
	}
	// A judged copy without the draft forgets every entry of it, effective or released.
	if w := b.Without("draft.1"); len(w.Entries) != 0 || !w.Allocated.IsZero() {
		t.Fatalf("without %+v", w)
	}
}
