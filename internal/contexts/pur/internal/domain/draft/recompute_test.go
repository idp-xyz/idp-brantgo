package draft

import (
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/allocation"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// A47 #3 (PUR-INV-009; A47 最低验收: 重算结果不一致时失败关闭): a purchase commitment decision prepares the
// draft's stored request again on the facts as they are now and requires the result equal to the
// sealed document field by field. Who prepared it, when, the cost snapshot's adoptability and an S3
// preparer's overbuy approval are not the calculation; anything else that differs is
// PURCHASE_DRAFT_STALE naming the first field.
func TestRecomputeRequiresTheSealedDraftFieldByField(t *testing.T) {
	w := fixture()
	d, err := w.prepare(t).Seal("adoptable", "", "", "trade")
	if err != nil {
		t.Fatal(err)
	}
	req, err := d.DecodeRequest()
	if err != nil {
		t.Fatal(err)
	}
	again, err := CanonicalRequest(req)
	if err != nil || string(again) != string(d.Request) {
		t.Fatalf("the stored request decodes to itself: %s, %v", again, err)
	}
	// The same facts later, by another decider, with the snapshot no longer adoptable: equal.
	later := fixture()
	later.in.Now, later.in.Epoch, later.in.Preparer = now.Add(3*time.Hour), 9, kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer"}
	p := later.prepare(t)
	p.Doc.CostingSnapshot.Adoptability = "not_current"
	if err = d.Recompute(p, "/args/commitment_id"); err != nil {
		t.Fatalf("equal: %v", err)
	}
	for _, c := range []struct {
		edit  func(w *world)
		field string
	}{
		{func(w *world) { w.in.Order.Delivery.Date = "2026-11-16" }, "/lines/0/delivery/required_date"},
		{func(w *world) { w.in.Order.OrderHash = "otherhash" }, "/order/order_hash"},
		{func(w *world) {
			ref := w.req.Lines[0].Source.Ref
			s := w.in.Supply[ref]
			s.ContentHash = "otherhash"
			w.in.Supply[ref] = s
		}, "/sources/0/content_hash"},
		{func(w *world) {
			c := w.in.Configs[zeroTax]
			c.TaxProfile = &TaxProfile{Rate: "0.13", PriceBasis: BasisExclusive, Recovery: Nonrecoverable}
			w.in.Configs[zeroTax] = c
		}, "/lines/0/cost_minor"},
	} {
		x := fixture()
		c.edit(x)
		err := d.Recompute(x.prepare(t), "/args/commitment_id")
		r, _ := kernel.AsRejection(err)
		if r == nil || r.Code != CodePurchaseDraftStale || r.Category != kernel.CategoryConflict || r.Fields[0].Path != "/args/commitment_id" ||
			!strings.Contains(r.Fields[0].Reason, c.field) {
			t.Errorf("%s: %v", c.field, err)
		}
	}
	tampered := *d
	tampered.Document = []byte("{")
	if code(tampered.Recompute(p, "/x")) != "REFERENCE_INVALID /x" {
		t.Fatal("a document that does not decode")
	}
}

// A47 #3 on the allocation: the draft's own entry is left out when it is prepared again, so the
// line it fills still fits, and the copy is never the stored root.
func TestRecomputeAgainstTheAllocationWithoutTheDraft(t *testing.T) {
	w := fixture()
	p := w.prepare(t)
	root := p.Allocations[0]
	root.Stored = root.RowVersion
	if root.Allocated.String() != "120" {
		t.Fatalf("root %+v", root)
	}
	again := fixture()
	again.in.Allocations = map[allocation.Key]*allocation.Allocation{root.Key: root}
	again.refused(t, "INVALID_STATE /args/lines/0/quantity") // the draft already holds the line
	without := root.Without("purchase.a46")
	if without.Allocated.Sign() != 0 || len(without.Entries) != 0 || root.Allocated.String() != "120" || len(root.Entries) != 1 {
		t.Fatalf("copy %+v root %+v", without, root)
	}
	again.in.Allocations = map[allocation.Key]*allocation.Allocation{root.Key: without}
	if q := again.prepare(t); q.Allocations[0] == root {
		t.Fatal("the copy is judged, not the root")
	}
}
