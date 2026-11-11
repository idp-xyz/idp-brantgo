//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// PUR (WBS S3-10, S3-16) on PostgreSQL 16. The fixture is Python T04 A46's on the T02 costing
// fixture (newFrozenWorld): rfq.1 line L1, 120 PCS of sku.1; offer.1 of supplier.1, line OL1 at 5 USD
// per PCS, MOQ 100, delivered on 2027-01-15 at handover in DEHAM; the 660.00 USD snapshot (600.00
// purchase, 60.00 freight) the order's quote adopted; and order.1, active, whose line L1 is the
// RFQ's 120 PCS required by 2027-01-31 at the same place. The order comes through PUR's port to TRD
// (purOrders, an in-memory TRD: phase 1 of S3-10, before TRD's sales orders); every other upstream
// is the real context. Test names cite the Python test they port.

// purOrders is TRD as PUR's port sees it: the orders of one tenant's company.A, which the actors
// in blind do not see (TRD's data scope), and OrderGuard v1's verdict, by default the lookup's
// (another binding is REFERENCE_STALE) and overridden per order by verdicts. With hold set, the
// guard announces on held that PUR's transaction has passed every other guard and waits for hold
// to close.
type purOrders struct {
	mu       sync.Mutex
	tenant   string
	orders   map[string]pur.Order
	blind    map[string]bool
	verdicts map[string]pur.OrderVerdict
	hold     chan struct{}
	held     chan struct{}
	guarded  int
}

func (f *purOrders) ResolveOrders(ctx context.Context, ids []string) (map[string]pur.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]pur.Order{}
	p, err := identity.FromContext(ctx)
	if err != nil || string(p.Scope.Tenant) != f.tenant || p.Scope.Company != "company.A" || f.blind[p.Actor.ID] {
		return out, err
	}
	for _, id := range ids {
		if o, ok := f.orders[id]; ok {
			out[id] = o
		}
	}
	return out, nil
}

func (f *purOrders) GuardOrder(ctx context.Context, exec bentopg.Executor, b pur.OrderBinding) (pur.OrderVerdict, error) {
	// The guard runs on PUR's own executor, inside PUR's transaction and its scope.
	var tenant, company string
	if err := exec.QueryRow(ctx, `SELECT current_setting('app.tenant_id', true), current_setting('app.company_id', true)`).
		Scan(&tenant, &company); err != nil {
		return pur.OrderVerdict{}, err
	}
	f.mu.Lock()
	f.guarded++
	hold, held := f.hold, f.held
	o, ok := f.orders[b.OrderID]
	v, override := f.verdicts[b.OrderID]
	f.mu.Unlock()
	if hold != nil {
		held <- struct{}{}
		<-hold
	}
	switch {
	case tenant != f.tenant || company != "company.A" || !ok:
		return pur.OrderVerdict{}, nil
	case override:
		return v, nil
	}
	v = pur.OrderVerdict{Known: true, Status: o.Status, Code: o.Code}
	if v.Code == "" && o.Binding != b {
		v.Code = pur.OrderStale
	}
	return v, nil
}

func (f *purOrders) set(id string, edit func(o *pur.Order)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orders[id]
	edit(&o)
	f.orders[id] = o
}

func (f *purOrders) verdict(id string, v pur.OrderVerdict) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verdicts[id] = v
}

// purHook runs fn once, right after PUR's last lookup (CST's snapshot) and before PUR's transaction
// begins: what fn commits upstream is seen by the guards and not by the lookups.
type purHook struct {
	cstapi.SnapshotLookups
	mu sync.Mutex
	fn func()
}

func (h *purHook) ResolveSnapshots(ctx context.Context, refs []cstapi.CostSnapshotRef, at time.Time) (map[cstapi.CostSnapshotRef]cstapi.ResolvedSnapshot, error) {
	out, err := h.SnapshotLookups.ResolveSnapshots(ctx, refs, at)
	h.mu.Lock()
	fn := h.fn
	h.fn = nil
	h.mu.Unlock()
	if fn != nil {
		fn()
	}
	return out, err
}

func (h *purHook) then(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fn = fn
}

type purWorld struct {
	*scenWorld
	snapshot cstapi.SnapshotReceipt
	orders   *purOrders
	hook     *purHook
	pur      *pur.Module
}

var (
	purOrderHash  = strings.Repeat("ab", 32)
	purReason     = "Synthetic costed supplier condition"
	purVoidReason = "Wrong supply condition chosen"
)

func newPurWorld(t *testing.T) *purWorld {
	w, f, _ := newFrozenWorld(t)
	p := &purWorld{scenWorld: w, snapshot: f, orders: &purOrders{tenant: w.tenant, orders: map[string]pur.Order{}, blind: map[string]bool{},
		verdicts: map[string]pur.OrderVerdict{}}}
	p.orders.orders["order.1"] = p.order("order.1")
	p.hook = &purHook{SnapshotLookups: w.c.CST}
	p.pur = p.module()
	return p
}

// module wires a PUR of its own on a new pool, as a restarted process would.
func (w *purWorld) module() *pur.Module {
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	w.must(err)
	m, err := pur.New(db, pur.Upstream{Orders: w.orders, Supply: w.c.SUP, Snapshots: w.hook, Parties: w.c.PTY, Configs: w.c.POL, Skus: w.c.CAT})
	w.must(err)
	return m
}

// order is the active order the fixture's quote converted, as TRD's ActiveOrderRef v1 shows it.
func (w *purWorld) order(id string) pur.Order {
	return pur.Order{Binding: pur.OrderBinding{OrderID: id, OrderRevision: 1, OrderHash: purOrderHash}, Status: "active", QuoteID: "quote.1",
		BusinessRevision: 1, BusinessHash: strings.Repeat("cd", 32), ReviewID: "review.order", CustomerID: "customer.1",
		Rfq:          &pur.OrderRfq{RfqID: "rfq.1", RowVersion: w.rfqVersion},
		CostSnapshot: &pur.OrderSnapshot{SnapshotID: w.snapshot.SnapshotID, ResultHash: w.snapshot.ResultHash},
		Delivery:     &pur.OrderDelivery{Kind: "fixed", Date: "2027-01-31", Milestone: "handover", PlaceID: "DEHAM", TimeZone: "Asia/Shanghai"},
		Lines: []pur.OrderLine{{LineID: "L1", RfqLineID: "L1", SkuID: "sku.1", SpecRevision: w.spec, Quantity: "120",
			Unit: pur.OrderUnit{ID: "PCS", Revision: w.pcs.Revision}}}}
}

func purSource(id string) purapi.SourceRef {
	return purapi.SourceRef{Kind: purapi.KindSupplierOffer, ID: id, Revision: 1, ConditionLineID: "OL1"}
}

func purLine(q string, src purapi.SourceRef) purapi.DraftLine {
	return purapi.DraftLine{OrderLineID: "L1", Quantity: q, Source: src}
}

