package orderresult

import (
	"fmt"
	"slices"
	"sort"
	"time"

	fa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/application"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/charge"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The order's facts the figures rest on (baseline 4.5): TRD's order at its current revision, FUL's shipment lines,
// PUR's commitment lines, and STL's own statements, credit notes, applications and charges.

// TRD's ends of an active order a closed result accepts (STL-INV-011 "履约完成或批准短交关闭").
const (
	EndClose          = "order.close"
	EndCloseRemainder = "order.close_remainder"
	OrderClosed       = "closed"
)

// Order is what TRD says of the order now (OrderCommercialSnapshot v1): binding, state and how it ended, its
// currency and the current revision's net sales, and its analysis rate (nil: analysed in its own currency).
type Order struct {
	ID       string
	Revision int64
	Hash     string
	Status   string
	Ended    string
	Currency kernel.Currency
	Net      kernel.Money
	Analysis *Analysis
}

// Analysis is the order's analysis basis (TRD AnalysisFX, fixed when the order took effect): one unit of the
// order's currency is Rate units of Currency.
type Analysis struct {
	Currency kernel.Currency
	Rate     kernel.Decimal
}

// ShipmentLine is a line of a FUL shipment of the order (ShipmentLineRef v1), in any state.
type ShipmentLine struct {
	ShipmentID  string
	LineID      string
	Status      string // prepared, confirmed or cancelled
	OrderLineID string
	Quantity    kernel.Decimal
}

// ShipmentConfirmed is FUL's state of a confirmed shipment.
const ShipmentConfirmed = "confirmed"

// CommitmentLine is a line of a PUR purchase commitment of the order (PurchaseLineRef v1): its state, what it
// buys for (the sealed net amount: the expected purchase cost) and its verified deliveries.
type CommitmentLine struct {
	CommitmentID string
	OrderLineID  string
	Status       string
	SupplierID   string
	Net          kernel.Money
	Deliveries   []Delivery
}

// Delivery is a verified supplier delivery of a commitment line, by the source key of a purchase statement line
// (the delivery and the order line).
type Delivery struct {
	DeliveryID string
	LineID     string
	Version    int64
}

// Buying are the commitment states that buy (PUR: issued or confirmed; a requested or approved commitment has
// not bought yet, a cancelled one buys nothing more).
var Buying = []string{"issued", "confirmed"}

// Input is everything the figures and close conditions of one order are derived from, as of At.
type Input struct {
	At                  time.Time
	Order               Order
	Sales               []*settlement.Statement // the order's sales statements, reversed ones included
	CreditNotes         map[string][]settlement.CreditNote
	Purchases           []*settlement.Statement         // the purchase statements with a line of the order
	Applied             map[string]map[int]kernel.Money // per statement: what stands applied on each installment
	Applications        []fa.Application                // the order's applications to its stages and receivables
	PayableApplications map[string][]fa.Application     // per purchase statement
	Charges             []*charge.Charge                // the charges with a beneficiary of the order
	Shipments           []ShipmentLine                  // FUL's lines of the order's shipments
	ShipmentsShown      bool                            // FUL answered
	Commitments         []CommitmentLine                // PUR's commitment lines of the order
	CommitmentsShown    bool                            // PUR answered
}

// Source is a record a figure counts: its kind, ID, version and what of it counts.
type Source struct {
	Kind    string
	ID      string
	Version int64
	Detail  string
}

// Figure is one view of the result in the analysis currency.
type Figure struct {
	NetSales          kernel.Money
	Credits           kernel.Money
	PurchaseCost      kernel.Money
	PurchaseConfirmed kernel.Money
	PurchaseEstimated kernel.Money
	DirectCharges     kernel.Money
	ChargesConfirmed  kernel.Money
	ChargesEstimated  kernel.Money
	Result            kernel.Money
	Margin            string // result ÷ (net sales − credits), percent HALF_UP to 2 places; "" when nothing is sold
	Sources           []Source
	Gaps              []string
}

// Batch is the confirmed result of one shipment the order's standing sales statements settle (S-A "分批").
type Batch struct {
	ShipmentID    string
	StatementIDs  []string
	NetSales      kernel.Money
	Credits       kernel.Money
	CostOfSales   kernel.Money
	DirectCharges kernel.Money
	Result        kernel.Money
}

// Confirmed is the confirmed result with its batches, the direct charges of the order itself (or of a shipment
// not settled yet) and the confirmed purchase cost of goods not sold yet (not cost of sales).
type Confirmed struct {
	Figure
	Batches            []Batch
	OrderLevelCharges  kernel.Money
	UnsoldPurchaseCost kernel.Money
}

// FXItem is one cross-currency receipt of the order and its business settlement FX difference.
type FXItem struct {
	ApplicationID string
	TargetKind    string
	StatementID   string
	StageID       string
	Source        kernel.Money
	Target        kernel.Money
	Difference    kernel.Money
}

// FX is the business settlement FX difference: the order's cross-currency receipts against its analysis rate
// (baseline 4.5 "资金换算与相应业务分析基准之间的差额，单列"). Never a receipt or revenue.
type FX struct {
	Difference kernel.Money
	Items      []FXItem
	Gaps       []string
}

// Cash is the net cash result: receipts applied to the order less payments applied to its share of purchase
// payables (baseline 4.5 "实际收款－实际付款；明确范围，不等同于利润"); charges are not paid through STL.
type Cash struct {
	Receipts kernel.Money
	Payments kernel.Money
	Net      kernel.Money
	Sources  []Source
	Gaps     []string
}

// Figures are the order result as of AsOf in Currency (the order's analysis currency).
type Figures struct {
	AsOf      time.Time
	Currency  kernel.Currency
	Rate      string // the analysis rate from the order's currency ("1": the same currency)
	Forecast  Figure
	Executing Figure
	Confirmed Confirmed
	FX        FX
	Cash      Cash
}

func must(m kernel.Money, err error) kernel.Money {
	if err != nil {
		panic("stl: order result amounts in one currency: " + err.Error())
	}
	return m
}

func zero(c kernel.Currency) kernel.Money { return must(kernel.NewMoney(c, 0)) }

func add(a, b kernel.Money) kernel.Money { return must(a.Add(b)) }

func sub(a, b kernel.Money) kernel.Money { return must(a.Sub(b)) }

// basis converts amounts into the result currency.
type basis struct {
	order, result kernel.Currency
	rate          kernel.Decimal // order → result
}

func basisOf(o Order) basis {
	b := basis{order: o.Currency, result: o.Currency, rate: kernel.DecimalFromInt(1)}
	if a := o.Analysis; a != nil && a.Currency != o.Currency && a.Rate.Sign() > 0 {
		b.result, b.rate = a.Currency, a.Rate
	}
	return b
}

// of converts m into the result currency: as it is when it is in it, at the analysis rate (HALF_UP once) when it
// is in the order's currency; ok is false for any other currency (no rate known to STL relates them).
func (b basis) of(m kernel.Money) (kernel.Money, bool) {
	switch m.Currency() {
	case b.result:
		return m, true
	case b.order:
		return must(kernel.RoundMoney(b.result, m.Decimal().Mul(b.rate))), true
	}
	return kernel.Money{}, false
}

// charged converts m, an amount of charge c, into the result currency by the charge's conversion basis.
func (b basis) charged(c *charge.Charge, m kernel.Money) (kernel.Money, bool) {
	switch c.Conversion.Kind {
	case charge.ConversionSameCurrency:
		if m.Currency() == b.result {
			return m, true
		}
	case charge.ConversionOrderAnalysis:
		if m.Currency() == b.order {
			return b.of(m)
		}
	case charge.ConversionFXRate:
		if r := c.Conversion.Rate; r != nil && r.To == b.result.Code() {
			return must(kernel.RoundMoney(b.result, m.Decimal().Mul(r.Value))), true
		}
	}
	return kernel.Money{}, false
}

// Converts reports whether charge c converts into the result currency of order o by its basis.
func Converts(c *charge.Charge, o Order) bool {
	_, ok := basisOf(o).charged(c, c.Amount)
	return ok
}

// split splits total over weights by the largest remainder in minor units, ties by ID; a negative total on its
// absolute value. Negative weights count as zero; with no positive weight the first part takes everything.
func split(total kernel.Money, ids []string, weights []kernel.Decimal) []kernel.Money {
	out := make([]kernel.Money, len(ids))
	for i := range out {
		out[i] = zero(total.Currency())
	}
	if len(ids) == 0 {
		return out
	}
	shares := make([]kernel.Share, len(ids))
	positive := false
	for i, id := range ids {
		w := weights[i]
		if w.Sign() < 0 {
			w = kernel.DecimalFromInt(0)
		}
		positive = positive || w.Sign() > 0
		shares[i] = kernel.Share{ID: id, Weight: w}
	}
	if !positive {
		out[0] = total
		return out
	}
	abs := total
	if total.Sign() < 0 {
		abs = total.Neg()
	}
	parts, err := abs.Allocate(shares)
	if err != nil {
		panic("stl: order result split: " + err.Error())
	}
	for i := range parts {
		if total.Sign() < 0 {
			parts[i] = parts[i].Neg()
		}
		out[i] = parts[i]
	}
	return out
}

// margin is result ÷ base in percent, HALF_UP to 2 places ("31.25"); "" for no base.
func margin(result, base kernel.Money) string {
	if base.Sign() <= 0 {
		return ""
	}
	q, err := result.Decimal().Mul(kernel.DecimalFromInt(100)).Quo(base.Decimal())
	if err != nil {
		return ""
	}
	return q.StringFixed(2)
}

func standing(st *settlement.Statement) bool { return st.Status == settlement.StatusConfirmed }

func (in Input) applied(st *settlement.Statement) map[int]kernel.Money {
	if a, ok := in.Applied[st.ID]; ok {
		return a
	}
	return map[int]kernel.Money{}
}

// share is one charge's part of this order, converted.
type share struct {
	charge *charge.Charge
	target charge.Target
	amount kernel.Money
}

// chargeShares returns, for each charge counted (counts nil: every one), its parts of this order of
// amountOf(charge), converted into the result currency; gaps name the charges that are not attributable or do not
// convert.
func (in Input) chargeShares(b basis, amountOf func(*charge.Charge) kernel.Money, counts func(*charge.Charge) bool) (out []share, gaps []string) {
	for _, c := range in.Charges {
		if counts != nil && !counts(c) {
			continue
		}
		parts, ok := c.SharesOf(amountOf(c))
		if !ok {
			gaps = append(gaps, "charge "+c.ID+" benefits several orders and is not allocated")
			continue
		}
		for _, p := range parts {
			if p.Target.OrderID != in.Order.ID {
				continue
			}
			m, ok := b.charged(c, p.Amount)
			if !ok {
				gaps = append(gaps, "charge "+c.ID+" in "+p.Amount.Currency().Code()+" does not convert into "+b.result.Code()+
					" by its basis "+c.Conversion.Kind)
				break
			}
			out = append(out, share{charge: c, target: p.Target, amount: m})
		}
	}
	return out, gaps
}

func chargeSource(c *charge.Charge, detail string) Source {
	return Source{Kind: "charge", ID: c.ID, Version: c.Version, Detail: detail}
}

// lineCost is the confirmed purchase cost of an order line and the quantity it buys.
type lineCost struct {
	cost      kernel.Money
	purchased kernel.Decimal
}

// purchases is the confirmed purchase side of the order: its cost per commitment line and per order line, with
// the sources and what did not convert.
type purchases struct {
	total   kernel.Money
	byLine  map[string]kernel.Money // commitment/order line
	byOrder map[string]*lineCost    // order line
	sources []Source
	gaps    []string
}

func (in Input) purchases(b basis) purchases {
	p := purchases{total: zero(b.result), byLine: map[string]kernel.Money{}, byOrder: map[string]*lineCost{}}
	for _, st := range in.Purchases {
		if !standing(st) {
			continue
		}
		for _, l := range st.Lines {
			if l.OrderID != in.Order.ID {
				continue
			}
			m, ok := b.of(l.Net)
			if !ok {
				p.gaps = append(p.gaps, fmt.Sprintf("purchase statement %s line %d in %s does not convert into %s", st.ID, l.Seq,
					l.Net.Currency().Code(), b.result.Code()))
				continue
			}
			p.total = add(p.total, m)
			k := l.CommitmentID + "/" + l.OrderLineID
			if x, ok := p.byLine[k]; ok {
				m2 := add(x, m)
				p.byLine[k] = m2
			} else {
				p.byLine[k] = m
			}
			lc := p.byOrder[l.OrderLineID]
			if lc == nil {
				lc = &lineCost{cost: zero(b.result), purchased: kernel.DecimalFromInt(0)}
				p.byOrder[l.OrderLineID] = lc
			}
			lc.cost, lc.purchased = add(lc.cost, m), lc.purchased.Add(l.Quantity)
			p.sources = append(p.sources, Source{Kind: "purchase_statement", ID: st.ID, Version: st.Version,
				Detail: fmt.Sprintf("line %d: %s of order line %s from %s, net %s", l.Seq, l.Quantity, l.OrderLineID, l.Source.ID, l.Net)})
		}
	}
	return p
}

// Compute derives the figures and close conditions of in.Order as of in.At (baseline 4.5, STL-INV-011).
func Compute(in Input) (Figures, []Dimension) {
	b := basisOf(in.Order)
	f := Figures{AsOf: kernel.Instant(in.At), Currency: b.result, Rate: b.rate.String()}
	orderSource := Source{Kind: "order", ID: in.Order.ID, Version: in.Order.Revision,
		Detail: "net sales of the current revision " + in.Order.Net.String()}
	net, _ := b.of(in.Order.Net)

	// Expected purchase cost: the commitment lines that buy for the order (PUR).
	expected := zero(b.result)
	expectedBy := map[string]kernel.Money{}
	var commitmentSources []Source
	var expectGaps []string
	if !in.CommitmentsShown {
		expectGaps = append(expectGaps, "PUR shows the caller no purchase commitment of the order: no expected purchase cost")
	}
	for _, c := range in.Commitments {
		if !slices.Contains(Buying, c.Status) {
			continue
		}
		m, ok := b.of(c.Net)
		if !ok {
			expectGaps = append(expectGaps, "commitment "+c.CommitmentID+" line "+c.OrderLineID+" in "+c.Net.Currency().Code()+
				" does not convert into "+b.result.Code())
			continue
		}
		expected = add(expected, m)
		expectedBy[c.CommitmentID+"/"+c.OrderLineID] = m
		commitmentSources = append(commitmentSources, Source{Kind: "commitment_line", ID: c.CommitmentID + "/" + c.OrderLineID,
			Detail: c.Status + ", expected purchase cost " + c.Net.String()})
	}

	// Forecast (报价预计经营毛利): net sales − expected purchase cost − expected direct charges: the charges that began
	// as estimates, at their estimated amount (a charge first recorded with its bill was not expected; it counts in
	// the executing and confirmed views).
	fc := Figure{NetSales: net, Credits: zero(b.result), PurchaseCost: expected, PurchaseConfirmed: zero(b.result), PurchaseEstimated: expected,
		DirectCharges: zero(b.result), ChargesConfirmed: zero(b.result)}
	fShares, fGaps := in.chargeShares(b, (*charge.Charge).ForecastAmount, func(c *charge.Charge) bool { return c.Estimate != nil })
	fc.Sources = append([]Source{orderSource}, commitmentSources...)
	for _, s := range fShares {
		fc.DirectCharges = add(fc.DirectCharges, s.amount)
		fc.Sources = append(fc.Sources, chargeSource(s.charge, "expected "+s.amount.String()))
	}
	fc.ChargesEstimated = fc.DirectCharges
	fc.Result = sub(sub(net, expected), fc.DirectCharges)
	fc.Margin = margin(fc.Result, net)
	fc.Gaps = append(append([]string{}, expectGaps...), fGaps...)
	f.Forecast = fc

	// Executing (执行中预计经营毛利): net sales − confirmed costs − an estimate of the rest (each commitment line's
	// expected cost beyond what is settled; charges not confirmed at their amount now).
	p := in.purchases(b)
	ex := Figure{NetSales: net, Credits: zero(b.result), PurchaseConfirmed: p.total, PurchaseEstimated: zero(b.result),
		ChargesConfirmed: zero(b.result), ChargesEstimated: zero(b.result)}
	keys := make([]string, 0, len(expectedBy))
	for k := range expectedBy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		rest := expectedBy[k]
		if c, ok := p.byLine[k]; ok {
			rest = sub(rest, c)
		}
		if rest.Sign() > 0 {
			ex.PurchaseEstimated = add(ex.PurchaseEstimated, rest)
		}
	}
	ex.PurchaseCost = add(ex.PurchaseConfirmed, ex.PurchaseEstimated)
	cShares, cGaps := in.chargeShares(b, func(c *charge.Charge) kernel.Money { return c.Amount }, nil)
	ex.Sources = append(append([]Source{orderSource}, commitmentSources...), p.sources...)
	for _, s := range cShares {
		if s.charge.Status == charge.StatusConfirmed {
			ex.ChargesConfirmed = add(ex.ChargesConfirmed, s.amount)
		} else {
			ex.ChargesEstimated = add(ex.ChargesEstimated, s.amount)
		}
		ex.Sources = append(ex.Sources, chargeSource(s.charge, s.charge.Status+" "+s.amount.String()))
	}
	ex.DirectCharges = add(ex.ChargesConfirmed, ex.ChargesEstimated)
	ex.Result = sub(sub(net, ex.PurchaseCost), ex.DirectCharges)
	ex.Margin = margin(ex.Result, net)
	ex.Gaps = append(append(append([]string{}, expectGaps...), p.gaps...), cGaps...)
	f.Executing = ex

	f.Confirmed = in.confirmed(b, p, cShares, cGaps)
	f.FX = in.fx(b)
	f.Cash = in.cash(b)
	return f, in.conditions(b, p)
}

