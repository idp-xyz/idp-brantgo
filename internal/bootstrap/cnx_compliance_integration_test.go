//go:build integration

package bootstrap_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	"go.idp.xyz/idp-brant/internal/contexts/cnx"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// CNX's trade compliance check (S5 A7, WBS S5-13; tactical model CNX-AGG-TradeComplianceCheck,
// SM-CNX-COMPLIANCE, CNX-INV-001; TRD-INV-023; TD-AT12, ES-11 step 5, SC-09; S5 decisions D-8,
// D-9, E-3). Python V0.50 has no CNX ("—（Python 未实现）"), so no Python test is ported; the
// vectors come from the design.
//
// The world is the order world (draft and active orders of customer.1 r2, whose buyer address hq
// has no country: it was confirmed before PTY recorded countries) with the synthetic CN and UN
// country programme lists of S5 A3b imported (E-3: only uninhabited territories are named; AQ is
// under a synthetic embargo, DE under none). CNX runs on its real adapters to TRD's order
// snapshots, PTY's address countries and reference data's programme lists; only CAT's declaration
// profiles (S5 A5, not merged) come from a fake port.

// cnxCompliancePermissions is what the compliance role template holds for the check, with the
// reads, the evidence registration and the programme import the suite needs.
var cnxCompliancePermissions = []string{"trade_compliance.evaluate", "trade_compliance.decide", "trade_compliance.reopen", "cnx.read",
	"cnx.audit", "evd.read", "source.register", "country_programme.import", "ref.read", "trd.read"}

// cnxProfiles stands in for CAT's DeclarationProfileRef v1 (S5 A5): every specification has a
// verified profile classifying it as not controlled, unless the test says otherwise.
type cnxProfiles struct {
	mu         sync.Mutex
	unverified bool
	controlled bool
}

func (p *cnxProfiles) set(unverified, controlled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unverified, p.controlled = unverified, controlled
}

func (p *cnxProfiles) ResolveProfiles(_ context.Context, specs []cnx.SpecRef) (map[cnx.SpecRef]cnx.Profile, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[cnx.SpecRef]cnx.Profile{}
	for _, s := range specs {
		pr := cnx.Profile{ProfileID: "dp." + s.SkuID, Revision: 1, CodeTableVersion: "2026", Verified: !p.unverified, CommodityCode: "6205200099",
			ExportControl: cnx.NotControlled, ContentHash: strings.Repeat("d", 64)}
		if p.controlled {
			pr.ExportControl, pr.ListReference = cnx.Controlled, "合成两用物项 1A001"
		}
		out[s] = pr
	}
	return out, nil
}

type cnxWorld struct {
	*trdWorld
	cnx                                *cnx.Module
	profiles                           *cnxProfiles
	app                                *bentopg.DB
	compliance, agent, system, outside context.Context
	regulation                         polapi.ConfigRef
}

func newCnxWorld(t *testing.T) *cnxWorld {
	q := newOrderWorld(t)
	w := &cnxWorld{trdWorld: q, profiles: &cnxProfiles{}}
	w.compliance = q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "officer"},
		Permissions: cnxCompliancePermissions, DataScope: "company"})
	w.agent = q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.cnx"},
		Permissions: []string{"trade_compliance.evaluate", "trade_compliance.decide", "trade_compliance.reopen", "cnx.read", "evd.read"},
		DataScope:   "company"})
	w.system = q.provision(bootstrap.Grant{Scope: q.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "system.inbox.cnx"},
		Permissions: []string{"trade_compliance.reopen", "cnx.read"}, DataScope: "company"})
	w.outside = q.provision(bootstrap.Grant{Scope: q.scope("company.B"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "officer.b"},
		Permissions: cnxCompliancePermissions, DataScope: "company"})
	for _, code := range []string{cnProgrammeList, unProgrammeList} {
		cmd := syntheticCountryProgrammes(code, "v1", "2026-11-01")
		cmd.IdempotencyKey = q.key()
		_, err := q.c.REF.ImportCountryProgrammes(w.compliance, cmd)
		q.must(err)
	}
	q.source(w.compliance, "cnx.evidence", "合成提单与客户 PO：卸货港与最终用途（测试数据）")
	cfg, err := q.c.POL.GetConfig(q.author, polapi.KindScreeningRegulation, "screening")
	q.must(err)
	w.regulation = polapi.ConfigRef{Kind: polapi.KindScreeningRegulation, ConfigID: "screening", Revision: cfg.ActiveRevision}
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	q.must(err)
	w.app = db
	db, err = bentopg.NewDB(pgtest.AppPool(t))
	q.must(err)
	w.cnx, err = cnx.New(db, cnx.Upstream{Orders: cnx.TRDOrders(q.c.TRD), Countries: cnx.PTYCountries(q.c.PTY), Profiles: w.profiles,
		Programmes: cnx.REFProgrammes(q.c.REF), Configs: q.c.POL, Sources: q.c.EVD})
	q.must(err)
	return w
}

func (w *cnxWorld) binding(orderID string) cnxapi.OrderBinding {
	v := w.getOrder(orderID)
	return cnxapi.OrderBinding{OrderID: orderID, OrderRevision: v.OrderRevision, OrderHash: v.OrderHash}
}

