//go:build integration

package bootstrap_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The scenario suite (WBS S2-05, first half) evaluates cost scenarios through cst/api against the
// composed INQ, CAT, POL, SUP and CST on PostgreSQL 16. Its fixture is Python's
// costing_fixture.TradeFixture (tests/costing_fixture.py, fixtures/costing_inputs.json) on the SUP
// suite's world: the ready RFQ rfq.1 (L1: 120 PCS of sku.1), offer.1 (OL1: flat 5 USD per PCS, MOQ
// 100), the logistics offer log.1 (F1: 60 USD freight borne by the seller, allocated by quantity),
// the pricing policy policy.1 calculating in USD with exchange rates up to 3600 s old, the actual
// costing context context.1 requiring freight, and scenario.1 buying 120 PCS under OL1 with log.1.
// Python's location references are UN/LOCODEs in Brant: the comparison place, the delivery place
// and the logistics destination are DEHAM, where the service role of provider.1 delivers. The
// author saves, the reviewer verifies and evaluates, as in Python.

type scenWorld struct {
	*supWorld
	policy, eur, dozen polapi.ConfigRef
	serial             int
}

var deham = cstapi.BusinessRef{Kind: cstapi.PlaceKind, ID: "DEHAM", Revision: 1}

func newScenWorld(t *testing.T) *scenWorld {
	s := newSupWorld(t)
	w := &scenWorld{supWorld: s}
	w.policy = s.configure(s.author, s.reviewer, polapi.KindPricingPolicy, "policy.1", 0, s.win.from, time.Time{},
		polapi.ConfigData{PricingPolicy: &polapi.PricingPolicy{Name: "synthetic costing policy", CalculationCurrency: "USD",
			MinimumMargin: "0.1", QuoteValidDays: 7, MaxFXAgeSeconds: 3600, TaxProfile: s.tax}}, true)
	s.verified(supapi.KindSupplierOffer, "offer.1", w.offerWith(w.line()))
	s.verified(supapi.KindLogisticsOffer, "log.1", s.logistics())
	w.verifiedContext("context.1", w.contextData())
	_, err := w.saveScenario(s.author, "scenario.1", 0, w.scenario("offer.1", "log.1"))
	w.must(err)
	return w
}

func (w *scenWorld) next(prefix string) string {
	w.serial++
	return fmt.Sprintf("%s.v%d", prefix, w.serial)
}

// line is OL1 delivered at the comparison place.
func (w *scenWorld) line() supapi.ConditionLine {
	l := w.supWorld.line()
	l.Delivery.Place = "DEHAM"
	return l
}

func (w *scenWorld) offerWith(lines ...supapi.ConditionLine) supapi.SupplierOffer {
	o := w.offer()
	o.Lines = lines
	return o
}

// contextData is the Python fixture's costing context context.1 on this world's references.
func (w *scenWorld) contextData() cstapi.ContextData {
	return cstapi.ContextData{RfqRef: &cstapi.RfqRef{RfqID: "rfq.1", RowVersion: w.rfqVersion}, Kind: cstapi.ContextActual,
		Lines:                  []cstapi.ContextLine{{LineID: "L1", SkuID: "sku.1", SkuSpecRevision: w.spec, Quantity: "120", UnitRef: cfg(w.pcs)}},
		CalculationCurrencyRef: cfgPtr(w.usd), PricingPolicyRef: cfgPtr(w.policy),
		ComparisonBasis: &cstapi.ComparisonBasis{SpecificationBasis: "SPEC-1", NamedPlaceRef: deham, RequiredFeeTypeRefs: []cstapi.ConfigRef{cfg(w.freight)},
			TaxBasis: "synthetic_zero_nonrecoverable", CostBasis: cstapi.CostBasisNominalDirect, SalesTradeTermsRef: cfg(w.trade),
			SalesPaymentTermsRef: cfg(w.payment), Delivery: cstapi.Delivery{Kind: cstapi.DeliveryFixed, Date: "2027-01-31",
				Milestone: cstapi.MilestoneHandover, PlaceRef: deham, TimeZone: "Asia/Shanghai"}}}
}