// batchOf collects one batch while the confirmed result is derived.
type batchOf struct {
	statements []string
	net        kernel.Money // order currency
	credits    kernel.Money // order currency
	sold       map[string]kernel.Decimal
}

// confirmed is the confirmed result (已确认经营毛利): confirmed net sales (standing sales statements less their
// effective credit notes) − the confirmed purchase cost of the goods sold (each order line's settled purchase cost
// over the quantity sold and the quantity not sold yet, by the largest remainder) − confirmed direct charges, per
// shipment batch; charges of the order itself, or of a shipment not settled yet, are the order's.
func (in Input) confirmed(b basis, p purchases, shares []share, chargeGaps []string) Confirmed {
	oc := b.order
	out := Confirmed{Figure: Figure{NetSales: zero(b.result), Credits: zero(b.result), PurchaseCost: zero(b.result),
		PurchaseConfirmed: zero(b.result), PurchaseEstimated: zero(b.result), DirectCharges: zero(b.result), ChargesConfirmed: zero(b.result),
		ChargesEstimated: zero(b.result)}, OrderLevelCharges: zero(b.result), UnsoldPurchaseCost: zero(b.result)}
	batches := map[string]*batchOf{}
	var order []string
	totalNet, totalCredits := zero(oc), zero(oc)
	soldBy := map[string]kernel.Decimal{}
	for _, st := range in.Sales {
		if !standing(st) {
			continue
		}
		ids, weights := []string{}, []kernel.Decimal{}
		for _, l := range st.Lines {
			if l.Source.Kind != settlement.SourceShipmentLine || l.Net.Currency() != oc {
				continue
			}
			bt := batches[l.Source.ID]
			if bt == nil {
				bt = &batchOf{net: zero(oc), credits: zero(oc), sold: map[string]kernel.Decimal{}}
				batches[l.Source.ID] = bt
				order = append(order, l.Source.ID)
			}
			if !slices.Contains(bt.statements, st.ID) {
				bt.statements = append(bt.statements, st.ID)
			}
			bt.net = add(bt.net, l.Net)
			q := bt.sold[l.OrderLineID]
			bt.sold[l.OrderLineID] = q.Add(l.Quantity)
			s := soldBy[l.OrderLineID]
			soldBy[l.OrderLineID] = s.Add(l.Quantity)
			totalNet = add(totalNet, l.Net)
			if !slices.Contains(ids, l.Source.ID) {
				ids, weights = append(ids, l.Source.ID), append(weights, l.Net.Decimal())
			} else {
				i := slices.Index(ids, l.Source.ID)
				weights[i] = weights[i].Add(l.Net.Decimal())
			}
		}
		out.Sources = append(out.Sources, Source{Kind: "sales_statement", ID: st.ID, Version: st.Version, Detail: "net " + st.Net.String()})
		credited := zero(oc)
		for _, n := range in.CreditNotes[st.ID] {
			if n.Effective(st) && n.Amount.Currency() == oc {
				credited = add(credited, n.Amount)
				out.Sources = append(out.Sources, Source{Kind: "credit_note", ID: n.ID, Version: n.Version, Detail: n.Kind + " " + n.Amount.String()})
			}
		}
		if credited.Sign() != 0 {
			totalCredits = add(totalCredits, credited)
			for i, part := range split(credited, ids, weights) {
				batches[ids[i]].credits = add(batches[ids[i]].credits, part)
			}
		}
	}
	sort.Strings(order)
	netR, _ := b.of(totalNet)
	creditsR, _ := b.of(totalCredits)
	netW, creditW := make([]kernel.Decimal, len(order)), make([]kernel.Decimal, len(order))
	for i, id := range order {
		netW[i], creditW[i] = batches[id].net.Decimal(), batches[id].credits.Decimal()
	}
	netParts, creditParts := split(netR, order, netW), split(creditsR, order, creditW)
	result := make([]Batch, len(order))
	for i, id := range order {
		result[i] = Batch{ShipmentID: id, StatementIDs: batches[id].statements, NetSales: netParts[i], Credits: creditParts[i],
			CostOfSales: zero(b.result), DirectCharges: zero(b.result)}
	}

	// Cost of sales: each order line's settled purchase cost over the batches by the quantity they sold and the
	// quantity bought but not sold yet.
	lines := make([]string, 0, len(p.byOrder))
	for l := range p.byOrder {
		lines = append(lines, l)
	}
	sort.Strings(lines)
	for _, line := range lines {
		lc := p.byOrder[line]
		sold := soldBy[line]
		ids, weights := []string{}, []kernel.Decimal{}
		for _, id := range order {
			if q, ok := batches[id].sold[line]; ok && q.Sign() > 0 {
				ids, weights = append(ids, id), append(weights, q)
			}
		}
		unsold := lc.purchased.Sub(sold)
		ids, weights = append(ids, "~unsold"), append(weights, unsold)
		if sold.Sign() == 0 {
			out.UnsoldPurchaseCost = add(out.UnsoldPurchaseCost, lc.cost)
			continue
		}
		parts := split(lc.cost, ids, weights)
		for i, id := range ids {
			if id == "~unsold" {
				out.UnsoldPurchaseCost = add(out.UnsoldPurchaseCost, parts[i])
				continue
			}
			j := slices.Index(order, id)
			result[j].CostOfSales = add(result[j].CostOfSales, parts[i])
		}
		if unsold.Sign() < 0 {
			out.Gaps = append(out.Gaps, fmt.Sprintf("order line %s: %s sold, only %s with a confirmed purchase cost", line, sold, lc.purchased))
		}
	}
	for _, line := range sortedKeys(soldBy) {
		if _, ok := p.byOrder[line]; !ok {
			out.Gaps = append(out.Gaps, fmt.Sprintf("order line %s: %s sold without a confirmed purchase cost", line, soldBy[line]))
		}
	}
	out.Sources = append(out.Sources, p.sources...)
	out.Gaps = append(out.Gaps, p.gaps...)

	// Confirmed direct charges by batch, or the order's.
	for _, s := range shares {
		if s.charge.Status != charge.StatusConfirmed {
			continue
		}
		out.DirectCharges = add(out.DirectCharges, s.amount)
		out.Sources = append(out.Sources, chargeSource(s.charge, "confirmed "+s.amount.String()+" to "+s.target.Kind+" "+targetID(s.target)))
		if s.target.Kind == charge.TargetShipment {
			if j := slices.Index(order, s.target.ShipmentID); j >= 0 {
				result[j].DirectCharges = add(result[j].DirectCharges, s.amount)
				continue
			}
		}
		out.OrderLevelCharges = add(out.OrderLevelCharges, s.amount)
	}
	out.Gaps = append(out.Gaps, chargeGaps...)
	for i := range result {
		bt := &result[i]
		bt.Result = sub(sub(sub(bt.NetSales, bt.Credits), bt.CostOfSales), bt.DirectCharges)
		out.PurchaseCost = add(out.PurchaseCost, bt.CostOfSales)
	}
	out.NetSales, out.Credits, out.PurchaseConfirmed, out.ChargesConfirmed = netR, creditsR, out.PurchaseCost, out.DirectCharges
	out.Result = sub(sub(sub(netR, creditsR), out.PurchaseCost), out.DirectCharges)
	out.Margin = margin(out.Result, sub(netR, creditsR))
	out.Batches = result
	return out
}

