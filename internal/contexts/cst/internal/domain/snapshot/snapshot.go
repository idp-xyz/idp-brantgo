// Package snapshot is the CostSnapshot aggregate (tactical model CST-AGG-CostSnapshot, CST-INV-001,
// CST-INV-002, SM-CST-ADOPT; Python costing.py and integrated_model cost_snapshot.freeze) and the
// standing CST keeps about the EVD sources costings rest on (costing.attest_source, Python
// source_denial.record and attest_costing_source).
//
// cost_snapshot.freeze (human) freezes one verified and comparable evaluation of a draft scenario:
// the scenario moves to frozen with the next root version and its one snapshot is created in the
// same transaction (the registered exception of T2). The freeze recomputes the evaluation on the
// current inputs at its own instant and requires the same normalized input, engine and
// normalization versions and result (CST-INV-001); the snapshot keeps the original evaluation, its
// hashes and the selection reason in a canonical document whose SHA-256 is the evidence hash
// (Python output_evidence: the snapshot hash includes the selection and keeps the result hash).
//
// Adoptability is derived (SM-CST-ADOPT): a snapshot is adoptable until a source its evaluation rests
// on is denied or a later snapshot of the same costing context supersedes it, whichever comes first,
// and while every input it bound is still in force (Python snapshot_query re-evaluates the inputs at
// the query's instant). CST's guard function decides it in SQL; this package decides the freeze and
// the attestations.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/scenario"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of the CST error catalog rows this package decides; package api declares the same.
const (
	CodeCostInputNotReady       = "COST_INPUT_NOT_READY"
	CodeComparisonBasisMismatch = "COMPARISON_BASIS_MISMATCH"
	CodeEvidenceHashMismatch    = "EVIDENCE_HASH_MISMATCH"
	CodeReferenceStale          = "REFERENCE_STALE"
)

// Categories maps the codes above to their error catalog category.
var Categories = map[string]kernel.Category{
	CodeCostInputNotReady:       kernel.CategoryInvalid,
	CodeComparisonBasisMismatch: kernel.CategoryInvalid,
	CodeEvidenceHashMismatch:    kernel.CategoryInvalid,
	CodeReferenceStale:          kernel.CategoryConflict,
}

func fail(code, path, reason string) error {
	if c, ok := Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}

// DocumentKind is the kind of a snapshot document.
const DocumentKind = "cost_snapshot"

// Snapshot is a frozen cost snapshot. It never changes.
type Snapshot struct {
	ID                    string
	ScenarioID            string
	EvaluationID          string
	InputScenarioVersion  int64
	FrozenScenarioVersion int64
	Context               scenario.ContextRef
	Rfq                   scenario.RfqBinding
	EngineVersion         string
	NormalizationVersion  string
	InputHash             string
	ResultHash            string
	SelectionReason       string
	Currency              string
	TotalMinor            string
	Supersedes            string // the snapshot this one supersedes; empty for none
	FrozenBy              kernel.ActorRef
	Epoch                 int64
	FrozenAt              time.Time
	Document              []byte // canonical JSON of the Document
	EvidenceHash          string
	Components            []Component
	Allocations           []Allocation
	OwnerActor            string
	OwnerTeam             string
}

// Component is one cost component of a snapshot, numbered from 1 in result order.
type Component struct {
	Ordinal      int
	ComponentID  string
	Kind         string
	ObligationID string
	Currency     string
	AmountMinor  string
}

// Allocation is what one demand line received of a component, numbered from 1 in result order.
type Allocation struct {
	Ordinal     int
	ComponentID string
	LineID      string
	Basis       string
	BaseInteger string
	AmountMinor string
}

// Document is the canonical snapshot document the evidence hash covers: the frozen evaluation's
// identity, hashes and outcome, both scenario versions, the selection reason, the supersession and
// who froze it when.
type Document struct {
	Kind                  string              `json:"kind"`
	SnapshotID            string              `json:"snapshot_id"`
	ScenarioID            string              `json:"scenario_id"`
	EvaluationID          string              `json:"evaluation_id"`
	InputScenarioVersion  int64               `json:"input_scenario_version"`
	FrozenScenarioVersion int64               `json:"frozen_scenario_version"`
	ContextRef            scenario.ContextRef `json:"context_ref"`
	Rfq                   scenario.RfqBinding `json:"rfq"`
	EngineVersion         string              `json:"engine_version"`
	NormalizationVersion  string              `json:"normalization_version"`
	InputHash             string              `json:"input_hash"`
	ResultHash            string              `json:"result_hash"`
	VerificationStatus    string              `json:"verification_status"`
	ComparisonStatus      string              `json:"comparison_status"`
	SelectionReason       string              `json:"selection_reason"`
	Currency              string              `json:"currency"`
	TotalMinor            string              `json:"total_minor"`
	SupersedesSnapshotID  string              `json:"supersedes_snapshot_id,omitempty"`
	FrozenByKind          string              `json:"frozen_by_kind"`
	FrozenByID            string              `json:"frozen_by_id"`
	FrozenAt              string              `json:"frozen_at"`
}