func (w *purWorld) cmd(id string, lines ...purapi.DraftLine) purapi.PrepareDraft {
	return purapi.PrepareDraft{IdempotencyKey: w.key(), DraftID: id, OrderID: "order.1", Lines: lines, SelectionReason: purReason}
}

func (w *purWorld) prepare(ctx context.Context, id, q, offerID string) (purapi.DraftReceipt, error) {
	return w.pur.PrepareDraft(ctx, w.cmd(id, purLine(q, purSource(offerID))))
}

func (w *purWorld) void(ctx context.Context, id string, expected int64) (purapi.DraftReceipt, error) {
	return w.pur.VoidDraft(ctx, purapi.VoidDraft{IdempotencyKey: w.key(), DraftID: id, ExpectedVersion: expected, Reason: purVoidReason})
}

func (w *purWorld) draft(ctx context.Context, id string) purapi.DraftView {
	w.t.Helper()
	v, err := w.pur.GetDraft(ctx, id)
	w.must(err)
	return v
}

func (w *purWorld) allocation() purapi.AllocationView {
	w.t.Helper()
	vs, err := w.pur.GetAllocations(w.author, "order.1")
	w.must(err)
	if len(vs) != 1 {
		w.t.Fatalf("allocations %+v", vs)
	}
	return vs[0]
}

// document decodes a draft's document into a generic tree for assertions.
func purDocument(t *testing.T, v purapi.DraftView) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(v.Document), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (w *purWorld) drafts() int { return w.count("company.A", "brant_pur.purchase_draft") }

// secondSupplier verifies offer.b of supplier.2 at 5.30 USD per PCS with an MOQ of 40 (Python
// BRC-001's supplier.B) and returns its ID.
func (w *scenWorld) secondSupplier() string {
	w.t.Helper()
	version := w.staffParty(w.author, "supplier.2", []string{ptyapi.RoleSupplier}).RowVersion
	app := w.configure(w.author, w.reviewer, polapi.KindTaxApplication, "tax.goods.2", 0, w.win.from, time.Time{},
		polapi.ConfigData{TaxApplication: &polapi.TaxApplication{Name: "goods 2", Side: polapi.TaxSidePurchase, SubjectKind: polapi.SubjectSKU,
			SubjectID: "sku.1", PartyID: "supplier.2", TaxProfile: w.tax, Jurisdiction: "CN", BasisNote: "synthetic"}}, true)
	l := w.line()
	l.MOQ.Quantity, l.Tiers, l.TaxApplicationRef = "40", []supapi.Tier{{LowerInclusive: "0", UnitPrice: "5.3"}}, cref(app)
	o := w.offerWith(l)
	o.Supplier = &supapi.PartyRef{PartyID: "supplier.2", RowVersion: version}
	w.verified(supapi.KindSupplierOffer, "offer.b", o)
	return "offer.b"
}

// moq40 verifies a variant of offer.1 with an MOQ of 40 and returns its ID.
func (w *scenWorld) moq40() string {
	return w.variant(func(l *supapi.ConditionLine) { l.MOQ.Quantity = "40" })
}

