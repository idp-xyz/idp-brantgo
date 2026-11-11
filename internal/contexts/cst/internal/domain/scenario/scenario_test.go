package scenario

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/costing"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The vectors are the Python costing fixture (tests/costing_fixture.py, fixtures/
// costing_inputs.json) as the application resolves it: context.1 (L1: 120 PCS of sku.1, USD,
// policy.1, freight required, a fixed delivery on 2027-01-31 at DEHAM), offer.1 (OL1: flat 5 USD per
// PCS, MOQ 100) and log.1 (F1: 60 USD freight borne by the seller, by quantity to L1).

var clock = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

func pol(kind, id string) Ref { return Ref{Kind: kind, ID: id, Revision: 1} }

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func rejected(t *testing.T, err error, code, path string) {
	t.Helper()
	r, isRejection := kernel.AsRejection(err)
	if !isRejection || r.Code != code || r.Fields[0].Path != path {
		t.Fatalf("got %v, want %s at %s", err, code, path)
	}
}

func fixture() Inputs {
	ref := func(r Ref) costing.Ref { return costing.Ref{Kind: r.Kind, ID: r.ID, Revision: r.Revision} }
	deham := costing.Ref{Kind: "location", ID: "DEHAM", Revision: 1}
	data := costing.Data{Rfq: &costing.RfqRef{RfqID: "rfq.1", RowVersion: 2}, Kind: costing.Actual,
		Lines:               []costing.Line{{LineID: "L1", SkuID: "sku.1", SkuSpecRevision: 1, Quantity: "120", Unit: ref(pol("unit", "PCS"))}},
		CalculationCurrency: &costing.Ref{Kind: "currency", ID: "USD", Revision: 1}, PricingPolicy: &costing.Ref{Kind: "pricing_policy", ID: "policy.1", Revision: 1},
		Basis: &costing.Basis{SpecificationBasis: "SPEC-1", NamedPlace: deham, RequiredFeeTypes: []costing.Ref{ref(pol("fee_category", "freight"))},
			TaxBasis: "synthetic", CostBasis: "nominal_direct_cost", SalesTradeTerms: ref(pol("trade_terms", "FCA")),
			SalesPaymentTerms: ref(pol("payment_terms", "payment")),
			Delivery:          costing.Delivery{Kind: "fixed", Date: "2027-01-31", Milestone: "handover", Place: deham, TimeZone: "Asia/Shanghai"}}}
	rev := &costing.Revision{Number: 1, Status: costing.Active, Data: data, Hash: data.Hash(), Proposals: map[string]costing.Proposal{},
		Seal: &costing.Seal{At: clock, Rfq: costing.Binding{RfqID: "rfq.1", RowVersion: 2, LinesHash: "lines"},
			RfqLines: []costing.RfqLine{{LineID: "L1", SkuID: "sku.1", SkuSpecRevision: 1, Quantity: "120", UOM: "PCS"}}}}
	in := func(path string) error { return nil }
	configs := map[Ref]Config{
		pol("currency", "USD"): {Check: in, ContentHash: "c-usd", Currency: &Currency{Code: "USD", MinorUnits: 2}},
		pol("unit", "PCS"):     {Check: in, ContentHash: "c-pcs", Unit: &Unit{Code: "PCS", Dimension: "count", Precision: 0}},
		pol("pricing_policy", "policy.1"): {Check: in, ContentHash: "c-policy", PricingPolicy: &PricingPolicy{CalculationCurrency: "USD",
			MaxFXAgeSeconds: 3600, TaxProfile: pol("tax_profile", "tax.zero")}},
		pol("fee_category", "freight"): {Check: in, ContentHash: "c-freight", FeeCategory: &FeeCategory{Category: "freight", Side: "purchase_cost"}},
		pol("trade_terms", "FCA"):      {Check: in, ContentHash: "c-fca", TradeTerms: &TradeTerms{Incoterm: "FCA"}},
		pol("payment_terms", "payment"): {Check: in, ContentHash: "c-pay", PaymentTerms: &PaymentTerms{Stages: []PaymentStage{{StageID: "all",
			Percent: "100", Trigger: "before_shipment"}}}},
		pol("tax_profile", "tax.zero"): {Check: in, ContentHash: "c-tax", TaxProfile: &TaxProfile{Rate: "0", PriceBasis: "exclusive", Recovery: "nonrecoverable"}},
		pol("tax_application", "tax.goods"): {Check: in, ContentHash: "c-goods", TaxApplication: &TaxApplication{Side: "purchase", SubjectKind: "sku",
			SubjectID: "sku.1", PartyID: "supplier.1", TaxProfile: pol("tax_profile", "tax.zero")}},
		pol("tax_application", "tax.service"): {Check: in, ContentHash: "c-service", TaxApplication: &TaxApplication{Side: "purchase",
			SubjectKind: "fee_category", SubjectID: "freight", PartyID: "provider.1", TaxProfile: pol("tax_profile", "tax.zero")}},
	}
	p := func(k, id string) *Ref { r := pol(k, id); return &r }
	line := ConditionLine{ID: "OL1", SkuID: "sku.1", SpecRevision: 1, PurchaseUnit: p("unit", "PCS"), PriceUnit: p("unit", "PCS"),
		PriceBasisQuantity: "1", TierUnit: p("unit", "PCS"), PriceTierScope: "sku", PricingMode: "flat", Tiers: []Tier{{Lower: "0", UnitPrice: "5"}},
		MOQ: &MOQ{Quantity: "100", Unit: p("unit", "PCS"), Scope: "sku"}, Currency: p("currency", "USD"), TaxProfile: p("tax_profile", "tax.zero"),
		TaxApplication: p("tax_application", "tax.goods"), TradeTerms: p("trade_terms", "FCA"), PaymentTerms: p("payment_terms", "payment"),
		Delivery: &Delivery{Kind: "fixed", Date: "2027-01-15", Milestone: "handover", Place: "DEHAM", TimeZone: "Asia/Shanghai"}}
	fee := Fee{ID: "F1", ObligationID: "freight.1", FeeType: p("fee_category", "freight"), Party: "seller", Currency: p("currency", "USD"),
		TaxProfile: p("tax_profile", "tax.zero"), TaxApplication: p("tax_application", "tax.service"), Billing: &Billing{Kind: "fixed", Amount: "60"},
		Chargeable: &Chargeable{Kind: "fixed"}, Allocation: &FeeAllocation{Basis: "quantity", Targets: []string{"L1"}}}
	supply := map[Ref]Supply{
		{Kind: KindSupplierOffer, ID: "offer.1", Revision: 1}: {Status: "active", Check: in, ContentHash: "s-offer", VerifiedAt: clock,
			Offer: &Offer{SupplierID: "supplier.1", RfqID: "rfq.1", RfqRowVersion: 2, RfqLineIDs: []string{"L1"}, Lines: []ConditionLine{line}}},
		{Kind: KindLogisticsOffer, ID: "log.1", Revision: 1}: {Status: "active", Check: in, ContentHash: "s-log", VerifiedAt: clock,
			Logistics: &Logistics{ProviderID: "provider.1", ServiceCode: "sea.fcl", Mode: "sea", Origin: "CNSHA", Destination: "DEHAM", Fees: []Fee{fee}}},
	}
	return Inputs{ScenarioID: "scenario.1", Now: clock, Data: Data{Context: &ContextRef{ContextID: "context.1", Revision: 1},
		Allocations: []Allocation{{RfqLineID: "L1", PurchaseQuantity: "120", PurchaseUnit: pol("unit", "PCS"),
			Source: LineRef{Kind: KindSupplierOffer, ID: "offer.1", Revision: 1, ConditionLineID: "OL1"}}},
		LogisticsOffers: []Ref{{Kind: KindLogisticsOffer, ID: "log.1", Revision: 1}}, SelectionReason: "verified synthetic fixture"},
		Context: &Context{ID: "context.1", Revision: rev}, Rfq: &Rfq{RfqID: "rfq.1", RowVersion: 2, Ready: true, LinesHash: "lines"},
		Specs: map[costing.Spec]Standing{{SkuID: "sku.1", Revision: 1}: {Check: in}}, Configs: configs, Supply: supply}
}

