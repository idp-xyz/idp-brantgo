package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TasksConcerned locks FOR UPDATE, in ID order, the running and waiting tasks in scope a fact of
// TRD may concern (task.Fact, TSK's consumer of TRD's events): those whose target quote is quoteID,
// and those whose wait awaits one of answers by kind and identifier. It returns their IDs; the
// caller loads each and decides (task.WakeOn). Lock order: task rows first, before any command takes
// the authority guard.
func TasksConcerned(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, quoteID string, answers []task.Ref) ([]string, error) {
	t, c := scope2(scope)
	keys := make([]string, 0, len(answers))
	for _, r := range answers {
		keys = append(keys, r.Kind+"\x1f"+r.ID)
	}
	rows, err := exec.Query(ctx, `SELECT task_id FROM brant_tsk.business_task
		WHERE tenant_id = $1 AND company_id = $2
			AND lifecycle IN ('running', 'waiting_input', 'waiting_approval', 'waiting_external')
			AND (target_quote_id = $3 OR awaiting_kind || chr(31) || awaiting_id = ANY($4))
		ORDER BY task_id FOR UPDATE`, t, c, quoteID, keys)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
