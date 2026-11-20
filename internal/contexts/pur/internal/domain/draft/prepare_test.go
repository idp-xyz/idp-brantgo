package draft

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/allocation"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func decodeDoc(t *testing.T, d *Draft) Document {
	t.Helper()
	doc, err := d.Decode()
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// Python test_active_order_line_forms_reviewable_draft_from_effective_offer (PYT-0556), the
// calculation half (the PostgreSQL half is TestActiveOrderLineFormsReviewableDraftFromEffectiveOffer
// in internal/bootstrap): 120 PCS at 5 USD is 600.00 USD net and gross with zero tax; delivery on
// the required date leaves 0 days of slack; the supply is valid until 2026-10-29; the costing
// reference is the same single source with no variance; the draft is not committed and hashes its
// canonical document.
func TestActiveOrderLineFormsReviewableDraft(t *testing.T) {
	w := fixture()
	p := w.prepare(t)
	d, err := p.Seal("adoptable", "", "", "trade")
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeDoc(t, d)
	l := doc.Lines[0]
	if d.Status != StatusPrepared || d.RowVersion != 1 || d.CommitmentState != CommitmentNotCommitted || doc.CommitmentState != "not_committed" ||
		d.Hash != Hash(d.Document) || d.SupplierID != "supplier.1" || doc.CurrencyRef.ID != "USD" || d.Currency != "USD" {
		t.Fatalf("draft %+v", d)
	}
	if l.OrderQuantity != "120" || l.AllocatedQuantity != "120" || l.PurchaseQuantity != "120" || l.OverbuyQuantity != "0" ||
		l.NetMinor != "60000" || l.TaxMinor != "0" || l.GrossMinor != "60000" || l.CostMinor != "60000" || l.SoldCostMinor != "60000" ||
		l.OverbuyCostMinor != "0" {
		t.Fatalf("line %+v", l)
	}
	if doc.Totals != (DocTotals{NetMinor: "60000", TaxMinor: "0", GrossMinor: "60000", CostMinor: "60000"}) {
		t.Fatalf("totals %+v", doc.Totals)
	}
	if l.Delivery.RequiredDate != "2026-11-15" || l.Delivery.SlackDays != 0 {
		t.Fatalf("delivery %+v", l.Delivery)
	}
	if doc.Sources[0].ID != "offer.1" || doc.Sources[0].VerifiedBy.ID != "reviewer" || doc.SupplyValidUntil != "2026-10-29T00:00:00.000000Z" ||
		!d.SupplyValidUntil.Equal(time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("sources %+v until %s", doc.Sources, doc.SupplyValidUntil)
	}
	if doc.Order.OrderReviewID != "review.order" || doc.Order.OrderHash != "orderhash" || doc.CostingSnapshot.Adoptability != "adoptable" {
		t.Fatalf("order %+v costing %+v", doc.Order, doc.CostingSnapshot)
	}
	ref := l.CostingReference
	if !ref.SameSource || *ref.ShareCostMinor != "60000" || *ref.LineCostMinor != "60000" || *ref.CostVarianceMinor != "0" {
		t.Fatalf("costing reference %+v", ref)
	}
	if d.State(now) != StatusPrepared || d.State(d.SupplyValidUntil) != SupplyExpired {
		t.Fatal("supply_expired is derived from the clock")
	}
	if err = d.Verify(w.in.Scope, "/id"); err != nil {
		t.Fatal(err)
	}
	if p.Allocations[0].Remaining().String() != "0" || len(p.Allocations) != 1 {
		t.Fatalf("allocation %+v", p.Allocations[0])
	}
}

// Python test_hundred_piece_price_basis_matches_per_piece_amount (PYT-0565): 500 per 100 PCS is the
// same 600.00 as 5 per PCS; another source than the costing's is not the same source, with no
// variance.
func TestHundredPiecePriceBasisMatchesPerPieceAmount(t *testing.T) {
	w := fixture()
	src := w.variant("offer.a46.basis", func(c *Condition) {
		c.PriceBasisQuantity, c.Tiers = "100", []Tier{{Lower: "0", UnitPrice: "500"}}
	})
	l := w.with(src).prepare(t).Doc.Lines[0]
	if l.NetMinor != "60000" || l.CostingReference.SameSource || *l.CostingReference.CostVarianceMinor != "0" {
		t.Fatalf("line %+v", l)
	}
}

