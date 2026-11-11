//go:build integration

package bootstrap_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The snapshot suite (WBS S2-05 second half, S2-06, S2-07) freezes cost scenarios, records source
// attestations and queries adoptability through cst/api against the composed INQ, CAT, POL, SUP, EVD
// and CST on PostgreSQL 16, on the scenario suite's fixture (Python costing_fixture.TradeFixture):
// scenario.1 buys 120 PCS of L1 under offer.1 with the freight of log.1, 660.00 USD. Its evaluation
// rests on two EVD sources: src.1, the evidence of context.1's verification, and evidence, the
// evidence of offer.1, log.1 and their verifications. The author saves, the reviewer evaluates,
// freezes and attests, as in Python. Test names cite the Python V0.50 test they port.

const selection = "同口径成本已核实"

func (w *scenWorld) freezeCmd(r cstapi.EvaluationReceipt, expected int64) cstapi.FreezeSnapshot {
	return cstapi.FreezeSnapshot{IdempotencyKey: w.key(), ScenarioID: r.ScenarioID, ExpectedVersion: expected, EvaluationID: r.EvaluationID,
		ResultHash: r.ResultHash, SelectionReason: selection}
}

func (w *scenWorld) freeze(ctx context.Context, r cstapi.EvaluationReceipt, expected int64) (cstapi.SnapshotReceipt, error) {
	return w.c.CST.FreezeSnapshot(ctx, w.freezeCmd(r, expected))
}

// frozen saves d as a new scenario, evaluates and freezes it as the reviewer.
func (w *scenWorld) frozen(d cstapi.ScenarioData) cstapi.SnapshotReceipt {
	w.t.Helper()
	r, err := w.evaluated(d)
	w.must(err)
	f, err := w.freeze(w.reviewer, r, 1)
	w.must(err)
	return f
}

// attest records verdict on source as the reviewer under a fresh attestation ID, citing evidence.
func (w *scenWorld) attest(ctx context.Context, source, verdict string, evidence ...string) (cstapi.Receipt, error) {
	cmd := cstapi.AttestSource{IdempotencyKey: w.key(), AttestationID: w.next("attestation"), SourceID: source, Verdict: verdict,
		Reason: "新反证明确否认原来源"}
	for _, e := range evidence {
		cmd.Evidence = append(cmd.Evidence, cstapi.Evidence{SourceID: e, Locator: "独立反证"})
	}
	return w.c.CST.AttestSource(ctx, cmd)
}

// counter registers a source the reviewer cites against another and returns its ID.
func (w *scenWorld) counter() string {
	w.t.Helper()
	return w.source(w.reviewer, w.next("counter"), "合成反证资料")
}

func (w *scenWorld) query(id string) cstapi.SnapshotQuery {
	w.t.Helper()
	q, err := w.c.CST.QuerySnapshot(w.reviewer, id)
	w.must(err)
	return q
}

func (w *scenWorld) snapshot(id string) cstapi.SnapshotView {
	w.t.Helper()
	v, err := w.c.CST.GetSnapshot(w.reviewer, id)
	w.must(err)
	return v
}

func (w *scenWorld) scenarioStatus(id string) (string, int64) {
	w.t.Helper()
	v, err := w.c.CST.GetScenario(w.author, id)
	w.must(err)
	return v.Status, v.RootVersion
}

func (w *scenWorld) snapshotsOf(scenarioID string) int {
	w.t.Helper()
	var n int
	w.must(w.execScan("company.A", "SELECT count(*) FROM brant_cst.cost_snapshot WHERE scenario_id = '"+scenarioID+"'", &n))
	return n
}

