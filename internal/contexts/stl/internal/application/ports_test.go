package application

import "testing"

// S5 D-9 (unit B4, A6 gap 8): credit.reserve reserves TRD's current revision or, for an active
// order, a revision one of its amendments proposes, with that revision's stages and credit
// portion; any other binding is unknown, and only a revision number TRD states is told apart from
// a wrong hash.
func TestOrderRevisionOfABinding(t *testing.T) {
	current := OrderBinding{OrderID: "order.1", OrderRevision: 2, OrderHash: "h2"}
	proposed := OrderBinding{OrderID: "order.1", OrderRevision: 3, OrderHash: "h3"}
	o := Order{Binding: current, Status: OrderActive, Stages: []OrderStage{{StageID: "P1"}}, CreditPortionMinor: "50400",
		Proposed: []ProposedRevision{{AmendmentID: "a.more", Binding: proposed, Stages: []OrderStage{{StageID: "P1"}, {StageID: "P2"}},
			CreditPortionMinor: "61600"}}}
	if s, p, ok := o.revision(current); !ok || p != "50400" || len(s) != 1 {
		t.Fatalf("the current revision: %v %s %v", s, p, ok)
	}
	if s, p, ok := o.revision(proposed); !ok || p != "61600" || len(s) != 2 {
		t.Fatalf("the proposed revision: %v %s %v", s, p, ok)
	}
	if _, _, ok := o.revision(OrderBinding{OrderID: "order.1", OrderRevision: 3, OrderHash: "other"}); ok || !o.numbers(3) || !o.numbers(2) ||
		o.numbers(4) {
		t.Fatal("a wrong hash of a stated revision, or an unstated revision")
	}
	o.Status = OrderDraft
	if _, _, ok := o.revision(proposed); ok || o.numbers(3) {
		t.Fatal("a draft order's proposals count")
	}
}