var tiers = []Tier{{Lower: "0", Upper: "100", UnitPrice: "6"}, {Lower: "100", UnitPrice: "5"}}

// Python test_all_units_tiers_price_the_whole_quantity_at_the_reached_tier (PYT-0566).
func TestAllUnitsTiersPriceTheWholeQuantityAtTheReachedTier(t *testing.T) {
	w := fixture()
	src := w.variant("offer.a46.all_units", func(c *Condition) { c.PricingMode, c.Tiers = ModeAllUnits, tiers })
	p := w.with(src).prepare(t)
	g := p.Doc.PriceGroups[0]
	if len(g.AppliedTiers) != 1 || g.AppliedTiers[0] != (DocTier{LowerInclusive: "100", UnitPrice: "5", Quantity: "120"}) ||
		p.Doc.Lines[0].NetMinor != "60000" {
		t.Fatalf("group %+v", g)
	}
}

// Python test_graduated_tiers_price_each_band_and_expose_cost_variance (PYT-0567): 100 at 6 and 20
// at 5 is 700.00, 100.00 above the costing.
func TestGraduatedTiersPriceEachBandAndExposeCostVariance(t *testing.T) {
	w := fixture()
	src := w.variant("offer.a46.graduated", func(c *Condition) { c.PricingMode, c.Tiers = ModeGraduated, tiers })
	p := w.with(src).prepare(t)
	g := p.Doc.PriceGroups[0]
	want := []DocTier{{LowerInclusive: "0", UpperExclusive: "100", UnitPrice: "6", Quantity: "100"}, {LowerInclusive: "100", UnitPrice: "5", Quantity: "20"}}
	if len(g.AppliedTiers) != 2 || g.AppliedTiers[0] != want[0] || g.AppliedTiers[1] != want[1] || p.Doc.Lines[0].NetMinor != "70000" ||
		*p.Doc.Lines[0].CostingReference.CostVarianceMinor != "10000" {
		t.Fatalf("group %+v line %+v", g, p.Doc.Lines[0])
	}
}

// Python test_moq_and_order_multiple_are_enforced (PYT-0564).
func TestMoqAndOrderMultipleAreEnforced(t *testing.T) {
	w := fixture()
	moq := w.variant("offer.a46.moq", func(c *Condition) { c.MOQ.Quantity = "200" })
	w.with(moq).refused(t, "MOQ_NOT_MET /args/lines/0/quantity")
	w = fixture()
	uneven := w.variant("offer.a46.multiple", func(c *Condition) { c.OrderMultiple = "50" })
	w.with(uneven).refused(t, "ORDER_MULTIPLE_INVALID /args/lines/0/quantity")
	w = fixture()
	exact := w.variant("offer.a46.multiple.ok", func(c *Condition) { c.OrderMultiple = "40" })
	l := w.with(exact).prepare(t).Doc.Lines[0]
	if l.OrderMultiple != "40" || l.MOQ.Quantity != "100" || l.MOQ.Purchased != "120" {
		t.Fatalf("line %+v", l)
	}
}

