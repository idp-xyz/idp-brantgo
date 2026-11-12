package exportlot

import (
	"math/big"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func dec(s string) kernel.Decimal {
	d, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	if err != nil {
		panic(err)
	}
	return d
}

var act = Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "merch.1"}, Epoch: 1, At: time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)}

// A shipment of two sales lines (CN-AT02 "同次出口包含多销售行"): L1 120 PCS for 96000 minor units, L2 30
// PCS for 9000 + 1170 tax.
func twoLineShipment(status string) (*shipment.Shipment, map[string]shipment.Price) {
	s := &shipment.Shipment{ID: "shipment.1", Status: status, RowVersion: 1, CustomerID: "customer.1",
		Order: shipment.OrderBinding{OrderID: "order.1", OrderRevision: 2, OrderHash: "h"}, Lines: []shipment.Line{
			{ID: "S1", OrderLineID: "L1", SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS", UnitRevision: 1, OrderQuantity: dec("120"), Quantity: dec("60")},
			{ID: "S2", OrderLineID: "L2", SkuID: "sku.2", SpecRevision: 3, UnitID: "PCS", UnitRevision: 1, OrderQuantity: dec("30"), Quantity: dec("30")}}}
	prices := map[string]shipment.Price{
		"S1": {CurrencyID: "USD", CurrencyRevision: 1, UnitPrice: dec("8"), Net: big.NewInt(96000), Tax: big.NewInt(0), Payable: big.NewInt(96000)},
		"S2": {CurrencyID: "USD", CurrencyRevision: 1, UnitPrice: dec("3"), Net: big.NewInt(9000), Tax: big.NewInt(1170), Payable: big.NewInt(10170)},
	}
	return s, prices
}

func codeAt(err error) string {
	rej, ok := kernel.AsRejection(err)
	if !ok {
		return ""
	}
	return rej.Code + " " + rej.Fields[0].Path
}

// CN-AT02, the lot side: one export of two sales lines maps each lot line to its shipment line and
// order line with the amounts at the order's prices; the totals are their sums.
func TestRegisterMapsSalesLines(t *testing.T) {
	s, prices := twoLineShipment(shipment.StatusPrepared)
	l, err := Register(Request{ID: "lot.1", ShipmentID: "shipment.1", RequestHash: "h", Lines: []RequestLine{
		{ID: "E1", ShipmentLineID: "S2", Quantity: dec("30")}, {ID: "E2", ShipmentLineID: "S1", Quantity: dec("60")}}},
		Inputs{Shipment: s, Prices: prices, Taken: map[string]string{}, Act: act, Team: "team.1"})
	if err != nil {
		t.Fatal(err)
	}
	if l.Lines[0].OrderLineID != "L2" || l.Lines[0].Amounts.Net.String() != "9000" || l.Lines[0].Amounts.Tax.String() != "1170" ||
		l.Lines[1].OrderLineID != "L1" || l.Lines[1].Amounts.Net.String() != "48000" || l.Totals.Net.String() != "57000" ||
		l.Totals.Tax.String() != "1170" || l.Totals.Payable.String() != "58170" || l.ShipmentStatus != shipment.StatusPrepared ||
		l.ShipmentVersion != 1 || l.Order.OrderRevision != 2 || l.CurrencyID != "USD" || l.OwnerActor != "merch.1" || l.OwnerTeam != "team.1" ||
		l.RowVersion != 1 || l.Status != StatusRegistered {
		t.Fatalf("%+v", l)
	}
	if err = l.Answer("h", act.By); err != nil || codeAt(l.Answer("x", act.By)) != "BUSINESS_KEY_CONFLICT /args/export_lot_id" {
		t.Fatal("the same request answers from the lot, another conflicts")
	}
}

// What a registration refuses, in order (UL FUL-ExportLot "不复制第二份库存").
func TestRegisterRefusals(t *testing.T) {
	line := func(id, sl, q string) RequestLine { return RequestLine{ID: id, ShipmentLineID: sl, Quantity: dec(q)} }
	for _, c := range []struct {
		want   string
		status string
		taken  map[string]string
		drop   string
		lines  []RequestLine
		noShip bool
	}{
		{want: "REFERENCE_INVALID /args/shipment_id", noShip: true, lines: []RequestLine{line("E1", "S1", "1")}},
		{want: "INVALID_STATE /args/shipment_id", status: shipment.StatusCancelled, lines: []RequestLine{line("E1", "S1", "1")}},
		{want: "REFERENCE_INVALID /args/lines/1/shipment_line_id", lines: []RequestLine{line("E1", "S1", "1"), line("E2", "S9", "1")}},
		{want: "BUSINESS_KEY_CONFLICT /args/lines/0/shipment_line_id", taken: map[string]string{"S1": "lot.0"}, lines: []RequestLine{line("E1", "S1", "1")}},
		{want: "REFERENCE_INVALID /args/lines/0/shipment_line_id", drop: "S1", lines: []RequestLine{line("E1", "S1", "1")}},
		{want: "INVALID_KNOWN_VALUE /args/lines/0/quantity", lines: []RequestLine{line("E1", "S1", "60.000001")}},
	} {
		status := c.status
		if status == "" {
			status = shipment.StatusConfirmed
		}
		s, prices := twoLineShipment(status)
		delete(prices, c.drop)
		in := Inputs{Shipment: s, Prices: prices, Taken: c.taken, Act: act}
		if c.noShip {
			in.Shipment = nil
		}
		if _, err := Register(Request{ID: "lot.1", ShipmentID: "shipment.1", Lines: c.lines}, in); codeAt(err) != c.want {
			t.Errorf("%s: %s", c.want, codeAt(err))
		}
	}
}

// "确认数量与批次不一致时标记复核": a lot awaits its shipment's confirmation, is matched when the
// confirmation confirms what it states and review_required when it confirms another quantity or the
// shipment is cancelled.
func TestStandingAgainstTheShipment(t *testing.T) {
	s, prices := twoLineShipment(shipment.StatusPrepared)
	l, err := Register(Request{ID: "lot.1", ShipmentID: "shipment.1", Lines: []RequestLine{{ID: "E1", ShipmentLineID: "S1", Quantity: dec("60")},
		{ID: "E2", ShipmentLineID: "S2", Quantity: dec("20")}}}, Inputs{Shipment: s, Prices: prices, Act: act})
	if err != nil {
		t.Fatal(err)
	}
	if st := l.Against(s); st.State != AwaitingConfirmation || st.Confirmed != nil {
		t.Fatalf("prepared %+v", st)
	}
	s.Status, s.RowVersion = shipment.StatusConfirmed, 2
	s.Confirmation = &shipment.Confirmation{Confirmed: shipment.Act{At: act.At}}
	st := l.Against(s)
	if st.State != ReviewRequired || len(st.Differences) != 1 || st.Differences[0].LineID != "E2" || st.Differences[0].Confirmed.String() != "30" ||
		st.Confirmed["E1"].String() != "60" || !st.ConfirmedAt.Equal(act.At) {
		t.Fatalf("confirmed with more %+v", st)
	}
	l.Lines[1].Quantity = dec("30")
	if st = l.Against(s); st.State != Matched || len(st.Differences) != 0 {
		t.Fatalf("matched %+v", st)
	}
	s.Status, s.Confirmation = shipment.StatusCancelled, nil
	if st = l.Against(s); st.State != ReviewRequired || len(st.Differences) != 2 || !st.Confirmed["E1"].IsZero() {
		t.Fatalf("cancelled %+v", st)
	}
}
