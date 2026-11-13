// Package allocation is PUR's DemandAllocation aggregate (tactical model PUR-AGG-DemandAllocation,
// PUR-INV-002, UL-INV-20; WBS S3-16): the ledger of how much of one active order line is allocated
// to purchase drafts. Its root is one order line of one order, (tenant, company, order_id,
// order_line_id); its entries are the drafts that hold a quantity of it. The effective allocation
// (the entries not released: drafts not voided; in S5 commitments as well) never exceeds the
// allocatable demand, the order line's quantity at the order revision the root is bound to. Several
// drafts, of several suppliers, may share one line (80 + 40 of 120, TD-AT05); one more piece is
// PURCHASE_ALLOCATION_EXCEEDED. Quantities an approved overbuy adds to a draft (MOQ, order multiple,
// loss) are not demand and are not allocated here: the draft marks them (PUR-INV-005).
//
// Concurrency is the root row's: a command locks every root it touches FOR UPDATE, in (order_id,
// order_line_id) order, before it decides, and writes the balance with a conditional update, so of
// two drafts competing for the remainder exactly one commits (PUR-INV-002). Releasing a voided
// draft's entry gives its quantity back; the entry stays as history.
package allocation

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of this package (error catalog V0.1, PUR rows).
const (
	CodePurchaseAllocationExceeded = "PURCHASE_ALLOCATION_EXCEEDED" // 409: the effective allocation would exceed the demand
	CodeReferenceStale             = "REFERENCE_STALE"              // 409: the order revision bound is older than the root's
)

// Codes maps this package's codes to their categories.
var Codes = map[string]kernel.Category{
	CodePurchaseAllocationExceeded: kernel.CategoryConflict,
	CodeReferenceStale:             kernel.CategoryConflict,
}

// Key identifies a root: one line of one order.
type Key struct {
	OrderID string
	LineID  string
}

// Compare orders keys as the lock order takes them: order ID, then line ID.
func Compare(a, b Key) int {
	return cmp.Or(cmp.Compare(a.OrderID, b.OrderID), cmp.Compare(a.LineID, b.LineID))
}

// Unit is the order line's unit, a POL unit revision; the root's quantities are in it.
type Unit struct {
	ID       string
	Revision int64
}

// Entry is one draft's share of the line: Quantity, taken at root version Taken for the draft's
// commitment revision Revision (1: the draft as prepared; a later one: what a change or the
// cancellation of its purchase commitment made it allocate, S5-19); Released is the root version that
// released it (0: effective). A draft holds at most one effective entry of a line.
type Entry struct {
	DraftID  string
	Revision int64
	Quantity kernel.Decimal
	Taken    int64
	Released int64
	At       time.Time // when it was released
	fresh    bool
}

// Effective reports whether the entry still counts.
func (e Entry) Effective() bool { return e.Released == 0 }

// Allocation is the DemandAllocation root of one order line.
type Allocation struct {
	Key           Key
	OrderRevision int64          // the order revision Demand is the line's quantity in
	Unit          Unit           // fixed when the root opens
	Demand        kernel.Decimal // the allocatable demand
	Allocated     kernel.Decimal // the sum of the effective entries
	RowVersion    int64          // 0 for a root not stored yet
	Stored        int64          // the row version as loaded (0: new)
	Entries       []Entry
}

// Open returns a new root for line key of an order at revision, whose quantity is demand in unit.
func Open(key Key, revision int64, unit Unit, demand kernel.Decimal) *Allocation {
	return &Allocation{Key: key, OrderRevision: revision, Unit: unit, Demand: demand}
}

// New reports whether the root is not stored yet.
func (a *Allocation) New() bool { return a.Stored == 0 }

// Remaining is the demand not allocated.
func (a *Allocation) Remaining() kernel.Decimal { return a.Demand.Sub(a.Allocated) }

func reject(code, path, reason string) *kernel.Rejection {
	return kernel.RejectAs(code, Codes[code], path, reason)
}

// Take allocates quantity q of the line to draft draftID, judged against the order line as bound
// now: the order at revision with the line's quantity demand in unit. A revision older than the
// root's is REFERENCE_STALE; a newer one rebinds the root's demand to it (an amended order, S3-15;
// OrderAmended is reviewed in S5). A unit other than the root's is REFERENCE_STALE too. The
// effective allocation plus q must not exceed the demand (PURCHASE_ALLOCATION_EXCEEDED at path). A
// draft takes a line once, at its commitment revision 1.
func (a *Allocation) Take(draftID string, q kernel.Decimal, revision int64, unit Unit, demand kernel.Decimal, path string) error {
	return a.TakeAt(draftID, 1, q, revision, unit, demand, path)
}