// Python test_delivery_must_be_comparable_on_time_and_not_already_passed (PYT-0568), with a lead
// time after an event (DELIVERY_UNRESOLVED, PUR-INV-006) and an order whose requirement is not a
// fixed date.
func TestDeliveryMustBeComparableOnTimeAndNotAlreadyPassed(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(d *SupplyDelivery)
		want string
	}{
		{"late", func(d *SupplyDelivery) { d.Date = "2026-11-16" }, "DELIVERY_LATE /args/lines/0/source"},
		{"milestone", func(d *SupplyDelivery) { d.Milestone = "goods_ready" }, "DELIVERY_NOT_COMPARABLE /args/lines/0/source"},
		{"place", func(d *SupplyDelivery) { d.Place = "CNSHA" }, "DELIVERY_NOT_COMPARABLE /args/lines/0/source"},
		{"zone", func(d *SupplyDelivery) { d.TimeZone = "UTC" }, "DELIVERY_NOT_COMPARABLE /args/lines/0/source"},
		{"passed", func(d *SupplyDelivery) { d.Date = "2026-10-21" }, "DELIVERY_DATE_PASSED /args/lines/0/source"},
		{"lead time", func(d *SupplyDelivery) { d.Kind, d.Date = "lead_time", "" }, "DELIVERY_UNRESOLVED /args/lines/0/source"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := fixture()
			src := w.variant("offer.a46."+c.name, func(x *Condition) { c.edit(x.Delivery) })
			w.with(src).refused(t, c.want)
		})
	}
	w := fixture()
	early := w.variant("offer.a46.early", func(c *Condition) { c.Delivery.Date = "2026-11-10" })
	d := w.with(early).prepare(t).Doc.Lines[0].Delivery
	if d.SupplierDate != "2026-11-10" || d.SlackDays != 5 {
		t.Fatalf("delivery %+v", d)
	}
	// Today in Asia/Shanghai is already 2026-10-23 at 2026-10-22T18:00Z.
	w = fixture()
	w.in.Now = time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC)
	today := w.variant("offer.a46.today", func(c *Condition) { c.Delivery.Date = "2026-10-22" })
	w.with(today).refused(t, "DELIVERY_DATE_PASSED /args/lines/0/source")
	w = fixture()
	w.in.Order.Delivery.Kind = "lead_time"
	w.refused(t, "DELIVERY_NOT_COMPARABLE /args/order_id")
}

// Python test_draft_or_revoked_order_cannot_enter_purchase_preparation (PYT-0559) and
// test_activation_without_independent_order_review_is_not_purchase_ready (PYT-0560), on TRD's
// verdict (ActiveOrderRef v1): a draft order, a revoked activating approval, an order without one,
// an order at another revision; an unknown order is NOT_FOUND.
func TestOrderMustServeNewPurchaseWork(t *testing.T) {
	for _, c := range []string{"ORDER_NOT_ACTIVE", "ORDER_APPROVAL_REVOKED", "ORDER_APPROVAL_REQUIRED", "REFERENCE_STALE"} {
		w := fixture()
		w.in.Order.Code = c
		w.refused(t, c+" /args/order_id")
	}
	w := fixture()
	w.in.Order = nil
	w.refused(t, "NOT_FOUND /args/order_id")
	w = fixture()
	w.in.Order.Code = "SOMETHING_ELSE"
	w.refused(t, "REFERENCE_INVALID /args/order_id")
}

// Python test_lines_sources_and_reason_must_be_explicit (PYT-0561), the rules' half (the structural
// half is api TestPrepareDraftContract): a line the order does not have, a condition line or a
// supply condition that does not exist.
func TestLinesAndSourcesMustExist(t *testing.T) {
	w := fixture()
	w.req.Lines[0].OrderLineID = "L9"
	w.refused(t, "ORDER_LINE_NOT_FOUND /args/lines/0/order_line_id")
	w = fixture()
	w.req.Lines[0].Source.ConditionLineID = "OL9"
	w.refused(t, "REFERENCE_INVALID /args/lines/0/source/condition_line_id")
	w = fixture()
	w.req.Lines[0].Source.ID = "offer.missing"
	w.refused(t, "REFERENCE_INVALID /args/lines/0/source")
}