// verifiedContext saves d as the author and verifies it as the reviewer.
func (w *scenWorld) verifiedContext(id string, d cstapi.ContextData) {
	w.t.Helper()
	_, err := w.c.CST.SaveContext(w.author, cstapi.SaveContext{IdempotencyKey: w.key(), ContextID: id, Data: d})
	w.must(err)
	_, err = w.c.CST.VerifyContext(w.reviewer, cstapi.VerifyContext{IdempotencyKey: w.key(), ContextID: id, ExpectedVersion: 1,
		WorkingRevision: 1, Evidence: proof})
	w.must(err)
}

// scenario is scenario.1's draft: 120 PCS of L1 under OL1 of offer, with the logistics offers.
func (w *scenWorld) scenario(offer string, logistics ...string) cstapi.ScenarioData {
	d := cstapi.ScenarioData{ContextRef: &cstapi.ContextRef{ContextID: "context.1", Revision: 1},
		Allocations: []cstapi.ScenarioAllocation{{RfqLineID: "L1", PurchaseQuantity: "120", PurchaseUnitRef: cfg(w.pcs),
			Source: cstapi.SupplyLineRef{Kind: cstapi.SupplySupplierOffer, ID: offer, Revision: 1, ConditionLineID: "OL1"}}},
		LogisticsOfferRefs: []cstapi.SupplyRef{}, FXRefs: []cstapi.ConfigRef{},
		SelectionReason: "Same specification and terms; verified synthetic fixture"}
	for _, l := range logistics {
		d.LogisticsOfferRefs = append(d.LogisticsOfferRefs, cstapi.SupplyRef{Kind: cstapi.SupplyLogisticsOffer, ID: l, Revision: 1})
	}
	return d
}

func (w *scenWorld) saveScenario(ctx context.Context, id string, expected int64, d cstapi.ScenarioData) (cstapi.Receipt, error) {
	return w.c.CST.SaveScenario(ctx, cstapi.SaveScenario{IdempotencyKey: w.key(), ScenarioID: id, ExpectedVersion: expected, Data: d})
}

func (w *scenWorld) evaluate(ctx context.Context, id string, expected int64) (cstapi.EvaluationReceipt, error) {
	return w.c.CST.EvaluateScenario(ctx, cstapi.EvaluateScenario{IdempotencyKey: w.key(), ScenarioID: id, ExpectedVersion: expected})
}

// evaluated saves d as a new scenario and evaluates it as the reviewer.
func (w *scenWorld) evaluated(d cstapi.ScenarioData) (cstapi.EvaluationReceipt, error) {
	w.t.Helper()
	id := w.next("scenario")
	_, err := w.saveScenario(w.author, id, 0, d)
	w.must(err)
	return w.evaluate(w.reviewer, id, 1)
}

func (w *scenWorld) evaluation(id string) cstapi.EvaluationView {
	w.t.Helper()
	v, err := w.c.CST.GetEvaluation(w.reviewer, id)
	w.must(err)
	return v
}

// variant verifies a new offer whose line OL1 edit changes and returns its ID.
func (w *scenWorld) variant(edit func(l *supapi.ConditionLine)) string {
	w.t.Helper()
	l := w.line()
	edit(&l)
	id := w.next("offer")
	w.verified(supapi.KindSupplierOffer, id, w.offerWith(l))
	return id
}

// logisticsVariant verifies a new logistics offer whose fee F1 edit changes and returns its ID.
func (w *scenWorld) logisticsVariant(edit func(f *supapi.Fee)) string {
	w.t.Helper()
	l := w.logistics()
	edit(&l.Fees[0])
	id := w.next("log")
	w.verified(supapi.KindLogisticsOffer, id, l)
	return id
}

// fxRate publishes an exchange rate id observed at observed, valid from then for a week.
func (w *scenWorld) fxRate(from, to, rate, purpose string, observed time.Time) cstapi.ConfigRef {
	w.t.Helper()
	observed = observed.UTC().Truncate(time.Second)
	r := w.configure(w.author, w.reviewer, polapi.KindFXRate, w.next("fx"), 0, observed, observed.Add(7*24*time.Hour),
		polapi.ConfigData{FXRate: &polapi.FXRate{FromCurrency: from, ToCurrency: to, Rate: rate, Purpose: purpose, SourceName: "SYNTHETIC RATE",
			ObservedAt: kernel.FormatTimestamp(observed)}}, true)
	return cfg(r)
}

