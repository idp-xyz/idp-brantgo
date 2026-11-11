package api

import (
	"context"
	"regexp"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// A cost snapshot (CostSnapshot, T02 §13.3; SM-CST-SCENARIO draft → frozen, SM-CST-ADOPT) freezes
// one verified and comparable evaluation of a draft scenario, once per scenario (CST-INV-002).
// cost_snapshot.freeze recomputes the evaluation on the current inputs at its own instant inside
// CST's transaction, after locking the scenario root FOR UPDATE and the inputs FOR SHARE, and requires
// the same normalized input, engine version and result (CST-INV-001). The snapshot never changes; it
// is adoptable for new quotes and orders while no source its evaluation rests on is denied
// (costing.attest_source), no later snapshot of the same costing context supersedes it and every
// input it bound is still in force: CostSnapshotRef v1 and AdoptabilityGuard v1 (guard.go) give TRD
// and PUR that answer (Context Map R11, R21).

const (
	// Events of the event catalog.
	EventCostSnapshotFrozen     = "idp-brant.cst.cost_snapshot_frozen.v1"
	EventCostSourceDenied       = "idp-brant.cst.cost_source_denied.v1"
	EventCostSnapshotSuperseded = "idp-brant.cst.cost_snapshot_superseded.v1"

	CommandFreezeSnapshot = "cost_snapshot.freeze"
	CommandAttestSource   = "costing.attest_source"

	// Verdicts of costing.attest_source, and the receipt status of an attestation.
	VerdictAttested = "attested"
	VerdictDenied   = "denied"

	// Standing of a source in CST: undisputed until denied.
	SourceUndisputed = "undisputed"
	SourceDenied     = "denied"

	// Adoptability of a snapshot (SM-CST-ADOPT, derived): adoptable, or not adoptable because a
	// source was denied, a later snapshot superseded it (whichever came first), or an input is no
	// longer in force.
	AdoptionAdoptable  = "adoptable"
	AdoptionDenied     = "denied"
	AdoptionSuperseded = "superseded"
	AdoptionNotCurrent = "not_current"

	// An evaluation binds the EVD sources its inputs rest on (Context Map R64) as inputs of context
	// evd, kind source and use evidence.
	InputFromEVD = "evd"
	KindSource   = "source"
	UseEvidence  = "evidence"
)

var resultHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// FreezeSnapshot freezes evaluation EvaluationID of draft scenario ScenarioID at its root version
// ExpectedVersion (Python cost_snapshot.freeze: scenario_id, expected_version, result_id,
// result_hash, selection_reason). ResultHash is the evaluation's result hash as its receipt gave it.
// SupersedesSnapshotID optionally names the snapshot of the same costing context the new one
// supersedes. Human only.
type FreezeSnapshot struct {
	IdempotencyKey       string
	ScenarioID           string
	ExpectedVersion      int64
	EvaluationID         string
	ResultHash           string
	SelectionReason      string
	SupersedesSnapshotID string
}

// AttestSource records a human's verdict on an EVD source a costing rests on (Python
// source_denial.record, and attest_costing_source for attested): VerdictDenied makes every
// snapshot whose evaluation rests on the source not adoptable and refuses new evaluations and
// freezes on it; VerdictAttested records a confirmation. AttestationID names the record, which is
// created once; Reason and Evidence (sources the attesting human reads, not denied) are required.
// Human only.
type AttestSource struct {
	IdempotencyKey string
	AttestationID  string
	SourceID       string
	Verdict        string
	Reason         string
	Evidence       []Evidence
}

func (FreezeSnapshot) cstCommand() {}
func (AttestSource) cstCommand()   {}

// Validate checks the closed contract (Python commands.schema.json, cost_snapshot.freeze).
func (c FreezeSnapshot) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ScenarioID):
		return schema("/args/scenario_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case !kernel.ValidIdentifier(c.EvaluationID):
		return schema("/args/evaluation_id")
	case !resultHash.MatchString(c.ResultHash):
		return schema("/args/result_hash")
	case !text(c.SelectionReason, maxText):
		return schema("/args/selection_reason")
	case c.SupersedesSnapshotID != "" && !kernel.ValidIdentifier(c.SupersedesSnapshotID):
		return schema("/args/supersedes_snapshot_id")
	}
	return nil
}

// Validate checks the closed contract (Python commands.schema.json, source_denial.record, with the
// verdict).
func (c AttestSource) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.AttestationID):
		return schema("/args/id")
	case !kernel.ValidIdentifier(c.SourceID):
		return schema("/args/source_id")
	case c.Verdict != VerdictAttested && c.Verdict != VerdictDenied:
		return schema("/args/verdict")
	}
	if err := reason(c.Reason); err != nil {
		return err
	}
	return evidence(c.Evidence, 1, MaxEvidence, "/args/evidence")
}

// SnapshotReceipt is the answer of cost_snapshot.freeze: the command receipt (ObjectID is the
// snapshot ID, as Python's object_id; RowVersion the scenario's frozen root version; Status
// "frozen") and what the snapshot froze. A replay rebuilds it from the stored snapshot.
type SnapshotReceipt struct {
	Receipt
	SnapshotID            string
	ScenarioID            string
	EvaluationID          string
	InputScenarioVersion  int64
	FrozenScenarioVersion int64
	InputHash             string
	ResultHash            string
	EvidenceHash          string
	Currency              string
	TotalMinor            string
	SupersedesSnapshotID  string
}

// SnapshotCommands are the snapshot and attestation commands of CST. Errors are as for Commands.
type SnapshotCommands interface {
	FreezeSnapshot(ctx context.Context, cmd FreezeSnapshot) (SnapshotReceipt, error)
	// AttestSource returns a Receipt whose ObjectID is the attestation ID, RowVersion 1 and Status
	// the verdict.
	AttestSource(ctx context.Context, cmd AttestSource) (Receipt, error)
}

