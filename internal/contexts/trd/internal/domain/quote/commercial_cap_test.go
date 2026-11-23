package quote

import "testing"

// sealedDraft is enough of a draft for sealedTerms to copy. The members it dereferences are
// present and empty; the test is about the cap fields only.
func sealedDraft(mode string, lines []Line) Data {
	return Data{Issuer: &Ref{}, Customer: &PartyRef{}, Contact: &ContactRef{}, Profile: &ProfileRef{},
		Rfq: &RfqRef{}, Snapshot: &SnapshotRef{}, Currency: &Ref{}, PricingPolicy: &Ref{}, TradeTerms: &Ref{},
		NamedPlace: &BusinessRef{}, PaymentTerms: &Ref{}, Delivery: &Delivery{}, DocumentTemplate: &Ref{}, Sequence: &Ref{},
		ConversionMode: mode, Lines: lines}
}

// Sealing a quote that is not multi_order clears max_order_quantity and uncapped, so a document
// that omitted them stays byte-for-byte what it was. multi_order keeps the cap and trims it.
func TestSealedTermsClearsCapsUnlessMultiOrder(t *testing.T) {
	both := Line{LineID: "L1", Quantity: "1200", MaxOrderQuantity: "10.500", Uncapped: true}
	for _, mode := range []string{"", ModeSingleFull} {
		got := sealedTerms(sealedDraft(mode, []Line{both}))
		if got.ConversionMode != "" || got.Lines[0].MaxOrderQuantity != "" || got.Lines[0].Uncapped {
			t.Fatalf("mode %q kept a cap: %+v", mode, got.Lines[0])
		}
	}
	capped := sealedTerms(sealedDraft(ModeMultiOrder, []Line{{LineID: "L1", Quantity: "1200", MaxOrderQuantity: "10.500"}}))
	if capped.ConversionMode != ModeMultiOrder || capped.Lines[0].MaxOrderQuantity != "10.5" || capped.Lines[0].Uncapped {
		t.Fatalf("multi_order cap: %+v", capped.Lines[0])
	}
	open := sealedTerms(sealedDraft(ModeMultiOrder, []Line{{LineID: "L1", Quantity: "1200", Uncapped: true}}))
	if open.ConversionMode != ModeMultiOrder || !open.Lines[0].Uncapped || open.Lines[0].MaxOrderQuantity != "" {
		t.Fatalf("multi_order uncapped: %+v", open.Lines[0])
	}
}
