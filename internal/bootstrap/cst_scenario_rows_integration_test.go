//go:build integration

package bootstrap_test

import (
	"slices"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The rows of Python test_costing_domains.py and test_costing.py that need the evaluation (the
// "application only" and "partly" rows of the rules' port), without their freeze or source-denial
// parts, which belong to S2-05's second half.

// Python test_costing_domains.py test_late_delivery_can_be_analyzed_but_cannot_be_frozen (the
// evaluation; the freeze refusal is the next step): a supplier date after the required one is
// analyzed at full cost but not comparable.
func TestScenarioLateDeliveryIsAnalyzedButNotComparable(t *testing.T) {
	w := newScenWorld(t)
	late := w.variant(func(l *supapi.ConditionLine) { l.Delivery.Date = "2027-02-01" })
	r, err := w.evaluated(w.scenario(late, "log.1"))
	w.must(err)
	if r.TotalMinor != "66000" || r.ComparisonStatus != cstapi.ComparisonNotComparable || r.VerificationStatus != cstapi.VerificationVerified {
		t.Fatalf("receipt %+v", r)
	}
	v := w.evaluation(r.EvaluationID)
	found := false
	for _, d := range v.Result.Differences {
		found = found || d.Path == "/supplier_offer/"+late+"/lines/0/delivery"
	}
	if !found {
		t.Fatalf("differences %+v", v.Result.Differences)
	}
	b, err := w.c.CST.LatestEvaluation(w.reviewer, w.scenarioOf(r))
	w.must(err)
	if code(b.Freezable("/args")) != cstapi.CodeComparisonBasisMismatch {
		t.Fatalf("freezable %v", b.Freezable("/args"))
	}
}

func (w *scenWorld) scenarioOf(r cstapi.EvaluationReceipt) string { return r.ScenarioID }

// Python test_costing_domains.py test_different_supplier_payment_schedule_is_not_imputed_financing:
// a supplier paid 14 days later than the customer pays is recorded as different_cashflow; the cost
// stays 660.00 USD and comparable, and the excluded cost categories are named.
func TestScenarioDifferentPaymentScheduleIsNotImputedFinancing(t *testing.T) {
	w := newScenWorld(t)
	pay := w.configure(w.author, w.reviewer, polapi.KindPaymentTerms, "payment.14", 0, w.win.from, time.Time{}, polapi.ConfigData{
		PaymentTerms: &polapi.PaymentTerms{Name: "100 before shipment, 14 days", Stages: []polapi.PaymentStage{{StageID: "all", Percent: "100",
			Trigger: polapi.TriggerBeforeShipment, Days: 14}}}}, true)
	r, err := w.evaluated(w.scenario(w.variant(func(l *supapi.ConditionLine) { l.PaymentTermsRef = cref(pay) }), "log.1"))
	w.must(err)
	v := w.evaluation(r.EvaluationID)
	if r.TotalMinor != "66000" || r.ComparisonStatus != cstapi.ComparisonComparable || !slices.Contains(v.Result.ExcludedCostCategories, "imputed_financing_cost") {
		t.Fatalf("result %+v", v.Result)
	}
	if !slices.ContainsFunc(v.Result.CommercialChecks, func(c cstapi.CommercialCheck) bool { return c.Status == "different_cashflow" }) {
		t.Fatalf("checks %+v", v.Result.CommercialChecks)
	}
	same, err := w.evaluated(w.scenario("offer.1", "log.1"))
	w.must(err)
	if !slices.ContainsFunc(w.evaluation(same.EvaluationID).Result.CommercialChecks, func(c cstapi.CommercialCheck) bool {
		return c.Category == "payment_terms" && c.Status == "matched" && c.Path == "/supplier_offer/offer.1/lines/0/payment_terms_ref"
	}) {
		t.Fatal("the same schedule is matched")
	}
}

// leadContext verifies a costing context whose delivery requirement is days working days after the
// verification of the revision anchor names, on a Monday–Friday calendar of Asia/Shanghai.
func (w *scenWorld) leadContext(anchor string, days int) string {
	w.t.Helper()
	cal := w.configure(w.author, w.reviewer, polapi.KindCalendar, w.next("cal"), 0, w.win.from, time.Time{}, polapi.ConfigData{
		Calendar: &polapi.Calendar{TimeZone: "Asia/Shanghai", WorkingWeekdays: []int{1, 2, 3, 4, 5}}}, true)
	d := w.contextData()
	d.ComparisonBasis.Delivery = cstapi.Delivery{Kind: cstapi.DeliveryLeadTime, AnchorRef: &cstapi.BusinessRef{Kind: cstapi.AnchorKind, ID: anchor,
		Revision: 1}, Days: days, DayBasis: cstapi.DayBasisWorking, CalendarRef: cfgPtr(cal), Milestone: cstapi.MilestoneHandover,
		PlaceRef: deham, TimeZone: "Asia/Shanghai"}
	id := w.next("context")
	w.verifiedContext(id, d)
	return id
}

// workingDays is the independent oracle: the anchor's local date in Asia/Shanghai, then days
// Monday–Friday days counted after it.
func workingDays(at time.Time, days int) string {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	y, m, d := at.In(loc).Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	for days > 0 {
		day = day.AddDate(0, 0, 1)
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
			days--
		}
	}
	return day.Format(time.DateOnly)
}

func requirement(v cstapi.EvaluationView) cstapi.DeliveryCheck {
	for _, c := range v.Result.DeliveryChecks {
		if c.Role == "requirement" {
			return c
		}
	}
	return cstapi.DeliveryCheck{}
}

// Python test_costing_domains.py test_working_days_use_real_verification_event_and_reviewed_overrides
// (the evaluation; the freeze and the eligibility after withdrawal are the next step),
// test_zero_lead_days_resolves_to_anchor_local_date and
// test_anchor_history_does_not_reactivate_old_commercial_role. A lead time counts from the real
// verification event of the revision its anchor names, on the POL calendar's working weekdays; 0
// days is the anchor's local date. Brant's supplier offers count lead times from POL order events,
// which have not occurred while costing, so the anchored rule here is the customer's requirement.
// An anchor on a logistics offer counts from its verification whatever its service role became.
func TestScenarioLeadTimeCountsFromTheVerificationEvent(t *testing.T) {
	w := newScenWorld(t)
	offer := w.condition(supapi.KindSupplierOffer, "offer.1")
	verified := offer.Revisions[0].Verification.At
	early := w.variant(func(l *supapi.ConditionLine) { l.Delivery.Date = workingDays(verified, 0) })
	for _, c := range []struct {
		name, anchor string
		days         int
		at           time.Time
	}{
		{"three working days after offer.1's verification", "supplier_offer:offer.1", 3, verified},
		{"zero days is the anchor's local date", "supplier_offer:offer.1", 0, verified},
		{"a logistics offer's verification", "logistics_offer:log.1", 3, w.condition(supapi.KindLogisticsOffer, "log.1").Revisions[0].Verification.At},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := w.leadContext(c.anchor, c.days)
			d := w.scenario(early, "log.1")
			d.ContextRef = &cstapi.ContextRef{ContextID: ctx, Revision: 1}
			r, err := w.evaluated(d)
			w.must(err)
			got := requirement(w.evaluation(r.EvaluationID))
			if got.Status != "resolved" || got.Date != workingDays(c.at, c.days) || r.TotalMinor != "66000" ||
				r.VerificationStatus != cstapi.VerificationVerified || r.ComparisonStatus != cstapi.ComparisonComparable {
				t.Fatalf("requirement %+v, receipt %+v; want %s", got, r, workingDays(c.at, c.days))
			}
		})
	}
}