func targetID(t charge.Target) string {
	if t.Kind == charge.TargetShipment {
		return t.ShipmentID
	}
	return t.OrderID
}

func sortedKeys(m map[string]kernel.Decimal) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fx is the business settlement FX difference: the standing cross-currency receipts of the order whose analysis
// is in the result currency (C8 recorded each against the order's analysis rate).
func (in Input) fx(b basis) FX {
	out := FX{Difference: zero(b.result)}
	for i := range in.Applications {
		x := &in.Applications[i]
		if !x.Stands() || x.CreditNoteID != "" {
			continue
		}
		c := x.Conversion
		if c == nil {
			if x.Amount.Currency() == b.order && b.order != b.result {
				out.Gaps = append(out.Gaps, "application "+x.ID+": a receipt in "+b.order.Code()+"; a later conversion of that fund is not attributed to the order")
			}
			continue
		}
		a := c.Analysis
		if a == nil || a.Difference == nil || a.Currency != b.result {
			out.Gaps = append(out.Gaps, "application "+x.ID+": no business settlement FX difference in "+b.result.Code())
			continue
		}
		out.Difference = add(out.Difference, *a.Difference)
		out.Items = append(out.Items, FXItem{ApplicationID: x.ID, TargetKind: x.TargetKind, StatementID: x.StatementID, StageID: x.StageID,
			Source: c.Source, Target: x.Amount, Difference: *a.Difference})
	}
	return out
}

