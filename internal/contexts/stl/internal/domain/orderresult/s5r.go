package orderresult

import (
	"fmt"
	"slices"
	"sort"

	fa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/application"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/charge"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The order result after the S5R decisions (S5R unit R8; simulated sign-off E0 of 2026-11-13; STL-INV-011,
// STL-INV-013, STL-INV-010; tactical model STL-AGG-OrderResult notes):
//
//   - S5R-D19 (1): a receipt in the order's currency applied to an order analysed in another currency is valued by
//     what it realised: the part its fx_conversion records converted at their bank rates weighted by the amounts sold
//     (realised), the rest at the POL settlement rate the application recorded when it was made (unrealised). The
//     business settlement FX difference is that value less the receipt at the order's analysis rate. Conversions
//     recorded after the result closed do not reopen it (the snapshot stays; a funds matter).
//   - S5R-D19 (2): net cash = receipts applied − purchase payments applied − charge payments applied (the order's share
//     of each); charges not paid through STL are listed unpaid.
//   - S5R-D20: a confirmed payable adjustment reduces the purchase cost by its net part (S5R-D46(2)), split over its
//     statement's lines by their net amounts.
//   - S5R-D24: a recharged charge is advanced for the buyer and recovered from it: it is neither a cost nor cash of the
//     result (listed apart).
//   - S5R-D42: an approved write_off disposition of PUR (PurchaseLineRef v1) is sunk cost: the commitment line's net
//     for the written-off quantity, never more than was verified and not shipped (FUL's confirmed shipments; R4's cap).
//     A cancelled order's result exists when it has sunk costs — a charge or a write-off — and closes settled, marked
//     cancelled, with no revenue; without any it has no result.

// OrderCancelled is TRD's state of a cancelled order (SM-TRD-ORDER).
const OrderCancelled = "cancelled"

// FX bases of a business settlement FX difference item.
const (
	FXSettlementRate = "settlement_rate" // C8: a receipt in another currency converted at a POL settlement rate when applied
	FXConversion     = "conversion"      // S5R-D19: the part of a receipt in the order's currency its conversions realised
	FXUnrealized     = "unrealized"      // S5R-D19: the part not converted yet, at the settlement rate recorded at application
)

// FundBasis is a fund record applied to the order as its realisation needs it (S5R-D19): what it received, in the
// order's currency, and its fx_conversion records that stand.
type FundBasis struct {
	Received    kernel.Money
	Conversions []FundConversion
}

// FundConversion is an fx_conversion record of a fund: Sold in the fund's currency at the bank's Rate into Currency.
type FundConversion struct {
	ID       string
	Sold     kernel.Money
	Rate     kernel.Decimal
	Currency kernel.Currency
}

// Disposition is an approved disposition of a commitment line's delivered goods (PUR, R31 PurchaseLineRef v1).
type Disposition struct {
	ID       string
	Kind     string // return_to_supplier, write_off or hold_for_reuse
	Quantity kernel.Decimal
}

// DispositionWriteOff is the disposition whose goods are lost: sunk cost (S5R-D42).
const DispositionWriteOff = "write_off"

// realisation is what a receipt in the order's currency realised (S5R-D19).
type realisation struct {
	value kernel.Money // in the result currency: realised + unrealised parts, or the analysis value
	items []FXItem
	gaps  []string
}

// realise values standing application x — a receipt in the order's currency, the result in another currency — by its
// fund's conversions and the settlement rate it recorded.
func (in Input) realise(b basis, x *fa.Application) realisation {
	a := x.Amount
	analysed, _ := b.of(a)
	out := realisation{value: analysed}
	fb, ok := in.Funds[x.FundRecordID]
	if !ok {
		out.gaps = append(out.gaps, "application "+x.ID+": STL shows no fund "+x.FundRecordID)
		return out
	}
	sold, weighted := zero(a.Currency()), kernel.DecimalFromInt(0)
	var ids []string
	for _, c := range fb.Conversions {
		if c.Currency != b.result || c.Sold.Currency() != a.Currency() {
			continue
		}
		sold = add(sold, c.Sold)
		weighted = weighted.Add(c.Sold.Decimal().Mul(c.Rate))
		ids = append(ids, c.ID)
	}
	converted := zero(a.Currency())
	if sold.Sign() > 0 && fb.Received.Sign() > 0 {
		share := kernel.DecimalFromInt(1)
		if c, _ := sold.Cmp(fb.Received); c < 0 {
			share, _ = sold.Decimal().Quo(fb.Received.Decimal())
		}
		converted = must(a.MulHalfUp(share))
	}
	rest := sub(a, converted)
	convertedAnalysed := zero(b.result)
	value := zero(b.result)
	if converted.Sign() > 0 {
		rate, _ := weighted.Quo(sold.Decimal())
		convertedAnalysed, _ = b.of(converted)
		realised := must(kernel.RoundMoney(b.result, converted.Decimal().Mul(rate)))
		value = add(value, realised)
		out.items = append(out.items, FXItem{ApplicationID: x.ID, TargetKind: x.TargetKind, StatementID: x.StatementID, StageID: x.StageID,
			Basis: FXConversion, FundRecordID: x.FundRecordID, Source: converted, Target: realised, Rate: rateString(rate), ConversionIDs: ids,
			Difference: sub(realised, convertedAnalysed)})
	}
	restAnalysed := sub(analysed, convertedAnalysed)
	if rest.Sign() > 0 {
		s := x.Settlement
		if s == nil || s.Currency != b.result {
			out.gaps = append(out.gaps, "application "+x.ID+": "+rest.String()+" not converted and no settlement rate recorded when it was applied: "+
				"valued at the analysis rate")
			value = add(value, restAnalysed)
		} else {
			unrealised := must(kernel.RoundMoney(b.result, rest.Decimal().Mul(s.Value)))
			value = add(value, unrealised)
			out.items = append(out.items, FXItem{ApplicationID: x.ID, TargetKind: x.TargetKind, StatementID: x.StatementID, StageID: x.StageID,
				Basis: FXUnrealized, FundRecordID: x.FundRecordID, Source: rest, Target: unrealised, Rate: s.Value.String(),
				Difference: sub(unrealised, restAnalysed), Unrealized: true})
		}
	}
	out.value = value
	return out
}

// chargePaid is what stands applied to charge c (in its currency).
func (in Input) chargePaid(c *charge.Charge) kernel.Money {
	paid := zero(c.Currency())
	for _, x := range in.ChargePayments[c.ID] {
		if x.Stands() && x.Amount.Currency() == c.Currency() {
			paid = add(paid, x.Amount)
		}
	}
	return paid
}

// orderPart is this order's share of amount m of charge c, converted into the result currency; ok is false when the
// charge is not attributable or does not convert.
func (in Input) orderPart(b basis, c *charge.Charge, m kernel.Money) (kernel.Money, bool) {
	parts, ok := c.SharesOf(m)
	if !ok {
		return kernel.Money{}, false
	}
	total := zero(b.result)
	for _, p := range parts {
		if p.Target.OrderID != in.Order.ID {
			continue
		}
		r, ok := b.charged(c, p.Amount)
		if !ok {
			return kernel.Money{}, false
		}
		total = add(total, r)
	}
	return total, true
}

// chargeCash adds the order's share of the payments applied to its charges to out, and lists what is unpaid (S5R-D19).
func (in Input) chargeCash(b basis, out *Cash) {
	for _, c := range in.Charges {
		if c.Recharged {
			continue
		}
		paid := in.chargePaid(c)
		if paid.Sign() > 0 {
			m, ok := in.orderPart(b, c, paid)
			if !ok {
				out.Gaps = append(out.Gaps, "payments of charge "+c.ID+" are not attributable to the order or do not convert")
			} else {
				out.Payments = add(out.Payments, m)
				out.ChargePayments = add(out.ChargePayments, m)
				out.Sources = append(out.Sources, Source{Kind: "charge_payment", ID: c.ID, Version: c.Version,
					Detail: "the order's part of payments " + m.String() + " of " + paid.String()})
			}
		}
		if c.Amount.Sign() <= 0 {
			continue
		}
		open := fa.ChargeOpen(c, paid)
		if open.Sign() <= 0 {
			continue
		}
		m, ok := in.orderPart(b, c, open)
		if !ok || m.Sign() == 0 {
			continue
		}
		out.Unpaid = add(out.Unpaid, m)
		out.UnpaidSources = append(out.UnpaidSources, Source{Kind: "charge", ID: c.ID, Version: c.Version,
			Detail: c.Status + ", " + c.PaymentStatus(paid) + ": the order's part " + m.String() + " not paid through STL"})
	}
}

// adjustments returns the net part of the confirmed payable adjustments of standing purchase statement st on each of its
// lines (by seq), split by the lines' net amounts (S5R-D20, S5R-D46(2)), and their sources.
func (in Input) adjustments(st *settlement.Statement) (map[int]kernel.Money, []Source) {
	out := map[int]kernel.Money{}
	net := zero(st.Currency())
	var sources []Source
	for _, a := range in.PayableAdjustments[st.ID] {
		if !a.Effective(st) {
			continue
		}
		n, _ := a.Split(st)
		net = add(net, n)
		sources = append(sources, Source{Kind: "payable_adjustment", ID: a.ID, Version: a.Version,
			Detail: a.Kind + " " + a.Amount.String() + " (net " + n.String() + ") on " + st.ID + ", invoice " + a.PurchaseInvoiceID})
	}
	if net.Sign() == 0 || len(st.Lines) == 0 {
		return out, sources
	}
	ids, weights := make([]string, len(st.Lines)), make([]kernel.Decimal, len(st.Lines))
	for i, l := range st.Lines {
		ids[i], weights[i] = fmt.Sprintf("%06d", l.Seq), l.Net.Decimal()
	}
	for i, part := range split(net, ids, weights) {
		out[st.Lines[i].Seq] = part
	}
	return out, sources
}

// writeOff is the write-off cost of the order (S5R-D42): every approved write_off disposition of its commitment lines,
// at the line's net for the quantity (HALF_UP once per line), never more than the order line has verified and not
// shipped (FUL's confirmed shipment lines).
type writeOff struct {
	cost    kernel.Money
	sources []Source
	gaps    []string
	any     bool
}

func (in Input) writeOffs(b basis) writeOff {
	out := writeOff{cost: zero(b.result)}
	verified, shipped := map[string]kernel.Decimal{}, map[string]kernel.Decimal{}
	for _, c := range in.Commitments {
		verified[c.OrderLineID] = verified[c.OrderLineID].Add(c.Verified)
	}
	for _, l := range in.Shipments {
		if l.Status == ShipmentConfirmed {
			shipped[l.OrderLineID] = shipped[l.OrderLineID].Add(l.Quantity)
		}
	}
	room := map[string]kernel.Decimal{}
	for line, v := range verified {
		r := v.Sub(shipped[line])
		if r.Sign() < 0 {
			r = kernel.DecimalFromInt(0)
		}
		room[line] = r
	}
	lines := append([]CommitmentLine(nil), in.Commitments...)
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].OrderLineID != lines[j].OrderLineID {
			return lines[i].OrderLineID < lines[j].OrderLineID
		}
		return lines[i].CommitmentID < lines[j].CommitmentID
	})
	for _, c := range lines {
		wanted := kernel.DecimalFromInt(0)
		var ids []string
		for _, d := range c.Dispositions {
			if d.Kind == DispositionWriteOff {
				wanted, ids = wanted.Add(d.Quantity), append(ids, d.ID)
			}
		}
		if wanted.Sign() == 0 {
			continue
		}
		take := wanted
		if r := room[c.OrderLineID]; take.Cmp(r) > 0 {
			take = r
			out.gaps = append(out.gaps, fmt.Sprintf("write-off %s of commitment %s line %s counted for %s: only that much of order line %s is "+
				"verified and not shipped", wanted, c.CommitmentID, c.OrderLineID, take, c.OrderLineID))
		}
		room[c.OrderLineID] = room[c.OrderLineID].Sub(take)
		if take.Sign() == 0 {
			continue
		}
		if c.Purchased.Sign() <= 0 {
			out.gaps = append(out.gaps, "commitment "+c.CommitmentID+" line "+c.OrderLineID+" states no purchase quantity: its write-off is not valued")
			continue
		}
		share, err := take.Quo(c.Purchased)
		if err != nil {
			continue
		}
		m := must(c.Net.MulHalfUp(share))
		r, ok := b.of(m)
		if !ok {
			out.gaps = append(out.gaps, "write-off of commitment "+c.CommitmentID+" line "+c.OrderLineID+" in "+m.Currency().Code()+
				" does not convert into "+b.result.Code())
			continue
		}
		out.cost, out.any = add(out.cost, r), true
		out.sources = append(out.sources, Source{Kind: "commitment_disposition", ID: c.CommitmentID + "/" + c.OrderLineID,
			Detail: fmt.Sprintf("write_off %s of %s purchased (%s): %s", take, c.Purchased, joinIDs(ids), r)})
	}
	return out
}