// Python test_costing_domains.py test_pending_verification_anchor_does_not_invent_a_date (the
// evaluation; the freeze refusal is the next step): an anchor on a revision that was saved but not
// verified has no occurrence, so no date is assumed and the result is provisional.
func TestScenarioPendingAnchorDoesNotInventADate(t *testing.T) {
	w := newScenWorld(t)
	draft := w.next("offer")
	_, err := w.save(w.author, supapi.KindSupplierOffer, draft, 0, w.offerWith(w.line()))
	w.must(err)
	ctx := w.leadContext("supplier_offer:"+draft, 3)
	d := w.scenario("offer.1", "log.1")
	d.ContextRef = &cstapi.ContextRef{ContextID: ctx, Revision: 1}
	r, err := w.evaluated(d)
	w.must(err)
	got := requirement(w.evaluation(r.EvaluationID))
	if got.Status != "unresolved" || got.Date != "" || r.VerificationStatus != cstapi.VerificationProvisional || r.TotalMinor != "66000" {
		t.Fatalf("requirement %+v, receipt %+v", got, r)
	}
	b, err := w.c.CST.LatestEvaluation(w.reviewer, r.ScenarioID)
	w.must(err)
	if code(b.Freezable("/args")) != cstapi.CodeCostInputNotReady {
		t.Fatalf("freezable %v", b.Freezable("/args"))
	}
}

