package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/sampleapproval"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// SampleRecord returns the approval and the revisions a read shows.
func SampleRecord(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*sampleapproval.Approval, []sampleapproval.Revision, error) {
	a, err := LoadSample(ctx, exec, scope, id, false)
	if err != nil || a == nil {
		return a, nil, err
	}
	loaded, err := loadSampleRevisions(ctx, exec, scope, id)
	if err != nil {
		return nil, nil, err
	}
	revs := make([]sampleapproval.Revision, len(loaded))
	for i, rev := range loaded {
		revs[i] = *rev
	}
	return a, revs, nil
}

// LoadSample returns the approval and its draft and current approved revision, or nil.
func LoadSample(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, forUpdate bool) (*sampleapproval.Approval, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	var r sampleapproval.Approval
	var status string
	var team *string
	err := exec.QueryRow(ctx, `SELECT sample_approval_id, sku_id, sample_stage, status, root_version, last_revision, current_revision,
		owner_actor_id, owner_team_id FROM brant_cat.sample_approval WHERE tenant_id = $1 AND company_id = $2 AND sample_approval_id = $3`+lock,
		string(scope.Tenant), string(scope.Company), id).Scan(&r.ID, &r.SkuID, &r.Stage, &status, &r.Version, &r.Last, &r.Current, &r.OwnerActor, &team)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Status = sampleapproval.Status(status)
	if team != nil {
		r.OwnerTeam = *team
	}
	revs, err := loadSampleRevisions(ctx, exec, scope, id)
	if err != nil {
		return nil, err
	}
	for _, rev := range revs {
		switch rev.Status {
		case sampleapproval.Draft:
			r.Draft = rev
		case sampleapproval.Approved:
			r.Approved = rev
		}
	}
	return &r, nil
}

func loadSampleRevisions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*sampleapproval.Revision, error) {
	rows, err := exec.Query(ctx, `SELECT revision, status, spec_revision, tech_pack_sha256, evidence_ids,
		saved_by_kind, saved_by_id, saved_epoch, saved_at,
		approved_by_kind, approved_by_id, approve_epoch, approved_at,
		coalesce(revoke_reason, ''), revoked_by_kind, revoked_by_id, revoke_epoch, revoked_at, superseded_at
		FROM brant_cat.sample_approval_revision
		WHERE tenant_id = $1 AND company_id = $2 AND sample_approval_id = $3
		ORDER BY revision`, string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*sampleapproval.Revision, error) {
		var rev sampleapproval.Revision
		var status, savedKind string
		var approvedKind, approvedID, revokedKind, revokedID *string
		var approveEpoch, revokeEpoch *int64
		var approvedAt, revokedAt, supersededAt *time.Time
		err := row.Scan(&rev.Number, &status, &rev.SpecRevision, &rev.TechPackSHA256, &rev.EvidenceIDs,
			&savedKind, &rev.SavedBy.ID, &rev.SavedEpoch, &rev.SavedAt,
			&approvedKind, &approvedID, &approveEpoch, &approvedAt,
			&rev.RevokeReason, &revokedKind, &revokedID, &revokeEpoch, &revokedAt, &supersededAt)
		if err != nil {
			return nil, err
		}
		rev.Status = sampleapproval.Status(status)
		rev.SavedBy.Kind = kernel.ActorKind(savedKind)
		rev.SavedAt = kernel.Instant(rev.SavedAt)
		if approvedKind != nil {
			rev.ApprovedBy = kernel.ActorRef{Kind: kernel.ActorKind(*approvedKind), ID: *approvedID}
			rev.ApproveEpoch = *approveEpoch
			rev.ApprovedAt = kernel.Instant(*approvedAt)
		}
		if revokedKind != nil {
			rev.RevokedBy = kernel.ActorRef{Kind: kernel.ActorKind(*revokedKind), ID: *revokedID}
			if revokeEpoch != nil {
				rev.RevokeEpoch = *revokeEpoch
			}
			if revokedAt != nil {
				rev.RevokedAt = *revokedAt
			}
		}
		if supersededAt != nil {
			rev.SupersededAt = *supersededAt
		}
		return &rev, nil
	})
}