// evaluateCmd evaluates order at rev under the world's regulation and both synthetic lists, with
// destination entered (with evidence, D-8), the buyer address hq's country entered (PTY has none)
// and the end use stated.
func (w *cnxWorld) evaluateCmd(order string, rev, expected int64, destination string) cnxapi.Evaluate {
	ev := []string{"cnx.evidence"}
	return cnxapi.Evaluate{IdempotencyKey: w.key(), OrderID: order, OrderRevision: rev, ExpectedVersion: expected,
		RegulationID: w.regulation.ConfigID, RegulationRevision: w.regulation.Revision, ProgrammeLists: []string{cnProgrammeList, unProgrammeList},
		Destination:      &cnxapi.Destination{Country: destination, Reason: "提单与客户 PO 载明的卸货国（合成）", EvidenceIDs: ev},
		AddressCountries: []cnxapi.AddressCountry{{PartyID: "customer.1", AddressID: "hq", Country: syntheticClear, EvidenceIDs: ev}},
		EndUse:           &cnxapi.EndUse{Statement: "服装零售（合成）", EvidenceIDs: ev}}
}

func (w *cnxWorld) decideCmd(r cnxapi.CheckReceipt, conclusion string, license bool) cnxapi.Decide {
	cmd := cnxapi.Decide{IdempotencyKey: w.key(), CheckID: r.CheckID, ExpectedVersion: r.RowVersion, EvaluationID: r.EvaluationID,
		Conclusion: conclusion, Reason: "核查完成：目的国、交易方所在国与商品（合成）", EvidenceIDs: []string{"cnx.evidence"}}
	if conclusion == cnxapi.ConclusionClear {
		cmd.LicenseRequired = &license
	}
	return cmd
}

// cleared evaluates order at its current revision for destination DE and concludes clear.
func (w *cnxWorld) cleared(order string) cnxapi.CheckReceipt {
	w.t.Helper()
	b := w.binding(order)
	var expected int64
	if cs, err := w.cnx.OrderChecks(w.compliance, order); err == nil {
		for _, c := range cs {
			if c.OrderRevision == b.OrderRevision {
				expected = c.RowVersion
			}
		}
	}
	e, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(order, b.OrderRevision, expected, syntheticClear))
	w.must(err)
	d, err := w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionClear, false))
	w.must(err)
	return d
}

// inApp runs fn in a transaction of the downstream's own transaction owner in company's scope.
func (w *cnxWorld) inApp(company string, fn func(ctx context.Context, exec bentopg.Executor) error) error {
	return bento.ScopeTransactor{DB: w.app, Scope: w.scope(company)}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := w.app.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return fn(ctx, exec)
	})
}

// complianceGuard calls TradeComplianceGuard v1 for b at at in a downstream transaction; pid
// receives its backend pid and hold runs while the transaction holds the guard's locks.
func (w *cnxWorld) complianceGuard(company string, b cnxapi.OrderBinding, at time.Time, pid chan<- int32, hold func()) cnxapi.TradeComplianceVerdict {
	var v cnxapi.TradeComplianceVerdict
	err := w.inApp(company, func(ctx context.Context, exec bentopg.Executor) error {
		if pid != nil {
			var p int32
			if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&p); err != nil {
				return err
			}
			pid <- p
		}
		var err error
		v, err = cnxapi.ScanTradeComplianceGuard(b, exec.QueryRow(ctx, cnxapi.TradeComplianceGuardSQL, cnxapi.TradeComplianceGuardArgs(b, at)...).Scan)
		if err == nil && hold != nil {
			hold()
		}
		return err
	})
	if err != nil {
		w.t.Errorf("guard: %v", err)
	}
	return v
}

func (w *cnxWorld) wantVerdict(b cnxapi.OrderBinding, status, reason string) cnxapi.TradeComplianceVerdict {
	w.t.Helper()
	v := w.complianceGuard("company.A", b, time.Now(), nil, nil)
	if v.Status != status || v.Reason != reason || (v.Clear != (status == cnxapi.StatusClear)) {
		w.t.Fatalf("guard for %+v: %+v, want %s/%s", b, v, status, reason)
	}
	if err := v.Check("/args/order_id"); (err == nil) != v.Clear || (err != nil && code(err) != cnxapi.CodeTradeComplianceNotClear) {
		w.t.Fatalf("check: %v", err)
	}
	return v
}