func (w *scenWorld) euro() polapi.ConfigRef {
	if w.eur.ConfigID == "" {
		w.eur = w.currency(w.author, w.reviewer, "EUR", w.win)
	}
	return w.eur
}

// taxed returns OL1 edited to a new tax profile and the goods application binding it (Python
// test_costing_domains.py taxed_offer).
func (w *scenWorld) taxed(rate, basis, recovery string) func(l *supapi.ConditionLine) {
	profile := w.configure(w.author, w.reviewer, polapi.KindTaxProfile, w.next("tax"), 0, w.win.from, time.Time{}, polapi.ConfigData{
		TaxProfile: &polapi.TaxProfile{Name: "SYNTHETIC TAX", Rate: rate, PriceBasis: basis, RecoveryTreatment: recovery,
			PolicyBasis: "test only, not a legal determination"}}, true)
	app := w.configure(w.author, w.reviewer, polapi.KindTaxApplication, w.next("app"), 0, w.win.from, time.Time{}, polapi.ConfigData{
		TaxApplication: &polapi.TaxApplication{Name: "goods", Side: polapi.TaxSidePurchase, SubjectKind: polapi.SubjectSKU, SubjectID: "sku.1",
			PartyID: "supplier.1", TaxProfile: profile, Jurisdiction: "CN", BasisNote: "synthetic"}}, true)
	return func(l *supapi.ConditionLine) { l.TaxProfileRef, l.TaxApplicationRef = cref(profile), cref(app) }
}

// dozens returns OL1 bought and priced per DOZEN at 60 USD with an MOQ of moq DOZEN, citing a
// conversion between DOZEN and PCS (Python dozen_offer); reverse publishes PCS→DOZEN instead and
// convert false cites none.
func (w *scenWorld) dozens(moq string, convert, reverse bool) func(l *supapi.ConditionLine) {
	if w.dozen.ConfigID == "" {
		w.dozen = w.configure(w.author, w.reviewer, polapi.KindUnit, "DOZEN", 0, w.win.from, time.Time{},
			polapi.ConfigData{Unit: &polapi.Unit{Code: "DOZEN", Name: "打", Dimension: polapi.DimensionCount, Precision: 0}}, true)
	}
	var conversions []supapi.ConfigRef
	if convert {
		from, to := "DOZEN", "PCS"
		if reverse {
			from, to = to, from
		}
		c := w.configure(w.author, w.reviewer, polapi.KindUnitConversion, w.next("dozen"), 0, w.win.from, time.Time{}, polapi.ConfigData{
			UnitConversion: &polapi.UnitConversion{SkuID: "sku.1", SpecRevision: w.spec, FromUnit: from, ToUnit: to, Factor: "12",
				Basis: "合成包装资料：每打12件"}}, true)
		conversions = []supapi.ConfigRef{*cref(c)}
	}
	return func(l *supapi.ConditionLine) {
		l.PurchaseUnitRef, l.PriceUnitRef, l.TierUnitRef = cref(w.dozen), cref(w.dozen), cref(w.dozen)
		l.MOQ = &supapi.MOQ{Quantity: moq, UnitRef: cref(w.dozen), Scope: supapi.MOQScopeSKU}
		l.Tiers = []supapi.Tier{{LowerInclusive: "0", UnitPrice: "60"}}
		l.ConversionRefs = conversions
	}
}

func inDozens(w *scenWorld, d *cstapi.ScenarioData) {
	d.Allocations[0].PurchaseQuantity, d.Allocations[0].PurchaseUnitRef = "10", cfg(w.dozen)
}

func components(v cstapi.EvaluationView) map[string]cstapi.CostComponent {
	out := map[string]cstapi.CostComponent{}
	for _, c := range v.Result.Components {
		out[c.ComponentID] = c
	}
	return out
}

