package application

import (
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5R unit R8 (simulated sign-off E0, 2026-11-13). Python V0.50 has no STL: the tests cite the decisions and the design.

// S5R-D20, S5R-D22: payable_adjustment.request (treasury), payable_adjustment.confirm and fund.reverse (credit_manager)
// are humanOnlyCommands under the permission of their name, held by those role templates alone.
func TestS5R8HumanOnlyCommandsAndTheirHolders(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	for c, holders := range map[string][]string{
		api.CommandRequestPayableAdjustment: {"treasury"},
		api.CommandConfirmPayableAdjustment: {"credit_manager"},
		api.CommandReverseFund:              {"credit_manager"},
	} {
		r, err := policy.Rule(c)
		if err != nil || r.Permission != c || !r.Admits(kernel.ActorHuman) || r.Admits(kernel.ActorAI) || r.Admits(kernel.ActorSystem) {
			t.Errorf("%s: %+v %v", c, r, err)
		}
		var got []string
		for _, tpl := range authorization.Templates() {
			if slices.Contains(tpl.Permissions, c) {
				got = append(got, tpl.Code)
			}
		}
		if !slices.Equal(got, holders) {
			t.Errorf("%s is held by %v, want %v", c, got, holders)
		}
		if name, _, _ := map[string]func() (string, string, any){
			api.CommandRequestPayableAdjustment: func() (string, string, any) { return request(api.RequestPayableAdjustment{}) },
			api.CommandConfirmPayableAdjustment: func() (string, string, any) { return request(api.ConfirmPayableAdjustment{}) },
			api.CommandReverseFund:              func() (string, string, any) { return request(api.ReverseFund{}) },
		}[c](); name != c {
			t.Errorf("%s has no request name: %q", c, name)
		}
	}
}

// The members R8 adds to application.apply (target charge_id, settlement_fx_rate) and charge.record (cost_component,
// recharged) are left out of the request hash while empty: an S5 request keeps its hash.
func TestS5R8RequestsKeepTheirHash(t *testing.T) {
	type targetS5 struct {
		Kind        string
		OrderID     string
		StageID     string
		StatementID string
	}
	type applyS5 struct {
		IdempotencyKey string
		ApplicationID  string
		FundRecordID   string
		Target         targetS5
		Amount         string
	}
	hash := func(v any) string {
		h, err := action.RequestHash(v)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	_, _, apply := request(api.Apply{IdempotencyKey: "k", ApplicationID: "a", FundRecordID: "f", Target: api.ApplicationTarget{Kind: "order_stage",
		OrderID: "o", StageID: "P1"}, Amount: "1.00"})
	if hash(apply) != hash(applyS5{ApplicationID: "a", FundRecordID: "f", Target: targetS5{Kind: "order_stage", OrderID: "o", StageID: "P1"},
		Amount: "1.00"}) {
		t.Fatal("application.apply: the request hash changed for an S5 request")
	}
	_, _, charge := request(api.RecordCharge{IdempotencyKey: "k", ChargeID: "c", Amount: "1.00"})
	_, _, component := request(api.RecordCharge{IdempotencyKey: "k", ChargeID: "c", Amount: "1.00", CostComponent: api.ComponentPreCarriage})
	if hash(charge) == hash(component) {
		t.Fatal("the cost component is not part of the request")
	}
}

// S5R-D20: the invoice_reversed flag derives from CNX's invoices of the commitments a purchase statement settles — a blue
// invoice matched to one of its commitment lines that red invoices reversed (partly or wholly); an invoice of another
// commitment line, or one not reversed, raises nothing; without CNX it is not shown.
func TestS5RInvoiceReversedFlag(t *testing.T) {
	st := &settlement.Statement{ID: "st.p", Kind: settlement.KindPurchase, CounterpartyID: "supplier.1", Lines: []settlement.Line{
		{Seq: 1, CommitmentID: "commitment.1", OrderLineID: "L1"}}}
	blue := PurchaseInvoice{PurchaseInvoiceID: "inv.blue", Colour: InvoiceBlue, Status: "verified", SupplierPartyID: "supplier.1", CurrencyCode: "CNY",
		Matches: []PurchaseInvoiceMatch{{LineNo: 1, CommitmentID: "commitment.1", OrderLineID: "L1"}}}
	if flag, views := invoiceFlag(st, invoiceFacts{shown: true, invoices: []PurchaseInvoice{blue}}); flag || len(views) != 0 {
		t.Fatalf("not reversed: %v %v", flag, views)
	}
	blue.Reversal, blue.ReversedBy = "partial", []string{"inv.red"}
	red := PurchaseInvoice{PurchaseInvoiceID: "inv.red", Colour: InvoiceRed, Status: "verified", SupplierPartyID: "supplier.1", CurrencyCode: "CNY",
		RedOf: "inv.blue"}
	flag, views := invoiceFlag(st, invoiceFacts{shown: true, invoices: []PurchaseInvoice{blue, red}})
	if !flag || len(views) != 2 || views[0].Reversal != "partial" || views[1].RedOf != "inv.blue" {
		t.Fatalf("reversed: %v %+v", flag, views)
	}
	other := blue
	other.Matches = []PurchaseInvoiceMatch{{CommitmentID: "commitment.1", OrderLineID: "L2"}}
	if flag, _ := invoiceFlag(st, invoiceFacts{shown: true, invoices: []PurchaseInvoice{other, red}}); flag {
		t.Fatal("an invoice of another commitment line raises the flag")
	}
	if flag, views := invoiceFlag(st, invoiceFacts{}); flag || views == nil {
		t.Fatal("CNX not shown")
	}

	// The invoice a payable adjustment references.
	cny := kernel.MustCurrency("CNY", 2)
	st.Amount, _ = kernel.NewMoney(cny, 60000)
	facts := invoiceFacts{shown: true, invoices: []PurchaseInvoice{blue, red}}
	for name, c := range map[string]struct {
		kind, id string
		facts    invoiceFacts
		ok       bool
	}{
		"an allowance on the red invoice":     {settlement.AdjustmentPriceAllowance, "inv.red", facts, true},
		"an allowance on the blue invoice":    {settlement.AdjustmentPriceAllowance, "inv.blue", facts, false},
		"under-invoicing on the blue invoice": {settlement.AdjustmentUnderInvoiced, "inv.blue", facts, true},
		"an invoice CNX does not show":        {settlement.AdjustmentUnderInvoiced, "inv.x", facts, false},
		"CNX not wired":                       {settlement.AdjustmentPriceAllowance, "inv.red", invoiceFacts{}, false},
		"another supplier's invoice":          {settlement.AdjustmentUnderInvoiced, "inv.s2", invoiceFacts{shown: true, invoices: []PurchaseInvoice{{PurchaseInvoiceID: "inv.s2", Colour: InvoiceBlue, SupplierPartyID: "supplier.2", CurrencyCode: "CNY", Matches: blue.Matches}}}, false},
		"an invoice of no line it settles":    {settlement.AdjustmentUnderInvoiced, "inv.o", invoiceFacts{shown: true, invoices: []PurchaseInvoice{{PurchaseInvoiceID: "inv.o", Colour: InvoiceBlue, SupplierPartyID: "supplier.1", CurrencyCode: "CNY", Matches: other.Matches}}}, false},
	} {
		err := adjustmentInvoice(st, c.kind, c.id, c.facts)
		if r, refused := kernel.AsRejection(err); c.ok != (err == nil) || (refused && (r.Code != kernel.CodeReferenceInvalid ||
			r.Fields[0].Path != "/args/purchase_invoice_id")) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