// reopened returns the CST of a second platform on the same database (Python Core(path) again).
func reopened(t *testing.T) cstapi.SnapshotQueries {
	t.Helper()
	platform, err := bootstrap.Open(context.Background(), bootstrap.Config{PostgresDSN: pgtest.AppDSN(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	c, err := platform.Contexts()
	if err != nil {
		t.Fatal(err)
	}
	return c.CST
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// unchanged reports whether the immutable evidence of two views of a snapshot is the same.
func unchanged(a, b cstapi.SnapshotView) bool {
	return a.Document == b.Document && a.Input == b.Input && a.EvidenceHash == b.EvidenceHash && a.ResultHash == b.ResultHash &&
		a.TotalMinor == b.TotalMinor && a.Result.TotalMinor == b.Result.TotalMinor && len(a.Inputs) == len(b.Inputs)
}

// Python test_costing.py test_actual_database_inputs_evaluate_freeze_and_read_after_restart and T02
// §11 既有基准回归, end to end on PostgreSQL 16 (WBS S2 exit: 660 USD 冻结快照): the 660.00 USD
// evaluation is frozen by the reviewer; the scenario is frozen at version 2, the snapshot keeps the
// evaluation's result and hashes, its components add up to the total, its document hashes to the
// evidence hash, the key replays the original action, CostSnapshotFrozen is in the Outbox, and a
// second platform reads the same snapshot, currently eligible for a quote.
func TestSnapshotBaselineFreezes660USD(t *testing.T) {
	w := newScenWorld(t)
	r, err := w.evaluate(w.reviewer, "scenario.1", 1)
	w.must(err)
	cmd := w.freezeCmd(r, 1)
	f, err := w.c.CST.FreezeSnapshot(w.reviewer, cmd)
	w.must(err)
	if f.Status != cstapi.ScenarioFrozen || f.RowVersion != 2 || f.ObjectID != f.SnapshotID || f.ScenarioID != "scenario.1" || f.Replayed ||
		f.EvaluationID != r.EvaluationID || f.InputScenarioVersion != 1 || f.FrozenScenarioVersion != 2 || f.ResultHash != r.ResultHash ||
		f.InputHash != r.InputHash || f.TotalMinor != "66000" || f.Currency != "USD" || len(f.EvidenceHash) != 64 {
		t.Fatalf("receipt %+v", f)
	}
	again, err := w.c.CST.FreezeSnapshot(w.reviewer, cmd)
	if err != nil || !again.Replayed || again.ActionID != f.ActionID || again.SnapshotID != f.SnapshotID || again.EvidenceHash != f.EvidenceHash {
		t.Fatalf("replay %+v, %v", again, err)
	}
	v, err := reopened(t).GetSnapshot(w.reviewer, f.SnapshotID)
	w.must(err)
	if v.TotalMinor != "66000" || v.Result.TotalMinor != "66000" || v.ResultHash != r.ResultHash || v.EvidenceHash != f.EvidenceHash ||
		sha(v.Document) != v.EvidenceHash || sha(v.Input) != v.InputHash || v.SelectionReason != selection || v.FrozenBy.ID != "reviewer" ||
		v.Context.ContextID != "context.1" || v.Rfq.RfqID != "rfq.1" || v.SupersedesSnapshotID != "" || v.SupersededBySnapshotID != "" {
		t.Fatalf("snapshot %+v", v)
	}
	var sources []string
	for _, s := range v.Sources {
		if s.Status != cstapi.SourceUndisputed {
			t.Errorf("source %+v", s)
		}
		sources = append(sources, s.SourceID)
	}
	if !slices.Equal(sources, []string{"evidence", "src.1"}) {
		t.Fatalf("sources %v", sources)
	}
	var count int
	var total string
	w.must(w.execScan("company.A", `SELECT count(*), sum(amount_minor)::text FROM brant_cst.cost_snapshot_component WHERE snapshot_id = '`+
		f.SnapshotID+`'`, &count, &total))
	if count != 2 || total != "66000" {
		t.Fatalf("%d components adding up to %s", count, total)
	}
	if status, version := w.scenarioStatus("scenario.1"); status != cstapi.ScenarioFrozen || version != 2 {
		t.Fatalf("scenario %s at %d", status, version)
	}
	var frozen []event
	for _, e := range w.events("company.A", cstapi.Source) {
		if e.Type == cstapi.EventCostSnapshotFrozen {
			frozen = append(frozen, e)
		}
	}
	if len(frozen) != 1 || frozen[0].Payload["snapshot_id"] != f.SnapshotID || frozen[0].Payload["result_hash"] != r.ResultHash ||
		frozen[0].Payload["total_minor"] != "66000" || frozen[0].Payload["aggregate_version"] != float64(2) {
		t.Fatalf("events %+v", frozen)
	}
	q := w.query(f.SnapshotID)
	if !q.CurrentlyEligibleForQuote || q.Adoptability.Status != cstapi.AdoptionAdoptable || !unchanged(q.Snapshot, v) ||
		time.Since(q.EligibilityCheckedAt) > time.Minute {
		t.Fatalf("query %+v", q)
	}
	resolved, err := w.c.CST.ResolveSnapshots(w.author, []cstapi.CostSnapshotRef{{SnapshotID: f.SnapshotID, ResultHash: f.ResultHash},
		{SnapshotID: f.SnapshotID, ResultHash: strings.Repeat("0", 64)}, {SnapshotID: "missing"}}, time.Now())
	w.must(err)
	got, ok := resolved[cstapi.CostSnapshotRef{SnapshotID: f.SnapshotID, ResultHash: f.ResultHash}]
	if len(resolved) != 1 || !ok || got.TotalMinor != "66000" || !got.Adoptability.Adoptable() || len(got.Components) != 2 || got.Lines[0].AmountMinor != "66000" {
		t.Fatalf("resolved %+v", resolved)
	}
}

// The freeze's own checks, in Python's order (cost_snapshot.freeze: existence, root version, state,
// the named evaluation, its outcome, its version and hash), each refused without changing the
// scenario or writing a snapshot; the AI half of Python test_costing.py
// test_ai_with_wildcard_cannot_attest_or_freeze.
func TestFreezeRefusesWhatCannotBeFrozen(t *testing.T) {
	w := newScenWorld(t)
	r, err := w.evaluate(w.reviewer, "scenario.1", 1)
	w.must(err)
	other, err := w.evaluated(w.scenario("offer.1", "log.1"))
	w.must(err)
	ai := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "assistant"},
		Permissions: allPermissions, DataScope: "company", Team: "trade"})
	changed, err := w.evaluated(w.scenario("offer.1", "log.1"))
	w.must(err)
	_, err = w.saveScenario(w.author, changed.ScenarioID, 1, w.scenario("offer.1", "log.1"))
	w.must(err)
	for _, c := range []struct {
		name, code, path string
		ctx              context.Context
		cmd              func() cstapi.FreezeSnapshot
	}{
		{"test_ai_with_wildcard_cannot_attest_or_freeze", kernel.CodePermissionDenied, "/", ai, func() cstapi.FreezeSnapshot { return w.freezeCmd(r, 1) }},
		{"missing scenario", kernel.CodeNotFound, "/args/scenario_id", w.reviewer, func() cstapi.FreezeSnapshot {
			c := w.freezeCmd(r, 1)
			c.ScenarioID = "missing"
			return c
		}},
		{"stale expected version", kernel.CodeVersionConflict, "/args/expected_version", w.reviewer, func() cstapi.FreezeSnapshot { return w.freezeCmd(r, 2) }},
		{"another scenario's evaluation", kernel.CodeReferenceInvalid, "/args/evaluation_id", w.reviewer, func() cstapi.FreezeSnapshot {
			c := w.freezeCmd(r, 1)
			c.EvaluationID = other.EvaluationID
			return c
		}},
		{"another result hash", cstapi.CodeReferenceStale, "/args/result_hash", w.reviewer, func() cstapi.FreezeSnapshot {
			c := w.freezeCmd(r, 1)
			c.ResultHash = strings.Repeat("0", 64)
			return c
		}},
		{"the draft changed since the evaluation", cstapi.CodeReferenceStale, "/args/scenario_version", w.reviewer,
			func() cstapi.FreezeSnapshot { return w.freezeCmd(changed, 2) }},
		{"another snapshot to supersede that does not exist", kernel.CodeReferenceInvalid, "/args/supersedes_snapshot_id", w.reviewer,
			func() cstapi.FreezeSnapshot {
				c := w.freezeCmd(r, 1)
				c.SupersedesSnapshotID = "no.such.snapshot"
				return c
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cmd := c.cmd()
			_, err := w.c.CST.FreezeSnapshot(c.ctx, cmd)
			if code(err) != c.code || path(err) != c.path {
				t.Fatalf("got %s at %s (%v), want %s at %s", code(err), path(err), err, c.code, c.path)
			}
		})
	}
	for _, id := range []string{"scenario.1", changed.ScenarioID} {
		if status, _ := w.scenarioStatus(id); status != cstapi.ScenarioDraft || w.snapshotsOf(id) != 0 {
			t.Fatalf("%s is %s with %d snapshots", id, status, w.snapshotsOf(id))
		}
	}
	// The human freezes what the AI could not.
	_, err = w.freeze(w.reviewer, r, 1)
	w.must(err)
}

// CST-INV-001 (依赖当前有效): the freeze recomputes the evaluation on the inputs as they are at its
// instant, so an input withdrawn or superseded since the evaluation refuses it with its owner's
// verdict, and the evaluation stays readable (Python test_costing.py
// test_expired_inputs_block_new_freeze_without_consuming_key, for withdrawal and supersession).
func TestFreezeRechecksEveryInputAtItsInstant(t *testing.T) {
	w := newScenWorld(t)
	withdrawn := w.variant(func(*supapi.ConditionLine) {})
	r, err := w.evaluated(w.scenario(withdrawn, "log.1"))
	w.must(err)
	_, err = w.end(w.reviewer, supapi.KindSupplierOffer, withdrawn, 2, 1)
	w.must(err)
	_, err = w.freeze(w.reviewer, r, 1)
	if code(err) != supapi.CodeSupplyConditionNotCurrent || path(err) != "/args/data/allocations/0/source" {
		t.Fatalf("withdrawn offer: %s at %s", code(err), path(err))
	}
	if v := w.evaluation(r.EvaluationID); v.Result.TotalMinor != "66000" {
		t.Fatal("history stays readable")
	}

	r, err = w.evaluate(w.reviewer, "scenario.1", 1)
	w.must(err)
	_, err = w.c.CST.ReviseContext(w.author, cstapi.ReviseContext{IdempotencyKey: w.key(), ContextID: "context.1", ExpectedVersion: 2,
		BaseRevision: 1, Reason: "重新核实询价范围"})
	w.must(err)
	_, err = w.c.CST.VerifyContext(w.reviewer, cstapi.VerifyContext{IdempotencyKey: w.key(), ContextID: "context.1", ExpectedVersion: 3,
		WorkingRevision: 2, SupersedesRevision: 1, Evidence: proof})
	w.must(err)
	_, err = w.freeze(w.reviewer, r, 1)
	if code(err) != cstapi.CodeReferenceStale || path(err) != "/args/data/context_ref" {
		t.Fatalf("superseded context: %s at %s", code(err), path(err))
	}
	if status, _ := w.scenarioStatus("scenario.1"); status != cstapi.ScenarioDraft {
		t.Fatal("a refused freeze leaves the draft")
	}
}

// Python test_costing.py test_expired_inputs_block_new_freeze_without_consuming_key: an offer whose
// validity ends after the evaluation and before the freeze refuses it (SUP's
// SUPPLY_CONDITION_EXPIRED; Python REFERENCE_STALE); the scenario stays a draft, the evaluation
// stays readable, and the refused key was not consumed: the same key with the arguments of a new
// evaluation freezes, not replayed. Brant's clock is real, so the offer's validity ends seconds
// after it is verified.
func TestExpiredInputsBlockNewFreezeWithoutConsumingKey(t *testing.T) {
	w := newScenWorld(t)
	until := time.Now().Add(8 * time.Second).UTC().Truncate(time.Second)
	o := w.offerWith(w.line())
	o.ValidUntil = kernel.FormatTimestamp(until)
	w.verified(supapi.KindSupplierOffer, "offer.short", o)
	r, err := w.evaluated(w.scenario("offer.short", "log.1"))
	w.must(err)
	if time.Now().After(until) {
		t.Fatal("the evaluation ran after the offer expired; the window is too short for this host")
	}
	time.Sleep(time.Until(until) + 200*time.Millisecond)
	cmd := w.freezeCmd(r, 1)
	_, err = w.c.CST.FreezeSnapshot(w.reviewer, cmd)
	if code(err) != supapi.CodeSupplyConditionExpired || path(err) != "/args/data/allocations/0/source" {
		t.Fatalf("expired offer: %s at %s", code(err), path(err))
	}
	if status, version := w.scenarioStatus(r.ScenarioID); status != cstapi.ScenarioDraft || version != 1 {
		t.Fatalf("scenario %s at %d", status, version)
	}
	if v := w.evaluation(r.EvaluationID); v.Result.TotalMinor != "66000" {
		t.Fatal("history stays readable")
	}
	saved, err := w.saveScenario(w.author, r.ScenarioID, 1, w.scenario("offer.1", "log.1"))
	w.must(err)
	fresh, err := w.evaluate(w.reviewer, r.ScenarioID, saved.RowVersion)
	w.must(err)
	retry := w.freezeCmd(fresh, saved.RowVersion)
	retry.IdempotencyKey = cmd.IdempotencyKey
	f, err := w.c.CST.FreezeSnapshot(w.reviewer, retry)
	if err != nil || f.Replayed || f.TotalMinor != "66000" {
		t.Fatalf("retry %+v, %v", f, err)
	}
}

// CST-INV-002 (同场景唯一快照) and Python test_costing.py
// test_same_key_parallel_freeze_returns_one_original_action and
// test_different_key_parallel_freeze_commits_only_one_snapshot: two freezes with the same key
// return one action, one of them replayed, audited once as succeeded; two with different keys
// commit one snapshot and the other is VERSION_CONFLICT. Afterwards a freeze at the frozen version
// is INVALID_STATE.
func TestFreezeOncePerScenario(t *testing.T) {
	w := newScenWorld(t)
	parallel := func(cmds ...cstapi.FreezeSnapshot) ([]cstapi.SnapshotReceipt, []error) {
		out, errs := make([]cstapi.SnapshotReceipt, len(cmds)), make([]error, len(cmds))
		var wg sync.WaitGroup
		for i, c := range cmds {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out[i], errs[i] = w.c.CST.FreezeSnapshot(w.reviewer, c)
			}()
		}
		wg.Wait()
		return out, errs
	}
	r, err := w.evaluate(w.reviewer, "scenario.1", 1)
	w.must(err)
	cmd := w.freezeCmd(r, 1)
	same, errs := parallel(cmd, cmd)
	if errs[0] != nil || errs[1] != nil || same[0].ActionID != same[1].ActionID || same[0].Replayed == same[1].Replayed ||
		same[0].SnapshotID != same[1].SnapshotID {
		t.Fatalf("same key: %+v %v", same, errs)
	}
	entries, err := w.c.CST.Audit(w.reviewer)
	w.must(err)
	succeeded := 0
	for _, e := range entries {
		if e.Command == cstapi.CommandFreezeSnapshot && e.Outcome == "succeeded" {
			succeeded++
		}
	}
	if succeeded != 1 || w.snapshotsOf("scenario.1") != 1 {
		t.Fatalf("%d succeeded freezes, %d snapshots", succeeded, w.snapshotsOf("scenario.1"))
	}

	d, err := w.evaluated(w.scenario("offer.1", "log.1"))
	w.must(err)
	results, errs := parallel(w.freezeCmd(d, 1), w.freezeCmd(d, 1))
	ok := 0
	for i := range errs {
		switch {
		case errs[i] == nil:
			ok++
		case code(errs[i]) != kernel.CodeVersionConflict:
			t.Fatalf("different keys: %v", errs[i])
		}
	}
	if ok != 1 || w.snapshotsOf(d.ScenarioID) != 1 {
		t.Fatalf("different keys: %+v %v, %d snapshots", results, errs, w.snapshotsOf(d.ScenarioID))
	}
	_, err = w.freeze(w.reviewer, d, 2)
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/scenario_id" {
		t.Fatalf("frozen scenario: %s at %s", code(err), path(err))
	}
	_, err = w.freeze(w.reviewer, d, 1)
	w.want(err, kernel.CodeVersionConflict)
}

