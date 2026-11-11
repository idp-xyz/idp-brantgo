package draft

import (
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/allocation"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Line is one line of a draft as it claims its order line: the allocated quantity, the approved
// overbuy (0 when none) and the condition line it buys under.
type Line struct {
	OrderLineID string
	Quantity    kernel.Decimal
	Overbuy     kernel.Decimal
	Source      Source
}

// Use is an upstream reference a preparation adopted, with the path of the request it came from
// and the content hash its lookup returned: the guards re-check each inside the transaction.
type Use struct {
	Path        string
	ContentHash string
}

// PartyUse is a supplier as the adopted supply conditions bind it.
type PartyUse struct {
	PartyID    string
	RowVersion int64
	Path       string
}

// Prepared is a decided preparation before its guards: the document without the snapshot's
// adoptability, the draft's lines in request order, the upstream references it adopted (for the
// guards, in context code order), the DemandAllocation roots it changed (in lock order) and the
// end of its supply's validity.
type Prepared struct {
	Doc              Document
	Lines            []Line
	Specs            map[Spec]string // path
	Snapshot         SnapshotRef
	Configs          map[ConfigKey]Use
	Parties          []PartyUse
	Sources          map[Ref]Use
	Allocations      []*allocation.Allocation
	SupplyValidUntil time.Time
	request          Request
}

// line is one request line as the rules judged it.
type picked struct {
	path     string // /args/lines/<i>
	req      RequestLine
	order    OrderLine
	supply   Supply
	cond     Condition
	purchase kernel.Decimal
	delivery DocDelivery
}

// Prepare decides purchase.draft.prepare (PUR-INV-001 to 007, Python t04_purchase prepare and
// _calculate) on facts in. It refuses, in this order: an order not visible (NOT_FOUND) or not
// serving new work (TRD's verdict: ORDER_NOT_ACTIVE, ORDER_APPROVAL_REQUIRED, ORDER_APPROVAL_REVOKED,
// REFERENCE_STALE); a line the order does not have (ORDER_LINE_NOT_FOUND); an allocation beyond the
// line's remaining demand (PURCHASE_ALLOCATION_EXCEEDED, PUR-INV-002); an order whose delivery is not
// a fixed date (DELIVERY_NOT_COMPARABLE); then line by line the supply condition (unknown:
// REFERENCE_INVALID; SUP's verdict SUPPLY_CONDITION_NOT_CURRENT or SUPPLY_CONDITION_EXPIRED), its
// supplier (not a visible supplier: REFERENCE_INVALID; not confirmed at the bound row version:
// REFERENCE_STALE), an offer for another RFQ (SUPPLY_SCOPE_MISMATCH), the condition line (unknown:
// REFERENCE_INVALID; another specification revision: SPECIFICATION_MISMATCH; an RFQ line the offer
// does not answer: SUPPLY_SCOPE_MISMATCH), the specification and every configuration in force
// (CAT's and POL's verdicts), one unit throughout (DEPENDENCY_NOT_SUPPORTED: no conversion in this
// stage), quantities exact at the unit's precision and at most 10^6 (INVALID_KNOWN_VALUE) and the
// delivery (PUR-INV-006); then one supplier and one set of terms (SUPPLIER_MISMATCH,
// PURCHASE_TERMS_INCONSISTENT, PUR-INV-003); every overbuy marked with a reason and approved
// (OVERBUY_NOT_APPROVED); MOQ and order multiple on the purchased quantities (MOQ_NOT_MET,
// ORDER_MULTIPLE_INVALID, PUR-INV-005); and a cost snapshot to compare with (COST_SNAPSHOT_MISSING,
// REFERENCE_INVALID). It never chooses a source and never raises a quantity.
func Prepare(req Request, in Inputs) (*Prepared, error) {
	o := in.Order
	if o == nil {
		return nil, fail(kernel.CodeNotFound, "/args/order_id", "")
	}
	if err := o.Check("/args/order_id"); err != nil {
		return nil, err
	}
	picks := make([]picked, len(req.Lines))
	for i, l := range req.Lines {
		p := index("/args/lines", i)
		ol, ok := o.Lines[l.OrderLineID]
		if !ok {
			return nil, fail(CodeOrderLineNotFound, p+"/order_line_id", "The order has no such line")
		}
		picks[i] = picked{path: p, req: l, order: ol, purchase: l.Quantity}
		if l.Overbuy != nil {
			picks[i].purchase = l.Quantity.Add(l.Overbuy.Quantity)
		}
	}
	roots := map[allocation.Key]*allocation.Allocation{}
	for _, pk := range picks {
		key := allocation.Key{OrderID: o.OrderID, LineID: pk.order.LineID}
		unit := allocation.Unit{ID: pk.order.Unit.ID, Revision: pk.order.Unit.Revision}
		a := in.Allocations[key]
		if a == nil {
			a = allocation.Open(key, o.OrderRevision, unit, pk.order.Quantity)
		}
		if err := a.Take(req.DraftID, pk.req.Quantity, o.OrderRevision, unit, pk.order.Quantity, pk.path+"/quantity"); err != nil {
			return nil, err
		}
		roots[key] = a
	}
	if o.Delivery == nil || o.Delivery.Kind != DeliveryFixed {
		return nil, fail(CodeDeliveryNotComparable, "/args/order_id", "Only a fixed delivery date of the order is compared in this stage")
	}
	out := &Prepared{Specs: map[Spec]string{}, Configs: map[ConfigKey]Use{}, Sources: map[Ref]Use{}, request: req}
	judged := map[Ref]bool{}
	for i := range picks {
		if err := in.judge(&picks[i], judged, out); err != nil {
			return nil, err
		}
	}
	first := picks[0]
	for _, pk := range picks[1:] {
		switch {
		case pk.supply.SupplierID != first.supply.SupplierID:
			return nil, fail(CodeSupplierMismatch, pk.path+"/source", "One draft buys from one supplier; prepare another draft for this one")
		case *pk.cond.Currency != *first.cond.Currency || *pk.cond.PaymentTerms != *first.cond.PaymentTerms ||
			*pk.cond.TradeTerms != *first.cond.TradeTerms:
			return nil, fail(CodePurchaseTermsInconsistent, pk.path+"/source", "Currency, payment terms and trade terms are the same throughout a draft")
		}
	}
	for _, pk := range picks {
		if ob := pk.req.Overbuy; ob != nil {
			if strings.TrimSpace(ob.Reason) == "" {
				return nil, fail(CodeOverbuyNotApproved, pk.path+"/overbuy/reason", "An overbuy is marked with its reason")
			}
			if !in.OverbuyApproved {
				return nil, fail(CodeOverbuyNotApproved, pk.path+"/overbuy", "The preparer may not approve an overbuy")
			}
		}
	}
	if err := checkMOQ(picks); err != nil {
		return nil, err
	}
	if o.Snapshot == nil {
		return nil, fail(CodeCostSnapshotMissing, "/args/order_id", "The order's quote binds no cost snapshot")
	}
	if in.Costing == nil || in.Costing.Ref != *o.Snapshot {
		return nil, fail(kernel.CodeReferenceInvalid, "/args/order_id", "The order's cost snapshot is not readable")
	}
	if err := in.document(req, picks, out); err != nil {
		return nil, err
	}
	out.Snapshot = *o.Snapshot
	for _, pk := range picks {
		out.Lines = append(out.Lines, Line{OrderLineID: pk.order.LineID, Quantity: pk.req.Quantity, Overbuy: pk.purchase.Sub(pk.req.Quantity),
			Source: pk.req.Source})
	}
	out.Allocations = slices.Collect(maps.Values(roots))
	allocation.Sort(out.Allocations)
	return out, nil
}

// judge decides one line's supply condition, references, units, quantities and delivery.
func (in Inputs) judge(pk *picked, judged map[Ref]bool, out *Prepared) error {
	p := pk.path + "/source"
	o := in.Order
	s, ok := in.Supply[pk.req.Source.Ref]
	if !ok {
		return fail(kernel.CodeReferenceInvalid, p, "No sealed supply condition revision the caller sees")
	}
	if !judged[s.Ref] {
		if err := s.Check(p); err != nil {
			return err
		}
		party, ok := in.Parties[s.SupplierID]
		switch {
		case !ok || !party.Supplier:
			return fail(kernel.CodeReferenceInvalid, p, "The supplier is not a supplier the caller sees")
		case party.Status != PartyConfirmed || party.RowVersion != s.SupplierRowVersion:
			return fail(CodeReferenceStale, p, "The supplier is not confirmed at the row version the supply condition binds")
		}
		if s.Ref.Kind == KindSupplierOffer && (s.Rfq == nil || o.Rfq == nil || *s.Rfq != *o.Rfq) {
			return fail(CodeSupplyScopeMismatch, p, "The offer does not answer the order's source RFQ")
		}
		judged[s.Ref] = true
		out.Sources[s.Ref] = Use{Path: p, ContentHash: s.ContentHash}
		if !slices.ContainsFunc(out.Parties, func(u PartyUse) bool { return u.PartyID == s.SupplierID }) {
			out.Parties = append(out.Parties, PartyUse{PartyID: s.SupplierID, RowVersion: s.SupplierRowVersion, Path: p})
		}
	}
	c, ok := s.Lines[pk.req.Source.ConditionLineID]
	if !ok {
		return fail(kernel.CodeReferenceInvalid, p+"/condition_line_id", "The supply condition has no such line")
	}
	ol := pk.order
	if c.SkuID != ol.SkuID || c.SpecRevision != ol.SpecRevision {
		return fail(CodeSpecificationMismatch, p, "The condition line's specification revision is not the order line's")
	}
	if s.RfqLineIDs != nil && !slices.Contains(s.RfqLineIDs, ol.RfqLineID) {
		return fail(CodeSupplyScopeMismatch, p, "The offer does not answer the order line's RFQ line")
	}
	spec := Spec{SkuID: ol.SkuID, Revision: ol.SpecRevision}
	check, ok := in.Specs[spec]
	if !ok {
		return fail(kernel.CodeReferenceInvalid, p, "CAT gave no standing of the specification revision")
	}
	if err := check(p); err != nil {
		return err
	}
	if _, seen := out.Specs[spec]; !seen {
		out.Specs[spec] = p
	}
	refs := []*ConfigKey{&ol.Unit, c.PurchaseUnit, c.PriceUnit, c.TierUnit, c.Currency, c.TaxProfile, c.TaxApplication, c.TradeTerms, c.PaymentTerms}
	if c.MOQ != nil {
		refs = append(refs, c.MOQ.Unit)
	}
	for _, r := range refs {
		if r == nil {
			return fail(kernel.CodeReferenceInvalid, p, "The verified condition line misses a configuration")
		}
		cfg, ok := in.Configs[*r]
		if !ok {
			return fail(kernel.CodeReferenceInvalid, p, "POL gave no standing of a configuration the line uses")
		}
		if err := cfg.Check(p); err != nil {
			return err
		}
		if _, seen := out.Configs[*r]; !seen {
			out.Configs[*r] = Use{Path: p, ContentHash: cfg.ContentHash}
		}
	}
	for _, r := range []*ConfigKey{c.PurchaseUnit, c.PriceUnit, c.TierUnit, c.MOQ.unit()} {
		if r != nil && *r != ol.Unit {
			return fail(CodeDependencyNotSupported, p, "Purchase, price, tier and MOQ units are the order line's unit; no conversion in this stage")
		}
	}
	unit := in.Configs[ol.Unit].Unit
	if unit == nil {
		return fail(kernel.CodeReferenceInvalid, p, "The order line's unit is not a unit")
	}
	for _, q := range []struct {
		v    kernel.Decimal
		path string
	}{{pk.req.Quantity, pk.path + "/quantity"}, {pk.purchase.Sub(pk.req.Quantity), pk.path + "/overbuy/quantity"}} {
		if !exactAt(q.v, unit.Precision) {
			return fail(kernel.CodeInvalidKnownValue, q.path, fmt.Sprintf("%s %s has more than %d fractional digits", q.v, unit.Code, unit.Precision))
		}
	}
	if pk.purchase.Cmp(maxQuantity) > 0 {
		return fail(kernel.CodeInvalidKnownValue, pk.path+"/quantity", "A purchase quantity is at most 1000000")
	}
	d, err := compareDelivery(c.Delivery, *o.Delivery, in.Now, p)
	if err != nil {
		return err
	}
	pk.supply, pk.cond, pk.delivery = s, c, d
	return nil
}

func (m *MOQ) unit() *ConfigKey {
	if m == nil {
		return nil
	}
	return m.Unit
}

type moqKey struct {
	ref   Ref
	scope string
	id    string
}

type moqGroup struct {
	key      moqKey
	rule     MOQ
	quantity kernel.Decimal
	first    string // path of the first line counted
	sig      string
}

func (m MOQ) signature() string {
	members := slices.Clone(m.Members)
	slices.Sort(members)
	return fmt.Sprintf("%s|%v|%s|%s|%q", m.Quantity, m.Unit, m.Scope, m.GroupID, members)
}

// checkMOQ applies the MOQ of each condition line to the quantities the draft buys under the same
// source (CST Purchase, Python purchase_groups): per SKU, or over an explicit group of RFQ lines the
// supplier lets be ordered together; never across suppliers or drafts, and never raised.
func checkMOQ(picks []picked) error {
	groups := map[moqKey]*moqGroup{}
	var order []*moqGroup
	for _, pk := range picks {
		m := pk.cond.MOQ
		if m == nil {
			continue
		}
		mk := moqKey{ref: pk.supply.Ref, scope: m.Scope, id: pk.cond.SkuID}
		switch m.Scope {
		case MOQScopeSKU:
		case MOQScopeExplicitGroup:
			if !slices.Contains(m.Members, pk.order.RfqLineID) {
				return fail(kernel.CodeReferenceInvalid, pk.path+"/source", "The order line is not a member of the condition's MOQ group")
			}
			mk.id = m.GroupID
		default:
			return fail(kernel.CodeReferenceInvalid, pk.path+"/source", "Unknown MOQ scope")
		}
		g := groups[mk]
		if g == nil {
			g = &moqGroup{key: mk, rule: *m, first: pk.path + "/quantity", sig: m.signature()}
			groups[mk] = g
			order = append(order, g)
		} else if g.sig != m.signature() {
			return fail(kernel.CodeReferenceInvalid, pk.path+"/source", "Lines of one MOQ group state different rules")
		}
		g.quantity = g.quantity.Add(pk.purchase)
	}
	for _, g := range order {
		min, err := decimal(g.rule.Quantity, kernel.QuantityFormat, g.first)
		if err != nil {
			return err
		}
		if g.quantity.Cmp(min) < 0 {
			return fail(CodeMOQNotMet, g.first, fmt.Sprintf("The draft buys %s, below the MOQ %s; quantities are never raised automatically", g.quantity, min))
		}
	}
	return nil
}

type priceGroup struct {
	source  Source
	cond    Condition
	supply  Supply
	members []*picked
	qty     kernel.Decimal
}

func minor(m kernel.Money) string { return m.MinorString() }

func text(d kernel.Decimal) string {
	s, _ := d.Exact()
	return s
}

// document prices the draft and writes its body (T02 §13.3, Python _calculate): one price group per
// condition line, priced once on the aggregated quantity and split over its lines in proportion to
// their purchased quantities (largest remainder, order line IDs ascending on ties); net, tax and
// gross per line; each line's cost split between its allocated quantity and its overbuy, the
// overbuy's share not counting as the cost of goods sold; and the comparison with the cost snapshot.
func (in Inputs) document(req Request, picks []picked, out *Prepared) error {
	o := in.Order
	first := picks[0]
	cur := in.Configs[*first.cond.Currency].Currency
	if cur == nil {
		return fail(kernel.CodeReferenceInvalid, first.path+"/source", "The currency is not a currency")
	}
	currency, err := kernel.NewCurrency(cur.Code, cur.MinorUnits)
	if err != nil {
		return fail(kernel.CodeReferenceInvalid, first.path+"/source", "The currency is not a currency")
	}
	byKey := map[Source]*priceGroup{}
	for i := range picks {
		pk := &picks[i]
		g := byKey[pk.req.Source]
		if g == nil {
			g = &priceGroup{source: pk.req.Source, cond: pk.cond, supply: pk.supply}
			byKey[pk.req.Source] = g
		}
		g.members = append(g.members, pk)
		g.qty = g.qty.Add(pk.purchase)
	}
	groups := slices.SortedFunc(maps.Values(byKey), func(a, b *priceGroup) int { return compareSource(a.source, b.source) })
	type lineAmounts struct {
		group          string
		net, tax, cost kernel.Money
		recovery       string
	}
	amounts := map[string]lineAmounts{}
	doc := &out.Doc
	for n, g := range groups {
		id := fmt.Sprintf("group.%d", n+1)
		slices.SortFunc(g.members, func(a, b *picked) int { return strings.Compare(a.order.LineID, b.order.LineID) })
		at := g.members[0].path + "/quantity"
		if g.cond.OrderMultiple != "" {
			multiple, err := decimal(g.cond.OrderMultiple, kernel.QuantityFormat, at)
			if err != nil {
				return err
			}
			if times, err := g.qty.Quo(multiple); err != nil || !times.IsInteger() {
				return fail(CodeOrderMultipleInvalid, at, fmt.Sprintf("The draft buys %s, not a multiple of %s; quantities are never raised automatically", g.qty, multiple))
			}
		}
		s, err := parseSchedule(g.cond, g.members[0].path+"/source")
		if err != nil {
			return err
		}
		raw, applied, err := s.amount(g.qty, at)
		if err != nil {
			return err
		}
		profile := in.Configs[*g.cond.TaxProfile].TaxProfile
		if profile == nil {
			return fail(kernel.CodeReferenceInvalid, g.members[0].path+"/source", "The tax profile is not a tax profile")
		}
		split, _, err := taxSplit(currency, raw, *profile, g.members[0].path+"/source")
		if err != nil {
			return err
		}
		shares := make([]kernel.Share, len(g.members))
		lineIDs := make([]string, len(g.members))
		for i, m := range g.members {
			shares[i] = kernel.Share{ID: m.order.LineID, Weight: m.purchase}
			lineIDs[i] = m.order.LineID
		}
		nets, err := kernel.AllocateLargestRemainder(split.Net.Minor(), shares)
		if err != nil {
			return fail(CodeCalculationLimitExceeded, at, "The group amount cannot be split")
		}
		taxes, err := kernel.AllocateLargestRemainder(split.Tax.Minor(), shares)
		if err != nil {
			return fail(CodeCalculationLimitExceeded, at, "The group tax cannot be split")
		}
		for i, m := range g.members {
			net, _ := kernel.MoneyFromMinor(currency, nets[i])
			tax, _ := kernel.MoneyFromMinor(currency, taxes[i])
			cost := net
			if profile.Recovery == Nonrecoverable {
				cost, _ = net.Add(tax)
			}
			amounts[m.order.LineID] = lineAmounts{group: id, net: net, tax: tax, cost: cost, recovery: profile.Recovery}
		}
		tiers := make([]DocTier, len(applied))
		for i, t := range applied {
			tiers[i] = DocTier{LowerInclusive: t.Lower, UpperExclusive: t.Upper, UnitPrice: t.UnitPrice, Quantity: text(t.Quantity)}
		}
		doc.PriceGroups = append(doc.PriceGroups, DocGroup{GroupID: id, Source: docSource(g.source), OrderLineIDs: lineIDs,
			Quantity: text(g.qty), UnitRef: docRef(*g.cond.PurchaseUnit), PricingMode: g.cond.PricingMode,
			PriceBasisQuantity: g.cond.PriceBasisQuantity, AppliedTiers: tiers, TaxProfileRef: docRef(*g.cond.TaxProfile),
			TaxPriceBasis: profile.PriceBasis, TaxRate: profile.Rate, TaxRecovery: profile.Recovery, RawAmount: text(raw),
			NetMinor: minor(split.Net), TaxMinor: minor(split.Tax), GrossMinor: minor(split.Gross)})
	}

	totals := struct{ net, tax, gross, cost *big.Int }{new(big.Int), new(big.Int), new(big.Int), new(big.Int)}
	sorted := slices.Clone(picks)
	slices.SortFunc(sorted, func(a, b picked) int { return strings.Compare(a.order.LineID, b.order.LineID) })
	for _, pk := range sorted {
		a := amounts[pk.order.LineID]
		gross, _ := a.net.Add(a.tax)
		overbuy := pk.purchase.Sub(pk.req.Quantity)
		sold := a.cost
		var extra kernel.Money
		if overbuy.Sign() <= 0 {
			extra, _ = kernel.NewMoney(currency, 0)
		} else {
			parts, err := kernel.AllocateLargestRemainder(a.cost.Minor(), []kernel.Share{{ID: "allocated", Weight: pk.req.Quantity},
				{ID: "overbuy", Weight: overbuy}})
			if err != nil {
				return fail(CodeCalculationLimitExceeded, pk.path+"/overbuy", "The line's cost cannot be split")
			}
			sold, _ = kernel.MoneyFromMinor(currency, parts[0])
			extra, _ = kernel.MoneyFromMinor(currency, parts[1])
		}
		totals.net.Add(totals.net, a.net.Minor())
		totals.tax.Add(totals.tax, a.tax.Minor())
		totals.gross.Add(totals.gross, gross.Minor())
		totals.cost.Add(totals.cost, a.cost.Minor())
		line := DocLine{OrderLineID: pk.order.LineID, RfqLineID: pk.order.RfqLineID,
			Sku: DocSku{SkuID: pk.order.SkuID, SpecRevision: pk.order.SpecRevision}, UnitRef: docRef(pk.order.Unit),
			OrderQuantity: text(pk.order.Quantity), AllocatedQuantity: text(pk.req.Quantity), OverbuyQuantity: text(overbuy),
			PurchaseQuantity: text(pk.purchase), Source: docSource(pk.req.Source), SupplierItemCode: pk.cond.SupplierItemCode,
			PriceGroupID: a.group, NetMinor: minor(a.net), TaxMinor: minor(a.tax), GrossMinor: minor(gross), CostMinor: minor(a.cost),
			SoldCostMinor: minor(sold), OverbuyCostMinor: minor(extra), TaxProfileRef: docRef(*pk.cond.TaxProfile),
			TaxApplicationRef: docRef(*pk.cond.TaxApplication), OrderMultiple: pk.cond.OrderMultiple, Delivery: pk.delivery}
		if ob := pk.req.Overbuy; ob != nil {
			line.Overbuy = &DocOverbuy{Quantity: text(ob.Quantity), Reason: ob.Reason,
				ApprovedBy: DocActor{Kind: string(in.Preparer.Kind), ID: in.Preparer.ID}, AuthorityEpoch: in.Epoch}
		}
		if m := pk.cond.MOQ; m != nil {
			line.MOQ = &DocMOQ{Quantity: m.Quantity, UnitRef: docRef(*m.Unit), Scope: m.Scope, GroupID: m.GroupID,
				MemberLineIDs: slices.Clone(m.Members)}
		}
		line.CostingReference = in.compare(pk, sold, currency)
		doc.Lines = append(doc.Lines, line)
	}
	for i := range doc.Lines {
		if m := doc.Lines[i].MOQ; m != nil {
			m.Purchased = purchasedUnder(picks, doc.Lines[i])
		}
	}

	var until time.Time
	var sources []DocSource
	for _, ref := range slices.SortedFunc(maps.Keys(out.Sources), compareRef) {
		s := in.Supply[ref]
		ds := DocSource{Kind: ref.Kind, ID: ref.ID, Revision: ref.Revision, ContentHash: s.ContentHash,
			VerifiedBy: DocActor{Kind: string(s.VerifiedBy.Kind), ID: s.VerifiedBy.ID}, VerifiedAt: kernel.FormatTimestamp(s.VerifiedAt),
			ValidFrom: kernel.FormatTimestamp(s.ValidFrom), ValidUntil: kernel.FormatTimestamp(s.ValidUntil)}
		ends := []time.Time{s.ValidUntil}
		if og := s.Origin; og != nil {
			ds.Origin = &DocOrigin{Kind: og.Ref.Kind, ID: og.Ref.ID, Revision: og.Ref.Revision, ValidityMode: og.ValidityMode,
				ValidFrom: kernel.FormatTimestamp(og.ValidFrom), ValidUntil: kernel.FormatTimestamp(og.ValidUntil)}
			if og.ValidityMode == ValidityDependent {
				ends = append(ends, og.ValidUntil)
			}
		}
		for _, e := range ends {
			if until.IsZero() || e.Before(until) {
				until = e
			}
		}
		sources = append(sources, ds)
	}
	out.SupplyValidUntil = kernel.Instant(until)

	sup := first.supply
	*doc = Document{Contract: DocumentContract, Scope: DocScope{Tenant: string(in.Scope.Tenant), Company: string(in.Scope.Company)},
		DraftID: req.DraftID, CommitmentState: CommitmentNotCommitted,
		Order: DocOrder{OrderID: o.OrderID, OrderRevision: o.OrderRevision, OrderHash: o.OrderHash, QuoteID: o.QuoteID,
			QuoteRevision: o.QuoteRevision, QuoteHash: o.QuoteHash, OrderReviewID: o.ReviewID, CustomerID: o.CustomerID,
			Rfq:              DocRfq{RfqID: o.Rfq.RfqID, RowVersion: o.Rfq.RowVersion},
			RequiredDelivery: DocRequired(*o.Delivery)},
		Supplier:    DocParty{PartyID: sup.SupplierID, RowVersion: sup.SupplierRowVersion},
		CurrencyRef: docRef(*first.cond.Currency), PaymentTermsRef: docRef(*first.cond.PaymentTerms), TradeTermsRef: docRef(*first.cond.TradeTerms),
		Currency: currency.Code(), MinorUnits: currency.MinorUnits(), Rounding: "HALF_UP", Sources: sources,
		PriceGroups: doc.PriceGroups, Lines: doc.Lines,
		Totals: DocTotals{NetMinor: totals.net.String(), TaxMinor: totals.tax.String(), GrossMinor: totals.gross.String(),
			CostMinor: totals.cost.String()},
		CostingSnapshot:  DocCosting{SnapshotID: o.Snapshot.SnapshotID, ResultHash: o.Snapshot.ResultHash, Currency: in.Costing.Currency},
		SupplyValidUntil: kernel.FormatTimestamp(until), SelectionReason: req.SelectionReason,
		PreparedBy: DocActor{Kind: string(in.Preparer.Kind), ID: in.Preparer.ID}, AuthorityEpoch: in.Epoch,
		CheckedAt: kernel.FormatTimestamp(in.Now)}
	return nil
}

// purchasedUnder is what the draft buys towards the MOQ rule of line l.
func purchasedUnder(picks []picked, l DocLine) string {
	total := zero
	for _, pk := range picks {
		m := pk.cond.MOQ
		if m == nil || pk.req.Source.Ref != (Ref{Kind: l.Source.Kind, ID: l.Source.ID, Revision: l.Source.Revision}) || m.Scope != l.MOQ.Scope {
			continue
		}
		if (m.Scope == MOQScopeExplicitGroup && m.GroupID == l.MOQ.GroupID) || (m.Scope != MOQScopeExplicitGroup && pk.cond.SkuID == l.Sku.SkuID) {
			total = total.Add(pk.purchase)
		}
	}
	return text(total)
}

// compare is a line's comparison with the cost snapshot (R21; Python _reference): the sources the
// snapshot bought the RFQ line under and its purchase cost of the line; the share of that cost the
// allocated quantity stands for (HALF_UP to a minor unit); the same single source or not; and the
// variance of the draft's cost of the allocated quantity against the share, in the same currency
// only.
func (in Inputs) compare(pk picked, sold kernel.Money, currency kernel.Currency) DocCostingRef {
	cl := in.Costing.Lines[pk.order.RfqLineID]
	out := DocCostingRef{Sources: []DocLineSource{}, Currency: in.Costing.Currency}
	for _, s := range cl.Sources {
		out.Sources = append(out.Sources, docSource(s))
	}
	out.SameSource = len(cl.Sources) == 1 && cl.Sources[0] == pk.req.Source
	if cl.PurchaseMinor == nil || pk.order.Quantity.Sign() <= 0 {
		return out
	}
	line := text(*cl.PurchaseMinor)
	shareValue, _ := cl.PurchaseMinor.Mul(pk.req.Quantity).Quo(pk.order.Quantity)
	share := shareValue.RoundHalfUp(0)
	shareText := text(share)
	out.LineCostMinor, out.ShareCostMinor = &line, &shareText
	if in.Costing.Currency == currency.Code() {
		v := text(kernel.DecimalFromBigInt(sold.Minor()).Sub(share))
		out.CostVarianceMinor = &v
	}
	return out
}

// Request returns the request the preparation decided.
func (p *Prepared) Request() Request { return p.request }
