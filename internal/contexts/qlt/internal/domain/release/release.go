// Package release holds QLT's QualityRelease aggregate (tactical model QLT-AGG-QualityRelease with
// its ReleaseRevocation, SM-QLT-RELEASE, QLT-INV-001, QLT-INV-002; F-06, BT21, baseline 7.5).
//
// A release is our authorized QC's decision that a quantity of a subject at its specification may
// serve shipments within a validity interval. It is granted once and never edited; it is never
// above the conforming quantity of the subject's valid inspections at that specification less
// every quantity released before (QLT-INV-001). Revoked releases keep counting in full: QLT does
// not know what FUL shipped on them, so a revocation never gives quantity back, and neither does a
// re-inspection or a replayed message (baseline 7.5). Expiry is derived at an instant, never
// written (tactical principle 3). A revocation freezes what FUL has not shipped on the release —
// FUL's guard refuses it from then on — and leaves what it shipped; a second revocation changes
// nothing (QLT-INV-002, BT21).
package release

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/domain/inspection"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// States of a release (SM-QLT-RELEASE); StateNotYetValid and StateExpired are derived.
const (
	StatusGranted    = "granted"
	StatusRevoked    = "revoked"
	StateNotYetValid = "not_yet_valid"
	StateExpired     = "expired"
)

// Codes the release rules give besides the shared kernel codes, with their categories.
const (
	CodeQualityReleaseExceedsConforming = "QUALITY_RELEASE_EXCEEDS_CONFORMING"
	CodeQualityReleaseRequired          = "QUALITY_RELEASE_REQUIRED"
)

// Codes are the codes of this package and their categories (error catalog V0.1).
var Codes = map[string]kernel.Category{
	CodeQualityReleaseExceedsConforming: kernel.CategoryInvalid,
	CodeQualityReleaseRequired:          kernel.CategoryConflict,
	inspection.CodeReferenceStale:       kernel.CategoryConflict,
	inspection.CodeBusinessKeyConflict:  kernel.CategoryConflict,
}

func fail(code, path, reason string) error { return inspection.Fail(Codes, code, path, reason) }

// Revocation is the ReleaseRevocation entity.
type Revocation struct {
	Reason   string
	Evidence []string
	Revoked  inspection.Act
}

// Release is the aggregate root: row version 1 granted, 2 revoked. Conforming and ReleasedBefore
// are the quantities the grant was judged on, InspectionIDs the valid inspections they came from.
type Release struct {
	ID             string
	Status         string
	RowVersion     int64
	Subject        inspection.Subject
	Quantity       kernel.Decimal
	ValidFrom      time.Time
	ValidUntil     time.Time
	Conforming     kernel.Decimal
	ReleasedBefore kernel.Decimal
	InspectionIDs  []string
	Evidence       []string
	RequestHash    string
	OwnerActor     string
	OwnerTeam      string
	Granted        inspection.Act
	Revocation     *Revocation
}

// Counts reports whether the release counts against a subject's conforming quantity at spec.
// Revoked releases count: what FUL shipped on them is unknown to QLT.
func (r *Release) Counts(spec inspection.Spec) bool { return r.Subject.Spec() == spec }

// State is the release's state at instant at: revoked, not_yet_valid before its validity, expired
// from its end, granted within it.
func (r *Release) State(at time.Time) string {
	at = kernel.Instant(at)
	switch {
	case r.Status == StatusRevoked:
		return StatusRevoked
	case at.Before(r.ValidFrom):
		return StateNotYetValid
	case !at.Before(r.ValidUntil):
		return StateExpired
	}
	return StatusGranted
}

// Code is QualityReleaseGuard v1's code at instant at: "" when the release serves a shipment then,
// QUALITY_RELEASE_REQUIRED otherwise.
func (r *Release) Code(at time.Time) string {
	if r.State(at) == StatusGranted {
		return ""
	}
	return CodeQualityReleaseRequired
}

// Released returns the quantity released on a subject at spec, revoked releases included.
func Released(releases []*Release, spec inspection.Spec) kernel.Decimal {
	var sum kernel.Decimal
	for _, r := range releases {
		if r.Counts(spec) {
			sum = sum.Add(r.Quantity)
		}
	}
	return sum
}

