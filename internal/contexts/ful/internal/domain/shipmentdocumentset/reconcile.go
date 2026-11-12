package shipmentdocumentset

import (
	"sort"
	"strconv"
	"strings"
	"unicode"

	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// What a difference's value is compared with (Difference.Against).
const (
	AgainstSet           = "set"
	AgainstShipment      = "shipment"
	AgainstShipmentLine  = "shipment_line"
	AgainstOrderLine     = "order_line"
	AgainstOrder         = "order"
	AgainstActualPacking = "actual_packing"
	AgainstBooking       = "booking"
)

// Difference is one difference of a reconciliation: Path into the document
// ("/packing_list/totals/quantity/PCS"), the document's kind and version, the item, the value the
// document states (Found; "" for none), the value it was compared with (Expected) and where that
// value comes from (Against: one of the Against constants or another document's kind). Required
// marks a missing CI or PL, which no acknowledgement covers (BT12 "缺必需核对不能签发").
type Difference struct {
	Path     string `json:"path"`
	Document string `json:"document"`
	Version  int64  `json:"version"`
	Item     string `json:"item"`
	Found    string `json:"found"`
	Expected string `json:"expected"`
	Against  string `json:"against"`
	Required bool   `json:"required,omitempty"`
}

type diffs struct{ out []Difference }

func (x *diffs) add(d *Document, item, path, found, expected, against string) {
	x.out = append(x.out, Difference{Path: "/" + d.Kind + path, Document: d.Kind, Version: d.Version, Item: item, Found: found,
		Expected: expected, Against: against})
}

func (x *diffs) decimal(d *Document, item, path, found, expected, against string) {
	f, okF := kernel.ParseDecimal(found, kernel.QuantityFormat)
	e, okE := kernel.ParseDecimal(expected, kernel.QuantityFormat)
	if okF == nil && okE == nil && f.Equal(e) {
		return
	}
	if found != expected {
		x.add(d, item, path, found, expected, against)
	}
}

func (x *diffs) text(d *Document, item, path, found, expected, against string) {
	if fold(found) != fold(expected) {
		x.add(d, item, path, found, expected, against)
	}
}

// fold is s as text compares: white space runs one space, trimmed, case folded.
func fold(s string) string {
	return strings.ToLower(strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " "))
}

func partyText(p *PartyContent) string {
	if p == nil {
		return ""
	}
	return p.Name + " / " + strings.Join(strings.Split(p.Address, "\n"), " ")
}

func marks(s string) string {
	if strings.TrimSpace(s) == "" {
		return "N/M"
	}
	return s
}

// Reconcile compares, item by item, the current version of every kind of set s (Set.Current) with
// facts f — the shipment's lines, the order lines' prices the shipment keeps, its current actual
// packing and booking — and with each other (FUL-INV-004, BT12): a missing CI or PL; for the CI
// every shipment line's SKU, quantity and unit (against the shipment line), unit price and amount
// (against the order line's price the shipment keeps), lines of no shipment line, the order
// revision its terms came from (against the shipment's), its consignee (against the order's) and
// ports (against the booking); for the PL every line's packed quantity (against the shipment line),
// its quantity totals (against the CI), cartons, weights and volume and the actual packing it was
// printed from (against the actual packing), consignee, notify and marks (against the CI) and ports;
// for the B/L record its packages, gross weight and volume (against the PL, else the actual
// packing), consignee, notify and marks (against the CI, else the PL), ports, vessel and voyage
// (against the booking, else the CI). The differences are in path order.
func Reconcile(s *Set, f Facts) []Difference {
	x := &diffs{}
	ci, pl, bl := s.Current(KindCommercialInvoice), s.Current(KindPackingList), s.Current(KindBillOfLading)
	for _, k := range []string{KindCommercialInvoice, KindPackingList} {
		if s.Current(k) == nil {
			x.out = append(x.out, Difference{Path: "/" + k, Document: k, Item: "document", Expected: "a " + strings.ReplaceAll(k, "_", " "),
				Against: AgainstSet, Required: true})
		}
	}
	sh := f.Shipment
	var actual *shipment.Packing
	if sh != nil {
		actual = sh.Actual
	}
	var ciContent, plContent *Content
	if ci != nil && ci.Content != nil {
		ciContent = ci.Content
		invoiceLines(x, ci, sh, f.Prices)
		if t := ci.Content.Terms; t != nil && sh != nil && t.OrderRevision != sh.Order.OrderRevision {
			x.add(ci, "order_revision", "/sources/order", "r"+strconv.FormatInt(t.OrderRevision, 10), "r"+strconv.FormatInt(sh.Order.OrderRevision, 10),
				AgainstShipment)
		}
		if oc := ci.Content.OrderConsignee; oc != nil {
			x.text(ci, "consignee", "/consignee", partyText(&ci.Content.Consignee), partyText(oc), AgainstOrder)
		}
		ports(x, ci, ci.Content.PortOfLoading, ci.Content.PortOfDischarge, f.Booking, nil)
	}
	if pl != nil && pl.Content != nil {
		plContent = pl.Content
		packingLines(x, pl, sh, ciContent, actual)
		if ciContent != nil {
			x.text(pl, "consignee", "/consignee", partyText(&pl.Content.Consignee), partyText(&ciContent.Consignee), KindCommercialInvoice)
			x.text(pl, "notify", "/notify", partyText(pl.Content.Notify), partyText(ciContent.Notify), KindCommercialInvoice)
			x.text(pl, "marks", "/marks", marks(pl.Content.Marks), marks(ciContent.Marks), KindCommercialInvoice)
		}
		ports(x, pl, pl.Content.PortOfLoading, pl.Content.PortOfDischarge, f.Booking, ciContent)
	}
	if bl != nil && bl.Record != nil && bl.Record.BillOfLading != nil {
		billOfLading(x, bl, plContent, ciContent, actual, f.Booking)
	}
	sort.SliceStable(x.out, func(i, j int) bool { return x.out[i].Path < x.out[j].Path })
	return x.out
}

