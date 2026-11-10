package api

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func rejection(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code + " " + rej.Fields[0].Path
	}
	if err != nil {
		return "ERROR"
	}
	return "OK"
}

var (
	usd   = ConfigRef{Kind: KindCurrency, ID: "USD", Revision: 1}
	port  = BusinessRef{Kind: PlaceKind, ID: "port.test", Revision: 1}
	proof = []Evidence{{SourceID: "src.1", Locator: "line 1"}}
)

func data() ContextData {
	return ContextData{RfqRef: &RfqRef{RfqID: "rfq.1", RowVersion: 2}, Kind: ContextActual,
		Lines:                  []ContextLine{{LineID: "L1", SkuID: "sku.1", SkuSpecRevision: 1, Quantity: "120", UnitRef: ConfigRef{Kind: KindUnit, ID: "PCS", Revision: 1}}},
		CalculationCurrencyRef: &usd, PricingPolicyRef: &ConfigRef{Kind: KindPricingPolicy, ID: "policy.1", Revision: 1},
		ComparisonBasis: &ComparisonBasis{SpecificationBasis: "SPEC-1", NamedPlaceRef: port, TaxBasis: "zero", CostBasis: CostBasisNominalDirect,
			SalesTradeTermsRef: ConfigRef{Kind: KindTradeTerms, ID: "t", Revision: 1}, SalesPaymentTermsRef: ConfigRef{Kind: KindPaymentTerms, ID: "p", Revision: 1},
			Delivery: Delivery{Kind: DeliveryFixed, Date: "2026-11-15", Milestone: MilestoneHandover, PlaceRef: port, TimeZone: "Asia/Shanghai"}},
		Assumptions: []Assumption{{AssumptionID: "a.1", Path: "/context/lines/0/quantity", ValueText: "120", Reason: "试算", Evidence: proof}}}
}

