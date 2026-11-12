package api

import "testing"

// settlement.confirm's closed contract (S5-04b) and the statement targets of application.apply.
func TestConfirmSettlementContract(t *testing.T) {
	sales := ConfirmSettlement{IdempotencyKey: "k", StatementID: "st.1", StatementKind: StatementSales,
		Sources: []SettlementSourceRef{{ShipmentID: "shipment.1", LineID: "S1"}, {ShipmentID: "shipment.1", LineID: "S2"}}}
	if err := sales.Validate(); err != nil {
		t.Fatal(err)
	}
	purchase := ConfirmSettlement{IdempotencyKey: "k", StatementID: "st.2", StatementKind: StatementPurchase,
		Sources: []SettlementSourceRef{{DeliveryID: "delivery.1", LineID: "L1", Version: 1}}, EvidenceIDs: []string{"bill"}}
	if err := purchase.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		cmd  ConfirmSettlement
		path string
	}{
		"kind":            {ConfirmSettlement{IdempotencyKey: "k", StatementID: "s", StatementKind: "invoice", Sources: sales.Sources}, "/args/kind"},
		"no sources":      {ConfirmSettlement{IdempotencyKey: "k", StatementID: "s", StatementKind: StatementSales}, "/args/sources"},
		"statement id":    {ConfirmSettlement{IdempotencyKey: "k", StatementID: "", StatementKind: StatementSales, Sources: sales.Sources}, "/args/statement_id"},
		"delivery ref":    {ConfirmSettlement{IdempotencyKey: "k", StatementID: "s", StatementKind: StatementSales, Sources: purchase.Sources}, "/args/sources/0/shipment_id"},
		"shipment ref":    {ConfirmSettlement{IdempotencyKey: "k", StatementID: "s", StatementKind: StatementPurchase, Sources: sales.Sources}, "/args/sources/0/delivery_id"},
		"no version":      {ConfirmSettlement{IdempotencyKey: "k", StatementID: "s", StatementKind: StatementPurchase, Sources: []SettlementSourceRef{{DeliveryID: "d", LineID: "L1"}}}, "/args/sources/0/version"},
		"no line":         {ConfirmSettlement{IdempotencyKey: "k", StatementID: "s", StatementKind: StatementSales, Sources: []SettlementSourceRef{{ShipmentID: "s"}}}, "/args/sources/0/line_id"},
		"twice":           {ConfirmSettlement{IdempotencyKey: "k", StatementID: "s", StatementKind: StatementSales, Sources: []SettlementSourceRef{sales.Sources[0], sales.Sources[0]}}, "/args/sources/1"},
		"twice versioned": {ConfirmSettlement{IdempotencyKey: "k", StatementID: "s", StatementKind: StatementPurchase, Sources: []SettlementSourceRef{{DeliveryID: "d", LineID: "L1", Version: 1}, {DeliveryID: "d", LineID: "L1", Version: 2}}}, "/args/sources/1"},
		"evidence":        {ConfirmSettlement{IdempotencyKey: "k", StatementID: "s", StatementKind: StatementSales, Sources: sales.Sources, EvidenceIDs: []string{"e", "e"}}, "/args/evidence_ids/1"},
	} {
		if got := pathOf(c.cmd.Validate()); got != c.path {
			t.Errorf("%s: %q, want %q", name, got, c.path)
		}
	}
	many := ConfirmSettlement{IdempotencyKey: "k", StatementID: "s", StatementKind: StatementSales}
	for range MaxSources + 1 {
		many.Sources = append(many.Sources, SettlementSourceRef{ShipmentID: "s", LineID: "x"})
	}
	if got := pathOf(many.Validate()); got != "/args/sources" {
		t.Errorf("too many: %q", got)
	}
	if _, ok := Category(CodeSettlementSourceDuplicate); !ok {
		t.Fatal("SETTLEMENT_SOURCE_DUPLICATE is not declared")
	}
}

func TestApplyToStatementContract(t *testing.T) {
	a := Apply{IdempotencyKey: "k", ApplicationID: "app.1", FundRecordID: "fund.1", Target: ApplicationTarget{Kind: TargetReceivable, StatementID: "st.1"},
		Amount: "672.00"}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	a.Target.StageID = "P2"
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	p := Apply{IdempotencyKey: "k", ApplicationID: "app.2", FundRecordID: "fund.2", Target: ApplicationTarget{Kind: TargetPayable, StatementID: "st.2"},
		Amount: "600.00"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		target ApplicationTarget
		path   string
	}{
		"receivable without statement": {ApplicationTarget{Kind: TargetReceivable}, "/args/target/statement_id"},
		"receivable with order":        {ApplicationTarget{Kind: TargetReceivable, StatementID: "st.1", OrderID: "o"}, "/args/target/statement_id"},
		"payable with stage":           {ApplicationTarget{Kind: TargetPayable, StatementID: "st.2", StageID: "P1"}, "/args/target/stage_id"},
		"stage with statement":         {ApplicationTarget{Kind: TargetOrderStage, OrderID: "o", StageID: "P1", StatementID: "st.1"}, "/args/target/order_id"},
	} {
		x := a
		x.Target = c.target
		if got := pathOf(x.Validate()); got != c.path {
			t.Errorf("%s: %q, want %q", name, got, c.path)
		}
	}
}
