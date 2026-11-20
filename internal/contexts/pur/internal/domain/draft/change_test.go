package draft

import (
	"slices"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// A commitment change's proposed revision (S5-19, PUR-INV-011: 重算与偏差确认同 PUR-INV-009; A47 #7: 交期变更、
// 取消、短收和价格调整作为后续受控动作) is the draft's request prepared again with Change: what a draft
// refuses and the change's decision acknowledges instead — a supplier date that is not the
// condition's, one after the order's, one passed, an MOQ not met — is recorded in the document; the
// revision's document recomputes equal or is PURCHASE_DRAFT_STALE.
func TestChangeModeRecordsWhatADraftRefuses(t *testing.T) {
	w := fixture()
	w.req.Lines[0].Quantity = kernel.MustDecimal("90")
	w.refused(t, "MOQ_NOT_MET /args/lines/0/quantity")
	w.in.Change = &Change{Revision: 2, Dates: map[string]string{"L1": "2026-11-20"}}
	p := w.prepare(t)
	l := p.Doc.Lines[0]
	if l.Delivery.SupplierDate != "2026-11-20" || l.Delivery.ConditionDate != "2026-11-15" || l.Delivery.SlackDays != -5 ||
		!slices.Equal(l.Waivers, []string{WaiverMOQNotMet}) || l.PurchaseQuantity != "90" {
		t.Fatalf("revision line %+v", l)
	}
	if e := p.Allocations[0].Fresh(); len(e) != 1 || e[0].Revision != 2 {
		t.Fatalf("entries for revision 2: %+v", e)
	}
	doc, hash, err := p.SealRevision(DocRevision{CommitmentID: "commitment.1", ChangeID: "change.1", BaseRevision: 1, Revision: 2}, "adoptable", "", "")
	if err != nil || hash != Hash(doc) || !strings.Contains(string(doc), `"contract":"`+RevisionContract+`"`) {
		t.Fatalf("sealed %s %v", doc, err)
	}
	if got, err := DecodeRevision(doc); err != nil || got.Revision == nil || got.Revision.Revision != 2 {
		t.Fatalf("decoded %+v %v", got, err)
	}
	if err := RecomputeRevision(doc, w.prepare(t), "/args/change_id"); err != nil {
		t.Fatalf("recomputes equal: %v", err)
	}
	w.in.Change.Dates["L1"] = "2026-11-18"
	if got := code(RecomputeRevision(doc, w.prepare(t), "/args/change_id")); got != "PURCHASE_DRAFT_STALE /args/change_id" {
		t.Fatal(got)
	}
	if _, err := DecodeRevision([]byte(`{}`)); err == nil {
		t.Fatal("not a revision")
	}
	// A draft's document keeps its bytes: none of the revision's members appear.
	d := fixture()
	if b, _ := canonical(d.prepare(t).Doc); strings.Contains(string(b), "condition_date") || strings.Contains(string(b), "waivers") ||
		strings.Contains(string(b), `"revision":{`) {
		t.Fatalf("draft document %s", b)
	}
}

// S5R-D41 (PUR-INV-009 "只减少数量（含整行降为零）的变更不查供应条件有效期，单价沿用当前修订；增加数量或改价仍须有效的已核实供应条件"):
// a decrease-only change is prepared on a supply condition that lapsed — SUP's SUPPLY_CONDITION_EXPIRED or
// SUPPLY_CONDITION_NOT_CURRENT is no refusal, any other verdict still is — and an all_units price group
// keeps the tier the revision in force applied: 1000 PCS at 4.50 (the tier from 1000), cut to 950 by a
// short receipt, stays at 4.50 instead of the 5.00 of the tier below. A change that is not decrease-only
// is refused on the lapsed condition and priced at the tier its quantity falls in.
func TestDecreaseOnlyChangeKeepsTheIssuedPrice(t *testing.T) {
	w := fixture()
	tiered := w.variant("offer.tiered", func(c *Condition) {
		c.PricingMode = ModeAllUnits
		c.Tiers = []Tier{{Lower: "0", Upper: "1000", UnitPrice: "5"}, {Lower: "1000", UnitPrice: "4.5"}}
	})
	w = w.with(tiered)
	w.in.Order.Lines["L1"] = line("L1", "L1", "sku.1", "1000")
	w.req.Lines[0].Quantity = kernel.MustDecimal("1000")
	issued := w.prepare(t).Doc
	if g := issued.PriceGroups[0]; g.RawAmount != "4500" || g.AppliedTiers[0].UnitPrice != "4.5" || g.PriceKept {
		t.Fatalf("issued group %+v", g)
	}
	kept := KeptPrices(issued)
	if len(kept) != 1 || kept[tiered].UnitPrice != "4.5" {
		t.Fatalf("kept prices %+v", kept)
	}
	w.req.Lines[0].Quantity = kernel.MustDecimal("950")
	s := w.in.Supply[tiered.Ref]
	s.Check = func(p string) error {
		return kernel.RejectAs(CodeSupplyConditionExpired, kernel.CategoryInvalid, p, "")
	}
	w.in.Supply[tiered.Ref] = s
	w.in.Change = &Change{Revision: 2, Dates: map[string]string{}}
	w.refused(t, "SUPPLY_CONDITION_EXPIRED /args/lines/0/source")
	w.in.Change.DecreaseOnly, w.in.Change.Prices = true, kept
	p := w.prepare(t)
	if g := p.Doc.PriceGroups[0]; g.RawAmount != "4275" || !g.PriceKept || len(g.AppliedTiers) != 1 || g.AppliedTiers[0].UnitPrice != "4.5" ||
		g.AppliedTiers[0].Quantity != "950" || !p.DecreaseOnly {
		t.Fatalf("decrease-only group %+v", g)
	}
	doc, _, err := p.SealRevision(DocRevision{CommitmentID: "commitment.1", ChangeID: "change.short", BaseRevision: 1, Revision: 2, DecreaseOnly: true},
		"adoptable", "", "")
	if err != nil || !strings.Contains(string(doc), `"decrease_only":true`) || !strings.Contains(string(doc), `"price_kept":true`) {
		t.Fatalf("sealed %s %v", doc, err)
	}
	if err = RecomputeRevision(doc, w.prepare(t), "/args/change_id"); err != nil {
		t.Fatalf("recomputes equal: %v", err)
	}
	if !Lapsed(kernel.RejectAs(CodeSupplyConditionNotCurrent, kernel.CategoryConflict, "/", "")) ||
		Lapsed(kernel.Reject(kernel.CodeReferenceInvalid, "/", "")) || Lapsed(nil) {
		t.Fatal("lapsed")
	}
	s.Check = func(p string) error { return kernel.Reject(kernel.CodeReferenceInvalid, p, "") }
	w.in.Supply[tiered.Ref] = s
	w.refused(t, "REFERENCE_INVALID /args/lines/0/source")
	// Not decrease-only: the tier the quantity falls in.
	s.Check = inForce
	w.in.Supply[tiered.Ref] = s
	w.in.Change.DecreaseOnly, w.in.Change.Prices = false, nil
	if g := w.prepare(t).Doc.PriceGroups[0]; g.RawAmount != "4750" || g.PriceKept {
		t.Fatalf("repriced group %+v", g)
	}
}