func invoiceLines(x *diffs, ci *Document, sh *shipment.Shipment, prices map[string]shipment.Price) {
	if sh == nil {
		return
	}
	stated := map[string]LineContent{}
	for _, l := range ci.Content.Lines {
		stated[l.LineID] = l
	}
	for _, l := range sh.Lines {
		at := "/lines/" + l.ID
		got, ok := stated[l.ID]
		if !ok {
			x.add(ci, "line", at, "", text(l.Quantity)+" "+l.UnitID+" "+l.SkuID, AgainstShipmentLine)
			continue
		}
		delete(stated, l.ID)
		x.text(ci, "sku", at+"/sku", got.SkuID+" r"+strconv.FormatInt(got.SpecRevision, 10), l.SkuID+" r"+strconv.FormatInt(l.SpecRevision, 10),
			AgainstShipmentLine)
		x.decimal(ci, "quantity", at+"/quantity", got.Quantity, text(l.Quantity), AgainstShipmentLine)
		x.text(ci, "unit", at+"/unit", got.UnitID, l.UnitID, AgainstShipmentLine)
		if p, ok := prices[l.ID]; ok {
			x.decimal(ci, "unit_price", at+"/unit_price", got.UnitPrice, text(p.UnitPrice), AgainstOrderLine)
			a := p.Of(l.Quantity, l.OrderQuantity)
			if got.NetMinor != minorText(a.Net) {
				x.add(ci, "amount", at+"/amount", got.NetMinor, minorText(a.Net), AgainstOrderLine)
			}
		}
	}
	for _, l := range ci.Content.Lines {
		if _, extra := stated[l.LineID]; extra {
			x.add(ci, "line", "/lines/"+l.LineID, l.Quantity+" "+l.UnitID+" "+l.SkuID, "", AgainstShipmentLine)
		}
	}
}

