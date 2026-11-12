package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Review requests, approvals and margin exceptions of a quote root (migrations/trd/000002), loaded
// with the root and written in its transaction after its revisions.

func loadReviews(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*quote.Review, error) {
	rows, err := exec.Query(ctx, `SELECT review_id, revision, business_hash, target_command, coalesce(expected_active_revision, 0),
		margin_exception_required, status, requested_by_kind, requested_by_id, requested_epoch, requested_at, decided_by_kind, decided_by_id,
		decided_epoch, decided_at, decision_reason, decision_evidence
		FROM brant_trd.quote_review WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3 ORDER BY requested_at, review_id`,
		string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*quote.Review, error) {
		var r quote.Review
		var status, kind string
		var decidedKind, decidedID, reason, proofs *string
		var decidedEpoch *int64
		var decidedAt *time.Time
		err := row.Scan(&r.ID, &r.Revision, &r.BusinessHash, &r.Target, &r.ExpectedActive, &r.MarginExceptionRequired, &status, &kind,
			&r.Requested.By.ID, &r.Requested.Epoch, &r.Requested.At, &decidedKind, &decidedID, &decidedEpoch, &decidedAt, &reason, &proofs)
		if err != nil {
			return nil, err
		}
		r.Status, r.Requested.By.Kind, r.Requested.At = quote.ReviewStatus(status), kernel.ActorKind(kind), kernel.Instant(r.Requested.At)
		if decidedAt != nil {
			a, err := act(*decidedKind, *decidedID, *decidedEpoch, *decidedAt, reason, *proofs)
			if err != nil {
				return nil, err
			}
			r.Decided = &a
		}
		return &r, nil
	})
}

func loadApprovals(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*quote.Approval, error) {
	rows, err := exec.Query(ctx, `SELECT a.approval_id, a.review_id, a.revision, a.business_hash, a.target_command,
		coalesce(a.expected_active_revision, 0), a.normalization_version, a.calculation_version, a.pricing_policy_kind, a.pricing_policy_id,
		a.pricing_policy_revision, a.approved_by_kind, a.approved_by_id, a.approved_epoch, a.approved_at, a.approval_reason,
		a.approval_evidence, a.expires_at, a.effectivity, a.effectivity_version, a.revoked_by_kind, a.revoked_by_id, a.revoke_epoch,
		a.revoked_at, a.revoke_reason, a.revoke_evidence,
		e.business_hash, e.normalization_version, e.calculation_version, e.pricing_policy_kind, e.pricing_policy_id,
		e.pricing_policy_revision, e.minimum_margin, e.shortfall, e.currency, e.approved_by_kind, e.approved_by_id, e.approved_epoch,
		e.approved_at, e.reason, e.evidence
		FROM brant_trd.quote_approval a LEFT JOIN brant_trd.quote_margin_exception e ON e.tenant_id = a.tenant_id
			AND e.company_id = a.company_id AND e.quote_id = a.quote_id AND e.approval_id = a.approval_id
		WHERE a.tenant_id = $1 AND a.company_id = $2 AND a.quote_id = $3 ORDER BY a.approved_at, a.approval_id`,
		string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*quote.Approval, error) {
		var a quote.Approval
		var approvedKind, approvedProofs, effectivity string
		var approvedReason *string
		var approvedAt time.Time
		var approvedEpoch int64
		var revokedKind, revokedID, revokeReason, revokeProofs *string
		var revokeEpoch *int64
		var revokedAt *time.Time
		var eHash, eNorm, eCalc, ePolicyKind, ePolicyID, eMinimum, eShortfall, eCurrency, eKind, eID, eReason, eProofs *string
		var ePolicyRevision, eEpoch *int64
		var eAt *time.Time
		err := row.Scan(&a.ID, &a.ReviewID, &a.Revision, &a.BusinessHash, &a.Target, &a.ExpectedActive, &a.Rules.NormalizationVersion,
			&a.Rules.CalculationVersion, &a.Rules.PricingPolicy.Kind, &a.Rules.PricingPolicy.ID, &a.Rules.PricingPolicy.Revision,
			&approvedKind, &a.Approved.By.ID, &approvedEpoch, &approvedAt, &approvedReason, &approvedProofs, &a.ExpiresAt, &effectivity,
			&a.EffectivityVersion, &revokedKind, &revokedID, &revokeEpoch, &revokedAt, &revokeReason, &revokeProofs,
			&eHash, &eNorm, &eCalc, &ePolicyKind, &ePolicyID, &ePolicyRevision, &eMinimum, &eShortfall, &eCurrency, &eKind, &eID, &eEpoch,
			&eAt, &eReason, &eProofs)
		if err != nil {
			return nil, err
		}
		if a.Approved, err = act(approvedKind, a.Approved.By.ID, approvedEpoch, approvedAt, approvedReason, approvedProofs); err != nil {
			return nil, err
		}
		a.ExpiresAt, a.Effectivity = kernel.Instant(a.ExpiresAt), quote.Effectivity(effectivity)
		if revokedAt != nil {
			r, err := act(*revokedKind, *revokedID, *revokeEpoch, *revokedAt, revokeReason, *revokeProofs)
			if err != nil {
				return nil, err
			}
			a.Revocation = &r
		}
		if eHash != nil {
			e := &quote.MarginException{BusinessHash: *eHash, Rules: quote.RuleVersions{NormalizationVersion: *eNorm, CalculationVersion: *eCalc,
				PricingPolicy: quote.Ref{Kind: *ePolicyKind, ID: *ePolicyID, Revision: *ePolicyRevision}}, MinimumMargin: *eMinimum,
				Shortfall: *eShortfall, Currency: *eCurrency}
			if e.Approved, err = act(*eKind, *eID, *eEpoch, *eAt, eReason, *eProofs); err != nil {
				return nil, err
			}
			a.Exception = e
		}
		return &a, nil
	})
}