// ES-11 step 5, CNX-INV-001, TRD-INV-023 positive: a draft order revision is evaluated before its
// activation and concluded clear by a human compliance officer with the regulation and list
// versions; TradeComplianceGuard v1 then says clear for exactly that revision and hash until the
// conclusion's validity ends. An AI evaluation is a candidate only: with countries missing (D-8) it
// cannot conclude, an AI decision is PERMISSION_DENIED, and a stale evaluation REFERENCE_STALE.
// Replays, reads, events, the audit trail, reconciliation and FORCE RLS behave as for every context.
func TestTradeComplianceEvaluateDecideAndGuard(t *testing.T) {
	w := newCnxWorld(t)
	_, o := w.prepared("quote.1")
	b := w.binding(o.ObjectID)
	w.wantVerdict(b, cnxapi.StatusPending, cnxapi.ReasonNotChecked)

	// The agent evaluates what TRD and PTY say: no consignee address, the buyer's without a country.
	bare := cnxapi.Evaluate{IdempotencyKey: w.key(), OrderID: o.ObjectID, OrderRevision: 1, RegulationID: "screening",
		RegulationRevision: w.regulation.Revision, ProgrammeLists: []string{cnProgrammeList, unProgrammeList}}
	first, err := w.cnx.Evaluate(w.agent, bare)
	w.must(err)
	if first.Status != cnxapi.StatusPending || first.RowVersion != 1 || first.Outcome != cnxapi.OutcomeIncomplete || first.OrderHash != b.OrderHash ||
		first.OrderRevision != 1 || first.DecisionID != "" {
		t.Fatalf("agent evaluation: %+v", first)
	}
	w.wantVerdict(b, cnxapi.StatusPending, cnxapi.ReasonAwaitingDecision)
	_, err = w.cnx.Decide(w.agent, w.decideCmd(first, cnxapi.ConclusionClear, false))
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.cnx.Decide(w.compliance, w.decideCmd(first, cnxapi.ConclusionClear, false))
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/evaluation_id" {
		t.Fatalf("clear on an incomplete evaluation: %s %s", code(err), path(err))
	}
	_, err = w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, 0, syntheticClear))
	w.want(err, kernel.CodeVersionConflict)

	second, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, 1, syntheticClear))
	w.must(err)
	if second.CheckID != first.CheckID || second.RowVersion != 2 || second.Outcome != cnxapi.OutcomeNoMatch || second.EvaluationID == first.EvaluationID {
		t.Fatalf("second evaluation: %+v", second)
	}
	_, err = w.cnx.Decide(w.compliance, cnxapi.Decide{IdempotencyKey: w.key(), CheckID: first.CheckID, ExpectedVersion: 2,
		EvaluationID: first.EvaluationID, Conclusion: cnxapi.ConclusionBlocked, Reason: "旧评估"})
	w.want(err, cnxapi.CodeReferenceStale)

	decide := w.decideCmd(second, cnxapi.ConclusionClear, false)
	d, err := w.cnx.Decide(w.compliance, decide)
	w.must(err)
	if d.Status != cnxapi.StatusClear || d.RowVersion != 3 || d.DecisionID == "" || d.LicenseRequired || d.ValidUntil.IsZero() {
		t.Fatalf("clear: %+v", d)
	}
	replay, err := w.cnx.Decide(w.compliance, decide)
	if err != nil || !replay.Replayed || replay.ActionID != d.ActionID {
		t.Fatalf("replay %+v %v", replay, err)
	}
	v := w.wantVerdict(b, cnxapi.StatusClear, cnxapi.ReasonCleared)
	if v.CheckID != d.CheckID || v.DecisionID != d.DecisionID || v.RegulationID != "screening" || v.RegulationRevision != w.regulation.Revision ||
		!v.ValidUntil.Equal(d.ValidUntil) || v.Evidence() != "clear/cleared/"+d.CheckID+"/"+d.DecisionID+"/false" {
		t.Fatalf("verdict %+v", v)
	}
	if v := w.complianceGuard("company.A", b, d.ValidUntil.Add(time.Second), nil, nil); v.Clear || v.Status != cnxapi.StatusExpired {
		t.Fatalf("after the validity: %+v", v)
	}
	if v := w.complianceGuard("company.A", cnxapi.OrderBinding{OrderID: b.OrderID, OrderRevision: 1, OrderHash: strings.Repeat("0", 64)},
		time.Now(), nil, nil); v.Clear || v.Reason != cnxapi.ReasonOrderChanged {
		t.Fatalf("another order hash: %+v", v)
	}

	view, err := w.cnx.GetCheck(w.compliance, d.CheckID)
	w.must(err)
	if view.State != cnxapi.StatusClear || view.Reason != cnxapi.ReasonCleared || len(view.Evaluations) != 2 || len(view.Decisions) != 1 ||
		view.CurrentEvaluationID != second.EvaluationID || view.OpenedBy.Kind != kernel.ActorAI {
		t.Fatalf("view %+v", view)
	}
	ev := view.Evaluations[1]
	if ev.Destination.Country != syntheticClear || ev.Destination.Source != "entered" || ev.Parties[0].Source != "entered" || len(ev.Programmes) != 2 ||
		ev.Programmes[0].Code != cnProgrammeList || ev.Programmes[0].Version != "v1" || len(ev.Gaps) != 0 || ev.Lines[0].Classification != cnx.NotControlled {
		t.Fatalf("evaluation %+v", ev)
	}
	gaps := []string{}
	for _, g := range view.Evaluations[0].Gaps {
		gaps = append(gaps, g.Kind+":"+g.Ref)
	}
	if strings.Join(gaps, " ") != "destination_unknown: country_unknown:customer.1/hq end_use_missing:" {
		t.Fatalf("agent's gaps %v", gaps)
	}
	if view.Decisions[0].Regulation.RescreenIntervalDays != 30 || view.Decisions[0].DecidedBy.ID != "officer" || len(view.Decisions[0].Evidence) != 1 {
		t.Fatalf("decision %+v", view.Decisions[0])
	}
	checks, err := w.cnx.OrderChecks(w.compliance, o.ObjectID)
	if err != nil || len(checks) != 1 || checks[0].State != cnxapi.StatusClear || checks[0].DecisionID != d.DecisionID {
		t.Fatalf("order checks %+v %v", checks, err)
	}

	events := w.events("company.A", cnxapi.Source)
	if len(events) != 1 || events[0].Type != cnxapi.EventTradeComplianceDecided || events[0].Payload["conclusion"] != "clear" ||
		events[0].Payload["order_hash"] != b.OrderHash || events[0].Payload["license_required"] != false {
		t.Fatalf("events %+v", events)
	}
	audit, err := w.cnx.Audit(w.compliance)
	w.must(err)
	var trail []string
	for _, e := range audit {
		trail = append(trail, e.Command+"/"+e.Outcome+"/"+e.Code)
	}
	for _, want := range []string{"trade_compliance.evaluate/succeeded/", "trade_compliance.decide/rejected/PERMISSION_DENIED",
		"trade_compliance.decide/rejected/INVALID_STATE", "trade_compliance.decide/succeeded/", "trade_compliance.decide/replayed/",
		"trade_compliance.get/read/"} {
		if !strings.Contains(strings.Join(trail, " "), want) {
			t.Fatalf("audit lacks %s: %v", want, trail)
		}
	}
	rec, err := w.cnx.Reconcile(w.compliance, decide)
	if err != nil || rec.State != kernel.ReconcileCommitted || rec.Receipt == nil || rec.Receipt.ActionID != d.ActionID {
		t.Fatalf("reconcile %+v %v", rec, err)
	}
	lost := decide
	lost.IdempotencyKey = w.key()
	if rec, err = w.cnx.Reconcile(w.compliance, lost); err != nil || rec.State != kernel.ReconcileNotCommitted {
		t.Fatalf("not committed %+v %v", rec, err)
	}

	// FORCE RLS: company.B sees neither the check nor a conclusion for the same order binding.
	_, err = w.cnx.GetCheck(w.outside, d.CheckID)
	w.want(err, kernel.CodeNotFound)
	if v := w.complianceGuard("company.B", b, time.Now(), nil, nil); v.Clear || v.Reason != cnxapi.ReasonNotChecked {
		t.Fatalf("company.B guard %+v", v)
	}
	if n := w.count("company.B", "brant_cnx.trade_compliance_check"); n != 0 {
		t.Fatalf("company.B sees %d checks", n)
	}
}