// Python test_costing_domains.py test_anchor_original_source_permission_is_rechecked_for_history
// (history read): the caller of a history read must still read every input the evaluation bound.
// The anchor's revision belongs to another owner; once the author's data scope is narrowed to its
// own records the author still sees its scenario, but the evaluation is PERMISSION_DENIED.
func TestScenarioHistoryRechecksInputPermissions(t *testing.T) {
	w := newScenWorld(t)
	private := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "private.owner"},
		Permissions: allPermissions, DataScope: "company"})
	anchor := w.next("offer")
	_, err := w.save(private, supapi.KindSupplierOffer, anchor, 0, w.offerWith(w.line()))
	w.must(err)
	_, err = w.seal(w.reviewer, supapi.KindSupplierOffer, anchor, 1, 1, 0)
	w.must(err)
	ctx := w.leadContext("supplier_offer:"+anchor, 3)
	d := w.scenario(w.variant(func(l *supapi.ConditionLine) { l.Delivery.Date = "2026-01-01" }), "log.1")
	d.ContextRef = &cstapi.ContextRef{ContextID: ctx, Revision: 1}
	r, err := w.evaluated(d)
	w.must(err)
	_, err = w.c.CST.GetEvaluation(w.author, r.EvaluationID)
	w.must(err)
	w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "editor"},
		Permissions: allPermissions, DataScope: "own", Team: "trade"})
	if _, err = w.c.CST.GetScenario(w.author, r.ScenarioID); err != nil {
		t.Fatalf("the author still sees its scenario: %v", err)
	}
	_, err = w.c.CST.GetEvaluation(w.author, r.EvaluationID)
	w.want(err, kernel.CodePermissionDenied)
}

// Python test_costing_domains.py test_three_line_one_cent_allocation_has_deterministic_remainder
// (the evaluation; the freeze and its re-read are the next step): three lines L3, L2, L1 of 1 PCS at
// 5 USD and a 0.01 USD freight by quantity: 1501 cents, the cent to L1 (CST-INV-004).
func TestScenarioThreeLineOneCentAllocation(t *testing.T) {
	w := newScenWorld(t)
	cmd := inqapi.SaveRfq{IdempotencyKey: w.key(), RfqID: "rfq.3", CustomerID: "customer.1", ContactID: "contact.1", SourceIDs: []string{"src.1"},
		UnresolvedFields: []string{}}
	for _, id := range []string{"L3", "L2", "L1"} {
		cmd.Lines = append(cmd.Lines, inqapi.RfqLine{LineID: id, SkuID: "sku.1", Quantity: "1", UOM: "PCS", SourceID: "src.1", Locator: id})
	}
	_, err := w.c.INQ.SaveRfq(w.author, cmd)
	w.must(err)
	ready, err := w.confirm(w.author, "rfq.3", 1, "src.1")
	w.must(err)
	cd := w.contextData()
	cd.RfqRef = &cstapi.RfqRef{RfqID: "rfq.3", RowVersion: ready.RowVersion}
	cd.Lines = nil
	for _, id := range []string{"L3", "L2", "L1"} {
		cd.Lines = append(cd.Lines, cstapi.ContextLine{LineID: id, SkuID: "sku.1", SkuSpecRevision: w.spec, Quantity: "1", UnitRef: cfg(w.pcs)})
	}
	w.verifiedContext("context.3", cd)
	o := w.offerWith(w.line())
	o.Rfq, o.RfqLineIDs = &supapi.RfqRef{RfqID: "rfq.3", RowVersion: ready.RowVersion}, []string{"L3", "L2", "L1"}
	o.Lines[0].MOQ.Quantity = "1"
	w.verified(supapi.KindSupplierOffer, "offer.3", o)
	log := w.logisticsVariant(func(f *supapi.Fee) {
		f.Billing.Amount = "0.01"
		f.Allocation.TargetLineIDs = []string{"L3", "L2", "L1"}
	})
	d := w.scenario("offer.3", log)
	d.ContextRef = &cstapi.ContextRef{ContextID: "context.3", Revision: 1}
	d.Allocations = nil
	for _, id := range []string{"L3", "L2", "L1"} {
		d.Allocations = append(d.Allocations, cstapi.ScenarioAllocation{RfqLineID: id, PurchaseQuantity: "1", PurchaseUnitRef: cfg(w.pcs),
			Source: cstapi.SupplyLineRef{Kind: cstapi.SupplySupplierOffer, ID: "offer.3", Revision: 1, ConditionLineID: "OL1"}})
	}
	r, err := w.evaluated(d)
	w.must(err)
	if r.TotalMinor != "1501" {
		t.Fatalf("total %s", r.TotalMinor)
	}
	fee := components(w.evaluation(r.EvaluationID))["fee.1"]
	var got []string
	for _, a := range fee.Allocations {
		got = append(got, a.LineID+"="+a.AmountMinor)
	}
	if !slices.Equal(got, []string{"L1=1", "L2=0", "L3=0"}) {
		t.Fatalf("freight %v", got)
	}
}