// T02 §11 既有基准回归 (Python test_costing.py test_actual_database_inputs_evaluate_freeze_and_read_
// after_restart): 120 × 5 USD and a separate 60 USD freight are 660.00 USD, verified and comparable;
// the result binds the input hash and the engine version.
func TestEvaluateBaseline(t *testing.T) {
	c, err := Evaluate(fixture())
	ok(t, err)
	r := c.Result
	if r.TotalMinor != "66000" || r.VerificationStatus != Verified || r.ComparisonStatus != Comparable || len(r.Components) != 2 ||
		r.Components[0].ComponentID != "fee.1" || r.Components[1].AmountMinor != "60000" || r.InputHash != c.InputHash ||
		r.EngineVersion != EngineVersion || HashBytes(c.ResultBytes) != c.ResultHash || HashBytes(c.InputBytes) != c.InputHash {
		t.Fatalf("result %s", c.ResultBytes)
	}
	if !bytes.Equal(c.ResultBytes, Canonical(r)) || !strings.HasPrefix(string(c.ResultBytes), `{"assumptions":[],"budget":`) {
		t.Fatalf("not canonical: %s", c.ResultBytes)
	}
}

// The result is a pure function of the normalized input: the instant of the evaluation (within the
// inputs' validity) and the spelling of a decimal change neither document.
func TestEvaluateIsDeterministic(t *testing.T) {
	a, err := Evaluate(fixture())
	ok(t, err)
	later := fixture()
	later.Now = clock.Add(time.Hour)
	later.Data.Allocations[0].PurchaseQuantity = "120.000000"
	b, err := Evaluate(later)
	ok(t, err)
	if !bytes.Equal(a.InputBytes, b.InputBytes) || !bytes.Equal(a.ResultBytes, b.ResultBytes) || a.ResultHash != b.ResultHash {
		t.Fatalf("documents differ:\n%s\n%s", a.ResultBytes, b.ResultBytes)
	}
}

