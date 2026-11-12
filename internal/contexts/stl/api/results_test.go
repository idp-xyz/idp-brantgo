package api

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The closed contracts of charges and order results (C9): what charge.record needs for a new charge and for the
// bill of an estimate, charge.allocate's lines, the order result commands' versions.
func TestChargeAndOrderResultContracts(t *testing.T) {
	fee, cur := ConfigRef{Kind: KindFeeCategory, ConfigID: "freight", Revision: 2}, ConfigRef{Kind: KindCurrency, ConfigID: "CNY", Revision: 2}
	record := func() RecordCharge {
		return RecordCharge{IdempotencyKey: "k", ChargeID: "freight.sa", Status: ChargeEstimated,
			Source:        ChargeSource{Kind: ChargeSourceLogistics, PartyID: "forwarder.1", Reference: "OFFER-1"},
			Beneficiaries: []ChargeTarget{{Kind: ChargeTargetShipment, OrderID: "order.sa", ShipmentID: "shipment.60"}},
			FeeCategory:   &fee, Currency: &cur, Conversion: &ChargeConversion{Kind: ConversionSameCurrency}, Amount: "500.00",
			OccurredOn: "2026-11-12", AnalysisOn: "2026-11-30", Basis: "预计运费"}
	}
	if err := record().Validate(); err != nil {
		t.Fatalf("a new estimate: %v", err)
	}
	bill := RecordCharge{IdempotencyKey: "k", ChargeID: "freight.sa", ExpectedVersion: 1, Status: ChargePending,
		Source: ChargeSource{Kind: ChargeSourceSupplierBill, PartyID: "forwarder.1", Reference: "BILL-1"}, Amount: "-12.50",
		OccurredOn: "2026-12-03", AnalysisOn: "2026-12-03", Basis: "账单", EvidenceIDs: []string{"bill.1"}}
	if err := bill.Validate(); err != nil {
		t.Fatalf("a bill: %v", err)
	}
	for name, x := range map[string]struct {
		mutate func(c *RecordCharge)
		path   string
	}{
		"status":                {func(c *RecordCharge) { c.Status = ChargeConfirmed }, "/args/status"},
		"no beneficiary":        {func(c *RecordCharge) { c.Beneficiaries = nil }, "/args/beneficiaries"},
		"shipment without id":   {func(c *RecordCharge) { c.Beneficiaries[0].ShipmentID = "" }, "/args/beneficiaries/0/shipment_id"},
		"order with a shipment": {func(c *RecordCharge) { c.Beneficiaries[0].Kind = ChargeTargetOrder }, "/args/beneficiaries/0/shipment_id"},
		"no fee category":       {func(c *RecordCharge) { c.FeeCategory = nil }, "/args/fee_category"},
		"fee category kind":     {func(c *RecordCharge) { c.FeeCategory = &cur }, "/args/fee_category/kind"},
		"fx_rate without rate":  {func(c *RecordCharge) { c.Conversion = &ChargeConversion{Kind: ConversionFXRate} }, "/args/conversion/fx_rate"},
		"a rate for same":       {func(c *RecordCharge) { c.Conversion.FXRate = &ConfigRef{Kind: KindFXRate, ConfigID: "x", Revision: 1} }, "/args/conversion/fx_rate"},
		"amount":                {func(c *RecordCharge) { c.Amount = "5.0.0" }, "/args/amount"},
		"date":                  {func(c *RecordCharge) { c.OccurredOn = "2026-11-31" }, "/args/occurred_on"},
		"bill party":            {func(c *RecordCharge) { c.Source.PartyID = "" }, "/args/source/party_id"},
		"bill reference":        {func(c *RecordCharge) { c.Source.Reference = "" }, "/args/source/reference"},
		"internal with party":   {func(c *RecordCharge) { c.Source = ChargeSource{Kind: ChargeSourceInternal, PartyID: "p"} }, "/args/source/party_id"},
		"pending, no evidence":  {func(c *RecordCharge) { c.Status = ChargePending }, "/args/evidence_ids"},
	} {
		c := record()
		x.mutate(&c)
		want(t, name, c.Validate(), x.path)
	}
	for name, x := range map[string]struct {
		mutate func(c *RecordCharge)
		path   string
	}{
		"a bill names beneficiaries": {func(c *RecordCharge) { c.Beneficiaries = []ChargeTarget{{Kind: ChargeTargetOrder, OrderID: "o"}} }, "/args/beneficiaries"},
		"a bill names a currency":    {func(c *RecordCharge) { c.Currency = &cur }, "/args/currency"},
		"a bill is pending":          {func(c *RecordCharge) { c.Status = ChargeEstimated }, "/args/status"},
	} {
		c := bill
		x.mutate(&c)
		want(t, name, c.Validate(), x.path)
	}
	alloc := AllocateCharge{IdempotencyKey: "k", ChargeID: "freight.sa", ExpectedVersion: 3, Basis: AllocationByQuantity, DataBasis: "FUL 60 / 40",
		Lines: []AllocationLine{{LineID: "A", Target: ChargeTarget{Kind: ChargeTargetShipment, OrderID: "o", ShipmentID: "s"}, Weight: "-1"}}}
	if err := alloc.Validate(); err != nil {
		t.Fatalf("a negative weight is the rules' refusal, not the contract's: %v", err)
	}
	alloc.Basis = "volume"
	want(t, "basis", alloc.Validate(), "/args/basis")
	if err := (ReviewOrderResult{IdempotencyKey: "k", OrderID: "order.1"}).Validate(); err != nil {
		t.Fatalf("a first review: %v", err)
	}
	want(t, "close at 0", CloseOrderResult{IdempotencyKey: "k", OrderID: "order.1"}.Validate(), "/args/expected_version")
	want(t, "reopen without reason", ReopenOrderResult{IdempotencyKey: "k", OrderID: "order.1", ExpectedVersion: 3}.Validate(), "/args/reason")
}

func want(t *testing.T, name string, err error, path string) {
	t.Helper()
	rej, ok := kernel.AsRejection(err)
	if !ok || rej.Code != kernel.CodeSchemaInvalid || rej.Fields[0].Path != path {
		t.Fatalf("%s: want SCHEMA_INVALID at %s, got %v", name, path, err)
	}
}