// SourceStanding is CST's standing of one source a snapshot rests on.
type SourceStanding struct {
	SourceID string
	Status   string // undisputed or denied
	DenialID string
	DeniedAt time.Time
}

// SnapshotView is a frozen snapshot with its provenance: the evaluation it froze (result, normalized
// input and every input it bound, the sources included), both scenario versions, its canonical
// document and evidence hash, who froze it when, its supersession both ways and the standing of its
// sources now. History stays readable whatever became of the inputs.
type SnapshotView struct {
	SnapshotID             string
	ScenarioID             string
	EvaluationID           string
	InputScenarioVersion   int64
	FrozenScenarioVersion  int64
	Context                ContextRef
	Rfq                    RfqBinding
	EngineVersion          string
	NormalizationVersion   string
	InputHash              string
	ResultHash             string
	EvidenceHash           string
	SelectionReason        string
	Currency               string
	TotalMinor             string
	SupersedesSnapshotID   string
	SupersededBySnapshotID string
	FrozenBy               kernel.ActorRef
	AuthorityEpoch         int64
	FrozenAt               time.Time
	Document               string // the canonical snapshot document the evidence hash covers
	Input                  string // the frozen evaluation's normalized input
	Result                 EvaluationResult
	Inputs                 []BoundInput
	Sources                []SourceStanding
}

// Adoptability is the AdoptabilityGuard's verdict on a snapshot at CheckedAt: Status adoptable, or
// denied, superseded or not_current with Code COST_SNAPSHOT_INELIGIBLE and Input naming the source
// (/sources/<id>), the successor (/superseded_by/<id>) or the input
// (/inputs/<context>/<kind>/<id>/<revision>) that decided it.
type Adoptability struct {
	Status    string
	Code      string
	Input     string
	CheckedAt time.Time
}

// Adoptable reports whether a new quote or order may adopt the snapshot now.
func (a Adoptability) Adoptable() bool { return a.Status == AdoptionAdoptable }

// SnapshotQuery is Python's snapshot_query: the immutable snapshot and whether it is currently
// eligible for a new quote, checked at EligibilityCheckedAt, which is not part of the evidence.
type SnapshotQuery struct {
	Snapshot                  SnapshotView
	Adoptability              Adoptability
	CurrentlyEligibleForQuote bool
	EligibilityCheckedAt      time.Time
}

// AttestationView is one recorded attestation.
type AttestationView struct {
	AttestationID  string
	SourceID       string
	Verdict        string
	Reason         string
	Evidence       []Evidence
	AttestedBy     kernel.ActorRef
	AuthorityEpoch int64
	AttestedAt     time.Time
}

// SnapshotQueries is CST's audited read side of snapshots and attestations. Every read re-checks the
// caller's permission and data scope (X-INV-003): a snapshot of a scenario the caller does not see
// is NOT_FOUND, one with an input or source the caller no longer reads PERMISSION_DENIED (Python
// _history_access); an evaluation ID is no snapshot (NOT_FOUND).
type SnapshotQueries interface {
	// GetSnapshot returns a snapshot with its provenance, audited as cost_snapshot.get.
	GetSnapshot(ctx context.Context, snapshotID string) (SnapshotView, error)
	// QuerySnapshot returns a snapshot and its adoptability now, audited as cost_snapshot.query.
	QuerySnapshot(ctx context.Context, snapshotID string) (SnapshotQuery, error)
	// GetAttestation returns an attestation the caller sees, audited as source_attestation.get.
	GetAttestation(ctx context.Context, attestationID string) (AttestationView, error)
}

// CostSnapshotRef is CostSnapshotRef v1: a frozen snapshot and the result hash its adopter bound
// (Python T03 snapshot_ref: id and result_hash). An empty ResultHash binds the snapshot alone.
type CostSnapshotRef struct {
	SnapshotID string
	ResultHash string
}

// Validate checks the form of the reference at path.
func (r CostSnapshotRef) Validate(path string) error {
	switch {
	case !kernel.ValidIdentifier(r.SnapshotID):
		return schema(path + "/snapshot_id")
	case r.ResultHash != "" && !resultHash.MatchString(r.ResultHash):
		return schema(path + "/result_hash")
	}
	return nil
}

// ResolvedSnapshot is what CST says about one CostSnapshotRef v1 at an instant, for a downstream
// context before its transaction (TRD quoting, R11; PUR comparing, R21): the snapshot's identity
// and amounts, its lines and components, and its adoptability then. Inside its transaction TRD
// re-checks with AdoptabilityGuard v1.
type ResolvedSnapshot struct {
	Ref          CostSnapshotRef
	ScenarioID   string
	Context      ContextRef
	Rfq          RfqBinding
	EvidenceHash string
	Currency     string
	TotalMinor   string
	Lines        []LineTotal
	Components   []CostComponent
	Adoptability Adoptability
}

// SnapshotLookups answers CostSnapshotRef v1 lookups. It needs no permission beyond an active grant
// and is not audited; snapshots that do not exist in the caller's company, lie outside its data
// scope or whose result hash differs from the reference's are absent. At most MaxSnapshotLookup
// references are asked at once (SCHEMA_INVALID otherwise).
type SnapshotLookups interface {
	ResolveSnapshots(ctx context.Context, refs []CostSnapshotRef, at time.Time) (map[CostSnapshotRef]ResolvedSnapshot, error)
}

// MaxSnapshotLookup bounds a snapshot lookup.
const MaxSnapshotLookup = 200
