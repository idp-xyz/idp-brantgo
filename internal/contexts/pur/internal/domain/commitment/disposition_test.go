package commitment

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5R-D05 (simulated sign-off E0): SM-PUR-COMMITMENT-DISPOSITION and PUR-INV-013 on the synthetic
// commitment of commitment_test.go. Python V0.50 has no dispositions: the vectors are the design's.

var pcs = Unit{ID: "PCS", Revision: 1}

func disposeReq(id, kind, quantity string, evidence ...Evidence) DispositionRequest {
	return DispositionRequest{ID: id, CommitmentID: "commitment.1", OrderLineID: "L1", Kind: kind, Quantity: q(quantity),
		Reason: "合成：订单取消，工厂同意退货", Evidence: evidence}
}

// PUR-INV-013: a disposition is requested for a line of an issued commitment (issued, confirmed or
// cancelled since), within what is verified delivered of the line less its dispositions approved or
// still requested; a commitment never issued, a line it does not buy and a quantity beyond are refused,
// in that order. The disposition is requested at version 1 in the line's unit and never touches the
// commitment.
func TestDispositionIsRequestedWithinTheVerifiedDeliveries(t *testing.T) {
	c := issued(t, fixture())
	verified := map[string]kernel.Decimal{"L1": q("60")}
	lines := []string{"L1"}
	at := now.Add(3 * time.Hour)
	if _, err := Dispose(disposeReq("disp.1", WriteOff, "1"), requested(t, fixture()), lines, pcs, verified, nil, buyer, 7, at); code(err) != "INVALID_STATE /args/commitment_id" {
		t.Fatal(code(err))
	}
	other := disposeReq("disp.1", WriteOff, "1")
	other.OrderLineID = "L9"
	if _, err := Dispose(other, c, lines, pcs, verified, nil, buyer, 7, at); code(err) != "REFERENCE_INVALID /args/order_line_id" {
		t.Fatal(code(err))
	}
	if _, err := Dispose(disposeReq("disp.1", WriteOff, "61"), c, lines, pcs, verified, nil, buyer, 7, at); code(err) != "INVALID_KNOWN_VALUE /args/quantity" {
		t.Fatal(code(err))
	}
	if _, err := Dispose(disposeReq("disp.1", WriteOff, "1"), c, lines, pcs, nil, nil, buyer, 7, at); code(err) != "INVALID_KNOWN_VALUE /args/quantity" {
		t.Fatal("nothing verified delivered is disposed of: " + code(err))
	}
	version, status := c.RowVersion, c.Status
	x, err := Dispose(disposeReq("disp.1", ReturnToSupplier, "40", Evidence{SourceID: "rma", Locator: "p1"}), c, lines, pcs, verified, nil, buyer, 7, at)
	if err != nil {
		t.Fatal(err)
	}
	if x.Status != DispositionRequested || x.RowVersion != 1 || x.CommitmentID != "commitment.1" || x.DraftID != "draft.1" || x.OrderID != "order.1" ||
		x.Unit != pcs || !x.Quantity.Equal(q("40")) || x.Requested.By != buyer || x.Requested.Epoch != 7 || x.Decision != nil {
		t.Fatalf("requested %+v", x)
	}
	if c.RowVersion != version || c.Status != status {
		t.Fatal("a disposition never moves the commitment")
	}
	// 40 requested: 21 more is beyond the 60 verified; 20 fits; a rejected one holds nothing.
	others := []*Disposition{x}
	if _, err = Dispose(disposeReq("disp.2", WriteOff, "21"), c, lines, pcs, verified, others, buyer, 7, at); code(err) != "INVALID_KNOWN_VALUE /args/quantity" {
		t.Fatal(code(err))
	}
	x.Reject(Act{By: reviewer, At: at, Reason: "no"})
	if _, err = Dispose(disposeReq("disp.2", WriteOff, "60"), c, lines, pcs, verified, others, buyer, 7, at); err != nil {
		t.Fatalf("a rejected disposition holds nothing: %v", err)
	}
	cancelled := issued(t, fixture())
	if _, err = cancelled.Cancel(3, []LineQuantity{lineQ("100", "20")}, verified, nil, Act{By: buyer, At: at, Reason: "r"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = Dispose(disposeReq("disp.3", HoldForReuse, "60"), cancelled, lines, pcs, verified, nil, buyer, 7, at); err != nil {
		t.Fatalf("the goods of a cancelled commitment are disposed of: %v", err)
	}
}

// A taken disposition ID answers the same request by the same requester; anything else is
// VERSION_CONFLICT.
func TestDispositionAnswer(t *testing.T) {
	c := issued(t, fixture())
	r := disposeReq("disp.1", WriteOff, "10")
	x, err := Dispose(r, c, []string{"L1"}, pcs, map[string]kernel.Decimal{"L1": q("60")}, nil, buyer, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = x.Answer(r, buyer); err != nil {
		t.Fatal(err)
	}
	more := r
	more.Quantity = q("11")
	for _, got := range []error{x.Answer(r, author), x.Answer(more, buyer)} {
		if code(got) != "VERSION_CONFLICT /args/disposition_id" {
			t.Fatal(code(got))
		}
	}
}

// SM-PUR-COMMITMENT-DISPOSITION requested → approved | rejected: decided once, at its version, by a
// human other than the requester (role templates' independence of purchase.commitment.disposition.decide
// from 处置申请人: INDEPENDENT_REVIEW_REQUIRED); an approval keeps the line's approved dispositions within
// what is verified now (a correction since the request: INVALID_KNOWN_VALUE at /disposition/quantity) and
// moves the version to 2.
func TestDispositionIsDecidedIndependentlyOnce(t *testing.T) {
	c := issued(t, fixture())
	verified := map[string]kernel.Decimal{"L1": q("60")}
	x, err := Dispose(disposeReq("disp.1", WriteOff, "40"), c, []string{"L1"}, pcs, verified, nil, buyer, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := code(x.Decidable(2, reviewer)); got != "VERSION_CONFLICT /args/expected_version" {
		t.Fatal(got)
	}
	if got := code(x.Decidable(1, buyer)); got != "INDEPENDENT_REVIEW_REQUIRED /args/disposition_id" {
		t.Fatal(got)
	}
	if err = x.Decidable(1, reviewer); err != nil {
		t.Fatal(err)
	}
	approvedOther := &Disposition{ID: "disp.0", OrderLineID: "L1", Status: DispositionApproved, Quantity: q("30")}
	act := Act{By: reviewer, Epoch: 8, At: now.Add(time.Hour)}
	if got := code(x.Approve(q("60"), []*Disposition{approvedOther, x}, act)); got != "INVALID_KNOWN_VALUE /disposition/quantity" {
		t.Fatal(got)
	}
	if got := code(x.Approve(q("39"), nil, act)); got != "INVALID_KNOWN_VALUE /disposition/quantity" {
		t.Fatal("a correction since the request: " + got)
	}
	if err = x.Approve(q("70"), []*Disposition{approvedOther, x}, act); err != nil {
		t.Fatal(err)
	}
	if x.Status != DispositionApproved || x.RowVersion != 2 || x.Decision == nil || !x.Decision.Approve || x.Decision.By != reviewer {
		t.Fatalf("approved %+v", x)
	}
	if got := code(x.Decidable(2, reviewer)); got != "INVALID_STATE /args/disposition_id" {
		t.Fatal(got)
	}
	if d := Disposed([]*Disposition{approvedOther, x, {ID: "disp.r", OrderLineID: "L1", Status: DispositionRequested, Quantity: q("5")}}); !d["L1"].Equal(q("70")) {
		t.Fatalf("disposed %v", d)
	}
	// PUR-INV-013: a verification may not leave the line verified below its approved dispositions.
	xs := []*Disposition{approvedOther, x}
	if Undercut(xs, map[string]kernel.Decimal{"L1": q("69")}, []string{"L1"}) != "L1" || Undercut(xs, map[string]kernel.Decimal{"L1": q("70")}, []string{"L1"}) != "" {
		t.Fatal("undercut")
	}
	ret, err := Dispose(disposeReq("disp.2", ReturnToSupplier, "1"), c, []string{"L1"}, pcs, map[string]kernel.Decimal{"L1": q("100")}, nil, buyer, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := code(ret.Approve(q("100"), nil, act)); got != "INVALID_STATE /disposition/evidence" {
		t.Fatal("a return to the supplier cites its evidence: " + got)
	}
	ret.Reject(Act{By: reviewer, At: now.Add(time.Hour), Reason: "no evidence"})
	if ret.Status != DispositionRejected || ret.RowVersion != 2 || ret.Decision.Approve || ret.Decision.Reason != "no evidence" {
		t.Fatalf("rejected %+v", ret)
	}
}

// S5R-D41 (PUR-INV-009 "只减少数量（含整行降为零）的变更不查供应条件有效期，单价沿用当前修订"): a proposal is decrease-only
// when no line buys more — allocated or overbuy, a line the revision in force does not buy is an
// increase —, every line still bought keeps its condition line and supplier date, and some line buys
// less (a line dropped included).
func TestDecreaseOnlyProposals(t *testing.T) {
	src := draft.Source{Ref: draft.Ref{Kind: draft.KindSupplierOffer, ID: "offer.1", Revision: 1}, ConditionLineID: "OL1"}
	ds := draft.DocLineSource{Kind: src.Kind, ID: src.ID, Revision: src.Revision, ConditionLineID: src.ConditionLineID}
	line := func(id, allocated, overbuy, date, conditionDate string) draft.DocLine {
		return draft.DocLine{OrderLineID: id, AllocatedQuantity: allocated, OverbuyQuantity: overbuy, Source: ds,
			Delivery: draft.DocDelivery{SupplierDate: date, ConditionDate: conditionDate}}
	}
	current := draft.Document{Lines: []draft.DocLine{line("L1", "100", "20", "2027-01-15", ""), line("L2", "10", "0", "2027-01-20", "2027-01-15")}}
	propose := func(dates map[string]string, ls ...draft.RequestLine) Proposal {
		if dates == nil {
			dates = map[string]string{}
		}
		return Proposal{Request: draft.Request{Lines: ls}, Dates: dates}
	}
	rl := func(id, quantity, overbuy string) draft.RequestLine {
		l := draft.RequestLine{OrderLineID: id, Quantity: q(quantity), Source: src}
		if overbuy != "0" {
			l.Overbuy = &draft.Overbuy{Quantity: q(overbuy), Reason: "MOQ"}
		}
		return l
	}
	l2 := map[string]string{"L2": "2027-01-20"}
	for _, c := range []struct {
		name string
		p    Proposal
		want bool
	}{
		{"short receipt", propose(l2, rl("L1", "90", "20"), rl("L2", "10", "0")), true},
		{"overbuy dropped", propose(l2, rl("L1", "100", "0"), rl("L2", "10", "0")), true},
		{"line dropped", propose(nil, rl("L1", "100", "20")), true},
		{"nothing less", propose(l2, rl("L1", "100", "20"), rl("L2", "10", "0")), false},
		{"more allocated", propose(l2, rl("L1", "101", "0"), rl("L2", "5", "0")), false},
		{"more overbuy", propose(l2, rl("L1", "90", "25"), rl("L2", "10", "0")), false},
		{"a new line", propose(l2, rl("L1", "90", "20"), rl("L2", "10", "0"), rl("L3", "1", "0")), false},
		{"another date", propose(map[string]string{"L1": "2027-01-16", "L2": "2027-01-20"}, rl("L1", "90", "20"), rl("L2", "10", "0")), false},
		{"back to the condition's date", propose(nil, rl("L1", "90", "20"), rl("L2", "10", "0")), false},
		{"the same stated date as the condition's", propose(map[string]string{"L1": "2027-01-15", "L2": "2027-01-20"}, rl("L1", "90", "20"), rl("L2", "10", "0")), true},
	} {
		if got := c.p.DecreaseOnly(current); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	other := rl("L1", "90", "20")
	other.Source.Revision = 2
	if propose(l2, other, rl("L2", "10", "0")).DecreaseOnly(current) {
		t.Error("another condition line is another price")
	}
}
