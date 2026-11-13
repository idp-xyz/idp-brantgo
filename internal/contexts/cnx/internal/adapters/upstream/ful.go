package upstream

import (
	"context"
	"math/big"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/application"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// FULLookups is what CNX's port to FUL reads (Context Map R36 FUL→CNX, ExportLotRef v1 and the
// shipments' lines, guardLock false): the export lot and shipment line lookups, and the shipment
// read for a shipment's lines and the parties it binds. FUL publishes no lookup of a whole shipment
// by its ID, so the shipment comes from GetShipment, which FUL audits as shipment.get and which
// needs ful.read (the documentation and tax specialist templates hold it).
type FULLookups interface {
	fulapi.ExportLotLookups
	fulapi.ShipmentLineLookups
	GetShipment(ctx context.Context, shipmentID string) (fulapi.ShipmentView, error)
}

// Currencies is the part of POL's queries the adapter needs: the currency revision an export's
// amounts are in, for its minor units (POL is upstream of CNX, R50).
type Currencies interface {
	ResolveConfigs(ctx context.Context, refs []polapi.ConfigRef, at time.Time) (map[polapi.ConfigRef]polapi.ResolvedConfig, error)
}

// FUL answers application.Exports from FUL's lookups, before CNX's transaction, with FUL's data
// scope (a shipment or lot the caller does not see is absent):
//
//   - a shipment: its state (prepared, confirmed or cancelled), row version, the order binding, every
//     line with its SKU specification revision, quantity and unit and its net amount at the order's
//     price at the bound revision, and the shipment's net value in the order currency;
//   - an export lot: "registered", or "cancelled" when its shipment was cancelled (a lot is never
//     changed itself), its row version, the order binding, every lot line with its shipment, order
//     line, SKU specification revision, quantity, unit and net amount, and the lot's net value.
//
// Both carry the end user and the destination from the parties the shipment binds, as TRD sealed
// them: FUL records no end user, so the end user is the consignee the order binds, or the order's
// customer when it binds none; the destination is the country PTY records for the consignee's
// address (CNX's port Countries, R77; "" when the order binds no consignee address or PTY records no
// country for it — the licence allocation then takes the destination the trade compliance
// evaluation of the order revision recorded, S5 D-8). FUL records no export date: a lot's export
// date is the one customs states on its declaration, which CNX records itself (S5 C3a).
//
// Code is REFERENCE_INVALID when FUL's record cannot be read as an export: no lines, a quantity, unit
// or amount that does not parse, a line without a price, or a currency POL does not resolve.
//
// FUL is constructed after CNX (bootstrap), so the lookups are bound through a function called at
// lookup time (S5 D-2).
type FUL struct {
	FUL        func() FULLookups
	Currencies Currencies
	Countries  application.Countries
	Clock      func() time.Time
}

var _ application.Exports = FUL{}

// ResolveExports asks FUL, POL and PTY before CNX's transaction.
func (f FUL) ResolveExports(ctx context.Context, refs []application.ExportRef) (map[application.ExportRef]application.Export, error) {
	out := map[application.ExportRef]application.Export{}
	ful := f.FUL()
	var lotIDs []string
	for _, r := range refs {
		if r.Kind == api.TargetExportLot && !slices.Contains(lotIDs, r.ID) {
			lotIDs = append(lotIDs, r.ID)
		}
	}
	lots := map[string]fulapi.ExportLotRef{}
	for start := 0; start < len(lotIDs); start += fulapi.MaxLotLookups {
		got, err := ful.ResolveExportLots(ctx, lotIDs[start:min(start+fulapi.MaxLotLookups, len(lotIDs))])
		if err != nil {
			return nil, err
		}
		for id, l := range got {
			lots[id] = l
		}
	}
	shipments := map[string]*fulapi.ShipmentView{}
	shipment := func(id string) (*fulapi.ShipmentView, error) {
		if v, ok := shipments[id]; ok {
			return v, nil
		}
		v, err := ful.GetShipment(ctx, id)
		if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodeNotFound {
			shipments[id] = nil
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		shipments[id] = &v
		return &v, nil
	}
	for _, r := range refs {
		var x application.Export
		switch r.Kind {
		case api.TargetShipment:
			s, err := shipment(r.ID)
			if err != nil {
				return nil, err
			}
			if s == nil {
				continue
			}
			if x, err = f.shipmentExport(ctx, ful, r, s); err != nil {
				return nil, err
			}
		case api.TargetExportLot:
			l, ok := lots[r.ID]
			if !ok {
				continue
			}
			s, err := shipment(l.ShipmentID)
			if err != nil {
				return nil, err
			}
			if x, err = f.lotExport(ctx, r, l, s); err != nil {
				return nil, err
			}
		default:
			continue
		}
		out[r] = x
	}
	return out, nil
}

// shipmentExport reads a shipment's lines with their amounts.
func (f FUL) shipmentExport(ctx context.Context, ful FULLookups, r application.ExportRef, s *fulapi.ShipmentView) (application.Export, error) {
	x := application.Export{Ref: r, Status: s.Status, Version: s.RowVersion, OrderID: s.Order.OrderID, OrderRevision: s.Order.OrderRevision,
		OrderHash: s.Order.OrderHash}
	if err := f.parties(ctx, &x, s, s.CustomerID); err != nil {
		return x, err
	}
	keys := make([]fulapi.ShipmentLineKey, len(s.Lines))
	for i, l := range s.Lines {
		keys[i] = fulapi.ShipmentLineKey{ShipmentID: s.ShipmentID, LineID: l.LineID}
	}
	refs, err := ful.ResolveShipmentLines(ctx, keys)
	if err != nil {
		return x, err
	}
	var currency *fulapi.CurrencyRef
	total := new(big.Int)
	intact := len(s.Lines) > 0 && len(s.Lines) <= fulapi.MaxLineLookups
	for _, k := range keys {
		l, ok := refs[k]
		if !ok || l.Price == nil {
			intact = false
			continue
		}
		if currency == nil {
			currency = &l.Price.Currency
		}
		net, ok1 := new(big.Int).SetString(l.Amounts.NetMinor, 10)
		q, err := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		if !ok1 || err != nil || *currency != l.Price.Currency || !kernel.UnitCode(l.UnitID).Valid() {
			intact = false
			continue
		}
		total.Add(total, net)
		x.Lines = append(x.Lines, application.ExportLine{LineID: l.Key.LineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision, Quantity: q,
			Unit: kernel.UnitCode(l.UnitID), ShipmentID: s.ShipmentID, OrderLineID: l.OrderLineID})
	}
	if !intact || currency == nil {
		x.Code, x.Lines = kernel.CodeReferenceInvalid, nil
		return x, nil
	}
	return f.value(ctx, x, *currency, total, func(c kernel.Currency, i int) (kernel.Money, error) {
		return kernel.ParseMinor(c, refs[keys[i]].Amounts.NetMinor)
	})
}

// lotExport reads an export lot.
func (f FUL) lotExport(ctx context.Context, r application.ExportRef, l fulapi.ExportLotRef, s *fulapi.ShipmentView) (application.Export, error) {
	x := application.Export{Ref: r, Status: l.Status, Version: l.RowVersion, OrderID: l.Order.OrderID, OrderRevision: l.Order.OrderRevision,
		OrderHash: l.Order.OrderHash}
	if l.ShipmentStatus == fulapi.StatusCancelled {
		x.Status = application.ExportLotCancelled
	}
	if s != nil {
		if err := f.parties(ctx, &x, s, l.CustomerID); err != nil {
			return x, err
		}
	} else {
		x.EndUserPartyID = l.CustomerID
	}
	total, ok := new(big.Int).SetString(l.Totals.NetMinor, 10)
	intact := ok && len(l.Lines) > 0
	for _, line := range l.Lines {
		q, err := kernel.ParseDecimal(line.Quantity, kernel.QuantityFormat)
		if _, ok := new(big.Int).SetString(line.Amounts.NetMinor, 10); !ok || err != nil || !kernel.UnitCode(line.UnitID).Valid() {
			intact = false
			continue
		}
		x.Lines = append(x.Lines, application.ExportLine{LineID: line.LineID, SkuID: line.SkuID, SpecRevision: line.SpecRevision, Quantity: q,
			Unit: kernel.UnitCode(line.UnitID), ShipmentID: l.ShipmentID, OrderLineID: line.OrderLineID, ShipmentLineID: line.ShipmentLineID})
	}
	if !intact {
		x.Code, x.Lines = kernel.CodeReferenceInvalid, nil
		return x, nil
	}
	return f.value(ctx, x, l.Currency, total, func(c kernel.Currency, i int) (kernel.Money, error) {
		return kernel.ParseMinor(c, l.Lines[i].Amounts.NetMinor)
	})
}

// value puts the export's value and its lines' amounts in the order currency, whose minor units
// POL's revision states.
func (f FUL) value(ctx context.Context, x application.Export, ref fulapi.CurrencyRef, total *big.Int,
	line func(kernel.Currency, int) (kernel.Money, error)) (application.Export, error) {
	pref := polapi.ConfigRef{Kind: polapi.KindCurrency, ConfigID: ref.ID, Revision: ref.Revision}
	got, err := f.Currencies.ResolveConfigs(ctx, []polapi.ConfigRef{pref}, f.Clock())
	if err != nil {
		return x, err
	}
	invalid := func() (application.Export, error) {
		x.Code, x.Lines, x.Value = kernel.CodeReferenceInvalid, nil, kernel.Money{}
		return x, nil
	}
	r, ok := got[pref]
	if !ok || r.Data.Currency == nil {
		return invalid()
	}
	c, err := kernel.NewCurrency(r.Data.Currency.Code, r.Data.Currency.MinorUnits)
	if err != nil {
		return invalid()
	}
	if x.Value, err = kernel.MoneyFromMinor(c, total); err != nil {
		return invalid()
	}
	for i := range x.Lines {
		if x.Lines[i].Amount, err = line(c, i); err != nil {
			return invalid()
		}
	}
	return x, nil
}

// parties sets the end user and the destination from the parties shipment s binds: the consignee
// (one party; several are no end user) and the country PTY records for its address (one country;
// several or none are no destination), the customer when the order binds no consignee.
func (f FUL) parties(ctx context.Context, x *application.Export, s *fulapi.ShipmentView, customer string) error {
	var consignees []string
	var addresses []application.AddressRef
	for _, p := range s.Parties {
		if p.Role != ptyapi.AddressConsignee {
			continue
		}
		if !slices.Contains(consignees, p.PartyID) {
			consignees = append(consignees, p.PartyID)
		}
		if p.AddressID != "" {
			addresses = append(addresses, application.AddressRef{PartyID: p.PartyID, PartyRevision: p.PartyRevision, AddressID: p.AddressID})
		}
	}
	switch len(consignees) {
	case 0:
		x.EndUserPartyID = customer
		return nil
	case 1:
		x.EndUserPartyID = consignees[0]
	}
	countries, err := f.Countries.ResolveAddressCountries(ctx, addresses)
	if err != nil {
		return err
	}
	var found []string
	for _, a := range addresses {
		if c := countries[a].Country; c != "" && !slices.Contains(found, c) {
			found = append(found, c)
		}
	}
	if len(found) == 1 {
		x.Destination = found[0]
	}
	return nil
}