// The order of the checks follows Python _cost: completeness, the context revision, the RFQ, then
// the inputs; each refusal names the input.
func TestEvaluateRefusals(t *testing.T) {
	for _, c := range []struct {
		name, code, path string
		edit             func(in *Inputs)
	}{
		{"incomplete", CodeCostInputNotReady, "/args/data/selection_reason", func(in *Inputs) { in.Data.SelectionReason = "" }},
		{"context absent", CodeReferenceStale, "/args/data/context_ref", func(in *Inputs) { in.Context = nil }},
		{"context superseded", CodeReferenceStale, "/args/data/context_ref", func(in *Inputs) { in.Context.Revision.Status = costing.Superseded }},
		{"RFQ not ready", CodeReferenceStale, "/context/rfq_ref", func(in *Inputs) { in.Rfq.Ready = false }},
		{"RFQ lines changed", CodeReferenceStale, "/context/rfq_ref", func(in *Inputs) { in.Rfq.LinesHash = "other" }},
		{"configuration not in force", "CONFIG_NOT_ACTIVE", "/context/calculation_currency_ref", func(in *Inputs) {
			c := in.Configs[pol("currency", "USD")]
			c.Check = func(path string) error {
				return kernel.RejectAs("CONFIG_NOT_ACTIVE", kernel.CategoryConflict, path, "")
			}
			in.Configs[pol("currency", "USD")] = c
		}},
		{"unknown demand line", kernel.CodeReferenceInvalid, "/args/data/allocations/0/rfq_line_id", func(in *Inputs) { in.Data.Allocations[0].RfqLineID = "L9" }},
		{"unknown source", kernel.CodeReferenceInvalid, "/args/data/allocations/0/source", func(in *Inputs) { in.Data.Allocations[0].Source.Revision = 2 }},
		{"offer of another RFQ version", kernel.CodeReferenceInvalid, "/args/data/allocations/0/source", func(in *Inputs) {
			s := in.Supply[Ref{Kind: KindSupplierOffer, ID: "offer.1", Revision: 1}]
			s.Offer.RfqRowVersion = 1
		}},
		{"logistics elsewhere", CodeComparisonBasisMismatch, "/logistics_offer/log.1/destination", func(in *Inputs) {
			in.Supply[Ref{Kind: KindLogisticsOffer, ID: "log.1", Revision: 1}].Logistics.Destination = "CNSHA"
		}},
		{"goods tax application for a service", kernel.CodeReferenceInvalid, "/supplier_offer/offer.1/lines/0/tax_application_ref", func(in *Inputs) {
			in.Supply[Ref{Kind: KindSupplierOffer, ID: "offer.1", Revision: 1}].Offer.Lines[0].TaxApplication = &Ref{Kind: "tax_application", ID: "tax.service", Revision: 1}
		}},
		{"anchor on a PTY role", CodeDependencyNotSupported, "/context/comparison_basis/delivery/anchor_ref", func(in *Inputs) {
			b := in.Context.Revision.Data.Basis
			b.Delivery = costing.Delivery{Kind: "lead_time", Anchor: &costing.Ref{Kind: "delivery_anchor", ID: "service_party_role:role.1", Revision: 1},
				Days: 3, DayBasis: "working", Calendar: &costing.Ref{Kind: "calendar", ID: "cal", Revision: 1}, Milestone: "handover",
				Place: b.NamedPlace, TimeZone: "Asia/Shanghai"}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := fixture()
			c.edit(&in)
			_, err := Evaluate(in)
			rejected(t, err, c.code, c.path)
		})
	}
}