// CheckEvaluation admits evaluation e of scenario sc for a freeze that names it with resultHash
// (Python cost_snapshot.freeze, after existence, root version and state): e is an evaluation of sc
// (REFERENCE_INVALID at /args/evaluation_id), its result verified (COST_INPUT_NOT_READY at
// /args/verification_status) and comparable (COMPARISON_BASIS_MISMATCH at /args/comparison_status),
// computed on the scenario's current version (REFERENCE_STALE at /args/scenario_version), and
// resultHash is its result hash (REFERENCE_STALE at /args/result_hash).
func CheckEvaluation(sc *scenario.Scenario, e *scenario.Evaluation, resultHash string) error {
	switch {
	case e == nil || e.ScenarioID != sc.ID:
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/evaluation_id", "The scenario has no such evaluation")
	case e.Result.VerificationStatus != scenario.Verified:
		return fail(CodeCostInputNotReady, "/args/verification_status", "A provisional result is not frozen")
	case e.Result.ComparisonStatus != scenario.Comparable:
		return fail(CodeComparisonBasisMismatch, "/args/comparison_status", "A result that is not comparable is not frozen")
	case e.ScenarioVersion != sc.RootVersion:
		return fail(CodeReferenceStale, "/args/scenario_version", "The draft changed since the evaluation; evaluate it again")
	case e.ResultHash != resultHash:
		return fail(CodeReferenceStale, "/args/result_hash", "The result hash is not the evaluation's")
	}
	return nil
}

// CheckCurrent decides CST-INV-001 on the recomputation c of evaluation e's inputs at the freeze's
// instant: the result is still verified and comparable, and the normalized input, the engine and
// normalization versions and the result are the same bytes (REFERENCE_STALE at /args/evaluation_id
// otherwise: an input changed since the evaluation). Everything else the recomputation refuses —
// an input no longer in force, an exchange rate older than the policy allows, a source no longer
// readable or denied — it refuses with its own code.
func CheckCurrent(e *scenario.Evaluation, c scenario.Computed) error {
	switch {
	case c.Result.VerificationStatus != scenario.Verified:
		return fail(CodeCostInputNotReady, "/args/verification_status", "The inputs no longer yield a verified result")
	case c.Result.ComparisonStatus != scenario.Comparable:
		return fail(CodeComparisonBasisMismatch, "/args/comparison_status", "The inputs no longer yield a comparable result")
	case c.InputHash != e.InputHash || c.Result.EngineVersion != e.EngineVersion || c.Input.NormalizationVersion != e.NormalizationVersion ||
		c.ResultHash != e.ResultHash:
		return fail(CodeReferenceStale, "/args/evaluation_id", "The inputs changed since the evaluation; evaluate the scenario again")
	}
	return nil
}

// Freeze returns snapshot id of evaluation e of scenario sc, which the caller has just frozen at
// sc.RootVersion (the version after e's), frozen by by under epoch at now. supersedes names the
// snapshot of the same costing context it supersedes, or is empty.
func Freeze(id string, sc *scenario.Scenario, e *scenario.Evaluation, reason, supersedes string, by kernel.ActorRef, epoch int64,
	now time.Time) *Snapshot {
	now = kernel.Instant(now)
	r := e.Result
	s := &Snapshot{ID: id, ScenarioID: sc.ID, EvaluationID: e.ID, InputScenarioVersion: e.ScenarioVersion, FrozenScenarioVersion: sc.RootVersion,
		Context: r.ContextRef, Rfq: r.Rfq, EngineVersion: e.EngineVersion, NormalizationVersion: e.NormalizationVersion,
		InputHash: e.InputHash, ResultHash: e.ResultHash, SelectionReason: reason, Currency: r.Currency, TotalMinor: r.TotalMinor,
		Supersedes: supersedes, FrozenBy: by, Epoch: epoch, FrozenAt: now, OwnerActor: sc.OwnerActor, OwnerTeam: sc.OwnerTeam}
	for _, c := range r.Components {
		s.Components = append(s.Components, Component{Ordinal: len(s.Components) + 1, ComponentID: c.ComponentID, Kind: c.Kind,
			ObligationID: c.ObligationID, Currency: r.Currency, AmountMinor: c.AmountMinor})
		for _, a := range c.Allocations {
			s.Allocations = append(s.Allocations, Allocation{Ordinal: len(s.Allocations) + 1, ComponentID: c.ComponentID, LineID: a.LineID,
				Basis: a.Basis, BaseInteger: a.BaseInteger, AmountMinor: a.AmountMinor})
		}
	}
	s.Document = scenario.Canonical(s.document(r))
	s.EvidenceHash = scenario.HashBytes(s.Document)
	return s
}