// TD-AT12 (CNX half), UL CNX-EmbargoCheck, SC-09 second order: the destination is under the
// synthetic CN embargo programme; the evaluation finds it (and the export-control programme naming
// the same country, which does not apply to uncontrolled goods); compliance concludes blocked;
// TradeComplianceGuard v1 refuses (TRADE_COMPLIANCE_NOT_CLEAR) — order.activate and FUL's release
// refuse with it once B4 and C1 wire the guard — and nothing but compliance's evaluation of changed
// facts moves the check again: no second decision, no reopening.
func TestTDAT12EmbargoedDestinationIsBlocked(t *testing.T) {
	w := newCnxWorld(t)
	_, o := w.prepared("quote.1")
	b := w.binding(o.ObjectID)
	e, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, 0, syntheticEmbargoed))
	w.must(err)
	if e.Outcome != cnxapi.OutcomeMatch {
		t.Fatalf("%+v", e)
	}
	view, err := w.cnx.GetCheck(w.compliance, e.CheckID)
	w.must(err)
	var found []string
	for _, f := range view.Evaluations[0].Findings {
		found = append(found, f.ProgrammeID+"/"+f.Measure+"/"+f.Country+"/"+strings.Join(f.Roles, "+")+"/"+map[bool]string{true: "applies", false: "n/a"}[f.Applies])
	}
	if strings.Join(found, " ") != "SYN-CN-EMB-1/embargo/AQ/destination/applies SYN-CN-EXP-1/export_control/AQ/destination/n/a" {
		t.Fatalf("findings %v", found)
	}
	blocked, err := w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionBlocked, false))
	w.must(err)
	if blocked.Status != cnxapi.StatusBlocked || !blocked.ValidUntil.IsZero() {
		t.Fatalf("%+v", blocked)
	}
	v := w.wantVerdict(b, cnxapi.StatusBlocked, cnxapi.ReasonBlocked)
	if err := v.Check("/args/order_id"); code(err) != cnxapi.CodeTradeComplianceNotClear || path(err) != "/args/order_id" {
		t.Fatalf("%v", err)
	}
	if v := w.complianceGuard("company.A", b, time.Now().AddDate(5, 0, 0), nil, nil); v.Status != cnxapi.StatusBlocked {
		t.Fatalf("blocked never lapses: %+v", v)
	}
	_, err = w.cnx.Decide(w.compliance, w.decideCmd(blocked, cnxapi.ConclusionClear, false))
	w.want(err, kernel.CodeInvalidState)
	_, err = w.cnx.Reopen(w.compliance, cnxapi.Reopen{IdempotencyKey: w.key(), Cause: cnxapi.CauseExpired, CheckID: blocked.CheckID,
		ExpectedVersion: blocked.RowVersion, Reason: "试图解除"})
	w.want(err, kernel.CodeInvalidState)

	// The facts change (the customer's corrected PO names another destination): evaluate again.
	again, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, blocked.RowVersion, syntheticClear))
	w.must(err)
	if again.Status != cnxapi.StatusPending || again.Outcome != cnxapi.OutcomeNoMatch || again.DecisionID != "" {
		t.Fatalf("%+v", again)
	}
	w.wantVerdict(b, cnxapi.StatusPending, cnxapi.ReasonAwaitingDecision)
}