// failOutbox makes every Outbox insert of eventType in company.A of w's tenant fail until the
// returned function drops the trigger (Python: a real database trigger injects the storage
// failure, without mocking service internals).
func (w *scenWorld) failOutbox(eventType string) func() {
	w.t.Helper()
	migrator := pgtest.MigratorPool(w.t)
	app := pgtest.AppPool(w.t)
	trigger := strings.ReplaceAll(pgtest.UniqueID(w.t, "fail_"), ".", "_")
	pgtest.Schema(w.t, migrator, app, "brant_t_fail_", fmt.Sprintf(`
CREATE FUNCTION {{schema}}.fail() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'test storage failure'; END $$;
CREATE TRIGGER %s BEFORE INSERT ON bento.outbox FOR EACH ROW
    WHEN (NEW.event_type = '%s' AND NEW.scope = '%s') EXECUTE FUNCTION {{schema}}.fail();`, trigger, eventType, w.scope("company.A").String()))
	return func() {
		if _, err := migrator.Exec(context.Background(), "DROP TRIGGER "+trigger+" ON bento.outbox"); err != nil {
			w.t.Fatal(err)
		}
	}
}

// Python test_costing.py test_outbox_failure_rolls_back_freeze_and_key_can_be_retried: a failing
// Outbox write rolls the freeze back whole (no frozen scenario, no snapshot, no succeeded audit),
// and the same key freezes once the store accepts the write again, not replayed.
func TestOutboxFailureRollsBackFreezeAndKeyCanBeRetried(t *testing.T) {
	w := newScenWorld(t)
	r, err := w.evaluate(w.reviewer, "scenario.1", 1)
	w.must(err)
	cmd := w.freezeCmd(r, 1)
	drop := w.failOutbox(cstapi.EventCostSnapshotFrozen)
	_, err = w.c.CST.FreezeSnapshot(w.reviewer, cmd)
	if err == nil || !strings.Contains(err.Error(), "test storage failure") {
		t.Fatalf("freeze with a failing Outbox: %v", err)
	}
	if _, isRejection := kernel.AsRejection(err); isRejection {
		t.Fatal("a storage failure is not a rejection")
	}
	if status, version := w.scenarioStatus("scenario.1"); status != cstapi.ScenarioDraft || version != 1 || w.snapshotsOf("scenario.1") != 0 {
		t.Fatalf("scenario %s at %d", status, version)
	}
	entries, err := w.c.CST.Audit(w.reviewer)
	w.must(err)
	for _, e := range entries {
		if e.Command == cstapi.CommandFreezeSnapshot && e.Outcome == "succeeded" {
			t.Fatal("the rolled back freeze was audited as succeeded")
		}
	}
	drop()
	f, err := w.c.CST.FreezeSnapshot(w.reviewer, cmd)
	if err != nil || f.Replayed {
		t.Fatalf("retry %+v, %v", f, err)
	}
}

