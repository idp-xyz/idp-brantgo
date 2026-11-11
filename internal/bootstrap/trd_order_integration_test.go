//go:build integration

package bootstrap_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// The sales order suite (WBS S3-07, the order half of S3-13; TRD-INV-010, 013, 014, 016, 017, 021;
// ADR-BR-004, ADR-BR-013; SC-04, SC-07, BC-AT13, 15–17, TD-AT09) on the TRD world: "editor" (the
// author) drafts and activates the quotes, records the customer's replies, prepares the orders,
// requests their reviews and activates them; "reviewer" approves the quotes, verifies the replies
// and decides the order reviews — independent of the order's preparer and requester. Both are
// humans with every permission and company data scope unless a test provisions them otherwise. The
// customer customer.1 is screened clear against a synthetic CN list (PTY PartyScreeningGuard v1,
// which order.activate calls). Test names cite the Python V0.50 test they port.

// newOrderWorld is the TRD world with the customer's mailbox registered and customer.1 screened
// against a CN list holding entries (none matching: clear).
func newOrderWorld(t *testing.T, entries ...refapi.ScreeningEntry) *trdWorld {
	q := newTrdWorld(t)
	q.mail()
	if len(entries) == 0 {
		entries = []refapi.ScreeningEntry{listEntry("E1", "Globex Corporation")}
	}
	sum := sha256.Sum256([]byte("synthetic order screening list"))
	_, err := q.c.REF.ImportScreeningList(q.author, refapi.ImportScreeningList{IdempotencyKey: q.key(), ListCode: "cn.uel",
		Jurisdiction: refapi.JurisdictionCN, Version: "v1", SourceName: "Synthetic list cn.uel", SourceURL: "https://lists.example.org/cn.uel",
		PublishedOn: "2026-11-01", FileSHA256: hex.EncodeToString(sum[:]), EntryCount: len(entries), Entries: entries})
	q.must(err)
	reg := q.configure(q.author, q.reviewer, polapi.KindScreeningRegulation, "screening", 0, q.win.from, time.Time{},
		polapi.ConfigData{ScreeningRegulation: &polapi.ScreeningRegulation{Lists: []polapi.ScreeningList{cnList}, RescreenIntervalDays: 30}}, true)
	_, err = q.c.PTY.RecordScreening(q.author, ptyapi.RecordScreening{IdempotencyKey: q.key(), PartyID: "customer.1", ExpectedVersion: 0,
		RegulationID: reg.ConfigID, RegulationRevision: reg.Revision})
	q.must(err)
	return q
}

// prepareCmd prepares order of the revision acceptance a accepts.
func (q *trdWorld) prepareCmd(order string, a trdapi.AcceptanceReceipt) trdapi.PrepareOrder {
	return trdapi.PrepareOrder{IdempotencyKey: q.key(), OrderID: order, QuoteID: a.QuoteID, BusinessRevision: a.BusinessRevision,
		BusinessHash: a.BusinessHash, AcceptanceID: a.ObjectID}
}

// prepared activates and accepts quote id (accepted) and has the author prepare order "order.<id>".
func (q *trdWorld) prepared(id string) (trdapi.AcceptanceReceipt, trdapi.OrderReceipt) {
	q.t.Helper()
	_, a := q.accepted(id)
	o, err := q.c.TRD.PrepareOrder(q.author, q.prepareCmd("order."+id, a))
	q.must(err)
	return a, o
}

func (q *trdWorld) requestCmd(order string, expected int64, review string) trdapi.RequestOrderReview {
	return trdapi.RequestOrderReview{IdempotencyKey: q.key(), OrderID: order, ExpectedVersion: expected, ReviewID: review}
}

// decideCmd approves review of order at hash, valid for a day, acknowledging risks.
func (q *trdWorld) decideCmd(order string, expected int64, review, hash string, risks ...trdapi.RiskAcknowledgement) trdapi.DecideOrderReview {
	return trdapi.DecideOrderReview{IdempotencyKey: q.key(), OrderID: order, ExpectedVersion: expected, ReviewID: review, OrderHash: hash,
		Decision: trdapi.DecisionApprove, ExpiresAt: kernel.FormatTimestamp(time.Now().Add(24 * time.Hour)), RiskAcknowledgements: risks}
}