// Determinism: evaluating the same input again stores byte-identical documents with the same
// hashes, under a new evaluation number; re-saving the selection with another spelling of the same
// quantity (Python test_costing.py
// test_restart_keeps_raw_input_binding_separate_from_business_normalization, the evaluation) moves
// the root version but not the normalized input, so the documents are still the same bytes.
func TestScenarioEvaluationIsDeterministic(t *testing.T) {
	w := newScenWorld(t)
	first, err := w.evaluate(w.reviewer, "scenario.1", 1)
	w.must(err)
	second, err := w.evaluate(w.author, "scenario.1", 1)
	w.must(err)
	d := w.scenario("offer.1", "log.1")
	d.Allocations[0].PurchaseQuantity = "120.000000"
	saved, err := w.saveScenario(w.author, "scenario.1", 1, d)
	w.must(err)
	third, err := w.evaluate(w.reviewer, "scenario.1", saved.RowVersion)
	w.must(err)
	a, b, c := w.evaluation(first.EvaluationID), w.evaluation(second.EvaluationID), w.evaluation(third.EvaluationID)
	for _, e := range []cstapi.EvaluationView{b, c} {
		if e.Input != a.Input || e.Document != a.Document || e.InputHash != a.InputHash || e.ResultHash != a.ResultHash || e.EvaluationID == a.EvaluationID {
			t.Fatalf("evaluation %d differs from evaluation 1:\n%s\n%s", e.Number, e.Document, a.Document)
		}
	}
	if a.Number != 1 || b.Number != 2 || c.Number != 3 || c.ScenarioVersion != 2 || saved.RowVersion != 2 {
		t.Fatalf("numbers %d %d %d, versions %d %d", a.Number, b.Number, c.Number, c.ScenarioVersion, saved.RowVersion)
	}
	var stored []string
	w.must(w.execScan("company.A", `SELECT array_agg(DISTINCT result || normalized_input) FROM brant_cst.cost_evaluation
		WHERE scenario_id = 'scenario.1'`, &stored))
	if len(stored) != 1 {
		t.Fatalf("%d distinct stored documents", len(stored))
	}
}

