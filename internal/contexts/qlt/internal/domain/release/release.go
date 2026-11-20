// Package release holds QLT's QualityRelease aggregate (tactical model QLT-AGG-QualityRelease with
// its ReleaseRevocation, FrozenQuantity and ConcessionFlag, SM-QLT-RELEASE, QLT-INV-001,
// QLT-INV-002; F-06, BT21, baseline 7.5; decision S5R-D18).
//
// A release is our authorized QC's decision that a quantity of a subject at its specification may
// serve shipments within a validity interval. It is granted once and never edited; it is never
// above the conforming quantity of the subject's valid inspections at that specification plus the
// quantity their dispositions accept by concession, less what the releases before count
// (QLT-INV-001). The part of a release beyond the conforming quantity left is conceded goods: the
// release carries it as its concession quantity and flag. Expiry is derived at an instant, never
// written (tactical principle 3).
//
// A revocation freezes what FUL has not shipped on the release — FUL's guard refuses it from then on
// — and leaves what it shipped: it records FUL's shipped quantity (ReleaseConsumption v1, R79) and
// the frozen quantity, the release's quantity less it. A second revocation changes nothing
// (QLT-INV-002, BT21). A revoked release counts in full until every inspection it was judged on has
// been inspected again after the revocation (a re-inspection supersedes it and restates the result);
// from then on it counts only what was shipped on it, so the frozen quantity may be released again
// on the restated result. A revocation recorded before S5R-D18 knows no shipped quantity and keeps
// counting in full. A replayed message changes nothing (baseline 7.5).
package release