func (q *trdWorld) activateOrderCmd(order string, expected int64, review string) trdapi.ActivateOrder {
	return trdapi.ActivateOrder{IdempotencyKey: q.key(), OrderID: order, ExpectedVersion: expected, ReviewID: review}
}

// reviewed has the author request review of order o and the reviewer approve it acknowledging
// risks; it returns the decision's receipt.
func (q *trdWorld) reviewed(o trdapi.OrderReceipt, review string, risks ...trdapi.RiskAcknowledgement) trdapi.OrderReceipt {
	q.t.Helper()
	r, err := q.c.TRD.RequestOrderReview(q.author, q.requestCmd(o.ObjectID, q.orderVersion(o.ObjectID), review))
	q.must(err)
	d, err := q.c.TRD.DecideOrderReview(q.reviewer, q.decideCmd(o.ObjectID, r.RowVersion, review, o.OrderHash, risks...))
	q.must(err)
	return d
}

// activeOrder prepares order "order.<id>" of an accepted quote id, has it reviewed as
// "review.<id>" and activated.
func (q *trdWorld) activeOrder(id string) trdapi.OrderReceipt {
	q.t.Helper()
	_, o := q.prepared(id)
	d := q.reviewed(o, "review."+id)
	a, err := q.c.TRD.ActivateOrder(q.author, q.activateOrderCmd(o.ObjectID, d.RowVersion, "review."+id))
	q.must(err)
	return a
}

func (q *trdWorld) getOrder(id string) trdapi.OrderView {
	q.t.Helper()
	v, err := q.c.TRD.GetOrder(q.author, id)
	q.must(err)
	return v
}

func (q *trdWorld) orderVersion(id string) int64 { return q.getOrder(id).RowVersion }

// ack acknowledges risk r.
func ack(r trdapi.Risk) trdapi.RiskAcknowledgement {
	return trdapi.RiskAcknowledgement(r)
}

// risks returns the upstream risks order.check finds for order id now.
func (q *trdWorld) risks(id string) []trdapi.Risk {
	q.t.Helper()
	c, err := q.c.TRD.CheckOrder(q.reviewer, id, "")
	q.must(err)
	return c.Risks
}

// deny has CST deny the cost snapshot's supplier offer source after the contract formed (SC-04,
// SC-07).
func (q *trdWorld) deny() {
	q.t.Helper()
	_, err := q.c.CST.AttestSource(q.reviewer, q.denyCmd("src.1", q.counter()))
	q.must(err)
}

func (q *trdWorld) orderRows() (orders, reviews, commits int) {
	return q.count("company.A", "brant_trd.sales_order"), q.count("company.A", "brant_trd.sales_order_review"),
		q.count("company.A", "brant_trd.sales_order_commit_evidence")
}

