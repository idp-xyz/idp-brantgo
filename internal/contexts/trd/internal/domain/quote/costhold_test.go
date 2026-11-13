package quote

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

var consumer = kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.trd.cost_source_holds"}

// WF-03 step 4, PL-07, SC-04 activation first, S4 decision D-9, S3-D67's hold without release: a
// denial of a source the active revision's cost snapshot rests on holds that revision once — cause
// cost_source_denied, code COST_SOURCE_DENIED, the root version moved by one — while no contract
// formed on it; its forward use stops (Usable), the revision stays active. Nothing is held for
// another snapshot, a revision that is not active, a root under contract (a live acceptance or the
// conversion slot taken: ADR-BR-013, TRD-INV-014) or a second delivery of the same attestation;
// only the consumer's system actor registers a hold.
func TestCostSourceDenialHoldsTheUncontractedActiveRevision(t *testing.T) {
	denial := CostDenial{AttestationID: "denial.1", SourceID: "src.1", EventID: "evt.1"}
	active := func(t *testing.T) (*Quote, *Revision) {
		t.Helper()
		q := frozen(t, "8", "0.1")
		approvedFor(t, q, 1, "review.1", "approval.1", TargetActivate, 0)
		rev, _, err := formalize(q, 1, "approval.1", TargetActivate, 0, documentOf(q, 1, "QT-1", "one"))
		if err != nil {
			t.Fatal(err)
		}
		return q, rev
	}

	q, rev := active(t)
	if s, r := q.CostSnapshotOf(); s != "snapshot.1" || r != rev {
		t.Fatalf("the active revision's snapshot: %s %v", s, r)
	}
	if h, err := q.HoldCostSource(denial, []string{"snapshot.1"}, human("reviewer"), 3, at); code(err) != "PERMISSION_DENIED at /" || h != nil {
		t.Fatalf("a human registering a cost hold: %+v %v", h, err)
	}
	if h, err := q.HoldCostSource(denial, []string{"snapshot.other"}, consumer, 3, at); err != nil || h != nil || len(q.Holds) != 0 {
		t.Fatalf("another snapshot: %+v %v", h, err)
	}
	before := q.RootVersion
	h, err := q.HoldCostSource(denial, []string{"snapshot.other", "snapshot.1"}, consumer, 3, at)
	if err != nil || h == nil || h.ID != "cost_source_denied:denial.1" || h.Cause != HoldCostSourceDenied || h.Code != CodeCostSourceDenied ||
		h.Revision != 1 || h.ApprovalID != "" || h.Denial == nil || *h.Denial != (CostDenial{AttestationID: "denial.1", SourceID: "src.1",
		EventID: "evt.1", SnapshotID: "snapshot.1"}) || h.Registered.By != consumer || h.Registered.Epoch != 3 || h.RootVersion != before+1 ||
		!h.Fresh || q.RootVersion != before+1 || q.Hold(1) != h {
		t.Fatalf("hold %+v %v", h, err)
	}
	if rev.Status != Active || q.Active() != rev {
		t.Fatal("the held revision stays active")
	}
	if err = q.Usable(1, "", "/x"); code(err) != "COST_SOURCE_DENIED at /x" {
		t.Fatalf("a held revision serves no new business: %s", code(err))
	}
	if again, err := q.HoldCostSource(denial, []string{"snapshot.1"}, consumer, 3, at); err != nil || again != nil || len(q.Holds) != 1 ||
		q.RootVersion != before+1 {
		t.Fatalf("the same attestation again: %+v %v", again, err)
	}
	other := CostDenial{AttestationID: "denial.2", SourceID: "src.2", EventID: "evt.2"}
	if h2, err := q.HoldCostSource(other, []string{"snapshot.1"}, consumer, 3, at); err != nil || h2 == nil || len(q.Holds) != 2 ||
		q.Hold(1) != h {
		t.Fatalf("another denial of the snapshot holds again, keeping the first hold first: %+v %v", h2, err)
	}

	// Under contract: a live acceptance, or the conversion slot taken.
	q, _ = active(t)
	q.Contracts = map[int64][]string{1: {"acceptance.1"}}
	if h, err := q.HoldCostSource(denial, []string{"snapshot.1"}, consumer, 3, at); err != nil || h != nil || len(q.Holds) != 0 {
		t.Fatalf("a live acceptance: %+v %v", h, err)
	}
	q, rev = active(t)
	if _, err = q.TakeSlot(1, rev.Seal.BusinessHash, "order.1", human("author"), 3, at); err != nil {
		t.Fatal(err)
	}
	if h, err := q.HoldCostSource(denial, []string{"snapshot.1"}, consumer, 3, at); err != nil || h != nil || len(q.Holds) != 0 ||
		!q.Contracted(1) {
		t.Fatalf("the conversion slot taken: %+v %v", h, err)
	}

	// No active revision: nothing to hold (a pending revision's activation re-checks the snapshot).
	q = frozen(t, "8", "0.1")
	if h, err := q.HoldCostSource(denial, []string{"snapshot.1"}, consumer, 3, at); err != nil || h != nil {
		t.Fatalf("no active revision: %+v %v", h, err)
	}
}
