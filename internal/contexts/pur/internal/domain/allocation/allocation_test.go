package allocation

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

var pcs = Unit{ID: "PCS", Revision: 1}

// TD-AT05 (PUR-INV-002; Go intentionally diverges from Python test_t04_a46_purchase_draft.py
// test_replay_line_exclusivity_and_database_immutability, where an order line enters one draft
// only): 120 pieces of one order line go 80 to supplier.A's draft and 40 to supplier.B's; one more
// piece is PURCHASE_ALLOCATION_EXCEEDED. Voiding the first draft releases its 80 (TD-AT06), which a
// new draft can take; the released entry stays.
func TestOrderLineSplitsAcrossDraftsUpToItsDemand(t *testing.T) {
	a := Open(Key{OrderID: "order.1", LineID: "L1"}, 1, pcs, kernel.MustDecimal("120"))
	if !a.New() || a.Remaining().String() != "120" {
		t.Fatalf("new root %+v", a)
	}
	for _, c := range []struct {
		draft, q, want, remaining string
		version                   int64
	}{
		{"draft.A", "80", "OK", "40", 1},
		{"draft.B", "40", "OK", "0", 2},
		{"draft.C", "1", "PURCHASE_ALLOCATION_EXCEEDED /args/lines/0/quantity", "0", 2},
	} {
		err := a.Take(c.draft, kernel.MustDecimal(c.q), 1, pcs, kernel.MustDecimal("120"), "/args/lines/0/quantity")
		if got := code(err); got != c.want || a.Remaining().String() != c.remaining || a.RowVersion != c.version {
			t.Fatalf("%s takes %s: %s, remaining %s at %d", c.draft, c.q, got, a.Remaining(), a.RowVersion)
		}
	}
	if r, _ := kernel.AsRejection(a.Take("draft.C", kernel.MustDecimal("1"), 1, pcs, kernel.MustDecimal("120"), "/x")); r.Category != kernel.CategoryConflict {
		t.Fatalf("category %s", r.Category)
	}
	if len(a.Fresh()) != 2 || !a.Consistent() {
		t.Fatalf("entries %+v", a.Entries)
	}
	a.Stored = a.RowVersion // as stored
	q, err := a.Release("draft.A", time.Now())
	if err != nil || q.String() != "80" || a.Remaining().String() != "80" || a.RowVersion != 3 || len(a.Released()) != 1 || !a.Consistent() {
		t.Fatalf("release %s, %v: %+v", q, err, a)
	}
	if _, err = a.Release("draft.A", time.Now()); code(err) != "INVALID_STATE /" {
		t.Fatalf("a released entry is not released again: %s", code(err))
	}
	if err = a.Take("draft.D", kernel.MustDecimal("80"), 1, pcs, kernel.MustDecimal("120"), "/x"); err != nil || a.Remaining().String() != "0" {
		t.Fatalf("the released quantity is allocatable again: %v", err)
	}
	if len(a.Entries) != 3 || a.Entries[0].Effective() || a.Entries[0].Released != 3 {
		t.Fatalf("the released entry stays as history: %+v", a.Entries)
	}
}

// A draft takes a line once; quantities are positive; an older order revision than the root's is
// REFERENCE_STALE and a newer one rebinds the demand (an amended order, S3-15); the unit is fixed.
func TestTakeRules(t *testing.T) {
	a := Open(Key{OrderID: "order.1", LineID: "L1"}, 2, pcs, kernel.MustDecimal("120"))
	if err := a.Take("draft.A", kernel.MustDecimal("100"), 2, pcs, kernel.MustDecimal("120"), "/q"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		draft, q string
		revision int64
		unit     Unit
		demand   string
		want     string
	}{
		{"twice", "draft.A", "1", 2, pcs, "120", "INVALID_STATE /q"},
		{"zero", "draft.B", "0", 2, pcs, "120", "INVALID_KNOWN_VALUE /q"},
		{"older revision", "draft.B", "1", 1, pcs, "120", "REFERENCE_STALE /q"},
		{"other unit", "draft.B", "1", 2, Unit{ID: "DZ", Revision: 1}, "120", "REFERENCE_STALE /q"},
		{"amended below the allocation", "draft.B", "1", 3, pcs, "90", "PURCHASE_ALLOCATION_EXCEEDED /q"},
	} {
		b := *a
		b.Entries = append([]Entry(nil), a.Entries...)
		if got := code(b.Take(c.draft, kernel.MustDecimal(c.q), c.revision, c.unit, kernel.MustDecimal(c.demand), "/q")); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	if err := a.Take("draft.B", kernel.MustDecimal("30"), 3, pcs, kernel.MustDecimal("130"), "/q"); err != nil || a.OrderRevision != 3 ||
		a.Demand.String() != "130" || a.Remaining().String() != "0" {
		t.Fatalf("rebind %+v, %v", a, err)
	}
}

func TestLockOrder(t *testing.T) {
	as := []*Allocation{{Key: Key{OrderID: "o.2", LineID: "L1"}}, {Key: Key{OrderID: "o.1", LineID: "L2"}}, {Key: Key{OrderID: "o.1", LineID: "L1"}}}
	Sort(as)
	if as[0].Key.LineID != "L1" || as[0].Key.OrderID != "o.1" || as[1].Key.LineID != "L2" || as[2].Key.OrderID != "o.2" {
		t.Fatalf("order %+v %+v %+v", as[0].Key, as[1].Key, as[2].Key)
	}
}