// A supplier's lead time counts from an order event that has not occurred while costing: the
// delivery stays unresolved, the result provisional and not comparable, and the cost is unchanged.
func TestSupplyLeadTimeStaysUnresolved(t *testing.T) {
	in := fixture()
	cal := pol("calendar", "cal")
	in.Configs[cal] = Config{Check: func(string) error { return nil }, ContentHash: "c-cal", ValidFrom: clock.Add(-24 * time.Hour),
		Calendar: &Calendar{TimeZone: "Asia/Shanghai", WorkingWeekdays: []int{1, 2, 3, 4, 5}}}
	in.Supply[Ref{Kind: KindSupplierOffer, ID: "offer.1", Revision: 1}].Offer.Lines[0].Delivery = &Delivery{Kind: "lead_time",
		AnchorEvent: "order_confirmed", Days: 30, DayBasis: "working", Calendar: &cal, Milestone: "handover", Place: "DEHAM", TimeZone: "Asia/Shanghai"}
	c, err := Evaluate(in)
	ok(t, err)
	r := c.Result
	if r.VerificationStatus != Provisional || r.ComparisonStatus != NotComparable || r.TotalMinor != "66000" {
		t.Fatalf("result %s", c.ResultBytes)
	}
	if d := r.DeliveryChecks[1]; d.Role != RoleSupply || d.Status != "unresolved" || d.Date != "" || !strings.Contains(d.Reason, "order_confirmed") {
		t.Fatalf("delivery %+v", d)
	}
}

// Normalized orders the selection and trims decimals; text is kept as given.
func TestNormalized(t *testing.T) {
	d := Data{Allocations: []Allocation{{RfqLineID: "L2", PurchaseQuantity: "1.50"}, {RfqLineID: "L1", PurchaseQuantity: "120.000000"}},
		FX: []Ref{{Kind: "fx_rate", ID: "b", Revision: 1}, {Kind: "fx_rate", ID: "a", Revision: 2}}, SelectionReason: " as given "}
	n := d.Normalized()
	if n.Allocations[0].RfqLineID != "L1" || n.Allocations[0].PurchaseQuantity != "120" || n.Allocations[1].PurchaseQuantity != "1.5" ||
		n.FX[0].ID != "a" || n.SelectionReason != " as given " || d.Allocations[0].RfqLineID != "L2" {
		t.Fatalf("normalized %+v", n)
	}
	if trimDecimal("100") != "100" || trimDecimal("0.0") != "0" || trimDecimal("10.010") != "10.01" {
		t.Fatal("trimDecimal")
	}
}

// Canonical JSON sorts members at every level and does not escape HTML.
func TestCanonical(t *testing.T) {
	got := string(Canonical(map[string]any{"b": []any{map[string]any{"z": 1, "a": "<&>"}}, "a": "x"}))
	if got != `{"a":"x","b":[{"a":"<&>","z":1}]}` {
		t.Fatal(got)
	}
}

// The state rules of SM-CST-SCENARIO: a missing scenario is created at version 0 only; a command on
// an existing one needs visibility, the current root version and a draft; evaluating counts an
// evaluation without changing the root version.
func TestScenarioState(t *testing.T) {
	rejected(t, CheckSave(nil, false, 1), kernel.CodeNotFound, "/args/id")
	ok(t, CheckSave(nil, false, 0))
	s := New("s", "a", "")
	s.Save(Data{}, kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent"}, 1, clock)
	rejected(t, Check(s, false, 1), kernel.CodeNotFound, "/args/id")
	rejected(t, Check(s, true, 2), kernel.CodeVersionConflict, "/args/expected_version")
	if n := s.Append(); n != 1 || s.RootVersion != 1 {
		t.Fatalf("append %d at %d", n, s.RootVersion)
	}
	s.Status = Frozen
	rejected(t, Check(s, true, 1), kernel.CodeInvalidState, "/args/id")
}