// purOwner runs statements as the schema owner in company.A's scope and commits them.
func (w *purWorld) purOwner(statements ...string) error {
	tx, err := pgtest.MigratorPool(w.t).Begin(context.Background())
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true), set_config('app.company_id', 'company.A', true)`,
		w.tenant); err != nil {
		return err
	}
	for _, s := range statements {
		if _, err = tx.Exec(context.Background(), s); err != nil {
			return err
		}
	}
	return tx.Commit(context.Background())
}

// Python test_active_order_line_forms_reviewable_draft_from_effective_offer (PYT-0556; PUR-INV-001
// to 007; BRC-001's 600.00 USD draft): the active order's 120 PCS from offer.1 is a draft of 600.00
// USD net and gross, not committed, hashed; the reviewer reads the same document; the order, the
// offer and the snapshot are unchanged; the line's DemandAllocation holds 120 of 120; the audit
// trail has the prepare and the read; PurchaseDraftPrepared is in the Outbox; the snapshot's
// adoptability at preparation is recorded.
func TestActiveOrderLineFormsReviewableDraftFromEffectiveOffer(t *testing.T) {
	w := newPurWorld(t)
	before := w.condition(supapi.KindSupplierOffer, "offer.1")
	r, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.must(err)
	if r.Replayed || r.Status != purapi.StatusPrepared || r.RowVersion != 1 || r.ObjectID != "purchase.a46" || r.OrderID != "order.1" ||
		r.OrderRevision != 1 || r.OrderHash != purOrderHash || r.SupplierID != "supplier.1" || r.Currency != "USD" ||
		r.CommitmentState != purapi.CommitmentNotCommitted || r.NetMinor != "60000" || r.TaxMinor != "0" || r.GrossMinor != "60000" ||
		len(r.DraftHash) != 64 || !r.SupplyValidUntil.Equal(w.win.until) {
		t.Fatalf("receipt %+v", r)
	}
	v := w.draft(w.reviewer, "purchase.a46")
	if v.DraftHash != r.DraftHash || sha(v.Document) != v.DraftHash || v.State != purapi.StatusPrepared || v.PreparedBy.ID != "editor" ||
		v.SelectionReason != purReason || len(v.Lines) != 1 {
		t.Fatalf("view %+v", v)
	}
	l := v.Lines[0]
	if l.AllocatedQuantity != "120" || l.PurchaseQuantity != "120" || l.OverbuyQuantity != "0" || l.NetMinor != "60000" ||
		l.RequiredDate != "2027-01-31" || l.SupplierDate != "2027-01-15" || l.SlackDays != 16 || !l.SameSource || l.CostVarianceMinor != "0" {
		t.Fatalf("line %+v", l)
	}
	doc := purDocument(t, v)
	costing := doc["costing_snapshot"].(map[string]any)
	if doc["contract"] != purapi.DocumentContract || doc["commitment_state"] != "not_committed" ||
		costing["snapshot_id"] != w.snapshot.SnapshotID || costing["adoptability"] != cstapi.AdoptionAdoptable {
		t.Fatalf("document %v", doc)
	}
	if after := w.condition(supapi.KindSupplierOffer, "offer.1"); after.RootVersion != before.RootVersion {
		t.Fatal("the supply condition is unchanged")
	}
	if q := w.query(w.snapshot.SnapshotID); !q.CurrentlyEligibleForQuote {
		t.Fatal("the snapshot is unchanged")
	}
	if a := w.allocation(); a.Demand != "120" || a.Allocated != "120" || a.Remaining != "0" || a.OrderRevision != 1 || a.UnitID != "PCS" ||
		len(a.Entries) != 1 || a.Entries[0].DraftID != "purchase.a46" || !a.Entries[0].Effective {
		t.Fatalf("allocation %+v", a)
	}
	audit, err := w.pur.Audit(w.reviewer)
	w.must(err)
	var trail []string
	for _, e := range audit {
		trail = append(trail, e.Command+" "+e.ObjectID+" "+e.Outcome)
	}
	want := []string{"purchase.draft.prepare purchase.a46 succeeded", "purchase.draft.get purchase.a46 read", "demand_allocation.get order.1 read"}
	if !slices.Equal(trail, want) {
		t.Fatalf("audit %v", trail)
	}
	events := w.events("company.A", purapi.Source)
	if len(events) != 1 || events[0].Type != purapi.EventPurchaseDraftPrepared || events[0].Payload["draft_hash"] != r.DraftHash ||
		events[0].Payload["supplier_id"] != "supplier.1" || events[0].Payload["commitment_state"] != "not_committed" ||
		events[0].Payload["aggregate_version"] != float64(1) {
		t.Fatalf("events %+v", events)
	}
	if w.orders.guarded != 1 {
		t.Fatalf("OrderGuard ran %d times", w.orders.guarded)
	}
}

// Python test_replay_line_exclusivity_and_database_immutability (PYT-0557; PUR-INV-007): the same
// key replays the action; the same request under another key by the same preparer is answered from
// the stored draft; another reason, or another preparer, is PURCHASE_DRAFT_IMMUTABLE. Go diverges on
// the line (PUR-INV-002): a second draft for the line is refused only because the line is fully
// allocated (PURCHASE_ALLOCATION_EXCEEDED; Python PURCHASE_LINE_ALREADY_DRAFTED). The database
// refuses edits and deletion, for the application role and for the owner.
func TestReplayLineExclusivityAndDatabaseImmutability(t *testing.T) {
	w := newPurWorld(t)
	cmd := w.cmd("purchase.a46", purLine("120", purSource("offer.1")))
	first, err := w.pur.PrepareDraft(w.author, cmd)
	w.must(err)
	again, err := w.pur.PrepareDraft(w.author, cmd)
	if err != nil || !again.Replayed || again.ActionID != first.ActionID || again.DraftHash != first.DraftHash {
		t.Fatalf("replay %+v, %v", again, err)
	}
	other := cmd
	other.IdempotencyKey = w.key()
	answered, err := w.pur.PrepareDraft(w.author, other)
	if err != nil || !answered.Replayed || answered.ActionID == first.ActionID || answered.DraftHash != first.DraftHash {
		t.Fatalf("answered from the stored draft %+v, %v", answered, err)
	}
	changed := other
	changed.IdempotencyKey, changed.SelectionReason = w.key(), "Different synthetic reason"
	_, err = w.pur.PrepareDraft(w.author, changed)
	w.want(err, purapi.CodePurchaseDraftImmutable)
	other.IdempotencyKey = w.key()
	_, err = w.pur.PrepareDraft(w.reviewer, other)
	w.want(err, purapi.CodePurchaseDraftImmutable)
	_, err = w.prepare(w.author, "purchase.a46.second", "120", "offer.1")
	w.want(err, purapi.CodePurchaseAllocationExceeded)
	if w.drafts() != 1 || len(w.events("company.A", purapi.Source)) != 1 {
		t.Fatal("one draft, one event")
	}
	for _, c := range []struct{ sql, want string }{
		{"UPDATE brant_pur.purchase_draft SET document = document", "42501"},
		{"UPDATE brant_pur.purchase_draft SET row_version = 2", "23000"},
		{"UPDATE brant_pur.purchase_draft SET status = status, row_version = row_version", "23000"},
		{"DELETE FROM brant_pur.purchase_draft", "42501"},
		{"UPDATE brant_pur.purchase_draft_line SET quantity = 1", "42501"},
		{"DELETE FROM brant_pur.purchase_draft_line", "42501"},
		{"UPDATE brant_pur.demand_allocation SET allocated = 0", "23000"},
		{"UPDATE brant_pur.demand_allocation SET allocated = 0, row_version = row_version + 1", "23000"},
		{"UPDATE brant_pur.allocation_entry SET released_version = 9, released_at = now()", "23000"},
		{"DELETE FROM brant_pur.allocation_entry", "42501"},
		{`INSERT INTO brant_pur.purchase_draft_line (tenant_id, company_id, draft_id, order_line_id, line_no, order_id, quantity, overbuy,
			source_kind, source_id, source_revision, condition_line_id) SELECT tenant_id, company_id, draft_id, 'L2', 1, order_id, 1, 0,
			'supplier_offer', 'offer.1', 1, 'OL1' FROM brant_pur.purchase_draft`, "23000"},
	} {
		if got := sqlCode(w.exec("company.A", c.sql)); got != c.want {
			t.Errorf("%s: %s, want %s", c.sql, got, c.want)
		}
	}
	if got := sqlCode(w.purOwner("UPDATE brant_pur.purchase_draft SET selection_reason = 'x'")); got != "42703" {
		t.Errorf("no such column: %s", got)
	}
	if got := sqlCode(w.purOwner("UPDATE brant_pur.purchase_draft SET supplier_id = 'supplier.2'")); got != "23000" {
		t.Errorf("the owner's edit: %s", got)
	}
	if got := sqlCode(w.purOwner("DELETE FROM brant_pur.purchase_draft_line")); got != "23000" {
		t.Errorf("the owner's delete: %s", got)
	}
	if v := w.draft(w.author, "purchase.a46"); v.DraftHash != first.DraftHash {
		t.Fatal("the draft is unchanged")
	}
}

// Python test_competing_drafts_claim_an_order_line_once (PYT-0558) and TD-AT05's race (PUR-INV-002:
// 并发争用剩余量只有一个成功): two drafts racing for the whole line commit once; the other is
// PURCHASE_ALLOCATION_EXCEEDED (Python PURCHASE_LINE_ALREADY_DRAFTED), whether the line's root is new
// (the second insert waits on the first, then sees it) or stored (FOR UPDATE).
func TestCompetingDraftsClaimAnOrderLineOnce(t *testing.T) {
	w := newPurWorld(t)
	race := func(round int) []string {
		cmds := []purapi.PrepareDraft{w.cmd("purchase.left."+string(rune('a'+round)), purLine("120", purSource("offer.1"))),
			w.cmd("purchase.right."+string(rune('a'+round)), purLine("120", purSource("offer.1")))}
		out := make([]string, 2)
		var wg sync.WaitGroup
		for i, c := range cmds {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, err := w.pur.PrepareDraft(w.author, c)
				if err == nil {
					out[i] = "OK " + r.ObjectID
				} else {
					out[i] = code(err)
				}
			}()
		}
		wg.Wait()
		return out
	}
	for round := range 4 {
		got := race(round)
		var winner string
		for _, g := range got {
			if strings.HasPrefix(g, "OK ") {
				winner = strings.TrimPrefix(g, "OK ")
			}
		}
		if winner == "" || !slices.Contains(got, purapi.CodePurchaseAllocationExceeded) {
			t.Fatalf("round %d: %v", round, got)
		}
		if a := w.allocation(); a.Allocated != "120" {
			t.Fatalf("round %d: allocation %+v", round, a)
		}
		_, err := w.void(w.author, winner, 1)
		w.must(err)
	}
	if w.drafts() != 4 || w.allocation().Allocated != "0" {
		t.Fatal("four drafts, all voided")
	}
}

// TD-AT05 (PUR-INV-002, an intentional divergence from Python's one draft per order line): 120 PCS
// split 80 to supplier.1 and 40 to supplier.2; one more piece is PURCHASE_ALLOCATION_EXCEEDED. Two
// suppliers racing for a released remainder commit once.
func TestOrderLineSplitsAcrossSuppliers(t *testing.T) {
	w := newPurWorld(t)
	a, b := w.moq40(), w.secondSupplier()
	ra, err := w.prepare(w.author, "draft.A", "80", a)
	w.must(err)
	rb, err := w.prepare(w.author, "draft.B", "40", b)
	w.must(err)
	if ra.NetMinor != "40000" || ra.SupplierID != "supplier.1" || rb.NetMinor != "21200" || rb.SupplierID != "supplier.2" {
		t.Fatalf("drafts %+v %+v", ra, rb)
	}
	_, err = w.prepare(w.author, "draft.C", "1", a)
	w.want(err, purapi.CodePurchaseAllocationExceeded)
	if al := w.allocation(); al.Allocated != "120" || al.Remaining != "0" || len(al.Entries) != 2 {
		t.Fatalf("allocation %+v", al)
	}
	vb := w.draft(w.author, "draft.B")
	if vb.Lines[0].SameSource || vb.Lines[0].CostVarianceMinor != "1200" {
		t.Fatalf("supplier.2's comparison %+v", vb.Lines[0])
	}
	_, err = w.void(w.author, "draft.B", 1)
	w.must(err)
	for round := range 3 {
		cmds := []purapi.PrepareDraft{w.cmd("race.a."+string(rune('a'+round)), purLine("40", purSource(a))),
			w.cmd("race.b."+string(rune('a'+round)), purLine("40", purSource(b)))}
		results := make([]error, 2)
		receipts := make([]purapi.DraftReceipt, 2)
		var wg sync.WaitGroup
		for i, c := range cmds {
			wg.Add(1)
			go func() { defer wg.Done(); receipts[i], results[i] = w.pur.PrepareDraft(w.author, c) }()
		}
		wg.Wait()
		var won []string
		for i, err := range results {
			switch code(err) {
			case "OK":
				won = append(won, receipts[i].ObjectID)
			case purapi.CodePurchaseAllocationExceeded:
			default:
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if len(won) != 1 || w.allocation().Allocated != "120" {
			t.Fatalf("round %d: %v", round, won)
		}
		_, err = w.void(w.author, won[0], 1)
		w.must(err)
	}
}

// Python test_draft_or_revoked_order_cannot_enter_purchase_preparation (PYT-0559) and
// test_activation_without_independent_order_review_is_not_purchase_ready (PYT-0560), on TRD's
// ActiveOrderRef v1 through PUR's port: a draft order, a revoked activating approval, an order
// without one; an approval revoked after PUR's lookups and before its transaction is OrderGuard
// v1's refusal inside it. Nothing is stored; an order TRD does not show is NOT_FOUND.
func TestDraftOrRevokedOrderCannotEnterPurchasePreparation(t *testing.T) {
	w := newPurWorld(t)
	for _, c := range []string{pur.OrderNotActive, pur.OrderApprovalRevoked, pur.OrderApprovalRequired} {
		w.orders.set("order.1", func(o *pur.Order) { o.Code = c })
		_, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
		if code(err) != c || path(err) != "/args/order_id" {
			t.Fatalf("%s: %s at %s", c, code(err), path(err))
		}
	}
	w.orders.set("order.1", func(o *pur.Order) { o.Code = "" })
	w.hook.then(func() {
		w.orders.verdict("order.1", pur.OrderVerdict{Known: true, Status: "active", Code: pur.OrderApprovalRevoked})
	})
	_, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.want(err, purapi.CodeOrderApprovalRevoked)
	w.orders.verdict("order.1", pur.OrderVerdict{Known: true, Status: "active"})
	w.orders.set("order.1", func(o *pur.Order) { o.Binding.OrderHash = strings.Repeat("ef", 32) })
	w.orders.verdicts = map[string]pur.OrderVerdict{}
	w.hook.then(func() { w.orders.set("order.1", func(o *pur.Order) { o.Binding.OrderRevision = 2 }) })
	_, err = w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.want(err, purapi.CodeReferenceStale)
	w.orders.blind["editor"] = true // the fixture's author
	_, err = w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.want(err, kernel.CodeNotFound)
	if w.drafts() != 0 || w.count("company.A", "brant_pur.demand_allocation") != 0 {
		t.Fatal("nothing stored")
	}
}

// Python test_lines_sources_and_reason_must_be_explicit (PYT-0561): malformed requests are
// SCHEMA_INVALID and audited, a repeated line DUPLICATE_LINE_ID, a line the order does not have
// ORDER_LINE_NOT_FOUND, a condition line or an offer that does not exist REFERENCE_INVALID.
func TestLinesSourcesAndReasonMustBeExplicit(t *testing.T) {
	w := newPurWorld(t)
	src := purSource("offer.1")
	for _, c := range []struct {
		edit func(c *purapi.PrepareDraft)
		want string
	}{
		{func(c *purapi.PrepareDraft) { c.Lines = nil }, kernel.CodeSchemaInvalid},
		{func(c *purapi.PrepareDraft) { c.Lines[0].Source = purapi.SourceRef{} }, kernel.CodeSchemaInvalid},
		{func(c *purapi.PrepareDraft) { c.Lines[0].OrderLineID = "L 1" }, kernel.CodeSchemaInvalid},
		{func(c *purapi.PrepareDraft) { c.Lines[0].Source.Kind = supapi.KindLogisticsOffer }, kernel.CodeSchemaInvalid},
		{func(c *purapi.PrepareDraft) { c.SelectionReason = " " }, kernel.CodeSchemaInvalid},
		{func(c *purapi.PrepareDraft) { c.SelectionReason = strings.Repeat("x", 1001) }, kernel.CodeSchemaInvalid},
		{func(c *purapi.PrepareDraft) { c.SelectionReason = "synthetic\x00reason" }, kernel.CodeSchemaInvalid},
		{func(c *purapi.PrepareDraft) { c.DraftID = "采购 草稿" }, kernel.CodeSchemaInvalid},
		{func(c *purapi.PrepareDraft) { c.Lines = append(c.Lines, c.Lines[0]) }, kernel.CodeDuplicateLineID},
		{func(c *purapi.PrepareDraft) { c.Lines[0].OrderLineID = "L9" }, purapi.CodeOrderLineNotFound},
		{func(c *purapi.PrepareDraft) { c.Lines[0].Source.ConditionLineID = "OL9" }, kernel.CodeReferenceInvalid},
		{func(c *purapi.PrepareDraft) { c.Lines[0].Source.ID = "offer.missing" }, kernel.CodeReferenceInvalid},
	} {
		cmd := w.cmd("purchase.a46", purLine("120", src))
		c.edit(&cmd)
		_, err := w.pur.PrepareDraft(w.author, cmd)
		w.want(err, c.want)
	}
	if w.drafts() != 0 {
		t.Fatal("nothing stored")
	}
	audit, err := w.pur.Audit(w.reviewer)
	w.must(err)
	if len(audit) != 12 || audit[0].Outcome != "rejected" || audit[0].Code != kernel.CodeSchemaInvalid {
		t.Fatalf("every refusal is audited: %+v", audit)
	}
}

// Python test_unverified_superseded_and_expired_conditions_are_rejected (PYT-0562): an unverified
// working revision is not a supply condition for a draft (REFERENCE_INVALID: SUP's lookup leaves
// drafts out; Python SUPPLY_CONDITION_NOT_CURRENT), a superseded revision is
// SUPPLY_CONDITION_NOT_CURRENT, an expired one SUPPLY_CONDITION_EXPIRED; the new revision drafts
// 600.00, not the costing's source. The snapshot became not_current: recorded, not blocking (R21).
func TestUnverifiedSupersededAndExpiredConditionsAreRejected(t *testing.T) {
	w := newPurWorld(t)
	_, err := w.revise(w.author, supapi.KindSupplierOffer, "offer.1", 2, 1)
	w.must(err)
	two := purapi.SourceRef{Kind: purapi.KindSupplierOffer, ID: "offer.1", Revision: 2, ConditionLineID: "OL1"}
	_, err = w.pur.PrepareDraft(w.author, w.cmd("purchase.a46", purLine("120", two)))
	w.want(err, kernel.CodeReferenceInvalid)
	_, err = w.seal(w.reviewer, supapi.KindSupplierOffer, "offer.1", 3, 2, 1)
	w.must(err)
	_, err = w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.want(err, supapi.CodeSupplyConditionNotCurrent)
	r, err := w.pur.PrepareDraft(w.author, w.cmd("purchase.a46", purLine("120", two)))
	w.must(err)
	v := w.draft(w.author, r.ObjectID)
	costing := purDocument(t, v)["costing_snapshot"].(map[string]any)
	if v.NetMinor != "60000" || v.Lines[0].SameSource || costing["adoptability"] != "not_current" || costing["code"] != cstapi.CodeCostSnapshotIneligible {
		t.Fatalf("draft %+v costing %v", v.Lines[0], costing)
	}
	until := time.Now().Add(6 * time.Second).UTC().Truncate(time.Second)
	short := w.offerWith(w.line())
	short.ValidUntil = kernel.FormatTimestamp(until)
	w.verified(supapi.KindSupplierOffer, "offer.short", short)
	_, err = w.void(w.author, r.ObjectID, 1)
	w.must(err)
	cmd := w.cmd("purchase.short", purLine("120", purSource("offer.short")))
	expiring, err := w.pur.PrepareDraft(w.author, cmd)
	w.must(err)
	if time.Now().After(until) {
		t.Fatal("the window is too short for this host")
	}
	time.Sleep(time.Until(until) + 200*time.Millisecond)
	_, err = w.void(w.author, "purchase.short", 1)
	w.must(err)
	_, err = w.prepare(w.author, "purchase.late", "120", "offer.short")
	w.want(err, supapi.CodeSupplyConditionExpired)
	// Python test_audit_failure_rolls_back_and_history_survives_restart_and_expiry, the expiry half:
	// history reads unchanged after its supply expired, and the key replays.
	again, err := w.module().PrepareDraft(w.author, cmd)
	if err != nil || !again.Replayed || again.DraftHash != expiring.DraftHash {
		t.Fatalf("replay after expiry %+v, %v", again, err)
	}
}

// Python test_purchase_terms_follow_their_validity_mode (PYT-0563): purchase terms of dependent
// validity stop with their source offer's supersession (SUPPLY_CONDITION_NOT_CURRENT through SUP's
// guard); independent terms keep drafting with the historical offer as their recorded origin.
func TestPurchaseTermsFollowTheirValidityMode(t *testing.T) {
	w := newPurWorld(t)
	for _, mode := range []string{supapi.ValidityDependent, supapi.ValidityIndependent} {
		terms := w.terms("offer.1", mode)
		terms.Lines = []supapi.ConditionLine{w.line()}
		w.verified(supapi.KindPurchaseTerms, "terms."+mode, terms)
	}
	_, err := w.revise(w.author, supapi.KindSupplierOffer, "offer.1", 2, 1)
	w.must(err)
	_, err = w.seal(w.reviewer, supapi.KindSupplierOffer, "offer.1", 3, 2, 1)
	w.must(err)
	src := func(mode string) purapi.SourceRef {
		return purapi.SourceRef{Kind: purapi.KindPurchaseTerms, ID: "terms." + mode, Revision: 1, ConditionLineID: "OL1"}
	}
	_, err = w.pur.PrepareDraft(w.author, w.cmd("purchase.dependent", purLine("120", src(supapi.ValidityDependent))))
	w.want(err, supapi.CodeSupplyConditionNotCurrent)
	r, err := w.pur.PrepareDraft(w.author, w.cmd("purchase.independent", purLine("120", src(supapi.ValidityIndependent))))
	w.must(err)
	sources := purDocument(t, w.draft(w.author, r.ObjectID))["sources"].([]any)
	origin := sources[0].(map[string]any)["origin"].(map[string]any)
	if r.NetMinor != "60000" || origin["id"] != "offer.1" || origin["revision"] != float64(1) || origin["validity_mode"] != supapi.ValidityIndependent {
		t.Fatalf("draft %+v origin %v", r, origin)
	}
}

// Python test_withdrawn_specification_blocks_new_purchase_price (PYT-0569): a specification
// reopened for revision is REFERENCE_STALE (CAT's verdict).
func TestWithdrawnSpecificationBlocksNewPurchasePrice(t *testing.T) {
	w := newPurWorld(t)
	sku, err := w.c.CAT.GetSku(w.author, "sku.1")
	w.must(err)
	w.must(w.reviseSpec(w.author, "sku.1", sku.RowVersion, w.key()))
	_, err = w.prepare(w.author, "purchase.a46", "120", "offer.1")
	w.want(err, purapi.CodeReferenceStale)
}

// Python test_identity_permissions_and_data_scope (PYT-0570): an AI agent with every permission is
// refused (human only); a human without purchase.draft.prepare is refused; another company's actor
// sees no order (NOT_FOUND); an actor who does not see the supply condition is REFERENCE_INVALID.
// Go needs no read permissions beyond purchase.draft.prepare (the lookups need an active grant;
// Python also asked quote.read, read, costing.read and foundation.read).
func TestIdentityPermissionsAndDataScope(t *testing.T) {
	w := newPurWorld(t)
	ai := w.other("a46.ai.agent", kernel.ActorAI, allPermissions...)
	_, err := w.prepare(ai, "purchase.a46", "120", "offer.1")
	w.want(err, kernel.CodePermissionDenied)
	limited := w.provision(bootstrapGrant(w.supWorld, "a46.limited", "company", "pur.read", "sup.read", "trd.read", "cst.read"))
	_, err = w.prepare(limited, "purchase.a46", "120", "offer.1")
	w.want(err, kernel.CodePermissionDenied)
	outsider := w.provision(bootstrapGrantIn(w.supWorld, "company.B", "a46.outsider", "company", allPermissions...))
	_, err = w.prepare(outsider, "purchase.a46", "120", "offer.1")
	w.want(err, kernel.CodeNotFound)
	own := w.provision(bootstrapGrant(w.supWorld, "a46.own", "own", allPermissions...))
	w.orders.blind["a46.own"] = true // TRD shows an own-scope actor no order of another's quote
	_, err = w.prepare(own, "purchase.a46", "120", "offer.1")
	w.want(err, kernel.CodeNotFound)
	team := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "a46.team"), Permissions: allPermissions,
		DataScope: authorizationScope("team"), Team: "other"})
	_, err = w.prepare(team, "purchase.a46", "120", "offer.1")
	w.want(err, kernel.CodeReferenceInvalid)
	if w.drafts() != 0 {
		t.Fatal("nothing stored")
	}
}

// Python test_reads_recheck_current_grants_scope_and_supply_access (PYT-0572): a reader with
// pur.read reads the draft; without it PERMISSION_DENIED; another company or a missing draft
// NOT_FOUND; a malformed ID SCHEMA_INVALID; a reader who no longer sees the order or a supply
// condition the draft binds REFERENCE_INVALID, for the read and for the write's replay.
func TestReadsRecheckCurrentGrantsScopeAndSupplyAccess(t *testing.T) {
	w := newPurWorld(t)
	reader := w.provision(bootstrapGrant(w.supWorld, "a46.reader", "company", allPermissions...))
	cmd := w.cmd("purchase.a46", purLine("120", purSource("offer.1")))
	r, err := w.pur.PrepareDraft(reader, cmd)
	w.must(err)
	if w.draft(reader, "purchase.a46").DraftHash != r.DraftHash {
		t.Fatal("read")
	}
	w.provision(bootstrapGrant(w.supWorld, "a46.reader", "company", "sup.read", "trd.read"))
	_, err = w.pur.GetDraft(reader, "purchase.a46")
	w.want(err, kernel.CodePermissionDenied)
	outsider := w.provision(bootstrapGrantIn(w.supWorld, "company.B", "a46.outsider", "company", allPermissions...))
	_, err = w.pur.GetDraft(outsider, "purchase.a46")
	w.want(err, kernel.CodeNotFound)
	_, err = w.pur.GetDraft(w.author, "purchase.missing")
	w.want(err, kernel.CodeNotFound)
	_, err = w.pur.GetDraft(w.author, "采购 草稿")
	w.want(err, kernel.CodeSchemaInvalid)
	// The reader keeps its own draft but no longer sees offer.1, owned by the author.
	w.provision(bootstrapGrant(w.supWorld, "a46.reader", "own", allPermissions...))
	_, err = w.pur.GetDraft(reader, "purchase.a46")
	w.want(err, kernel.CodeReferenceInvalid)
	w.provision(bootstrapGrant(w.supWorld, "a46.reader", "company", allPermissions...))
	if w.draft(reader, "purchase.a46").DraftHash != r.DraftHash {
		t.Fatal("read again")
	}
	w.orders.blind["a46.reader"] = true
	_, err = w.pur.GetDraft(reader, "purchase.a46")
	w.want(err, kernel.CodeReferenceInvalid)
}

// Python test_tampered_draft_records_fail_closed (PYT-0573): the database refuses a document,
// request or hash that does not hash, even for the owner with the guard trigger off; a record
// tampered past the triggers (the preparation instant) is REFERENCE_INVALID for reads, write
// replays and voids, and serves again once restored.
func TestTamperedDraftRecordsFailClosed(t *testing.T) {
	w := newPurWorld(t)
	cmd := w.cmd("purchase.a46", purLine("120", purSource("offer.1")))
	r, err := w.pur.PrepareDraft(w.author, cmd)
	w.must(err)
	off := "SET CONSTRAINTS ALL IMMEDIATE; ALTER TABLE brant_pur.purchase_draft DISABLE TRIGGER purchase_draft_guard"
	on := "ALTER TABLE brant_pur.purchase_draft ENABLE TRIGGER purchase_draft_guard"
	for _, edit := range []string{
		"UPDATE brant_pur.purchase_draft SET document = replace(document, '\"60000\"', '\"1\"')",
		"UPDATE brant_pur.purchase_draft SET draft_hash = repeat('0', 64)",
		"UPDATE brant_pur.purchase_draft SET request = '{}'",
	} {
		if got := sqlCode(w.purOwner(off, edit, on)); got != "23514" {
			t.Errorf("%s: %s", edit, got)
		}
	}
	w.must(w.purOwner(off, "UPDATE brant_pur.purchase_draft SET prepared_at = prepared_at + interval '1 second'", on))
	_, err = w.pur.GetDraft(w.author, "purchase.a46")
	w.want(err, kernel.CodeReferenceInvalid)
	_, err = w.pur.PrepareDraft(w.author, cmd)
	w.want(err, kernel.CodeReferenceInvalid)
	_, err = w.void(w.author, "purchase.a46", 1)
	w.want(err, kernel.CodeReferenceInvalid)
	w.must(w.purOwner(off, "UPDATE brant_pur.purchase_draft SET prepared_at = prepared_at - interval '1 second'", on))
	if w.draft(w.author, "purchase.a46").DraftHash != r.DraftHash {
		t.Fatal("restored")
	}
}

// Python test_audit_failure_rolls_back_and_history_survives_restart_and_expiry (PYT-0574), the
// failure half: a failing Outbox write rolls the preparation back whole (no draft, no allocation,
// no succeeded audit); the same key prepares once the store accepts the write; a restarted process
// reads the same draft and replays the key.
func TestAuditFailureRollsBackAndHistorySurvivesRestart(t *testing.T) {
	w := newPurWorld(t)
	restore := w.failOutbox(purapi.EventPurchaseDraftPrepared)
	cmd := w.cmd("purchase.a46", purLine("120", purSource("offer.1")))
	_, err := w.pur.PrepareDraft(w.author, cmd)
	if err == nil || code(err) == "OK" {
		t.Fatal("the Outbox failure fails the command")
	}
	if _, ok := kernel.AsRejection(err); ok {
		t.Fatalf("an infrastructure failure is no rejection: %v", err)
	}
	if w.drafts() != 0 || w.count("company.A", "brant_pur.demand_allocation") != 0 || w.count("company.A", "brant_pur.action_result") != 0 {
		t.Fatal("nothing stays")
	}
	restore()
	r, err := w.pur.PrepareDraft(w.author, cmd)
	if err != nil || r.Replayed {
		t.Fatalf("retry %+v, %v", r, err)
	}
	m := w.module()
	v, err := m.GetDraft(w.reviewer, "purchase.a46")
	if err != nil || v.DraftHash != r.DraftHash {
		t.Fatalf("restart %+v, %v", v, err)
	}
	again, err := m.PrepareDraft(w.author, cmd)
	if err != nil || !again.Replayed || again.ActionID != r.ActionID {
		t.Fatalf("replay after restart %+v, %v", again, err)
	}
}

// TD-AT06 (SM-PUR-DRAFT, PUR-INV-007; Python has no void, A46 限制): a draft on the wrong supply
// condition, not committed, is voided by a human with a reason; its 80 PCS return to the line, the
// record stays (voided, row version 2, readable) and PurchaseDraftVoided is in the Outbox; the key
// replays; voiding again is INVALID_STATE, at another version VERSION_CONFLICT; an AI agent is
// refused. The released quantity drafts again.
func TestVoidReleasesAllocationAndKeepsTheRecord(t *testing.T) {
	w := newPurWorld(t)
	a := w.moq40()
	_, err := w.prepare(w.author, "draft.wrong", "80", a)
	w.must(err)
	ai := w.other("a46.ai.agent", kernel.ActorAI, allPermissions...)
	_, err = w.void(ai, "draft.wrong", 1)
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.void(w.author, "draft.wrong", 2)
	w.want(err, kernel.CodeVersionConflict)
	_, err = w.void(w.author, "draft.missing", 1)
	w.want(err, kernel.CodeNotFound)
	cmd := purapi.VoidDraft{IdempotencyKey: w.key(), DraftID: "draft.wrong", ExpectedVersion: 1, Reason: purVoidReason}
	r, err := w.pur.VoidDraft(w.reviewer, cmd)
	w.must(err)
	if r.Status != purapi.StatusVoided || r.RowVersion != 2 || r.Replayed || r.NetMinor != "40000" {
		t.Fatalf("receipt %+v", r)
	}
	again, err := w.pur.VoidDraft(w.reviewer, cmd)
	if err != nil || !again.Replayed || again.ActionID != r.ActionID {
		t.Fatalf("replay %+v, %v", again, err)
	}
	_, err = w.void(w.reviewer, "draft.wrong", 2)
	w.want(err, kernel.CodeInvalidState)
	v := w.draft(w.author, "draft.wrong")
	if v.State != purapi.StatusVoided || v.VoidedBy.ID != "reviewer" || v.VoidReason != purVoidReason || v.Lines[0].AllocatedQuantity != "80" {
		t.Fatalf("view %+v", v)
	}
	al := w.allocation()
	if al.Allocated != "0" || al.Remaining != "120" || len(al.Entries) != 1 || al.Entries[0].Effective || al.Entries[0].ReleasedAt.IsZero() {
		t.Fatalf("allocation %+v", al)
	}
	var voided []event
	for _, e := range w.events("company.A", purapi.Source) {
		if e.Type == purapi.EventPurchaseDraftVoided {
			voided = append(voided, e)
		}
	}
	if len(voided) != 1 || voided[0].Payload["draft_id"] != "draft.wrong" || voided[0].Payload["aggregate_version"] != float64(2) {
		t.Fatalf("events %+v", voided)
	}
	audit, err := w.pur.Audit(w.reviewer)
	w.must(err)
	if !slices.ContainsFunc(audit, func(e kernel.AuditEntry) bool {
		return e.Command == purapi.CommandVoidDraft && e.Outcome == "succeeded" && e.Reason == purVoidReason
	}) {
		t.Fatal("the reason is in the audit trail")
	}
	_, err = w.prepare(w.author, "draft.right", "120", "offer.1")
	w.must(err)
}

// PUR-INV-005: an overbuy needs its reason and the preparer's purchase.overbuy.approve; an approved
// one buys 150 for an allocation of 120 and keeps the 30's cost out of the cost of goods sold.
func TestOverbuyIsMarkedAndApproved(t *testing.T) {
	w := newPurWorld(t)
	moq := w.variant(func(l *supapi.ConditionLine) { l.MOQ.Quantity = "150" })
	_, err := w.prepare(w.author, "draft.moq", "120", moq)
	w.want(err, purapi.CodeMOQNotMet)
	buyer := w.provision(bootstrapGrant(w.supWorld, "a46.buyer", "company",
		slices.DeleteFunc(slices.Clone(allPermissions), func(p string) bool { return p == purapi.PermissionApproveOverbuy })...))
	line := purLine("120", purSource(moq))
	line.Overbuy = &purapi.Overbuy{Quantity: "30"}
	_, err = w.pur.PrepareDraft(w.author, w.cmd("draft.moq", line))
	w.want(err, purapi.CodeOverbuyNotApproved)
	line.Overbuy.Reason = "MOQ 150; the remainder goes to stock"
	_, err = w.pur.PrepareDraft(buyer, w.cmd("draft.moq", line))
	w.want(err, purapi.CodeOverbuyNotApproved)
	r, err := w.pur.PrepareDraft(w.author, w.cmd("draft.moq", line))
	w.must(err)
	l := w.draft(w.author, r.ObjectID).Lines[0]
	if r.NetMinor != "75000" || l.PurchaseQuantity != "150" || l.OverbuyQuantity != "30" || l.SoldCostMinor != "60000" || l.CostVarianceMinor != "0" ||
		l.OverbuyReason != line.Overbuy.Reason || w.allocation().Allocated != "120" {
		t.Fatalf("draft %+v line %+v", r, l)
	}
}

// ADR-BR-004 option B and ADR-BR-011 on PUR's own transaction: an upstream change that commits
// after PUR's lookups and before its transaction is refused by the owner's guard inside it (CAT,
// POL, PTY, SUP, TRD); a CST source denial is recorded in the draft, never blocking (R21). And while
// PUR's transaction holds the guards' FOR SHARE locks, the upstream command waits until PUR commits.
func TestPurchaseGuardsDecideEveryCommitOrder(t *testing.T) {
	type change struct {
		name string
		run  func(w *purWorld) error
		want string
	}
	changes := []change{
		{"CAT specification reopened", func(w *purWorld) error {
			sku, err := w.c.CAT.GetSku(w.author, "sku.1")
			if err != nil {
				return err
			}
			return w.reviseSpec(w.author, "sku.1", sku.RowVersion, w.key())
		}, purapi.CodeReferenceStale},
		{"POL trade terms retired", func(w *purWorld) error {
			_, err := w.c.POL.Retire(w.reviewer, polapi.Retire{IdempotencyKey: w.key(), Kind: w.trade.Kind, ConfigID: w.trade.ConfigID,
				ExpectedVersion: 2, Reason: "停用"})
			return err
		}, polapi.CodeConfigNotActive},
		{"PTY supplier deactivated", func(w *purWorld) error {
			_, err := w.c.PTY.DeactivateParty(w.author, ptyapi.DeactivateParty{IdempotencyKey: w.key(), PartyID: "supplier.1",
				ExpectedVersion: w.supplierVersion, Reason: "停止合作"})
			return err
		}, purapi.CodeReferenceStale},
		{"SUP offer withdrawn", func(w *purWorld) error {
			_, err := w.end(w.reviewer, supapi.KindSupplierOffer, "offer.1", 2, 1)
			return err
		}, supapi.CodeSupplyConditionNotCurrent},
	}
	for _, c := range changes {
		t.Run("upstream first: "+c.name, func(t *testing.T) {
			w := newPurWorld(t)
			w.hook.then(func() { w.must(c.run(w)) })
			_, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
			if code(err) != c.want || path(err) != "/args/lines/0/source" {
				t.Fatalf("%s at %s", code(err), path(err))
			}
			if w.drafts() != 0 {
				t.Fatal("nothing stored")
			}
		})
		t.Run("PUR first: "+c.name, func(t *testing.T) {
			w := newPurWorld(t)
			w.orders.hold, w.orders.held = make(chan struct{}), make(chan struct{}, 1)
			done := make(chan error, 1)
			go func() {
				_, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
				done <- err
			}()
			select {
			case <-w.orders.held:
			case err := <-done:
				t.Fatalf("PUR finished before its guards held: %v", err)
			case <-time.After(30 * time.Second):
				t.Fatal("PUR never reached OrderGuard")
			}
			upstream := make(chan error, 1)
			go func() { upstream <- c.run(w) }()
			select {
			case err := <-upstream:
				close(w.orders.hold)
				t.Fatalf("the upstream command did not wait: %v (PUR: %v)", err, <-done)
			case <-time.After(500 * time.Millisecond):
			}
			close(w.orders.hold)
			if err := <-done; err != nil {
				t.Fatalf("PUR: %v", err)
			}
			if err := <-upstream; err != nil {
				t.Fatalf("the upstream command after PUR committed: %v", err)
			}
			if w.drafts() != 1 {
				t.Fatal("the draft committed")
			}
		})
	}
	t.Run("CST source denied: recorded", func(t *testing.T) {
		w := newPurWorld(t)
		w.hook.then(func() {
			_, err := w.attest(w.reviewer, "evidence", cstapi.VerdictDenied, w.counter())
			w.must(err)
		})
		r, err := w.prepare(w.author, "purchase.a46", "120", "offer.1")
		w.must(err)
		costing := purDocument(t, w.draft(w.author, r.ObjectID))["costing_snapshot"].(map[string]any)
		if costing["adoptability"] != cstapi.AdoptionDenied || costing["input"] != "/sources/evidence" {
			t.Fatalf("costing %v", costing)
		}
	})
}

// PartyGuard v1 (R22) as PUR's transactions call it: confirmed at the bound row version is current;
// revised (a draft now) party_unavailable; confirmed again at a new row version party_changed;
// another company's or an unknown party is unknown; it is SECURITY DEFINER with a fixed
// search_path, executable by brant_app and not by PUBLIC.
func TestPartyGuardVerdicts(t *testing.T) {
	w := newPurWorld(t)
	guard := func(company, id string, version int64) ptyapi.PartyGuardVerdict {
		t.Helper()
		b := ptyapi.PartyBinding{PartyID: id, RowVersion: version}
		var v ptyapi.PartyGuardVerdict
		db, err := bentopg.NewDB(pgtest.AppPool(t))
		w.must(err)
		err = bento.ScopeTransactor{DB: db, Scope: w.scope(company)}.WithinTransaction(context.Background(), func(ctx context.Context) error {
			exec, err := db.RequireExecutor(ctx)
			if err != nil {
				return err
			}
			v, err = ptyapi.ScanPartyGuard(b, exec.QueryRow(ctx, ptyapi.PartyGuardSQL, ptyapi.PartyGuardArgs(b)...).Scan)
			return err
		})
		w.must(err)
		return v
	}
	v := guard("company.A", "supplier.1", w.supplierVersion)
	if !v.Current() || !v.HasRole(ptyapi.RoleSupplier) || v.Status != ptyapi.StatusConfirmed || v.RowVersion != w.supplierVersion {
		t.Fatalf("current %+v", v)
	}
	_, err := w.c.PTY.ReviseParty(w.author, ptyapi.ReviseParty{IdempotencyKey: w.key(), PartyID: "supplier.1", ExpectedVersion: w.supplierVersion,
		Reason: "地址变更"})
	w.must(err)
	if v = guard("company.A", "supplier.1", w.supplierVersion); v.Stale != ptyapi.StalePartyUnavailable || code(v.Check("/p")) != ptyapi.CodeReferenceStale {
		t.Fatalf("revised %+v", v)
	}
	_, err = w.c.PTY.ConfirmParty(w.author, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: "supplier.1", ExpectedVersion: w.supplierVersion + 1,
		EvidenceIDs: []string{"evidence"}})
	w.must(err)
	if v = guard("company.A", "supplier.1", w.supplierVersion); v.Stale != ptyapi.StalePartyChanged || v.RowVersion != w.supplierVersion+2 {
		t.Fatalf("reconfirmed %+v", v)
	}
	if v = guard("company.A", "supplier.1", w.supplierVersion+2); !v.Current() || v.Revision != 2 {
		t.Fatalf("current again %+v", v)
	}
	for _, c := range []struct{ company, id string }{{"company.B", "supplier.1"}, {"company.A", "missing"}} {
		if v = guard(c.company, c.id, 1); v.Known || code(v.Check("/p")) != kernel.CodeReferenceInvalid {
			t.Fatalf("%s %s: %+v", c.company, c.id, v)
		}
	}
	var definer, publicExec, appExec bool
	var config []string
	w.must(w.execScan("company.A", `SELECT p.prosecdef, p.proconfig, has_function_privilege('public', p.oid, 'EXECUTE'),
		has_function_privilege('brant_app', p.oid, 'EXECUTE') FROM pg_proc p WHERE p.oid = 'brant_pty.guard_party_current_v1(text, bigint)'::regprocedure`,
		&definer, &config, &publicExec, &appExec))
	if !definer || publicExec || !appExec || !slices.Contains(config, "search_path=pg_catalog, pg_temp") {
		t.Fatalf("definer %v config %v public %v app %v", definer, config, publicExec, appExec)
	}
	if err = w.exec("company.A", "SELECT * FROM brant_pty.guard_party_current_v1(NULL, 1)"); sqlCode(err) != "22004" {
		t.Fatalf("NULL: %s", sqlCode(err))
	}
}