// PUR-INV-004 and PUR-INV-005: SUP's verdict, the supplier as bound, an offer of another RFQ or RFQ
// line, another specification revision, CAT's and POL's verdicts, another unit and a fractional
// piece.
func TestSupplyConditionAndReferencesAreCurrent(t *testing.T) {
	stale := func(string) error {
		return kernel.RejectAs("SUPPLY_CONDITION_NOT_CURRENT", kernel.CategoryConflict, "/args/lines/0/source", "")
	}
	for _, c := range []struct {
		name string
		edit func(w *world)
		want string
	}{
		{"superseded", func(w *world) { s := w.in.Supply[offer1]; s.Check = stale; w.in.Supply[offer1] = s }, "SUPPLY_CONDITION_NOT_CURRENT /args/lines/0/source"},
		{"supplier unseen", func(w *world) { delete(w.in.Parties, "supplier.1") }, "REFERENCE_INVALID /args/lines/0/source"},
		{"not a supplier", func(w *world) { w.in.Parties["supplier.1"] = Party{Status: PartyConfirmed, RowVersion: 2} }, "REFERENCE_INVALID /args/lines/0/source"},
		{"supplier revised", func(w *world) {
			w.in.Parties["supplier.1"] = Party{Status: PartyConfirmed, RowVersion: 3, Supplier: true}
		},
			"REFERENCE_STALE /args/lines/0/source"},
		{"supplier inactive", func(w *world) { w.in.Parties["supplier.1"] = Party{Status: "inactive", RowVersion: 2, Supplier: true} },
			"REFERENCE_STALE /args/lines/0/source"},
		{"other rfq", func(w *world) {
			s := w.in.Supply[offer1]
			s.Rfq = &RfqRef{RfqID: "rfq.2", RowVersion: 1}
			w.in.Supply[offer1] = s
		},
			"SUPPLY_SCOPE_MISMATCH /args/lines/0/source"},
		{"other rfq line", func(w *world) { s := w.in.Supply[offer1]; s.RfqLineIDs = []string{"L2"}; w.in.Supply[offer1] = s },
			"SUPPLY_SCOPE_MISMATCH /args/lines/0/source"},
		{"other spec", func(w *world) { l := w.in.Order.Lines["L1"]; l.SpecRevision = 2; w.in.Order.Lines["L1"] = l },
			"SPECIFICATION_MISMATCH /args/lines/0/source"},
		{"spec reopened", func(w *world) {
			w.in.Specs[Spec{SkuID: "sku.1", Revision: 1}] = func(p string) error { return kernel.RejectAs("REFERENCE_STALE", kernel.CategoryConflict, p, "") }
		}, "REFERENCE_STALE /args/lines/0/source"},
		{"config retired", func(w *world) {
			c := w.in.Configs[trade]
			c.Check = func(p string) error { return kernel.RejectAs("CONFIG_NOT_ACTIVE", kernel.CategoryInvalid, p, "") }
			w.in.Configs[trade] = c
		}, "CONFIG_NOT_ACTIVE /args/lines/0/source"},
		// S5R-D41 (PUR-INV-012 "承诺行的单位等于订单行的单位"; "单位不同的草稿返回 UNIT_SCOPE_MISMATCH"): was
		// DEPENDENCY_NOT_SUPPORTED before S5R.
		{"other unit", func(w *world) {
			dz := ConfigKey{Kind: "unit", ID: "DZ", Revision: 1}
			w.in.Configs[dz] = Config{Check: inForce, Unit: &Unit{Code: "DZ"}}
			l := w.in.Order.Lines["L1"]
			l.Unit = dz
			w.in.Order.Lines["L1"] = l
		}, "UNIT_SCOPE_MISMATCH /args/lines/0/source"},
		{"fractional piece", func(w *world) { w.req.Lines[0].Quantity = kernel.MustDecimal("119.5") }, "INVALID_KNOWN_VALUE /args/lines/0/quantity"},
		{"no snapshot", func(w *world) { w.in.Order.Snapshot = nil }, "COST_SNAPSHOT_MISSING /args/order_id"},
		{"snapshot unread", func(w *world) { w.in.Costing = nil }, "REFERENCE_INVALID /args/order_id"},
		{"unknown tax treatment", func(w *world) {
			c := w.in.Configs[zeroTax]
			c.TaxProfile = &TaxProfile{Rate: "0", PriceBasis: BasisExclusive, Recovery: RecoveryUnknown}
			w.in.Configs[zeroTax] = c
		}, "TAX_TREATMENT_UNKNOWN /args/lines/0/source"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := fixture()
			c.edit(w)
			w.refused(t, c.want)
		})
	}
}