// Python test_costing.py test_actual_database_inputs_evaluate_freeze_and_read_after_restart (the
// evaluation; the freeze is TestSnapshotBaselineFreezes660USD) and T02 §11 既有基准回归: 120 PCS × 5 USD and a
// separate 60 USD freight are 660.00 USD, verified and comparable. The evaluation leaves the
// scenario's root version as it is, replays its receipt, emits CostScenarioEvaluated and binds every
// input with its content hash; the stored documents are the bytes their hashes cover.
func TestScenarioBaselineEvaluatesTo660USD(t *testing.T) {
	w := newScenWorld(t)
	cmd := cstapi.EvaluateScenario{IdempotencyKey: w.key(), ScenarioID: "scenario.1", ExpectedVersion: 1}
	r, err := w.c.CST.EvaluateScenario(w.reviewer, cmd)
	w.must(err)
	if r.Status != cstapi.StatusEvaluated || r.RowVersion != 1 || r.ObjectID != r.EvaluationID || r.Number != 1 || r.ScenarioVersion != 1 ||
		r.TotalMinor != "66000" || r.Currency != "USD" || r.VerificationStatus != cstapi.VerificationVerified ||
		r.ComparisonStatus != cstapi.ComparisonComparable || r.Replayed {
		t.Fatalf("receipt %+v", r)
	}
	again, err := w.c.CST.EvaluateScenario(w.reviewer, cmd)
	if err != nil || !again.Replayed || again.ActionID != r.ActionID || again.EvaluationID != r.EvaluationID || again.ResultHash != r.ResultHash {
		t.Fatalf("replay %+v, %v", again, err)
	}
	v := w.evaluation(r.EvaluationID)
	parts := components(v)
	if p, f := parts["purchase.1"], parts["fee.1"]; p.AmountMinor != "60000" || f.AmountMinor != "6000" || p.Quantity != "120" ||
		p.Allocations[0].LineID != "L1" || f.Allocations[0].AmountMinor != "6000" || len(v.Result.ExcludedComponents) != 0 {
		t.Fatalf("components %+v", v.Result.Components)
	}
	if v.InputHash != r.InputHash || v.ResultHash != r.ResultHash || v.EngineVersion != "brant.cst.evaluation/0.1.0" ||
		v.Result.InputHash != v.InputHash || v.Result.Lines[0].AmountMinor != "66000" || len(v.Result.Differences) != 0 {
		t.Fatalf("view %+v", v)
	}
	kinds := map[string]int{}
	for _, b := range v.Inputs {
		kinds[b.Context+"/"+b.Kind]++
		if b.Context != cstapi.InputFromCAT && b.ContentHash == "" {
			t.Errorf("input %+v has no content hash", b)
		}
	}
	for _, k := range []string{"cst/costing_context", "inq/rfq", "cat/sku_spec", "sup/supplier_offer", "sup/logistics_offer",
		"pol/currency", "pol/pricing_policy", "pol/unit", "pol/fee_category", "pol/tax_profile", "pol/tax_application",
		"pol/trade_terms", "pol/payment_terms"} {
		if kinds[k] == 0 {
			t.Errorf("no %s input among %v", k, kinds)
		}
	}
	var hashed int
	w.must(w.execScan("company.A", fmt.Sprintf(`SELECT count(*) FROM brant_cst.cost_evaluation WHERE evaluation_id = '%s'
		AND encode(sha256(convert_to(result, 'UTF8')), 'hex') = '%s' AND encode(sha256(convert_to(normalized_input, 'UTF8')), 'hex') = '%s'`,
		r.EvaluationID, r.ResultHash, r.InputHash), &hashed))
	if hashed != 1 {
		t.Fatal("stored documents do not hash to the receipt's hashes")
	}
	sv, err := w.c.CST.GetScenario(w.author, "scenario.1")
	w.must(err)
	if sv.RootVersion != 1 || sv.Status != cstapi.ScenarioDraft || sv.LastEvaluation != 1 || len(sv.Evaluations) != 1 ||
		sv.Evaluations[0].EvaluationID != r.EvaluationID {
		t.Fatalf("scenario %+v", sv)
	}
	var evaluated []event
	for _, e := range w.events("company.A", cstapi.Source) {
		if e.Type == cstapi.EventCostScenarioEvaluated {
			evaluated = append(evaluated, e)
		}
	}
	if len(evaluated) != 1 || evaluated[0].Payload["evaluation_id"] != r.EvaluationID || evaluated[0].Payload["result_hash"] != r.ResultHash {
		t.Fatalf("events %+v", evaluated)
	}
	b, err := w.c.CST.LatestEvaluation(w.reviewer, "scenario.1")
	w.must(err)
	if b.EvaluationID != r.EvaluationID || b.InputHash != r.InputHash || b.Freezable("/args") != nil || len(b.Inputs) != len(v.Inputs) ||
		b.Rfq.RfqID != "rfq.1" || b.Context.ContextID != "context.1" {
		t.Fatalf("bound %+v", b)
	}
}