// The closed contract of costing_context.save_draft (Python commands.schema.json context_draft).
func TestSaveContextValidate(t *testing.T) {
	cases := []struct {
		name string
		edit func(c *SaveContext)
		want string
	}{
		{"complete", func(*SaveContext) {}, "OK"},
		{"empty draft", func(c *SaveContext) { c.Data = ContextData{} }, "OK"},
		{"key", func(c *SaveContext) { c.IdempotencyKey = "" }, "SCHEMA_INVALID /idempotency_key"},
		{"id", func(c *SaveContext) { c.ContextID = "bad id" }, "SCHEMA_INVALID /args/id"},
		{"version", func(c *SaveContext) { c.ExpectedVersion = -1 }, "SCHEMA_INVALID /args/expected_version"},
		{"rfq", func(c *SaveContext) { c.Data.RfqRef.RowVersion = 0 }, "SCHEMA_INVALID /args/data/rfq_ref/row_version"},
		{"kind", func(c *SaveContext) { c.Data.Kind = "formal" }, "SCHEMA_INVALID /args/data/context_kind"},
		{"empty lines", func(c *SaveContext) { c.Data.Lines = []ContextLine{} }, "SCHEMA_INVALID /args/data/lines"},
		{"zero quantity", func(c *SaveContext) { c.Data.Lines[0].Quantity = "0" }, "SCHEMA_INVALID /args/data/lines/0/quantity"},
		{"float quantity", func(c *SaveContext) { c.Data.Lines[0].Quantity = "1e2" }, "SCHEMA_INVALID /args/data/lines/0/quantity"},
		{"seven digits", func(c *SaveContext) { c.Data.Lines[0].Quantity = "1.0000001" }, "SCHEMA_INVALID /args/data/lines/0/quantity"},
		{"spec revision", func(c *SaveContext) { c.Data.Lines[0].SkuSpecRevision = 0 }, "SCHEMA_INVALID /args/data/lines/0/sku_spec_revision"},
		{"unit", func(c *SaveContext) { c.Data.Lines[0].UnitRef.Revision = 0 }, "SCHEMA_INVALID /args/data/lines/0/unit_ref/revision"},
		{"conversions", func(c *SaveContext) { c.Data.Lines[0].ConversionRefs = make([]ConfigRef, 9) }, "SCHEMA_INVALID /args/data/lines/0/conversion_refs"},
		{"currency", func(c *SaveContext) {
			c.Data.CalculationCurrencyRef = &ConfigRef{Kind: "currency", ID: "", Revision: 1}
		},
			"SCHEMA_INVALID /args/data/calculation_currency_ref/id"},
		{"cost basis", func(c *SaveContext) { c.Data.ComparisonBasis.CostBasis = "full" }, "SCHEMA_INVALID /args/data/comparison_basis/cost_basis"},
		{"port kind", func(c *SaveContext) { c.Data.ComparisonBasis.NamedPlaceRef.Kind = "port" },
			"SCHEMA_INVALID /args/data/comparison_basis/named_place_ref/kind"},
		{"milestone", func(c *SaveContext) { c.Data.ComparisonBasis.Delivery.Milestone = "shipped" },
			"SCHEMA_INVALID /args/data/comparison_basis/delivery/milestone"},
		{"date", func(c *SaveContext) { c.Data.ComparisonBasis.Delivery.Date = "2026-02-30" }, "SCHEMA_INVALID /args/data/comparison_basis/delivery/date"},
		{"fixed with days", func(c *SaveContext) { c.Data.ComparisonBasis.Delivery.Days = 3 }, "SCHEMA_INVALID /args/data/comparison_basis/delivery"},
		{"lead time without anchor", func(c *SaveContext) {
			c.Data.ComparisonBasis.Delivery = Delivery{Kind: DeliveryLeadTime, Days: 3, DayBasis: DayBasisWorking, Milestone: MilestoneHandover,
				PlaceRef: port, TimeZone: "Asia/Shanghai", CalendarRef: &ConfigRef{Kind: KindCalendar, ID: "c", Revision: 1}}
		}, "SCHEMA_INVALID /args/data/comparison_basis/delivery/anchor_ref"},
		{"lead time", func(c *SaveContext) {
			c.Data.ComparisonBasis.Delivery = Delivery{Kind: DeliveryLeadTime, AnchorRef: &BusinessRef{Kind: AnchorKind, ID: "costing_context:c", Revision: 1},
				DayBasis: DayBasisCalendar, Milestone: MilestoneHandover, PlaceRef: port, TimeZone: "Asia/Shanghai",
				CalendarRef: &ConfigRef{Kind: KindCalendar, ID: "c", Revision: 1}}
		}, "OK"},
		{"lead days", func(c *SaveContext) {
			c.Data.ComparisonBasis.Delivery = Delivery{Kind: DeliveryLeadTime, AnchorRef: &BusinessRef{Kind: AnchorKind, ID: "costing_context:c", Revision: 1},
				Days: 3651, DayBasis: DayBasisCalendar, Milestone: MilestoneHandover, PlaceRef: port, TimeZone: "Asia/Shanghai",
				CalendarRef: &ConfigRef{Kind: KindCalendar, ID: "c", Revision: 1}}
		}, "SCHEMA_INVALID /args/data/comparison_basis/delivery/days"},
		{"assumption path", func(c *SaveContext) { c.Data.Assumptions[0].Path = "" }, "SCHEMA_INVALID /args/data/assumptions/0/path"},
		{"assumption evidence", func(c *SaveContext) { c.Data.Assumptions[0].Evidence = nil }, "SCHEMA_INVALID /args/data/assumptions/0/evidence"},
		{"too many assumptions", func(c *SaveContext) { c.Data.Assumptions = make([]Assumption, MaxAssumptions+1) }, "SCHEMA_INVALID /args/data/assumptions"},
	}
	for _, k := range cases {
		t.Run(k.name, func(t *testing.T) {
			c := SaveContext{IdempotencyKey: "k1", ContextID: "context.1", Data: data()}
			basis := *c.Data.ComparisonBasis
			c.Data.ComparisonBasis = &basis
			c.Data.Lines = []ContextLine{c.Data.Lines[0]}
			c.Data.Assumptions = []Assumption{c.Data.Assumptions[0]}
			k.edit(&c)
			if got := rejection(c.Validate()); got != k.want {
				t.Fatalf("%s, want %s", got, k.want)
			}
		})
	}
}