// Python test_costing.py test_source_denial_blocks_new_freeze_but_keeps_history: once a human denies
// a source the evaluation rests on, neither a freeze nor a new evaluation may rest on it
// (REFERENCE_STALE at the source), and the evaluation stays readable.
func TestSourceDenialBlocksNewFreezeButKeepsHistory(t *testing.T) {
	w := newScenWorld(t)
	r, err := w.evaluate(w.reviewer, "scenario.1", 1)
	w.must(err)
	denied, err := w.attest(w.reviewer, "evidence", cstapi.VerdictDenied, w.counter())
	w.must(err)
	if denied.Status != cstapi.VerdictDenied || denied.RowVersion != 1 {
		t.Fatalf("denial %+v", denied)
	}
	_, err = w.freeze(w.reviewer, r, 1)
	if code(err) != cstapi.CodeReferenceStale || path(err) != "/sources/evidence" {
		t.Fatalf("freeze: %s at %s", code(err), path(err))
	}
	_, err = w.evaluate(w.reviewer, "scenario.1", 1)
	if code(err) != cstapi.CodeReferenceStale || path(err) != "/sources/evidence" {
		t.Fatalf("evaluation: %s at %s", code(err), path(err))
	}
	if v := w.evaluation(r.EvaluationID); v.Result.TotalMinor != "66000" {
		t.Fatal("history stays readable")
	}
	var events []event
	for _, e := range w.events("company.A", cstapi.Source) {
		if e.Type == cstapi.EventCostSourceDenied {
			events = append(events, e)
		}
	}
	if len(events) != 1 || events[0].Payload["source_id"] != "evidence" || events[0].Payload["attestation_id"] != denied.ObjectID ||
		len(events[0].Payload["snapshot_ids"].([]any)) != 0 {
		t.Fatalf("events %+v", events)
	}
}

