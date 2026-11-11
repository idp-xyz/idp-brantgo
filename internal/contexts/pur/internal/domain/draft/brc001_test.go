package draft

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// BRC-001's purchase draft in its archive shape (event storming ES-01 steps 3, 7 and 19; WBS S3-12):
// four order lines, ST01 red/blue × S/M of 24, 36, 12 and 48 PCS, bought from supplier.A's offer at
// 5.00 USD, whose four condition lines share an MOQ of 100 PCS over the explicit group ST01 (MOQ
// 100 按 ST01 款号合计). The whole order is one 600.00 USD draft with a price group per condition
// line; two lines alone (60 PCS) do not meet the group's MOQ, and quantities are never raised.
func TestBRC001FourLinesMeetTheStyleMOQTogether(t *testing.T) {
	type spec struct{ line, sku, qty string }
	lines := []spec{{"L1", "st01.red.s", "24"}, {"L2", "st01.red.m", "36"}, {"L3", "st01.blue.s", "12"}, {"L4", "st01.blue.m", "48"}}
	build := func(ids ...string) *world {
		w := fixture()
		w.in.Order.Lines = map[string]OrderLine{}
		costed := map[string]CostedLine{}
		var conditions []Condition
		for _, l := range lines {
			w.in.Order.Lines[l.line] = line(l.line, l.line, l.sku, l.qty)
			w.in.Specs[Spec{SkuID: l.sku, Revision: 1}] = inForce
			c := condition("OL"+l.line[1:], l.sku)
			c.MOQ.Scope, c.MOQ.GroupID, c.MOQ.Members = MOQScopeExplicitGroup, "ST01", []string{"L1", "L2", "L3", "L4"}
			conditions = append(conditions, c)
			minor := kernel.MustDecimal(l.qty).Mul(kernel.DecimalFromInt(500))
			costed[l.line] = CostedLine{Sources: []Source{{Ref: offer1, ConditionLineID: c.ID}}, PurchaseMinor: &minor}
		}
		s := offer(offer1, "supplier.1", conditions...)
		s.RfqLineIDs = []string{"L1", "L2", "L3", "L4"}
		w.in.Supply[offer1] = s
		w.in.Costing.Lines = costed
		w.req.Lines = nil
		for _, id := range ids {
			for _, l := range lines {
				if l.line == id {
					w.req.Lines = append(w.req.Lines, RequestLine{OrderLineID: id, Quantity: kernel.MustDecimal(l.qty),
						Source: Source{Ref: offer1, ConditionLineID: "OL" + id[1:]}})
				}
			}
		}
		return w
	}
	build("L1", "L2").refused(t, "MOQ_NOT_MET /args/lines/0/quantity")
	p := build("L1", "L2", "L3", "L4").prepare(t)
	if p.Doc.Totals.NetMinor != "60000" || len(p.Doc.PriceGroups) != 4 || len(p.Allocations) != 4 {
		t.Fatalf("totals %+v, %d groups", p.Doc.Totals, len(p.Doc.PriceGroups))
	}
	for i, want := range []string{"12000", "18000", "6000", "24000"} {
		l := p.Doc.Lines[i]
		if l.NetMinor != want || l.MOQ.Purchased != "120" || !l.CostingReference.SameSource || *l.CostingReference.CostVarianceMinor != "0" {
			t.Fatalf("line %s: %+v", l.OrderLineID, l)
		}
	}
}
