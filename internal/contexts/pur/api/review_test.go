package api

import (
	"strings"
	"testing"
)

// The closed contracts of the allocation review commands (S5-09b; SM-PUR-ALLOCATION-REVIEW).
func TestAllocationReviewContracts(t *testing.T) {
	open := func(edit func(c *OpenAllocationReview)) error {
		c := OpenAllocationReview{IdempotencyKey: "evt-1", OrderID: "order.1", OrderRevision: 2, OrderHash: strings.Repeat("a", 64), OrderVersion: 7,
			Cause: CauseOrderAmended, EventID: "evt-1", Lines: []ReviewTarget{{OrderLineID: "L1", Quantity: "80", SkuID: "sku.1", SpecRevision: 1,
				UnitID: "PCS"}}}
		edit(&c)
		return c.Validate()
	}
	for _, tc := range []struct {
		edit func(c *OpenAllocationReview)
		want string
	}{
		{func(*OpenAllocationReview) {}, "OK"},
		{func(c *OpenAllocationReview) { c.Cause, c.Lines = CauseOrderCancelled, nil }, "OK"},
		{func(c *OpenAllocationReview) { c.Lines[0].Quantity, c.Lines[0].SkuID, c.Lines[0].UnitID = "0", "", "" }, "OK"},
		{func(c *OpenAllocationReview) { c.OrderHash = "x" }, "SCHEMA_INVALID /args/order_hash"},
		{func(c *OpenAllocationReview) { c.OrderVersion = 0 }, "SCHEMA_INVALID /args/order_version"},
		{func(c *OpenAllocationReview) { c.Cause = "order_closed" }, "SCHEMA_INVALID /args/cause"},
		{func(c *OpenAllocationReview) { c.Lines[0].Quantity = "-1" }, "SCHEMA_INVALID /args/lines/0/quantity"},
		{func(c *OpenAllocationReview) { c.Lines[0].SpecRevision = 0 }, "SCHEMA_INVALID /args/lines/0/sku_id"},
		{func(c *OpenAllocationReview) { c.Lines = append(c.Lines, c.Lines[0]) }, "DUPLICATE_LINE_ID /args/lines/1/order_line_id"},
	} {
		if got := pathOf(open(tc.edit)); got != tc.want {
			t.Errorf("open: %s, want %s", got, tc.want)
		}
	}
	settle := func(edit func(c *SettleAllocationReview)) error {
		c := SettleAllocationReview{IdempotencyKey: "k", ReviewID: "alr-1", ExpectedVersion: 1, Items: []ReviewItemDisposition{
			{ItemNo: 1, Disposition: DispositionCommitmentChanged, ChangeID: "change.1", Reason: "供应商确认减量"},
			{ItemNo: 2, Disposition: DispositionDraftVoided, DraftID: "draft.2", Reason: "作废"},
			{ItemNo: 3, Disposition: DispositionCommitmentCancelled, CommitmentID: "c.1", Reason: "取消"},
			{ItemNo: 4, Disposition: DispositionNoAction, Reason: "已交付的货物保留"}}}
		edit(&c)
		return c.Validate()
	}
	for _, tc := range []struct {
		edit func(c *SettleAllocationReview)
		want string
	}{
		{func(*SettleAllocationReview) {}, "OK"},
		{func(c *SettleAllocationReview) { c.Items, c.Reason = nil, "合成：没有可处置的项" }, "OK"},
		{func(c *SettleAllocationReview) { c.Items = nil }, "SCHEMA_INVALID /args/reason"},
		{func(c *SettleAllocationReview) { c.ExpectedVersion = 0 }, "SCHEMA_INVALID /args/expected_version"},
		{func(c *SettleAllocationReview) { c.Items[1].ItemNo = 1 }, "SCHEMA_INVALID /args/items/1/item_no"},
		{func(c *SettleAllocationReview) { c.Items[0].ItemNo = 0 }, "SCHEMA_INVALID /args/items/0/item_no"},
		{func(c *SettleAllocationReview) { c.Items[0].Disposition = "voided" }, "SCHEMA_INVALID /args/items/0/disposition"},
		{func(c *SettleAllocationReview) { c.Items[3].Reason = "" }, "SCHEMA_INVALID /args/items/3/reason"},
		{func(c *SettleAllocationReview) { c.Items[0].ChangeID = "" }, "SCHEMA_INVALID /args/items/0/change_id"},
		{func(c *SettleAllocationReview) { c.Items[1].ChangeID = "change.1" }, "SCHEMA_INVALID /args/items/1/change_id"},
		{func(c *SettleAllocationReview) { c.Items[3].DraftID = "draft.2" }, "SCHEMA_INVALID /args/items/3/draft_id"},
		{func(c *SettleAllocationReview) { c.Items[2].CommitmentID = "bad id" }, "SCHEMA_INVALID /args/items/2/commitment_id"},
	} {
		if got := pathOf(settle(tc.edit)); got != tc.want {
			t.Errorf("settle: %s, want %s", got, tc.want)
		}
	}
}
