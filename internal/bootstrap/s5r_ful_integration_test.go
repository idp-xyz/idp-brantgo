//go:build integration

package bootstrap_test

import (
	"strings"
	"testing"
	"time"

	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5R unit R6 (WBS S5R-06) on PostgreSQL 16: the simulated decisions of 2026-11-13 (E0) on FUL's
// commercial release, documents, insurance, service roles and parties — S5R-D06, D09, D10, D11, D12,
// D28, D29, D35, D36, D40 (tactical model FUL-INV-002, FUL-INV-004, FUL-INV-005, SM-FUL-RELEASE,
// FUL-AGG-MilestonePlan; Context Map R27, R36, R44, R49, R56). Python V0.50 has no fulfilment
// context: the tests cite the register.

// S5R-D09, D28, D29 (FUL-INV-002, SM-FUL-RELEASE, R49, role templates' independence of
// commercial_release.grant_exception) on SC-08's order, 2.00 short of P2:
//   - a system actor holding commercial_release.evaluate is PERMISSION_DENIED (B0 has no system
//     re-evaluation); the documentation clerk evaluates (blocked);
//   - an exception naming no credit policy is SCHEMA_INVALID, one POL does not show
//     REFERENCE_INVALID, a superseded revision POL's CONFIG_NOT_ACTIVE — the policy in force decides;
//   - a validity one second beyond the E-2 policy's 7 days is INVALID_KNOWN_VALUE at
//     /args/valid_until, the reason naming the policy;
//   - a credit manager who evaluated the release (a grant holding both permissions) is
//     INDEPENDENT_REVIEW_REQUIRED; the credit manager who did not grants it for 7 days;
//   - the exception records the policy revision, its hash and the 7 days, and its guard evidence
//     names it; the database refuses an exception granted by one of the release's evaluators.
func TestS5RD09D28D29ExceptionsAreBoundedByTheCreditPolicyAndIndependent(t *testing.T) {
	w := newFulWorld(t)
	w.sc08()
	sys := w.provision(releaseGrant(w, kernel.ActorSystem, "system:inbox.ful.release", "company", fulapi.CommandEvaluateRelease, "ful.read"))
	_, err := w.ful.EvaluateRelease(sys, w.releaseCmd("shipment.sc08", fulapi.ScopeShipment))
	w.want(err, kernel.CodePermissionDenied)
	both := w.provision(releaseGrant(w, kernel.ActorHuman, "manager.eval", "company", fulapi.CommandEvaluateRelease,
		fulapi.CommandGrantReleaseException, "ful.read", "evd.read"))
	r, err := w.ful.EvaluateRelease(both, w.releaseCmd("shipment.sc08", fulapi.ScopeShipment))
	if err != nil || r.Status != fulapi.ReleaseBlocked || r.Code != fulapi.CodeCommercialReleaseBlocked {
		t.Fatalf("the evaluating credit manager %+v %v", r, err)
	}
	r, err = w.evaluateRelease(w.ful, "shipment.sc08", fulapi.ScopeShipment)
	if err != nil || r.Status != fulapi.ReleaseBlocked || r.RowVersion != 2 {
		t.Fatalf("the documentation clerk %+v %v", r, err)
	}
	ex := func(edit func(*fulapi.GrantReleaseException)) fulapi.GrantReleaseException {
		c := fulapi.GrantReleaseException{IdempotencyKey: w.key(), ShipmentID: "shipment.sc08", Scope: fulapi.ScopeShipment, ExpectedVersion: 2,
			StageID: "P2", Currency: "USD", ShortfallMinor: "200", ValidUntil: kernel.FormatTimestamp(time.Now().Add(7*24*time.Hour - time.Minute)),
			Reason: "客户书面承诺 2.00 差额随下批货款支付", EvidenceIDs: []string{"evidence"}, CreditPolicy: w.creditPolicy()}
		if edit != nil {
			edit(&c)
		}
		return c
	}
	revised := w.configure(w.author, w.reviewer, polapi.KindCreditPolicy, w.exceptionPolicy.ConfigID,
		w.rootVersion(polapi.KindCreditPolicy, w.exceptionPolicy.ConfigID), w.win.from, time.Time{}, e2Policy(), true)
	for _, c := range []struct {
		name, code, path string
		edit             func(*fulapi.GrantReleaseException)
	}{
		{"no credit policy", kernel.CodeSchemaInvalid, "/args/credit_policy", func(c *fulapi.GrantReleaseException) { c.CreditPolicy = nil }},
		{"a policy POL does not show", kernel.CodeReferenceInvalid, "/args/credit_policy", func(c *fulapi.GrantReleaseException) {
			c.CreditPolicy = &fulapi.ConfigRef{Kind: fulapi.KindCreditPolicy, ID: "credit.none", Revision: 1}
		}},
		{"a superseded revision", polapi.CodeConfigNotActive, "/args/credit_policy", nil},
		{"beyond the policy's 7 days", kernel.CodeInvalidKnownValue, "/args/valid_until", func(c *fulapi.GrantReleaseException) {
			c.CreditPolicy = &fulapi.ConfigRef{Kind: fulapi.KindCreditPolicy, ID: revised.ConfigID, Revision: revised.Revision}
			c.ValidUntil = kernel.FormatTimestamp(time.Now().Add(7*24*time.Hour + time.Hour))
		}},
		{"the evaluating credit manager", fulapi.CodeIndependentReviewRequired, "/args/scope", nil},
	} {
		ctx := w.manager
		if c.code == fulapi.CodeIndependentReviewRequired {
			ctx = both
		}
		cmd := ex(c.edit)
		if c.code == fulapi.CodeIndependentReviewRequired {
			cmd.CreditPolicy = &fulapi.ConfigRef{Kind: fulapi.KindCreditPolicy, ID: revised.ConfigID, Revision: revised.Revision}
		}
		_, err := w.ful.GrantReleaseException(ctx, cmd)
		if code(err) != c.code || path(err) != c.path {
			t.Errorf("%s: %s at %s (%v)", c.name, code(err), path(err), err)
		}
		if c.name == "beyond the policy's 7 days" && !strings.Contains(reasonOf(err), "decision_validity_days 7") {
			t.Errorf("the reason names the policy: %v", err)
		}
	}
	g, err := w.ful.GrantReleaseException(w.manager, ex(func(c *fulapi.GrantReleaseException) {
		c.CreditPolicy = &fulapi.ConfigRef{Kind: fulapi.KindCreditPolicy, ID: revised.ConfigID, Revision: revised.Revision}
	}))
	if err != nil || g.Status != fulapi.ReleaseGranted || g.RowVersion != 3 {
		t.Fatalf("the independent credit manager %+v %v", g, err)
	}
	v := w.releaseView(g.ReleaseID)
	x := v.Exceptions[0]
	if len(v.Exceptions) != 1 || x.CreditPolicy == nil || *x.CreditPolicy != (fulapi.ConfigRef{Kind: fulapi.KindCreditPolicy, ID: revised.ConfigID,
		Revision: revised.Revision}) || len(x.CreditPolicyHash) != 64 || x.DecisionValidityDays != 7 || x.GrantedBy.ID != "manager.1" ||
		!strings.HasPrefix(releaseGuard(fulapi.EvaluationView{Guards: x.Guards}, "pol:credit_policy:"+revised.ConfigID+"@2"), "in_force/"+x.CreditPolicyHash+
			"/decision_validity_days=7") {
		t.Fatalf("the exception %+v", x)
	}
	// The database keeps the rules for any writer: an evaluator's exception, and one without its policy.
	err = w.world.asOwner("company.A", `UPDATE brant_ful.commercial_release SET status = 'blocked', row_version = row_version + 1,
		evaluations = evaluations + 1 WHERE release_id = '`+g.ReleaseID+`';
		INSERT INTO brant_ful.commercial_release_evaluation (tenant_id, company_id, release_id, seq, release_version, outcome, guard, code, reason,
		guard_entries, guard_entries_sha256, bindings, evaluated_by_kind, evaluated_by_id, evaluate_epoch, evaluated_at)
		SELECT tenant_id, company_id, release_id, evaluations, row_version, 'blocked', 'x', 'X', 'x', '[]', encode(sha256('[]'::bytea), 'hex'), '{}',
		'system', 'system:x', 1, now() FROM brant_ful.commercial_release WHERE release_id = '`+g.ReleaseID+`'`)
	if sqlCode(err) != "23514" || !strings.Contains(err.Error(), "commercial_release_evaluation_actor_s5r") {
		t.Fatalf("a system evaluation inserted after S5R: %v", err)
	}
	err = w.world.asOwner("company.A", exceptionBy(g.ReleaseID, "manager.eval", true))
	if sqlCode(err) != "23514" || !strings.Contains(err.Error(), "S5R-D28") {
		t.Fatalf("the evaluator's exception: %v", err)
	}
	err = w.world.asOwner("company.A", exceptionBy(g.ReleaseID, "manager.2", false))
	if sqlCode(err) != "23514" || !strings.Contains(err.Error(), "commercial_release_exception_credit_policy_s5r") {
		t.Fatalf("an exception without its credit policy: %v", err)
	}
}

// exceptionBy is SQL that blocks granted release id by one more evaluation of the documentation clerk
// and grants it again by an exception of human by, naming the world's credit policy when policy —
// for asOwner, which rolls back.
func exceptionBy(id, by string, policy bool) string {
	columns, values := "", ""
	if policy {
		columns, values = ", credit_policy_id, credit_policy_revision, credit_policy_hash, decision_validity_days", ", 'credit.ful', 2, repeat('a', 64), 7"
	}
	return `UPDATE brant_ful.commercial_release SET status = 'blocked', row_version = row_version + 1, evaluations = evaluations + 1
		WHERE release_id = '` + id + `';
		INSERT INTO brant_ful.commercial_release_evaluation (tenant_id, company_id, release_id, seq, release_version, outcome, guard, code, reason,
		guard_entries, guard_entries_sha256, bindings, evaluated_by_kind, evaluated_by_id, evaluate_epoch, evaluated_at)
		SELECT tenant_id, company_id, release_id, evaluations, row_version, 'blocked', 'x', 'X', 'x', '[]', encode(sha256('[]'::bytea), 'hex'), '{}',
		'human', 'doc.1', 1, now() FROM brant_ful.commercial_release WHERE release_id = '` + id + `';
		UPDATE brant_ful.commercial_release SET status = 'granted', row_version = row_version + 1, exceptions = exceptions + 1
		WHERE release_id = '` + id + `';
		INSERT INTO brant_ful.commercial_release_exception (tenant_id, company_id, release_id, seq, release_version, order_id, order_revision,
		order_hash, stage_id, currency, shortfall_minor, valid_until, reason, evidence_ids, guard_entries, guard_entries_sha256, granted_by_kind,
		granted_by_id, grant_epoch, granted_at` + columns + `)
		SELECT r.tenant_id, r.company_id, r.release_id, r.exceptions, r.row_version, r.order_id, r.order_revision, r.order_hash, 'P2', 'USD', 1,
		now() + interval '1 day', 'x', '{}', '[]', encode(sha256('[]'::bytea), 'hex'), 'human', '` + by + `', 1, now()` + values + `
		FROM brant_ful.commercial_release r WHERE r.release_id = '` + id + `'`
}

// S5R-D10 (FUL-INV-002, R56; R9's TradeComplianceGuard v1 body): a blocked conclusion under
// re-evaluation stays blocked for the documents release of a confirmed shipment — FUL changes
// nothing: after the block, compliance evaluates the facts again (pending, its latest decision
// blocked: blocked_under_review), and the documents release is TRADE_COMPLIANCE_NOT_CLEAR, the
// rechecked conclusion bound as blocked; once compliance concludes clear, it is granted and the
// original B/L is recorded.
func TestS5RD10DocumentsReleaseRefusesABlockedConclusionUnderReview(t *testing.T) {
	w := newFulWorld(t)
	order := w.order.ObjectID
	c := w.compliance()
	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	w.confirmed("shipment.1", "40", d, rel)
	w.issuedBillOfLading(w.ful, "shipment.1")
	first := w.grantedOn(w.ful, "shipment.1", fulapi.ScopeDocuments)
	checks, err := c.cnx.OrderChecks(c.compliance, order)
	w.must(err)
	check := checks[0]
	list := syntheticCountryProgrammes(cnProgrammeList, "v2", "2026-11-10")
	list.IdempotencyKey = w.key()
	_, err = w.c.REF.ImportCountryProgrammes(c.compliance, list)
	w.must(err)
	reopened, err := c.cnx.Reopen(c.compliance, cnxapi.Reopen{IdempotencyKey: w.key(), Cause: cnxapi.CauseProgrammeUpdated, CheckID: check.CheckID,
		ExpectedVersion: check.RowVersion, Reason: "合成国别项目名单新版本"})
	w.must(err)
	evaluated, err := c.cnx.Evaluate(c.compliance, c.evaluateCmd(order, 1, reopened.RowVersion, syntheticEmbargoed))
	w.must(err)
	blocked, err := c.cnx.Decide(c.compliance, c.decideCmd(evaluated, cnxapi.ConclusionBlocked, false))
	w.must(err)
	under, err := c.cnx.Evaluate(c.compliance, c.evaluateCmd(order, 1, blocked.RowVersion, syntheticClear))
	w.must(err)
	if under.Status != cnxapi.StatusPending || under.DecisionID != "" {
		t.Fatalf("evaluated again %+v", under)
	}
	r, err := w.evaluateRelease(w.ful, "shipment.1", fulapi.ScopeDocuments)
	e := w.wantBlocked("blocked under review", r, err, fulapi.GuardTradeCompliance, fulapi.CodeTradeComplianceNotClear, "")
	if tc := e.Bindings.Rechecked.TradeCompliance; len(tc) != 1 || !tc[0].Blocked || tc[0].Status != cnxapi.StatusBlocked ||
		tc[0].Reason != cnxapi.ReasonBlockedUnderReview || tc[0].DecisionID != blocked.DecisionID ||
		guardOf(e.Guards, fulapi.GuardTradeCompliance) != "blocked/blocked_under_review/"+check.CheckID+"/"+blocked.DecisionID+"/false/r1" {
		t.Fatalf("the blocked conclusion under review %+v %+v", e.Bindings.Rechecked, e.Guards)
	}
	_, err = w.recordDocuments("shipment.1", r.RowVersion, fulapi.ReleaseOriginalBL)
	refusedAt(t, "the original B/L under review", err, fulapi.CodeTradeComplianceNotClear, "/args/shipment_id")

	_, err = c.cnx.Decide(c.compliance, c.decideCmd(under, cnxapi.ConclusionClear, false))
	w.must(err)
	g := w.grantedOn(w.ful, "shipment.1", fulapi.ScopeDocuments)
	if g.ReleaseID != first.ReleaseID {
		t.Fatalf("the same release %+v", g)
	}
	if rec, err := w.recordDocuments("shipment.1", g.RowVersion, fulapi.ReleaseOriginalBL); err != nil || rec.Status != fulapi.ReleaseConsumed {
		t.Fatalf("the original B/L once cleared %+v %v", rec, err)
	}
}

// S5R-D11 (FUL-AGG-MilestonePlan node list; UL FUL-MilestonePlan): a shipment's plan names the
// booking request as a node before the confirmed booking — its planned date the deadline before the
// cut-off — and the merchandiser records its actual; the database admits the node type.
func TestS5RD11BookingRequestedIsAMilestoneNode(t *testing.T) {
	w := newFulWorld(t)
	rel, d := w.released("commitment.1", "50"), w.delivered("commitment.1", "50")
	_, err := w.prepare(w.merch, "shipment.1", "50", d, rel)
	w.must(err)
	p, err := w.ful.PlanMilestones(w.merch, fulapi.PlanMilestones{IdempotencyKey: w.key(), PlanID: "plan.s5r", Subject: fulapi.MilestoneSubject{
		Kind: fulapi.SubjectShipment, ID: "shipment.1"}, Nodes: []fulapi.MilestoneNode{
		{NodeID: "N1", NodeType: fulapi.NodeBookingRequested, PlannedOn: "2026-12-10"},
		{NodeID: "N2", NodeType: fulapi.NodeBookingConfirmed, PlannedOn: "2026-12-12", Predecessors: []string{"N1"}}}})
	if err != nil || p.RowVersion != 1 {
		t.Fatalf("plan %+v %v", p, err)
	}
	a, err := w.ful.RecordMilestoneActual(w.merch, fulapi.RecordMilestoneActual{IdempotencyKey: w.key(), PlanID: "plan.s5r", ExpectedVersion: 1,
		NodeID: "N1", OccurredAt: kernel.FormatTimestamp(time.Now().Add(-time.Hour)), EvidenceLevel: fulapi.LevelSupplierClaim,
		Note: "已向货代提交订舱委托（合成）"})
	if err != nil || a.RowVersion != 2 {
		t.Fatalf("actual %+v %v", a, err)
	}
	v, err := w.ful.GetMilestonePlan(w.merch, "plan.s5r")
	w.must(err)
	if len(v.Nodes) != 2 || v.Nodes[0].NodeType != fulapi.NodeBookingRequested || v.Nodes[0].SupplierClaim == nil ||
		v.Nodes[0].HighestLevel != fulapi.LevelSupplierClaim || v.Nodes[1].SupplierClaim != nil {
		t.Fatalf("plan %+v", v)
	}
}

// insuredTerms publishes the next revision of the fixture's trade terms on incoterm stating the
// contract insurance percentage percent ("" states none: 110; S5R-D06, POL trade_terms
// insurance_percent).
func (w *fulInsWorld) insuredTerms(incoterm, percent string) polapi.ConfigRef {
	w.t.Helper()
	return w.configure(w.author, w.reviewer, polapi.KindTradeTerms, w.trade.ConfigID, w.rootVersion(polapi.KindTradeTerms, w.trade.ConfigID),
		w.win.from, time.Time{}, polapi.ConfigData{TradeTerms: &polapi.TradeTerms{Incoterm: incoterm, Edition: polapi.IncotermsEdition2020,
			NamedPlace: "HAMBURG", ResponsibilityNote: "synthetic evidence only", Delivery: polapi.Delivery{Kind: polapi.DeliveryFixedDate, Date: "2027-01-31"},
			InsurancePercent: percent}}, true)
}

// amendedPer is amended with the unit price stated per basis pieces.
func (w *fulInsWorld) amendedPer(o trdapi.OrderReceipt, id, qty, price, basis string, terms polapi.ConfigRef) trdapi.OrderReceipt {
	w.t.Helper()
	v := w.getOrder(o.ObjectID)
	cmd := w.amendCmd(o.ObjectID, id, 0, v.OrderRevision, v.OrderHash, qty)
	cmd.Lines[0].UnitPrice, cmd.Lines[0].PriceBasisQuantity, cmd.Terms.TradeTermsRef = price, basis, tref(terms)
	p, err := w.c.TRD.PrepareAmendment(w.author, cmd)
	w.must(err)
	d := w.decidedAmendment(o.ObjectID, id, p.RowVersion)
	_, err = w.c.TRD.ApplyAmendment(w.reviewer, w.applyCmd(o.ObjectID, id, d.RowVersion))
	w.must(err)
	v = w.getOrder(o.ObjectID)
	o.OrderRevision, o.OrderHash = v.OrderRevision, v.OrderHash
	return o
}

// S5R-D06, D35, D36 (FUL-INV-005, FUL-INV-004, R27, R44, R49) on SIM-02's numbers: the B2 order
// adopted the insured costing; its PO revision (2) is 1,000 PCS at 920 USD per 100 PCS (9,200.00)
// CIF Hamburg on trade terms whose contract insurance percentage is 120. shipment.b2 of 850 PCS is
// prepared on revision 2; the order then moves to FOB at 8.00 (revision 3). The CI prints revision 2 —
// CIF, the price per 100 PCS, 7,820.00 — and the set needs a policy; the contract cover is 120 % of
// 7,820.00 = 9,384.00: a policy of 8,993.00 (115 %) is INSURANCE_COVERAGE_INSUFFICIENT, a stated 115 %
// INVALID_KNOWN_VALUE; insurer.2, whose insurer role PTY verified without a route (D36), insures
// 9,384.00: the record names the contract's 120 % and its trade terms revision, and the premium
// comparison shows the shipment as 85.00 % of the order revision. The set issues.
func TestS5RD06D35D36InsuranceOnTheRevisionTheShipmentBinds(t *testing.T) {
	w := newFulInsWorld(t)
	o := w.insuredOrder("quote.b2")
	cif := w.insuredTerms("CIF", "120")
	o = w.amendedPer(o, "amendment.b2.po", "1000", "920", "100", cif)
	if f := w.snapshots(w.author, o.ObjectID)[o.ObjectID]; f.PayableMinor != "920000" || f.TradeTerms.Incoterm != "CIF" || o.OrderRevision != 2 {
		t.Fatalf("the PO revision %+v", f)
	}
	restore := w.onOrder(o)
	_, err := w.prepare(w.merch, "shipment.b2", "850", w.delivered("commitment.b2", "850"), w.releasedOf("commitment.b2", "1000", "850"))
	w.must(err)
	restore()
	fob := w.insuredTerms("FOB", "")
	if o3 := w.amended(o, "amendment.b2.fob", "1000", "8", fob); o3.OrderRevision != 3 {
		t.Fatalf("revision 3 %+v", o3)
	}
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.ciCmd("docs.b2", "shipment.b2", 0))
	w.must(err)
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.plCmd("docs.b2", "shipment.b2", 1, fulapi.Package{Cartons: 85, CartonFrom: 1,
		Contents: []fulapi.PackageContent{{LineID: "S1", QuantityPerCarton: "10"}}, GrossWeightKg: "5", NetWeightKg: "4.5", VolumeCbm: "0.05"}))
	w.must(err)
	ci := w.rendition("docs.b2", fulapi.DocumentCommercialInvoice, 1, fulapi.LanguageEn)
	if !strings.Contains(ci, "Terms: CIF 2020") || !strings.Contains(ci, "| 850 PCS | price 920 per 100 PCS | net 7820.00 |") ||
		!strings.Contains(ci, "Order: "+o.ObjectID+" r2") {
		t.Fatalf("the CI prints revision 2:\n%s", ci)
	}
	v := w.set("docs.b2")
	if !slicesEqual(paths(v), []string{"/insurance_policy"}) || entryOf(v.Documents[0].Sources, "terms_order_revision") != "r2 "+o.OrderHash {
		t.Fatalf("the set %v %+v", paths(v), v.Documents[0].Sources)
	}

	ed := w.author
	w.staffParty(ed, "insurer.2", []string{"supplier"})
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	_, err = w.c.PTY.VerifyServiceRole(ed, ptyapi.VerifyServiceRole{IdempotencyKey: w.key(), PartyID: "insurer.2", ExpectedVersion: 2,
		ServiceRoleID: "cargo", Role: ptyapi.ServiceInsurer, ServiceCode: "cargo.icc-a", ValidFrom: kernel.FormatTimestamp(from),
		ValidUntil: kernel.FormatTimestamp(from.Add(60 * 24 * time.Hour)), EvidenceIDs: []string{"evidence"}})
	w.must(err)
	insurer := fulapi.ServiceRoleRef{PartyID: "insurer.2", ServiceRoleID: "cargo", Revision: 1}
	_, err = w.ful.RecordInsurance(w.docs, w.policyCmd("docs.b2", "shipment.b2", 2, "8993.00", "12.00"))
	if code(err) != fulapi.CodeInsuranceCoverageInsufficient || path(err) != "/args/insured_amount" ||
		!strings.Contains(reasonOf(err), "8993.00 USD is below 120 % of the CIF commercial invoice") || !strings.Contains(reasonOf(err), ": 9384.00 USD") {
		t.Fatalf("115 %% of the invoice: %v", err)
	}
	lower := w.policyCmd("docs.b2", "shipment.b2", 2, "9384.00", "12.00")
	lower.CoveragePercent = "115"
	_, err = w.ful.RecordInsurance(w.docs, lower)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/coverage_percent" || !strings.Contains(reasonOf(err), "The contract insures 120 %") {
		t.Fatalf("a stated 115 %%: %v", err)
	}
	cmd := w.policyCmd("docs.b2", "shipment.b2", 2, "9384.00", "12.00")
	cmd.Insurer = insurer
	r, err := w.ful.RecordInsurance(w.docs, cmd)
	if err != nil || r.Coverage == nil || r.Coverage.Percent != "120" || r.Coverage.RequiredAmount != "9384.00" || !r.Coverage.Sufficient ||
		r.Costing.ShipmentPercent != "85.00" || r.Costing.ShipmentNet != "7820.00" || r.Costing.OrderNet != "9200.00" || r.Costing.Status != fulapi.PremiumEqual {
		t.Fatalf("the contract's cover %+v %v", r, err)
	}
	ins := w.insurance("docs.b2")
	if c := ins.Current; c == nil || c.Insurer != insurer || c.CoveragePercent != "120" || c.ContractPercent != "120" ||
		c.ContractSource != "trade_terms:"+cif.ConfigID+"@"+strconvI(cif.Revision) {
		t.Fatalf("the policy %+v", ins.Current)
	}
	if v = w.set("docs.b2"); v.Reconciliation.State != fulapi.Reconciled {
		t.Fatalf("covered %v", paths(v))
	}
	if _, err := w.ful.IssueShipmentDocuments(w.docs, fulapi.IssueShipmentDocuments{IdempotencyKey: w.key(), DocumentSetID: "docs.b2",
		ExpectedVersion: 3}); err != nil {
		t.Fatalf("issue %v", err)
	}
}