func TestContextCommandsValidate(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{ReviseContext{IdempotencyKey: "k", ContextID: "c", ExpectedVersion: 2, BaseRevision: 1, Reason: "修订"}.Validate(), "OK"},
		{ReviseContext{IdempotencyKey: "k", ContextID: "c", ExpectedVersion: 0, BaseRevision: 1, Reason: "修订"}.Validate(), "SCHEMA_INVALID /args/expected_version"},
		{ReviseContext{IdempotencyKey: "k", ContextID: "c", ExpectedVersion: 2, Reason: "修订"}.Validate(), "SCHEMA_INVALID /args/base_revision"},
		{ReviseContext{IdempotencyKey: "k", ContextID: "c", ExpectedVersion: 2, BaseRevision: 1}.Validate(), "SCHEMA_INVALID /args/reason"},
		{DiscardContextDraft{IdempotencyKey: "k", ContextID: "c", ExpectedVersion: 1, WorkingRevision: 1, Reason: "丢弃"}.Validate(), "OK"},
		{DiscardContextDraft{IdempotencyKey: "k", ContextID: "c", ExpectedVersion: 1, Reason: "丢弃"}.Validate(), "SCHEMA_INVALID /args/working_revision"},
		{VerifyContext{IdempotencyKey: "k", ContextID: "c", ExpectedVersion: 1, WorkingRevision: 1, Evidence: proof}.Validate(), "OK"},
		{VerifyContext{IdempotencyKey: "k", ContextID: "c", ExpectedVersion: 1, WorkingRevision: 1}.Validate(), "SCHEMA_INVALID /args/evidence"},
		{VerifyContext{IdempotencyKey: "k", ContextID: "c", ExpectedVersion: 1, WorkingRevision: 1, SupersedesRevision: -1, Evidence: proof}.Validate(),
			"SCHEMA_INVALID /args/supersedes_revision"},
		{VerifyContext{IdempotencyKey: "k", ContextID: "c", ExpectedVersion: 1, WorkingRevision: 1,
			Evidence: []Evidence{{SourceID: "s", Locator: ""}}}.Validate(), "SCHEMA_INVALID /args/evidence/0/locator"},
		{WithdrawContext{IdempotencyKey: "k", ContextID: "c", ExpectedVersion: 3, TargetRevision: 1, Reason: "撤销", Evidence: proof}.Validate(), "OK"},
		{WithdrawContext{IdempotencyKey: "k", ContextID: "c", ExpectedVersion: 3, TargetRevision: 1, Reason: "撤销"}.Validate(), "SCHEMA_INVALID /args/evidence"},
	} {
		if got := rejection(c.err); got != c.want {
			t.Errorf("%s, want %s", got, c.want)
		}
	}
}

func measured() MeasurementData {
	return MeasurementData{Kind: MeasurementMeasured, ObservedAt: "2026-10-22T08:00:00Z", DeclaredVolumeM3: "1", Evidence: proof,
		Packages: []Package{{PackageID: "P1", Count: 1, LengthM: "1", WidthM: "1", HeightM: "1", NetKG: "9", GrossKG: "10", LineIDs: []string{"L1"}}}}
}