// Python test_costing.py test_draft_survives_restart_and_replays_original_action and
// test_cross_company_cannot_read_or_replay_same_named_objects, on scenarios: an empty draft is
// saved and read back, its key replays the original action, and another company's same key and ID
// are a different scenario it alone reads.
func TestScenarioDraftReplayAndScope(t *testing.T) {
	w := newScenWorld(t)
	cmd := cstapi.SaveScenario{IdempotencyKey: "test.command", ScenarioID: "same", Data: cstapi.ScenarioData{}}
	first, err := w.c.CST.SaveScenario(w.author, cmd)
	w.must(err)
	v, err := w.c.CST.GetScenario(w.author, "same")
	w.must(err)
	if v.RootVersion != 1 || v.Status != cstapi.ScenarioDraft || v.Data.ContextRef != nil || len(v.Data.Allocations) != 0 || first.Status != cstapi.ScenarioDraft {
		t.Fatalf("view %+v, receipt %+v", v, first)
	}
	again, err := w.c.CST.SaveScenario(w.author, cmd)
	if err != nil || !again.Replayed || again.ActionID != first.ActionID {
		t.Fatalf("replay %+v, %v", again, err)
	}
	outsider := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "editor"},
		Permissions: allPermissions, DataScope: "company"})
	_, err = w.c.CST.GetScenario(outsider, "same")
	w.want(err, kernel.CodeNotFound)
	other, err := w.c.CST.SaveScenario(outsider, cmd)
	if err != nil || other.Replayed || other.ActionID == first.ActionID {
		t.Fatalf("other company %+v, %v", other, err)
	}
	// An empty draft is not evaluated (Python: the scenario schema, COST_INPUT_NOT_READY).
	_, err = w.evaluate(w.reviewer, "same", 1)
	w.want(err, cstapi.CodeCostInputNotReady)
}

// Python test_costing.py test_authorization_and_missing_history_rejections_are_audited, on
// evaluations: a reader without cst.read and a read of a missing evaluation are both refused and
// audited as cost_evaluation.get.
func TestScenarioHistoryRejectionsAreAudited(t *testing.T) {
	w := newScenWorld(t)
	reader := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reader"},
		Permissions: []string{"pol.read"}, DataScope: "company"})
	_, err := w.c.CST.GetEvaluation(reader, "missing")
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.c.CST.GetEvaluation(w.reviewer, "missing")
	w.want(err, kernel.CodeNotFound)
	entries, err := w.c.CST.Audit(w.reviewer)
	w.must(err)
	rejected := 0
	for _, e := range entries {
		if e.Command == "cost_evaluation.get" && e.Outcome == "rejected" {
			rejected++
		}
	}
	if rejected != 2 {
		t.Fatalf("%d audited rejections", rejected)
	}
}

// Python test_costing.py test_source_access_revocation_blocks_successful_replay_and_history: once
// the evaluating reviewer no longer sees the scenario, a replay of its evaluation and the history
// read are refused without field detail.
func TestScenarioAccessRevocationBlocksReplayAndHistory(t *testing.T) {
	w := newScenWorld(t)
	cmd := cstapi.EvaluateScenario{IdempotencyKey: "evaluate.saved", ScenarioID: "scenario.1", ExpectedVersion: 1}
	r, err := w.c.CST.EvaluateScenario(w.reviewer, cmd)
	w.must(err)
	w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer"},
		Permissions: allPermissions, DataScope: "own", Team: "other"})
	_, err = w.c.CST.EvaluateScenario(w.reviewer, cmd)
	w.want(err, kernel.CodeNotFound)
	_, err = w.c.CST.GetEvaluation(w.reviewer, r.EvaluationID)
	w.want(err, kernel.CodeNotFound)
}

