package application

import (
	"reflect"
	"strings"
	"testing"
	"time"

	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The price proposal in the contract's form (S4 unit G4a; owner decision 2026-11-18): what INQ, POL
// and CST said becomes one line per RFQ line with decimal strings, never floats — a formula price
// with the currency's digits, a list price as published, the snapshot's qualification, and the
// margin of each price on the snapshot's cost. The rules are calc.ProposePrices' (proposal_test.go
// there); the composed read runs on PostgreSQL 16 (internal/bootstrap).
func TestProposalInTheContractsForm(t *testing.T) {
	usd := kernel.MustCurrency("USD", 2)
	at := time.Date(2026, 11, 18, 6, 0, 0, 123456789, time.UTC)
	hash := strings.Repeat("a", 64)
	q := api.PriceProposalQuery{RfqID: "rfq.1", Currency: "USD", CostSnapshotRef: &api.CostSnapshotRef{SnapshotID: "snapshot.1", ResultHash: hash},
		PricingPolicyRef: api.ConfigRef{Kind: api.PricingPolicyKind, ID: "policy.1", Revision: 1}}
	in := proposalInputs{
		rfq: inqapi.RfqRef{RfqID: "rfq.1", RowVersion: 2, Status: inqapi.StatusReady, CustomerID: "customer.1", ContactID: "contact.1",
			Lines: []inqapi.RfqRefLine{{LineID: "L1", SkuID: "ST01.red.S", SkuSpecRevision: 1, Quantity: "24", UOM: "PCS"},
				{LineID: "L2", SkuID: "ST01.red.M", SkuSpecRevision: 1, Quantity: "36", UOM: "PCS"},
				{LineID: "L3", SkuID: "ST01.blue.S", SkuSpecRevision: 1, Quantity: "12", UOM: "PCS"}}},
		policy: polapi.PricingPolicy{Name: "policy", CalculationCurrency: "USD", MinimumMargin: "0.25", TargetMargin: "0.3125", QuoteValidDays: 7},
		prices: polapi.SalesPrices{Currency: usd, Prices: []polapi.SalesPriceInForce{{
			Ref: polapi.ConfigRef{Kind: polapi.KindSalesPrice, ConfigID: "price.red.M", Revision: 2}, SkuID: "ST01.red.M", Unit: "PCS",
			UnitPrice: kernel.MustDecimal("8.4"), PriceBasisQuantity: kernel.MustDecimal("1")}}},
		snapshot: &cstapi.ResolvedSnapshot{Ref: cstapi.CostSnapshotRef{SnapshotID: "snapshot.1", ResultHash: hash}, EvidenceHash: "evidence",
			Rfq: cstapi.RfqBinding{RfqID: "rfq.1", RowVersion: 2}, Currency: "USD", TotalMinor: "33000",
			Lines: []cstapi.LineTotal{{LineID: "L1", Quantity: "24", Unit: "PCS", AmountMinor: "13200"},
				{LineID: "L2", Quantity: "36", Unit: "PCS", AmountMinor: "19800"}},
			Adoptability: cstapi.Adoptability{Status: cstapi.AdoptionAdoptable}},
	}
	got, err := proposal(q, in, at)
	if err != nil {
		t.Fatal(err)
	}
	want := api.PriceProposal{RfqID: "rfq.1", RfqRowVersion: 2, CustomerID: "customer.1", Currency: "USD", PricePrecision: 2,
		PricingPolicyRef: q.PricingPolicyRef, MinimumMargin: "0.25", TargetMargin: "0.3125",
		// The policy's seven days from the instant of the read, to the microsecond (owner decision
		// 2026-11-18 on the quote's validity: the drafter is told the freeze's bound).
		QuoteValidDays: 7, ExpiresAtLatest: time.Date(2026, 11, 25, 6, 0, 0, 123456000, time.UTC),
		CostSnapshot: &api.CostQualification{SnapshotID: "snapshot.1", ResultHash: hash, EvidenceHash: "evidence", Status: "adoptable", Adoptable: true},
		CheckedAt:    time.Date(2026, 11, 18, 6, 0, 0, 123456000, time.UTC),
		Lines: []api.ProposedLine{
			{RfqLineID: "L1", SkuID: "ST01.red.S", Quantity: "24", Unit: "PCS", Source: api.PriceSourceFormula, UnitPrice: "8.00",
				PriceBasisQuantity: "1", UnitCost: "5.50", TargetMargin: "0.3125", NetMinor: "19200", CostMinor: "13200", Margin: "0.3125"},
			{RfqLineID: "L2", SkuID: "ST01.red.M", Quantity: "36", Unit: "PCS", Source: api.PriceSourceList, UnitPrice: "8.40",
				PriceBasisQuantity: "1", PriceListRef: &api.ConfigRef{Kind: "sales_price", ID: "price.red.M", Revision: 2},
				PriceListScope: api.PriceListGeneral, NetMinor: "30240", CostMinor: "19800", Margin: "0.345238095238"},
			{RfqLineID: "L3", SkuID: "ST01.blue.S", Quantity: "12", Unit: "PCS", Source: api.PriceSourceNone, Reason: api.ProposalNoAdoptableSnapshot},
		}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}

	// A snapshot that is not adoptable now gives the formula nothing and judges no list price; its
	// qualification is shown. A snapshot in another currency says so.
	stale := in
	snap := *in.snapshot
	snap.Adoptability = cstapi.Adoptability{Status: cstapi.AdoptionNotCurrent, Code: cstapi.CodeCostSnapshotIneligible}
	stale.snapshot = &snap
	got, err = proposal(q, stale, at)
	if err != nil || got.CostSnapshot.Adoptable || got.CostSnapshot.Status != "not_current" || got.Lines[0].Source != api.PriceSourceNone ||
		got.Lines[0].Reason != api.ProposalNoAdoptableSnapshot || got.Lines[1].Source != api.PriceSourceList || got.Lines[1].Margin != "" ||
		got.Lines[1].NetMinor != "" || got.Lines[1].UnderFloor {
		t.Fatalf("%+v %v", got, err)
	}
	other := in
	snap = *in.snapshot
	snap.Currency = "EUR"
	other.snapshot = &snap
	if got, err = proposal(q, other, at); err != nil || got.Lines[0].Reason != api.ProposalAnotherCurrency || got.Lines[1].Margin != "" {
		t.Fatalf("%+v %v", got, err)
	}
	none := in
	none.snapshot, none.policy.TargetMargin = nil, ""
	if got, err = proposal(q, none, at); err != nil || got.CostSnapshot != nil || got.TargetMargin != "" ||
		got.Lines[0].Reason != api.ProposalNoAdoptableSnapshot {
		t.Fatalf("%+v %v", got, err)
	}
	// A list price under the floor is proposed and flagged.
	low := in
	low.prices.Prices = []polapi.SalesPriceInForce{{Ref: polapi.ConfigRef{Kind: polapi.KindSalesPrice, ConfigID: "price.red.M", Revision: 3},
		SkuID: "ST01.red.M", PartyID: "customer.1", Unit: "PCS", UnitPrice: kernel.MustDecimal("700"), PriceBasisQuantity: kernel.MustDecimal("100")}}
	got, err = proposal(q, low, at)
	if l := got.Lines[1]; err != nil || l.UnitPrice != "700.00" || l.PriceBasisQuantity != "100" || l.PriceListScope != api.PriceListCustomer ||
		l.NetMinor != "25200" || l.Margin != "0.214285714286" || !l.UnderFloor {
		t.Fatalf("%+v %v", l, err)
	}
}

func TestPriceText(t *testing.T) {
	for _, c := range []struct {
		value string
		minor int
		want  string
	}{
		{"8", 2, "8.00"}, {"812.5", 2, "812.50"}, {"7.33", 2, "7.33"}, {"7.33333333", 2, "7.33333333"}, {"0.0049", 2, "0.0049"},
		{"444", 0, "444"}, {"1.5", 0, "1.5"}, {"12.3", 3, "12.300"},
	} {
		if got := priceText(kernel.MustDecimal(c.value), c.minor); got != c.want {
			t.Errorf("%s with %d minor digits: %s, want %s", c.value, c.minor, got, c.want)
		}
		if _, err := kernel.ParseDecimal(priceText(kernel.MustDecimal(c.value), c.minor), kernel.PriceFormat); err != nil {
			t.Errorf("%s is not a unit price of the contract: %v", c.value, err)
		}
	}
	// A quotient that does not terminate is cut to the 8 digits of a unit price.
	third, _ := kernel.MustDecimal("1").Quo(kernel.MustDecimal("3"))
	if got := priceText(third, 2); got != "0.33333333" {
		t.Fatal(got)
	}
}