// SampleStageTaken reports whether another approval already covers the SKU and stage.
func SampleKeyTaken(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, skuID, stage, exceptID string) (bool, error) {
	var taken bool
	err := exec.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brant_cat.sample_approval
		WHERE tenant_id = $1 AND company_id = $2 AND sku_id = $3 AND sample_stage = $4 AND sample_approval_id <> $5)`,
		string(scope.Tenant), string(scope.Company), skuID, stage, exceptID).Scan(&taken)
	return taken, err
}

// InsertSample stores a new approval root.
func InsertSample(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *sampleapproval.Approval, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cat.sample_approval (tenant_id, company_id, sample_approval_id, sku_id, sample_stage,
		status, root_version, last_revision, current_revision, owner_actor_id, owner_team_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12)`,
		string(scope.Tenant), string(scope.Company), r.ID, r.SkuID, r.Stage, string(r.Status), r.Version, r.Last, r.Current,
		r.OwnerActor, textOrNil(r.OwnerTeam), at)
	return unique(err)
}

// UpdateSample writes the root when it is still at from.
func UpdateSample(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *sampleapproval.Approval, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cat.sample_approval SET status = $4, root_version = $5, last_revision = $6, current_revision = $7, updated_at = $8
		WHERE tenant_id = $1 AND company_id = $2 AND sample_approval_id = $3 AND root_version = $9`,
		string(scope.Tenant), string(scope.Company), r.ID, string(r.Status), r.Version, r.Last, r.Current, at, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return unique(err)
}

// InsertSampleRevision stores a new draft revision.
func InsertSampleRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, a *sampleapproval.Approval, rev *sampleapproval.Revision) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cat.sample_approval_revision (tenant_id, company_id, sample_approval_id, revision,
		sku_id, sample_stage, spec_revision, tech_pack_sha256, evidence_ids, status,
		saved_by_kind, saved_by_id, saved_epoch, saved_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'draft',$10,$11,$12,$13)`,
		string(scope.Tenant), string(scope.Company), a.ID, rev.Number, a.SkuID, a.Stage, rev.SpecRevision, rev.TechPackSHA256, rev.EvidenceIDs,
		string(rev.SavedBy.Kind), rev.SavedBy.ID, rev.SavedEpoch, rev.SavedAt)
	return err
}

// UpdateSampleDraft replaces a draft revision's body.
func UpdateSampleRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, rev *sampleapproval.Revision) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cat.sample_approval_revision SET spec_revision = $5, tech_pack_sha256 = $6, evidence_ids = $7,
		saved_by_kind = $8, saved_by_id = $9, saved_epoch = $10, saved_at = $11
		WHERE tenant_id = $1 AND company_id = $2 AND sample_approval_id = $3 AND revision = $4 AND status = 'draft'`,
		string(scope.Tenant), string(scope.Company), id, rev.Number, rev.SpecRevision, rev.TechPackSHA256, rev.EvidenceIDs,
		string(rev.SavedBy.Kind), rev.SavedBy.ID, rev.SavedEpoch, rev.SavedAt)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// ApproveSampleRevision publishes a draft revision.
func ApproveSampleRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, rev *sampleapproval.Revision) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cat.sample_approval_revision SET status = 'approved', approved_by_kind = 'human',
		approved_by_id = $5, approve_epoch = $6, approved_at = $7
		WHERE tenant_id = $1 AND company_id = $2 AND sample_approval_id = $3 AND revision = $4 AND status = 'draft'`,
		string(scope.Tenant), string(scope.Company), id, rev.Number, rev.ApprovedBy.ID, rev.ApproveEpoch, rev.ApprovedAt)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// SupersedeSampleRevision closes the approval a newer one replaces.
func SupersedeSampleRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, rev *sampleapproval.Revision) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cat.sample_approval_revision SET status = 'superseded', superseded_at = $5
		WHERE tenant_id = $1 AND company_id = $2 AND sample_approval_id = $3 AND revision = $4 AND status = 'approved'`,
		string(scope.Tenant), string(scope.Company), id, rev.Number, rev.SupersededAt)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// RevokeSampleRevision records the reason on the approved revision.
func RevokeSampleRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, rev *sampleapproval.Revision) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cat.sample_approval_revision SET status = 'revoked', revoke_reason = $5,
		revoked_by_kind = 'human', revoked_by_id = $6, revoke_epoch = $7, revoked_at = $8
		WHERE tenant_id = $1 AND company_id = $2 AND sample_approval_id = $3 AND revision = $4 AND status = 'approved'`,
		string(scope.Tenant), string(scope.Company), id, rev.Number, rev.RevokeReason, rev.RevokedBy.ID, rev.RevokeEpoch, rev.RevokedAt)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}