// Python test_t03_a20_acceptance_order.py test_acceptance_is_version_bound_and_order_full_v1_is_idempotent
// (PYT-0166, the order half) and test_acceptance_and_order_queries_are_scoped (PYT-0169, the order
// half), test_t03_a25_order_snapshot.py test_complete_source_and_actual_terms_are_preserved
// (PYT-0203), test_nonzero_charges_lines_and_payment_amounts_are_exact (PYT-0204, on this world's
// single line), test_prepare_does_not_recalculate_an_approved_snapshot (PYT-0207),
// test_activation_changes_state_without_changing_commercial_snapshot (PYT-0208),
// test_local_public_replay_and_restart_keep_one_snapshot (PYT-0209) and
// test_snapshot_contract_and_order_identity_are_hash_bound (PYT-0227),
// test_t03_a26_order_qualification.py test_new_activation_records_qualification_and_keeps_order_commercial_hash
// (PYT-0243), test_t04_a38_order_review.py test_end_to_end_and_historical_replay (PYT-0479),
// test_t04_a41_public_review.py test_public_request_and_decision_commit_and_replay (PYT-0514) and
// test_t04_a42_public_review_query.py test_pending_approved_revoked_and_activated_projections
// (PYT-0519); TRD-INV-010, 013, 016, SM-TRD-ORDER: an AI agent prepares the order of the accepted
// revision (人工或 AI), taking the quote's conversion slot; the snapshot copies the sealed commercial
// document whole, with the acceptance it binds, and hashes to the order hash; an AI agent requests
// the review, the reviewer approves it, the author activates: the order is active with its commit
// evidence (the acceptance's arrival, the quote's expires_at as bound, the review, the risks
// acknowledged, the formalization approval, CST's verdict, the screening, the STL and CNX extension
// points not wired) and the events in order; every key replays; the reads and the lookup tell PUR
// what it binds.
func TestOrderPrepareReviewAndActivate(t *testing.T) {
	q := newOrderWorld(t)
	agent := q.actorOf(kernel.ActorAI, "agent.1")
	_, a := q.accepted("quote.1")
	prepare := q.prepareCmd("order.1", a)
	o, err := q.c.TRD.PrepareOrder(agent, prepare)
	if err != nil || o.ObjectID != "order.1" || o.RowVersion != 1 || o.Status != trdapi.OrderDraft || o.QuoteID != "quote.1" ||
		o.BusinessRevision != 1 || o.BusinessHash != a.BusinessHash || o.AcceptanceID != a.ObjectID || o.OrderRevision != 1 || len(o.OrderHash) != 64 {
		t.Fatalf("prepared %+v %v", o, err)
	}
	if again, err := q.c.TRD.PrepareOrder(agent, prepare); err != nil || !again.Replayed || again.ActionID != o.ActionID || again.OrderHash != o.OrderHash {
		t.Fatalf("the same key %+v %v", again, err)
	}
	quoteView := q.getQuote("quote.1")
	if c := quoteView.Conversion; c == nil || c.OrderID != "order.1" || c.Slot != "full_v1" || c.BusinessRevision != 1 || c.TakenBy.ID != "agent.1" {
		t.Fatalf("the conversion slot %+v", quoteView.Conversion)
	}
	v := q.getOrder("order.1")
	if v.Status != trdapi.OrderDraft || v.PreparedBy.ID != "agent.1" || v.Number != a.Number || !v.ArrivalAt.Equal(a.ArrivalAt) ||
		!v.QuoteExpiresAt.Equal(a.ExpiresAt) || sha256Hex(v.Snapshot) != v.OrderHash || v.OrderHash != o.OrderHash {
		t.Fatalf("the order %+v", v)
	}
	var snapshot map[string]any
	q.must(json.Unmarshal([]byte(v.Snapshot), &snapshot))
	commercial := quoteView.Revisions[0].Commercial
	var source map[string]any
	q.must(json.Unmarshal([]byte(commercial), &source))
	data := source["data"].(map[string]any)
	terms := snapshot["terms"].(map[string]any)
	sourceDoc, _ := json.Marshal(snapshot["source_quote_snapshot"])
	sealed, _ := json.Marshal(source)
	if snapshot["snapshot_contract"] != trdapi.OrderSnapshotContract || snapshot["order_id"] != "order.1" ||
		snapshot["conversion_slot"] != "full_v1" || snapshot["source_quote_hash"] != a.BusinessHash || snapshot["acceptance_id"] != a.ObjectID ||
		string(sourceDoc) != string(sealed) || !jsonEqual(snapshot["calculation"], source["calculation"]) ||
		!jsonEqual(snapshot["lines"], data["lines"]) || !jsonEqual(snapshot["charges"], data["charges"]) ||
		!jsonEqual(snapshot["contact_ref"], data["contact_ref"]) || !jsonEqual(terms["delivery"], data["delivery"]) ||
		terms["issued_at"] != source["issued_at"] || terms["expires_at"] != data["expires_at"] ||
		!jsonEqual(terms["payment_terms_ref"], data["payment_terms_ref"]) {
		t.Fatalf("the snapshot is not the sealed source:\n%s", v.Snapshot)
	}
	calc := snapshot["calculation"].(map[string]any)
	var payments int64
	for _, p := range calc["payments"].([]any) {
		payments += mustInt(t, p.(map[string]any)["amount_minor"])
	}
	if calc["net_minor"] != "96000" || payments != mustInt(t, calc["payable_minor"]) {
		t.Fatalf("the snapshot's amounts %v / %d", calc["net_minor"], payments)
	}

	request := q.requestCmd("order.1", 1, "review.1")
	r, err := q.c.TRD.RequestOrderReview(agent, request)
	if err != nil || r.Status != trdapi.OrderReviewOpen || r.RowVersion != 2 || r.ReviewID != "review.1" || r.OrderStatus != trdapi.OrderDraft {
		t.Fatalf("requested %+v %v", r, err)
	}
	if again, err := q.c.TRD.RequestOrderReview(agent, request); err != nil || !again.Replayed || again.ActionID != r.ActionID {
		t.Fatalf("the same key %+v %v", again, err)
	}
	if got := q.getOrder("order.1").Reviews; len(got) != 1 || got[0].DecisionState != "pending" || got[0].RequestedBy.ID != "agent.1" {
		t.Fatalf("pending %+v", got)
	}
	decide := q.decideCmd("order.1", 2, "review.1", o.OrderHash)
	d, err := q.c.TRD.DecideOrderReview(q.reviewer, decide)
	if err != nil || d.Status != trdapi.OrderReviewApproved || d.RowVersion != 3 || d.ReviewStatus != trdapi.OrderReviewApproved {
		t.Fatalf("approved %+v %v", d, err)
	}
	if again, err := q.c.TRD.DecideOrderReview(q.reviewer, decide); err != nil || !again.Replayed || again.ActionID != d.ActionID {
		t.Fatalf("the same key %+v %v", again, err)
	}
	if c, err := q.c.TRD.CheckOrder(q.reviewer, "order.1", "review.1"); err != nil || !c.Activatable || len(c.Risks) != 0 || c.Code != "" ||
		!slices.Equal(c.Unchecked, []string{trdapi.GuardCNXTradeCompliance, trdapi.GuardSTLCredit}) {
		t.Fatalf("checked %+v %v", c, err)
	}
	activate := q.activateOrderCmd("order.1", 3, "review.1")
	act, err := q.c.TRD.ActivateOrder(q.author, activate)
	if err != nil || act.Status != trdapi.OrderActive || act.RowVersion != 4 || act.ReviewID != "review.1" || act.OrderHash != o.OrderHash {
		t.Fatalf("activated %+v %v", act, err)
	}
	if again, err := q.c.TRD.ActivateOrder(q.author, activate); err != nil || !again.Replayed || again.ActionID != act.ActionID ||
		again.ReviewID != "review.1" {
		t.Fatalf("the same key %+v %v", again, err)
	}
	v = q.getOrder("order.1")
	if v.Status != trdapi.OrderActive || v.OrderHash != o.OrderHash || v.ActivatedReviewID != "review.1" || len(v.Commits) != 1 ||
		!v.Reviews[0].Activated || v.Reviews[0].DecisionState != trdapi.OrderReviewApproved {
		t.Fatalf("active %+v", v)
	}
	e := v.Commits[0].Entries
	for name, want := range map[string]string{"command": "order.activate", "idempotency_key": activate.IdempotencyKey, "order_id": "order.1",
		"order_hash": o.OrderHash, "business_hash": a.BusinessHash, "acceptance_id": a.ObjectID, "arrival_at": kernel.FormatTimestamp(a.ArrivalAt),
		"arrival_basis": trdapi.ArrivalChannelServer, "quote_expires_at": kernel.FormatTimestamp(a.ExpiresAt), "contract_formed": "true",
		"review_id": "review.1", "reviewer": "human:reviewer", "formalization_approval_id": "approval.quote.1", "risks_found": "[]",
		"risks_acknowledged": "0", "cost_snapshot_adoptability": cstapi.AdoptionAdoptable, trdapi.GuardSTLCredit: "not_wired",
		trdapi.GuardCNXTradeCompliance: "not_wired", "committed_by": "human:editor", "row_version": "4"} {
		if e[name] != want {
			t.Errorf("commit evidence %s = %q, want %q", name, e[name], want)
		}
	}
	if !strings.HasPrefix(e["screening:customer.1"], ptyapi.ScreeningClear+"/") {
		t.Errorf("the screening in the evidence: %q", e["screening:customer.1"])
	}
	var kinds []string
	for _, ev := range q.events("company.A", trdapi.Source) {
		if ev.Payload["order_id"] == "order.1" {
			kinds = append(kinds, strings.TrimPrefix(ev.Type, "idp-brant.trd.")+"@"+jsonString(ev.Payload["aggregate_version"]))
		}
	}
	if !slices.Equal(kinds, []string{"order_prepared.v1@1", "order_review_requested.v1@2", "order_review_decided.v1@3", "order_activated.v1@4"}) {
		t.Fatalf("events %v", kinds)
	}
	activated := q.eventsOf(trdapi.EventOrderActivated)[0].Payload
	if activated["unchecked_guards"] != trdapi.GuardCNXTradeCompliance+","+trdapi.GuardSTLCredit || activated["review_id"] != "review.1" ||
		activated["arrival_at"] != kernel.FormatTimestamp(a.ArrivalAt) {
		t.Fatalf("OrderActivated %v", activated)
	}
	facts, err := q.c.TRD.ResolveActiveOrders(q.author, []string{"order.1", "order.none"})
	f := facts["order.1"]
	if err != nil || len(facts) != 1 || f.Check("/x") != nil || f.Binding.OrderHash != o.OrderHash || f.Status != trdapi.OrderActive ||
		len(f.Lines) != 1 || f.Lines[0].LineID != "L1" || f.Lines[0].Quantity != "120" || f.Lines[0].NetMinor != "96000" ||
		f.CustomerRef.PartyID != "customer.1" || f.ReviewID != "review.1" || f.ActivatedAt.IsZero() ||
		f.RfqRef != (trdapi.RfqRef{RfqID: "rfq.1", RowVersion: q.rfqVersion}) || f.CostSnapshotRef.SnapshotID != q.snap.SnapshotID ||
		f.CostSnapshotRef.ResultHash != q.snap.ResultHash || f.Delivery.Date != handover.Date || f.Delivery.PlaceRef != dehamPlace {
		t.Fatalf("ActiveOrderRef %+v %v", facts, err)
	}
	for _, c := range []trdapi.Command{prepare, request, decide, activate} {
		if rec, err := q.c.TRD.Reconcile(q.author, c); err != nil || rec.State != kernel.ReconcileCommitted || rec.ObjectID != "order.1" {
			t.Fatalf("reconcile %T %+v %v", c, rec, err)
		}
	}
	if orders, reviews, commits := q.orderRows(); orders != 1 || reviews != 1 || commits != 1 {
		t.Fatalf("%d orders, %d reviews, %d commits", orders, reviews, commits)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func mustInt(t *testing.T, v any) int64 {
	t.Helper()
	s, _ := v.(string)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("%v is not an integer: %v", v, err)
	}
	return n
}

// BC-AT15, TRD-INV-010 and Python test_t03_a20 test_distinct_public_actions_cannot_bypass_full_v1_slot
// (PYT-0172) and test_t03_a25 test_two_local_preparations_share_one_snapshot_and_event (PYT-0210):
// two preparations of one root under different keys at once have one winner; the other is
// CONVERSION_SLOT_CONFLICT (Python's public adapter aliases it ROOT_CONVERSION_LOCKED); one order,
// one OrderPrepared. Another order of the root under any ID is CONVERSION_SLOT_CONFLICT; an order ID
// another quote's order has is BUSINESS_KEY_CONFLICT.
func TestOnePreparationPerRoot(t *testing.T) {
	q := newOrderWorld(t)
	_, a := q.accepted("quote.1")
	cmds := []trdapi.PrepareOrder{q.prepareCmd("order.1", a), q.prepareCmd("order.1", a), q.prepareCmd("order.other", a)}
	got := make([]string, len(cmds))
	var wg sync.WaitGroup
	for i, c := range cmds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := q.c.TRD.PrepareOrder(q.author, c)
			got[i] = code(err)
		}()
	}
	wg.Wait()
	if s := slices.Sorted(slices.Values(got)); !slices.Equal(s, []string{trdapi.CodeConversionSlotConflict, trdapi.CodeConversionSlotConflict, "OK"}) {
		t.Fatalf("three preparations of one root: %v", got)
	}
	if orders, _, _ := q.orderRows(); orders != 1 || len(q.eventsOf(trdapi.EventOrderPrepared)) != 1 || q.count("company.A", "brant_trd.quote_conversion_slot") != 1 {
		t.Fatalf("%d orders", orders)
	}
	taken := q.getQuote("quote.1").Conversion.OrderID
	_, b := q.accepted("quote.2")
	_, err := q.c.TRD.PrepareOrder(q.author, q.prepareCmd(taken, b))
	q.want(err, trdapi.CodeBusinessKeyConflict)
	if q.getQuote("quote.2").Conversion != nil {
		t.Fatal("a refused preparation took the slot")
	}
}