// cash is the net cash result: receipts applied to the order's stages and receivables (what the fund gave, in
// the result currency: its own amount when it is in it, at the analysis rate when it is in the order's) less
// payments applied to the payables of its purchase statements, each statement's payments shared by the order's
// part of the statement (largest remainder).
func (in Input) cash(b basis) Cash {
	out := Cash{Receipts: zero(b.result), Payments: zero(b.result)}
	for i := range in.Applications {
		x := &in.Applications[i]
		if !x.Stands() || x.FundRecordID == "" {
			continue
		}
		m := x.Amount
		if x.Conversion != nil {
			m = x.Conversion.Source
		}
		r, ok := b.of(m)
		if !ok {
			out.Gaps = append(out.Gaps, "receipt "+x.ID+" in "+m.Currency().Code()+" does not convert into "+b.result.Code())
			continue
		}
		if m.Currency() != b.result {
			out.Gaps = append(out.Gaps, "receipt "+x.ID+" in "+m.Currency().Code()+" is valued at the analysis rate")
		}
		out.Receipts = add(out.Receipts, r)
		out.Sources = append(out.Sources, Source{Kind: "application", ID: x.ID, Version: x.Version, Detail: "receipt " + m.String() +
			" from fund " + x.FundRecordID})
	}
	for _, st := range in.Purchases {
		if !standing(st) {
			continue
		}
		paid := zero(st.Currency())
		for _, x := range in.PayableApplications[st.ID] {
			if !x.Stands() || x.FundRecordID == "" || x.Amount.Currency() != st.Currency() {
				continue
			}
			paid = add(paid, x.Amount)
		}
		if paid.IsZero() {
			continue
		}
		mine, others := zero(st.Currency()), zero(st.Currency())
		for _, l := range st.Lines {
			if l.OrderID == in.Order.ID {
				mine = add(mine, l.Amount)
			} else {
				others = add(others, l.Amount)
			}
		}
		part := split(paid, []string{"order", "others"}, []kernel.Decimal{mine.Decimal(), others.Decimal()})[0]
		r, ok := b.of(part)
		if !ok {
			out.Gaps = append(out.Gaps, "payment of purchase statement "+st.ID+" in "+part.Currency().Code()+" does not convert into "+b.result.Code())
			continue
		}
		out.Payments = add(out.Payments, r)
		out.Sources = append(out.Sources, Source{Kind: "payable", ID: st.ID, Version: st.Version, Detail: "the order's part of payments " + part.String()})
	}
	out.Gaps = append(out.Gaps, "direct charges are not paid through STL: their payments are not counted")
	out.Net = sub(out.Receipts, out.Payments)
	return out
}