func (s *Snapshot) document(r scenario.Result) Document {
	return Document{Kind: DocumentKind, SnapshotID: s.ID, ScenarioID: s.ScenarioID, EvaluationID: s.EvaluationID,
		InputScenarioVersion: s.InputScenarioVersion, FrozenScenarioVersion: s.FrozenScenarioVersion, ContextRef: s.Context, Rfq: s.Rfq,
		EngineVersion: s.EngineVersion, NormalizationVersion: s.NormalizationVersion, InputHash: s.InputHash, ResultHash: s.ResultHash,
		VerificationStatus: r.VerificationStatus, ComparisonStatus: r.ComparisonStatus, SelectionReason: s.SelectionReason,
		Currency: s.Currency, TotalMinor: s.TotalMinor, SupersedesSnapshotID: s.Supersedes, FrozenByKind: string(s.FrozenBy.Kind),
		FrozenByID: s.FrozenBy.ID, FrozenAt: kernel.FormatTimestamp(s.FrozenAt)}
}

// Verify re-checks the stored evidence (Python output_evidence.check_hashes): the snapshot document
// hashes to the evidence hash and the frozen evaluation's result document to its result hash
// (EVIDENCE_HASH_MISMATCH otherwise). The database refuses either mismatch on insert; a read checks
// again before it hands the evidence out.
func (s *Snapshot) Verify(e *scenario.Evaluation) error {
	switch {
	case hash(s.Document) != s.EvidenceHash:
		return fail(CodeEvidenceHashMismatch, "/evidence_hash", "")
	case e == nil || e.ID != s.EvaluationID || hash(e.Document) != e.ResultHash || e.ResultHash != s.ResultHash ||
		hash(e.Input) != e.InputHash || e.InputHash != s.InputHash:
		return fail(CodeEvidenceHashMismatch, "/result_hash", "")
	}
	return nil
}

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CheckSupersede admits the snapshot a freeze supersedes (SM-CST-ADOPT eligible → superseded): it
// exists and the caller sees it (REFERENCE_INVALID at /args/supersedes_snapshot_id), costs the same
// costing context as the scenario being frozen (COMPARISON_BASIS_MISMATCH there) and has no
// successor yet (INVALID_STATE there: a snapshot is superseded once).
func CheckSupersede(old *Snapshot, visible bool, successor string, context scenario.ContextRef) error {
	const p = "/args/supersedes_snapshot_id"
	switch {
	case old == nil || !visible:
		return kernel.Reject(kernel.CodeReferenceInvalid, p, "")
	case old.Context.ContextID != context.ContextID:
		return fail(CodeComparisonBasisMismatch, p, "A snapshot supersedes a snapshot of the same costing context")
	case successor != "":
		return kernel.Reject(kernel.CodeInvalidState, p, "The snapshot was already superseded")
	}
	return nil
}

// Verdicts of an attestation.
const (
	Attested = "attested"
	Denied   = "denied"
)

// Evidence is a source a human relied on and where in it.
type Evidence struct {
	SourceID string `json:"source_id"`
	Locator  string `json:"locator"`
}

// Standing is CST's standing of an EVD source: undisputed until a human denies it, once and for
// ever (T02 §13: restoring effect takes new verified revisions on other evidence, never deleting the
// denial).
type Standing struct {
	SourceID    string
	RootVersion int64
	DenialID    string // empty: undisputed
	DeniedAt    time.Time
}

// Denied reports whether the source was denied.
func (s Standing) Denied() bool { return s.DenialID != "" }

// Attestation is a human's immutable verdict on a source (costing.attest_source): attested or
// denied, with the reason and the evidence it rests on.
type Attestation struct {
	ID        string
	SourceID  string
	Verdict   string
	Reason    string
	Evidence  []Evidence
	By        kernel.ActorRef
	Epoch     int64
	At        time.Time
	OwnerTeam string
}

// Attest records verdict on source standing as attestation id (Python source_denial.record and
// attest_costing_source): an attestation ID is used once (VERSION_CONFLICT at /args/id, as Python);
// a denied source is neither denied again nor attested (INVALID_STATE at /args/source_id). A denial
// moves the standing to denied with the next root version.
func Attest(existing *Attestation, standing *Standing, id, verdict, reason string, evidence []Evidence, by kernel.ActorRef, team string,
	epoch int64, now time.Time) (*Attestation, error) {
	switch {
	case existing != nil:
		return nil, kernel.Reject(kernel.CodeVersionConflict, "/args/id", "")
	case standing.Denied() && verdict == Denied:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/source_id", "The source is already denied")
	case standing.Denied():
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/source_id",
			"A denied source is not attested again; verify new revisions on other evidence")
	}
	a := &Attestation{ID: id, SourceID: standing.SourceID, Verdict: verdict, Reason: reason, Evidence: slices.Clone(evidence), By: by,
		Epoch: epoch, At: kernel.Instant(now), OwnerTeam: team}
	if verdict == Denied {
		standing.RootVersion++
		standing.DenialID, standing.DeniedAt = a.ID, a.At
	}
	return a, nil
}