// rateString renders a rate exactly when it terminates, at 10 fractional digits HALF_UP otherwise (a weighted rate).
func rateString(d kernel.Decimal) string {
	if s, ok := d.Exact(); ok {
		return s
	}
	return d.StringFixed(10)
}

func joinIDs(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ", "
		}
		out += id
	}
	return out
}

// sunk reports whether a cancelled order has sunk costs (S5R-D42): a charge of it that is not recharged, or a
// write-off.
func (in Input) sunk(w writeOff) bool {
	if w.any {
		return true
	}
	return slices.ContainsFunc(in.Charges, func(c *charge.Charge) bool { return !c.Recharged })
}

// recharged lists the recharged charges of the order (S5R-D24): neither cost nor cash of the result.
func (in Input) recharged() []Source {
	var out []Source
	for _, c := range in.Charges {
		if c.Recharged {
			out = append(out, Source{Kind: "charge", ID: c.ID, Version: c.Version, Detail: c.CostComponent + " " + c.Amount.String() +
				" recharged to the buyer: not in the order result"})
		}
	}
	return out
}

// counted are the charges the result counts: every charge of the order that is not recharged (S5R-D24).
func (in Input) counted() Input {
	out := in
	out.Charges = nil
	for _, c := range in.Charges {
		if !c.Recharged {
			out.Charges = append(out.Charges, c)
		}
	}
	return out
}
