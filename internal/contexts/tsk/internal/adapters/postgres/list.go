package postgres

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ListFilter selects a page of tasks. DataScope, ActorID and Team are the caller's authorization
// decision: the query returns only the tasks task.Task.Visible accepts for it, as SQL. After is
// the last entry of the previous page, nil for the first.
type ListFilter struct {
	Lifecycle, ControlMode, RfqID, QuoteID string
	DataScope, ActorID, Team               string
	After                                  *kernel.ListCursor
	Limit                                  int
}

// ListRow is one task of a list.
type ListRow struct {
	ID, GoalCode, Lifecycle, ControlMode, Owner, Agent, TargetRfq, TargetQuote, BoundQuote, WaitKind string
	TaskVersion                                                                                      int64
	Deadline                                                                                         *time.Time
	UpdatedAt                                                                                        time.Time
}

// List returns at most f.Limit tasks of the scope that the caller sees, in (updated_at, task_id)
// descending order (index business_task_listing), without locks.
func List(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, f ListFilter) ([]ListRow, error) {
	var afterAt *time.Time
	afterID := ""
	if f.After != nil {
		afterAt, afterID = &f.After.At, f.After.ID
	}
	rows, err := exec.Query(ctx, `SELECT task_id, goal_code, lifecycle, control_mode, task_version, owner_id, coalesce(agent_id, ''),
		coalesce(target_rfq_id, ''), coalesce(target_quote_id, ''),
		CASE WHEN binding_state = 'bound' THEN coalesce(binding_quote_id, '') ELSE '' END, coalesce(wait_kind, ''), deadline, updated_at
		FROM brant_tsk.business_task
		WHERE tenant_id = $1 AND company_id = $2
		  AND ($3 = '' OR lifecycle = $3)
		  AND ($4 = '' OR control_mode = $4)
		  AND ($5 = '' OR target_rfq_id = $5)
		  AND ($6 = '' OR target_quote_id = $6 OR (binding_state = 'bound' AND binding_quote_id = $6))
		  AND ($7 = 'company' OR owner_id = $8 OR initiator_id = $8 OR agent_id = $8
		       OR ($7 = 'team' AND $9 <> '' AND owner_team_id = $9))
		  AND ($10::timestamptz IS NULL OR (updated_at, task_id) < ($10::timestamptz, $11))
		ORDER BY updated_at DESC, task_id DESC
		LIMIT $12`,
		string(scope.Tenant), string(scope.Company), f.Lifecycle, f.ControlMode, f.RfqID, f.QuoteID, f.DataScope, f.ActorID, f.Team,
		afterAt, afterID, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ListRow
	for rows.Next() {
		var r ListRow
		if err := rows.Scan(&r.ID, &r.GoalCode, &r.Lifecycle, &r.ControlMode, &r.TaskVersion, &r.Owner, &r.Agent, &r.TargetRfq, &r.TargetQuote,
			&r.BoundQuote, &r.WaitKind, &r.Deadline, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
