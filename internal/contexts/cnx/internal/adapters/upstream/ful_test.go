package upstream

import (
	"context"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/application"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

type fakeFUL struct {
	lots      map[string]fulapi.ExportLotRef
	shipments map[string]fulapi.ShipmentView
	lines     map[fulapi.ShipmentLineKey]fulapi.ShipmentLineRef
	reads     int
}

func (f *fakeFUL) ResolveExportLots(_ context.Context, ids []string) (map[string]fulapi.ExportLotRef, error) {
	out := map[string]fulapi.ExportLotRef{}
	for _, id := range ids {
		if l, ok := f.lots[id]; ok {
			out[id] = l
		}
	}
	return out, nil
}

func (f *fakeFUL) ResolveShipmentExportLots(context.Context, []string) (map[string][]fulapi.ExportLotRef, error) {
	return nil, nil
}

func (f *fakeFUL) ResolveShipmentLines(_ context.Context, keys []fulapi.ShipmentLineKey) (map[fulapi.ShipmentLineKey]fulapi.ShipmentLineRef, error) {
	out := map[fulapi.ShipmentLineKey]fulapi.ShipmentLineRef{}
	for _, k := range keys {
		if l, ok := f.lines[k]; ok {
			out[k] = l
		}
	}
	return out, nil
}

func (f *fakeFUL) ResolveOrderShipmentLines(context.Context, []string) (map[string][]fulapi.ShipmentLineRef, error) {
	return nil, nil
}

func (f *fakeFUL) GetShipment(_ context.Context, id string) (fulapi.ShipmentView, error) {
	f.reads++
	if s, ok := f.shipments[id]; ok {
		return s, nil
	}
	return fulapi.ShipmentView{}, kernel.Reject(kernel.CodeNotFound, "/id", "")
}

type fakeCurrencies struct{}

func (fakeCurrencies) ResolveConfigs(_ context.Context, refs []polapi.ConfigRef, _ time.Time) (map[polapi.ConfigRef]polapi.ResolvedConfig, error) {
	out := map[polapi.ConfigRef]polapi.ResolvedConfig{}
	for _, r := range refs {
		if r.ConfigID == "USD" && r.Revision == 1 {
			out[r] = polapi.ResolvedConfig{Ref: r, Data: polapi.ConfigData{Currency: &polapi.Currency{Code: "USD", MinorUnits: 2}}}
		}
	}
	return out, nil
}

type fakeCountries map[application.AddressRef]string

func (f fakeCountries) ResolveAddressCountries(_ context.Context, refs []application.AddressRef) (map[application.AddressRef]application.AddressCountry, error) {
	out := map[application.AddressRef]application.AddressCountry{}
	for _, r := range refs {
		if c, ok := f[r]; ok {
			out[r] = application.AddressCountry{Role: "consignee", Country: c}
		}
	}
	return out, nil
}

func fulWorld() *fakeFUL {
	order := fulapi.OrderBinding{OrderID: "order.1", OrderRevision: 2, OrderHash: "h"}
	usd := fulapi.CurrencyRef{ID: "USD", Revision: 1}
	price := &fulapi.LinePrice{Currency: usd, UnitPrice: "8", OrderLineQuantity: "120", NetMinor: "96000", TaxMinor: "0", PayableMinor: "96000"}
	f := &fakeFUL{lots: map[string]fulapi.ExportLotRef{}, shipments: map[string]fulapi.ShipmentView{}, lines: map[fulapi.ShipmentLineKey]fulapi.ShipmentLineRef{}}
	f.shipments["ship.1"] = fulapi.ShipmentView{ShipmentID: "ship.1", Status: fulapi.StatusPrepared, RowVersion: 3, Order: order, CustomerID: "customer.1",
		Lines: []fulapi.ShipmentLineView{{LineID: "S1"}, {LineID: "S2"}},
		Parties: []fulapi.ShipmentParty{{Role: "customer", PartyID: "customer.1", PartyRevision: 2},
			{Role: "consignee", PartyID: "consignee.1", PartyRevision: 4, AddressID: "dock"}}}
	f.shipments["ship.2"] = fulapi.ShipmentView{ShipmentID: "ship.2", Status: fulapi.StatusCancelled, RowVersion: 2, Order: order, CustomerID: "customer.1",
		Lines: []fulapi.ShipmentLineView{{LineID: "S1"}}, Parties: []fulapi.ShipmentParty{{Role: "customer", PartyID: "customer.1", PartyRevision: 2}}}
	f.shipments["ship.old"] = fulapi.ShipmentView{ShipmentID: "ship.old", Status: fulapi.StatusPrepared, RowVersion: 1, Order: order,
		Lines: []fulapi.ShipmentLineView{{LineID: "S1"}}}
	for _, l := range []struct{ ship, line, sku, q, net string }{{"ship.1", "S1", "sku.1", "50", "40000"}, {"ship.1", "S2", "sku.2", "10.5", "8400"},
		{"ship.2", "S1", "sku.1", "70", "56000"}} {
		k := fulapi.ShipmentLineKey{ShipmentID: l.ship, LineID: l.line}
		f.lines[k] = fulapi.ShipmentLineRef{Key: k, ShipmentStatus: f.shipments[l.ship].Status, Order: order, OrderLineID: "L1", SkuID: l.sku, SpecRevision: 1,
			UnitID: "PCS", UnitRevision: 1, Quantity: l.q, Price: price, Amounts: fulapi.Amounts{NetMinor: l.net, TaxMinor: "0", PayableMinor: l.net}}
	}
	k := fulapi.ShipmentLineKey{ShipmentID: "ship.old", LineID: "S1"}
	f.lines[k] = fulapi.ShipmentLineRef{Key: k, Quantity: "1", UnitID: "PCS", Code: kernel.CodeReferenceInvalid} // prepared before FUL kept prices
	f.lots["lot.1"] = fulapi.ExportLotRef{ExportLotID: "lot.1", Status: fulapi.LotRegistered, RowVersion: 1, ShipmentID: "ship.1",
		ShipmentStatus: fulapi.StatusPrepared, Order: order, CustomerID: "customer.1", Currency: usd,
		Lines: []fulapi.ExportLotRefLine{{LineID: "E1", ShipmentLineID: "S1", OrderLineID: "L1", SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS",
			Quantity: "50", Amounts: fulapi.Amounts{NetMinor: "40000"}}, {LineID: "E2", ShipmentLineID: "S2", OrderLineID: "L2", SkuID: "sku.2",
			SpecRevision: 1, UnitID: "PCS", Quantity: "10.5", Amounts: fulapi.Amounts{NetMinor: "8400"}}},
		Totals: fulapi.Amounts{NetMinor: "48400"}}
	f.lots["lot.2"] = fulapi.ExportLotRef{ExportLotID: "lot.2", Status: fulapi.LotRegistered, RowVersion: 1, ShipmentID: "ship.2",
		ShipmentStatus: fulapi.StatusCancelled, Order: order, CustomerID: "customer.1", Currency: usd,
		Lines: []fulapi.ExportLotRefLine{{LineID: "E1", ShipmentLineID: "S1", SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS", Quantity: "70",
			Amounts: fulapi.Amounts{NetMinor: "56000"}}}, Totals: fulapi.Amounts{NetMinor: "56000"}}
	f.lots["lot.bad"] = fulapi.ExportLotRef{ExportLotID: "lot.bad", Status: fulapi.LotRegistered, RowVersion: 1, ShipmentID: "ship.gone",
		Order: order, CustomerID: "customer.1", Currency: fulapi.CurrencyRef{ID: "XXX", Revision: 1},
		Lines:  []fulapi.ExportLotRefLine{{LineID: "E1", SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS", Quantity: "1", Amounts: fulapi.Amounts{NetMinor: "1"}}},
		Totals: fulapi.Amounts{NetMinor: "1"}}
	return f
}

// R36 (ExportLotRef v1 and the shipments' lines): the adapter maps FUL's shipments and export lots to
// CNX's exports — state (a lot of a cancelled shipment is cancelled), version, order binding, lines
// with shipment, order line, SKU specification, quantity, unit and net amount, the net value in the
// order currency with POL's minor units; the end user is the consignee (FUL records none), else the
// customer; the destination is the consignee address's country in PTY (S5 D-8); a record that cannot
// be read as an export is REFERENCE_INVALID; what FUL does not show is absent.
func TestFULExports(t *testing.T) {
	ful := fulWorld()
	countries := fakeCountries{{PartyID: "consignee.1", PartyRevision: 4, AddressID: "dock"}: "DE"}
	a := FUL{FUL: func() FULLookups { return ful }, Currencies: fakeCurrencies{}, Countries: countries, Clock: time.Now}
	refs := []application.ExportRef{{Kind: "shipment", ID: "ship.1"}, {Kind: "shipment", ID: "ship.2"}, {Kind: "export_lot", ID: "lot.1"},
		{Kind: "export_lot", ID: "lot.2"}, {Kind: "export_lot", ID: "lot.bad"}, {Kind: "export_lot", ID: "lot.none"},
		{Kind: "shipment", ID: "ship.none"}, {Kind: "shipment", ID: "ship.old"}}
	got, err := a.ResolveExports(context.Background(), refs)
	if err != nil || len(got) != 6 {
		t.Fatalf("%d exports %v", len(got), err)
	}
	s := got[refs[0]]
	if s.Status != "prepared" || s.Version != 3 || s.OrderID != "order.1" || s.OrderRevision != 2 || s.EndUserPartyID != "consignee.1" ||
		s.Destination != "DE" || s.Code != "" || s.Value.String() != "484.00 USD" || len(s.Lines) != 2 {
		t.Fatalf("shipment %+v", s)
	}
	if l := s.Lines[1]; l.LineID != "S2" || l.SkuID != "sku.2" || l.Quantity.String() != "10.5" || l.Unit != "PCS" || l.ShipmentID != "ship.1" ||
		l.OrderLineID != "L1" || l.Amount.String() != "84.00 USD" {
		t.Fatalf("shipment line %+v", l)
	}
	if c := got[refs[1]]; c.Status != "cancelled" || c.EndUserPartyID != "customer.1" || c.Destination != "" || c.Value.String() != "560.00 USD" {
		t.Fatalf("cancelled shipment without consignee %+v", c)
	}
	l := got[refs[2]]
	if l.Status != "registered" || l.Version != 1 || l.EndUserPartyID != "consignee.1" || l.Destination != "DE" || l.Value.String() != "484.00 USD" ||
		len(l.Lines) != 2 || l.Lines[1].LineID != "E2" || l.Lines[1].OrderLineID != "L2" || l.Lines[1].ShipmentID != "ship.1" ||
		l.Lines[1].Amount.String() != "84.00 USD" || l.Code != "" {
		t.Fatalf("lot %+v", l)
	}
	if c := got[refs[3]]; c.Status != application.ExportLotCancelled || c.Code != "" {
		t.Fatalf("a lot of a cancelled shipment %+v", c)
	}
	if b := got[refs[4]]; b.Code != kernel.CodeReferenceInvalid || b.Lines != nil || b.EndUserPartyID != "customer.1" {
		t.Fatalf("a lot whose currency POL does not resolve %+v", b)
	}
	if o := got[refs[7]]; o.Code != kernel.CodeReferenceInvalid || o.Lines != nil {
		t.Fatalf("a shipment without prices %+v", o)
	}
	if ful.reads != 5 {
		t.Fatalf("each shipment is read once: %d reads", ful.reads)
	}
}