// act is a stored act; reason is nil when none was given.
func act(kind, id string, epoch int64, at time.Time, reason *string, proofs string) (quote.Act, error) {
	a := quote.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(kind), ID: id}, Epoch: epoch, At: kernel.Instant(at)}
	if reason != nil {
		a.Reason = *reason
	}
	if err := json.Unmarshal([]byte(proofs), &a.Evidence); err != nil {
		return quote.Act{}, err
	}
	if len(a.Evidence) == 0 {
		a.Evidence = nil
	}
	return a, nil
}

// proofsOf is evidence as stored: a JSON list, empty for none.
func proofsOf(es []quote.Evidence) (string, error) {
	if es == nil {
		es = []quote.Evidence{}
	}
	doc, err := json.Marshal(es)
	return string(doc), err
}

func reasonOf(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func writeReview(ctx context.Context, exec bentopg.Executor, t, c, id string, r *quote.Review) error {
	if r.Fresh {
		var expected any
		if r.ExpectedActive > 0 {
			expected = r.ExpectedActive
		}
		_, err := exec.Exec(ctx, `INSERT INTO brant_trd.quote_review (tenant_id, company_id, quote_id, review_id, revision, business_hash,
			target_command, expected_active_revision, margin_exception_required, status, requested_by_kind, requested_by_id, requested_epoch,
			requested_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			t, c, id, r.ID, r.Revision, r.BusinessHash, r.Target, expected, r.MarginExceptionRequired, string(r.Status),
			string(r.Requested.By.Kind), r.Requested.By.ID, r.Requested.Epoch, r.Requested.At)
		if err != nil || r.Decided == nil {
			return err
		}
	} else if !r.Changed {
		return nil
	}
	d := r.Decided
	proofs, err := proofsOf(d.Evidence)
	if err != nil {
		return err
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_trd.quote_review SET status = $5, decided_by_kind = $6, decided_by_id = $7, decided_epoch = $8,
		decided_at = $9, decision_reason = $10, decision_evidence = $11
		WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3 AND review_id = $4 AND status = 'open'`,
		t, c, id, r.ID, string(r.Status), string(d.By.Kind), d.By.ID, d.Epoch, d.At, reasonOf(d.Reason), proofs)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

func writeApproval(ctx context.Context, exec bentopg.Executor, t, c, id string, a *quote.Approval) error {
	if a.Fresh {
		var expected any
		if a.ExpectedActive > 0 {
			expected = a.ExpectedActive
		}
		proofs, err := proofsOf(a.Approved.Evidence)
		if err != nil {
			return err
		}
		p := a.Rules.PricingPolicy
		_, err = exec.Exec(ctx, `INSERT INTO brant_trd.quote_approval (tenant_id, company_id, quote_id, approval_id, review_id, revision,
			business_hash, target_command, expected_active_revision, normalization_version, calculation_version, pricing_policy_kind,
			pricing_policy_id, pricing_policy_revision, approved_by_kind, approved_by_id, approved_epoch, approved_at, approval_reason,
			approval_evidence, expires_at, effectivity, effectivity_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)`,
			t, c, id, a.ID, a.ReviewID, a.Revision, a.BusinessHash, a.Target, expected, a.Rules.NormalizationVersion,
			a.Rules.CalculationVersion, p.Kind, p.ID, p.Revision, string(a.Approved.By.Kind), a.Approved.By.ID, a.Approved.Epoch,
			a.Approved.At, reasonOf(a.Approved.Reason), proofs, a.ExpiresAt, string(a.Effectivity), a.EffectivityVersion)
		if err != nil {
			return err
		}
	} else if a.Changed {
		r := a.Revocation
		proofs, err := proofsOf(r.Evidence)
		if err != nil {
			return err
		}
		tag, err := exec.Exec(ctx, `UPDATE brant_trd.quote_approval SET effectivity = $5, effectivity_version = $6, revoked_by_kind = $7,
			revoked_by_id = $8, revoke_epoch = $9, revoked_at = $10, revoke_reason = $11, revoke_evidence = $12
			WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3 AND approval_id = $4 AND effectivity_version = $6 - 1`,
			t, c, id, a.ID, string(a.Effectivity), a.EffectivityVersion, string(r.By.Kind), r.By.ID, r.Epoch, r.At, r.Reason, proofs)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
	}
	if e := a.Exception; e != nil && e.Fresh {
		proofs, err := proofsOf(e.Approved.Evidence)
		if err != nil {
			return err
		}
		p := e.Rules.PricingPolicy
		_, err = exec.Exec(ctx, `INSERT INTO brant_trd.quote_margin_exception (tenant_id, company_id, quote_id, approval_id, business_hash,
			normalization_version, calculation_version, pricing_policy_kind, pricing_policy_id, pricing_policy_revision, minimum_margin,
			shortfall, currency, approved_by_kind, approved_by_id, approved_epoch, approved_at, reason, evidence)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`,
			t, c, id, a.ID, e.BusinessHash, e.Rules.NormalizationVersion, e.Rules.CalculationVersion, p.Kind, p.ID, p.Revision, e.MinimumMargin,
			e.Shortfall, e.Currency, string(e.Approved.By.Kind), e.Approved.By.ID, e.Approved.Epoch, e.Approved.At, e.Approved.Reason, proofs)
		return err
	}
	return nil
}