func slicesEqual(a, b []string) bool {
	return strings.Join(a, "\x00") == strings.Join(b, "\x00") && len(a) == len(b)
}

// entryOf is the value of the entry named name, or "".
func entryOf(es []fulapi.Entry, name string) string {
	for _, e := range es {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// S5R-D12 (FUL-INV-004, SM-FUL-DOCUMENT, MD-40, R56 CertificateResultRef v1) on shipment.1's set
// (CI and PL reconciled): the documentation clerk registers a certificate of origin of the shipment
// for DE in CNX, valid for a few seconds, and records it in the set; CNX's answer is asked again by
// the set's read and by the issue. While it applies the set is reconciled; once it has expired the
// read shows the required difference certificate_applicability at /certificate_of_origin/certificate_ref
// and the issue is SHIPMENT_DOCUMENTS_UNRECONCILED there (an acknowledgement is refused). A new record
// on a certificate valid 30 days resolves it: the set issues and the issue's evidence names CNX's
// answer at issue. On shipment.2's set a certificate of origin record that names no certificate result
// is the difference co_not_registered: the issue refuses it until the clerk acknowledges it.
func TestS5RD12TheCertificateOfOriginIsCheckedAgainAtIssue(t *testing.T) {
	w := newFulDocWorld(t)
	c := w.compliance()
	_, err := w.file(w.docs, "co.file", "co.txt", "text/plain", "合成原产地证扫描件（测试数据）", "")
	w.must(err)
	page := w.source(w.docs, "co.verify", "合成签发机构证书核验页：证书真实有效（测试数据）")
	register := func(number string, until time.Time) cnxapi.CertificateReceipt {
		t.Helper()
		from := time.Now().UTC().Add(-time.Hour)
		r, err := c.cnx.RegisterCertificate(w.docs, cnxapi.RegisterCertificate{IdempotencyKey: w.key(), CertificateKind: cnxapi.CertificateOfOrigin,
			Number: number, Issuer: "合成贸促会（测试）", IssuedOn: from.Format(time.DateOnly), Object: cnxapi.CertificateObject{Kind: cnxapi.ObjectShipment,
				ID: "shipment.1"}, Market: syntheticClear, ValidFrom: kernel.FormatTimestamp(from), ValidUntil: kernel.FormatTimestamp(until),
			FileID: "co.file", Basis: "原件与签发机构核验页逐项核对一致（合成）", EvidenceIDs: []string{page}})
		w.must(err)
		return r
	}
	co := func(set, shipment string, expected int64, number string, cert *fulapi.CertificateRef) fulapi.PrepareShipmentDocument {
		return fulapi.PrepareShipmentDocument{IdempotencyKey: w.key(), DocumentSetID: set, ShipmentID: shipment, ExpectedVersion: expected,
			DocumentKind: fulapi.DocumentCertificateOfOrigin, Certificate: &fulapi.CertificateOfOrigin{Number: number, FileID: "co.file",
				Issuer: "合成贸促会（测试）", IssuedOn: time.Now().UTC().Format(time.DateOnly), CertificateRef: cert}}
	}
	issue := func(set string, expected int64, acks ...fulapi.Acknowledgement) (fulapi.IssueReceipt, error) {
		return w.ful.IssueShipmentDocuments(w.docs, fulapi.IssueShipmentDocuments{IdempotencyKey: w.key(), DocumentSetID: set, ExpectedVersion: expected,
			Acknowledgements: acks})
	}
	w.shipment("shipment.1", "commitment.1", "50")
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.ciCmd("docs.1", "shipment.1", 0))
	w.must(err)
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.plCmd("docs.1", "shipment.1", 1))
	w.must(err)
	until := time.Now().UTC().Add(8 * time.Second)
	short := register("SYN-CO-S5R-1", until)
	_, err = w.ful.PrepareShipmentDocument(w.docs, co("docs.1", "shipment.1", 2, "SYN-CO-S5R-1",
		&fulapi.CertificateRef{CertificateID: short.CertificateID, Version: 1}))
	w.must(err)
	if v := w.set("docs.1"); v.Reconciliation.State != fulapi.Reconciled {
		t.Fatalf("while the certificate applies %v", paths(v))
	}
	time.Sleep(time.Until(until) + time.Second)
	v := w.set("docs.1")
	d := difference(v, "/certificate_of_origin/certificate_ref")
	if len(v.Reconciliation.Differences) != 1 || d == nil || d.Item != "certificate_applicability" || d.Against != "certificate_result" ||
		!strings.HasPrefix(d.Found, fulapi.CodeCertificateNotApplicable+": ") {
		t.Fatalf("expired since the draft %+v", v.Reconciliation.Differences)
	}
	_, err = issue("docs.1", 3, fulapi.Acknowledgement{Path: "/certificate_of_origin/certificate_ref", Reason: "客户同意"})
	refusedAt(t, "acknowledging the expired certificate", err, kernel.CodeInvalidKnownValue, "/args/acknowledgements/0/path")
	_, err = issue("docs.1", 3)
	refusedAt(t, "issuing on the expired certificate", err, fulapi.CodeShipmentDocumentsUnreconciled, "/certificate_of_origin/certificate_ref")

	long := register("SYN-CO-S5R-2", time.Now().UTC().Add(30*24*time.Hour))
	_, err = w.ful.PrepareShipmentDocument(w.docs, co("docs.1", "shipment.1", 3, "SYN-CO-S5R-2",
		&fulapi.CertificateRef{CertificateID: long.CertificateID, Version: 1}))
	w.must(err)
	iss, err := issue("docs.1", 4)
	if err != nil || len(iss.Issued) != 3 {
		t.Fatalf("issued on the valid certificate %+v %v", iss, err)
	}
	v = w.set("docs.1")
	if e := entryOf(v.Issues[0].Entries, "cnx.certificate_result_ref.v1:recheck:v2"); !strings.HasPrefix(e, long.CertificateID+"/v1/") ||
		!strings.Contains(e, "/applicable/") {
		t.Fatalf("the issue's evidence %+v", v.Issues[0].Entries)
	}

	w.shipment("shipment.2", "commitment.2", "30")
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.ciCmd("docs.2", "shipment.2", 0))
	w.must(err)
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.plCmd("docs.2", "shipment.2", 1))
	w.must(err)
	_, err = w.ful.PrepareShipmentDocument(w.docs, co("docs.2", "shipment.2", 2, "CUSTOMER-COPY-1", nil))
	w.must(err)
	v = w.set("docs.2")
	if d := difference(v, "/certificate_of_origin/certificate_ref"); len(v.Reconciliation.Differences) != 1 || d == nil || d.Item != "co_not_registered" {
		t.Fatalf("a record naming no certificate result %+v", v.Reconciliation.Differences)
	}
	_, err = issue("docs.2", 3)
	refusedAt(t, "co_not_registered not acknowledged", err, fulapi.CodeShipmentDocumentsUnreconciled, "/certificate_of_origin/certificate_ref")
	if iss, err := issue("docs.2", 3, fulapi.Acknowledgement{Path: "/certificate_of_origin/certificate_ref", Reason: "客户只要复印件，原产地证由客户自办"}); err != nil ||
		len(iss.Issued) != 3 {
		t.Fatalf("acknowledged %+v %v", iss, err)
	}
}