// costing.attest_source (Python source_denial.record and attest_costing_source, T02 §13: 来源否认
// 只允许授权人工登记并提供证据; 不能删除否认事件复活旧结论): human only, the target and the
// evidence must be readable (REFERENCE_INVALID alike for a missing source) and the evidence not
// denied; an ID is used once; a denial is final; an attestation is recorded without an event; a
// denial names the snapshots resting on the source.
func TestAttestSourceRules(t *testing.T) {
	w := newScenWorld(t)
	f, err := w.freeze(w.reviewer, func() cstapi.EvaluationReceipt {
		r, err := w.evaluate(w.reviewer, "scenario.1", 1)
		w.must(err)
		return r
	}(), 1)
	w.must(err)
	ai := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "assistant"},
		Permissions: allPermissions, DataScope: "company", Team: "trade"})
	lawyer := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "lawyer"},
		Permissions: allPermissions, DataScope: "company", Team: "legal"})
	checker := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "checker"},
		Permissions: allPermissions, DataScope: "team", Team: "trade"})
	blind := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "blind"},
		Permissions: []string{cstapi.CommandAttestSource, "cst.read"}, DataScope: "company", Team: "trade"})
	private := w.source(lawyer, "private.src", "合成私有资料")
	counter := w.counter()
	before := w.count("company.A", "brant_cst.source_attestation")
	for _, c := range []struct {
		name, code, path string
		ctx              context.Context
		source           string
		evidence         []string
	}{
		{"test_ai_with_wildcard_cannot_attest_or_freeze", kernel.CodePermissionDenied, "/", ai, "src.1", []string{counter}},
		{"without EVD's read permission", kernel.CodePermissionDenied, "/", blind, "src.1", []string{counter}},
		{"a source that does not exist", kernel.CodeReferenceInvalid, "/args/source_id", w.reviewer, "no.such.source", []string{counter}},
		{"a source the checker does not read", kernel.CodeReferenceInvalid, "/args/source_id", checker, private, []string{"src.1"}},
		{"evidence the checker does not read", kernel.CodeReferenceInvalid, "/args/evidence/0/source_id", checker, "src.1", []string{private}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := w.attest(c.ctx, c.source, cstapi.VerdictDenied, c.evidence...)
			if code(err) != c.code || path(err) != c.path {
				t.Fatalf("got %s at %s (%v), want %s at %s", code(err), path(err), err, c.code, c.path)
			}
		})
	}
	if w.count("company.A", "brant_cst.source_attestation") != before {
		t.Fatal("a refused attestation wrote a record")
	}

	cmd := cstapi.AttestSource{IdempotencyKey: w.key(), AttestationID: "attestation.ok", SourceID: "src.1", Verdict: cstapi.VerdictAttested,
		Reason: "人工核实合成来源真实性", Evidence: []cstapi.Evidence{{SourceID: counter, Locator: "核实记录"}}}
	ok, err := w.c.CST.AttestSource(w.reviewer, cmd)
	if err != nil || ok.Status != cstapi.VerdictAttested || ok.ObjectID != "attestation.ok" || ok.RowVersion != 1 {
		t.Fatalf("attested %+v, %v", ok, err)
	}
	if again, err := w.c.CST.AttestSource(w.reviewer, cmd); err != nil || !again.Replayed || again.ActionID != ok.ActionID {
		t.Fatalf("replay %+v, %v", again, err)
	}
	reused := cmd
	reused.IdempotencyKey = w.key()
	_, err = w.c.CST.AttestSource(w.reviewer, reused)
	if code(err) != kernel.CodeVersionConflict || path(err) != "/args/id" {
		t.Fatalf("reused ID: %s at %s", code(err), path(err))
	}
	view, err := w.c.CST.GetAttestation(w.reviewer, "attestation.ok")
	if err != nil || view.Verdict != cstapi.VerdictAttested || view.SourceID != "src.1" || view.AttestedBy.ID != "reviewer" || len(view.Evidence) != 1 {
		t.Fatalf("attestation %+v, %v", view, err)
	}
	if q := w.query(f.SnapshotID); !q.CurrentlyEligibleForQuote {
		t.Fatal("an attestation does not change adoptability")
	}

	denial, err := w.attest(w.reviewer, "src.1", cstapi.VerdictDenied, counter)
	w.must(err)
	_, err = w.attest(w.reviewer, "src.1", cstapi.VerdictDenied, counter)
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/source_id" {
		t.Fatalf("second denial: %s at %s", code(err), path(err))
	}
	_, err = w.attest(w.reviewer, "src.1", cstapi.VerdictAttested, counter)
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/source_id" {
		t.Fatalf("attesting a denied source: %s at %s", code(err), path(err))
	}
	_, err = w.attest(w.reviewer, counter, cstapi.VerdictDenied, "src.1")
	if code(err) != cstapi.CodeReferenceStale || path(err) != "/args/evidence/0/source_id" {
		t.Fatalf("denied evidence: %s at %s", code(err), path(err))
	}
	for _, e := range w.events("company.A", cstapi.Source) {
		if e.Type != cstapi.EventCostSourceDenied {
			continue
		}
		if e.Payload["attestation_id"] != denial.ObjectID || e.Payload["source_id"] != "src.1" ||
			fmt.Sprint(e.Payload["snapshot_ids"]) != fmt.Sprint([]any{f.SnapshotID}) {
			t.Fatalf("event %+v", e)
		}
	}
	if q := w.query(f.SnapshotID); q.CurrentlyEligibleForQuote || q.Adoptability.Status != cstapi.AdoptionDenied ||
		q.Adoptability.Input != "/sources/src.1" {
		t.Fatalf("after the denial %+v", q.Adoptability)
	}
	_, err = w.c.CST.GetAttestation(checker, denial.ObjectID)
	if err != nil {
		t.Fatalf("a teammate reads the attestation: %v", err)
	}
	_, err = w.c.CST.GetAttestation(lawyer, "missing")
	w.want(err, kernel.CodeNotFound)
}

