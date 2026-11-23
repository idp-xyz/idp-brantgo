package order

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/acceptance"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// multiQuote is a sealed multi_order quote of one line (TRD-INV-024). quantity, price and the
// line's cap are the test's. uncapped ignores max. The payment plan is one stage of 100 percent
// unless stages is set.
func multiQuote(t *testing.T, quantity, price, max string, uncapped bool, stages []quote.Payment) (*quote.Quote, *acceptance.Acceptance) {
	t.Helper()
	if stages == nil {
		stages = []quote.Payment{{StageID: "all", Percent: "100", Trigger: "order_confirmed", AmountMinor: "0"}}
	}
	line := quote.Line{LineID: "L1", Quantity: quantity, UnitPrice: price, PriceBasisQuantity: "1", DiscountRatio: "0",
		MaxOrderQuantity: max, Uncapped: uncapped}
	c := quote.Commercial{QuoteID: "quote.1", BusinessRevision: 1, IssuedAt: "2026-10-21T08:00:00Z",
		Data: quote.Terms{Customer: customer, Contact: contact, Currency: quote.Ref{Kind: "currency", ID: "usd", Revision: 1},
			TradeTerms: quote.Ref{Kind: "trade_terms", ID: "fob", Revision: 1}, PaymentTerms: quote.Ref{Kind: "payment_terms", ID: "pay.1", Revision: 1},
			ExpiresAt: kernel.FormatTimestamp(expires), Lines: []quote.Line{line}, Charges: []quote.Charge{}, ConversionMode: quote.ModeMultiOrder},
		Calculation: &quote.Calculation{Lines: []quote.AmountLine{}, Charges: []quote.AmountLine{}, NetMinor: "0", TaxMinor: "0", PayableMinor: "0",
			ExcludedCostCategories: []string{}, Payments: stages, CalculationVersion: quote.CalculationVersion},
		NormalizationVersion: quote.NormalizationVersion}
	tree, err := quote.TreeOf(c)
	if err != nil {
		t.Fatal(err)
	}
	doc, hash, err := quote.Digest(tree)
	if err != nil {
		t.Fatal(err)
	}
	q := quote.New("quote.1", "clerk", "trade")
	q.Revisions = []*quote.Revision{{Number: 1, Status: quote.Active, Seal: &quote.Seal{Commercial: doc, BusinessHash: hash, ExpiresAt: expires},
		Formal: &quote.Formalization{Document: quote.Document{Number: "QT-2026-00001", ContentSHA256: strings.Repeat("d", 64)}}}}
	q.RootVersion = 5
	a := &acceptance.Acceptance{ID: "acceptance.1", QuoteID: "quote.1", Revision: 1, BusinessHash: hash, Number: "QT-2026-00001",
		Customer: customer, Contact: contact, LineIDs: []string{"L1"}, ArrivalAt: arrived, ArrivalBasis: "channel_server",
		ExpiresAt: expires, Effectivity: acceptance.Valid, Version: 1}
	return q, a
}

func partialPrep(q *quote.Quote, order, qty string, consumed map[string]string) PartialPreparation {
	return PartialPreparation{Preparation: Preparation{OrderID: order, QuoteID: "quote.1", Revision: 1,
		BusinessHash: q.Revision(1).Seal.BusinessHash, AcceptanceID: "acceptance.1"},
		Lines: []PartialLine{{LineID: "L1", Quantity: qty}}, Currency: kernel.MustCurrency("USD", 2), Consumed: consumed}
}

func payableOf(t *testing.T, o *Order) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(o.Snapshot, &body); err != nil {
		t.Fatal(err)
	}
	calc := body["calculation"].(map[string]any)
	return calc["payable_minor"].(string)
}

