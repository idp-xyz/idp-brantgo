//go:build integration

package bootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	"go.idp.xyz/idp-brant/internal/contexts/pur"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// PUR purchase commitment changes and cancellation (WBS S5-19; tactical model SM-PUR-COMMITMENT,
// SM-PUR-COMMITMENT-CHANGE, PUR-INV-011 with PUR-INV-002 and PUR-INV-008; role templates' independence of
// purchase.commitment.change.decide from 变更的编制人与申请人; SIM-02 step 9) on PostgreSQL 16, on the PUR worlds
// of S3 and S5-01: newPurWorld (order.1 through PUR's port to an in-memory TRD) and TRD's real orders
// through the shared order world. Python V0.50 has the design only (A47 #6 and #7 of
// docs/T04_A46_PURCHASE_DRAFT_REPORT_V0.1.md: 已登记的承诺不倒写，须走取消或变更流程；交期变更、取消、短收和价格调整作为
// 后续受控动作) and no test to port. The fixture's order line L1 is 120 PCS; offer.1 sells sku.1 at 5.00 USD
// with an MOQ of 100, delivered on 2027-01-15, and the order requires 2027-01-31.

var purChangeEvidence = []purapi.Evidence{{SourceID: "evidence", Locator: "合成供应商变更确认函"}}

// purChange drives the commitment changes of the module m returns, in world w.
type purChange struct {
	w *scenWorld
	m func() *pur.Module
	x *purDeliver
}

func newPurChange(w *scenWorld, m func() *pur.Module) *purChange {
	return &purChange{w: w, m: m, x: newPurDeliver(w, m)}
}

// changeLine is line L1 of a change: quantity bought under offerID's line OL1 (none for zero), at
// the supplier's date (the condition's when empty).
func changeLine(quantity, offerID, date string) purapi.ChangeLine {
	l := purapi.ChangeLine{OrderLineID: "L1", Quantity: quantity, SupplierDate: date}
	if quantity != "0" {
		src := purSource(offerID)
		l.Source = &src
	}
	return l
}

func (c *purChange) prepareCmd(id, commitmentID string, revision int64, lines ...purapi.ChangeLine) purapi.PrepareCommitmentChange {
	return purapi.PrepareCommitmentChange{IdempotencyKey: c.w.key(), ChangeID: id, CommitmentID: commitmentID, Revision: revision, Lines: lines,
		Reason: "合成：供应商来函确认变更", Evidence: purChangeEvidence}
}

func (c *purChange) prepare(ctx context.Context, id, commitmentID string, revision int64, lines ...purapi.ChangeLine) (purapi.ChangeReceipt, error) {
	return c.m().PrepareCommitmentChange(ctx, c.prepareCmd(id, commitmentID, revision, lines...))
}

func (c *purChange) approveCmd(id string, version int64, hash string, acks ...purapi.Acknowledgement) purapi.DecideCommitmentChange {
	return purapi.DecideCommitmentChange{IdempotencyKey: c.w.key(), ChangeID: id, ExpectedVersion: version, ChangeHash: hash,
		Decision: purapi.DecisionApprove, Acknowledgements: acks}
}

func (c *purChange) approve(ctx context.Context, id string, version int64, hash string, acks ...purapi.Acknowledgement) (purapi.ChangeReceipt, error) {
	return c.m().DecideCommitmentChange(ctx, c.approveCmd(id, version, hash, acks...))
}

func (c *purChange) applyCmd(id string, version int64) purapi.ApplyCommitmentChange {
	return purapi.ApplyCommitmentChange{IdempotencyKey: c.w.key(), ChangeID: id, ExpectedVersion: version}
}

func (c *purChange) apply(ctx context.Context, id string, version int64) (purapi.ChangeReceipt, error) {
	return c.m().ApplyCommitmentChange(ctx, c.applyCmd(id, version))
}

func (c *purChange) cancel(ctx context.Context, commitmentID string, version int64) (purapi.CommitmentReceipt, error) {
	return c.m().CancelCommitment(ctx, purapi.CancelCommitment{IdempotencyKey: c.w.key(), CommitmentID: commitmentID, ExpectedVersion: version,
		Reason: "合成：工厂停产，双方确认取消余量", Evidence: purChangeEvidence})
}

// approved has the author prepare change id and the reviewer approve it, acknowledging acks.
func (c *purChange) approved(id, commitmentID string, revision int64, line purapi.ChangeLine, acks ...purapi.Acknowledgement) purapi.ChangeReceipt {
	c.w.t.Helper()
	r, err := c.prepare(c.w.author, id, commitmentID, revision, line)
	c.w.must(err)
	a, err := c.approve(c.w.reviewer, id, 1, r.ChangeHash, acks...)
	c.w.must(err)
	return a
}

// changed has change id prepared and approved, and applied by the author.
func (c *purChange) changed(id, commitmentID string, revision int64, line purapi.ChangeLine, acks ...purapi.Acknowledgement) purapi.ChangeReceipt {
	c.w.t.Helper()
	c.approved(id, commitmentID, revision, line, acks...)
	r, err := c.apply(c.w.author, id, 2)
	c.w.must(err)
	return r
}

func (c *purChange) view(ctx context.Context, id string) purapi.ChangeView {
	c.w.t.Helper()
	v, err := c.m().GetCommitmentChange(ctx, id)
	c.w.must(err)
	return v
}

// line returns line L1 of commitment id as the commitment read shows it: allocated, overbuy,
// purchased, verified and the supplier's date.
func (c *purChange) line(ctx context.Context, id string) string {
	c.w.t.Helper()
	v := c.x.p.view(ctx, id)
	if len(v.Lines) != 1 {
		c.w.t.Fatalf("lines of %s: %+v", id, v.Lines)
	}
	l := v.Lines[0]
	return strings.Join([]string{l.AllocatedQuantity, l.OverbuyQuantity, l.PurchaseQuantity, l.VerifiedQuantity, l.SupplierDate}, " ")
}

// payloads returns the payloads of the events of type eventType in company.A's Outbox.
func (c *purChange) payloads(eventType string) []map[string]any {
	c.w.t.Helper()
	var out []map[string]any
	for _, e := range c.w.events("company.A", purapi.Source) {
		if e.Type == eventType {
			out = append(out, e.Payload)
		}
	}
	return out
}

func raceTwo(a, b func() error) []string {
	out := make([]string, 2)
	var wg sync.WaitGroup
	for i, f := range []func() error{a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); out[i] = code(f()) }()
	}
	wg.Wait()
	return out
}