// conditions are the close conditions as they stand (STL-INV-011), in their order.
func (in Input) conditions(b basis, p purchases) []Dimension {
	dims := []Dimension{in.fulfilment(), in.receivables(), in.payables(), in.purchaseCosts(p), in.directCharges(b)}
	return append(dims, Dimension{Name: DimClaims, Status: NotEnabled,
		Items: []string{"clm: not_enabled — CLM is not enabled in S5; no open return or claim can be checked"}})
}

func dim(name string, items []string, met string) Dimension {
	if len(items) > 0 {
		return Dimension{Name: name, Status: Unmet, Items: items}
	}
	return Dimension{Name: name, Status: Met, Items: []string{met}}
}

func (in Input) fulfilment() Dimension {
	o := in.Order
	if o.Status == OrderClosed {
		switch o.Ended {
		case EndClose:
			return Dimension{Name: DimFulfilment, Status: Met, Items: []string{"fulfilment complete (TRD order.close)"}}
		case EndCloseRemainder:
			return Dimension{Name: DimFulfilment, Status: Met, Items: []string{"short-closed with approval (TRD order.close_remainder)"}}
		}
	}
	end := ""
	if o.Ended != "" {
		end = " by " + o.Ended
	}
	return Dimension{Name: DimFulfilment, Status: Unmet, Items: []string{"TRD: the order is " + o.Status + end + ", not complete or short-closed"}}
}

