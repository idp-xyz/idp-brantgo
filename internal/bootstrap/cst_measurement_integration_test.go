//go:build integration

package bootstrap_test

import (
	"slices"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func costingGrant(c *costingWorld, kind, id string) bootstrap.Grant {
	return bootstrap.Grant{Scope: c.scope("company.A"), Actor: actorRef(kind, id), Permissions: allPermissions, DataScope: "company", Team: "trade"}
}

// measured is the Python fixture's measurement.1 (costing_inputs.json): one 1 m³ package of line
// L1, 9 kg net and 10 kg gross, declared 1 m³, observed an hour ago.
func measured() cstapi.MeasurementData {
	return cstapi.MeasurementData{Kind: cstapi.MeasurementMeasured, ObservedAt: kernel.FormatTimestamp(time.Now().Add(-time.Hour)),
		Packages: []cstapi.Package{{PackageID: "P1", Count: 1, LengthM: "1", WidthM: "1", HeightM: "1", NetKG: "9", GrossKG: "10",
			LineIDs: []string{"L1"}}}, DeclaredVolumeM3: "1", Evidence: proof}
}

func (c *costingWorld) record(id string, d cstapi.MeasurementData) (cstapi.Receipt, error) {
	return c.c.CST.RecordMeasurement(c.checker, cstapi.RecordMeasurement{IdempotencyKey: c.key(), MeasurementID: id, Data: d})
}

// Python test_costing_domains.py test_measurement_volume_contradiction_is_rejected,
// test_gross_weight_cannot_be_less_than_net_weight and test_future_measurement_is_not_a_verified_fact:
// measurement.record_verified judges the data with the rule every evaluation applies
// (rules.Measurement.Check), and a refused record leaves nothing.
func TestMeasurementRecordingChecks(t *testing.T) {
	c := newCostingWorld(t)
	cases := []struct {
		name  string
		edit  func(d *cstapi.MeasurementData)
		code  string
		where string
	}{
		{"volume contradiction", func(d *cstapi.MeasurementData) { d.DeclaredVolumeM3 = "2" }, cstapi.CodeMeasurementConflict, "/args/data/declared_volume_m3"},
		{"gross below net", func(d *cstapi.MeasurementData) { d.Packages[0].GrossKG = "8" }, kernel.CodeInvalidKnownValue, "/args/data/packages/0/gross_kg"},
		{"future observation", func(d *cstapi.MeasurementData) { d.ObservedAt = kernel.FormatTimestamp(time.Now().Add(time.Hour)) },
			kernel.CodeInvalidKnownValue, "/args/data/observed_at"},
		{"duplicate package", func(d *cstapi.MeasurementData) { d.Packages = append(d.Packages, d.Packages[0]) }, kernel.CodeDuplicateLineID,
			"/args/data/packages/1/id"},
		{"zero dimension", func(d *cstapi.MeasurementData) { d.Packages[0].HeightM = "0" }, kernel.CodeSchemaInvalid, "/args/data/packages/0/height_m"},
		{"estimate without reason", func(d *cstapi.MeasurementData) { d.Kind = cstapi.MeasurementEstimated }, kernel.CodeSchemaInvalid,
			"/args/data/estimate_reason"},
		{"offset timestamp", func(d *cstapi.MeasurementData) { d.ObservedAt = "2026-10-22T08:00:00+08:00" }, kernel.CodeSchemaInvalid, "/args/data/observed_at"},
	}
	for _, k := range cases {
		t.Run(k.name, func(t *testing.T) {
			d := measured()
			d.Packages = slices.Clone(d.Packages)
			k.edit(&d)
			_, err := c.record("domain.measure", d)
			if code(err) != k.code || path(err) != k.where {
				t.Fatalf("%s at %s, want %s at %s", code(err), path(err), k.code, k.where)
			}
		})
	}
	if n := c.count("company.A", "brant_cst.measurement"); n != 0 {
		t.Fatalf("%d measurements after refusals", n)
	}
	r, err := c.record("domain.measure", measured())
	c.must(err)
	if r.Status != cstapi.StatusActive || r.RowVersion != 1 {
		t.Fatalf("record %+v", r)
	}
	_, err = c.record("domain.measure", measured())
	c.want(err, kernel.CodeVersionConflict)
	_, err = c.c.CST.RecordMeasurement(c.checker, cstapi.RecordMeasurement{IdempotencyKey: c.key(), MeasurementID: "m.2", ExpectedVersion: 1,
		Data: measured()})
	c.want(err, kernel.CodeSchemaInvalid)
	estimate := measured()
	estimate.Kind, estimate.EstimateReason = cstapi.MeasurementEstimated, "按包装规格估算"
	_, err = c.record("m.estimate", estimate)
	c.must(err)
}

// Python test_costing_lifecycle.py, the measurement commands: record_verified, supersede the exact
// active revision, withdraw the superseded revision 1, which leaves revision 2 active (T02 §13.8);
// withdrawing the active one leaves none and revives nothing.
func TestMeasurementLifecycle(t *testing.T) {
	c := newCostingWorld(t)
	first, err := c.record("measurement.1", measured())
	c.must(err)
	supersede := cstapi.SupersedeMeasurement{IdempotencyKey: c.key(), MeasurementID: "measurement.1", ExpectedVersion: 1, ActiveRevision: 1,
		Data: measured(), Reason: "重新实测"}
	second, err := c.c.CST.SupersedeMeasurement(c.checker, supersede)
	c.must(err)
	replayed(t, second, func(cmd cstapi.SupersedeMeasurement) (cstapi.Receipt, error) {
		return c.c.CST.SupersedeMeasurement(c.checker, cmd)
	}, supersede)
	withdraw := func(expected, target int64) (cstapi.Receipt, error) {
		return c.c.CST.WithdrawMeasurement(c.checker, cstapi.WithdrawMeasurement{IdempotencyKey: c.key(), MeasurementID: "measurement.1",
			ExpectedVersion: expected, TargetRevision: target, Reason: "撤销旧实测", Evidence: proof})
	}
	third, err := withdraw(2, 1)
	c.must(err)
	if first.RowVersion != 1 || second.RowVersion != 2 || third.RowVersion != 3 || third.Status != cstapi.StatusWithdrawn {
		t.Fatalf("receipts %+v %+v %+v", first, second, third)
	}
	v, err := c.c.CST.GetMeasurement(c.checker, "measurement.1")
	c.must(err)
	if v.ActiveRevision != 2 || v.RootVersion != 3 || v.Revisions[0].Status != cstapi.StatusWithdrawn || v.Revisions[0].SupersededAt.IsZero() ||
		v.Revisions[1].Reason != "重新实测" || v.Revisions[0].WithdrawnBy.ID != "checker" || !slices.Equal(v.Revisions[1].LineIDs, []string{"L1"}) {
		t.Fatalf("measurement %+v", v)
	}
	_, err = c.c.CST.SupersedeMeasurement(c.checker, cstapi.SupersedeMeasurement{IdempotencyKey: c.key(), MeasurementID: "measurement.1",
		ExpectedVersion: 3, ActiveRevision: 1, Data: measured(), Reason: "过期引用"})
	if code(err) != cstapi.CodeReferenceStale || path(err) != "/args/active_revision" {
		t.Fatalf("supersede a revision that is not active: %v", err)
	}
	_, err = withdraw(3, 1)
	c.want(err, kernel.CodeInvalidState)
	_, err = withdraw(3, 9)
	c.want(err, kernel.CodeReferenceInvalid)
	_, err = withdraw(2, 2)
	c.want(err, kernel.CodeVersionConflict)

	refs := []cstapi.MeasurementRef{{MeasurementID: "measurement.1", Revision: 1}, {MeasurementID: "measurement.1", Revision: 2}}
	got, err := c.c.CST.ResolveMeasurements(c.checker, refs)
	c.must(err)
	if got[refs[1]].Check("/m") != nil || code(got[refs[0]].Check("/m")) != cstapi.CodeReferenceStale || got[refs[1]].Data.DeclaredVolumeM3 != "1" {
		t.Fatalf("lookups %+v", got)
	}
	byLine, err := c.c.CST.MeasurementsForLines(c.checker, []string{"L1", "L9"})
	c.must(err)
	if len(byLine) != 1 || !byLine[refs[1]].Current() {
		t.Fatalf("by line %+v", byLine)
	}
	_, err = withdraw(3, 2)
	c.must(err)
	if v, _ = c.c.CST.GetMeasurement(c.checker, "measurement.1"); v.ActiveRevision != 0 {
		t.Fatalf("withdrawing the active revision revives nothing: %+v", v)
	}
	if byLine, _ = c.c.CST.MeasurementsForLines(c.checker, []string{"L1"}); len(byLine) != 0 {
		t.Fatalf("by line after withdrawal %+v", byLine)
	}
	_, err = c.c.CST.SupersedeMeasurement(c.checker, cstapi.SupersedeMeasurement{IdempotencyKey: c.key(), MeasurementID: "measurement.1",
		ExpectedVersion: 4, ActiveRevision: 2, Data: measured(), Reason: "无现行版本"})
	c.want(err, cstapi.CodeReferenceStale)
	if events := c.events("company.A", cstapi.Source); len(events) != 2 || events[1].Type != cstapi.EventMeasurementRecorded ||
		events[1].Payload["supersedes_revision"] != float64(1) {
		t.Fatalf("events %+v", events)
	}
}

// T02 §15.2 and WBS S2-03 acceptance provisional 不可冻结: assumptions annotate the context's own
// inputs with their explicit value; the server records who proposed each and keeps it while the
// assumption stays the same; verification never upgrades them, so the revision's results stay
// provisional and it is not freezable.
func TestAssumptionsKeepTheContextProvisional(t *testing.T) {
	c := newCostingWorld(t)
	d := c.data()
	quantity := cstapi.Assumption{AssumptionID: "a.qty", Path: "/context/lines/0/quantity", ValueText: "120", Reason: "客户口头确认数量",
		Evidence: proof}
	price := cstapi.Assumption{AssumptionID: "a.price", Path: "/supplier_offer/offer.1/lines/0/tiers/0/unit_price", ValueText: "5",
		Reason: "按历史价试算", Evidence: proof}
	d.Assumptions = []cstapi.Assumption{quantity}
	_, err := c.saveContext(c.author, "ctx", 0, d)
	c.must(err)
	ai := c.provision(costingGrant(c, "ai", "ai.1"))
	d.Assumptions = []cstapi.Assumption{quantity, price}
	_, err = c.saveContext(ai, "ctx", 1, d)
	c.must(err)
	v := c.context("ctx")
	props := v.Revisions[0].Proposals
	if len(props) != 2 || props[0].ProposedBy.ID != "author" || props[1].ProposedBy.ID != "ai.1" ||
		!slices.Equal(v.Revisions[0].Contributors, []string{"author", "ai.1"}) {
		t.Fatalf("proposals %+v, contributors %v", props, v.Revisions[0].Contributors)
	}
	_, err = c.verify(c.checker, "ctx", 2, 1, 0)
	c.must(err)
	ref := cstapi.ContextRef{ContextID: "ctx", Revision: 1}
	got, err := c.c.CST.ResolveContexts(c.checker, []cstapi.ContextRef{ref})
	c.must(err)
	vc := got[ref]
	if !vc.Current() || !vc.Provisional() || len(vc.Assumptions) != 2 || vc.Assumptions[0].VerificationStatus != cstapi.AssumptionUnverified ||
		vc.Assumptions[1].ProposedBy.ID != "ai.1" {
		t.Fatalf("lookup %+v", vc)
	}
	if err := vc.Freezable("/args/context_ref"); code(err) != cstapi.CodeCostInputNotReady || path(err) != "/args/context_ref/assumptions" {
		t.Fatalf("freezable: %v", err)
	}
	// An assumption states the input's explicit value; it targets supported inputs only, once.
	bad := c.data()
	bad.Assumptions = []cstapi.Assumption{{AssumptionID: "a.qty", Path: "/context/lines/0/quantity", ValueText: "100", Reason: "不一致", Evidence: proof}}
	_, err = c.saveContext(c.author, "ctx.bad", 0, bad)
	c.must(err)
	_, err = c.verify(c.checker, "ctx.bad", 1, 1, 0)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/data/assumptions/0/value_text" {
		t.Fatalf("value_text: %v", err)
	}
	bad.Assumptions[0].Path = "/context/lines/3/quantity"
	bad.Assumptions[0].ValueText = "120"
	_, err = c.saveContext(c.author, "ctx.bad", 1, bad)
	c.must(err)
	_, err = c.verify(c.checker, "ctx.bad", 2, 1, 0)
	c.want(err, kernel.CodeReferenceInvalid)
	bad.Assumptions[0].Path = "/context/calculation_currency_ref"
	_, err = c.saveContext(c.author, "ctx.bad", 2, bad)
	if code(err) != cstapi.CodeDependencyNotSupported || path(err) != "/args/data/assumptions/0/path" {
		t.Fatalf("unsupported path: %v", err)
	}
	bad.Assumptions = []cstapi.Assumption{quantity, quantity}
	_, err = c.saveContext(c.author, "ctx.bad", 2, bad)
	c.want(err, kernel.CodeDuplicateLineID)
}
