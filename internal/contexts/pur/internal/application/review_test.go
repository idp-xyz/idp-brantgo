package application

import (
	"errors"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
)

// What an order change says the order needs (S5-09b; WF-06 step 4): an amendment the lines of TRD's
// order at exactly the amended revision — an order TRD has moved past is left to the later
// revision's event, one TRD does not show, or shows at another hash, is refused for good —; an end
// what each line shipped.
func TestReviewTargets(t *testing.T) {
	hash := strings.Repeat("a", 64)
	e := OrderChange{EventID: "evt-1", Cause: api.CauseOrderAmended, OrderID: "order.1", Revision: 2, Hash: hash, Version: 6}
	at := func(rev int64, h string) ReviewBasis {
		return ReviewBasis{Order: &Order{Binding: OrderBinding{OrderID: "order.1", OrderRevision: rev, OrderHash: h},
			Lines: []OrderLine{{LineID: "L1", SkuID: "sku.1", SpecRevision: 1, Quantity: "80", Unit: OrderUnit{ID: "PCS", Revision: 1}}}}}
	}
	lines, effect, err := targets(e, at(2, hash))
	if err != nil || effect != "" || len(lines) != 1 || lines[0] != (api.ReviewTarget{OrderLineID: "L1", Quantity: "80", SkuID: "sku.1", SpecRevision: 1,
		UnitID: "PCS"}) {
		t.Fatalf("amended %+v %q %v", lines, effect, err)
	}
	if _, effect, err = targets(e, at(3, strings.Repeat("c", 64))); err != nil || effect != EffectLaterRevision {
		t.Fatalf("TRD moved on: %q %v", effect, err)
	}
	var mismatch *ReviewMismatch
	for name, b := range map[string]ReviewBasis{"no order": {}, "another hash": at(2, strings.Repeat("c", 64)), "an older revision": at(1, hash)} {
		if _, _, err = targets(e, b); !errors.As(err, &mismatch) || mismatch.Code != FailureReviewNotOpenable {
			t.Errorf("%s: %v", name, err)
		}
	}
	end := OrderChange{EventID: "evt-2", Cause: api.CauseOrderRemainderClosed, OrderID: "order.1", Revision: 2, Hash: hash, Version: 7,
		Lines: []EndedLine{{LineID: "L1", Shipped: "116"}, {LineID: "L2", Shipped: "0"}}}
	if lines, _, err = targets(end, ReviewBasis{}); err != nil || len(lines) != 2 || lines[0].Quantity != "116" || lines[1].Quantity != "0" ||
		lines[0].SkuID != "" {
		t.Fatalf("remainder closed %+v %v", lines, err)
	}
	end.Lines[0].Shipped = "-1"
	if _, _, err = targets(end, ReviewBasis{}); !errors.As(err, &mismatch) {
		t.Fatalf("a negative shipment: %v", err)
	}
}