func (in Input) receivables() Dimension {
	var items []string
	settled := map[string]bool{}
	for _, st := range in.Sales {
		if !standing(st) {
			continue
		}
		for _, l := range st.Lines {
			settled[l.Source.ID+"/"+l.Source.LineID] = true
		}
		if open := st.Open(in.applied(st)); open.Sign() > 0 {
			items = append(items, "receivable "+st.ID+" open "+open.String())
		}
	}
	if !in.ShipmentsShown {
		items = append(items, "FUL shows the caller no shipment of the order")
	}
	for _, l := range in.Shipments {
		if l.Status == ShipmentConfirmed && !settled[l.ShipmentID+"/"+l.LineID] {
			items = append(items, "shipment "+l.ShipmentID+" line "+l.LineID+" is not settled")
		}
	}
	prepaid := map[string]kernel.Money{}
	var stages []string
	for i := range in.Applications {
		x := &in.Applications[i]
		if x.Stands() && x.TargetKind == fa.TargetOrderStage {
			if m, ok := prepaid[x.StageID]; ok {
				prepaid[x.StageID] = add(m, x.Amount)
			} else {
				prepaid[x.StageID], stages = x.Amount, append(stages, x.StageID)
			}
		}
	}
	sort.Strings(stages)
	for _, s := range stages {
		items = append(items, prepaid[s].String()+" prepaid on stage "+s+" is applied to no receivable")
	}
	return dim(DimReceivables, items, "every shipment settled and every receivable paid")
}

