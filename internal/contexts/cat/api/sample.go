package api

import (
	"context"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Sample approvals (CAT-AGG-SampleApproval, BC-AT23). sample.save_draft is open to a human or an
// AI agent. sample.approve is human only, already registered for the quality inspector, and must
// be someone other than the draft's last editor. sample.revoke is human only. Approving binds the
// draft's specification revision. Revoking makes SampleGuard v1 fail closed.
const (
	EventSampleApproved = "idp-brant.cat.sample_approved.v1"
	EventSampleRevoked  = "idp-brant.cat.sample_revoked.v1"

	CommandSaveSample    = "sample.save_draft"
	CommandApproveSample = "sample.approve"
	CommandRevokeSample  = "sample.revoke"

	StageProto    = "proto"
	StageFit      = "fit"
	StagePPSample = "pp_sample"
	StageShipment = "shipment"
)

// SaveSample creates an approval at ExpectedVersion 0 or replaces its draft. SkuID and SampleStage
// are fixed by the first save and unique together in the company (BUSINESS_KEY_CONFLICT).
type SaveSample struct {
	IdempotencyKey   string
	SampleApprovalID string
	ExpectedVersion  int64
	SkuID            string
	SpecRevision     int64
	SampleStage      string
	TechPackSHA256   string
	EvidenceIDs      []string
}

// ApproveSample binds the draft revision. The caller must not be the draft's last editor
// (INDEPENDENT_REVIEW_REQUIRED). EvidenceIDs are the draft's evidence, which the approver can read.
type ApproveSample struct {
	IdempotencyKey   string
	SampleApprovalID string
	ExpectedVersion  int64
	Revision         int64
	EvidenceIDs      []string
}

// RevokeSample withdraws the current approval and records the reason.
type RevokeSample struct {
	IdempotencyKey   string
	SampleApprovalID string
	ExpectedVersion  int64
	Reason           string
}

// SampleRevisionView is one stored revision.
type SampleRevisionView struct {
	Revision       int64
	SpecRevision   int64
	TechPackSHA256 string
	EvidenceIDs    []string
	Status         string
	SavedBy        kernel.ActorRef
	ApprovedBy     kernel.ActorRef
	RevokeReason   string
	RevokedBy      kernel.ActorRef
}

// SampleApprovalView is an approval and its revisions.
type SampleApprovalView struct {
	SampleApprovalID string
	SkuID            string
	SampleStage      string
	Status           string
	RootVersion      int64
	CurrentRevision  int64
	Revisions        []SampleRevisionView
}

// SampleCommands is the sample-approval command entry point.
type SampleCommands interface {
	SaveSample(ctx context.Context, cmd SaveSample) (Receipt, error)
	ApproveSample(ctx context.Context, cmd ApproveSample) (Receipt, error)
	RevokeSample(ctx context.Context, cmd RevokeSample) (Receipt, error)
}

// SampleQueries is the sample-approval read.
type SampleQueries interface {
	GetSampleApproval(ctx context.Context, id string) (SampleApprovalView, error)
}

func (c SaveSample) Validate() error {
	if err := head(c.IdempotencyKey, c.SampleApprovalID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	if !kernel.ValidIdentifier(c.SkuID) || c.SpecRevision < 1 {
		return schema("/args/sku_id")
	}
	if !sampleStage(c.SampleStage) {
		return schema("/args/sample_stage")
	}
	if !sha256Text(c.TechPackSHA256) {
		return schema("/args/tech_pack_sha256")
	}
	return evidenceIDs(c.EvidenceIDs, "/args/evidence_ids")
}

func (c ApproveSample) Validate() error {
	if err := head(c.IdempotencyKey, c.SampleApprovalID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if c.Revision < 1 {
		return schema("/args/revision")
	}
	return evidenceIDs(c.EvidenceIDs, "/args/evidence_ids")
}

func (c RevokeSample) Validate() error {
	if err := head(c.IdempotencyKey, c.SampleApprovalID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if !text(c.Reason, 1000) {
		return schema("/args/reason")
	}
	return nil
}

func sampleStage(s string) bool {
	switch s {
	case StageProto, StageFit, StagePPSample, StageShipment:
		return true
	default:
		return false
	}
}

func sha256Text(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
