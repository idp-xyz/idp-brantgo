package charge

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5R-D24 / STL-INV-010 negative "FCA 订单记录主运费返回 INVALID_KNOWN_VALUE": the main carriage of an order whose buyer bears
// it (EXW, FCA, FAS, FOB) is refused unless recharged; the cost before delivery (pre_carriage, export_clearance) and the
// main carriage of a C or D term order are ours.
func TestS5RMainCarriageFollowsTheIncoterm(t *testing.T) {
	for _, term := range []string{"EXW", "FCA", "FAS", "FOB"} {
		err := CheckComponent(ComponentMainCarriage, false, term, "order.1")
		if r, ok := kernel.AsRejection(err); !ok || r.Code != kernel.CodeInvalidKnownValue || r.Fields[0].Path != "/args/cost_component" {
			t.Fatalf("%s: %v", term, err)
		}
		if err := CheckComponent(ComponentMainCarriage, true, term, "order.1"); err != nil {
			t.Fatalf("%s recharged: %v", term, err)
		}
		for _, c := range []string{ComponentPreCarriage, ComponentExportClearance, ComponentInsurance, ComponentOther} {
			if err := CheckComponent(c, false, term, "order.1"); err != nil {
				t.Fatalf("%s %s: %v", term, c, err)
			}
		}
	}
	for _, term := range []string{"CIF", "CFR", "CPT", "CIP", "DAP", "DPU", "DDP", ""} {
		if err := CheckComponent(ComponentMainCarriage, false, term, "order.1"); err != nil {
			t.Fatalf("%s: %v", term, err)
		}
	}
	ten, _ := kernel.NewMoney(kernel.MustCurrency("CNY", 2), 1000)
	if _, err := Record(RecordInput{ID: "c", Status: StatusPending, Amount: ten, CostComponent: "freight",
		Beneficiaries: []Target{{Kind: TargetOrder, OrderID: "o"}}}); err == nil {
		t.Fatal("an unknown component")
	}
}