// newFrozenWorld is Python SnapshotQueryTests.setUp: the fixture evaluated and frozen.
func newFrozenWorld(t *testing.T) (*scenWorld, cstapi.SnapshotReceipt, cstapi.SnapshotView) {
	w := newScenWorld(t)
	r, err := w.evaluate(w.reviewer, "scenario.1", 1)
	w.must(err)
	f, err := w.freeze(w.reviewer, r, 1)
	w.must(err)
	return w, f, w.snapshot(f.SnapshotID)
}

// ineligible checks that the snapshot is not adoptable now, for why at input, and that its history
// is unchanged (Python assert_ineligible_with_history).
func (w *scenWorld) ineligible(id string, before cstapi.SnapshotView, why, input string) {
	w.t.Helper()
	q, err := reopened(w.t).QuerySnapshot(w.reviewer, id)
	w.must(err)
	if q.CurrentlyEligibleForQuote || q.Adoptability.Status != why || q.Adoptability.Code != cstapi.CodeCostSnapshotIneligible ||
		q.Adoptability.Input != input || !unchanged(q.Snapshot, before) || !unchanged(w.snapshot(id), before) {
		w.t.Fatalf("query %+v, want %s at %s with history %v", q.Adoptability, why, input, unchanged(q.Snapshot, before))
	}
}

// guardAt runs AdoptabilityGuard v1 for snapshot id at instant at in company.A.
func (w *scenWorld) guardAt(id, hash string, at time.Time) cstapi.AdoptabilityVerdict {
	w.t.Helper()
	ref := cstapi.CostSnapshotRef{SnapshotID: id, ResultHash: hash}
	var v cstapi.AdoptabilityVerdict
	var known bool
	var status, verdict, input, result, evidence *string
	w.must(w.execScan("company.A", fmt.Sprintf(`SELECT known, status, code, input, result_hash, evidence_hash
		FROM brant_cst.guard_cost_snapshot_adoptable_v1('%s', %s, '%s')`, id, quoted(hash), kernel.FormatTimestamp(at)),
		&known, &status, &verdict, &input, &result, &evidence))
	v, err := cstapi.ScanAdoptabilityGuard(ref, func(dest ...any) error {
		*dest[0].(*bool) = known
		for i, p := range []*string{status, verdict, input, result, evidence} {
			*dest[i+1].(**string) = p
		}
		return nil
	})
	w.must(err)
	return v
}

func quoted(s string) string {
	if s == "" {
		return "NULL"
	}
	return "'" + s + "'"
}

