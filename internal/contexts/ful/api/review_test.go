package api

import (
	"strings"
	"testing"
)

// The closed contracts of the shipment review (S5-09b; SM-FUL-SHIPMENT-REVIEW): the consumer's
// shipment_review.open states the event's order binding and, for an amendment only, the amended
// revision's lines once each; shipment_review.settle states each item once with a known disposition,
// a replacement shipment exactly for replaced, a reason for each item, and a reason for a review
// without items.
func TestShipmentReviewContracts(t *testing.T) {
	open := OpenShipmentReview{IdempotencyKey: "evt.1", OrderID: "order.1", OrderRevision: 2, OrderHash: strings.Repeat("a", 64), OrderVersion: 5,
		Cause: CauseOrderAmended, EventID: "evt.1", Lines: []ReviewTarget{{OrderLineID: "L1", Quantity: "110", SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS"}}}
	if err := open.Validate(); err != nil {
		t.Fatal(err)
	}
	for want, edit := range map[string]func(*OpenShipmentReview){
		"SCHEMA_INVALID /idempotency_key":               func(c *OpenShipmentReview) { c.IdempotencyKey = "" },
		"SCHEMA_INVALID /args/order_revision":           func(c *OpenShipmentReview) { c.OrderRevision = 0 },
		"SCHEMA_INVALID /args/order_hash":               func(c *OpenShipmentReview) { c.OrderHash = strings.Repeat("A", 64) },
		"SCHEMA_INVALID /args/order_version":            func(c *OpenShipmentReview) { c.OrderVersion = 0 },
		"SCHEMA_INVALID /args/cause":                    func(c *OpenShipmentReview) { c.Cause = "order_closed" },
		"SCHEMA_INVALID /args/event_id":                 func(c *OpenShipmentReview) { c.EventID = "evt 1" },
		"SCHEMA_INVALID /args/lines":                    func(c *OpenShipmentReview) { c.Cause = CauseOrderCancelled },
		"SCHEMA_INVALID /args/lines/0/quantity":         func(c *OpenShipmentReview) { c.Lines[0].Quantity = "0" },
		"SCHEMA_INVALID /args/lines/0/sku_id":           func(c *OpenShipmentReview) { c.Lines[0].SpecRevision = 0 },
		"SCHEMA_INVALID /args/lines/0/unit_id":          func(c *OpenShipmentReview) { c.Lines[0].UnitID = "" },
		"DUPLICATE_LINE_ID /args/lines/1/order_line_id": func(c *OpenShipmentReview) { c.Lines = append(c.Lines, c.Lines[0]) },
	} {
		c := open
		c.Lines = append([]ReviewTarget(nil), open.Lines...)
		edit(&c)
		if outcome(c.Validate()) != want {
			t.Errorf("%s: %s", want, outcome(c.Validate()))
		}
	}
	end := open
	end.Cause, end.Lines = CauseOrderRemainderClosed, nil
	if err := end.Validate(); err != nil {
		t.Fatal(err)
	}

	settle := SettleShipmentReview{IdempotencyKey: "k", ReviewID: "shr.1", ExpectedVersion: 1, Items: []ReviewItemDisposition{
		{ItemNo: 1, Disposition: DispositionReplaced, ShipmentID: "shipment.3", Reason: "按修订 2 重排"},
		{ItemNo: 2, Disposition: DispositionShipmentCancelled, Reason: "随出运取消"}}}
	if err := settle.Validate(); err != nil {
		t.Fatal(err)
	}
	for want, edit := range map[string]func(*SettleShipmentReview){
		"SCHEMA_INVALID /args/review_id":            func(c *SettleShipmentReview) { c.ReviewID = "" },
		"SCHEMA_INVALID /args/expected_version":     func(c *SettleShipmentReview) { c.ExpectedVersion = 0 },
		"SCHEMA_INVALID /args/reason":               func(c *SettleShipmentReview) { c.Items = nil },
		"SCHEMA_INVALID /args/items/1/item_no":      func(c *SettleShipmentReview) { c.Items[1].ItemNo = 1 },
		"SCHEMA_INVALID /args/items/0/item_no":      func(c *SettleShipmentReview) { c.Items[0].ItemNo = 0 },
		"SCHEMA_INVALID /args/items/1/disposition":  func(c *SettleShipmentReview) { c.Items[1].Disposition = "voided" },
		"SCHEMA_INVALID /args/items/0/shipment_id":  func(c *SettleShipmentReview) { c.Items[0].ShipmentID = "" },
		"SCHEMA_INVALID /args/items/1/shipment_id":  func(c *SettleShipmentReview) { c.Items[1].ShipmentID = "shipment.3" },
		"SCHEMA_INVALID /args/items/1/reason":       func(c *SettleShipmentReview) { c.Items[1].Reason = "" },
		"SCHEMA_INVALID /args/items/0/shipment_id ": func(c *SettleShipmentReview) { c.Items[0].ShipmentID = "shipment 3" },
	} {
		c := settle
		c.Items = append([]ReviewItemDisposition(nil), settle.Items...)
		edit(&c)
		if outcome(c.Validate()) != strings.TrimSpace(want) {
			t.Errorf("%s: %s", want, outcome(c.Validate()))
		}
	}
	empty := SettleShipmentReview{IdempotencyKey: "k", ReviewID: "shr.1", ExpectedVersion: 1, Reason: "合成：被取代前无处置"}
	if err := empty.Validate(); err != nil {
		t.Fatal(err)
	}
}