// Purchase terms bind their source offer: the origin and, for dependent validity, its validity
// limit the supply's (Python test_purchase_terms_follow_their_validity_mode, PYT-0563, the
// document half).
func TestPurchaseTermsRecordTheirOrigin(t *testing.T) {
	for _, mode := range []string{ValidityDependent, "independent_validity"} {
		w := fixture()
		terms := Ref{Kind: KindPurchaseTerms, ID: "terms." + mode, Revision: 1}
		s := offer(terms, "supplier.1", condition("OL1", "sku.1"))
		s.Rfq, s.RfqLineIDs = nil, nil
		s.ValidUntil = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
		s.Origin = &Origin{Ref: offer1, ValidityMode: mode, ValidFrom: s.ValidFrom, ValidUntil: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)}
		w.in.Supply[terms] = s
		p := w.with(Source{Ref: terms, ConditionLineID: "OL1"}).prepare(t)
		o := p.Doc.Sources[0].Origin
		want := "2026-12-01T00:00:00.000000Z"
		if mode == ValidityDependent {
			want = "2026-10-29T00:00:00.000000Z"
		}
		if o == nil || o.ID != "offer.1" || o.ValidityMode != mode || p.Doc.SupplyValidUntil != want || p.Doc.Lines[0].NetMinor != "60000" ||
			p.Doc.Lines[0].CostingReference.SameSource {
			t.Fatalf("%s: origin %+v until %s", mode, o, p.Doc.SupplyValidUntil)
		}
	}
}

// multiLine is Python MultiLinePurchaseDraftTests' order: lines L1 and L2 of 60 PCS each, both of
// sku.1 at revision 1 answered by OL1, costed 300.00 each.
func multiLine() *world {
	w := fixture()
	w.in.Order.Lines = map[string]OrderLine{"L1": line("L1", "L1", "sku.1", "60"), "L2": line("L2", "L2", "sku.1", "60")}
	half := kernel.MustDecimal("30000")
	w.in.Costing.Lines = map[string]CostedLine{"L1": {Sources: []Source{ol1}, PurchaseMinor: &half}, "L2": {Sources: []Source{ol1}, PurchaseMinor: &half}}
	return w
}

func (w *world) lines(src Source, ids ...string) *world {
	w.req.Lines = nil
	for _, id := range ids {
		w.req.Lines = append(w.req.Lines, RequestLine{OrderLineID: id, Quantity: w.in.Order.Lines[id].Quantity, Source: src})
	}
	return w
}

// Python test_lines_sharing_a_condition_aggregate_moq_and_split_the_group_amount (PYT-0576): one
// line of 60 does not meet the MOQ of 100; both lines priced once as one group of 120 for 600.00,
// split 300.00 and 300.00, compared with 300.00 each.
func TestLinesSharingAConditionAggregateMoqAndSplitTheGroupAmount(t *testing.T) {
	multiLine().lines(ol1, "L1").refused(t, "MOQ_NOT_MET /args/lines/0/quantity")
	p := multiLine().lines(ol1, "L2", "L1").prepare(t)
	g := p.Doc.PriceGroups
	if len(g) != 1 || g[0].Quantity != "120" || strings.Join(g[0].OrderLineIDs, ",") != "L1,L2" || g[0].NetMinor != "60000" {
		t.Fatalf("groups %+v", g)
	}
	l := p.Doc.Lines
	if l[0].OrderLineID != "L1" || l[0].NetMinor != "30000" || l[1].OrderLineID != "L2" || l[1].NetMinor != "30000" ||
		*l[0].CostingReference.ShareCostMinor != "30000" || *l[1].CostingReference.ShareCostMinor != "30000" {
		t.Fatalf("lines %+v", l)
	}
	if p.Lines[0].OrderLineID != "L2" || len(p.Allocations) != 2 || p.Allocations[0].Key.LineID != "L1" {
		t.Fatal("claims keep the request's order, roots the lock order")
	}
}

