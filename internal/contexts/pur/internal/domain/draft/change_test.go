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
