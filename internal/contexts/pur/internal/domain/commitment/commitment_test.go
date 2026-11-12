package commitment

import (
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The rules of PUR-AGG-PurchaseCommitment (SM-PUR-COMMITMENT, PUR-INV-009, PUR-INV-010) on a
// synthetic draft: BRC-001's 600.00 USD draft of order.1 from supplier.1, prepared by "author",
// supply valid for a day. Python V0.50 has no test: A47 is design only
// (docs/T04_A46_PURCHASE_DRAFT_REPORT_V0.1.md, A47 #1–#8 and "A47 最低验收").

var (
	now       = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	author    = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "author"}
	requester = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent"}
	reviewer  = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer"}
	buyer     = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "buyer"}
	hashA     = strings.Repeat("a", 64)
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

func fixture() Draft {
	return Draft{ID: "draft.1", Hash: hashA, OrderID: "order.1", OrderRevision: 1, OrderHash: strings.Repeat("b", 64), SupplierID: "supplier.1",
		Currency: "USD", SupplyValidUntil: now.Add(24 * time.Hour), PreparedBy: author}
}

func requested(t *testing.T, d Draft) *Commitment {
	t.Helper()
	c, err := Request("commitment.1", d, d.Hash, nil, requester, 3, now, "trade")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func approved(t *testing.T, d Draft, acks ...Acknowledgement) *Commitment {
	t.Helper()
	c := requested(t, d)
	if err := c.Approve(d, now.Add(2*time.Hour), acks, Act{By: reviewer, Epoch: 4, At: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	return c
}

func issued(t *testing.T, d Draft) *Commitment {
	t.Helper()
	c := approved(t, d)
	if err := c.Issuable(2, d, now.Add(10*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(Issue{Act: Act{By: buyer, Epoch: 5, At: now.Add(10 * time.Minute)}, Number: "PO-2026-00001",
		Sequence: Sequence{ID: "po.seq", Revision: 1}, SentAt: now.Add(9 * time.Minute), Evidence: []Evidence{{SourceID: "mail", Locator: "body"}}}); err != nil {
		t.Fatal(err)
	}
	return c
}

// A47 #1: the request binds the draft at its hash and the order at its revision and hash; it is
// refused for another hash, a voided draft, an expired supply and a draft with a live commitment
// (PURCHASE_DRAFT_COMMITTED once issued). An AI may request; the requester owns it.
func TestRequestBindsTheDraft(t *testing.T) {
	d := fixture()
	c := requested(t, d)
	if c.Status != StatusRequested || c.RowVersion != 1 || c.DraftHash != hashA || c.OrderRevision != 1 || c.OwnerActor != "agent" ||
		c.OwnerTeam != "trade" || c.Requested.By != requester || !c.SupplyValidUntil.Equal(d.SupplyValidUntil) {
		t.Fatalf("commitment %+v", c)
	}
	_, err := Request("c", d, strings.Repeat("c", 64), nil, requester, 3, now, "")
	if code(err) != "REFERENCE_STALE /args/draft_hash" {
		t.Fatal(code(err))
	}
	voided := d
	voided.Voided = true
	if _, err = Request("c", voided, hashA, nil, requester, 3, now, ""); code(err) != "INVALID_STATE /args/draft_id" {
		t.Fatal(code(err))
	}
	if _, err = Request("c", d, hashA, nil, requester, 3, d.SupplyValidUntil, ""); code(err) != "SUPPLY_CONDITION_EXPIRED /args/draft_id" {
		t.Fatal(code(err))
	}
	for status, want := range map[string]string{StatusRequested: "INVALID_STATE", StatusApproved: "INVALID_STATE",
		StatusIssued: "PURCHASE_DRAFT_COMMITTED", StatusConfirmed: "PURCHASE_DRAFT_COMMITTED", StatusRejected: "OK", StatusRevoked: "OK"} {
		_, err = Request("c", d, hashA, []Live{{ID: "other", Status: status}}, requester, 3, now, "")
		if got, _, _ := strings.Cut(code(err), " "); got != want {
			t.Errorf("%s: %s", status, code(err))
		}
	}
	if c.Answer("draft.1", hashA, requester) != nil || code(c.Answer("draft.1", hashA, author)) != "VERSION_CONFLICT /args/commitment_id" ||
		code(c.Answer("draft.2", hashA, requester)) != "VERSION_CONFLICT /args/commitment_id" {
		t.Fatal("a taken ID is answered for the same request only")
	}
}

// A47 #2 (A47 最低验收: 编制人或申请人自审被拒): the draft's preparer and the requester do not decide; a
// commitment is decided once, at its version, on the draft it binds and not voided, stating the
// draft hash the decider reviewed.
func TestDecisionIsIndependentAndOnce(t *testing.T) {
	d := fixture()
	c := requested(t, d)
	for _, c2 := range []struct {
		by   kernel.ActorRef
		want string
	}{{author, "INDEPENDENT_REVIEW_REQUIRED /args/commitment_id"}, {kernel.ActorRef{Kind: kernel.ActorHuman, ID: "agent"},
		"INDEPENDENT_REVIEW_REQUIRED /args/commitment_id"}, {reviewer, "OK"}} {
		if got := code(c.Decidable(1, d, c2.by, hashA)); got != c2.want {
			t.Errorf("%s: %s", c2.by.ID, got)
		}
	}
	if code(c.Decidable(2, d, reviewer, hashA)) != "VERSION_CONFLICT /args/expected_version" {
		t.Fatal("version")
	}
	if code(c.Decidable(1, d, reviewer, strings.Repeat("c", 64))) != "REFERENCE_STALE /args/draft_hash" {
		t.Fatal("hash")
	}
	voided := d
	voided.Voided = true
	if code(c.Decidable(1, voided, reviewer, hashA)) != "INVALID_STATE /args/commitment_id" {
		t.Fatal("voided")
	}
	moved := d
	moved.OrderRevision = 2
	if code(c.Decidable(1, moved, reviewer, hashA)) != "REFERENCE_INVALID /args/commitment_id" {
		t.Fatal("a commitment binds its draft as stored")
	}
	c.Reject(Act{By: reviewer, Epoch: 4, At: now, Reason: "Price too high"})
	if c.Status != StatusRejected || c.RowVersion != 2 || c.Decision.Approve || c.Decision.Reason != "Price too high" {
		t.Fatalf("rejected %+v", c)
	}
	if code(c.Decidable(2, d, reviewer, hashA)) != "INVALID_STATE /args/commitment_id" {
		t.Fatal("decided once")
	}
}

// A47 #4: an approval is valid after its decision and not after the draft's supply validity.
func TestApprovalValidity(t *testing.T) {
	d := fixture()
	act := Act{By: reviewer, Epoch: 4, At: now}
	for _, until := range []time.Time{now, now.Add(-time.Second), d.SupplyValidUntil.Add(time.Microsecond)} {
		c := requested(t, d)
		if code(c.Approve(d, until, nil, act)) != "INVALID_KNOWN_VALUE /args/valid_until" {
			t.Fatalf("%s: %s", until, code(c.Approve(d, until, nil, act)))
		}
		if c.Status != StatusRequested || c.RowVersion != 1 {
			t.Fatal("a refused approval changes nothing")
		}
	}
	c := requested(t, d)
	if err := c.Approve(d, d.SupplyValidUntil, nil, act); err != nil || c.Status != StatusApproved || c.RowVersion != 2 ||
		!c.Decision.ValidUntil.Equal(d.SupplyValidUntil) || c.Decision.Acknowledgements == nil {
		t.Fatalf("approved %+v, %v", c, err)
	}
	if c.State(d.SupplyValidUntil.Add(-time.Microsecond)) != StatusApproved || c.State(d.SupplyValidUntil) != StateExpired {
		t.Fatal("expiry is derived at the validity's end")
	}
}

// A47 #5 and D-14 (PUR-INV-009): deviations from the costing — another source, a cost above it, a
// cost that cannot be compared, an overbuy — are acknowledged one by one with a reason
// (PURCHASE_DEVIATION_UNACKNOWLEDGED); an acknowledgement of a deviation the draft does not have is
// REFERENCE_INVALID.
func TestDeviationsAreAcknowledgedWithAReason(t *testing.T) {
	v1200, zero, minus := "1200", "0", "-300"
	doc := draft.Document{Lines: []draft.DocLine{
		{OrderLineID: "L1", CostingReference: draft.DocCostingRef{SameSource: true, CostVarianceMinor: &zero}},
		{OrderLineID: "L2", CostingReference: draft.DocCostingRef{SameSource: false, CostVarianceMinor: &v1200}},
		{OrderLineID: "L3", CostingReference: draft.DocCostingRef{SameSource: true}},
		{OrderLineID: "L4", CostingReference: draft.DocCostingRef{SameSource: true, CostVarianceMinor: &minus},
			Overbuy: &draft.DocOverbuy{Quantity: "30", Reason: "MOQ"}},
	}}
	got := Deviations(doc)
	want := []Deviation{{"L2", DeviationSourceDiffers}, {"L2", DeviationCostAbove}, {"L3", DeviationNotComparable}, {"L4", DeviationOverbuy}}
	if !slices.Equal(got, want) {
		t.Fatalf("deviations %v", got)
	}
	d := fixture()
	d.Deviations = got
	act := Act{By: reviewer, Epoch: 4, At: now}
	acks := []Acknowledgement{{"L4", DeviationOverbuy, "MOQ 150; stock"}, {"L2", DeviationSourceDiffers, "Second supplier"},
		{"L3", DeviationNotComparable, "CNY quote"}, {"L2", DeviationCostAbove, "Faster delivery"}}
	c := requested(t, d)
	err := c.Approve(d, now.Add(time.Hour), acks[:3], act)
	if r, _ := kernel.AsRejection(err); code(err) != "PURCHASE_DEVIATION_UNACKNOWLEDGED /args/acknowledgements" || !strings.Contains(r.Fields[0].Reason, "L2") {
		t.Fatalf("missing one: %v", err)
	}
	blank := slices.Clone(acks)
	blank[0].Reason = " "
	if code(c.Approve(d, now.Add(time.Hour), blank, act)) != "PURCHASE_DEVIATION_UNACKNOWLEDGED /args/acknowledgements" {
		t.Fatal("a reason is required")
	}
	extra := append(slices.Clone(acks), Acknowledgement{"L1", DeviationCostAbove, "?"})
	if code(c.Approve(d, now.Add(time.Hour), extra, act)) != "REFERENCE_INVALID /args/acknowledgements/4" {
		t.Fatal("no such deviation")
	}
	if err := c.Approve(d, now.Add(time.Hour), acks, act); err != nil {
		t.Fatal(err)
	}
	if c.Decision.Acknowledgements[0].OrderLineID != "L2" || c.Decision.Acknowledgements[3].Kind != DeviationOverbuy {
		t.Fatalf("stored by line and kind: %+v", c.Decision.Acknowledgements)
	}
}

// A47 #6: a revocation stops the issue (PURCHASE_COMMITMENT_NOT_APPROVED) and is only before it.
func TestRevokeOnlyBeforeIssue(t *testing.T) {
	d := fixture()
	c := requested(t, d)
	if code(c.Revoke(1, Act{By: reviewer, At: now, Reason: "r"})) != "INVALID_STATE /args/commitment_id" {
		t.Fatal("a request is not revoked")
	}
	c = approved(t, d)
	if code(c.Revoke(1, Act{By: reviewer, At: now, Reason: "r"})) != "VERSION_CONFLICT /args/expected_version" {
		t.Fatal("version")
	}
	if err := c.Revoke(2, Act{By: reviewer, Epoch: 6, At: now.Add(5 * time.Minute), Reason: "Supplier changed terms"}); err != nil ||
		c.Status != StatusRevoked || c.RowVersion != 3 || c.Revocation.Reason != "Supplier changed terms" {
		t.Fatalf("revoked %+v, %v", c, err)
	}
	if code(c.Issuable(3, d, now.Add(6*time.Minute), true)) != "PURCHASE_COMMITMENT_NOT_APPROVED /args/commitment_id" {
		t.Fatal("a revoked approval is not issued")
	}
	c = issued(t, d)
	if code(c.Revoke(3, Act{By: reviewer, At: now, Reason: "r"})) != "INVALID_STATE /args/commitment_id" {
		t.Fatal("an issued commitment is not revoked")
	}
}

// PUR-INV-010 and A47 #7: issued once, on a live approval (not requested or rejected:
// PURCHASE_COMMITMENT_NOT_APPROVED), before it expires (APPROVAL_EXPIRED), while its decider is
// qualified, for the draft not voided, with every overbuy approved by the decision
// (OVERBUY_NOT_APPROVED), sent after the approval and not after the record.
func TestIssueOnceOnALiveApproval(t *testing.T) {
	d := fixture()
	c := requested(t, d)
	if code(c.Issuable(1, d, now, true)) != "PURCHASE_COMMITMENT_NOT_APPROVED /args/commitment_id" {
		t.Fatal("requested")
	}
	c.Reject(Act{By: reviewer, At: now, Reason: "no"})
	if code(c.Issuable(2, d, now, true)) != "PURCHASE_COMMITMENT_NOT_APPROVED /args/commitment_id" {
		t.Fatal("rejected")
	}
	c = approved(t, d)
	for _, x := range []struct {
		at        time.Time
		qualified bool
		d         Draft
		want      string
	}{
		{now.Add(2 * time.Hour), true, d, "APPROVAL_EXPIRED /args/commitment_id"},
		{now.Add(time.Hour), false, d, "PURCHASE_COMMITMENT_NOT_APPROVED /args/commitment_id"},
		{now.Add(time.Hour), true, Draft{}, "REFERENCE_INVALID /args/commitment_id"},
	} {
		if got := code(c.Issuable(2, x.d, x.at, x.qualified)); got != x.want {
			t.Errorf("%v %v: %s", x.at, x.qualified, got)
		}
	}
	voided := d
	voided.Voided = true
	if code(c.Issuable(2, voided, now.Add(time.Hour), true)) != "INVALID_STATE /args/commitment_id" {
		t.Fatal("voided draft")
	}
	over := d
	over.Deviations = []Deviation{{"L1", DeviationOverbuy}}
	if code(c.Issuable(2, over, now.Add(time.Hour), true)) != "OVERBUY_NOT_APPROVED /args/commitment_id" {
		t.Fatal("an overbuy the decision did not approve")
	}
	for _, sent := range []time.Time{now.Add(11 * time.Minute), now} {
		x := approved(t, d)
		err := x.Send(Issue{Act: Act{By: buyer, At: now.Add(10 * time.Minute)}, Number: "PO-1", SentAt: sent})
		if code(err) != "INVALID_KNOWN_VALUE /args/sent_at" || x.Status != StatusApproved {
			t.Fatalf("sent %s: %s", sent, code(err))
		}
	}
	c = issued(t, d)
	if c.Status != StatusIssued || c.RowVersion != 3 || c.Issue.Number != "PO-2026-00001" || !IsCommitted(c.Status) {
		t.Fatalf("issued %+v", c)
	}
	if code(c.Issuable(3, d, now.Add(11*time.Minute), true)) != "INVALID_STATE /args/commitment_id" {
		t.Fatal("issued once")
	}
}

// A47 #7: a supplier confirmation is a separate fact; as issued it confirms the commitment, with
// deviations it is recorded and leaves the commitment issued; it is received after the commitment
// was sent and not after it is recorded; a confirmed commitment is not confirmed again.
func TestSupplierConfirmationIsASeparateFact(t *testing.T) {
	d := fixture()
	c := approved(t, d)
	conf := Confirmation{Act: Act{By: requester, Epoch: 7, At: now.Add(time.Hour)}, Result: ResultAsIssued, ReceivedAt: now.Add(30 * time.Minute)}
	if code(c.Confirm(2, conf)) != "INVALID_STATE /args/commitment_id" {
		t.Fatal("not issued")
	}
	c = issued(t, d)
	for _, received := range []time.Time{now.Add(2 * time.Hour), now.Add(8 * time.Minute)} {
		x := conf
		x.ReceivedAt = received
		if code(c.Confirm(3, x)) != "INVALID_KNOWN_VALUE /args/received_at" {
			t.Fatalf("received %s", received)
		}
	}
	dev := conf
	dev.Result, dev.Deviation = ResultWithDeviations, "Delivery 5 days later"
	if err := c.Confirm(3, dev); err != nil || c.Status != StatusIssued || c.RowVersion != 4 || c.Fresh().Version != 4 {
		t.Fatalf("with deviations %+v, %v", c, err)
	}
	c.Confirmations[0].Fresh = false
	if err := c.Confirm(4, conf); err != nil || c.Status != StatusConfirmed || c.RowVersion != 5 || len(c.Confirmations) != 2 ||
		c.Fresh().Result != ResultAsIssued {
		t.Fatalf("as issued %+v, %v", c, err)
	}
	if code(c.Confirm(5, conf)) != "INVALID_STATE /args/commitment_id" || code(c.Confirm(4, conf)) != "VERSION_CONFLICT /args/expected_version" {
		t.Fatal("confirmed once")
	}
}
