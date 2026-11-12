package application

import (
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/scenario"
)

// Context Map R78 (CostSnapshotRef v1 by cost category; S5 decision D-13; F-03): a purchase is
// goods; a charge takes the category of its fee's POL fee_category, read from the logistics offer
// revision and fee category revision the evaluation bound. A revision whose content hash is not the
// bound one, a fee the offer does not hold or a fee category CST cannot read leaves the charge
// unresolved, and a total over a category is then incomplete: the lookup never guesses.
func TestSnapshotComponentsByCategory(t *testing.T) {
	ref := api.CostSnapshotRef{SnapshotID: "snapshot.1"}
	charge := func(id, offer, fee, amount string) api.CostComponent {
		return api.CostComponent{ComponentID: id, Kind: api.ComponentCharge, Source: api.EvaluatedRef{Kind: scenario.KindLogisticsOffer, ID: offer,
			Revision: 1}, SourceLineID: fee, Currency: "USD", CostMinor: amount, AmountMinor: amount}
	}
	result := api.EvaluationResult{Currency: "USD", MinorUnits: 2, TotalMinor: "67200", Components: []api.CostComponent{
		{ComponentID: "purchase.1", Kind: api.ComponentPurchase, Source: api.EvaluatedRef{Kind: scenario.KindSupplierOffer, ID: "offer.1", Revision: 1},
			SourceLineID: "OL1", Currency: "USD", CostMinor: "60000", AmountMinor: "60000"},
		charge("fee.1", "log.ins", "F1", "6000"), charge("fee.2", "log.ins", "F2", "1200"), charge("fee.3", "log.moved", "F1", "500"),
		charge("fee.4", "log.ins", "F9", "100")}}
	freight := scenario.Ref{Kind: "fee_category", ID: "freight", Revision: 1}
	insurance := scenario.Ref{Kind: "fee_category", ID: "insurance", Revision: 1}
	bound := []scenario.BoundInput{{Context: api.InputFromSUP, Kind: scenario.KindLogisticsOffer, ID: "log.ins", Revision: 1, ContentHash: "h.log"},
		{Context: api.InputFromSUP, Kind: scenario.KindLogisticsOffer, ID: "log.moved", Revision: 1, ContentHash: "h.bound"},
		{Context: api.InputFromPOL, Kind: "fee_category", ID: "freight", Revision: 1, ContentHash: "h.freight"},
		{Context: api.InputFromPOL, Kind: "fee_category", ID: "insurance", Revision: 1, ContentHash: "h.insurance"}}
	supply := map[scenario.Ref]scenario.Supply{
		{Kind: scenario.KindLogisticsOffer, ID: "log.ins", Revision: 1}: {ContentHash: "h.log", Logistics: &scenario.Logistics{Fees: []scenario.Fee{
			{ID: "F1", FeeType: &freight}, {ID: "F2", FeeType: &insurance}}}},
		{Kind: scenario.KindLogisticsOffer, ID: "log.moved", Revision: 1}: {ContentHash: "h.other", Logistics: &scenario.Logistics{Fees: []scenario.Fee{
			{ID: "F1", FeeType: &insurance}}}}}
	configs := map[scenario.Ref]scenario.Config{freight: {ContentHash: "h.freight", FeeCategory: &scenario.FeeCategory{Category: "freight"}},
		insurance: {ContentHash: "h.insurance", FeeCategory: &scenario.FeeCategory{Category: api.CategoryInsurance}}}

	got := categorize(ref, result, bound, supply, configs)
	categories := map[string]string{}
	for _, c := range got.Components {
		categories[c.ComponentID] = c.Category
	}
	if got.Currency != "USD" || got.MinorUnits != 2 || got.TotalMinor != "67200" || categories["purchase.1"] != api.CategoryGoods ||
		categories["fee.1"] != "freight" || categories["fee.2"] != api.CategoryInsurance || categories["fee.3"] != "" || categories["fee.4"] != "" ||
		!slices.Equal(got.Unresolved, []string{"fee.3", "fee.4"}) || got.Components[2].FeeCategory == nil ||
		*got.Components[2].FeeCategory != (api.ConfigRef{Kind: "fee_category", ID: "insurance", Revision: 1}) {
		t.Fatalf("%+v", got)
	}
	if minor, ids, complete := got.Total(api.CategoryInsurance); minor != "1200" || !slices.Equal(ids, []string{"fee.2"}) || complete {
		t.Fatalf("insurance %s %v %v: the unresolved charges might be insurance too", minor, ids, complete)
	}

	// The fee category revision POL shows now is not the one bound: unresolved as well.
	configs[insurance] = scenario.Config{ContentHash: "h.changed", FeeCategory: &scenario.FeeCategory{Category: api.CategoryInsurance}}
	if c := categorize(ref, result, bound, supply, configs); c.Components[2].Category != "" || !slices.Contains(c.Unresolved, "fee.2") {
		t.Fatalf("%+v", c)
	}

	// Every charge resolved: the total is complete; a category without components totals zero.
	configs[insurance] = scenario.Config{ContentHash: "h.insurance", FeeCategory: &scenario.FeeCategory{Category: api.CategoryInsurance}}
	result.Components = result.Components[:3]
	got = categorize(ref, result, bound, supply, configs)
	if minor, ids, complete := got.Total(api.CategoryInsurance); minor != "1200" || len(ids) != 1 || !complete || len(got.Unresolved) != 0 {
		t.Fatalf("complete %s %v %v %+v", minor, ids, complete, got)
	}
	if minor, ids, complete := got.Total("packaging"); minor != "0" || ids != nil || !complete {
		t.Fatalf("packaging %s %v %v", minor, ids, complete)
	}
}