// CNX-INV-001 "订单变更后旧结论不适用", D-9, PL-14 (TD-AT12's binding): an active order cleared at
// revision 1 is amended to revision 2; the guard refuses revision 2 (order_revised) whatever
// revision 1's conclusion says, which stays bound to revision 1 and its hash; an evaluation of the
// old revision is REFERENCE_STALE; the system opens a pending check for revision 2 (reopen
// order_amended) linked to revision 1's, once; only a new evaluation and decision clear it.
func TestOrderAmendmentMakesTheOldConclusionInapplicable(t *testing.T) {
	w := newCnxWorld(t)
	o := w.activeOrder("quote.2")
	b1 := w.binding(o.ObjectID)
	c1 := w.cleared(o.ObjectID)
	w.wantVerdict(b1, cnxapi.StatusClear, cnxapi.ReasonCleared)

	d := w.approvedAmendment(o.ObjectID, "amendment.1", 1, b1.OrderHash, "90")
	r, err := w.c.TRD.ApplyAmendment(w.reviewer, w.applyCmd(o.ObjectID, "amendment.1", d.RowVersion))
	w.must(err)
	b2 := cnxapi.OrderBinding{OrderID: o.ObjectID, OrderRevision: r.OrderRevision, OrderHash: r.OrderHash}
	if b2.OrderRevision != 2 || b2.OrderHash == b1.OrderHash {
		t.Fatalf("amended %+v", r)
	}
	w.wantVerdict(b2, cnxapi.StatusPending, cnxapi.ReasonOrderRevised)
	w.wantVerdict(b1, cnxapi.StatusClear, cnxapi.ReasonCleared)
	if v := w.complianceGuard("company.A", cnxapi.OrderBinding{OrderID: o.ObjectID, OrderRevision: 1, OrderHash: b2.OrderHash}, time.Now(), nil,
		nil); v.Clear || v.Reason != cnxapi.ReasonOrderChanged {
		t.Fatalf("revision 1 at revision 2's hash: %+v", v)
	}
	_, err = w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, c1.RowVersion, syntheticClear))
	if code(err) != cnxapi.CodeReferenceStale || path(err) != "/args/order_revision" {
		t.Fatalf("evaluating the old revision: %s %s", code(err), path(err))
	}

	open := cnxapi.Reopen{IdempotencyKey: w.key(), Cause: cnxapi.CauseOrderAmended, OrderID: o.ObjectID, OrderRevision: 2,
		Reason: "OrderAmended: revision 2 (PL-14)"}
	agentOpen := open
	agentOpen.IdempotencyKey = w.key()
	_, err = w.cnx.Reopen(w.agent, agentOpen)
	w.want(err, kernel.CodePermissionDenied)
	c2, err := w.cnx.Reopen(w.system, open)
	w.must(err)
	if c2.CheckID == c1.CheckID || c2.Status != cnxapi.StatusPending || c2.RowVersion != 1 || c2.OrderRevision != 2 || c2.EvaluationID != "" {
		t.Fatalf("opened %+v", c2)
	}
	again := open
	again.IdempotencyKey = w.key()
	_, err = w.cnx.Reopen(w.system, again)
	w.want(err, kernel.CodeVersionConflict)
	w.wantVerdict(b2, cnxapi.StatusPending, cnxapi.ReasonNotEvaluated)
	view, err := w.cnx.GetCheck(w.compliance, c2.CheckID)
	w.must(err)
	if view.PreviousCheckID != c1.CheckID || len(view.Reopenings) != 1 || view.Reopenings[0].Cause != cnxapi.CauseOrderAmended ||
		view.OpenedBy.Kind != kernel.ActorSystem {
		t.Fatalf("view %+v", view)
	}
	events := w.events("company.A", cnxapi.Source)
	if last := events[len(events)-1]; last.Type != cnxapi.EventTradeComplianceReopened || last.Payload["cause"] != cnxapi.CauseOrderAmended ||
		last.Payload["previous_check_id"] != c1.CheckID || last.Payload["order_revision"] != float64(2) {
		t.Fatalf("event %+v", last)
	}

	e, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 2, 1, syntheticClear))
	w.must(err)
	_, err = w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionClear, false))
	w.must(err)
	w.wantVerdict(b2, cnxapi.StatusClear, cnxapi.ReasonCleared)
	checks, err := w.cnx.OrderChecks(w.compliance, o.ObjectID)
	if err != nil || len(checks) != 2 || checks[0].OrderRevision != 2 || checks[1].OrderRevision != 1 {
		t.Fatalf("order checks %+v %v", checks, err)
	}
}

