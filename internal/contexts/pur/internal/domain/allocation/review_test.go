package allocation

import (
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The allocation review (SM-PUR-ALLOCATION-REVIEW; WF-06 step 4; PL-09; S5 D-21; BT10, SIM-02 step 7,
// ES-09 step 5) on PUR's facts of one order: line L1 of 120 PCS allocated 100 to draft.1, whose
// commitment c.1 is issued with 40 verified delivered, and 15 to draft.2, prepared and never
// committed; line L2 allocated 30 to draft.3, prepared. Python V0.50 has no review.

var (
	sku1   = Spec{SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS"}
	sku2   = Spec{SkuID: "sku.2", SpecRevision: 1, UnitID: "PCS"}
	opened = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
)

func dec(s string) kernel.Decimal { return kernel.MustDecimal(s) }

func holdings(t *testing.T) Holdings {
	t.Helper()
	l1 := Open(Key{OrderID: "order.1", LineID: "L1"}, 1, pcs, dec("120"))
	l2 := Open(Key{OrderID: "order.1", LineID: "L2"}, 1, pcs, dec("50"))
	for _, x := range []struct {
		a     *Allocation
		draft string
		q     string
	}{{l1, "draft.1", "100"}, {l1, "draft.2", "15"}, {l2, "draft.3", "30"}} {
		if err := x.a.Take(x.draft, dec(x.q), 1, pcs, x.a.Demand, "/x"); err != nil {
			t.Fatal(err)
		}
	}
	return Holdings{Allocations: []*Allocation{l2, l1},
		Drafts: []HeldDraft{
			{ID: "draft.1", Hash: strings.Repeat("1", 64), Status: draftPrepared, SupplierID: "supplier.1", Committed: true, CommitmentID: "c.1",
				CommitmentStatus: commitmentIssued, Lines: []DraftLine{{LineID: "L1", Quantity: dec("100"), Overbuy: dec("0"), Spec: sku1}}},
			{ID: "draft.2", Hash: strings.Repeat("2", 64), Status: draftPrepared, SupplierID: "supplier.2", CommitmentID: "c.2", CommitmentStatus: "requested",
				Lines: []DraftLine{{LineID: "L1", Quantity: dec("15"), Overbuy: dec("5"), Spec: sku1}}},
			{ID: "draft.3", Hash: strings.Repeat("3", 64), Status: draftPrepared, SupplierID: "supplier.1",
				Lines: []DraftLine{{LineID: "L2", Quantity: dec("30"), Overbuy: dec("0"), Spec: sku2}}},
		},
		Commitments: []HeldCommitment{{ID: "c.1", DraftID: "draft.1", Status: commitmentIssued, Revision: 1, SupplierID: "supplier.1", PONumber: "PO-1",
			Lines: []CommitmentLine{{LineID: "L1", Allocated: dec("100"), Overbuy: dec("0"), Purchased: dec("100"), Verified: dec("40")}}}},
	}
}

func amended(version int64, lines ...Target) Change {
	c := Change{OrderID: "order.1", Revision: 2, Hash: strings.Repeat("a", 64), Version: version, Cause: CauseOrderAmended, EventID: "evt.2",
		Targets: map[string]Target{}}
	for _, l := range lines {
		c.Targets[l.LineID] = l
	}
	return c
}

func target(line, q string, s Spec) Target { return Target{LineID: line, Quantity: dec(q), Spec: &s} }

func kinds(items []Item) string {
	var out []string
	for _, it := range items {
		f := it.Facts
		out = append(out, strings.Join([]string{f.Kind, f.OrderLineID + f.DraftID + "/" + f.CommitmentID}, ":"))
	}
	return strings.Join(out, " ")
}

// BT10 / SIM-02 step 7: L1 reduced from 120 to 80 lists the allocation of L1 (115 > 80, excess 35,
// both holders, c.1 at revision 1), the voidable draft.2 (its requested commitment named) and the
// issued c.1 with what it buys and what is verified; L2, untouched, and draft.3 are not listed;
// draft.1 is committed and appears as its commitment only.
func TestAssessAReduction(t *testing.T) {
	items := Assess(amended(5, target("L1", "80", sku1), target("L2", "50", sku2)), holdings(t))
	if got := kinds(items); got != "allocation:L1/ draft:draft.2/c.2 commitment:draft.1/c.1" {
		t.Fatalf("items %s", got)
	}
	a := items[0].Facts
	if l := a.Lines[0]; items[0].No != 1 || l.Impact != ImpactReduced || l.TargetQuantity != "80" || l.AllocatedQuantity != "115" ||
		l.ExcessQuantity != "35" || l.DemandQuantity != "120" || l.DemandRevision != 1 || l.UnitID != "PCS" || len(a.Holders) != 2 ||
		a.Holders[0] != (Holder{DraftID: "draft.1", Quantity: "100", CommitmentID: "c.1", CommitmentStatus: commitmentIssued, CommitmentRevision: 1}) ||
		a.Holders[1] != (Holder{DraftID: "draft.2", Quantity: "15"}) {
		t.Fatalf("allocation item %+v", items[0])
	}
	if d := items[1].Facts; d.Status != "prepared/requested" || d.DraftHash != strings.Repeat("2", 64) || len(d.Lines) != 1 ||
		d.Lines[0].PurchaseQuantity != "20" || d.Lines[0].OverbuyQuantity != "5" {
		t.Fatalf("draft item %+v", d)
	}
	if c := items[2].Facts; c.CommitmentRevision != 1 || c.PONumber != "PO-1" || c.Status != commitmentIssued || len(c.Lines) != 1 ||
		c.Lines[0].PurchaseQuantity != "100" || c.Lines[0].VerifiedQuantity != "40" || c.Lines[0].TargetQuantity != "80" {
		t.Fatalf("commitment item %+v", c)
	}
	f, err := ParseFacts(items[2].Facts.FactsJSON())
	if err != nil || f.CommitmentID != "c.1" || f.Lines[0].VerifiedQuantity != "40" {
		t.Fatalf("facts round trip %+v %v", f, err)
	}
	// An increase, or a reduction still above the allocation, touches nothing.
	if items := Assess(amended(5, target("L1", "115", sku1), target("L2", "30", sku2)), holdings(t)); len(items) != 0 {
		t.Fatalf("nothing touched: %s", kinds(items))
	}
}

// A line the revision no longer has, and a line whose SKU specification changed (BT10 改色), need
// nothing of what was bought for them: target 0.
func TestAssessARemovedAndAChangedLine(t *testing.T) {
	items := Assess(amended(5, target("L1", "120", Spec{SkuID: "sku.1", SpecRevision: 2, UnitID: "PCS"})), holdings(t))
	if got := kinds(items); got != "allocation:L1/ allocation:L2/ draft:draft.2/c.2 draft:draft.3/ commitment:draft.1/c.1" {
		t.Fatalf("items %s", got)
	}
	if l := items[0].Facts.Lines[0]; l.Impact != ImpactSpecificationChanged || l.TargetQuantity != "0" || l.ExcessQuantity != "115" {
		t.Fatalf("changed line %+v", l)
	}
	if l := items[1].Facts.Lines[0]; l.Impact != ImpactLineRemoved || l.TargetQuantity != "0" {
		t.Fatalf("removed line %+v", l)
	}
}

// OrderCancelled needs nothing of any line; OrderRemainderClosed needs what was shipped (ES-09 step 6).
func TestAssessTheOrdersEnd(t *testing.T) {
	c := amended(9)
	c.Cause = CauseOrderCancelled
	items := Assess(c, holdings(t))
	if got := kinds(items); got != "allocation:L1/ allocation:L2/ draft:draft.2/c.2 draft:draft.3/ commitment:draft.1/c.1" ||
		items[0].Facts.Lines[0].Impact != ImpactOrderCancelled {
		t.Fatalf("cancelled %s", got)
	}
	c.Cause = CauseOrderRemainderClosed
	c.Targets = map[string]Target{"L1": {LineID: "L1", Quantity: dec("116")}, "L2": {LineID: "L2", Quantity: dec("30")}}
	items = Assess(c, holdings(t))
	if got := kinds(items); got != "" {
		t.Fatalf("remainder closed above the allocations: %s", got)
	}
	c.Targets["L1"] = Target{LineID: "L1", Quantity: dec("100")}
	items = Assess(c, holdings(t))
	if got := kinds(items); got != "allocation:L1/ draft:draft.2/c.2 commitment:draft.1/c.1" ||
		items[0].Facts.Lines[0].Impact != ImpactRemainderClosed || items[0].Facts.Lines[0].ExcessQuantity != "15" {
		t.Fatalf("remainder closed %s %+v", got, items)
	}
}

// WF-06 idempotency: one review per (order, revision, cause); a review of a later order version makes
// an older change old news; a later change supersedes open reviews (also without items); nothing held
// or nothing touched opens nothing.
func TestPlanOpensOnceAndSupersedes(t *testing.T) {
	h := holdings(t)
	c := amended(5, target("L1", "80", sku1))
	items := Assess(c, h)
	r2 := OpenReview("alr.2", c, items, Act{By: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.pur.allocation_review"}, Epoch: 1, At: opened})
	if e, _ := Plan(c, []*Review{r2}, h, items); e != EffectAlreadyOpen {
		t.Fatalf("again: %s", e)
	}
	r2.Status = ReviewSettled
	if e, _ := Plan(c, []*Review{r2}, h, items); e != EffectAlreadyOpen {
		t.Fatalf("settled is never reopened: %s", e)
	}
	r2.Status = ReviewOpen
	older := amended(4, target("L1", "90", sku1))
	older.Revision = 1
	if e, _ := Plan(older, []*Review{r2}, h, Assess(older, h)); e != EffectLater {
		t.Fatalf("older news: %s", e)
	}
	cancel := amended(6)
	cancel.Cause = CauseOrderCancelled
	e, sup := Plan(cancel, []*Review{r2}, h, Assess(cancel, h))
	if e != EffectOpened || len(sup) != 1 || sup[0] != r2 {
		t.Fatalf("cancel after the amendment: %s %v", e, sup)
	}
	back := amended(7, target("L1", "120", sku1), target("L2", "50", sku2))
	back.Revision = 3
	if e, sup := Plan(back, []*Review{r2}, h, Assess(back, h)); e != EffectOpened || len(sup) != 1 {
		t.Fatalf("a later revision supersedes the open review without items: %s %v", e, sup)
	}
	r2.Status = ReviewSettled
	if e, _ := Plan(back, []*Review{r2}, h, Assess(back, h)); e != EffectNothingTouched {
		t.Fatalf("nothing touched: %s", e)
	}
	if e, _ := Plan(back, nil, Holdings{}, nil); e != EffectNothingHeld {
		t.Fatalf("nothing held: %s", e)
	}
	r3 := OpenReview("alr.3", back, nil, r2.Opened)
	r2.Status = ReviewOpen
	r2.Stored = r2.RowVersion
	r2.Supersede(r3.ID, opened.Add(time.Hour))
	if r2.Status != ReviewSuperseded || r2.RowVersion != 2 || r2.SupersededBy != "alr.3" || r3.Status != ReviewOpen || r3.Role != ResponsibleRole {
		t.Fatalf("superseded %+v %+v", r2, r3)
	}
}

func review(t *testing.T) *Review {
	t.Helper()
	c := amended(5, target("L1", "80", sku1), target("L2", "50", sku2))
	r := OpenReview("alr.1", c, Assess(c, holdings(t)), Act{By: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox"}, Epoch: 1, At: opened})
	r.Stored = r.RowVersion
	return r
}

func nowShowing() Now {
	return Now{
		Drafts:      map[string]DraftNow{"draft.2": {Status: draftVoided, VoidedAt: opened.Add(time.Hour)}, "draft.3": {Status: draftPrepared}},
		Commitments: map[string]CommitmentNow{"c.1": {Status: commitmentIssued, Revision: 2}},
		Changes: map[string]ChangeNow{"change.1": {CommitmentID: "c.1", Status: changeApplied, BaseRevision: 1, AppliedRevision: 2,
			AppliedAt: opened.Add(2 * time.Hour)}, "change.draft": {CommitmentID: "c.1", Status: "approved", BaseRevision: 1}},
		Allocated: map[string]kernel.Decimal{"L1": dec("80"), "L2": dec("30")},
	}
}

func settle(no int, kind, ref, why string) Settlement {
	s := Settlement{ItemNo: no, Kind: kind, Reason: why}
	switch kind {
	case DispositionDraftVoided:
		s.DraftID = ref
	case DispositionCommitmentChanged:
		s.ChangeID = ref
	case DispositionCommitmentCancelled:
		s.CommitmentID = ref
	}
	return s
}

// SM-PUR-ALLOCATION-REVIEW open → settled, BT10 "处置可追溯": after draft.2 was voided and c.1 changed to
// 80 (change.1, revision 2), every item is settled by reference to what happened; the change's
// revision and the times are kept. A human act settles it.
func TestSettleByTheDispositionsMade(t *testing.T) {
	r := review(t)
	human := Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "purchaser.1"}, Epoch: 3, At: opened.Add(3 * time.Hour)}
	ss := []Settlement{settle(3, DispositionCommitmentChanged, "change.1", "供应商确认减至 80"), settle(1, DispositionCommitmentChanged, "change.1", "分配已降至 80"),
		settle(2, DispositionDraftVoided, "draft.2", "多余草稿作废")}
	if err := r.Settle(1, ss, nowShowing(), human); err != nil {
		t.Fatal(err)
	}
	if r.Status != ReviewSettled || r.RowVersion != 2 || r.Settled.By.ID != "purchaser.1" || r.Verify() != nil {
		t.Fatalf("settled %+v", r)
	}
	if d := r.Items[2].Done; d.ChangeID != "change.1" || d.CommitmentID != "c.1" || d.Revision != 2 || !d.At.Equal(opened.Add(2*time.Hour)) {
		t.Fatalf("the change's trace %+v", d)
	}
	if d := r.Items[1].Done; d.DraftID != "draft.2" || !d.At.Equal(opened.Add(time.Hour)) || d.Reason != "多余草稿作废" {
		t.Fatalf("the void's trace %+v", d)
	}
	if err := r.Settle(2, ss, nowShowing(), human); code(err) != kernel.CodeInvalidState+" /args/review_id" {
		t.Fatalf("settled twice: %v", err)
	}
}

// "An item whose disposition has not happened cannot be settled as done": each refusal, in order.
func TestSettleRefusesWhatDidNotHappen(t *testing.T) {
	human := Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "purchaser.1"}, Epoch: 3, At: opened.Add(time.Hour)}
	none := func(no int) Settlement { return settle(no, DispositionNoAction, "", "合成：无需处理") }
	cases := []struct {
		name     string
		expected int64
		ss       []Settlement
		now      func(*Now)
		want     string
	}{
		{"another version", 2, []Settlement{none(1), none(2), none(3)}, nil, kernel.CodeVersionConflict + " /args/expected_version"},
		{"no such item", 1, []Settlement{none(1), none(2), none(3), none(4)}, nil, kernel.CodeReferenceInvalid + " /args/items/3/item_no"},
		{"an item left out", 1, []Settlement{none(1), none(3)}, nil, kernel.CodeDraftNotReady + " /args/items"},
		{"a draft voided for a commitment item", 1, []Settlement{none(1), none(2), settle(3, DispositionDraftVoided, "draft.1", "x")}, nil,
			kernel.CodeInvalidKnownValue + " /args/items/2/disposition"},
		{"a commitment changed for a draft item", 1, []Settlement{none(1), settle(2, DispositionCommitmentChanged, "change.1", "x"), none(3)}, nil,
			kernel.CodeInvalidKnownValue + " /args/items/1/disposition"},
		{"another draft", 1, []Settlement{none(1), settle(2, DispositionDraftVoided, "draft.3", "x"), none(3)}, nil,
			kernel.CodeReferenceInvalid + " /args/items/1/draft_id"},
		{"a draft not voided", 1, []Settlement{none(1), settle(2, DispositionDraftVoided, "draft.2", "x"), none(3)},
			func(n *Now) { n.Drafts["draft.2"] = DraftNow{Status: draftPrepared} }, kernel.CodeInvalidState + " /args/items/1/draft_id"},
		{"a change not applied", 1, []Settlement{none(1), none(2), settle(3, DispositionCommitmentChanged, "change.draft", "x")}, nil,
			kernel.CodeInvalidState + " /args/items/2/change_id"},
		{"a change of another commitment", 1, []Settlement{none(1), none(2), settle(3, DispositionCommitmentChanged, "change.other", "x")},
			func(n *Now) {
				n.Changes["change.other"] = ChangeNow{CommitmentID: "c.9", Status: changeApplied, BaseRevision: 1, AppliedRevision: 2}
			},
			kernel.CodeReferenceInvalid + " /args/items/2/change_id"},
		{"a change applied before the review saw the revision", 1, []Settlement{none(1), none(2), settle(3, DispositionCommitmentChanged, "change.old", "x")},
			func(n *Now) {
				n.Changes["change.old"] = ChangeNow{CommitmentID: "c.1", Status: changeApplied, BaseRevision: 0, AppliedRevision: 1}
			},
			kernel.CodeInvalidState + " /args/items/2/change_id"},
		{"a commitment not cancelled", 1, []Settlement{none(1), none(2), settle(3, DispositionCommitmentCancelled, "c.1", "x")}, nil,
			kernel.CodeInvalidState + " /args/items/2/commitment_id"},
		{"another commitment", 1, []Settlement{none(1), none(2), settle(3, DispositionCommitmentCancelled, "c.9", "x")}, nil,
			kernel.CodeReferenceInvalid + " /args/items/2/commitment_id"},
		{"the line still allocated above its target", 1, []Settlement{settle(1, DispositionDraftVoided, "draft.2", "x"), none(2), none(3)},
			func(n *Now) { n.Allocated["L1"] = dec("100") }, kernel.CodeInvalidState + " /args/items/0/disposition"},
	}
	for _, c := range cases {
		r := review(t)
		now := nowShowing()
		if c.now != nil {
			c.now(&now)
		}
		if err := r.Settle(c.expected, c.ss, now, human); code(err) != c.want || r.Status != ReviewOpen {
			t.Errorf("%s: %v (%s)", c.name, err, r.Status)
		}
	}
	// A superseded review is not settled: the later order version's is.
	r := review(t)
	r.Supersede("alr.9", opened)
	if err := r.Settle(2, []Settlement{none(1), none(2), none(3)}, nowShowing(), human); code(err) != CodeReferenceStale+" /args/review_id" {
		t.Fatalf("superseded: %v", err)
	}
	// no_action settles anything, with its reason; a cancelled commitment settles its item.
	r = review(t)
	now := nowShowing()
	now.Commitments["c.1"] = CommitmentNow{Status: commitmentCancelled, Revision: 2, CancelledAt: opened}
	if err := r.Settle(1, []Settlement{none(1), none(2), settle(3, DispositionCommitmentCancelled, "c.1", "供应商证据")}, now, human); err != nil ||
		r.Items[2].Done.Revision != 2 || r.Items[0].Done.Kind != DispositionNoAction {
		t.Fatalf("no_action and cancelled: %v %+v", err, r.Items)
	}
}