// TRD-INV-024: a default quote omits conversion mode and caps, a multi_order quote of 1,200 takes
// 700 then 500 and refuses 1 more, an uncapped line takes 2,000, and order.prepare on that quote
// is CONVERSION_SLOT_CONFLICT. Money is exact minor units. The partial snapshot rebuilds.
func TestPartialOrdersSpendTheQuoteLine(t *testing.T) {
	raw, err := json.Marshal(quote.Line{LineID: "L1", Quantity: "1", UnitPrice: "1", PriceBasisQuantity: "1", DiscountRatio: "0"})
	if err != nil || strings.Contains(string(raw), "conversion_mode") || strings.Contains(string(raw), "max_order_quantity") || strings.Contains(string(raw), "uncapped") {
		t.Fatalf("a default line mentions a cap: %s", raw)
	}
	inside := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	q, a := multiQuote(t, "1200", "10", "", false, nil)
	first, err := PreparePartial(partialPrep(q, "order.1", "700", nil), q, a, scope, clerk, 3, inside)
	if err != nil {
		t.Fatal(err)
	}
	if first.Slot != quote.SlotMultiOrder || q.Slot != nil || !q.Consumed || q.RootVersion != 6 || payableOf(t, first) != "700000" {
		t.Fatalf("first order slot %s consumed %v version %d payable %s", first.Slot, q.Consumed, q.RootVersion, payableOf(t, first))
	}
	if _, err := first.Verify(scope, "/x"); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(preparation(q), q, a, scope, clerk, 3, inside); code(err) != "CONVERSION_SLOT_CONFLICT /args/quote_id" {
		t.Fatalf("order.prepare on multi_order: %s", code(err))
	}
	second, err := PreparePartial(partialPrep(q, "order.2", "500", map[string]string{"L1": "700"}), q, a, scope, clerk, 4, inside)
	if err != nil {
		t.Fatal(err)
	}
	if payableOf(t, second) != "500000" {
		t.Fatalf("second payable %s", payableOf(t, second))
	}
	if _, err := PreparePartial(partialPrep(q, "order.3", "1", map[string]string{"L1": "1200"}), q, a, scope, clerk, 5, inside); code(err) != "QUOTE_REMAINING_INSUFFICIENT /args/lines/0/quantity" {
		t.Fatalf("over the cap: %s", code(err))
	}
	// Cancelling does not put quantity back: the ledger the caller still holds refuses the repeat.
	if _, err := PreparePartial(partialPrep(q, "order.4", "700", map[string]string{"L1": "700"}), q, a, scope, clerk, 6, inside); code(err) != "QUOTE_REMAINING_INSUFFICIENT /args/lines/0/quantity" {
		t.Fatalf("cancel keeps the ledger: %s", code(err))
	}

	plain, acc := accepted(t)
	if _, err := PreparePartial(partialPrep(plain, "order.p", "1", nil), plain, acc, scope, clerk, 3, inside); code(err) != "CONVERSION_SLOT_UNSUPPORTED /args/quote_id" {
		t.Fatalf("partial on single_full: %s", code(err))
	}
	open, openAcc := multiQuote(t, "1200", "10", "", true, nil)
	repeat, err := PreparePartial(partialPrep(open, "order.r", "2000", nil), open, openAcc, scope, clerk, 3, inside)
	if err != nil || payableOf(t, repeat) != "2000000" {
		t.Fatalf("uncapped repeat: %v %s", err, payableOf(t, repeat))
	}
	if _, err := PreparePartial(partialPrep(q, "order.late", "1", nil), q, a, scope, clerk, 3, prepareOn); code(err) != "QUOTE_EXPIRED /args/quote_id" {
		t.Fatalf("after expiry: %s", code(err))
	}
}

// A unit price of 0.005 rounds HALF_UP to one cent, and a stated payment plan must sum to the
// payable. The quote's own percentages are the plan when the caller states none.
func TestPartialRepricesHalfUpAndThePaymentPlan(t *testing.T) {
	inside := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	q, a := multiQuote(t, "1", "0.005", "", true, nil)
	o, err := PreparePartial(partialPrep(q, "order.1", "1", nil), q, a, scope, clerk, 3, inside)
	if err != nil {
		t.Fatal(err)
	}
	if payableOf(t, o) != "1" {
		t.Fatalf("HALF_UP payable %s", payableOf(t, o))
	}
	var body map[string]any
	if err := json.Unmarshal(o.Snapshot, &body); err != nil {
		t.Fatal(err)
	}
	pays := body["calculation"].(map[string]any)["payments"].([]any)
	if body["payment_source"] != "quoted" || pays[0].(map[string]any)["amount_minor"] != "1" {
		t.Fatalf("quoted plan %+v", body["calculation"])
	}
	stated := partialPrep(q, "order.2", "1", nil)
	stated.Payments = []PartialStage{{StageID: "all", AmountMinor: "2"}}
	if _, err := PreparePartial(stated, q, a, scope, clerk, 4, inside); code(err) != "PAYMENT_TOTAL_MISMATCH /args/payment_plan" {
		t.Fatalf("a plan that does not sum: %s", code(err))
	}
}
