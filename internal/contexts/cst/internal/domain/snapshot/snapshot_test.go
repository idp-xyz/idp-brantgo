package snapshot

import (
	"encoding/json"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/scenario"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var now = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

func rejected(t *testing.T, err error, code, path string) {
	t.Helper()
	r, ok := kernel.AsRejection(err)
	if !ok || r.Code != code || r.Fields[0].Path != path {
		t.Fatalf("got %v, want %s at %s", err, code, path)
	}
}

// fixture is the 660.00 USD evaluation of scenario.1 at version 1 (Python costing_fixture): 120 PCS
// at 5 USD and 60 USD freight, both to L1.
func fixture() (*scenario.Scenario, *scenario.Evaluation) {
	sc := &scenario.Scenario{ID: "scenario.1", RootVersion: 1, Status: scenario.Draft, OwnerActor: "author", OwnerTeam: "trade"}
	r := scenario.Result{EngineVersion: scenario.EngineVersion, ScenarioID: "scenario.1",
		ContextRef: scenario.ContextRef{ContextID: "context.1", Revision: 1}, Rfq: scenario.RfqBinding{RfqID: "rfq.1", RowVersion: 2, LinesHash: "l"},
		Currency: "USD", MinorUnits: 2, VerificationStatus: scenario.Verified, ComparisonStatus: scenario.Comparable, TotalMinor: "66000",
		Components: []scenario.Component{
			{ComponentID: "fee.1", Kind: "charge", ObligationID: "freight.1", AmountMinor: "6000",
				Allocations: []scenario.LineAmount{{LineID: "L1", Basis: "quantity", BaseInteger: "120", AmountMinor: "6000"}}},
			{ComponentID: "purchase.1", Kind: "purchase", AmountMinor: "60000",
				Allocations: []scenario.LineAmount{{LineID: "L1", Basis: "purchase", BaseInteger: "1", AmountMinor: "60000"}}}}}
	input := []byte(`{"inputs":[]}`)
	r.InputHash = scenario.HashBytes(input)
	doc := scenario.Canonical(r)
	e := &scenario.Evaluation{ID: "eval.1", ScenarioID: "scenario.1", Number: 1, ScenarioVersion: 1, EngineVersion: scenario.EngineVersion,
		NormalizationVersion: scenario.NormalizationVersion, Input: input, InputHash: r.InputHash, Document: doc,
		ResultHash: scenario.HashBytes(doc), Result: r}
	return sc, e
}

func computed(e *scenario.Evaluation) scenario.Computed {
	return scenario.Computed{Input: scenario.NormalizedInput{NormalizationVersion: e.NormalizationVersion}, InputHash: e.InputHash,
		Result: e.Result, ResultHash: e.ResultHash}
}

// Python cost_snapshot.freeze, after existence, version and state: the named evaluation of the
// scenario, verified, comparable, on the current version, with its result hash.
func TestCheckEvaluation(t *testing.T) {
	sc, e := fixture()
	if err := CheckEvaluation(sc, e, e.ResultHash); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, code, path string
		edit             func(sc *scenario.Scenario, e *scenario.Evaluation, hash *string)
	}{
		{"another scenario's evaluation", kernel.CodeReferenceInvalid, "/args/evaluation_id",
			func(_ *scenario.Scenario, e *scenario.Evaluation, _ *string) { e.ScenarioID = "other" }},
		{"provisional", CodeCostInputNotReady, "/args/verification_status",
			func(_ *scenario.Scenario, e *scenario.Evaluation, _ *string) {
				e.Result.VerificationStatus = scenario.Provisional
			}},
		{"not comparable", CodeComparisonBasisMismatch, "/args/comparison_status",
			func(_ *scenario.Scenario, e *scenario.Evaluation, _ *string) {
				e.Result.ComparisonStatus = scenario.NotComparable
			}},
		{"draft changed since", CodeReferenceStale, "/args/scenario_version",
			func(sc *scenario.Scenario, _ *scenario.Evaluation, _ *string) { sc.RootVersion = 2 }},
		{"another result hash", CodeReferenceStale, "/args/result_hash",
			func(_ *scenario.Scenario, _ *scenario.Evaluation, h *string) { *h = "0" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			sc, e := fixture()
			hash := e.ResultHash
			c.edit(sc, e, &hash)
			rejected(t, CheckEvaluation(sc, e, hash), c.code, c.path)
		})
	}
	sc, _ = fixture()
	rejected(t, CheckEvaluation(sc, nil, ""), kernel.CodeReferenceInvalid, "/args/evaluation_id")
}

// CST-INV-001: the recomputation must be verified, comparable and the same bytes.
func TestCheckCurrent(t *testing.T) {
	_, e := fixture()
	if err := CheckCurrent(e, computed(e)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, code, path string
		edit             func(c *scenario.Computed)
	}{
		{"no longer verified", CodeCostInputNotReady, "/args/verification_status", func(c *scenario.Computed) { c.Result.VerificationStatus = scenario.Provisional }},
		{"no longer comparable", CodeComparisonBasisMismatch, "/args/comparison_status", func(c *scenario.Computed) { c.Result.ComparisonStatus = scenario.NotComparable }},
		{"other input", CodeReferenceStale, "/args/evaluation_id", func(c *scenario.Computed) { c.InputHash = "x" }},
		{"other engine", CodeReferenceStale, "/args/evaluation_id", func(c *scenario.Computed) { c.Result.EngineVersion = "brant.cst.evaluation/9" }},
		{"other normalization", CodeReferenceStale, "/args/evaluation_id", func(c *scenario.Computed) { c.Input.NormalizationVersion = "n/9" }},
		{"other result", CodeReferenceStale, "/args/evaluation_id", func(c *scenario.Computed) { c.ResultHash = "x" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := computed(e)
			c.edit(&x)
			rejected(t, CheckCurrent(e, x), c.code, c.path)
		})
	}
}

