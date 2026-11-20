package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	actions "go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/action"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ForShare locks the rows a read returns against changes until the transaction ends: an action
// command holds its task this way, so the task's control commands (FOR UPDATE) and the attempts of
// its steps are serialized (TSK-INV-003).
const ForShare Lock = " FOR SHARE"

// ErrActionConflict means a compare-and-set update found another action version.
var ErrActionConflict = errors.New("tsk: action version changed")

const actionColumns = `action_id, business_intent_id, task_id, target_context, target_command, request_hash, state, action_version,
	attempts, result_action_id, result_object_id, result_row_version, result_status, registered_by_kind, registered_by_id, created_at,
	updated_at`

func result(actionID, objectID, status *string, rowVersion *int64) *actions.Result {
	if actionID == nil {
		return nil
	}
	return &actions.Result{ActionID: *actionID, ObjectID: str(objectID), RowVersion: *rowVersion, Status: str(status)}
}

func resultColumns(r *actions.Result) (actionID, objectID, status *string, rowVersion *int64) {
	if r == nil {
		return nil, nil, nil, nil
	}
	return &r.ActionID, &r.ObjectID, &r.Status, &r.RowVersion
}

func scanAction(row pgx.Row) (*actions.Execution, error) {
	var e actions.Execution
	var state, kind string
	var rAction, rObject, rStatus *string
	var rVersion *int64
	err := row.Scan(&e.ID, &e.Intent, &e.TaskID, &e.Target.Context, &e.Target.Command, &e.Target.RequestHash, &state, &e.Version, &e.Attempts,
		&rAction, &rObject, &rVersion, &rStatus, &kind, &e.RegisteredBy.ID, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	e.State, e.RegisteredBy.Kind = actions.State(state), kernel.ActorKind(kind)
	e.Result = result(rAction, rObject, rStatus, rVersion)
	e.CreatedAt, e.UpdatedAt = e.CreatedAt.UTC(), e.UpdatedAt.UTC()
	return &e, nil
}

func loadAction(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, column, value string, lock Lock) (*actions.Execution, error) {
	tenant, company := scope2(scope)
	e, err := scanAction(exec.QueryRow(ctx, `SELECT `+actionColumns+` FROM brant_tsk.action_execution
		WHERE tenant_id = $1 AND company_id = $2 AND `+column+` = $3`+string(lock), tenant, company, value))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return e, err
}

// LoadAction returns action id of scope, or nil.
func LoadAction(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*actions.Execution, error) {
	return loadAction(ctx, exec, scope, "action_id", id, lock)
}

// LoadActionOfIntent returns the action of business intent intent in scope, or nil.
func LoadActionOfIntent(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, intent string, lock Lock) (*actions.Execution, error) {
	return loadAction(ctx, exec, scope, "business_intent_id", intent, lock)
}

// TaskActions returns the actions of task id, oldest first.
func TaskActions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*actions.Execution, error) {
	tenant, company := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT `+actionColumns+` FROM brant_tsk.action_execution
		WHERE tenant_id = $1 AND company_id = $2 AND task_id = $3 ORDER BY created_at, action_id`, tenant, company, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (*actions.Execution, error) { return scanAction(r) })
}

// OpenActions counts the actions of task id whose attempt is submitted or unknown (TSK-INV-003).
func OpenActions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (int, error) {
	tenant, company := scope2(scope)
	var n int
	err := exec.QueryRow(ctx, `SELECT count(*) FROM brant_tsk.action_execution WHERE tenant_id = $1 AND company_id = $2 AND task_id = $3
		AND state IN ('submitted', 'unknown')`, tenant, company, id).Scan(&n)
	return n, err
}

// InsertAction registers action e and appends its first log row s.
func InsertAction(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, e *actions.Execution, s actions.Step) error {
	tenant, company := scope2(scope)
	rAction, rObject, rStatus, rVersion := resultColumns(e.Result)
	_, err := exec.Exec(ctx, `INSERT INTO brant_tsk.action_execution (tenant_id, company_id, action_id, business_intent_id, task_id,
		target_context, target_command, idempotency_key, request_hash, state, action_version, attempts, result_action_id, result_object_id,
		result_row_version, result_status, registered_by_kind, registered_by_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
		tenant, company, e.ID, e.Intent, e.TaskID, e.Target.Context, e.Target.Command, e.Key(), e.Target.RequestHash, string(e.State), e.Version,
		e.Attempts, rAction, rObject, rVersion, rStatus, string(e.RegisteredBy.Kind), e.RegisteredBy.ID, kernel.Instant(e.CreatedAt),
		kernel.Instant(e.UpdatedAt))
	if err != nil {
		return err
	}
	return insertStep(ctx, exec, scope, e.ID, s)
}