// execScan runs a query as the application role in company's scope and scans its row into dest.
func (w *scenWorld) execScan(company, sql string, dest ...any) error {
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	if err != nil {
		return err
	}
	return bento.ScopeTransactor{DB: db, Scope: w.scope(company)}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return exec.QueryRow(ctx, sql).Scan(dest...)
	})
}

// The totals of Python test_costing_domains.py on the fixture, end to end through the commands, in
// cents, each with the independent integer arithmetic that yields it (T02 §11).
func TestScenarioFixtureTotals(t *testing.T) {
	w := newScenWorld(t)
	euro := w.variant(func(l *supapi.ConditionLine) { l.CurrencyRef = cref(w.euro()) })
	cases := []struct {
		name   string // the Python test
		data   func() cstapi.ScenarioData
		want   string
		oracle int64
	}{
		{"test_explicit_fx_produces_independent_known_cost", func() cstapi.ScenarioData {
			d := w.scenario(euro, "log.1")
			d.FXRefs = []cstapi.ConfigRef{w.fxRate("EUR", "USD", "1.1", polapi.PurposeCostEstimate, time.Now().Add(-10*time.Minute))}
			return d
		}, "72000", 120*500*11/10 + 6000},
		{"test_nonrecoverable_exclusive_tax_increases_cost", func() cstapi.ScenarioData {
			return w.scenario(w.variant(w.taxed("0.13", polapi.PriceBasisExclusive, polapi.RecoveryNonrecoverable)), "log.1")
		}, "73800", 60000 + 60000*13/100 + 6000},
		{"test_recoverable_exclusive_tax_does_not_increase_cost", func() cstapi.ScenarioData {
			return w.scenario(w.variant(w.taxed("0.13", polapi.PriceBasisExclusive, polapi.RecoveryRecoverable)), "log.1")
		}, "66000", 60000 + 6000},
		{"test_inclusive_recoverable_tax_is_removed_once", func() cstapi.ScenarioData {
			return w.scenario(w.variant(w.taxed("0.13", polapi.PriceBasisInclusive, polapi.RecoveryRecoverable)), "log.1")
		}, "59097", (2*60000*100+113)/(2*113) + 6000},
		{"test_graduated_tiers_price_each_band_once", func() cstapi.ScenarioData {
			return w.scenario(w.variant(func(l *supapi.ConditionLine) {
				l.PricingMode = supapi.PricingGraduated
				l.Tiers = []supapi.Tier{{LowerInclusive: "0", UpperExclusive: "100", UnitPrice: "5"}, {LowerInclusive: "100", UnitPrice: "4"}}
			}), "log.1")
		}, "64000", 100*500 + 20*400 + 6000},
		{"test_explicit_sku_conversion_prices_ten_dozen_as_120_pieces", func() cstapi.ScenarioData {
			d := w.scenario(w.variant(w.dozens("1", true, false)), "log.1")
			inDozens(w, &d)
			return d
		}, "66000", 10*6000 + 6000},
		{"T02 §11 两家同口径比较 A: the price includes the freight obligation", func() cstapi.ScenarioData {
			return w.scenario(w.variant(func(l *supapi.ConditionLine) { l.IncludedObligationIDs = []string{"freight.1"} }), "log.1")
		}, "60000", 120 * 500},
		{"T02 §11 两家同口径比较 B: 480 per 100 PCS with freight separate", func() cstapi.ScenarioData {
			return w.scenario(w.variant(func(l *supapi.ConditionLine) {
				l.PriceBasisQuantity, l.Tiers = "100", []supapi.Tier{{LowerInclusive: "0", UnitPrice: "480"}}
			}), "log.1")
		}, "63600", 120*48000/100 + 6000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := w.evaluated(c.data())
			if err != nil || r.TotalMinor != c.want || fmt.Sprint(c.oracle) != c.want || r.VerificationStatus != cstapi.VerificationVerified ||
				r.ComparisonStatus != cstapi.ComparisonComparable {
				t.Fatalf("%+v, %v; want %s (oracle %d)", r, err, c.want, c.oracle)
			}
		})
	}
	// T02 §11 两家同口径比较 A: the freight is shown as included, not added (CST-INV-003).
	r, err := w.evaluated(w.scenario(w.variant(func(l *supapi.ConditionLine) { l.IncludedObligationIDs = []string{"freight.1"} }), "log.1"))
	w.must(err)
	v := w.evaluation(r.EvaluationID)
	if len(v.Result.ExcludedComponents) != 1 || v.Result.ExcludedComponents[0].IncludedIn != "purchase.1" || len(v.Result.Components) != 1 {
		t.Fatalf("included freight %+v", v.Result)
	}
}