// Python test_one_draft_per_supplier_and_consistent_purchase_terms (PYT-0577): one currency, one
// supplier; separate drafts for each, the CNY one without a variance (no exchange rate is
// inferred).
func TestOneDraftPerSupplierAndConsistentPurchaseTerms(t *testing.T) {
	w := multiLine()
	usdSrc := w.variant("offer.a46.usd", func(*Condition) {})
	cnySrc := w.variant("offer.a46.cny", func(c *Condition) { c.Currency = &cny })
	w.req.Lines = []RequestLine{{OrderLineID: "L1", Quantity: kernel.MustDecimal("60"), Source: usdSrc},
		{OrderLineID: "L2", Quantity: kernel.MustDecimal("60"), Source: cnySrc}}
	w.refused(t, "PURCHASE_TERMS_INCONSISTENT /args/lines/1/source")
	w = multiLine()
	usdSrc = w.variant("offer.a46.usd", func(*Condition) {})
	provider := Ref{Kind: KindSupplierOffer, ID: "offer.a46.provider", Revision: 1}
	w.in.Supply[provider] = offer(provider, "provider.1", condition("OL1", "sku.1"))
	w.in.Parties["provider.1"] = Party{Status: PartyConfirmed, RowVersion: 2, Supplier: true}
	w.req.Lines = []RequestLine{{OrderLineID: "L1", Quantity: kernel.MustDecimal("60"), Source: usdSrc},
		{OrderLineID: "L2", Quantity: kernel.MustDecimal("60"), Source: Source{Ref: provider, ConditionLineID: "OL1"}}}
	w.refused(t, "SUPPLIER_MISMATCH /args/lines/1/source")
	for _, c := range []struct {
		edit     func(*Condition)
		line     string
		currency string
		variance bool
	}{{func(*Condition) {}, "L1", "USD", true}, {func(c *Condition) { c.Currency = &cny }, "L2", "CNY", false}} {
		w := multiLine()
		src := w.variant("offer."+c.currency, func(x *Condition) { c.edit(x); x.MOQ.Quantity = "50" })
		p := w.lines(src, c.line).prepare(t)
		if p.Doc.Currency != c.currency || p.Doc.Totals.NetMinor != "30000" || (p.Doc.Lines[0].CostingReference.CostVarianceMinor != nil) != c.variance {
			t.Fatalf("%s draft %+v", c.currency, p.Doc.Lines[0].CostingReference)
		}
	}
}

// TD-AT05 at the rules (PUR-INV-002; Go intentionally diverges from Python's one draft per order
// line, PYT-0557 and PYT-0558): 80 PCS of L1 to supplier.1, 40 to supplier.2, and one more piece is
// PURCHASE_ALLOCATION_EXCEEDED. The PostgreSQL half, with the concurrent race, is
// TestOrderLineSplitsAcrossSuppliers in internal/bootstrap.
func TestOrderLineSplitsAcrossSuppliersUpToItsQuantity(t *testing.T) {
	w := fixture()
	b := Ref{Kind: KindSupplierOffer, ID: "offer.b", Revision: 1}
	cb := condition("OL1", "sku.1")
	cb.MOQ.Quantity, cb.Tiers = "40", []Tier{{Lower: "0", UnitPrice: "5.3"}}
	w.in.Supply[b] = offer(b, "supplier.2", cb)
	w.in.Parties["supplier.2"] = Party{Status: PartyConfirmed, RowVersion: 2, Supplier: true}
	cA := w.in.Supply[offer1].Lines["OL1"]
	cA.MOQ.Quantity = "80"
	w.in.Supply[offer1].Lines["OL1"] = cA
	take := func(id, q string, src Source) (*Prepared, error) {
		w.req.DraftID, w.req.Lines = id, []RequestLine{{OrderLineID: "L1", Quantity: kernel.MustDecimal(q), Source: src}}
		p, err := Prepare(w.req, w.in)
		if err == nil {
			for _, a := range p.Allocations {
				a.Stored = a.RowVersion
				w.in.Allocations[a.Key] = a
			}
		}
		return p, err
	}
	a, err := take("draft.A", "80", ol1)
	if err != nil || a.Doc.Lines[0].NetMinor != "40000" || *a.Doc.Lines[0].CostingReference.ShareCostMinor != "40000" ||
		*a.Doc.Lines[0].CostingReference.CostVarianceMinor != "0" {
		t.Fatalf("draft.A %v", err)
	}
	bp, err := take("draft.B", "40", Source{Ref: b, ConditionLineID: "OL1"})
	if err != nil || bp.Doc.Lines[0].NetMinor != "21200" || *bp.Doc.Lines[0].CostingReference.CostVarianceMinor != "1200" {
		t.Fatalf("draft.B %v", err)
	}
	if _, err = take("draft.C", "1", ol1); code(err) != "PURCHASE_ALLOCATION_EXCEEDED /args/lines/0/quantity" {
		t.Fatalf("draft.C %s", code(err))
	}
	root := w.in.Allocations[allocation.Key{OrderID: "order.1", LineID: "L1"}]
	if root.Remaining().String() != "0" || len(root.Entries) != 2 {
		t.Fatalf("root %+v", root)
	}
}