// Python test_costing_query.py SnapshotQueryTests, one subtest per Python test.
func TestSnapshotQuery(t *testing.T) {
	// The query decides at its own instant and never rewrites the snapshot; an instant just before
	// the inputs' validity ends is still eligible.
	t.Run("test_query_uses_current_clock_and_preserves_immutable_snapshot", func(t *testing.T) {
		w, f, before := newFrozenWorld(t)
		q := w.query(f.SnapshotID)
		if !q.CurrentlyEligibleForQuote || q.EligibilityCheckedAt.Before(before.FrozenAt) || !unchanged(q.Snapshot, before) ||
			!unchanged(w.snapshot(f.SnapshotID), before) {
			t.Fatalf("query %+v", q)
		}
		if v := w.guardAt(f.SnapshotID, f.ResultHash, w.win.until.Add(-time.Microsecond)); !v.Adoptable() {
			t.Fatalf("just before the end of the validity: %+v", v)
		}
	})
	t.Run("test_source_denial_changes_eligibility_without_rewriting_history", func(t *testing.T) {
		w, f, before := newFrozenWorld(t)
		_, err := w.attest(w.reviewer, "src.1", cstapi.VerdictDenied, w.counter())
		w.must(err)
		w.ineligible(f.SnapshotID, before, cstapi.AdoptionDenied, "/sources/src.1")
		if v := w.snapshot(f.SnapshotID); v.Sources[1].SourceID != "src.1" || v.Sources[1].Status != cstapi.SourceDenied {
			t.Fatalf("sources %+v", v.Sources)
		}
	})
	// The validity of offer.1 and log.1 ends at win.until, exclusive; log.1 is the first of them in
	// the guard's order (context, kind, ID, revision).
	t.Run("test_expiration_at_exclusive_upper_bound_blocks_adoption", func(t *testing.T) {
		w, f, _ := newFrozenWorld(t)
		v := w.guardAt(f.SnapshotID, f.ResultHash, w.win.until)
		if v.Adoptable() || v.Status != cstapi.AdoptionNotCurrent || v.Input != "/inputs/sup/logistics_offer/log.1/1" ||
			code(v.Check("/args")) != cstapi.CodeCostSnapshotIneligible {
			t.Fatalf("at the exclusive end: %+v", v)
		}
	})
	t.Run("test_withdrawn_input_blocks_adoption", func(t *testing.T) {
		w, f, before := newFrozenWorld(t)
		_, err := w.c.CST.WithdrawContext(w.reviewer, cstapi.WithdrawContext{IdempotencyKey: w.key(), ContextID: "context.1", ExpectedVersion: 2,
			TargetRevision: 1, Reason: "原询价范围已经撤销", Evidence: proof})
		w.must(err)
		w.ineligible(f.SnapshotID, before, cstapi.AdoptionNotCurrent, "/inputs/cst/costing_context/context.1/1")
	})
	t.Run("test_retired_policy_blocks_adoption", func(t *testing.T) {
		w, f, before := newFrozenWorld(t)
		_, err := w.c.POL.Retire(w.reviewer, polapi.Retire{IdempotencyKey: w.key(), Kind: polapi.KindPricingPolicy, ConfigID: "policy.1",
			ExpectedVersion: 2, Reason: "规则停止采用"})
		w.must(err)
		w.ineligible(f.SnapshotID, before, cstapi.AdoptionNotCurrent, "/inputs/pol/pricing_policy/policy.1/1")
	})
	// Losing access is an error, never a business "not eligible": a reviewer narrowed to its own
	// records no longer sees the author's scenario (NOT_FOUND, Brant's answer for an object outside
	// the data scope), and a reader without EVD's read permission is PERMISSION_DENIED.
	t.Run("test_permissions_are_rejected_instead_of_returning_a_business_false", func(t *testing.T) {
		w, f, _ := newFrozenWorld(t)
		noEvidence := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "no.evd"},
			Permissions: slices.DeleteFunc(slices.Clone(allPermissions), func(p string) bool { return p == "evd.read" }), DataScope: "company",
			Team: "trade"})
		_, err := w.c.CST.QuerySnapshot(noEvidence, f.SnapshotID)
		w.want(err, kernel.CodePermissionDenied)
		w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer"},
			Permissions: allPermissions, DataScope: "own", Team: "other"})
		_, err = w.c.CST.QuerySnapshot(w.reviewer, f.SnapshotID)
		w.want(err, kernel.CodeNotFound)
	})
	t.Run("test_evaluation_cannot_be_queried_as_a_frozen_snapshot", func(t *testing.T) {
		w, f, _ := newFrozenWorld(t)
		_, err := w.c.CST.QuerySnapshot(w.reviewer, f.EvaluationID)
		w.want(err, kernel.CodeNotFound)
	})
	t.Run("test_unpublished_revision_does_not_remove_current_eligibility", func(t *testing.T) {
		w, f, before := newFrozenWorld(t)
		_, err := w.c.CST.ReviseContext(w.author, cstapi.ReviseContext{IdempotencyKey: w.key(), ContextID: "context.1", ExpectedVersion: 2,
			BaseRevision: 1, Reason: "新稿尚未核实，旧版继续有效"})
		w.must(err)
		if q := w.query(f.SnapshotID); !q.CurrentlyEligibleForQuote || !unchanged(q.Snapshot, before) {
			t.Fatalf("query %+v", q.Adoptability)
		}
	})
	t.Run("test_replacement_revision_invalidates_old_snapshot_adoption", func(t *testing.T) {
		w, f, before := newFrozenWorld(t)
		_, err := w.c.CST.ReviseContext(w.author, cstapi.ReviseContext{IdempotencyKey: w.key(), ContextID: "context.1", ExpectedVersion: 2,
			BaseRevision: 1, Reason: "重新核实询价范围"})
		w.must(err)
		_, err = w.c.CST.VerifyContext(w.reviewer, cstapi.VerifyContext{IdempotencyKey: w.key(), ContextID: "context.1", ExpectedVersion: 3,
			WorkingRevision: 2, SupersedesRevision: 1, Evidence: proof})
		w.must(err)
		w.ineligible(f.SnapshotID, before, cstapi.AdoptionNotCurrent, "/inputs/cst/costing_context/context.1/1")
	})
}

// Python test_costing.py test_authorization_and_missing_history_rejections_are_audited, on snapshots:
// a reader without cst.read and a read of a missing snapshot are both refused and audited as
// cost_snapshot.get.
func TestSnapshotHistoryRejectionsAreAudited(t *testing.T) {
	w := newScenWorld(t)
	reader := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reader"},
		Permissions: []string{"pol.read"}, DataScope: "company"})
	_, err := w.c.CST.GetSnapshot(reader, "missing")
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.c.CST.GetSnapshot(w.reviewer, "missing")
	w.want(err, kernel.CodeNotFound)
	entries, err := w.c.CST.Audit(w.reviewer)
	w.must(err)
	rejected := 0
	for _, e := range entries {
		if e.Command == "cost_snapshot.get" && e.Outcome == "rejected" {
			rejected++
		}
	}
	if rejected != 2 {
		t.Fatalf("%d audited rejections", rejected)
	}
}

