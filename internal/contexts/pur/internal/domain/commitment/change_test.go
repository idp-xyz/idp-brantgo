package commitment

import (
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The change and cancellation rules of PUR-AGG-PurchaseCommitment (SM-PUR-COMMITMENT,
// SM-PUR-COMMITMENT-CHANGE, PUR-INV-011; A47 #6 and #7 of docs/T04_A46_PURCHASE_DRAFT_REPORT_V0.1.md: 已登记
// 的承诺不倒写，须走取消或变更流程) on the synthetic commitment of commitment_test.go: one line L1 of 120 PCS.

func q(s string) kernel.Decimal { return kernel.MustDecimal(s) }

func lineQ(allocated, overbuy string) LineQuantity {
	return LineQuantity{OrderLineID: "L1", Allocated: q(allocated), Overbuy: q(overbuy), SupplierDate: "2027-01-15"}
}

// changeOf is change "change.1" of c proposing lines, prepared by by, sealed with hash hashA.
func changeOf(t *testing.T, c *Commitment, by kernel.ActorRef, lines ...LineQuantity) *Change {
	t.Helper()
	src := draft.Source{Ref: draft.Ref{Kind: draft.KindSupplierOffer, ID: "offer.1", Revision: 1}, ConditionLineID: "OL1"}
	r := ChangeRequest{ChangeID: "change.1", CommitmentID: c.ID, Revision: c.Revision, Reason: "supplier letter",
		Evidence: []Evidence{{SourceID: "letter", Locator: "p1"}}}
	for _, l := range lines {
		r.Lines = append(r.Lines, ChangeLine{OrderLineID: l.OrderLineID, Quantity: l.Allocated, Source: &src})
	}
	x, err := NewChange(r, c, lines, []byte(`{}`), hashA, 1, c.OrderHash, now.Add(24*time.Hour), by, 6, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// SM-PUR-COMMITMENT-CHANGE draft → approved | rejected: decided once, by a human who neither prepared
// the change nor requested the commitment (independence row of purchase.commitment.change.decide), on
// the change hash reviewed, while the commitment is issued or confirmed at the revision the change is
// bound to (PURCHASE_COMMITMENT_CHANGE_STALE); every deviation acknowledged with a reason.
func TestChangeDecisionIsIndependentAndBound(t *testing.T) {
	c := issued(t, fixture())
	x := changeOf(t, c, buyer, lineQ("110", "0"))
	if x.Status != ChangeDraft || x.RowVersion != 1 || x.BaseRevision != 1 || len(x.Request) == 0 || x.RequestHash != draft.Hash(x.Request) {
		t.Fatalf("change %+v", x)
	}
	for _, tc := range []struct {
		version int64
		by      kernel.ActorRef
		hash    string
		want    string
	}{
		{2, reviewer, hashA, "VERSION_CONFLICT /args/expected_version"},
		{1, buyer, hashA, "INDEPENDENT_REVIEW_REQUIRED /args/change_id"},
		{1, requester, hashA, "INDEPENDENT_REVIEW_REQUIRED /args/change_id"},
		{1, reviewer, strings.Repeat("c", 64), "REFERENCE_STALE /args/change_hash"},
		{1, reviewer, hashA, "OK"},
	} {
		if got := code(x.Decidable(tc.version, c, tc.by, tc.hash)); got != tc.want {
			t.Errorf("%+v: %s", tc, got)
		}
	}
	moved := *c
	moved.Revision = 2
	if got := code(x.Decidable(1, &moved, reviewer, hashA)); got != "PURCHASE_COMMITMENT_CHANGE_STALE /args/change_id" {
		t.Fatal(got)
	}
	devs := []Deviation{{OrderLineID: "L1", Kind: DeviationDeliveryChanged}}
	if got := code(x.Approve(devs, nil, Act{By: reviewer, Epoch: 7, At: now.Add(2 * time.Hour)})); got != "PURCHASE_DEVIATION_UNACKNOWLEDGED /args/acknowledgements" {
		t.Fatal(got)
	}
	if got := code(x.Approve(devs, []Acknowledgement{{OrderLineID: "L1", Kind: DeviationMOQNotMet, Reason: "x"}}, Act{By: reviewer})); got != "REFERENCE_INVALID /args/acknowledgements/0" {
		t.Fatal(got)
	}
	if err := x.Approve(devs, []Acknowledgement{{OrderLineID: "L1", Kind: DeviationDeliveryChanged, Reason: "five days later"}},
		Act{By: reviewer, Epoch: 7, At: now.Add(2 * time.Hour)}); err != nil || x.Status != ChangeApproved || x.RowVersion != 2 {
		t.Fatalf("approve %v %+v", err, x)
	}
	if got := code(x.Decidable(2, c, reviewer, hashA)); got != "INVALID_STATE /args/change_id" {
		t.Fatal(got)
	}
	y := changeOf(t, c, buyer, lineQ("110", "0"))
	y.Reject(Act{By: reviewer, Epoch: 7, At: now, Reason: "no"})
	if y.Status != ChangeRejected || y.RowVersion != 2 || y.Decision == nil || y.Decision.Approve {
		t.Fatalf("reject %+v", y)
	}
	if got := code(y.Appliable(2, c)); got != "INVALID_STATE /args/change_id" {
		t.Fatal(got)
	}
}

// SM-PUR-COMMITMENT-CHANGE approved → applied and SM-PUR-COMMITMENT confirmed → issued: the change
// becomes revision 2 — what it buys, the order binding and the document —, the commitment is issued
// again and both versions move; a second change of revision 1 is stale; an applied change is applied
// once.
func TestChangeAppliesTheNextRevision(t *testing.T) {
	c := issued(t, fixture())
	if err := c.Confirm(3, Confirmation{Act: Act{By: buyer, At: now.Add(20 * time.Minute)}, Result: ResultAsIssued, ReceivedAt: now.Add(15 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	x := changeOf(t, c, buyer, lineQ("110", "0"))
	other := changeOf(t, c, buyer, lineQ("105", "0"))
	for _, ch := range []*Change{x, other} {
		if err := ch.Approve(nil, nil, Act{By: reviewer, At: now.Add(2 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if got := code(x.Appliable(1, c)); got != "VERSION_CONFLICT /args/expected_version" {
		t.Fatal(got)
	}
	if err := x.Appliable(2, c); err != nil {
		t.Fatal(err)
	}
	r := c.Apply(x, Act{By: buyer, Epoch: 8, At: now.Add(3 * time.Hour)})
	if c.Status != StatusIssued || c.Revision != 2 || c.RowVersion != 5 || r.Number != 2 || r.Kind != RevisionChange || r.ChangeID != "change.1" ||
		!r.Fresh || c.Current() != r || x.Status != ChangeApplied || x.RowVersion != 3 || x.AppliedRevision != 2 {
		t.Fatalf("applied %+v %+v %+v", c, r, x)
	}
	if lines, err := c.Lines(nil); err != nil || len(lines) != 1 || !lines[0].Purchased().Equal(q("110")) {
		t.Fatalf("lines %+v %v", lines, err)
	}
	if rev, hash := c.OrderBinding(); rev != 1 || hash != c.OrderHash {
		t.Fatalf("binding %d %s", rev, hash)
	}
	if got := code(other.Appliable(2, c)); got != "PURCHASE_COMMITMENT_CHANGE_STALE /args/change_id" {
		t.Fatal(got)
	}
	if got := code(x.Appliable(3, c)); got != "INVALID_STATE /args/change_id" {
		t.Fatal(got)
	}
	if err := c.Confirm(5, Confirmation{Act: Act{By: buyer, At: now.Add(4 * time.Hour)}, Result: ResultAsIssued, ReceivedAt: now.Add(4 * time.Hour)}); err != nil ||
		c.Status != StatusConfirmed || c.Confirmations[len(c.Confirmations)-1].Revision != 2 || c.Confirmations[0].Revision != 1 {
		t.Fatalf("confirmation of revision 2: %v %+v", err, c)
	}
}

// PUR-INV-011 "已核实交付量不被变更或取消抹去" (PUR-INV-008): no revision buys less of a line than was
// verified delivered (SUPPLIER_DELIVERY_EXCEEDED); a short receipt — exactly the verified quantity —
// is fulfilled.
func TestRevisionsKeepTheVerifiedDeliveries(t *testing.T) {
	verified := map[string]kernel.Decimal{"L1": q("850")}
	at := func(string) string { return "/args/lines/0/quantity" }
	if got := code(Keeps([]LineQuantity{lineQ("849", "0")}, verified, at)); got != "SUPPLIER_DELIVERY_EXCEEDED /args/lines/0/quantity" {
		t.Fatal(got)
	}
	if err := Keeps([]LineQuantity{lineQ("800", "50")}, verified, at); err != nil {
		t.Fatal(err)
	}
	if Fulfilled([]LineQuantity{lineQ("900", "0")}, verified) || !Fulfilled([]LineQuantity{lineQ("850", "0")}, verified) || Fulfilled(nil, verified) {
		t.Fatal("fulfilled")
	}
}

// SM-PUR-COMMITMENT issued | confirmed → cancelled ("授权人工、理由与供应商证据；释放分配"; "已核实交付量保留"): each line
// keeps exactly what was verified — the allocated quantity first, the rest as overbuy —, the
// cancellation is its next revision with the binding of the one it ends; once. S5R-D41 (PUR-INV-011
// "承诺有未核实的交付声明（recorded）时取消被拒"): while a claim awaits its verification the cancellation is
// INVALID_STATE, reason unverified_delivery_pending, and nothing moves.
func TestCancelKeepsTheVerifiedDeliveries(t *testing.T) {
	c := issued(t, fixture())
	current := []LineQuantity{lineQ("100", "20")}
	act := Act{By: buyer, Epoch: 9, At: now.Add(time.Hour), Reason: "factory closed"}
	if _, err := c.Cancel(2, current, nil, nil, act, nil); code(err) != "VERSION_CONFLICT /args/expected_version" {
		t.Fatal(code(err))
	}
	_, err := c.Cancel(3, current, nil, []string{"delivery.2"}, act, nil)
	if rej, _ := kernel.AsRejection(err); code(err) != "INVALID_STATE /args/commitment_id" ||
		!strings.HasPrefix(rej.Fields[0].Reason, "unverified_delivery_pending: ") || !strings.Contains(rej.Fields[0].Reason, "delivery.2") {
		t.Fatalf("a pending claim stops the cancellation (S5R-D41): %v", err)
	}
	if c.Status != StatusIssued || c.Revision != 1 || c.RowVersion != 3 {
		t.Fatalf("nothing moved %+v", c)
	}
	r, err := c.Cancel(3, current, map[string]kernel.Decimal{"L1": q("110")}, nil, act, []Evidence{{SourceID: "letter", Locator: "p2"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != StatusCancelled || c.Revision != 2 || c.RowVersion != 4 || c.Cancellation == nil || r.Kind != RevisionCancel || r.Document != nil ||
		len(r.Lines) != 1 || !r.Lines[0].Allocated.Equal(q("100")) || !r.Lines[0].Overbuy.Equal(q("10")) || r.OrderRevision != 1 {
		t.Fatalf("cancelled %+v %+v", c, r)
	}
	if !IsCommitted(c.Status) || IsOpen(c.Status) {
		t.Fatal("a cancelled commitment's draft stays committed and the commitment is not open")
	}
	if _, err = c.Cancel(4, current, nil, nil, act, nil); code(err) != "INVALID_STATE /args/commitment_id" {
		t.Fatal(code(err))
	}
	if err = c.Confirm(4, Confirmation{Act: Act{At: now.Add(2 * time.Hour)}, Result: ResultAsIssued, ReceivedAt: now.Add(2 * time.Hour)}); code(err) != "INVALID_STATE /args/commitment_id" {
		t.Fatal(code(err))
	}
	none := issued(t, fixture())
	r, err = none.Cancel(3, []LineQuantity{lineQ("120", "0")}, nil, nil, act, nil)
	if err != nil || !r.Lines[0].Purchased().IsZero() {
		t.Fatalf("nothing delivered keeps nothing: %v %+v", err, r)
	}
	requestedOnly := requested(t, fixture())
	if _, err = requestedOnly.Cancel(1, nil, nil, []string{"delivery.2"}, act, nil); code(err) != "INVALID_STATE /args/commitment_id" {
		t.Fatal(code(err))
	}
}

// A change states every line of the draft once, a zero line nothing else, something bought and
// something changed; it is bound to the revision in force of an issued or confirmed commitment.
func TestChangeProposals(t *testing.T) {
	c := issued(t, fixture())
	d := &draft.Draft{ID: "draft.1", OrderID: "order.1", Lines: []draft.Line{{OrderLineID: "L1", Quantity: q("120")}, {OrderLineID: "L2", Quantity: q("10")}}}
	src := draft.Source{Ref: draft.Ref{Kind: draft.KindSupplierOffer, ID: "offer.1", Revision: 1}, ConditionLineID: "OL1"}
	ds := draft.DocLineSource{Kind: src.Kind, ID: src.ID, Revision: src.Revision, ConditionLineID: src.ConditionLineID}
	current := draft.Document{Lines: []draft.DocLine{
		{OrderLineID: "L1", AllocatedQuantity: "120", OverbuyQuantity: "0", Source: ds, Delivery: draft.DocDelivery{SupplierDate: "2027-01-15"}},
		{OrderLineID: "L2", AllocatedQuantity: "10", OverbuyQuantity: "0", Source: ds, Delivery: draft.DocDelivery{SupplierDate: "2027-01-15"}}}}
	req := func(lines ...ChangeLine) ChangeRequest {
		return ChangeRequest{ChangeID: "change.1", CommitmentID: c.ID, Revision: 1, Lines: lines, Reason: "r"}
	}
	for _, tc := range []struct {
		r    ChangeRequest
		want string
	}{
		{req(ChangeLine{OrderLineID: "L1", Quantity: q("120"), Source: &src}), "INVALID_KNOWN_VALUE /args/lines"},
		{req(ChangeLine{OrderLineID: "L1", Quantity: q("120"), Source: &src}, ChangeLine{OrderLineID: "L9", Quantity: q("1"), Source: &src}), "REFERENCE_INVALID /args/lines/1/order_line_id"},
		{req(ChangeLine{OrderLineID: "L1", Quantity: q("0"), Source: &src}, ChangeLine{OrderLineID: "L2", Quantity: q("10"), Source: &src}), "INVALID_KNOWN_VALUE /args/lines/0"},
		{req(ChangeLine{OrderLineID: "L1", Quantity: q("120")}, ChangeLine{OrderLineID: "L2", Quantity: q("10"), Source: &src}), "INVALID_KNOWN_VALUE /args/lines/0/source"},
		{req(ChangeLine{OrderLineID: "L1", Quantity: q("0")}, ChangeLine{OrderLineID: "L2", Quantity: q("0")}), "INVALID_KNOWN_VALUE /args/lines"},
		{req(ChangeLine{OrderLineID: "L1", Quantity: q("120"), Source: &src}, ChangeLine{OrderLineID: "L2", Quantity: q("10"), Source: &src}), "INVALID_KNOWN_VALUE /args/lines"},
		{req(ChangeLine{OrderLineID: "L1", Quantity: q("120"), Source: &src, SupplierDate: "2027-01-20"}, ChangeLine{OrderLineID: "L2", Quantity: q("0")}), "OK"},
	} {
		lines, err := tc.r.Proposable(c, d, current)
		if got := code(err); got != tc.want {
			t.Errorf("%+v: %s", tc.r.Lines, got)
		}
		if err == nil && (len(lines) != 2 || lines[0].SupplierDate != "2027-01-20" || !lines[1].Purchased().IsZero()) {
			t.Errorf("lines %+v", lines)
		}
	}
	stale := req(ChangeLine{OrderLineID: "L1", Quantity: q("100"), Source: &src}, ChangeLine{OrderLineID: "L2", Quantity: q("10"), Source: &src})
	stale.Revision = 2
	if _, err := stale.Proposable(c, d, current); code(err) != "PURCHASE_COMMITMENT_CHANGE_STALE /args/revision" {
		t.Fatal(code(err))
	}
	if _, err := stale.Proposable(requested(t, fixture()), d, current); code(err) != "INVALID_STATE /args/commitment_id" {
		t.Fatal(code(err))
	}
	// The request round-trips through its canonical form; the proposal leaves the zero line out.
	r := req(ChangeLine{OrderLineID: "L1", Quantity: q("120"), Source: &src, SupplierDate: "2027-01-20",
		Overbuy: &draft.Overbuy{Quantity: q("5"), Reason: "MOQ"}}, ChangeLine{OrderLineID: "L2", Quantity: q("0")})
	r.Evidence = []Evidence{{SourceID: "letter", Locator: "p1"}}
	b, err := r.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeChangeRequest(b)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := back.Canonical()
	if string(again) != string(b) || len(back.Lines) != 2 || back.Lines[1].Source != nil || back.Lines[0].Overbuy == nil {
		t.Fatalf("round trip %s\n%s", b, again)
	}
	p := r.Propose(d)
	if len(p.Request.Lines) != 1 || !slices.Equal(p.Index, []int{0}) || p.Dates["L1"] != "2027-01-20" || p.Request.SelectionReason != "r" {
		t.Fatalf("proposal %+v", p)
	}
}

// A change's deviations are a draft's and its own: another supplier date than the condition's, a date
// after the order's, an MOQ or order multiple not met.
func TestChangeDeviations(t *testing.T) {
	zero := "0"
	doc := draft.Document{Lines: []draft.DocLine{{OrderLineID: "L1", CostingReference: draft.DocCostingRef{SameSource: true, CostVarianceMinor: &zero},
		Delivery: draft.DocDelivery{SupplierDate: "2027-02-05", ConditionDate: "2027-01-15", SlackDays: -5},
		Waivers:  []string{draft.WaiverMOQNotMet}}}}
	got := ChangeDeviations(doc)
	want := []Deviation{{OrderLineID: "L1", Kind: DeviationDeliveryChanged}, {OrderLineID: "L1", Kind: DeviationDeliveryLate},
		{OrderLineID: "L1", Kind: DeviationMOQNotMet}}
	if !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
}
