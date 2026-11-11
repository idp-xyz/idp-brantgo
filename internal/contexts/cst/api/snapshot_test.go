package api

import (
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func schemaAt(t *testing.T, err error, path string) {
	t.Helper()
	if r, ok := kernel.AsRejection(err); !ok || r.Code != kernel.CodeSchemaInvalid || r.Fields[0].Path != path {
		t.Errorf("%s: got %v", path, err)
	}
}

var hash64 = strings.Repeat("a", 64)

// The closed contracts of cost_snapshot.freeze (Python commands.schema.json: scenario_id,
// expected_version ≥ 1, result_id, result_hash of 64 lower-case hex digits, selection_reason) and
// costing.attest_source (Python source_denial.record: id, source_id, reason, 1–100 evidence, and the
// verdict).
func TestSnapshotCommandsValidate(t *testing.T) {
	freeze := FreezeSnapshot{IdempotencyKey: "k", ScenarioID: "s", ExpectedVersion: 1, EvaluationID: "e", ResultHash: hash64,
		SelectionReason: "同口径成本已核实"}
	if err := freeze.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path string
		edit func(c *FreezeSnapshot)
	}{
		{"/idempotency_key", func(c *FreezeSnapshot) { c.IdempotencyKey = "" }},
		{"/args/scenario_id", func(c *FreezeSnapshot) { c.ScenarioID = "a b" }},
		{"/args/expected_version", func(c *FreezeSnapshot) { c.ExpectedVersion = 0 }},
		{"/args/evaluation_id", func(c *FreezeSnapshot) { c.EvaluationID = "" }},
		{"/args/result_hash", func(c *FreezeSnapshot) { c.ResultHash = strings.Repeat("A", 64) }},
		{"/args/selection_reason", func(c *FreezeSnapshot) { c.SelectionReason = "" }},
		{"/args/supersedes_snapshot_id", func(c *FreezeSnapshot) { c.SupersedesSnapshotID = "x y" }},
	} {
		x := freeze
		c.edit(&x)
		schemaAt(t, x.Validate(), c.path)
	}
	attest := AttestSource{IdempotencyKey: "k", AttestationID: "denial.1", SourceID: "source.1", Verdict: VerdictDenied,
		Reason: "新反证明确否认原来源", Evidence: []Evidence{{SourceID: "source.2", Locator: "独立反证"}}}
	if err := attest.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path string
		edit func(c *AttestSource)
	}{
		{"/args/id", func(c *AttestSource) { c.AttestationID = "" }},
		{"/args/source_id", func(c *AttestSource) { c.SourceID = "" }},
		{"/args/verdict", func(c *AttestSource) { c.Verdict = "withdrawn" }},
		{"/args/reason", func(c *AttestSource) { c.Reason = "" }},
		{"/args/evidence", func(c *AttestSource) { c.Evidence = nil }},
		{"/args/evidence/0/locator", func(c *AttestSource) { c.Evidence[0].Locator = "" }},
	} {
		x := attest
		x.Evidence = append([]Evidence{}, attest.Evidence...)
		c.edit(&x)
		schemaAt(t, x.Validate(), c.path)
	}
	if err := (CostSnapshotRef{SnapshotID: "snap.1"}).Validate("/ref"); err != nil {
		t.Fatal(err)
	}
	schemaAt(t, CostSnapshotRef{SnapshotID: "snap.1", ResultHash: "x"}.Validate("/ref"), "/ref/result_hash")
	schemaAt(t, CostSnapshotRef{}.Validate("/ref"), "/ref/snapshot_id")
}

// The guard's row maps to a verdict; Check passes REFERENCE_INVALID for an unknown snapshot and the
// catalog's COST_SNAPSHOT_INELIGIBLE (422) for one that is not adoptable.
func TestAdoptabilityVerdict(t *testing.T) {
	ref := CostSnapshotRef{SnapshotID: "snap.1", ResultHash: hash64}
	args := AdoptabilityGuardArgs(ref, time.Date(2026, 10, 22, 9, 0, 0, 123, time.UTC))
	if h, ok := args[1].(*string); !ok || *h != hash64 || !args[2].(time.Time).Equal(time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("args %v", args)
	}
	if h := AdoptabilityGuardArgs(CostSnapshotRef{SnapshotID: "snap.1"}, time.Now())[1].(*string); h != nil {
		t.Fatal("an empty result hash is NULL")
	}
	row := func(known bool, vals ...*string) func(dest ...any) error {
		return func(dest ...any) error {
			*dest[0].(*bool) = known
			for i, v := range vals {
				*dest[i+1].(**string) = v
			}
			return nil
		}
	}
	s := func(v string) *string { return &v }
	v, err := ScanAdoptabilityGuard(ref, row(true, s(AdoptionAdoptable), nil, nil, s(hash64), s(hash64)))
	if err != nil || !v.Adoptable() || v.Check("/ref") != nil || v.EvidenceHash != hash64 {
		t.Fatalf("%+v %v", v, err)
	}
	v, _ = ScanAdoptabilityGuard(ref, row(true, s(AdoptionDenied), s(CodeCostSnapshotIneligible), s("/sources/source.1"), s(hash64), s(hash64)))
	if r, ok := kernel.AsRejection(v.Check("/ref")); v.Adoptable() || !ok || r.Code != CodeCostSnapshotIneligible ||
		r.Category != kernel.CategoryInvalid || !strings.Contains(r.Fields[0].Reason, "/sources/source.1") {
		t.Fatalf("denied: %+v %v", v, v.Check("/ref"))
	}
	v, _ = ScanAdoptabilityGuard(ref, row(false, nil, s(kernel.CodeReferenceInvalid), nil, nil, nil))
	if r, ok := kernel.AsRejection(v.Check("/ref")); !ok || r.Code != kernel.CodeReferenceInvalid || v.Status != "" {
		t.Fatalf("unknown: %+v", v)
	}
	if !(Adoptability{Status: AdoptionAdoptable}).Adoptable() || (Adoptability{Status: AdoptionSuperseded}).Adoptable() {
		t.Fatal("adoptability")
	}
}