// Refusals of test_costing_domains.py whose rule needs the evaluation, end to end: the code, the
// JSON Pointer of the input, and no evaluation stored (Python reject: business store unchanged).
func TestScenarioEvaluationRefusals(t *testing.T) {
	w := newScenWorld(t)
	euro := w.variant(func(l *supapi.ConditionLine) { l.CurrencyRef = cref(w.euro()) })
	recent := time.Now().Add(-10 * time.Minute)
	cases := []struct {
		name, code, path string
		data             func() cstapi.ScenarioData
	}{
		{"test_missing_cross_currency_fx_is_not_assumed_one", cstapi.CodeFXNotApplicable, "/args/data/fx_refs",
			func() cstapi.ScenarioData { return w.scenario(euro, "log.1") }},
		{"test_reverse_fx_is_not_inverted_automatically", cstapi.CodeFXNotApplicable, "/args/data/fx_refs", func() cstapi.ScenarioData {
			d := w.scenario(euro, "log.1")
			d.FXRefs = []cstapi.ConfigRef{w.fxRate("USD", "EUR", "0.9", polapi.PurposeCostEstimate, recent)}
			return d
		}},
		{"test_quotation_fx_cannot_price_cost", cstapi.CodeFXNotApplicable, "/args/data/fx_refs", func() cstapi.ScenarioData {
			d := w.scenario(euro, "log.1")
			d.FXRefs = []cstapi.ConfigRef{w.fxRate("EUR", "USD", "1.1", polapi.PurposeQuotation, recent)}
			return d
		}},
		{"test_fx_older_than_policy_limit_is_rejected", cstapi.CodeFXNotApplicable, "/args/data/fx_refs/0", func() cstapi.ScenarioData {
			d := w.scenario(euro, "log.1")
			d.FXRefs = []cstapi.ConfigRef{w.fxRate("EUR", "USD", "1.1", polapi.PurposeCostEstimate, time.Now().Add(-2*time.Hour))}
			return d
		}},
		{"test_fractional_piece_quantity_is_rejected", kernel.CodeInvalidKnownValue, "/args/data/allocations/0/purchase_quantity",
			func() cstapi.ScenarioData {
				d := w.scenario("offer.1", "log.1")
				d.Allocations[0].PurchaseQuantity = "119.5"
				return d
			}},
		{"test_purchase_quantity_must_cover_original_demand", cstapi.CodeComparisonBasisMismatch, "/context/lines/0/quantity",
			func() cstapi.ScenarioData {
				d := w.scenario("offer.1", "log.1")
				d.Allocations[0].PurchaseQuantity = "119"
				return d
			}},
		{"test_moq_is_not_bypassed_by_demand_equality", cstapi.CodeMOQNotMet, "/lines/0/moq/quantity",
			func() cstapi.ScenarioData {
				return w.scenario(w.variant(func(l *supapi.ConditionLine) { l.MOQ.Quantity = "121" }), "log.1")
			}},
		{"test_order_multiple_is_checked_after_quantity_aggregation", cstapi.CodeOrderMultipleInvalid, "/lines/0/order_multiple",
			func() cstapi.ScenarioData {
				return w.scenario(w.variant(func(l *supapi.ConditionLine) { l.OrderMultiple = "7" }), "log.1")
			}},
		{"test_unknown_tax_recovery_cannot_be_assumed_nonrecoverable", cstapi.CodeTaxTreatmentUnknown, "/lines/0/tax_profile_ref/recovery_treatment",
			func() cstapi.ScenarioData {
				return w.scenario(w.variant(w.taxed("0", polapi.PriceBasisExclusive, polapi.RecoveryUnknown)), "log.1")
			}},
		{"test_missing_sku_conversion_is_not_inferred_from_unit_name", kernel.CodeReferenceInvalid, "/lines/0/conversion_refs", func() cstapi.ScenarioData {
			d := w.scenario(w.variant(w.dozens("1", false, false)), "log.1")
			inDozens(w, &d)
			return d
		}},
		{"test_reverse_unit_conversion_is_not_automatically_inverted", kernel.CodeReferenceInvalid, "/lines/0/conversion_refs", func() cstapi.ScenarioData {
			d := w.scenario(w.variant(w.dozens("1", true, true)), "log.1")
			inDozens(w, &d)
			return d
		}},
		{"T02 §11 包装及MOQ: an MOQ of 11 DOZEN blocks 10 DOZEN", cstapi.CodeMOQNotMet, "/lines/0/moq/quantity", func() cstapi.ScenarioData {
			d := w.scenario(w.variant(w.dozens("11", true, false)), "log.1")
			inDozens(w, &d)
			return d
		}},
		{"test_nonexistent_fee_inclusion_parent_is_not_silently_ignored", cstapi.CodeChargeScopeUnresolved, "/fees/0/included_in_component_ref",
			func() cstapi.ScenarioData {
				return w.scenario("offer.1", w.logisticsVariant(func(f *supapi.Fee) { f.IncludedInComponentRef = "unknown.parent" }))
			}},
		{"test_fee_inclusion_cycle_cannot_remove_its_own_cost", cstapi.CodeChargeDoubleCount, "/fees/0/included_in_component_ref", func() cstapi.ScenarioData {
			return w.scenario("offer.1", w.logisticsVariant(func(f *supapi.Fee) { f.IncludedInComponentRef = "F1" }))
		}},
		{"test_allocation_cannot_target_an_unpurchased_line", kernel.CodeReferenceInvalid, "/fees/0/allocation/weights/0/line_id", func() cstapi.ScenarioData {
			return w.scenario("offer.1", w.logisticsVariant(func(f *supapi.Fee) {
				f.Allocation = &supapi.Allocation{Basis: supapi.AllocationExplicit, Weights: []supapi.ExplicitWeight{{LineID: "not.purchased", Weight: "1"}}}
			}))
		}},
		{"test_missing_required_freight_is_not_zero_cost", cstapi.CodeCostInputNotReady,
			"/context/comparison_basis/required_fee_type_refs/0", func() cstapi.ScenarioData { return w.scenario("offer.1") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := w.count("company.A", "brant_cst.cost_evaluation")
			_, err := w.evaluated(c.data())
			if code(err) != c.code || !strings.HasSuffix(path(err), c.path) {
				t.Fatalf("got %s at %s (%v), want %s at %s", code(err), path(err), err, c.code, c.path)
			}
			if after := w.count("company.A", "brant_cst.cost_evaluation"); after != before {
				t.Fatalf("a refused evaluation stored %d rows", after-before)
			}
		})
	}
}