// SM-CNX-COMPLIANCE clear → reopen → pending, MD-47 and R50: a new version of a programme list a
// clear conclusion used makes the guard refuse (pending, list_updated) until compliance reopens
// and evaluates again; a new screening_regulation revision reopens a clear conclusion through
// regulation_revised (its own revision does not), and a superseded revision supports no new
// evaluation (POL's CONFIG_NOT_ACTIVE); an unexpired conclusion is not reopened as expired. A list
// the company never imported is a gap, never a clear one; a foreign list is refused (ADR-BR-017: the
// regulation's legal confirmation); a line without a verified declaration profile concludes clear
// never (DECLARATION_PROFILE_UNVERIFIED).
func TestReopeningsAndWhatAClearConclusionNeeds(t *testing.T) {
	w := newCnxWorld(t)
	_, o := w.prepared("quote.1")
	b := w.binding(o.ObjectID)
	c := w.cleared(o.ObjectID)
	reopen := func(ctx context.Context, cause string, expected int64, reg *polapi.ConfigRef) (cnxapi.CheckReceipt, error) {
		cmd := cnxapi.Reopen{IdempotencyKey: w.key(), Cause: cause, CheckID: c.CheckID, ExpectedVersion: expected, Reason: "合规复核"}
		if reg != nil {
			cmd.RegulationID, cmd.RegulationRevision = reg.ConfigID, reg.Revision
		}
		return w.cnx.Reopen(ctx, cmd)
	}
	_, err := reopen(w.compliance, cnxapi.CauseExpired, c.RowVersion, nil)
	w.want(err, kernel.CodeInvalidState)
	_, err = reopen(w.compliance, cnxapi.CauseProgrammeUpdated, c.RowVersion, nil)
	w.want(err, kernel.CodeInvalidState)
	_, err = reopen(w.compliance, cnxapi.CauseRegulationRevised, c.RowVersion, &w.regulation)
	w.want(err, kernel.CodeInvalidState)

	cmd := syntheticCountryProgrammes(cnProgrammeList, "v2", "2026-11-10")
	cmd.IdempotencyKey = w.key()
	_, err = w.c.REF.ImportCountryProgrammes(w.compliance, cmd)
	w.must(err)
	w.wantVerdict(b, cnxapi.StatusPending, cnxapi.ReasonListUpdated)
	r, err := reopen(w.system, cnxapi.CauseProgrammeUpdated, c.RowVersion, nil)
	w.must(err)
	if r.Status != cnxapi.StatusPending || r.EvaluationID != "" || r.DecisionID != "" {
		t.Fatalf("reopened %+v", r)
	}
	w.wantVerdict(b, cnxapi.StatusPending, cnxapi.ReasonNotEvaluated)
	view, err := w.cnx.GetCheck(w.compliance, c.CheckID)
	w.must(err)
	if rs := view.Reopenings; len(rs) != 1 || strings.Join(rs[0].UpdatedLists, ",") != cnProgrammeList || rs[0].DecisionID != c.DecisionID ||
		rs[0].ReopenedBy.Kind != kernel.ActorSystem {
		t.Fatalf("reopenings %+v", rs)
	}

	e, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, r.RowVersion, syntheticClear))
	w.must(err)
	c, err = w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionClear, false))
	w.must(err)
	w.wantVerdict(b, cnxapi.StatusClear, cnxapi.ReasonCleared)

	// A new screening_regulation revision: the old one supports no new evaluation; the conclusion is
	// reopened for it.
	cfg, err := w.c.POL.GetConfig(w.author, polapi.KindScreeningRegulation, "screening")
	w.must(err)
	reg2 := w.configure(w.author, w.reviewer, polapi.KindScreeningRegulation, "screening", cfg.RootVersion, w.win.from, time.Time{},
		polapi.ConfigData{ScreeningRegulation: &polapi.ScreeningRegulation{Lists: []polapi.ScreeningList{cnList}, RescreenIntervalDays: 14}}, true)
	r, err = reopen(w.system, cnxapi.CauseRegulationRevised, c.RowVersion, &reg2)
	w.must(err)
	stale := w.evaluateCmd(o.ObjectID, 1, r.RowVersion, syntheticClear)
	_, err = w.cnx.Evaluate(w.compliance, stale)
	w.want(err, polapi.CodeConfigNotActive)
	w.regulation = reg2
	missing := w.evaluateCmd(o.ObjectID, 1, r.RowVersion, syntheticClear)
	missing.ProgrammeLists = append(missing.ProgrammeLists, "cn.never_imported")
	e, err = w.cnx.Evaluate(w.compliance, missing)
	w.must(err)
	if e.Outcome != cnxapi.OutcomeIncomplete {
		t.Fatalf("a missing list: %+v", e)
	}
	_, err = w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionClear, false))
	w.want(err, kernel.CodeInvalidState)

	foreign := syntheticCountryProgrammes(unProgrammeList, "v1", "2026-11-01")
	foreign.IdempotencyKey, foreign.ListCode, foreign.Jurisdiction = w.key(), "foreign.programmes", refapi.JurisdictionForeign
	_, err = w.c.REF.ImportCountryProgrammes(w.compliance, foreign)
	w.must(err)
	withForeign := w.evaluateCmd(o.ObjectID, 1, e.RowVersion, syntheticClear)
	withForeign.ProgrammeLists = []string{cnProgrammeList, "foreign.programmes"}
	_, err = w.cnx.Evaluate(w.compliance, withForeign)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/programme_lists/1" {
		t.Fatalf("a foreign list: %s %s", code(err), path(err))
	}

	w.profiles.set(true, false)
	e, err = w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, e.RowVersion, syntheticClear))
	w.must(err)
	_, err = w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionClear, false))
	w.want(err, cnxapi.CodeDeclarationProfileUnverified)

	// Controlled goods: the export-control programme naming the destination applies, and compliance
	// states that a licence is required (ExportLicenseGuard, S5-14).
	w.profiles.set(false, true)
	e, err = w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, e.RowVersion, syntheticClear))
	w.must(err)
	if e.Outcome != cnxapi.OutcomeMatch {
		t.Fatalf("controlled goods: %+v", e)
	}
	c, err = w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionClear, true))
	w.must(err)
	v := w.wantVerdict(b, cnxapi.StatusClear, cnxapi.ReasonCleared)
	if !v.LicenseRequired || v.RegulationRevision != reg2.Revision || !c.ValidUntil.Before(time.Now().AddDate(0, 0, 15)) {
		t.Fatalf("licence and validity: %+v %+v", v, c)
	}
}

