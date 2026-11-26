// Package sampleapproval is the sample-approval aggregate (CAT-AGG-SampleApproval, BC-AT23). One
// approval is kept per SKU and sample stage. A draft names a confirmed specification revision;
// sample.approve binds that revision and is refused to the draft's last editor; sample.revoke
// records a reason and leaves no current approval. An older approval does not move forward when
// the SKU is confirmed again: SampleGuard v1 answers that, and this aggregate only changes state
// when a command says so.
package sampleapproval

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	// CodeBusinessKeyConflict is BUSINESS_KEY_CONFLICT (409): another approval has this SKU and stage.
	CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"
	// CodeIndependentReviewRequired is INDEPENDENT_REVIEW_REQUIRED (422): the approver last saved the draft.
	CodeIndependentReviewRequired = "INDEPENDENT_REVIEW_REQUIRED"
)

// Status of the root or of a revision. A root is draft, approved or revoked. A revision is also
// superseded.
type Status string

const (
	Draft      Status = "draft"
	Approved   Status = "approved"
	Superseded Status = "superseded"
	Revoked    Status = "revoked"
)

// Stages a sample approval binds.
const (
	StageProto    = "proto"
	StageFit      = "fit"
	StagePPSample = "pp_sample"
	StageShipment = "shipment"
)

// DraftContent is what a draft revision stores.
type DraftContent struct {
	SpecRevision   int64
	TechPackSHA256 string
	EvidenceIDs    []string
}

// Revision is one submission or decision.
type Revision struct {
	Number         int64
	SpecRevision   int64
	TechPackSHA256 string
	EvidenceIDs    []string
	Status         Status
	SavedBy        kernel.ActorRef
	SavedEpoch     int64
	SavedAt        time.Time
	ApprovedBy     kernel.ActorRef
	ApproveEpoch   int64
	ApprovedAt     time.Time
	RevokeReason   string
	RevokedBy      kernel.ActorRef
	RevokeEpoch    int64
	RevokedAt      time.Time
	SupersededAt   time.Time
}

// Approval is the aggregate root. Current is the revision the guard reads, 0 before the first
// approval. Approved is that revision while it is still approved.
type Approval struct {
	ID         string
	SkuID      string
	Stage      string
	Status     Status
	Version    int64
	Last       int64
	Current    int64
	OwnerActor string
	OwnerTeam  string
	Draft      *Revision
	Approved   *Revision
}

// New returns an approval that is not stored yet.
func New(id, sku, stage, owner, team string) *Approval {
	return &Approval{ID: id, SkuID: sku, Stage: stage, Status: Draft, OwnerActor: owner, OwnerTeam: team}
}

// CheckKey refuses a second approval of the same SKU and stage.
func CheckKey(taken bool) error {
	if taken {
		return kernel.RejectAs(CodeBusinessKeyConflict, kernel.CategoryConflict, "/args/id", "")
	}
	return nil
}

// CheckSave admits a whole draft. The SKU and stage are fixed by the first save.
func CheckSave(a *Approval, expected int64, sku, stage string) error {
	if a == nil {
		if expected != 0 {
			return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
		}
		return nil
	}
	if a.Version != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	switch {
	case sku != a.SkuID:
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/sku_id", "")
	case stage != a.Stage:
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/sample_stage", "")
	}
	return nil
}

// Save replaces the draft or starts the next revision. The root status stays: a new draft does not
// withdraw an approval that is still current.
func (a *Approval) Save(c DraftContent, by kernel.ActorRef, epoch int64, at time.Time) (*Revision, bool) {
	created := a.Draft == nil
	if created {
		a.Last++
		a.Draft = &Revision{Number: a.Last, Status: Draft}
	}
	d := a.Draft
	d.SpecRevision, d.TechPackSHA256 = c.SpecRevision, c.TechPackSHA256
	d.EvidenceIDs = slices.Sorted(slices.Values(c.EvidenceIDs))
	d.SavedBy, d.SavedEpoch, d.SavedAt = by, epoch, at
	a.Version++
	return d, created
}

// CheckApprove admits approving revision number by the human by. The approver is not the draft's
// last editor.
func (a *Approval) CheckApprove(expected, number int64, by kernel.ActorRef) (*Revision, error) {
	if a == nil {
		return nil, kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	}
	d := a.Draft
	switch {
	case a.Version != expected:
		return nil, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	case number > a.Last:
		return nil, kernel.Reject(kernel.CodeNotFound, "/args/revision", "")
	case d == nil || d.Number != number:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/revision", "")
	case d.SavedBy.ID == by.ID:
		return nil, kernel.RejectAs(CodeIndependentReviewRequired, kernel.CategoryInvalid, "/args/revision", "The verifier last edited this revision")
	}
	return d, nil
}

// Approve binds the draft and supersedes the approval that was current. Call it after CheckApprove.
func (a *Approval) Approve(by kernel.ActorRef, epoch int64, at time.Time) *Revision {
	superseded := a.Approved
	if superseded != nil {
		superseded.Status, superseded.SupersededAt = Superseded, at
	}
	d := a.Draft
	d.Status = Approved
	d.ApprovedBy, d.ApproveEpoch, d.ApprovedAt = by, epoch, at
	a.Approved, a.Draft = d, nil
	a.Current = d.Number
	a.Status = Approved
	a.Version++
	return superseded
}

// CheckRevoke admits revoking the current approval.
func (a *Approval) CheckRevoke(expected int64) (*Revision, error) {
	switch {
	case a == nil:
		return nil, kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	case a.Version != expected:
		return nil, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	case a.Status != Approved || a.Approved == nil:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/id", "")
	}
	return a.Approved, nil
}

// Revoke records the reason on the current approval. The guard then fails closed.
func (a *Approval) Revoke(by kernel.ActorRef, epoch int64, reason string, at time.Time) {
	d := a.Approved
	d.Status = Revoked
	d.RevokeReason, d.RevokedBy, d.RevokeEpoch, d.RevokedAt = reason, by, epoch, at
	a.Approved = nil
	a.Status = Revoked
	a.Version++
}
