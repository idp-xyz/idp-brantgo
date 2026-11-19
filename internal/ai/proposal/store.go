package proposal

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/kernel"
)

const selectProposal = `SELECT proposal_id, task_id, delegation_id, agent_id, initiator_id, coalesce(owner_team_id, ''), command, request_hash,
	object_type, object_id, object_version, arguments::text, status, row_version, attempts, sends, proposed_at, claimed_at,
	coalesce(grant_id, ''), settled_at, coalesce(outcome_code, ''), coalesce(outcome_path, ''), coalesce(action_id, '')
	FROM brant_ai.proposal WHERE tenant_id = $1 AND company_id = $2`

// Load reads proposal id of scope through exec; nil when there is none.
func Load(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock policy.Lock) (*Proposal, error) {
	rows, err := exec.Query(ctx, selectProposal+` AND proposal_id = $3`+string(lock), string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, scan)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	return &found[0], nil
}

// OfTask reads the proposals of a task in the order they were first proposed.
func OfTask(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, taskID string) ([]Proposal, error) {
	rows, err := exec.Query(ctx, selectProposal+` AND task_id = $3 ORDER BY proposed_at, proposal_id`,
		string(scope.Tenant), string(scope.Company), taskID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scan)
}

func scan(row pgx.CollectableRow) (Proposal, error) {
	var p Proposal
	var args string
	var claimed, settled *time.Time
	err := row.Scan(&p.ProposalID, &p.TaskID, &p.DelegationID, &p.AgentID, &p.InitiatorID, &p.OwnerTeamID, &p.Command, &p.RequestHash,
		&p.ObjectType, &p.ObjectID, &p.ObjectVersion, &args, &p.Status, &p.RowVersion, &p.Attempts, &p.Sends, &p.ProposedAt, &claimed,
		&p.GrantID, &settled, &p.OutcomeCode, &p.OutcomePath, &p.ActionID)
	p.Arguments = json.RawMessage(args)
	p.ProposedAt = p.ProposedAt.UTC()
	if claimed != nil {
		p.ClaimedAt = claimed.UTC()
	}
	if settled != nil {
		p.SettledAt = settled.UTC()
	}
	return p, err
}

func insert(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, p Proposal) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_ai.proposal (tenant_id, company_id, proposal_id, task_id, delegation_id, agent_id, initiator_id,
		owner_team_id, command, request_hash, object_type, object_id, object_version, arguments, status, row_version, attempts, sends,
		proposed_at) VALUES ($1, $2, $3, $4, $5, $6, $7, nullif($8, ''), $9, $10, $11, $12, $13, $14::jsonb, $15, $16, $17, $18, $19)`,
		string(scope.Tenant), string(scope.Company), p.ProposalID, p.TaskID, p.DelegationID, p.AgentID, p.InitiatorID, p.OwnerTeamID,
		p.Command, p.RequestHash, p.ObjectType, p.ObjectID, p.ObjectVersion, string(p.Arguments), p.Status, p.RowVersion, p.Attempts,
		p.Sends, kernel.Instant(p.ProposedAt))
	return err
}

// update writes the mutable columns of p, which was loaded at version p.RowVersion-1.
func update(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, p Proposal) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_ai.proposal SET status = $4, row_version = $5, attempts = $6, sends = $7, claimed_at = $8,
		grant_id = nullif($9, ''), settled_at = $10, outcome_code = nullif($11, ''), outcome_path = nullif($12, ''),
		action_id = nullif($13, ''), delegation_id = $14, initiator_id = $15, owner_team_id = nullif($16, '')
		WHERE tenant_id = $1 AND company_id = $2 AND proposal_id = $3 AND row_version = $17`,
		string(scope.Tenant), string(scope.Company), p.ProposalID, p.Status, p.RowVersion, p.Attempts, p.Sends, instantOrNil(p.ClaimedAt),
		p.GrantID, instantOrNil(p.SettledAt), p.OutcomeCode, p.OutcomePath, p.ActionID, p.DelegationID, p.InitiatorID, p.OwnerTeamID,
		p.RowVersion-1)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return kernel.Reject(kernel.CodeVersionConflict, "/proposal", "")
	}
	return nil
}

func instantOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	i := kernel.Instant(t)
	return &i
}