// Python test_costing.py test_snapshot_store_rejects_edits_deletion_and_late_children: the
// application role holds no privilege to change or delete a snapshot or its rows (42501); a late
// component is refused by the trigger (23000), a component under a counted ordinal by the key
// (23505); the schema owner's change is refused by the trigger; a source's denial cannot be undone.
// The snapshot still reads 660.00 USD.
func TestSnapshotStoreRejectsEditsDeletionAndLateChildren(t *testing.T) {
	w, f, _ := newFrozenWorld(t)
	_, err := w.attest(w.reviewer, "evidence", cstapi.VerdictDenied, w.counter())
	w.must(err)
	id := f.SnapshotID
	for _, c := range []struct{ sql, want string }{
		{"UPDATE brant_cst.cost_snapshot SET result_hash = repeat('0', 64) WHERE snapshot_id = '" + id + "'", "42501"},
		{"DELETE FROM brant_cst.cost_snapshot WHERE snapshot_id = '" + id + "'", "42501"},
		{"UPDATE brant_cst.cost_snapshot_component SET amount_minor = 0 WHERE snapshot_id = '" + id + "'", "42501"},
		{"DELETE FROM brant_cst.cost_snapshot_allocation WHERE snapshot_id = '" + id + "'", "42501"},
		{"DELETE FROM brant_cst.source_attestation", "42501"},
		{"UPDATE brant_cst.cost_snapshot SET snapshot_id = snapshot_id WHERE snapshot_id = '" + id + "'", "23000"},
		{`INSERT INTO brant_cst.cost_snapshot_component (tenant_id, company_id, snapshot_id, ordinal, component_id, kind, obligation_id, currency,
			amount_minor) SELECT tenant_id, company_id, snapshot_id, 3, 'late', 'charge', 'late', 'USD', 0 FROM brant_cst.cost_snapshot
			WHERE snapshot_id = '` + id + "'", "23000"},
		{`INSERT INTO brant_cst.cost_snapshot_component (tenant_id, company_id, snapshot_id, ordinal, component_id, kind, obligation_id, currency,
			amount_minor) SELECT tenant_id, company_id, snapshot_id, 1, 'late', 'charge', 'late', 'USD', 0 FROM brant_cst.cost_snapshot
			WHERE snapshot_id = '` + id + "'", "23505"},
		{"UPDATE brant_cst.source_standing SET status = 'undisputed', denial_id = NULL, denied_at = NULL, root_version = root_version + 1", "23000"},
		{"UPDATE brant_cst.cost_scenario SET status = 'draft', root_version = 3 WHERE scenario_id = 'scenario.1'", "23000"},
	} {
		if got := sqlCode(w.exec("company.A", c.sql)); got != c.want {
			t.Errorf("%s: %s, want %s", c.sql, got, c.want)
		}
	}
	err = w.asOwner(`UPDATE brant_cst.cost_snapshot SET selection_reason = 'edited' WHERE snapshot_id = '` + id + "'")
	if got := sqlCode(err); got != "23000" {
		t.Errorf("the owner's edit: %s", got)
	}
	if v := w.snapshot(id); v.TotalMinor != "66000" || v.SelectionReason != selection {
		t.Fatalf("snapshot %+v", v)
	}
}

// asOwner runs sql as the schema owner in company.A's scope.
func (w *scenWorld) asOwner(sql string) error {
	tx, err := pgtest.MigratorPool(w.t).Begin(context.Background())
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true), set_config('app.company_id', 'company.A', true)`,
		w.tenant); err != nil {
		return err
	}
	_, err = tx.Exec(context.Background(), sql)
	return err
}

// SM-CST-ADOPT eligible → superseded: a freeze of another scenario of the same costing context may
// supersede an earlier snapshot, once; the earlier one is then superseded (CostSnapshotSuperseded),
// a snapshot of another context is not superseded by it, and a later denial does not relabel a
// superseded snapshot (both are terminal; the first reached decides).
func TestFreezeSupersedesAnEarlierSnapshot(t *testing.T) {
	w, first, _ := newFrozenWorld(t)
	r, err := w.evaluated(w.scenario(w.variant(func(*supapi.ConditionLine) {}), "log.1"))
	w.must(err)
	cmd := w.freezeCmd(r, 1)
	cmd.SupersedesSnapshotID = first.SnapshotID
	second, err := w.c.CST.FreezeSnapshot(w.reviewer, cmd)
	w.must(err)
	if second.SupersedesSnapshotID != first.SnapshotID {
		t.Fatalf("receipt %+v", second)
	}
	if q := w.query(first.SnapshotID); q.Adoptability.Status != cstapi.AdoptionSuperseded || q.Adoptability.Input != "/superseded_by/"+second.SnapshotID ||
		q.Snapshot.SupersededBySnapshotID != second.SnapshotID {
		t.Fatalf("first %+v", q.Adoptability)
	}
	if q := w.query(second.SnapshotID); !q.CurrentlyEligibleForQuote || q.Snapshot.SupersedesSnapshotID != first.SnapshotID {
		t.Fatalf("second %+v", q.Adoptability)
	}
	superseded := 0
	for _, e := range w.events("company.A", cstapi.Source) {
		if e.Type == cstapi.EventCostSnapshotSuperseded && e.Payload["snapshot_id"] == first.SnapshotID &&
			e.Payload["superseded_by_snapshot_id"] == second.SnapshotID {
			superseded++
		}
	}
	if superseded != 1 {
		t.Fatalf("%d CostSnapshotSuperseded events", superseded)
	}
	third, err := w.evaluated(w.scenario("offer.1", "log.1"))
	w.must(err)
	again := w.freezeCmd(third, 1)
	again.SupersedesSnapshotID = first.SnapshotID
	_, err = w.c.CST.FreezeSnapshot(w.reviewer, again)
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/supersedes_snapshot_id" {
		t.Fatalf("superseding twice: %s at %s", code(err), path(err))
	}
	w.verifiedContext("context.2", w.contextData())
	d := w.scenario("offer.1", "log.1")
	d.ContextRef = &cstapi.ContextRef{ContextID: "context.2", Revision: 1}
	elsewhere, err := w.evaluated(d)
	w.must(err)
	across := w.freezeCmd(elsewhere, 1)
	across.SupersedesSnapshotID = second.SnapshotID
	_, err = w.c.CST.FreezeSnapshot(w.reviewer, across)
	if code(err) != cstapi.CodeComparisonBasisMismatch || path(err) != "/args/supersedes_snapshot_id" {
		t.Fatalf("superseding across contexts: %s at %s", code(err), path(err))
	}
	_, err = w.attest(w.reviewer, "evidence", cstapi.VerdictDenied, w.counter())
	w.must(err)
	if q := w.query(first.SnapshotID); q.Adoptability.Status != cstapi.AdoptionSuperseded {
		t.Fatalf("a superseded snapshot stays superseded: %+v", q.Adoptability)
	}
	if q := w.query(second.SnapshotID); q.Adoptability.Status != cstapi.AdoptionDenied {
		t.Fatalf("the successor is denied: %+v", q.Adoptability)
	}
}