// The closed contract of the measurement commands (Python commands.schema.json measurement).
func TestMeasurementCommandsValidate(t *testing.T) {
	cases := []struct {
		name string
		edit func(d *MeasurementData)
		want string
	}{
		{"fixture", func(*MeasurementData) {}, "OK"},
		{"kind", func(d *MeasurementData) { d.Kind = "guessed" }, "SCHEMA_INVALID /args/data/kind"},
		{"estimate needs a reason", func(d *MeasurementData) { d.Kind = MeasurementEstimated }, "SCHEMA_INVALID /args/data/estimate_reason"},
		{"measured has none", func(d *MeasurementData) { d.EstimateReason = "x" }, "SCHEMA_INVALID /args/data/estimate_reason"},
		{"estimate", func(d *MeasurementData) { d.Kind, d.EstimateReason = MeasurementEstimated, "按规格估算" }, "OK"},
		{"offset time", func(d *MeasurementData) { d.ObservedAt = "2026-10-22T08:00:00+08:00" }, "SCHEMA_INVALID /args/data/observed_at"},
		{"no packages", func(d *MeasurementData) { d.Packages = nil }, "SCHEMA_INVALID /args/data/packages"},
		{"count", func(d *MeasurementData) { d.Packages[0].Count = 0 }, "SCHEMA_INVALID /args/data/packages/0/count"},
		{"zero weight", func(d *MeasurementData) { d.Packages[0].NetKG = "0.0" }, "SCHEMA_INVALID /args/data/packages/0/net_kg"},
		{"no lines", func(d *MeasurementData) { d.Packages[0].LineIDs = nil }, "SCHEMA_INVALID /args/data/packages/0/line_ids"},
		{"declared volume", func(d *MeasurementData) { d.DeclaredVolumeM3 = "-1" }, "SCHEMA_INVALID /args/data/declared_volume_m3"},
		{"evidence", func(d *MeasurementData) { d.Evidence = nil }, "SCHEMA_INVALID /args/data/evidence"},
	}
	for _, k := range cases {
		t.Run(k.name, func(t *testing.T) {
			d := measured()
			d.Packages = []Package{d.Packages[0]}
			k.edit(&d)
			if got := rejection(RecordMeasurement{IdempotencyKey: "k", MeasurementID: "m", Data: d}.Validate()); got != k.want {
				t.Fatalf("%s, want %s", got, k.want)
			}
		})
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{RecordMeasurement{IdempotencyKey: "k", MeasurementID: "m", ExpectedVersion: 1, Data: measured()}.Validate(), "SCHEMA_INVALID /args/expected_version"},
		{SupersedeMeasurement{IdempotencyKey: "k", MeasurementID: "m", ExpectedVersion: 1, ActiveRevision: 1, Data: measured(), Reason: "重测"}.Validate(), "OK"},
		{SupersedeMeasurement{IdempotencyKey: "k", MeasurementID: "m", ExpectedVersion: 1, Data: measured(), Reason: "重测"}.Validate(),
			"SCHEMA_INVALID /args/active_revision"},
		{SupersedeMeasurement{IdempotencyKey: "k", MeasurementID: "m", ExpectedVersion: 1, ActiveRevision: 1, Data: measured()}.Validate(),
			"SCHEMA_INVALID /args/reason"},
		{WithdrawMeasurement{IdempotencyKey: "k", MeasurementID: "m", ExpectedVersion: 2, TargetRevision: 1, Reason: "撤销", Evidence: proof}.Validate(), "OK"},
		{WithdrawMeasurement{IdempotencyKey: "k", MeasurementID: "m", ExpectedVersion: 2, TargetRevision: 1, Reason: "撤销"}.Validate(),
			"SCHEMA_INVALID /args/evidence"},
	} {
		if got := rejection(c.err); got != c.want {
			t.Errorf("%s, want %s", got, c.want)
		}
	}
}

// The standing a lookup gives the evaluation: stale when not current, not freezable when what_if
// or provisional.
func TestLookupStanding(t *testing.T) {
	v := VerifiedContext{Status: StatusActive, Kind: ContextActual}
	if v.Check("/c") != nil || v.Freezable("/c") != nil || v.Provisional() {
		t.Fatal("an active actual context without assumptions")
	}
	v.Status = StatusSuperseded
	if rejection(v.Check("/c")) != "REFERENCE_STALE /c" {
		t.Fatal(rejection(v.Check("/c")))
	}
	if c, _ := kernel.AsRejection(v.Check("/c")); c.Category != kernel.CategoryConflict {
		t.Fatal("stale is a conflict")
	}
	v.Kind = ContextWhatIf
	if rejection(v.Freezable("/c")) != "COST_INPUT_NOT_READY /c/context_kind" {
		t.Fatal(rejection(v.Freezable("/c")))
	}
	v.Kind, v.Assumptions = ContextActual, []BoundAssumption{{VerificationStatus: AssumptionUnverified}}
	if !v.Provisional() || rejection(v.Freezable("/c")) != "COST_INPUT_NOT_READY /c/assumptions" {
		t.Fatal(rejection(v.Freezable("/c")))
	}
	m := ResolvedMeasurement{Status: StatusWithdrawn}
	if m.Current() || rejection(m.Check("/m")) != "REFERENCE_STALE /m" {
		t.Fatal("withdrawn measurement")
	}
}