// PUR-INV-011 positive ("供应商确认交期延后 5 天的变更被独立裁决并应用"), SM-PUR-COMMITMENT-CHANGE draft → approved →
// applied and SM-PUR-COMMITMENT confirmed → issued (A47 #7): on a confirmed commitment of 100 PCS an AI
// agent prepares the next revision — 110 PCS, delivered five days later — with the supplier's
// evidence; only an independent human decides it, acknowledging the changed date; the purchaser
// applies it. The commitment is at revision 2, issued again until the supplier confirms that
// revision; the allocation moved to 110 (PUR-INV-002); the reads, InspectionSubject v1 and
// DownstreamCommitments v1 show the revision; PurchaseCommitmentChanged is in the Outbox. A change
// bound to the old revision is PURCHASE_COMMITMENT_CHANGE_STALE.
func TestCommitmentChangeIsDecidedIndependentlyAndApplied(t *testing.T) {
	w := newPurWorld(t)
	c := newPurChange(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.a46", "100", "offer.1")
	w.must(err)
	c.x.issued("commitment.1", d)
	_, err = c.x.p.confirm(w.author, "commitment.1", 3, "")
	w.must(err)
	agent := w.other("change.agent", kernel.ActorAI, allPermissions...)
	cmd := c.prepareCmd("change.1", "commitment.1", 1, changeLine("110", "offer.1", "2027-01-20"))
	r, err := w.pur.PrepareCommitmentChange(agent, cmd)
	w.must(err)
	if r.Status != purapi.ChangeDraft || r.RowVersion != 1 || r.Replayed || r.ObjectID != "change.1" || r.CommitmentID != "commitment.1" ||
		r.BaseRevision != 1 || r.AppliedRevision != 0 || len(r.ChangeHash) != 64 {
		t.Fatalf("prepare %+v", r)
	}
	if again, err := w.pur.PrepareCommitmentChange(agent, cmd); err != nil || !again.Replayed || again.ActionID != r.ActionID {
		t.Fatalf("replay %+v, %v", again, err)
	}
	cmd.IdempotencyKey = w.key()
	if answered, err := w.pur.PrepareCommitmentChange(agent, cmd); err != nil || !answered.Replayed || answered.ActionID == r.ActionID {
		t.Fatalf("answered from the stored change %+v, %v", answered, err)
	}
	other := cmd
	other.IdempotencyKey, other.Lines = w.key(), []purapi.ChangeLine{changeLine("111", "offer.1", "2027-01-20")}
	_, err = w.pur.PrepareCommitmentChange(agent, other)
	w.want(err, kernel.CodeVersionConflict)

	v := c.view(w.reviewer, "change.1")
	if v.Status != purapi.ChangeDraft || v.BaseRevision != 1 || v.ChangeHash != r.ChangeHash || v.PreparedBy != actorRef("ai", "change.agent") ||
		len(v.Lines) != 1 || v.Lines[0].PurchaseQuantity != "110" || v.Lines[0].SupplierDate != "2027-01-20" || len(v.Evidence) != 1 ||
		!slices.Equal(v.Deviations, []purapi.DeviationView{{OrderLineID: "L1", Kind: purapi.DeviationDeliveryChanged}}) ||
		!strings.Contains(v.Document, `"contract":"brant.purchase-commitment-revision/0.1.0"`) || !strings.Contains(v.Document, `"condition_date":"2027-01-15"`) ||
		!strings.Contains(v.Document, `"base_revision":1`) {
		t.Fatalf("change view %+v", v)
	}
	if got := c.line(w.author, "commitment.1"); got != "100 0 100 0 2027-01-15" {
		t.Fatalf("a prepared change changes nothing yet: %s", got)
	}

	// Only an independent human decides (humanOnlyCommands; independence from the requester).
	_, err = c.approve(agent, "change.1", 1, r.ChangeHash)
	w.want(err, kernel.CodePermissionDenied)
	ack := purapi.Acknowledgement{OrderLineID: "L1", Kind: purapi.DeviationDeliveryChanged, Reason: "合成：工厂来函，交期延后 5 天仍早于订单交期"}
	_, err = c.approve(w.author, "change.1", 1, r.ChangeHash, ack)
	w.want(err, purapi.CodeIndependentReviewRequired)
	_, err = c.approve(w.reviewer, "change.1", 1, r.ChangeHash)
	w.want(err, purapi.CodePurchaseDeviationUnacknowledged)
	_, err = c.approve(w.reviewer, "change.1", 1, strings.Repeat("0", 64), ack)
	w.want(err, purapi.CodeReferenceStale)
	a, err := c.approve(w.reviewer, "change.1", 1, r.ChangeHash, ack)
	w.must(err)
	if a.Status != purapi.ChangeApproved || a.RowVersion != 2 {
		t.Fatalf("approve %+v", a)
	}

	// The purchaser applies it: revision 2, issued again.
	_, err = c.apply(agent, "change.1", 2)
	w.want(err, kernel.CodePermissionDenied)
	_, err = c.apply(w.author, "change.1", 1)
	w.want(err, kernel.CodeVersionConflict)
	ap, err := c.apply(w.author, "change.1", 2)
	w.must(err)
	if ap.Status != purapi.ChangeApplied || ap.RowVersion != 3 || ap.AppliedRevision != 2 {
		t.Fatalf("apply %+v", ap)
	}
	_, err = c.apply(w.author, "change.1", 3)
	w.want(err, kernel.CodeInvalidState)
	cv := c.x.p.view(w.reviewer, "commitment.1")
	if cv.Status != purapi.CommitmentIssued || cv.Revision != 2 || cv.RowVersion != 5 || cv.Fulfilled || len(cv.Revisions) != 1 ||
		cv.Revisions[0].Kind != purapi.RevisionChange || cv.Revisions[0].ChangeID != "change.1" || cv.Revisions[0].OrderRevision != 1 ||
		cv.Revisions[0].DocumentHash != r.ChangeHash || cv.Revisions[0].CreatedBy.ID != "editor" {
		t.Fatalf("commitment %+v", cv)
	}
	if got := c.line(w.author, "commitment.1"); got != "110 0 110 0 2027-01-20" {
		t.Fatalf("line at revision 2: %s", got)
	}
	al := w.allocation()
	if al.Allocated != "110" || al.Remaining != "10" || len(al.Entries) != 2 || al.Entries[0].Effective || al.Entries[0].Quantity != "100" ||
		!al.Entries[1].Effective || al.Entries[1].Quantity != "110" {
		t.Fatalf("allocation %+v", al)
	}
	if got := c.x.p.downstream("company.A", "order.1"); !slices.Equal(got, []string{"demand_allocation L1  L1 1 allocated 110 ",
		"purchase_commitment commitment.1 purchase.a46  1 issued  "}) {
		t.Fatalf("downstream %q", got)
	}
	subjects, err := w.pur.ResolveCommitmentLines(w.author, []purapi.CommitmentLineRef{{CommitmentID: "commitment.1", OrderLineID: "L1"}})
	w.must(err)
	if s := subjects[purapi.CommitmentLineRef{CommitmentID: "commitment.1", OrderLineID: "L1"}]; s.PurchaseQuantity != "110" ||
		s.AllocatedQuantity != "110" || s.SupplierDate != "2027-01-20" || s.Code != "" || s.RowVersion != 5 {
		t.Fatalf("inspection subject %+v", s)
	}

	// The supplier confirms the revision in force.
	confirm := purapi.RecordSupplierConfirmation{IdempotencyKey: w.key(), CommitmentID: "commitment.1", ExpectedVersion: 5, Revision: 1,
		Result: purapi.ConfirmationAsIssued, ReceivedAt: kernel.FormatTimestamp(time.Now()), Evidence: purReceived}
	_, err = w.pur.RecordSupplierConfirmation(w.author, confirm)
	w.want(err, purapi.CodeReferenceStale)
	confirm.IdempotencyKey, confirm.Revision = w.key(), 2
	cr, err := w.pur.RecordSupplierConfirmation(w.author, confirm)
	w.must(err)
	if cr.Status != purapi.CommitmentConfirmed || cr.RowVersion != 6 || cr.Revision != 2 {
		t.Fatalf("confirmation of revision 2 %+v", cr)
	}
	if cv = c.x.p.view(w.author, "commitment.1"); cv.Confirmations[len(cv.Confirmations)-1].CommitmentRevision != 2 ||
		cv.Confirmations[0].CommitmentRevision != 1 {
		t.Fatalf("confirmations %+v", cv.Confirmations)
	}

	events := c.payloads(purapi.EventPurchaseCommitmentChanged)
	if len(events) != 1 || events[0]["commitment_id"] != "commitment.1" || events[0]["change_id"] != "change.1" || events[0]["revision"] != float64(2) ||
		events[0]["base_revision"] != float64(1) || events[0]["commitment_status"] != "issued" || events[0]["fulfilled"] != false ||
		events[0]["aggregate_version"] != float64(5) || events[0]["aggregate_type"] != "PurchaseCommitment" || events[0]["applied_by"] != "editor" ||
		!strings.HasPrefix(events[0]["po_number"].(string), "PO-") {
		t.Fatalf("changed event %v", events)
	}
	if lines, _ := events[0]["lines"].([]any); len(lines) != 1 || lines[0].(map[string]any)["purchase_quantity"] != "110" ||
		lines[0].(map[string]any)["supplier_date"] != "2027-01-20" {
		t.Fatalf("changed event lines %v", events[0]["lines"])
	}

	// A change bound to the old revision is stale; one restating the revision in force changes nothing.
	_, err = c.prepare(w.author, "change.2", "commitment.1", 1, changeLine("105", "offer.1", ""))
	refusedAt(t, "a change of revision 1", err, purapi.CodePurchaseCommitmentChangeStale, "/args/revision")
	_, err = c.prepare(w.author, "change.3", "commitment.1", 2, changeLine("110", "offer.1", "2027-01-20"))
	w.want(err, kernel.CodeInvalidKnownValue)
	list, err := w.pur.GetCommitmentChanges(w.reviewer, "commitment.1")
	w.must(err)
	if len(list) != 1 || list[0].ChangeID != "change.1" || list[0].Status != purapi.ChangeApplied || list[0].AppliedBy.ID != "editor" ||
		list[0].DecidedBy.ID != "reviewer" || len(list[0].Acknowledgements) != 1 {
		t.Fatalf("changes %+v", list)
	}
}

// Role templates' independence row "purchase.commitment.change.decide independentOf 变更的编制人与申请人" and
// SM-PUR-COMMITMENT-CHANGE draft → rejected ("理由必填"): a change decided by its preparer, or by the
// commitment's requester, is INDEPENDENT_REVIEW_REQUIRED, also for the database; a rejection needs a
// reason and ends the change, which is never applied.
func TestCommitmentChangeDecidedByItsPreparerIsRefused(t *testing.T) {
	w := newPurWorld(t)
	c := newPurChange(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.a46", "100", "offer.1")
	w.must(err)
	c.x.issued("commitment.1", d)
	buyer := w.other("buyer", kernel.ActorHuman, allPermissions...)
	r, err := c.prepare(buyer, "change.1", "commitment.1", 1, changeLine("120", "offer.1", ""))
	w.must(err)
	_, err = c.approve(buyer, "change.1", 1, r.ChangeHash)
	refusedAt(t, "the preparer deciding", err, purapi.CodeIndependentReviewRequired, "/args/change_id")
	_, err = c.approve(w.author, "change.1", 1, r.ChangeHash)
	refusedAt(t, "the commitment's requester deciding", err, purapi.CodeIndependentReviewRequired, "/args/change_id")
	if got := sqlCode(w.purOwner("UPDATE brant_pur.purchase_commitment_change SET status = 'approved', row_version = 2, decided_by_kind = 'human', " +
		"decided_by_id = 'editor', decide_epoch = 1, decided_at = now(), decision = 'approve', acknowledgements = '[]', updated_at = now() " +
		"WHERE change_id = 'change.1'")); got != "23000" {
		t.Fatalf("the database refuses the requester's decision: %s", got)
	}
	_, err = w.pur.DecideCommitmentChange(w.reviewer, purapi.DecideCommitmentChange{IdempotencyKey: w.key(), ChangeID: "change.1", ExpectedVersion: 1,
		ChangeHash: r.ChangeHash, Decision: purapi.DecisionReject})
	refusedAt(t, "a rejection without a reason", err, kernel.CodeSchemaInvalid, "/args/reason")
	rj, err := w.pur.DecideCommitmentChange(w.reviewer, purapi.DecideCommitmentChange{IdempotencyKey: w.key(), ChangeID: "change.1",
		ExpectedVersion: 1, ChangeHash: r.ChangeHash, Decision: purapi.DecisionReject, Reason: "合成：增量未经客户确认"})
	w.must(err)
	if rj.Status != purapi.ChangeRejected || rj.RowVersion != 2 {
		t.Fatalf("reject %+v", rj)
	}
	_, err = c.apply(w.author, "change.1", 2)
	w.want(err, kernel.CodeInvalidState)
	_, err = c.approve(w.reviewer, "change.1", 2, r.ChangeHash)
	w.want(err, kernel.CodeInvalidState)
	if cv := c.x.p.view(w.author, "commitment.1"); cv.Revision != 1 || cv.RowVersion != 3 {
		t.Fatalf("a rejected change changes nothing %+v", cv)
	}
	if v := c.view(w.author, "change.1"); v.Decision != purapi.DecisionReject || v.DecisionReason == "" || v.DecidedBy.ID != "reviewer" {
		t.Fatalf("rejected change %+v", v)
	}
}

// PUR-INV-011 negative "增量超过分配余额返回 PURCHASE_ALLOCATION_EXCEEDED" with PUR-INV-002: the commitment of 100
// may grow by what is left of the line's 120 — 121 is refused when prepared; 120 is prepared and
// approved, but another draft takes 15 of the remainder before it is applied, and the application
// is refused on the allocation as it is then; a smaller increase still fits.
func TestCommitmentChangeIncreaseNeedsAllocationRoom(t *testing.T) {
	w := newPurWorld(t)
	c := newPurChange(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.a46", "100", "offer.1")
	w.must(err)
	c.x.issued("commitment.1", d)
	_, err = c.prepare(w.author, "change.over", "commitment.1", 1, changeLine("121", "offer.1", ""))
	refusedAt(t, "121 of 120", err, purapi.CodePurchaseAllocationExceeded, "/args/lines/0/quantity")
	c.approved("change.120", "commitment.1", 1, changeLine("120", "offer.1", ""))
	moq1 := w.variant(func(l *supapi.ConditionLine) { l.MOQ.Quantity = "1" })
	_, err = w.pur.PrepareDraft(w.author, w.cmd("draft.b", purLine("15", purSource(moq1))))
	w.must(err)
	_, err = c.apply(w.author, "change.120", 2)
	refusedAt(t, "applied after the room was taken", err, purapi.CodePurchaseAllocationExceeded, "/change/lines/0/quantity")
	if got := c.line(w.author, "commitment.1"); got != "100 0 100 0 2027-01-15" {
		t.Fatalf("the refused application changes nothing: %s", got)
	}
	c.changed("change.105", "commitment.1", 1, changeLine("105", "offer.1", ""))
	if al := w.allocation(); al.Allocated != "120" || al.Remaining != "0" {
		t.Fatalf("allocation %+v", al)
	}
	if got := c.line(w.author, "commitment.1"); got != "105 0 105 0 2027-01-15" {
		t.Fatalf("line: %s", got)
	}
	_, err = c.apply(w.author, "change.120", 2)
	w.want(err, purapi.CodePurchaseCommitmentChangeStale)
}

// PUR-INV-011 negative "减至低于已核实交付量返回 SUPPLIER_DELIVERY_EXCEEDED" and its rule 已核实交付量不被变更或取消抹去
// (PUR-INV-008): with 100 of 120 verified delivered, a change to 90 is refused when prepared; a change
// to 100 is approved, but another 10 are verified before it is applied, and the application is
// refused; the database refuses a revision below the verified deliveries on its own.
func TestCommitmentChangeNeverBuysLessThanVerified(t *testing.T) {
	w := newPurWorld(t)
	c := newPurChange(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.must(err)
	c.x.issued("commitment.1", d)
	merch := w.other("merchandiser", kernel.ActorHuman, allPermissions...)
	_, err = c.x.record(merch, "delivery.1", "commitment.1", "100")
	w.must(err)
	_, err = c.x.verify(w.author, "delivery.1", 1, "100")
	w.must(err)
	_, err = c.prepare(w.author, "change.90", "commitment.1", 1, changeLine("90", "offer.1", ""))
	refusedAt(t, "90 below 100 verified", err, purapi.CodeSupplierDeliveryExceeded, "/args/lines/0/quantity")
	c.approved("change.100", "commitment.1", 1, changeLine("100", "offer.1", ""))
	_, err = c.x.record(merch, "delivery.2", "commitment.1", "10")
	w.must(err)
	_, err = c.x.verify(w.author, "delivery.2", 1, "10")
	w.must(err)
	_, err = c.apply(w.author, "change.100", 2)
	refusedAt(t, "100 below 110 verified", err, purapi.CodeSupplierDeliveryExceeded, "/change/lines/0/quantity")
	if got := c.line(w.author, "commitment.1"); got != "120 0 120 110 2027-01-15" {
		t.Fatalf("line: %s", got)
	}
	// The database's own check at commit: a revision line below the verified deliveries.
	err = w.purOwner(
		"INSERT INTO brant_pur.purchase_commitment_revision (tenant_id, company_id, commitment_id, revision, kind, order_revision, order_hash, "+
			"line_count, created_by_kind, created_by_id, created_epoch, created_at) SELECT tenant_id, company_id, commitment_id, 2, 'cancel', "+
			"order_revision, order_hash, 1, 'human', 'intruder', 1, now() FROM brant_pur.purchase_commitment WHERE commitment_id = 'commitment.1'",
		"INSERT INTO brant_pur.purchase_commitment_revision_line (tenant_id, company_id, commitment_id, revision, order_line_id, allocated_quantity, "+
			"overbuy_quantity, purchase_quantity) SELECT tenant_id, company_id, commitment_id, 2, 'L1', 50, 0, 50 FROM brant_pur.purchase_commitment "+
			"WHERE commitment_id = 'commitment.1'")
	if got := sqlCode(err); got != "23000" || !strings.Contains(err.Error(), "PUR-INV-011") {
		t.Fatalf("the database's check: %s (%v)", got, err)
	}
}

// SM-PUR-COMMITMENT-CHANGE approved → applied "承诺修订未变化（PURCHASE_COMMITMENT_CHANGE_STALE）" and PUR-INV-011
// concurrency "两个变更同时应用只有一个成功": each round two approved changes of the revision in force race
// their applications and exactly one wins — the other finds the revision moved; one change applied
// twice at once is applied once; the allocation is the winner's.
func TestCommitmentChangeAppliesRaceOnce(t *testing.T) {
	w := newPurWorld(t)
	c := newPurChange(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.race", "100", "offer.1")
	w.must(err)
	c.x.issued("commitment.1", d)
	revision := int64(1)
	for round, pair := range [][2]string{{"110", "105"}, {"101", "102"}, {"103", "104"}} {
		a, b := "change."+string(rune('a'+round))+".1", "change."+string(rune('a'+round))+".2"
		c.approved(a, "commitment.1", revision, changeLine(pair[0], "offer.1", ""))
		c.approved(b, "commitment.1", revision, changeLine(pair[1], "offer.1", ""))
		got := raceTwo(func() error { _, err := c.apply(w.author, a, 2); return err },
			func() error { _, err := c.apply(w.author, b, 2); return err })
		if !slices.Contains(got, "OK") || !slices.Contains(got, purapi.CodePurchaseCommitmentChangeStale) {
			t.Fatalf("round %d, two applications: %v", round, got)
		}
		revision++
		won := pair[0]
		if got[1] == "OK" {
			won = pair[1]
		}
		if line := c.line(w.author, "commitment.1"); line != won+" 0 "+won+" 0 2027-01-15" {
			t.Fatalf("round %d: %s after %v", round, line, got)
		}
		if al := w.allocation(); al.Allocated != won {
			t.Fatalf("round %d allocation %+v", round, al)
		}
		if cv := c.x.p.view(w.author, "commitment.1"); cv.Revision != revision {
			t.Fatalf("round %d revision %d", round, cv.Revision)
		}
	}
	c.approved("change.twice", "commitment.1", revision, changeLine("120", "offer.1", ""))
	got := raceTwo(func() error { _, err := c.apply(w.author, "change.twice", 2); return err },
		func() error { _, err := c.apply(w.author, "change.twice", 2); return err })
	if !slices.Contains(got, "OK") || !slices.Contains(got, kernel.CodeVersionConflict) {
		t.Fatalf("one change applied twice: %v", got)
	}
	if cv := c.x.p.view(w.author, "commitment.1"); cv.Revision != revision+1 {
		t.Fatalf("applied once: revision %d", cv.Revision)
	}
}

// A change racing a delivery verification (PUR-INV-011 with PUR-INV-008 on the commitment root, B3's
// lock order delivery → commitment), both commit orders: 100 of 120 are verified and a claim of 15
// waits; a change to 105 is approved. When the application commits first, the verification that
// waited for the commitment root finds 105 bought and is SUPPLIER_DELIVERY_EXCEEDED; when the
// verification commits first, the application that waited finds 115 verified and is refused.
func TestCommitmentChangeRacesADeliveryVerification(t *testing.T) {
	setup := func() (*purWorld, *purChange, context.Context) {
		w := newPurWorld(t)
		c := newPurChange(w.scenWorld, func() *pur.Module { return w.pur })
		d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
		w.must(err)
		c.x.issued("commitment.1", d)
		merch := w.other("merchandiser", kernel.ActorHuman, allPermissions...)
		_, err = c.x.record(merch, "delivery.1", "commitment.1", "100")
		w.must(err)
		_, err = c.x.verify(w.author, "delivery.1", 1, "100")
		w.must(err)
		_, err = c.x.record(merch, "delivery.2", "commitment.1", "15")
		w.must(err)
		c.approved("change.105", "commitment.1", 1, changeLine("105", "offer.1", ""))
		return w, c, merch
	}

	w, c, _ := setup()
	apply := c.applyCmd("change.105", 2)
	one, two := w.blockedBehind(t, purapi.CommandApplyCommitmentChange, apply.IdempotencyKey,
		func() error { _, err := w.pur.ApplyCommitmentChange(w.author, apply); return err },
		func() error { _, err := c.x.verify(w.reviewer, "delivery.2", 1, "15"); return err })
	if one != nil || code(two) != purapi.CodeSupplierDeliveryExceeded {
		t.Fatalf("the change first: change %v, verification %s", one, code(two))
	}
	if got := c.line(w.author, "commitment.1"); got != "105 0 105 100 2027-01-15" {
		t.Fatalf("the change first: %s", got)
	}

	o, oc, _ := setup()
	verify := oc.x.verifyCmd("delivery.2", 1, "15")
	one, two = o.blockedBehind(t, purapi.CommandVerifySupplierDelivery, verify.IdempotencyKey,
		func() error { _, err := o.pur.VerifySupplierDelivery(o.reviewer, verify); return err },
		func() error { _, err := oc.apply(o.author, "change.105", 2); return err })
	if one != nil || code(two) != purapi.CodeSupplierDeliveryExceeded {
		t.Fatalf("the verification first: verification %v, change %s", one, code(two))
	}
	if got := oc.line(o.author, "commitment.1"); got != "120 0 120 115 2027-01-15" {
		t.Fatalf("the verification first: %s", got)
	}
}

// SM-PUR-COMMITMENT issued | confirmed → cancelled ("授权人工、理由与供应商证据；释放分配"; "已核实交付量保留") and
// PUR-INV-011 "取消须授权人工、理由与供应商证据并释放分配；已核实交付量不被变更或取消抹去": of a confirmed commitment of
// 120 the supplier delivered 70, verified, and claims 20 more; the purchaser cancels it on the
// supplier's evidence. S5R-D41 (SM-PUR-COMMITMENT cancel guard, PUR-INV-011 "承诺有未核实的交付声明（recorded）时取消
// 被拒，须先核实或驳回该声明"; changed from S5 C11, where the pending claim was left unverifiable): while the
// claim of 20 awaits its verification the cancellation is INVALID_STATE, reason
// unverified_delivery_pending, and the commitment database refuses it too; the purchaser's colleague
// rejects the claim, and the cancellation passes. The line keeps 70 — the allocation of the other 50
// is released and another draft may take it —, the verified delivery stays usable by FUL's guard and
// STL's settlement, the draft stays committed, and DownstreamCommitments v1 shows only the 70
// allocated. PurchaseCommitmentCancelled is in the Outbox.
func TestCommitmentCancelReleasesTheUndeliveredAllocationAndKeepsTheVerified(t *testing.T) {
	w := newPurWorld(t)
	c := newPurChange(w.scenWorld, func() *pur.Module { return w.pur })
	d, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.must(err)
	c.x.issued("commitment.1", d)
	_, err = c.x.p.confirm(w.author, "commitment.1", 3, "")
	w.must(err)
	merch := w.other("merchandiser", kernel.ActorHuman, allPermissions...)
	_, err = c.x.record(merch, "delivery.1", "commitment.1", "70")
	w.must(err)
	_, err = c.x.verify(w.author, "delivery.1", 1, "70")
	w.must(err)
	_, err = c.x.record(merch, "delivery.2", "commitment.1", "20")
	w.must(err)
	c.approved("change.open", "commitment.1", 1, changeLine("110", "offer.1", ""))

	agent := w.other("cancel.agent", kernel.ActorAI, allPermissions...)
	_, err = c.cancel(agent, "commitment.1", 4)
	w.want(err, kernel.CodePermissionDenied)
	_, err = c.cancel(w.author, "commitment.1", 4)
	refusedAt(t, "a cancellation while a claim awaits its verification (S5R-D41)", err, kernel.CodeInvalidState, "/args/commitment_id")
	if !strings.HasPrefix(reasonOf(err), "unverified_delivery_pending: ") || !strings.Contains(reasonOf(err), "delivery.2") {
		t.Fatalf("the refusal names the pending claim: %s", reasonOf(err))
	}
	if status, version := c.x.p.state("commitment.1"); status != purapi.CommitmentConfirmed || version != 4 {
		t.Fatalf("nothing moved: %s %d", status, version)
	}
	if err = w.purOwner(`INSERT INTO brant_pur.purchase_commitment_revision (tenant_id, company_id, commitment_id, revision, kind, order_revision,
		order_hash, line_count, created_by_kind, created_by_id, created_epoch, created_at) SELECT tenant_id, company_id, commitment_id, 2, 'cancel',
		order_revision, order_hash, 1, 'human', 'editor', 1, now() FROM brant_pur.purchase_commitment WHERE commitment_id = 'commitment.1'`,
		`UPDATE brant_pur.purchase_commitment SET status = 'cancelled', revision = 2, row_version = 5, cancelled_by_kind = 'human',
		cancelled_by_id = 'editor', cancel_epoch = 1, cancelled_at = now(), cancel_reason = 'r', cancel_evidence = '[{"locator":"x","source_id":"evidence"}]'
		WHERE commitment_id = 'commitment.1'`); sqlCode(err) != "23000" || !strings.Contains(err.Error(), "changes only along SM-PUR-COMMITMENT") {
		t.Fatalf("the database refuses a cancellation with a pending claim: %v", err)
	}
	_, err = c.x.reject(w.author, "delivery.2", 1)
	w.must(err)
	_, err = w.pur.CancelCommitment(w.author, purapi.CancelCommitment{IdempotencyKey: w.key(), CommitmentID: "commitment.1", ExpectedVersion: 4,
		Reason: "合成：取消"})
	refusedAt(t, "a cancellation without evidence", err, kernel.CodeSchemaInvalid, "/args/evidence")
	_, err = w.pur.CancelCommitment(w.author, purapi.CancelCommitment{IdempotencyKey: w.key(), CommitmentID: "commitment.1", ExpectedVersion: 4,
		Reason: "合成：取消", Evidence: []purapi.Evidence{{SourceID: "no.such.source", Locator: "x"}}})
	refusedAt(t, "evidence nobody reads", err, kernel.CodeReferenceInvalid, "/args/evidence/0/source_id")
	_, err = c.cancel(w.author, "commitment.1", 3)
	w.want(err, kernel.CodeVersionConflict)
	r, err := c.cancel(w.author, "commitment.1", 4)
	w.must(err)
	if r.Status != purapi.CommitmentCancelled || r.RowVersion != 5 || r.Revision != 2 {
		t.Fatalf("cancel %+v", r)
	}
	cv := c.x.p.view(w.reviewer, "commitment.1")
	if cv.Status != purapi.CommitmentCancelled || cv.State != purapi.CommitmentCancelled || cv.Revision != 2 || cv.CancelledBy.ID != "editor" ||
		cv.CancelReason == "" || len(cv.CancelEvidence) != 1 || cv.Fulfilled || len(cv.Revisions) != 1 || cv.Revisions[0].Kind != purapi.RevisionCancel ||
		cv.Revisions[0].DocumentHash != "" || cv.CancelledAt.IsZero() {
		t.Fatalf("cancelled %+v", cv)
	}
	if got := c.line(w.author, "commitment.1"); got != "70 0 70 70 2027-01-15" {
		t.Fatalf("the line keeps what was verified: %s", got)
	}
	if al := w.allocation(); al.Allocated != "70" || al.Remaining != "50" {
		t.Fatalf("allocation %+v", al)
	}
	if got := c.x.p.downstream("company.A", "order.1"); !slices.Equal(got, []string{"demand_allocation L1  L1 1 allocated 70 "}) {
		t.Fatalf("downstream after the cancellation %q", got)
	}
	if g := c.x.guard("company.A", deliveryRef("delivery.1", 1)); !g.Usable() || g.VerifiedQuantity != "70" {
		t.Fatalf("the verified delivery stays a shipment source %+v", g)
	}
	sources, err := w.pur.ResolveSettlementSources(w.author, []purapi.DeliveryLineRef{deliveryRef("delivery.1", 1)})
	w.must(err)
	if s, ok := sources[deliveryRef("delivery.1", 1)]; !ok || s.Code != "" || s.LineVerifiedQuantity != "70" || s.Line.PurchaseQuantity != "70" ||
		s.Line.Code != kernel.CodeInvalidState {
		t.Fatalf("settlement source after the cancellation %+v", s)
	}
	_, err = c.x.verify(w.author, "delivery.2", 2, "20")
	w.want(err, kernel.CodeInvalidState) // rejected before the cancellation: nothing awaits verification
	_, err = c.cancel(w.author, "commitment.1", 5)
	w.want(err, kernel.CodeInvalidState)
	_, err = c.prepare(w.author, "change.after", "commitment.1", 2, changeLine("70", "offer.1", ""))
	w.want(err, kernel.CodeInvalidState)
	_, err = c.apply(w.author, "change.open", 2)
	w.want(err, kernel.CodeInvalidState)
	_, err = w.void(w.author, "purchase.a46", 1)
	w.want(err, purapi.CodePurchaseDraftCommitted)
	_, err = c.x.p.request(w.author, "commitment.2", "purchase.a46", d.DraftHash)
	w.want(err, purapi.CodePurchaseDraftCommitted)
	if dv := w.draft(w.author, "purchase.a46"); dv.CommitmentState != purapi.CommitmentCommitted {
		t.Fatalf("the draft stays committed %+v", dv)
	}
	moq1 := w.variant(func(l *supapi.ConditionLine) { l.MOQ.Quantity = "1" })
	_, err = w.pur.PrepareDraft(w.author, w.cmd("draft.rest", purLine("50", purSource(moq1))))
	w.must(err)
	events := c.payloads(purapi.EventPurchaseCommitmentCancelled)
	if len(events) != 1 || events[0]["commitment_id"] != "commitment.1" || events[0]["revision"] != float64(2) || events[0]["cancelled_by"] != "editor" ||
		events[0]["aggregate_version"] != float64(5) {
		t.Fatalf("cancelled event %v", events)
	}
	if lines, _ := events[0]["lines"].([]any); len(lines) != 1 || lines[0].(map[string]any)["verified_quantity"] != "70" ||
		lines[0].(map[string]any)["released_quantity"] != "50" {
		t.Fatalf("cancelled event lines %v", events[0]["lines"])
	}
	// A commitment delivered in nothing keeps nothing.
	o := newPurWorld(t)
	oc := newPurChange(o.scenWorld, func() *pur.Module { return o.pur })
	od, err := o.prepare(o.author, "purchase.none", "120", "offer.1")
	o.must(err)
	oc.x.issued("commitment.none", od)
	_, err = oc.cancel(o.author, "commitment.none", 3)
	o.must(err)
	if got := oc.x.p.downstream("company.A", "order.1"); len(got) != 0 {
		t.Fatalf("nothing open after cancelling an undelivered commitment %q", got)
	}
}

// SIM-02 steps 7 and 9 ("客户减量 10%：订单变更…"; "工厂只交 850 件（变更后 900 件），出运 850 件并短交关闭余量") with
// BT10 (commitment impacts traceable): the order of 1,000 PCS is amended to 900 at revision 2; the
// issued commitment of 1,000 is changed to 900 against that revision, re-confirmed by the supplier;
// the factory delivers 850, verified; the short receipt is a change to exactly 850, after which the
// commitment counts as fulfilled (state fulfilled in DownstreamCommitments v1) and the 50 left of the
// order line are free.
func TestCommitmentShortReceiptSIM02(t *testing.T) {
	w := newPurWorld(t)
	c := newPurChange(w.scenWorld, func() *pur.Module { return w.pur })
	w.orders.set("order.1", func(o *pur.Order) { o.Lines[0].Quantity = "1000" })
	d, err := w.prepare(w.author, "purchase.sim02", "1000", "offer.1")
	w.must(err)
	cost := purapi.Acknowledgement{OrderLineID: "L1", Kind: purapi.DeviationCostAbove, Reason: "合成：SIM-02 数量远超核价快照"}
	c.x.issued("commitment.sim02", d, cost)
	_, err = c.x.p.confirm(w.author, "commitment.sim02", 3, "")
	w.must(err)

	// Step 7: the customer reduces by 10 %; the order moves to revision 2 and the commitment follows it.
	amended := strings.Repeat("ef", 32)
	w.orders.set("order.1", func(o *pur.Order) {
		o.Binding.OrderRevision, o.Binding.OrderHash = 2, amended
		o.Lines[0].Quantity = "900"
	})
	_, err = c.prepare(w.author, "change.900", "commitment.sim02", 1, changeLine("1001", "offer.1", ""))
	refusedAt(t, "beyond the amended 900", err, purapi.CodePurchaseAllocationExceeded, "/args/lines/0/quantity")
	r := c.changed("change.900", "commitment.sim02", 1, changeLine("900", "offer.1", ""), cost)
	if r.AppliedRevision != 2 {
		t.Fatalf("change to 900 %+v", r)
	}
	if v := c.view(w.author, "change.900"); v.OrderRevision != 2 || v.OrderHash != amended {
		t.Fatalf("the change binds the amended order %+v", v)
	}
	if got := c.x.p.downstream("company.A", "order.1"); !slices.Equal(got, []string{"demand_allocation L1  L1 2 allocated 900 ",
		"purchase_commitment commitment.sim02 purchase.sim02  2 issued  "}) {
		t.Fatalf("downstream at 900 %q", got)
	}
	_, err = w.pur.RecordSupplierConfirmation(w.author, purapi.RecordSupplierConfirmation{IdempotencyKey: w.key(), CommitmentID: "commitment.sim02",
		ExpectedVersion: 5, Revision: 2, Result: purapi.ConfirmationAsIssued, ReceivedAt: kernel.FormatTimestamp(time.Now()), Evidence: purReceived})
	w.must(err)

	// Step 9: the factory delivers 850 of 900.
	merch := w.other("merchandiser", kernel.ActorHuman, allPermissions...)
	_, err = c.x.record(merch, "delivery.850", "commitment.sim02", "850")
	w.must(err)
	_, err = c.x.verify(w.author, "delivery.850", 1, "850")
	w.must(err)
	if got := c.line(w.author, "commitment.sim02"); got != "900 0 900 850 2027-01-15" {
		t.Fatalf("before the short receipt: %s", got)
	}
	if cv := c.x.p.view(w.author, "commitment.sim02"); cv.Fulfilled || cv.Status != purapi.CommitmentConfirmed {
		t.Fatalf("not fulfilled yet %+v", cv)
	}
	_, err = c.prepare(w.author, "change.short.849", "commitment.sim02", 2, changeLine("849", "offer.1", ""))
	refusedAt(t, "a short receipt below what was delivered", err, purapi.CodeSupplierDeliveryExceeded, "/args/lines/0/quantity")
	r = c.changed("change.short", "commitment.sim02", 2, changeLine("850", "offer.1", ""), cost)
	if r.AppliedRevision != 3 {
		t.Fatalf("short receipt %+v", r)
	}
	cv := c.x.p.view(w.author, "commitment.sim02")
	if !cv.Fulfilled || cv.Status != purapi.CommitmentIssued || cv.Revision != 3 || len(cv.Revisions) != 2 {
		t.Fatalf("fulfilled after the short receipt %+v", cv)
	}
	if got := c.line(w.author, "commitment.sim02"); got != "850 0 850 850 2027-01-15" {
		t.Fatalf("after the short receipt: %s", got)
	}
	if got := c.x.p.downstream("company.A", "order.1"); !slices.Equal(got, []string{"demand_allocation L1  L1 2 allocated 850 ",
		"purchase_commitment commitment.sim02 purchase.sim02  2 fulfilled  "}) {
		t.Fatalf("downstream after the short receipt %q", got)
	}
	if al := w.allocation(); al.Demand != "900" || al.Allocated != "850" || al.Remaining != "50" || al.OrderRevision != 2 {
		t.Fatalf("allocation %+v", al)
	}
	if g := c.x.guard("company.A", deliveryRef("delivery.850", 1)); !g.Usable() || g.VerifiedQuantity != "850" {
		t.Fatalf("FUL ships the 850 %+v", g)
	}
	events := c.payloads(purapi.EventPurchaseCommitmentChanged)
	if len(events) != 2 || events[1]["fulfilled"] != true || events[1]["revision"] != float64(3) || events[0]["order_revision"] != float64(2) {
		t.Fatalf("changed events %v", events)
	}
	if got := c.x.standing(w.author, "commitment.sim02"); got != "850 850 0" {
		t.Fatalf("deliveries standing %s", got)
	}
}

// The change and cancellation commands over HTTP with OIDC on TRD's real order (the shared order
// world): the purchaser prepares, the reviewer decides, the purchaser applies and later cancels;
// reconcile answers a sent application; the change's reads; the wire forms of the refusals.
func TestPurchaseCommitmentChangeOverHTTP(t *testing.T) {
	q := newOrderWorld(t)
	order := q.activeOrder("quote.1")
	c := newPurChange(q.scenWorld, func() *pur.Module { return q.c.PUR })
	d, err := q.c.PUR.PrepareDraft(q.author, purapi.PrepareDraft{IdempotencyKey: q.key(), DraftID: "purchase.http", OrderID: order.ObjectID,
		Lines: []purapi.DraftLine{purLine("100", purSource("offer.1"))}, SelectionReason: purReason})
	q.must(err)
	c.x.issued("commitment.http", d)
	is := newIssuer(t)
	handler, err := q.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	q.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	ed := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "editor", "human")}
	rv := &client{t: t, base: srv.URL, token: is.token(q.tenant, "company.A", "reviewer", "human"), n: 300}

	evidence := []any{map[string]any{"source_id": "evidence", "locator": "合成供应商变更确认函"}}
	prep := ed.command("pur", purapi.Contract, purapi.CommandPrepareCommitmentChange, map[string]any{"change_id": "change.http",
		"commitment_id": "commitment.http", "revision": 1, "reason": "合成：供应商同意增量与延期",
		"lines": []any{map[string]any{"order_line_id": "L1", "quantity": "110", "supplier_date": "2027-01-20",
			"source": map[string]any{"kind": "supplier_offer", "id": "offer.1", "revision": 1, "condition_line_id": "OL1"}}},
		"evidence": evidence})
	if prep["status"] != "draft" || prep["row_version"] != float64(1) || prep["object_id"] != "change.http" {
		t.Fatalf("prepare %v", prep)
	}
	status, v := ed.do("GET", "/api/brant/v1/pur/purchase_commitment_changes/change.http", nil)
	if status != http.StatusOK || v["status"] != "draft" || v["base_revision"] != float64(1) || len(v["change_hash"].(string)) != 64 ||
		v["deviations"].([]any)[0].(map[string]any)["kind"] != "delivery_changed" {
		t.Fatalf("change %d %v", status, v)
	}
	hash := v["change_hash"].(string)
	status, out := ed.try("pur", purapi.Contract, purapi.CommandDecideCommitmentChange, map[string]any{"change_id": "change.http",
		"expected_version": 1, "change_hash": hash, "decision": "approve"})
	if status != http.StatusUnprocessableEntity || out["code"] != purapi.CodeIndependentReviewRequired {
		t.Fatalf("the preparer deciding %d %v", status, out)
	}
	dec := rv.command("pur", purapi.Contract, purapi.CommandDecideCommitmentChange, map[string]any{"change_id": "change.http",
		"expected_version": 1, "change_hash": hash, "decision": "approve",
		"acknowledgements": []any{map[string]any{"order_line_id": "L1", "kind": "delivery_changed", "reason": "合成：仍早于订单交期"}}})
	if dec["status"] != "approved" || dec["row_version"] != float64(2) {
		t.Fatalf("decide %v", dec)
	}
	apply := map[string]any{"contract": purapi.Contract, "command": purapi.CommandApplyCommitmentChange, "idempotency_key": "pur.http.apply",
		"args": map[string]any{"change_id": "change.http", "expected_version": 2}}
	status, a := ed.do("POST", "/api/brant/v1/pur/commands/"+purapi.CommandApplyCommitmentChange, apply)
	if status != http.StatusOK || a["status"] != "applied" || a["row_version"] != float64(3) {
		t.Fatalf("apply %d %v", status, a)
	}
	status, rec := ed.do("POST", "/api/brant/v1/pur/commands/"+purapi.CommandApplyCommitmentChange+"/reconcile", apply)
	if status != http.StatusOK || rec["state"] != "committed" || rec["action_id"] != a["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	status, out = ed.try("pur", purapi.Contract, purapi.CommandPrepareCommitmentChange, map[string]any{"change_id": "change.stale",
		"commitment_id": "commitment.http", "revision": 1, "reason": "合成", "lines": []any{map[string]any{"order_line_id": "L1", "quantity": "105",
			"source": map[string]any{"kind": "supplier_offer", "id": "offer.1", "revision": 1, "condition_line_id": "OL1"}}}, "evidence": evidence})
	if status != http.StatusConflict || out["code"] != purapi.CodePurchaseCommitmentChangeStale {
		t.Fatalf("a stale change %d %v", status, out)
	}
	status, list := ed.do("GET", "/api/brant/v1/pur/purchase_commitments/commitment.http/changes", nil)
	if status != http.StatusOK || list["commitment_id"] != "commitment.http" || len(list["changes"].([]any)) != 1 {
		t.Fatalf("changes %d %v", status, list)
	}
	status, cv := ed.do("GET", "/api/brant/v1/pur/purchase_commitments/commitment.http", nil)
	if status != http.StatusOK || cv["revision"] != float64(2) || cv["status"] != "issued" ||
		cv["lines"].([]any)[0].(map[string]any)["purchase_quantity"] != "110" {
		t.Fatalf("commitment %d %v", status, cv)
	}
	can := ed.command("pur", purapi.Contract, purapi.CommandCancelCommitment, map[string]any{"commitment_id": "commitment.http",
		"expected_version": 4, "reason": "合成：客户取消", "evidence": evidence})
	if can["status"] != "cancelled" || can["row_version"] != float64(5) {
		t.Fatalf("cancel %v", can)
	}
	status, out = ed.do("GET", "/api/brant/v1/pur/purchase_commitment_changes/change.none", nil)
	if status != http.StatusNotFound || out["code"] != kernel.CodeNotFound {
		t.Fatalf("no such change %d %v", status, out)
	}
}