// TRD-INV-017 and 010 for the preparation, with Python test_t03_a20
// test_acceptance_is_version_bound_and_order_full_v1_is_idempotent (PYT-0166: an unknown acceptance
// is ACCEPTANCE_SCOPE_MISMATCH) and test_wrong_version_and_unapproved_activation_are_blocked (PYT-0168,
// the preparation half), test_t03_a24_write_control.py test_order_activation_does_not_bypass_source_quote_scope
// (PYT-0196, for the preparation): an order converts only a formed contract on the quote's active,
// unheld formal revision — an acceptance of another quote, another hash, an invalidated or disputed
// acceptance, a held revision and an outsider are refused, and nothing is written (no order, no
// slot, the root's version unmoved).
func TestPrepareConvertsOnlyAFormedContract(t *testing.T) {
	q := newOrderWorld(t)
	_, a := q.accepted("quote.1")
	_, b := q.accepted("quote.2")
	_, c := q.accepted("quote.3")
	before := q.getQuote("quote.1").RootVersion
	wrongHash := q.prepareCmd("order.x", a)
	wrongHash.BusinessHash = b.BusinessHash
	other := q.prepareCmd("order.x", a)
	other.AcceptanceID = b.ObjectID
	unknown := q.prepareCmd("order.x", a)
	unknown.AcceptanceID = "acceptance.none"
	outsider := q.regrant("outsider", "own", allPermissions)
	for name, c := range map[string]struct {
		ctx  context.Context
		cmd  trdapi.PrepareOrder
		code string
	}{
		"another hash":                     {q.author, wrongHash, trdapi.CodeBusinessHashMismatch},
		"another quote's acceptance":       {q.author, other, trdapi.CodeAcceptanceScopeMismatch},
		"an unknown acceptance (PYT-0166)": {q.author, unknown, trdapi.CodeAcceptanceScopeMismatch},
		"an outsider (PYT-0196)":           {outsider, q.prepareCmd("order.x", a), kernel.CodeNotFound},
	} {
		_, err := q.c.TRD.PrepareOrder(c.ctx, c.cmd)
		if code(err) != c.code {
			t.Errorf("%s: %s, want %s", name, code(err), c.code)
		}
	}
	_, err := q.c.TRD.InvalidateAcceptance(q.reviewer, q.invalidation(a.ObjectID, 1))
	q.must(err)
	_, err = q.c.TRD.PrepareOrder(q.author, q.prepareCmd("order.1", a))
	q.want(err, trdapi.CodeAcceptanceNotValid)
	_, err = q.c.TRD.DisputeAcceptance(q.author, q.dispute(b.ObjectID, 1))
	q.must(err)
	_, err = q.c.TRD.PrepareOrder(q.author, q.prepareCmd("order.2", b))
	q.want(err, trdapi.CodeAcceptanceNotValid)
	_, err = q.c.TRD.RevokeApproval(q.reviewer, trdapi.RevokeApproval{IdempotencyKey: q.key(), QuoteID: "quote.3", ApprovalID: "approval.quote.3",
		ExpectedEffectivityVersion: 1, Reason: "批准依据失效", Evidence: q.proof})
	q.must(err)
	_, err = q.c.TRD.PrepareOrder(q.author, q.prepareCmd("order.3", c))
	q.want(err, trdapi.CodeApprovalRevoked)
	if orders, _, _ := q.orderRows(); orders != 0 || q.count("company.A", "brant_trd.quote_conversion_slot") != 0 ||
		q.getQuote("quote.1").RootVersion != before || len(q.eventsOf(trdapi.EventOrderPrepared)) != 0 {
		t.Fatalf("a refused preparation left %d orders", orders)
	}
}
