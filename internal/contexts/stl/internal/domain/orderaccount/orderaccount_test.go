package orderaccount

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var usd = kernel.MustCurrency("USD", 2)

func money(t *testing.T, s string) kernel.Money {
	t.Helper()
	m, err := kernel.ParseAmount(usd, s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var by = fundrecord.Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "treasury.1"}, Epoch: 1, At: time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)}

// SIM-01's terms on BRC-001's 960.00: 30 % on confirmation, 70 % after shipment against B/L copy
// with the original B/L controlled (D-5, F-04), and an O/A part to show what neither scope needs.
func plan(t *testing.T) *Account {
	t.Helper()
	a, err := Open("order.1", 2, "hash", "customer.1", 1, []Stage{
		{StageID: "P1", Trigger: TriggerOrderConfirmed, Percent: "30", Amount: money(t, "288.00")},
		{StageID: "P2", Trigger: TriggerAfterShipment, Days: 0, Percent: "60", Amount: money(t, "576.00"), ReleaseControl: ReleaseDocuments},
		{StageID: "P3", Trigger: TriggerAfterInvoice, Days: 30, Percent: "10", Amount: money(t, "96.00")}}, by, "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// STL-INV-003 and D-5: scope shipment needs the stages due on confirmation and before shipment;
// scope documents also the document-controlled post-shipment stage; an O/A stage neither. The first
// unmet stage and its shortfall are named; another revision and a missing account are blocked.
func TestConditionPerScope(t *testing.T) {
	a := plan(t)
	at := by.At
	if a.Stages[0].Seq != 1 || a.Stages[2].Seq != 3 || a.Currency() != usd {
		t.Fatalf("account %+v", a)
	}
	v := Condition(a, 2, ScopeShipment, nil, at)
	if v.Status != Blocked || v.Reason != ReasonStageUnpaid || v.Stage.StageID != "P1" || v.Shortfall.Amount() != "288.00" || v.Required.Amount() != "288.00" {
		t.Fatalf("nothing paid %+v", v)
	}
	applied := map[string]kernel.Money{"P1": money(t, "288.00")}
	if v = Condition(a, 2, ScopeShipment, applied, at); v.Status != Satisfied || v.Applied.Amount() != "288.00" || v.Stage != nil {
		t.Fatalf("deposit paid, shipment %+v", v)
	}
	v = Condition(a, 2, ScopeDocuments, applied, at)
	if v.Status != Blocked || v.Stage.StageID != "P2" || v.Shortfall.Amount() != "576.00" || v.Required.Amount() != "864.00" {
		t.Fatalf("deposit paid, documents %+v", v)
	}
	applied["P2"] = money(t, "575.99")
	if v = Condition(a, 2, ScopeDocuments, applied, at); v.Status != Blocked || v.Shortfall.Amount() != "0.01" {
		t.Fatalf("0.01 short %+v", v)
	}
	applied["P2"] = money(t, "576.00")
	if v = Condition(a, 2, ScopeDocuments, applied, at); v.Status != Satisfied || v.Applied.Amount() != "864.00" {
		t.Fatalf("documents %+v", v)
	}
	if v = Condition(a, 1, ScopeShipment, applied, at); v.Status != Blocked || v.Reason != ReasonRevision || v.OrderRevision != 2 {
		t.Fatalf("revision 1 %+v", v)
	}
	if v = Condition(nil, 2, ScopeShipment, nil, at); v.Status != Blocked || v.Reason != ReasonNoAccount {
		t.Fatalf("no account %+v", v)
	}
	oa, err := Open("order.2", 1, "hash", "customer.1", 1, []Stage{{StageID: "P1", Trigger: TriggerAfterShipment, Days: 60, Percent: "100",
		Amount: money(t, "960.00")}}, by, "")
	if err != nil {
		t.Fatal(err)
	}
	if v = Condition(oa, 1, ScopeDocuments, nil, at); v.Status != Satisfied || v.Required.Sign() != 0 {
		t.Fatalf("O/A %+v", v)
	}
}

// Open refuses plans STL cannot judge by.
func TestOpenNeedsKnownStages(t *testing.T) {
	for name, stages := range map[string][]Stage{
		"none":      nil,
		"trigger":   {{StageID: "P1", Trigger: "on_delivery", Amount: money(t, "1.00")}},
		"control":   {{StageID: "P1", Trigger: TriggerAfterShipment, ReleaseControl: "telex_release", Amount: money(t, "1.00")}},
		"duplicate": {{StageID: "P1", Trigger: TriggerBeforeShipment, Amount: money(t, "1.00")}, {StageID: "P1", Trigger: TriggerBeforeShipment, Amount: money(t, "1.00")}},
		"negative":  {{StageID: "P1", Trigger: TriggerBeforeShipment, Amount: money(t, "1.00").Neg()}},
	} {
		if _, err := Open("order.x", 1, "h", "c", 1, stages, by, ""); err == nil {
			t.Fatalf("%s opened", name)
		}
	}
}