import (
	"fmt"
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

// Revocation is the ReleaseRevocation entity. Shipped is what FUL had shipped on the release when it
// was revoked (ReleaseConsumption v1, R79), nil for a revocation recorded before S5R-D18; the rest
// of the release's quantity is frozen.
type Revocation struct {
	Reason   string
	Evidence []string
	Revoked  inspection.Act
	Shipped  *kernel.Decimal
}

// Release is the aggregate root: row version 1 granted, 2 revoked. Conforming, Conceded and
// ReleasedBefore are the quantities the grant was judged on, InspectionIDs the valid inspections they
// came from; ConcessionQuantity is the part of Quantity that is conceded goods (S5R-D18).
type Release struct {
	ID                 string
	Status             string
	RowVersion         int64
	Subject            inspection.Subject
	Quantity           kernel.Decimal
	ValidFrom          time.Time
	ValidUntil         time.Time
	Conforming         kernel.Decimal
	Conceded           kernel.Decimal
	ReleasedBefore     kernel.Decimal
	ConcessionQuantity kernel.Decimal
	InspectionIDs      []string
	Evidence           []string
	RequestHash        string
	OwnerActor         string
	OwnerTeam          string
	Granted            inspection.Act
	Revocation         *Revocation
}

// Counts reports whether the release counts against a subject's releasable quantity at spec.
func (r *Release) Counts(spec inspection.Spec) bool { return r.Subject.Spec() == spec }

// Concession reports whether the release carries conceded goods (the ConcessionFlag, S5R-D18).
func (r *Release) Concession() bool { return r.ConcessionQuantity.Sign() > 0 }

// Frozen returns the quantity the revocation froze: the release's quantity less what FUL had shipped
// on it; zero for a release not revoked or revoked before S5R-D18.
func (r *Release) Frozen() kernel.Decimal {
	if r.Revocation == nil || r.Revocation.Shipped == nil {
		return kernel.Decimal{}
	}
	return r.Quantity.Sub(*r.Revocation.Shipped)
}

// Thawed reports whether the frozen quantity of a revoked release may be released again: the
// revocation recorded what FUL shipped, and every inspection the release was judged on has been
// inspected again since (inspection.Reinspected over the subject's inspections, superseded ones
// included; QLT-INV-002, S5R-D18).
func (r *Release) Thawed(inspections []*inspection.Inspection) bool {
	return r.Status == StatusRevoked && r.Revocation != nil && r.Revocation.Shipped != nil &&
		inspection.Reinspected(inspections, r.InspectionIDs, r.Revocation.Revoked.At)
}

// Counted returns what the release counts against its subject's releasable quantity, given the
// subject's inspections: its quantity, except for a revoked release whose frozen quantity was
// inspected again, which counts what FUL shipped on it (QLT-INV-001, S5R-D18).
func (r *Release) Counted(inspections []*inspection.Inspection) kernel.Decimal {
	if r.Thawed(inspections) {
		return *r.Revocation.Shipped
	}
	return r.Quantity
}

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

// Released returns what the releases of a subject at spec count against its releasable quantity,
// given its inspections (Counted).
func Released(releases []*Release, inspections []*inspection.Inspection, spec inspection.Spec) kernel.Decimal {
	var sum kernel.Decimal
	for _, r := range releases {
		if r.Counts(spec) {
			sum = sum.Add(r.Counted(inspections))
		}
	}
	return sum
}

// Frozen returns the frozen quantity of the revoked releases of a subject at spec that still counts
// because it has not been inspected again (QLT-INV-002).
func Frozen(releases []*Release, inspections []*inspection.Inspection, spec inspection.Spec) kernel.Decimal {
	var sum kernel.Decimal
	for _, r := range releases {
		if r.Counts(spec) && r.Status == StatusRevoked && !r.Thawed(inspections) {
			sum = sum.Add(r.Frozen())
		}
	}
	return sum
}

// Releasable returns what may still be released on a subject at spec: the conforming quantity of
// its valid inspections plus their approved concessions less what its releases count, never below
// zero (QLT-INV-001).
func Releasable(inspections []*inspection.Inspection, releases []*Release, spec inspection.Spec) kernel.Decimal {
	_, conforming, _ := inspection.Totals(inspections, spec)
	rest := conforming.Add(inspection.Concessions(inspections, spec)).Sub(Released(releases, inspections, spec))
	if rest.Sign() < 0 {
		return kernel.Decimal{}
	}
	return rest
}

// ConcessionPart returns the part of a release of quantity that is conceded goods, when its subject
// has conforming at its specification and its releases count released: what exceeds the conforming
// quantity left (conforming goods are released first; S5R-D18).
func ConcessionPart(quantity, conforming, released kernel.Decimal) kernel.Decimal {
	left := conforming.Sub(released)
	if left.Sign() < 0 {
		left = kernel.Decimal{}
	}
	part := quantity.Sub(left)
	if part.Sign() < 0 {
		return kernel.Decimal{}
	}
	return part
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
// inspections at the subject's specification plus their approved concessions less what the
// releases at it count (QUALITY_RELEASE_EXCEEDS_CONFORMING: a revoked release's frozen quantity
// counts until it is inspected again, S5R-D18); evidence the granter does not read
// (REFERENCE_INVALID). That the granter is our authorized human (not AI, not system, not an agency)
// is the command rule's.
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
	conceded := inspection.Concessions(in.Inspections, spec)
	released := Released(in.Releases, in.Inspections, spec)
	if req.Quantity.Cmp(conforming.Add(conceded).Sub(released)) > 0 {
		return nil, fail(CodeQualityReleaseExceedsConforming, "/args/quantity",
			"The release exceeds the conforming quantity of the valid inspections and their approved concessions less what was released")
	}
	if err := inspection.Readable(req.Evidence, in.Sources, "/args/evidence_ids"); err != nil {
		return nil, err
	}
	return &Release{ID: req.ID, Status: StatusGranted, RowVersion: 1, Subject: *s, Quantity: req.Quantity, ValidFrom: from,
		ValidUntil: kernel.Instant(req.ValidUntil), Conforming: conforming, Conceded: conceded, ReleasedBefore: released,
		ConcessionQuantity: ConcessionPart(req.Quantity, conforming, released), InspectionIDs: counted, Evidence: slices.Clone(req.Evidence),
		RequestHash: req.RequestHash, OwnerActor: in.Granter.ID, OwnerTeam: in.Team, Granted: inspection.Act{By: in.Granter, Epoch: in.Epoch, At: now}}, nil
}

// Answer decides a grant whose release ID is taken: the same request by the same granter is
// answered from the stored release; anything else is BUSINESS_KEY_CONFLICT.
func (r *Release) Answer(requestHash string, granter kernel.ActorRef) error {
	if r.RequestHash != requestHash || r.Granted.By != granter {
		return fail(inspection.CodeBusinessKeyConflict, "/args/release_id", "The release ID holds another release")
	}
	return nil
}

// Revoke revokes r for reason (quality_release.revoke), recording shipped, what FUL shipped on it
// (ReleaseConsumption v1, read after the release row was locked); the rest is frozen (QLT-INV-002,
// S5R-D18). It returns false, changing nothing, for a release revoked before (a repeated revocation
// is idempotent); it refuses evidence the revoker does not read (REFERENCE_INVALID). A shipped
// quantity below zero or above the release's quantity is an error: FUL never ships more than a
// release releases.
func (r *Release) Revoke(reason string, evidence []string, sources map[string]string, shipped kernel.Decimal, act inspection.Act) (bool, error) {
	if r.Status == StatusRevoked {
		return false, nil
	}
	if shipped.Sign() < 0 || shipped.Cmp(r.Quantity) > 0 {
		return false, fmt.Errorf("qlt: FUL shipped %s on release %s of %s", shipped, r.ID, r.Quantity)
	}
	if err := inspection.Readable(evidence, sources, "/args/evidence_ids"); err != nil {
		return false, err
	}
	act.At = kernel.Instant(act.At)
	r.Status, r.RowVersion = StatusRevoked, r.RowVersion+1
	r.Revocation = &Revocation{Reason: reason, Evidence: slices.Clone(evidence), Revoked: act, Shipped: &shipped}
	return true, nil
}