func (in Input) payables() Dimension {
	var items []string
	for _, st := range in.Purchases {
		if !standing(st) {
			continue
		}
		if open := st.Open(in.applied(st)); open.Sign() > 0 {
			items = append(items, "payable "+st.ID+" open "+open.String())
		}
	}
	return dim(DimPayables, items, "every payable of the order's purchases paid")
}

func (in Input) purchaseCosts(p purchases) Dimension {
	var items []string
	if !in.CommitmentsShown {
		items = append(items, "PUR shows the caller no purchase commitment of the order")
	}
	settled := map[string]bool{}
	for _, st := range in.Purchases {
		if !standing(st) {
			continue
		}
		for _, l := range st.Lines {
			settled[l.Source.ID+"/"+l.Source.LineID] = true
		}
	}
	for _, c := range in.Commitments {
		for _, d := range c.Deliveries {
			if !settled[d.DeliveryID+"/"+d.LineID] {
				items = append(items, "delivery "+d.DeliveryID+" of commitment "+c.CommitmentID+" line "+d.LineID+" is verified, not settled")
			}
		}
	}
	items = append(items, p.gaps...)
	return dim(DimPurchaseCosts, items, "every verified delivery settled on a purchase statement")
}

func (in Input) directCharges(b basis) Dimension {
	var items []string
	for _, c := range in.Charges {
		if c.Status != charge.StatusConfirmed {
			items = append(items, "charge "+c.ID+" is "+c.Status)
		}
		if _, ok := c.SharesOf(c.Amount); !ok {
			items = append(items, "charge "+c.ID+" is not allocated among its orders")
		} else if _, ok := b.charged(c, c.Amount); !ok {
			items = append(items, "charge "+c.ID+" does not convert into "+b.result.Code())
		}
	}
	return dim(DimDirectCharges, items, "every direct charge confirmed and allocated")
}
