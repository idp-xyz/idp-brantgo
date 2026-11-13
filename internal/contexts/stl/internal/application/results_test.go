package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/charge"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Charges and order results (C9): humanOnlyCommands charge.confirm, order_result.close and order_result.reopen;
// charge.record and charge.allocate by a person or AI (role template credit_officer; command catalog
// charge.allocate "人工或 AI"); order_result.review by a person or AI (SM-STL-ORDER-RESULT); the reads need
// stl.read. The request hash ignores the key and the expected version.
func TestActorKindsOfChargesAndOrderResults(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	for c, kinds := range map[string][]kernel.ActorKind{
		api.CommandRecordCharge:      {kernel.ActorHuman, kernel.ActorAI},
		api.CommandConfirmCharge:     {kernel.ActorHuman},
		api.CommandAllocateCharge:    {kernel.ActorHuman, kernel.ActorAI},
		api.CommandReviewOrderResult: {kernel.ActorHuman, kernel.ActorAI},
		api.CommandCloseOrderResult:  {kernel.ActorHuman},
		api.CommandReopenOrderResult: {kernel.ActorHuman},
	} {
		r, err := policy.Rule(c)
		if err != nil || r.Permission != c || len(r.ActorKinds) != len(kinds) {
			t.Errorf("%s: %+v, %v", c, r, err)
			continue
		}
		for _, k := range kinds {
			if !r.Admits(k) {
				t.Errorf("%s does not admit %s", c, k)
			}
		}
	}
	for _, c := range []string{api.CommandGetCharge, api.CommandGetOrderResult} {
		if r, err := policy.Rule(c); err != nil || r.Permission != api.PermissionRead {
			t.Errorf("%s: %+v, %v", c, r, err)
		}
	}
	a := api.CloseOrderResult{IdempotencyKey: "k1", OrderID: "order.1", ExpectedVersion: 1}
	b := a
	b.IdempotencyKey, b.ExpectedVersion = "k2", 3
	if _, _, x := request(a); x != func() any { _, _, y := request(b); return y }() {
		t.Fatal("close hash args differ")
	}
	for _, c := range []api.Command{api.RecordCharge{}, api.ConfirmCharge{}, api.AllocateCharge{}, api.ReviewOrderResult{}, api.CloseOrderResult{},
		api.ReopenOrderResult{}} {
		if name, _, _ := request(c); name == "" {
			t.Errorf("%T has no name", c)
		}
	}
}

// A charge's conversion basis fits each order it benefits (baseline 4.14: no default 1, no guessed rate): the
// same currency as the order's analysis currency; the order's currency at its analysis rate; a settlement rate
// into the analysis currency. Anything else is FX_SCOPE_MISMATCH at /args/conversion.
func TestAChargeConvertsIntoTheOrdersAnalysisCurrency(t *testing.T) {
	cny, _ := kernel.NewCurrency("CNY", 2)
	usd, _ := kernel.NewCurrency("USD", 2)
	sa := Order{Binding: OrderBinding{OrderID: "order.sa"}, Currency: CurrencyRef{ID: "USD", Revision: 1},
		Analysis: &OrderAnalysis{OrderCurrency: "USD", Currency: "CNY", Rate: "7.0"}}
	own := Order{Binding: OrderBinding{OrderID: "order.1"}, Currency: CurrencyRef{ID: "USD", Revision: 1}}
	toCNY := &charge.RateUse{ID: "eur.cny", Revision: 1, To: "CNY", Value: kernel.MustDecimal("7.8")}
	for name, x := range map[string]struct {
		conv     charge.Conversion
		currency kernel.Currency
		order    Order
		fits     bool
	}{
		"CNY freight of S-A":            {charge.Conversion{Kind: charge.ConversionSameCurrency}, cny, sa, true},
		"USD freight of S-A, same":      {charge.Conversion{Kind: charge.ConversionSameCurrency}, usd, sa, false},
		"USD freight of S-A, analysis":  {charge.Conversion{Kind: charge.ConversionOrderAnalysis}, usd, sa, true},
		"CNY freight, analysis":         {charge.Conversion{Kind: charge.ConversionOrderAnalysis}, cny, sa, false},
		"USD freight of BRC-001":        {charge.Conversion{Kind: charge.ConversionSameCurrency}, usd, own, true},
		"a rate into CNY for S-A":       {charge.Conversion{Kind: charge.ConversionFXRate, Rate: toCNY}, usd, sa, true},
		"a rate into CNY for BRC-001":   {charge.Conversion{Kind: charge.ConversionFXRate, Rate: toCNY}, usd, own, false},
		"an fx_rate basis without rate": {charge.Conversion{Kind: charge.ConversionFXRate}, usd, sa, false},
	} {
		err := conversionFits(x.conv, x.currency, x.order)
		if x.fits != (err == nil) {
			t.Errorf("%s: %v", name, err)
		}
		if rej, ok := kernel.AsRejection(err); ok && (rej.Code != api.CodeFXScopeMismatch || rej.Fields[0].Path != "/args/conversion") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