// PUR-INV-005 as S5 decision D-14 amends it: an overbuy is marked with its quantity and reason, and
// its cost is kept out of the cost of goods sold; without the marking the MOQ refuses, without a
// reason it is OVERBUY_NOT_APPROVED. The draft carries no approval of it: the purchase commitment
// decision approves it (domain/commitment). The allocation counts the allocated quantity only.
func TestOverbuyIsMarkedAndKeptOutOfCostOfGoodsSold(t *testing.T) {
	w := fixture()
	src := w.variant("offer.moq150", func(c *Condition) { c.MOQ.Quantity = "150" })
	w.with(src).refused(t, "MOQ_NOT_MET /args/lines/0/quantity")
	w.req.Lines[0].Overbuy = &Overbuy{Quantity: kernel.MustDecimal("30"), Reason: " "}
	w.refused(t, "OVERBUY_NOT_APPROVED /args/lines/0/overbuy/reason")
	w.req.Lines[0].Overbuy.Reason = "MOQ 150; the remainder goes to stock"
	p := w.prepare(t)
	l := p.Doc.Lines[0]
	if l.AllocatedQuantity != "120" || l.OverbuyQuantity != "30" || l.PurchaseQuantity != "150" || l.NetMinor != "75000" ||
		l.SoldCostMinor != "60000" || l.OverbuyCostMinor != "15000" || *l.CostingReference.CostVarianceMinor != "0" ||
		l.Overbuy == nil || l.Overbuy.ApprovedBy != nil || l.Overbuy.AuthorityEpoch != 0 || l.MOQ.Purchased != "150" {
		t.Fatalf("line %+v", l)
	}
	if b, err := canonical(l.Overbuy); err != nil || string(b) != `{"quantity":"30","reason":"MOQ 150; the remainder goes to stock"}` {
		t.Fatalf("overbuy %s, %v", b, err)
	}
	s3 := DocOverbuy{Quantity: "30", Reason: "r", ApprovedBy: &DocActor{Kind: "human", ID: "author"}, AuthorityEpoch: 7}
	if b, err := canonical(s3); err != nil || string(b) != `{"approved_by":{"id":"author","kind":"human"},"authority_epoch":7,"quantity":"30","reason":"r"}` {
		t.Fatalf("an S3 draft's overbuy keeps its bytes: %s, %v", b, err)
	}
	if p.Allocations[0].Allocated.String() != "120" || !p.Lines[0].Overbuy.Equal(kernel.MustDecimal("30")) {
		t.Fatalf("allocation %+v", p.Allocations[0])
	}
	w.req.Lines[0].Overbuy.Quantity = kernel.MustDecimal("0.5")
	w.refused(t, "INVALID_KNOWN_VALUE /args/lines/0/overbuy/quantity")
}

// Tax: 113.00 inclusive of a recoverable 13 % is 100.00 net and 13.00 tax, costing the net (Python
// split_tax; kernel TaxInclusive).
func TestRecoverableInclusiveTaxCostsTheNet(t *testing.T) {
	w := fixture()
	src := w.variant("offer.vat", func(c *Condition) { c.TaxProfile, c.Tiers = &vat, []Tier{{Lower: "0", UnitPrice: "1.13"}} })
	w.req.Lines[0].Quantity = kernel.MustDecimal("100")
	w.in.Order.Lines["L1"] = line("L1", "L1", "sku.1", "100")
	l := w.with(src).prepare(t).Doc.Lines[0]
	if l.NetMinor != "10000" || l.TaxMinor != "1300" || l.GrossMinor != "11300" || l.CostMinor != "10000" {
		t.Fatalf("line %+v", l)
	}
}