// TradeComplianceGuard v1 decides every commit order (ADR-BR-004 option B, ADR-BR-011; the S5 plan's
// "decide ∥ activate" on the guard's side): a decision or reopening that committed first is what
// the downstream sees; one that starts while the downstream holds the guard waits until the
// downstream commits, and the downstream acted on what it saw; a programme list import waits for a
// downstream holding the list through the guard, and the next guard call sees the new version.
func TestTradeComplianceGuardDecidesEveryCommitOrder(t *testing.T) {
	w := newCnxWorld(t)
	pool := pgtest.AppPool(t)
	_, o := w.prepared("quote.1")
	b := w.binding(o.ObjectID)

	// While the downstream holds the pending check, the decision waits; the downstream saw pending.
	e, err := w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, 0, syntheticClear))
	w.must(err)
	held := func(fn func() error, wantStatus string) {
		t.Helper()
		pid, held, release := make(chan int32, 1), make(chan struct{}), make(chan struct{})
		seen := make(chan cnxapi.TradeComplianceVerdict, 1)
		go func() {
			seen <- w.complianceGuard("company.A", b, time.Now(), pid, func() { close(held); <-release })
		}()
		p := <-pid
		<-held
		done := make(chan error, 1)
		go func() { done <- fn() }()
		waitBlockedBy(t, pool, p, done)
		close(release)
		if v := <-seen; v.Status != wantStatus {
			t.Fatalf("the downstream saw %+v, want %s", v, wantStatus)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	var d cnxapi.CheckReceipt
	held(func() error {
		var err error
		d, err = w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionClear, false))
		return err
	}, cnxapi.StatusPending)
	// The decision committed first: the downstream sees clear.
	w.wantVerdict(b, cnxapi.StatusClear, cnxapi.ReasonCleared)

	// A list import waits for the downstream holding the clear conclusion's lists.
	held(func() error {
		cmd := syntheticCountryProgrammes(unProgrammeList, "v2", "2026-11-10")
		cmd.IdempotencyKey = w.key()
		_, err := w.c.REF.ImportCountryProgrammes(w.compliance, cmd)
		return err
	}, cnxapi.StatusClear)
	w.wantVerdict(b, cnxapi.StatusPending, cnxapi.ReasonListUpdated)
	r, err := w.cnx.Reopen(w.compliance, cnxapi.Reopen{IdempotencyKey: w.key(), Cause: cnxapi.CauseProgrammeUpdated, CheckID: d.CheckID,
		ExpectedVersion: d.RowVersion, Reason: "名单新版本"})
	w.must(err)
	e, err = w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, r.RowVersion, syntheticClear))
	w.must(err)
	d, err = w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionClear, false))
	w.must(err)

	// A reopening waits for the downstream holding the clear conclusion; the downstream saw clear.
	cfg, err := w.c.POL.GetConfig(w.author, polapi.KindScreeningRegulation, "screening")
	w.must(err)
	reg2 := w.configure(w.author, w.reviewer, polapi.KindScreeningRegulation, "screening", cfg.RootVersion, w.win.from, time.Time{},
		polapi.ConfigData{ScreeningRegulation: &polapi.ScreeningRegulation{Lists: []polapi.ScreeningList{cnList}, RescreenIntervalDays: 30}}, true)
	held(func() error {
		var err error
		r, err = w.cnx.Reopen(w.system, cnxapi.Reopen{IdempotencyKey: w.key(), Cause: cnxapi.CauseRegulationRevised, CheckID: d.CheckID,
			ExpectedVersion: d.RowVersion, RegulationID: reg2.ConfigID, RegulationRevision: reg2.Revision, Reason: "法规新修订"})
		return err
	}, cnxapi.StatusClear)
	// The reopening committed first: the downstream sees pending.
	w.wantVerdict(b, cnxapi.StatusPending, cnxapi.ReasonNotEvaluated)

	// A blocking decision while the downstream holds the pending check: the downstream saw pending,
	// and after the decision commits it sees blocked.
	w.regulation = reg2
	e, err = w.cnx.Evaluate(w.compliance, w.evaluateCmd(o.ObjectID, 1, r.RowVersion, syntheticEmbargoed))
	w.must(err)
	held(func() error {
		_, err := w.cnx.Decide(w.compliance, w.decideCmd(e, cnxapi.ConclusionBlocked, false))
		return err
	}, cnxapi.StatusPending)
	w.wantVerdict(b, cnxapi.StatusBlocked, cnxapi.ReasonBlocked)
}

// The real wiring over HTTP with OIDC access tokens: CNX's routes, the closed command contract and
// the reads; trade_compliance.evaluate answers DEPENDENCY_NOT_SUPPORTED while CAT's declaration
// profiles (S5 A5) are not wired, never a check without classifications.
func TestTradeComplianceOverHTTP(t *testing.T) {
	w := newCnxWorld(t)
	_, o := w.prepared("quote.1")
	c := w.cleared(o.ObjectID)
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)
	officer := &client{t: t, base: api.URL, token: is.token(w.tenant, "company.A", "officer", "human"), n: 300}
	status, out := officer.try("cnx", cnxapi.Contract, cnxapi.CommandEvaluate, map[string]any{"order_id": o.ObjectID, "order_revision": 1,
		"expected_version": 0, "regulation_id": "screening", "regulation_revision": w.regulation.Revision,
		"programme_lists": []string{cnProgrammeList}})
	if status != 422 || out["code"] != cnxapi.CodeDependencyNotSupported {
		t.Fatalf("evaluate without CAT: %d %v", status, out)
	}
	status, out = officer.try("cnx", cnxapi.Contract, cnxapi.CommandEvaluate, map[string]any{"order_id": o.ObjectID, "order_revision": 1,
		"expected_version": 0, "regulation_id": "screening", "regulation_revision": w.regulation.Revision, "programme_lists": []string{}})
	if status != 422 || out["code"] != kernel.CodeSchemaInvalid {
		t.Fatalf("no list: %d %v", status, out)
	}
	status, view := officer.do("GET", "/api/brant/v1/cnx/trade_compliance_checks/"+c.CheckID, nil)
	if status != 200 || view["state"] != "clear" || view["order_id"] != o.ObjectID || len(view["evaluations"].([]any)) != 1 {
		t.Fatalf("check %d %v", status, view)
	}
	status, list := officer.do("GET", "/api/brant/v1/cnx/orders/"+o.ObjectID+"/trade_compliance", nil)
	if checks, _ := list["checks"].([]any); status != 200 || len(checks) != 1 || checks[0].(map[string]any)["state"] != "clear" {
		t.Fatalf("order checks %d %v", status, list)
	}
	status, rec := officer.do("POST", "/api/brant/v1/cnx/commands/trade_compliance.decide/reconcile", map[string]any{"contract": cnxapi.Contract,
		"command": cnxapi.CommandDecide, "idempotency_key": "never.sent", "args": map[string]any{"check_id": c.CheckID, "expected_version": 3,
			"evaluation_id": c.EvaluationID, "conclusion": "blocked", "reason": "x"}})
	if status != 200 || rec["state"] != "not_committed" {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	if status, audit := officer.do("GET", "/api/brant/v1/cnx/audit", nil); status != 200 || len(audit["entries"].([]any)) == 0 {
		t.Fatalf("audit %d %v", status, audit)
	}
}