// S5R-D40 (FUL-INV-004, R44, R36, CNX-INV-002) on the world's order amended to bind notify.1's notify
// address (revision 2): a shipment binds the notify party and a stated end user as parties; an end
// user at a revision PTY no longer has is REFERENCE_STALE, an unknown one REFERENCE_INVALID. The CI
// prints the order's notify party (notify_source order) and a stated other one is a difference
// against the order. The shipment is released and confirmed with both screened clear; once
// compliance blocks notify.1, its documents release is PARTY_SCREENING_NOT_CLEAR naming the notify
// party, and once the end user is blocked as well, a new shipment stating it is
// PARTY_SCREENING_NOT_CLEAR naming the end user.
func TestS5RD40TheNotifyPartyAndTheEndUserAreTheShipmentsParties(t *testing.T) {
	w := newFulDocWorld(t)
	order := w.order.ObjectID
	notify := w.notifyParty()
	w.staffParty(w.author, "enduser.1", []string{ptyapi.RoleCustomer})
	reg := w.compliance().regulation
	for _, id := range []string{"notify.1", "enduser.1"} {
		_, err := w.c.PTY.RecordScreening(w.author, ptyapi.RecordScreening{IdempotencyKey: w.key(), PartyID: id, ExpectedVersion: 0,
			RegulationID: reg.ConfigID, RegulationRevision: reg.Revision})
		w.must(err)
	}
	b := w.orderBinding(order)
	cmd := w.amendCmd(order, "amendment.notify", 0, b.OrderRevision, b.OrderHash, "120")
	cmd.Terms.NotifyPartyRef = &notify
	p, err := w.c.TRD.PrepareAmendment(w.author, cmd)
	w.must(err)
	dec := w.decidedAmendment(order, "amendment.notify", p.RowVersion)
	_, err = w.c.TRD.ApplyAmendment(w.reviewer, w.applyCmd(order, "amendment.notify", dec.RowVersion))
	w.must(err)
	w.amendedReleasable(order)

	rel, d := w.released("commitment.1", "120"), w.delivered("commitment.1", "120")
	prep := func(id, quantity string, user *fulapi.PartyRef) (fulapi.ShipmentReceipt, error) {
		c := w.prepareCmd(id, quantity, d, rel)
		c.EndUserParty = user
		return w.ful.PrepareShipment(w.merch, c)
	}
	_, err = prep("shipment.x", "10", &fulapi.PartyRef{PartyID: "enduser.1", Revision: 2})
	refusedAt(t, "an end user at a revision PTY does not have", err, fulapi.CodeReferenceStale, "/args/end_user_party/revision")
	_, err = prep("shipment.x", "10", &fulapi.PartyRef{PartyID: "enduser.none", Revision: 1})
	refusedAt(t, "an unknown end user", err, kernel.CodeReferenceInvalid, "/args/end_user_party")
	_, err = prep("shipment.n", "40", &fulapi.PartyRef{PartyID: "enduser.1", Revision: 1})
	w.must(err)
	s := w.fulWorld.shipment("shipment.n")
	roles := map[string]fulapi.ShipmentParty{}
	for _, p := range s.Parties {
		roles[p.Role] = p
	}
	if s.Order.OrderRevision != 2 || roles[fulapi.PartyRoleNotify] != (fulapi.ShipmentParty{Role: fulapi.PartyRoleNotify, PartyID: "notify.1",
		PartyRevision: 1, AddressID: "rotterdam"}) || roles[fulapi.PartyRoleEndUser] != (fulapi.ShipmentParty{Role: fulapi.PartyRoleEndUser,
		PartyID: "enduser.1", PartyRevision: 1}) {
		t.Fatalf("the shipment's parties %+v", s.Parties)
	}

	_, err = prep("shipment.c", "10", nil)
	w.must(err)
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.ciCmd("docs.c", "shipment.c", 0))
	w.must(err)
	if ci := w.rendition("docs.c", fulapi.DocumentCommercialInvoice, 1, fulapi.LanguageEn); !strings.Contains(ci, "Notify: notify.1\n合成通知方地址\n鹿特丹") {
		t.Fatalf("the CI prints the order's notify party:\n%s", ci)
	}
	if d := difference(w.set("docs.c"), "/commercial_invoice/notify"); d != nil {
		t.Fatalf("the order's own notify party %+v", d)
	}
	stated := w.ciCmd("docs.c", "shipment.c", 1)
	stated.Particulars.Notify = &fulapi.DocumentParty{Name: "Another Notify BV", Address: "Rotterdam"}
	_, err = w.ful.PrepareShipmentDocument(w.docs, stated)
	w.must(err)
	if d := difference(w.set("docs.c"), "/commercial_invoice/notify"); d == nil || d.Against != "order" || d.Found != "Another Notify BV / Rotterdam" {
		t.Fatalf("a stated notify party other than the order's %+v", d)
	}

	w.granted("shipment.n")
	_, err = w.confirm(w.merch, "shipment.n", 1)
	w.must(err)
	w.issuedBillOfLading(w.ful, "shipment.n")
	docs := w.grantedOn(w.ful, "shipment.n", fulapi.ScopeDocuments)
	screening, err := w.c.PTY.GetScreening(w.author, "notify.1")
	w.must(err)
	_, err = w.c.PTY.BlockParty(w.author, ptyapi.BlockParty{IdempotencyKey: w.key(), PartyID: "notify.1", ExpectedVersion: screening.RowVersion,
		Reason: "合成：通知方列入负面清单"})
	w.must(err)
	r, err := w.evaluateRelease(w.ful, "shipment.n", fulapi.ScopeDocuments)
	if err != nil || r.Status != fulapi.ReleaseBlocked || r.Code != fulapi.CodePartyScreeningNotClear || !strings.Contains(r.Reason, "notify.1 (notify)") {
		t.Fatalf("the notify party blocked %+v %v", r, err)
	}
	_, err = w.recordDocuments("shipment.n", r.RowVersion, fulapi.ReleaseOriginalBL)
	refusedAt(t, "the original B/L to a blocked notify party", err, fulapi.CodePartyScreeningNotClear, "/shipment/parties/"+partyIndex(s, "notify.1"))
	if v := w.releaseView(docs.ReleaseID); v.Consumption != nil {
		t.Fatalf("not released %+v", v)
	}

	screening, err = w.c.PTY.GetScreening(w.author, "enduser.1")
	w.must(err)
	_, err = w.c.PTY.BlockParty(w.author, ptyapi.BlockParty{IdempotencyKey: w.key(), PartyID: "enduser.1", ExpectedVersion: screening.RowVersion,
		Reason: "合成：最终用户列入负面清单"})
	w.must(err)
	_, err = prep("shipment.e", "10", &fulapi.PartyRef{PartyID: "enduser.1", Revision: 1})
	w.must(err)
	r, err = w.evaluateRelease(w.ful, "shipment.e", fulapi.ScopeShipment)
	if err != nil || r.Code != fulapi.CodePartyScreeningNotClear || !strings.Contains(r.Reason, "enduser.1 (end_user)") {
		t.Fatalf("the end user blocked %+v %v", r, err)
	}
}