// SaveAction writes e's changes if the stored action is still at version from (ErrActionConflict
// otherwise) and appends s to its log.
func SaveAction(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, e *actions.Execution, from int64, s actions.Step) error {
	tenant, company := scope2(scope)
	rAction, rObject, rStatus, rVersion := resultColumns(e.Result)
	tag, err := exec.Exec(ctx, `UPDATE brant_tsk.action_execution SET state = $4, action_version = $5, attempts = $6, result_action_id = $7,
		result_object_id = $8, result_row_version = $9, result_status = $10, updated_at = $11
		WHERE tenant_id = $1 AND company_id = $2 AND action_id = $3 AND action_version = $12`,
		tenant, company, e.ID, string(e.State), e.Version, e.Attempts, rAction, rObject, rVersion, rStatus, kernel.Instant(e.UpdatedAt), from)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrActionConflict
	}
	return insertStep(ctx, exec, scope, e.ID, s)
}

func insertStep(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, s actions.Step) error {
	tenant, company := scope2(scope)
	rAction, rObject, rStatus, rVersion := resultColumns(s.Result)
	_, err := exec.Exec(ctx, `INSERT INTO brant_tsk.action_attempt (tenant_id, company_id, action_id, action_version, attempt, command, state,
		answer, code, control_epoch, delegation_id, grant_id, result_action_id, result_object_id, result_row_version, result_status,
		actor_kind, actor_id, authority_epoch, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
		tenant, company, id, s.Version, s.Attempt, s.Command, string(s.State), string(s.Answer), nullable(s.Code), s.ControlEpoch,
		nullable(s.DelegationID), nullable(s.GrantID), rAction, rObject, rVersion, rStatus, string(s.By.Kind), s.By.ID, s.Epoch,
		kernel.Instant(s.At))
	return err
}

// LoadSteps returns action id's log, oldest first.
func LoadSteps(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]actions.Step, error) {
	tenant, company := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT action_version, attempt, command, state, answer, coalesce(code, ''), control_epoch,
		coalesce(delegation_id, ''), coalesce(grant_id, ''), result_action_id, result_object_id, result_row_version, result_status, actor_kind,
		actor_id, authority_epoch, recorded_at
		FROM brant_tsk.action_attempt WHERE tenant_id = $1 AND company_id = $2 AND action_id = $3 ORDER BY action_version`, tenant, company, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (actions.Step, error) {
		var s actions.Step
		var state, answer, kind string
		var rAction, rObject, rStatus *string
		var rVersion *int64
		var at time.Time
		err := r.Scan(&s.Version, &s.Attempt, &s.Command, &state, &answer, &s.Code, &s.ControlEpoch, &s.DelegationID, &s.GrantID, &rAction,
			&rObject, &rVersion, &rStatus, &kind, &s.By.ID, &s.Epoch, &at)
		s.State, s.Answer, s.By.Kind, s.At = actions.State(state), kernel.ReconcileState(answer), kernel.ActorKind(kind), at.UTC()
		s.Result = result(rAction, rObject, rStatus, rVersion)
		return s, err
	})
}
