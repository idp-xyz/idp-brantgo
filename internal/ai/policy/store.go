package policy

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is the AI layer's schema (plan S4 D-6).
const Schema = "brant_ai"

// Lock is how a read locks the delegation row.
type Lock string

const (
	NoLock    Lock = ""
	ForShare  Lock = " FOR SHARE"
	ForUpdate Lock = " FOR UPDATE"
)

const selectDelegation = `SELECT delegation_id, initiator_id, agent_id, task_id, autonomy, capabilities, expires_at, status,
	row_version, coalesce(owner_team_id, ''), granted_epoch, granted_at, coalesce(revoked_by_id, ''), revoked_at,
	coalesce(revoke_reason, '')
	FROM brant_ai.delegation WHERE tenant_id = $1 AND company_id = $2 AND delegation_id = $3`

// Load reads delegation id of scope through exec; nil when there is none.
func Load(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*Delegation, error) {
	var d Delegation
	var autonomy string
	var revokedAt *time.Time
	err := exec.QueryRow(ctx, selectDelegation+string(lock), string(scope.Tenant), string(scope.Company), id).Scan(
		&d.DelegationID, &d.InitiatorID, &d.AgentID, &d.TaskID, &autonomy, &d.Capabilities, &d.ExpiresAt, &d.Status,
		&d.RowVersion, &d.OwnerTeamID, &d.GrantedEpoch, &d.GrantedAt, &d.RevokedByID, &revokedAt, &d.RevokeReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.Autonomy = capability.SideEffect(autonomy)
	d.ExpiresAt, d.GrantedAt = d.ExpiresAt.UTC(), d.GrantedAt.UTC()
	if revokedAt != nil {
		d.RevokedAt = revokedAt.UTC()
	}
	return &d, nil
}

func insert(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, d Delegation) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_ai.delegation (tenant_id, company_id, delegation_id, initiator_id, agent_id,
		task_id, autonomy, capabilities, expires_at, status, row_version, owner_team_id, granted_epoch, granted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, nullif($12, ''), $13, $14)`,
		string(scope.Tenant), string(scope.Company), d.DelegationID, d.InitiatorID, d.AgentID, d.TaskID, string(d.Autonomy),
		d.Capabilities, kernel.Instant(d.ExpiresAt), d.Status, d.RowVersion, d.OwnerTeamID, d.GrantedEpoch, kernel.Instant(d.GrantedAt))
	return err
}

func revoke(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, d Delegation, epoch int64) error {
	_, err := exec.Exec(ctx, `UPDATE brant_ai.delegation SET status = $4, row_version = $5, revoked_by_id = $6,
		revoked_epoch = $7, revoked_at = $8, revoke_reason = $9
		WHERE tenant_id = $1 AND company_id = $2 AND delegation_id = $3`,
		string(scope.Tenant), string(scope.Company), d.DelegationID, d.Status, d.RowVersion, d.RevokedByID, epoch,
		kernel.Instant(d.RevokedAt), d.RevokeReason)
	return err
}

// OfAgent reads the delegations of agent in scope, active or ended (revoked or expired) after since,
// ordered by task and delegation: the AI runner's work list (unit C2) — the active ones to act
// under, the ended ones to hand their running tasks back to people (AN04).
func OfAgent(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, agentID string, since time.Time) ([]Delegation, error) {
	rows, err := exec.Query(ctx, `SELECT delegation_id FROM brant_ai.delegation WHERE tenant_id = $1 AND company_id = $2 AND agent_id = $3
		AND (status = 'active' OR revoked_at > $4 OR expires_at > $4) ORDER BY task_id, delegation_id`,
		string(scope.Tenant), string(scope.Company), agentID, kernel.Instant(since))
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]Delegation, 0, len(ids))
	for _, id := range ids {
		d, err := Load(ctx, exec, scope, id, NoLock)
		if err != nil {
			return nil, err
		}
		if d != nil {
			out = append(out, *d)
		}
	}
	return out, nil
}