// partyIndex is the index of party id among s's parties, as text.
func partyIndex(s fulapi.ShipmentView, id string) string {
	for i, p := range s.Parties {
		if p.PartyID == id {
			return strconvI(int64(i))
		}
	}
	return "-1"
}

// S5R-D18 shown by FUL (R5's QualityReleaseRef v1 concession flag; UL QLT-QualityRelease "获批让步的
// 数量可放行并带 concession 标记，单证套和出运可见"): QLT finds 100 of 120 conforming, the customer consents
// to 15 of the 20 nonconforming on concession, and QLT grants rel.c1 of 100 and rel.c2 of the
// conceded 15. A shipment of 100 on rel.c1 and 15 on rel.c2 records the concession on the second
// line only; its shipment read and its document set show it.
func TestS5RD18TheConcessionShowsOnTheShipmentAndItsDocuments(t *testing.T) {
	w := newFulDocWorld(t)
	ref := qltapi.SubjectRef{Kind: qltapi.SubjectCommitmentLine, ID: "commitment.c", LineID: "L1"}
	w.subjects.put(qlt.Subject{Ref: ref, Revision: 1, Hash: hex64("commitment commitment.c"), Status: "issued", SupplierID: "supplier.1",
		OrderID: w.order.ObjectID, OrderRevision: w.order.OrderRevision, OrderHash: w.order.OrderHash, OrderLineID: "L1", SkuID: w.line.Sku.SkuID,
		SpecRevision: w.line.Sku.SpecRevision, UnitID: w.line.UnitRef.ID, UnitRevision: w.line.UnitRef.Revision, Quantity: "120"})
	_, err := w.qlt.RecordInspection(w.qc, qltapi.RecordInspection{IdempotencyKey: w.key(), InspectionID: "insp.c", Subject: ref,
		InspectedQuantity: "120", ConformingQuantity: "100", NonconformingQuantity: "20", Sampling: qltapi.Sampling{Method: qltapi.SamplingFull},
		Standard: qltapi.StandardRef{SourceID: "standard"}, Inspector: qltapi.Inspector{Kind: qltapi.InspectorInternal}, ReportFileID: "report",
		InspectedAt: kernel.FormatTimestamp(time.Now().Add(-time.Hour))})
	w.must(err)
	w.source(w.qc, "consent", "合成客户邮件：同意 15 件轻微色差让步接收。")
	_, err = w.qlt.DecideDisposition(w.qc, qltapi.DecideDisposition{IdempotencyKey: w.key(), InspectionID: "insp.c", ExpectedVersion: 1,
		Reason: "客户同意让步接收 15 件，报废 5 件", EvidenceIDs: []string{"consent"}, Items: []qltapi.DispositionItem{
			{Action: qltapi.DispositionConcession, Quantity: "15"}, {Action: qltapi.DispositionScrap, Quantity: "5"}}})
	w.must(err)
	until := kernel.FormatTimestamp(time.Now().Add(30 * 24 * time.Hour))
	for id, q := range map[string]string{"rel.c1": "100", "rel.c2": "15"} {
		_, err = w.qlt.GrantRelease(w.qc, qltapi.GrantRelease{IdempotencyKey: w.key(), ReleaseID: id, Subject: ref, Quantity: q, ValidUntil: until})
		w.must(err)
	}
	d := w.delivered("commitment.c", "115")
	cmd := w.prepareCmd("shipment.c", "100", d, "rel.c1")
	cmd.Lines = append(cmd.Lines, fulapi.ShipmentLine{LineID: "S2", OrderLineID: "L1", Quantity: "15", SupplierDelivery: d, QualityReleaseID: "rel.c2"})
	_, err = w.ful.PrepareShipment(w.merch, cmd)
	w.must(err)
	s := w.fulWorld.shipment("shipment.c")
	if len(s.Lines) != 2 || s.Lines[0].QualityConcession || !s.Lines[1].QualityConcession {
		t.Fatalf("the shipment's lines %+v", s.Lines)
	}
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.ciCmd("docs.c", "shipment.c", 0))
	w.must(err)
	if v := w.set("docs.c"); len(v.QualityConcessions) != 1 || v.QualityConcessions[0] != "S2" {
		t.Fatalf("the document set %+v", v.QualityConcessions)
	}
}