// The command order of Python cost_scenario.*: existence, root version, state, completeness, then
// the references; an AI agent saves and evaluates through the same path (command catalog: 人工或
// AI); a supply condition withdrawn or a context revision superseded since the save is refused.
func TestScenarioCommandRules(t *testing.T) {
	w := newScenWorld(t)
	_, err := w.evaluate(w.reviewer, "missing", 1)
	w.want(err, kernel.CodeNotFound)
	_, err = w.evaluate(w.reviewer, "scenario.1", 2)
	w.want(err, kernel.CodeVersionConflict)
	_, err = w.saveScenario(w.author, "scenario.1", 0, w.scenario("offer.1", "log.1"))
	w.want(err, kernel.CodeVersionConflict)
	_, err = w.saveScenario(w.author, "unknown", 0, w.scenario("no.such.offer", "log.1"))
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/data/allocations/0/source" {
		t.Fatalf("unknown offer: %s at %s", code(err), path(err))
	}
	bad := w.scenario("offer.1", "log.1")
	bad.FXRefs = []cstapi.ConfigRef{{Kind: "currency", ID: "USD", Revision: 1}}
	_, err = w.saveScenario(w.author, "bad", 0, bad)
	if code(err) != kernel.CodeSchemaInvalid || path(err) != "/args/data/fx_refs/0/kind" {
		t.Fatalf("fx kind: %s at %s", code(err), path(err))
	}

	ai := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "assistant"},
		Permissions: allPermissions, DataScope: "company", Team: "trade"})
	_, err = w.saveScenario(ai, "by.ai", 0, w.scenario("offer.1", "log.1"))
	w.must(err)
	r, err := w.evaluate(ai, "by.ai", 1)
	if err != nil || r.TotalMinor != "66000" {
		t.Fatalf("AI evaluation %+v, %v", r, err)
	}

	withdrawn := w.variant(func(*supapi.ConditionLine) {})
	_, err = w.saveScenario(w.author, "withdrawn", 0, w.scenario(withdrawn, "log.1"))
	w.must(err)
	_, err = w.end(w.reviewer, supapi.KindSupplierOffer, withdrawn, 2, 1)
	w.must(err)
	_, err = w.evaluate(w.reviewer, "withdrawn", 1)
	if code(err) != supapi.CodeSupplyConditionNotCurrent || path(err) != "/args/data/allocations/0/source" {
		t.Fatalf("withdrawn offer: %s at %s", code(err), path(err))
	}

	_, err = w.c.CST.ReviseContext(w.author, cstapi.ReviseContext{IdempotencyKey: w.key(), ContextID: "context.1", ExpectedVersion: 2,
		BaseRevision: 1, Reason: "修订"})
	w.must(err)
	_, err = w.c.CST.VerifyContext(w.reviewer, cstapi.VerifyContext{IdempotencyKey: w.key(), ContextID: "context.1", ExpectedVersion: 3,
		WorkingRevision: 2, SupersedesRevision: 1, Evidence: proof})
	w.must(err)
	_, err = w.evaluate(w.reviewer, "scenario.1", 1)
	if code(err) != cstapi.CodeReferenceStale || path(err) != "/args/data/context_ref" {
		t.Fatalf("superseded context: %s at %s", code(err), path(err))
	}
}

// The store keeps evaluations immutable and appends them only to the draft they evaluate: the
// application role cannot change or delete one, and the triggers refuse an evaluation of another
// root version and a change of a scenario that is not a transition.
func TestScenarioStoreKeepsEvaluationsImmutable(t *testing.T) {
	w := newScenWorld(t)
	r, err := w.evaluate(w.reviewer, "scenario.1", 1)
	w.must(err)
	for _, sql := range []string{
		"UPDATE brant_cst.cost_evaluation SET result_hash = repeat('0', 64) WHERE evaluation_id = '" + r.EvaluationID + "'",
		"DELETE FROM brant_cst.cost_evaluation WHERE evaluation_id = '" + r.EvaluationID + "'",
		"DELETE FROM brant_cst.cost_scenario WHERE scenario_id = 'scenario.1'",
	} {
		if got := sqlCode(w.exec("company.A", sql)); got != "42501" {
			t.Errorf("%s: %s, want 42501", sql, got)
		}
	}
	for _, sql := range []string{
		// content changed without a new root version
		"UPDATE brant_cst.cost_scenario SET data = '{}'::jsonb WHERE scenario_id = 'scenario.1'",
		// two evaluations counted at once
		"UPDATE brant_cst.cost_scenario SET last_evaluation = last_evaluation + 2 WHERE scenario_id = 'scenario.1'",
		// an evaluation appended to another root version
		`INSERT INTO brant_cst.cost_evaluation SELECT tenant_id, company_id, 'late', scenario_id, 2, 9, evaluated_by_kind, evaluated_by_id,
			authority_epoch, evaluated_at, engine_version, normalization_version, normalized_input, input_hash, result, result_hash,
			verification_status, comparison_status, currency, total_minor FROM brant_cst.cost_evaluation WHERE evaluation_id = '` + r.EvaluationID + "'",
	} {
		if got := sqlCode(w.exec("company.A", sql)); got != "23000" {
			t.Errorf("%s: %s, want 23000", sql, got)
		}
	}
	if got := w.evaluation(r.EvaluationID); got.ResultHash != r.ResultHash {
		t.Fatal("the evaluation changed")
	}
}