// The snapshot keeps the evaluation's hashes and amounts, numbers its components and allocations in
// result order, and its document, which carries the selection reason (Python output_evidence),
// hashes to the evidence hash.
func TestFreeze(t *testing.T) {
	sc, e := fixture()
	sc.RootVersion = 2
	by := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer"}
	s := Freeze("snap.1", sc, e, "同口径成本已核实", "snap.0", by, 7, now.Add(123*time.Nanosecond))
	if s.InputScenarioVersion != 1 || s.FrozenScenarioVersion != 2 || s.ResultHash != e.ResultHash || s.InputHash != e.InputHash ||
		s.TotalMinor != "66000" || s.Context.ContextID != "context.1" || s.Supersedes != "snap.0" || !s.FrozenAt.Equal(now) ||
		s.OwnerActor != "author" || s.OwnerTeam != "trade" {
		t.Fatalf("snapshot %+v", s)
	}
	if len(s.Components) != 2 || s.Components[1].Ordinal != 2 || s.Components[1].ComponentID != "purchase.1" || len(s.Allocations) != 2 ||
		s.Allocations[1].Ordinal != 2 || s.Allocations[1].ComponentID != "purchase.1" || s.Allocations[0].AmountMinor != "6000" {
		t.Fatalf("children %+v %+v", s.Components, s.Allocations)
	}
	var d Document
	if err := json.Unmarshal(s.Document, &d); err != nil {
		t.Fatal(err)
	}
	if d.Kind != DocumentKind || d.SelectionReason != "同口径成本已核实" || d.ResultHash != e.ResultHash || d.FrozenAt != "2026-10-22T09:00:00.000000Z" ||
		d.FrozenScenarioVersion != 2 || d.SupersedesSnapshotID != "snap.0" || scenario.HashBytes(s.Document) != s.EvidenceHash {
		t.Fatalf("document %s", s.Document)
	}
	other := Freeze("snap.1", sc, e, "another reason", "snap.0", by, 7, now)
	if other.EvidenceHash == s.EvidenceHash || other.ResultHash != s.ResultHash {
		t.Fatal("the selection reason is part of the evidence, not of the result")
	}
	if err := s.Verify(e); err != nil {
		t.Fatal(err)
	}
	tampered := *s
	tampered.Document = append([]byte{}, s.Document...)
	tampered.Document[len(tampered.Document)-2] = 'X'
	rejected(t, tampered.Verify(e), CodeEvidenceHashMismatch, "/evidence_hash")
	bad := *e
	bad.Document = []byte(`{}`)
	rejected(t, s.Verify(&bad), CodeEvidenceHashMismatch, "/result_hash")
}

func TestCheckSupersede(t *testing.T) {
	ctx := scenario.ContextRef{ContextID: "context.1", Revision: 2}
	old := &Snapshot{ID: "snap.0", Context: scenario.ContextRef{ContextID: "context.1", Revision: 1}}
	if err := CheckSupersede(old, true, "", ctx); err != nil {
		t.Fatal(err)
	}
	rejected(t, CheckSupersede(nil, false, "", ctx), kernel.CodeReferenceInvalid, "/args/supersedes_snapshot_id")
	rejected(t, CheckSupersede(old, false, "", ctx), kernel.CodeReferenceInvalid, "/args/supersedes_snapshot_id")
	rejected(t, CheckSupersede(old, true, "", scenario.ContextRef{ContextID: "context.2", Revision: 1}), CodeComparisonBasisMismatch,
		"/args/supersedes_snapshot_id")
	rejected(t, CheckSupersede(old, true, "snap.9", ctx), kernel.CodeInvalidState, "/args/supersedes_snapshot_id")
}

// Python source_denial.record: an ID is used once (VERSION_CONFLICT); a denial is final (T02 §13:
// 不能删除否认事件复活旧结论).
func TestAttest(t *testing.T) {
	by := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer"}
	ev := []Evidence{{SourceID: "source.2", Locator: "独立反证"}}
	st := &Standing{SourceID: "source.1", RootVersion: 1}
	a, err := Attest(nil, st, "att.1", Attested, "人工核实", ev, by, "trade", 3, now)
	if err != nil || a.Verdict != Attested || st.Denied() || st.RootVersion != 1 {
		t.Fatalf("%+v %+v %v", a, st, err)
	}
	d, err := Attest(nil, st, "denial.1", Denied, "新反证明确否认原来源", ev, by, "trade", 3, now)
	if err != nil || !st.Denied() || st.DenialID != "denial.1" || st.RootVersion != 2 || !st.DeniedAt.Equal(d.At) {
		t.Fatalf("%+v %+v %v", d, st, err)
	}
	_, err = Attest(nil, st, "denial.2", Denied, "again", ev, by, "", 3, now)
	rejected(t, err, kernel.CodeInvalidState, "/args/source_id")
	_, err = Attest(nil, st, "att.2", Attested, "revive", ev, by, "", 3, now)
	rejected(t, err, kernel.CodeInvalidState, "/args/source_id")
	_, err = Attest(a, &Standing{SourceID: "source.3", RootVersion: 1}, "att.1", Attested, "same id", ev, by, "", 3, now)
	rejected(t, err, kernel.CodeVersionConflict, "/args/id")
}
