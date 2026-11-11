package api

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

func valid() PrepareDraft {
	return PrepareDraft{IdempotencyKey: "k1", DraftID: "purchase.a46", OrderID: "order.1",
		Lines:           []DraftLine{{OrderLineID: "L1", Quantity: "120", Source: SourceRef{Kind: KindSupplierOffer, ID: "offer.1", Revision: 1, ConditionLineID: "OL1"}}},
		SelectionReason: "Synthetic costed supplier condition"}
}

// Python test_lines_sources_and_reason_must_be_explicit (PYT-0561), the structural half: no lines,
// a line without a source or with a malformed one, another kind (logistics offers are not bought
// under), a revision that is not a positive integer, a reason that is empty, blank, over 1000
// characters or holds a control character, a malformed draft ID, a repeated order line
// (DUPLICATE_LINE_ID); and Go's allocated quantity and overbuy.
func TestPrepareDraftContract(t *testing.T) {
	for want, edit := range map[string]func(c *PrepareDraft){
		"OK":                              func(*PrepareDraft) {},
		"OK ":                             func(c *PrepareDraft) { c.Lines[0].Overbuy = &Overbuy{Quantity: "30"} },
		"SCHEMA_INVALID /idempotency_key": func(c *PrepareDraft) { c.IdempotencyKey = "" },
		"SCHEMA_INVALID /args/draft_id":   func(c *PrepareDraft) { c.DraftID = "采购 草稿" },
		"SCHEMA_INVALID /args/order_id":   func(c *PrepareDraft) { c.OrderID = "" },
		"SCHEMA_INVALID /args/lines":      func(c *PrepareDraft) { c.Lines = nil },
		"SCHEMA_INVALID /args/lines/0/order_line_id":   func(c *PrepareDraft) { c.Lines[0].OrderLineID = "L 1" },
		"SCHEMA_INVALID /args/lines/0/quantity":        func(c *PrepareDraft) { c.Lines[0].Quantity = "0" },
		"SCHEMA_INVALID /args/lines/0/source/kind":     func(c *PrepareDraft) { c.Lines[0].Source.Kind = "logistics_offer" },
		"SCHEMA_INVALID /args/lines/0/source/id":       func(c *PrepareDraft) { c.Lines[0].Source.ID = "" },
		"SCHEMA_INVALID /args/lines/0/source/revision": func(c *PrepareDraft) { c.Lines[0].Source.Revision = 0 },
		"SCHEMA_INVALID /args/lines/0/source/condition_line_id": func(c *PrepareDraft) {
			c.Lines[0].Source.ConditionLineID = ""
		},
		"SCHEMA_INVALID /args/lines/0/overbuy/quantity": func(c *PrepareDraft) { c.Lines[0].Overbuy = &Overbuy{Quantity: "-1"} },
		"SCHEMA_INVALID /args/lines/0/overbuy/reason": func(c *PrepareDraft) {
			c.Lines[0].Overbuy = &Overbuy{Quantity: "1", Reason: "bad\x00reason"}
		},
		"DUPLICATE_LINE_ID /args/lines/1/order_line_id": func(c *PrepareDraft) { c.Lines = append(c.Lines, c.Lines[0]) },
		"SCHEMA_INVALID /args/selection_reason":         func(c *PrepareDraft) { c.SelectionReason = " " },
		"SCHEMA_INVALID /args/selection_reason ":        func(c *PrepareDraft) { c.SelectionReason = strings.Repeat("x", 1001) },
		"SCHEMA_INVALID /args/selection_reason  ":       func(c *PrepareDraft) { c.SelectionReason = "synthetic\x00reason" },
		"SCHEMA_INVALID /args/selection_reason   ":      func(c *PrepareDraft) { c.SelectionReason = "" },
	} {
		c := valid()
		edit(&c)
		if got := code(c.Validate()); got != strings.TrimSpace(want) {
			t.Errorf("want %s, got %s", want, got)
		}
	}
	many := valid()
	for i := range MaxLines {
		many.Lines = append(many.Lines, DraftLine{OrderLineID: "X" + strings.Repeat("1", i%60+1) + string(rune('a'+i/60)), Quantity: "1",
			Source: many.Lines[0].Source})
	}
	if code(many.Validate()) != "SCHEMA_INVALID /args/lines" {
		t.Fatal("at most 200 lines")
	}
}

func TestVoidDraftContract(t *testing.T) {
	ok := VoidDraft{IdempotencyKey: "k1", DraftID: "purchase.a46", ExpectedVersion: 1, Reason: "Wrong supply condition chosen"}
	for want, edit := range map[string]func(c *VoidDraft){
		"OK":                              func(*VoidDraft) {},
		"SCHEMA_INVALID /idempotency_key": func(c *VoidDraft) { c.IdempotencyKey = "bad key" },
		"SCHEMA_INVALID /args/draft_id":   func(c *VoidDraft) { c.DraftID = "" },
		"SCHEMA_INVALID /args/expected_version": func(c *VoidDraft) {
			c.ExpectedVersion = 0
		},
		"SCHEMA_INVALID /args/reason": func(c *VoidDraft) { c.Reason = "" },
	} {
		c := ok
		edit(&c)
		if got := code(c.Validate()); got != want {
			t.Errorf("want %s, got %s", want, got)
		}
	}
}

func TestCodes(t *testing.T) {
	if c, ok := Category(CodePurchaseAllocationExceeded); !ok || c != kernel.CategoryConflict {
		t.Fatal("PURCHASE_ALLOCATION_EXCEEDED is 409")
	}
	if c, _ := Category(CodeOrderLineNotFound); c != kernel.CategoryNotFound {
		t.Fatal("ORDER_LINE_NOT_FOUND is 404")
	}
	if r := Reject(CodeMOQNotMet, "/p"); r.Category != kernel.CategoryInvalid || r.Fields[0].Path != "/p" {
		t.Fatalf("%+v", r)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an undeclared code panics")
		}
	}()
	_ = Reject("NOT_A_CODE", "/")
}