// cnxOwner runs sql as the schema owner in company.A's scope and commits it.
func (w *cnxWorld) cnxOwner(sql string) error {
	tx, err := pgtest.MigratorPool(w.t).Begin(context.Background())
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true), set_config('app.company_id', 'company.A', true)`,
		w.tenant); err != nil {
		return err
	}
	if _, err = tx.Exec(context.Background(), sql); err != nil {
		return err
	}
	return tx.Commit(context.Background())
}

// CNX-INV-001 and the tactical model's "根 CAS；结论只追加" in the database: brant_app holds no UPDATE
// of evaluations, decisions and list versions, no UPDATE of a check's binding and no DELETE; the
// schema owner bypasses no trigger: a check moves only along SM-CNX-COMPLIANCE by one version with
// its binding fixed, evaluations, decisions and list versions never change and nothing is deleted,
// an evaluation's document is covered by its hash and a decision is human.
func TestTradeComplianceRecordsAreAppendOnly(t *testing.T) {
	w := newCnxWorld(t)
	_, o := w.prepared("quote.1")
	c := w.cleared(o.ObjectID)
	for _, sql := range []string{
		`UPDATE brant_cnx.trade_compliance_evaluation SET outcome = 'no_match'`,
		`UPDATE brant_cnx.trade_compliance_decision SET conclusion = 'clear'`,
		`UPDATE brant_cnx.trade_compliance_list_used SET version = 'v0'`,
		`DELETE FROM brant_cnx.trade_compliance_check`,
		`UPDATE brant_cnx.trade_compliance_check SET order_hash = repeat('0', 64)`,
	} {
		if err := w.exec("company.A", sql); err == nil {
			t.Errorf("brant_app must not run %s", sql)
		}
	}
	for _, x := range []struct{ sql, want string }{
		{`UPDATE brant_cnx.trade_compliance_check SET order_revision = 9, row_version = row_version + 1`, "SM-CNX-COMPLIANCE"},
		{`UPDATE brant_cnx.trade_compliance_check SET status = 'blocked', row_version = row_version + 1`, "SM-CNX-COMPLIANCE"},
		{`UPDATE brant_cnx.trade_compliance_check SET status = 'pending', current_decision_id = NULL, row_version = row_version + 3`,
			"SM-CNX-COMPLIANCE"},
		{`DELETE FROM brant_cnx.trade_compliance_check`, "never deleted"},
		{`UPDATE brant_cnx.trade_compliance_evaluation SET outcome = 'match'`, "never change"},
		{`DELETE FROM brant_cnx.trade_compliance_decision`, "never change"},
		{`UPDATE brant_cnx.trade_compliance_list_used SET version = 'v0'`, "never change"},
		{`INSERT INTO brant_cnx.trade_compliance_evaluation SELECT tenant_id, company_id, 'tce.forged', check_id, seq + 1, order_id,
			order_revision, order_hash, regulation_id, regulation_revision, regulation_hash, outcome, document || ' ', facts_hash, valid_until,
			evaluated_by_kind, evaluated_by_id, authority_epoch, evaluated_at FROM brant_cnx.trade_compliance_evaluation`, "check constraint"},
		{`INSERT INTO brant_cnx.trade_compliance_decision SELECT tenant_id, company_id, 'tcd.forged', check_id, evaluation_id, conclusion,
			reason, license_required, regulation_id, regulation_revision, regulation_hash, valid_until, evidence, 'ai', decided_by_id,
			authority_epoch, decided_at FROM brant_cnx.trade_compliance_decision`, "check constraint"},
	} {
		if err := w.cnxOwner(x.sql); err == nil || !strings.Contains(err.Error(), x.want) {
			t.Errorf("must refuse %s: %v", x.sql, err)
		}
	}
	// What trade_compliance.reopen writes, clear → pending without an evaluation, by one version, is
	// the transition the trigger admits.
	if err := w.cnxOwner(`UPDATE brant_cnx.trade_compliance_check SET status = 'pending', current_decision_id = NULL,
		current_evaluation_id = NULL, row_version = row_version + 1 WHERE check_id = '` + c.CheckID + `'`); err != nil {
		t.Fatalf("clear → pending: %v", err)
	}
}