// TakeAt is Take for the draft's commitment revision commitmentRevision: a draft that holds an
// effective entry of the line, or an entry of that revision, takes nothing (INVALID_STATE).
func (a *Allocation) TakeAt(draftID string, commitmentRevision int64, q kernel.Decimal, revision int64, unit Unit, demand kernel.Decimal, path string) error {
	switch {
	case revision < a.OrderRevision:
		return reject(CodeReferenceStale, path, "The order line was allocated at a later order revision")
	case unit != a.Unit:
		return reject(CodeReferenceStale, path, "The order line's unit is not the one its allocations are in")
	case q.Sign() <= 0:
		return kernel.Reject(kernel.CodeInvalidKnownValue, path, "An allocation is positive")
	case slices.ContainsFunc(a.Entries, func(e Entry) bool {
		return e.DraftID == draftID && (e.Effective() || e.Revision == commitmentRevision)
	}):
		return kernel.Reject(kernel.CodeInvalidState, path, "The draft already holds an allocation of this line")
	}
	if revision > a.OrderRevision {
		a.OrderRevision, a.Demand = revision, demand
	}
	if next := a.Allocated.Add(q); next.Cmp(a.Demand) > 0 {
		return reject(CodePurchaseAllocationExceeded, path,
			fmt.Sprintf("Allocating %s exceeds the remaining %s of the order line's %s", q, a.Remaining(), a.Demand))
	}
	a.RowVersion++
	a.Allocated = a.Allocated.Add(q)
	a.Entries = append(a.Entries, Entry{DraftID: draftID, Revision: commitmentRevision, Quantity: q, Taken: a.RowVersion, fresh: true})
	return nil
}

// Move moves draft draftID's share of the line to its commitment revision commitmentRevision under
// one version of the root (S5-19, PUR-INV-011): the effective entry is released at instant at and,
// when q is positive, an entry of q is taken for that revision, judged as TakeAt judges it against
// the order line as bound now (revision, unit, demand). A change applied or a cancellation moves it.
func (a *Allocation) Move(draftID string, commitmentRevision int64, q kernel.Decimal, revision int64, unit Unit, demand kernel.Decimal, at time.Time,
	path string) error {
	next := a.RowVersion + 1
	for i, e := range a.Entries {
		if e.DraftID == draftID && e.Effective() {
			a.Entries[i].Released, a.Entries[i].At = next, kernel.Instant(at)
			a.Allocated = a.Allocated.Sub(e.Quantity)
		}
	}
	if q.Sign() > 0 {
		if err := a.TakeAt(draftID, commitmentRevision, q, revision, unit, demand, path); err != nil {
			return err
		}
	}
	a.RowVersion = next
	return nil
}

// Holds reports whether draft draftID holds an effective entry of the line.
func (a *Allocation) Holds(draftID string) bool {
	return slices.ContainsFunc(a.Entries, func(e Entry) bool { return e.DraftID == draftID && e.Effective() })
}

// Release gives back the effective entry of draftID at instant at and returns its quantity. A
// draft without an effective entry of the line releases nothing (INVALID_STATE).
func (a *Allocation) Release(draftID string, at time.Time) (kernel.Decimal, error) {
	i := slices.IndexFunc(a.Entries, func(e Entry) bool { return e.DraftID == draftID && e.Effective() })
	if i < 0 {
		return kernel.Decimal{}, kernel.Reject(kernel.CodeInvalidState, "/", "The draft holds no allocation of the order line")
	}
	a.RowVersion++
	e := &a.Entries[i]
	e.Released, e.At = a.RowVersion, kernel.Instant(at)
	a.Allocated = a.Allocated.Sub(e.Quantity)
	return e.Quantity, nil
}

// Without returns a copy of the root as it would be had draft draftID never taken an entry: what a
// purchase commitment decision prepares the draft's request again against (A47 #3), and what a
// commitment change prepares its proposed revision against (PUR-INV-011). The copy is judged, never
// stored.
func (a *Allocation) Without(draftID string) *Allocation {
	out := *a
	out.Entries = nil
	for _, e := range a.Entries {
		if e.DraftID == draftID {
			if e.Effective() {
				out.Allocated = out.Allocated.Sub(e.Quantity)
			}
			continue
		}
		e.fresh = false
		out.Entries = append(out.Entries, e)
	}
	return &out
}

// Fresh returns the entries taken since the root was loaded.
func (a *Allocation) Fresh() []Entry {
	var out []Entry
	for _, e := range a.Entries {
		if e.fresh {
			out = append(out, e)
		}
	}
	return out
}

// Released returns the entries released since the root was loaded.
func (a *Allocation) Released() []Entry {
	var out []Entry
	for _, e := range a.Entries {
		if e.Released > a.Stored && e.Released != 0 {
			out = append(out, e)
		}
	}
	return out
}

// Consistent reports whether the stored balance is the sum of the effective entries and within the
// demand, as the database also enforces.
func (a *Allocation) Consistent() bool {
	sum := kernel.DecimalFromInt(0)
	for _, e := range a.Entries {
		if e.Effective() {
			sum = sum.Add(e.Quantity)
		}
	}
	return sum.Equal(a.Allocated) && a.Allocated.Sign() >= 0 && a.Allocated.Cmp(a.Demand) <= 0
}

// Sort orders roots by key, the lock order.
func Sort(as []*Allocation) {
	slices.SortFunc(as, func(a, b *Allocation) int { return Compare(a.Key, b.Key) })
}