// PUR-INV-007 and Python test_replay_line_exclusivity_and_database_immutability (PYT-0557), the
// rules' half: the same request by the same preparer is answered from the stored draft; another
// reason or another preparer is PURCHASE_DRAFT_IMMUTABLE. A tampered record fails closed (Python
// test_tampered_draft_records_fail_closed, PYT-0573).
func TestDraftIsImmutableAndVerified(t *testing.T) {
	w := fixture()
	d, err := w.prepare(t).Seal("adoptable", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Answer(w.req, w.in.Preparer); err != nil {
		t.Fatal(err)
	}
	other := w.req
	other.SelectionReason = "Different synthetic reason"
	if code(d.Answer(other, w.in.Preparer)) != "PURCHASE_DRAFT_IMMUTABLE /args/draft_id" ||
		code(d.Answer(w.req, kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer"})) != "PURCHASE_DRAFT_IMMUTABLE /args/draft_id" {
		t.Fatal("a draft is never replaced")
	}
	tamper := func(edit func(d *Draft)) string {
		c := *d
		c.Lines = append([]Line(nil), d.Lines...)
		edit(&c)
		return code(c.Verify(w.in.Scope, "/id"))
	}
	doc := decodeDoc(t, d)
	doc.Lines[0].NetMinor = "1"
	forged, _ := json.Marshal(doc)
	for name, edit := range map[string]func(d *Draft){
		"document":    func(d *Draft) { d.Document = forged },
		"hash":        func(d *Draft) { d.Hash = strings.Repeat("0", 64) },
		"rehashed":    func(d *Draft) { d.Document, d.Hash = forged, Hash(forged) },
		"request":     func(d *Draft) { d.Request, d.RequestHash = []byte("{}"), Hash([]byte("{}")) },
		"prepared at": func(d *Draft) { d.Prepared.At = d.Prepared.At.Add(time.Second) },
		"claims":      func(d *Draft) { d.Lines = nil },
		"quantity":    func(d *Draft) { d.Lines[0].Quantity = kernel.MustDecimal("119") },
		"scope":       func(d *Draft) { d.OrderHash = "other" },
	} {
		if got := tamper(edit); got != "REFERENCE_INVALID /id" {
			t.Errorf("%s: %s", name, got)
		}
	}
	if err = d.Verify(kernel.Scope{Tenant: "tenant.1", Company: "company.B"}, "/id"); code(err) != "REFERENCE_INVALID /id" {
		t.Fatal("another company's scope")
	}
}

// TD-AT06, SM-PUR-DRAFT and PUR-INV-010: a prepared or supply_expired draft is voided once at its
// version; a draft one of whose purchase commitments was issued is PURCHASE_DRAFT_COMMITTED.
func TestVoidTransitions(t *testing.T) {
	w := fixture()
	d, err := w.prepare(t).Seal("adoptable", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if code(d.Voidable(2, false)) != "VERSION_CONFLICT /args/expected_version" {
		t.Fatal("version")
	}
	if r, _ := kernel.AsRejection(d.Voidable(1, true)); r == nil || r.Code != CodePurchaseDraftCommitted || r.Category != kernel.CategoryConflict {
		t.Fatalf("committed: %v", r)
	}
	tampered := *d
	tampered.CommitmentState = "committed"
	if r, _ := kernel.AsRejection(tampered.Voidable(1, false)); r == nil || r.Code != CodePurchaseDraftCommitted {
		t.Fatalf("a stored state other than not_committed: %v", r)
	}
	if err = d.Voidable(1, false); err != nil {
		t.Fatal(err)
	}
	d.VoidBy(kernel.ActorRef{Kind: kernel.ActorHuman, ID: "author"}, 8, now.Add(time.Hour), "Wrong supply condition chosen")
	if d.Status != StatusVoided || d.RowVersion != 2 || d.State(now) != StatusVoided || d.Void.Reason != "Wrong supply condition chosen" {
		t.Fatalf("voided %+v", d)
	}
	if code(d.Voidable(2, false)) != "INVALID_STATE /args/draft_id" {
		t.Fatal("voided once")
	}
	if err = d.Verify(w.in.Scope, "/id"); err != nil {
		t.Fatal("a voided draft's record stays intact")
	}
}