// Request is a quality_release.grant as the rules judge it; the api checked its structure.
type Request struct {
	ID          string
	Subject     inspection.SubjectRef
	Quantity    kernel.Decimal
	ValidFrom   time.Time // zero: the decision's instant
	ValidUntil  time.Time
	Evidence    []string
	RequestHash string
}

// Inputs are the facts a grant is judged on.
type Inputs struct {
	Subject     *inspection.Subject      // PUR's subject now; nil when PUR shows the caller none
	SubjectCode string                   // PUR's reason the subject does not serve now
	Inspections []*inspection.Inspection // the subject's inspections, locked
	Releases    []*Release               // the subject's releases
	Sources     map[string]string        // the EVD sources the granter reads
	Granter     kernel.ActorRef
	Epoch       int64
	Team        string
	Now         time.Time
}

// Grant judges req on in (QLT-INV-001). It refuses, in this order: a subject PUR does not show
// (REFERENCE_INVALID) or that no longer serves (REFERENCE_STALE); a validity that has ended at the
// decision (INVALID_KNOWN_VALUE); a quantity above the conforming quantity of the subject's valid
// inspections at the subject's specification less what was released at it before
// (QUALITY_RELEASE_EXCEEDS_CONFORMING); evidence the granter does not read (REFERENCE_INVALID). That
// the granter is our authorized human (not AI, not system, not an agency) is the command rule's.
func Grant(req Request, in Inputs) (*Release, error) {
	s := in.Subject
	now := kernel.Instant(in.Now)
	switch {
	case s == nil:
		return nil, fail(kernel.CodeReferenceInvalid, "/args/subject", "PUR shows no such subject")
	case in.SubjectCode != "":
		return nil, fail(inspection.CodeReferenceStale, "/args/subject", "The subject does not serve a release: "+in.SubjectCode)
	case !kernel.Instant(req.ValidUntil).After(now):
		return nil, fail(kernel.CodeInvalidKnownValue, "/args/valid_until", "The validity has ended")
	}
	from := now
	if !req.ValidFrom.IsZero() {
		from = kernel.Instant(req.ValidFrom)
	}
	spec := s.Spec()
	_, conforming, counted := inspection.Totals(in.Inspections, spec)
	released := Released(in.Releases, spec)
	if req.Quantity.Cmp(conforming.Sub(released)) > 0 {
		return nil, fail(CodeQualityReleaseExceedsConforming, "/args/quantity",
			"The release exceeds the conforming quantity of the valid inspections less what was released")
	}
	if err := inspection.Readable(req.Evidence, in.Sources, "/args/evidence_ids"); err != nil {
		return nil, err
	}
	return &Release{ID: req.ID, Status: StatusGranted, RowVersion: 1, Subject: *s, Quantity: req.Quantity, ValidFrom: from,
		ValidUntil: kernel.Instant(req.ValidUntil), Conforming: conforming, ReleasedBefore: released, InspectionIDs: counted,
		Evidence: slices.Clone(req.Evidence), RequestHash: req.RequestHash, OwnerActor: in.Granter.ID, OwnerTeam: in.Team,
		Granted: inspection.Act{By: in.Granter, Epoch: in.Epoch, At: now}}, nil
}

// Answer decides a grant whose release ID is taken: the same request by the same granter is
// answered from the stored release; anything else is BUSINESS_KEY_CONFLICT.
func (r *Release) Answer(requestHash string, granter kernel.ActorRef) error {
	if r.RequestHash != requestHash || r.Granted.By != granter {
		return fail(inspection.CodeBusinessKeyConflict, "/args/release_id", "The release ID holds another release")
	}
	return nil
}

// Revoke revokes r for reason (quality_release.revoke). It returns false, changing nothing, for a
// release revoked before (QLT-INV-002: a repeated revocation is idempotent); it refuses evidence
// the revoker does not read (REFERENCE_INVALID).
func (r *Release) Revoke(reason string, evidence []string, sources map[string]string, act inspection.Act) (bool, error) {
	if r.Status == StatusRevoked {
		return false, nil
	}
	if err := inspection.Readable(evidence, sources, "/args/evidence_ids"); err != nil {
		return false, err
	}
	act.At = kernel.Instant(act.At)
	r.Status, r.RowVersion = StatusRevoked, r.RowVersion+1
	r.Revocation = &Revocation{Reason: reason, Evidence: slices.Clone(evidence), Revoked: act}
	return true, nil
}
