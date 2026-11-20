package approvalgrant

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/kernel"
)

const selectGrant = `SELECT grant_id, delegation_id, agent_id, task_id, command, request_hash, object_type, object_id,
	object_version, expires_at, status, row_version, issuer_id, coalesce(owner_team_id, ''), issued_epoch, issued_at,
	coalesce(used_key, ''), coalesce(used_by_id, ''), coalesce(used_epoch, 0), used_at, coalesce(revoked_by_id, ''), revoked_at,
	coalesce(revoke_reason, '')
	FROM brant_ai.approval_grant WHERE tenant_id = $1 AND company_id = $2`

// Load reads grant id of scope through exec; nil when there is none.
func Load(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock policy.Lock) (*Grant, error) {
	rows, err := exec.Query(ctx, selectGrant+` AND grant_id = $3`+string(lock), string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	grants, err := pgx.CollectRows(rows, scanGrant)
	if err != nil || len(grants) == 0 {
		return nil, err
	}
	return &grants[0], nil
}

// OfDelegation reads the grants of delegation id in issue order.
func OfDelegation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]Grant, error) {
	rows, err := exec.Query(ctx, selectGrant+` AND delegation_id = $3 ORDER BY issued_at, grant_id`,
		string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanGrant)
}

func scanGrant(row pgx.CollectableRow) (Grant, error) {
	var g Grant
	var usedAt, revokedAt *time.Time
	err := row.Scan(&g.GrantID, &g.DelegationID, &g.AgentID, &g.TaskID, &g.Command, &g.RequestHash, &g.ObjectType, &g.ObjectID,
		&g.ObjectVersion, &g.ExpiresAt, &g.Status, &g.RowVersion, &g.IssuerID, &g.OwnerTeamID, &g.IssuedEpoch, &g.IssuedAt,
		&g.UsedKey, &g.UsedByID, &g.UsedEpoch, &usedAt, &g.RevokedByID, &revokedAt, &g.RevokeReason)
	g.ExpiresAt, g.IssuedAt = g.ExpiresAt.UTC(), g.IssuedAt.UTC()
	if usedAt != nil {
		g.UsedAt = usedAt.UTC()
	}
	if revokedAt != nil {
		g.RevokedAt = revokedAt.UTC()
	}
	return g, err
}

func insert(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, g Grant) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_ai.approval_grant (tenant_id, company_id, grant_id, delegation_id, agent_id,
		task_id, command, request_hash, object_type, object_id, object_version, expires_at, status, row_version, issuer_id,
		owner_team_id, issued_epoch, issued_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, nullif($16, ''), $17, $18)`,
		string(scope.Tenant), string(scope.Company), g.GrantID, g.DelegationID, g.AgentID, g.TaskID, g.Command, g.RequestHash,
		g.ObjectType, g.ObjectID, g.ObjectVersion, kernel.Instant(g.ExpiresAt), g.Status, g.RowVersion, g.IssuerID, g.OwnerTeamID,
		g.IssuedEpoch, kernel.Instant(g.IssuedAt))
	return err
}

func markUsed(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, g Grant) error {
	_, err := exec.Exec(ctx, `UPDATE brant_ai.approval_grant SET status = $4, row_version = $5, used_key = $6, used_by_id = $7,
		used_epoch = $8, used_at = $9 WHERE tenant_id = $1 AND company_id = $2 AND grant_id = $3`,
		string(scope.Tenant), string(scope.Company), g.GrantID, g.Status, g.RowVersion, g.UsedKey, g.UsedByID, g.UsedEpoch,
		kernel.Instant(g.UsedAt))
	return err
}

func revoke(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, g Grant, epoch int64) error {
	_, err := exec.Exec(ctx, `UPDATE brant_ai.approval_grant SET status = $4, row_version = $5, revoked_by_id = $6,
		revoked_epoch = $7, revoked_at = $8, revoke_reason = $9 WHERE tenant_id = $1 AND company_id = $2 AND grant_id = $3`,
		string(scope.Tenant), string(scope.Company), g.GrantID, g.Status, g.RowVersion, g.RevokedByID, epoch,
		kernel.Instant(g.RevokedAt), g.RevokeReason)
	return err
}