func packingLines(x *diffs, pl *Document, sh *shipment.Shipment, ci *Content, actual *shipment.Packing) {
	c := pl.Content
	if sh != nil {
		packed := map[string]string{}
		for _, l := range c.Lines {
			packed[l.LineID] = l.Quantity
		}
		for _, l := range sh.Lines {
			got, ok := packed[l.ID]
			if !ok {
				got = "0"
			}
			x.decimal(pl, "quantity", "/lines/"+l.ID+"/quantity", got, text(l.Quantity), AgainstShipmentLine)
		}
	}
	if ci != nil {
		want := map[string]string{}
		for _, q := range ci.Totals.Quantities {
			want[q.UnitID] = q.Quantity
		}
		got := map[string]string{}
		for _, q := range c.Totals.Quantities {
			got[q.UnitID] = q.Quantity
		}
		units := map[string]bool{}
		for u := range want {
			units[u] = true
		}
		for u := range got {
			units[u] = true
		}
		for u := range units {
			f, e := got[u], want[u]
			if f == "" {
				f = "0"
			}
			if e == "" {
				e = "0"
			}
			x.decimal(pl, "quantity_total", "/totals/quantity/"+u, f, e, KindCommercialInvoice)
		}
	}
	if actual == nil {
		return
	}
	if strings.HasPrefix(c.PackingSource, "actual:") && c.PackingSource != "actual:"+strconv.FormatInt(actual.Seq, 10) {
		x.add(pl, "actual_packing", "/sources/actual_packing", strings.TrimPrefix(c.PackingSource, "actual:"), strconv.FormatInt(actual.Seq, 10),
			AgainstActualPacking)
	}
	x.decimal(pl, "cartons", "/totals/cartons", strconv.FormatInt(c.Totals.Cartons, 10), strconv.FormatInt(actual.Cartons(), 10), AgainstActualPacking)
	if gross, net, volume, ok := actual.Totals(); ok {
		x.decimal(pl, "net_weight_kg", "/totals/net_weight_kg", c.Totals.NetWeightKg, text(net), AgainstActualPacking)
		x.decimal(pl, "gross_weight_kg", "/totals/gross_weight_kg", c.Totals.GrossWeightKg, text(gross), AgainstActualPacking)
		x.decimal(pl, "volume_cbm", "/totals/volume_cbm", c.Totals.VolumeCbm, text(volume), AgainstActualPacking)
	}
}

// ports compares a version's ports with the booking, else with the CI (when the version is not the
// CI).
func ports(x *diffs, d *Document, loading, discharge string, b *shipment.Booking, ci *Content) {
	switch {
	case b != nil:
		x.text(d, "port_of_loading", "/port_of_loading", loading, b.PortOfLoading, AgainstBooking)
		x.text(d, "port_of_discharge", "/port_of_discharge", discharge, b.PortOfDischarge, AgainstBooking)
	case ci != nil:
		x.text(d, "port_of_loading", "/port_of_loading", loading, ci.PortOfLoading, KindCommercialInvoice)
		x.text(d, "port_of_discharge", "/port_of_discharge", discharge, ci.PortOfDischarge, KindCommercialInvoice)
	}
}

func billOfLading(x *diffs, bl *Document, pl, ci *Content, actual *shipment.Packing, b *shipment.Booking) {
	r := bl.Record.BillOfLading
	switch {
	case pl != nil:
		x.decimal(bl, "packages", "/packages", strconv.FormatInt(r.Packages, 10), strconv.FormatInt(pl.Totals.Cartons, 10), KindPackingList)
		x.decimal(bl, "gross_weight_kg", "/gross_weight_kg", r.GrossWeightKg, pl.Totals.GrossWeightKg, KindPackingList)
		if r.VolumeCbm != "" {
			x.decimal(bl, "volume_cbm", "/volume_cbm", r.VolumeCbm, pl.Totals.VolumeCbm, KindPackingList)
		}
	case actual != nil:
		x.decimal(bl, "packages", "/packages", strconv.FormatInt(r.Packages, 10), strconv.FormatInt(actual.Cartons(), 10), AgainstActualPacking)
		if gross, _, volume, ok := actual.Totals(); ok {
			x.decimal(bl, "gross_weight_kg", "/gross_weight_kg", r.GrossWeightKg, text(gross), AgainstActualPacking)
			if r.VolumeCbm != "" {
				x.decimal(bl, "volume_cbm", "/volume_cbm", r.VolumeCbm, text(volume), AgainstActualPacking)
			}
		}
	}
	ref, against := ci, KindCommercialInvoice
	if ref == nil {
		ref, against = pl, KindPackingList
	}
	if ref != nil {
		x.text(bl, "consignee", "/consignee", partyText(&r.Consignee), partyText(&ref.Consignee), against)
		x.text(bl, "notify", "/notify", partyText(r.Notify), partyText(ref.Notify), against)
		x.text(bl, "marks", "/marks", marks(r.Marks), marks(ref.Marks), against)
	}
	ports(x, bl, r.PortOfLoading, r.PortOfDischarge, b, ci)
	if b != nil {
		x.text(bl, "vessel", "/vessel", r.Vessel, b.Vessel, AgainstBooking)
		x.text(bl, "voyage", "/voyage", r.Voyage, b.Voyage, AgainstBooking)
	}
}

// Seal returns the differences as canonical JSON and its SHA-256: what an issue binds.
func Seal(d []Difference) (string, string) {
	if d == nil {
		d = []Difference{}
	}
	return Canonical(d)
}
