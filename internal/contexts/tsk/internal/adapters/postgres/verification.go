package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Goal verifications (OutcomeEvidence; migration tsk 000002): one row per goal.verify, keyed by the
// task version it produced, with the assertions as a JSON array.

type evidenceJSON struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Version int64  `json:"version"`
	Hash    string `json:"hash"`
}

type checkJSON struct {
	Check  string `json:"check"`
	Answer string `json:"answer"`
	Code   string `json:"code"`
	Path   string `json:"path"`
}

type assertionJSON struct {
	Assertion string         `json:"assertion"`
	Answer    string         `json:"answer"`
	Code      string         `json:"code"`
	Checks    []checkJSON    `json:"checks"`
	Evidence  []evidenceJSON `json:"evidence"`
}

func encodeAssertions(as []task.Assertion) ([]byte, error) {
	out := make([]assertionJSON, len(as))
	for i, a := range as {
		x := assertionJSON{Assertion: a.Assertion, Answer: a.Answer, Code: a.Code, Checks: []checkJSON{}, Evidence: []evidenceJSON{}}
		for _, c := range a.Checks {
			x.Checks = append(x.Checks, checkJSON(c))
		}
		for _, e := range a.Evidence {
			x.Evidence = append(x.Evidence, evidenceJSON(e))
		}
		out[i] = x
	}
	return json.Marshal(out)
}

func decodeAssertions(raw []byte) ([]task.Assertion, error) {
	var in []assertionJSON
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := make([]task.Assertion, len(in))
	for i, x := range in {
		a := task.Assertion{Assertion: x.Assertion, Answer: x.Answer, Code: x.Code, Checks: []task.Check{}, Evidence: []task.Evidence{}}
		for _, c := range x.Checks {
			a.Checks = append(a.Checks, task.Check(c))
		}
		for _, e := range x.Evidence {
			a.Evidence = append(a.Evidence, task.Evidence(e))
		}
		out[i] = a
	}
	return out, nil
}

func nullInt(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}

// InsertVerification records v after its task change was written (its history row exists).
func InsertVerification(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, v task.Verification) error {
	tenant, company := scope2(scope)
	assertions, err := encodeAssertions(v.Assertions)
	if err != nil {
		return err
	}
	var cState, cTask *string
	var cEpoch *int64
	if c := v.Control; c != nil {
		cState, cTask, cEpoch = &c.State, &c.TaskID, &c.Epoch
	}
	unmet := v.Unmet
	if unmet == nil {
		unmet = []string{}
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_tsk.goal_verification (tenant_id, company_id, task_id, task_version, lifecycle, goal_code,
		goal_version, assessor, outcome, code, unmet, quote_id, root_version, business_revision, business_hash, order_id, order_revision,
		order_hash, control_state, control_task_id, control_epoch, unsettled_actions, assertions, checked_at, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25)`,
		tenant, company, v.TaskID, v.TaskVersion, string(v.To), v.Goal, v.GoalVersion, v.Assessor, v.Outcome, nullable(v.Code), unmet,
		nullable(v.QuoteID), nullInt(v.RootVersion), nullInt(v.BusinessRevision), nullable(v.BusinessHash), nullable(v.OrderID),
		nullInt(v.OrderRevision), nullable(v.OrderHash), cState, cTask, cEpoch, v.Unsettled, assertions, kernel.Instant(v.CheckedAt),
		kernel.Instant(v.RecordedAt))
	return err
}

// RecordedVerification is a stored verification with the actor that ran goal.verify (its history row).
type RecordedVerification struct {
	task.Verification
	By kernel.ActorRef
}

// LoadVerifications returns the verifications of task id, oldest first.
func LoadVerifications(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]RecordedVerification, error) {
	tenant, company := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT v.task_version, v.lifecycle, v.goal_code, v.goal_version, v.assessor, v.outcome,
		coalesce(v.code, ''), v.unmet, coalesce(v.quote_id, ''), coalesce(v.root_version, 0), coalesce(v.business_revision, 0),
		coalesce(v.business_hash, ''), coalesce(v.order_id, ''), coalesce(v.order_revision, 0), coalesce(v.order_hash, ''),
		v.control_state, v.control_task_id, v.control_epoch, v.unsettled_actions, v.assertions, v.checked_at, v.recorded_at,
		h.actor_kind, h.actor_id
		FROM brant_tsk.goal_verification v JOIN brant_tsk.task_transition h ON h.tenant_id = v.tenant_id AND h.company_id = v.company_id
			AND h.task_id = v.task_id AND h.task_version = v.task_version
		WHERE v.tenant_id = $1 AND v.company_id = $2 AND v.task_id = $3 ORDER BY v.task_version`, tenant, company, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (RecordedVerification, error) {
		var v RecordedVerification
		var to, kind string
		var cState, cTask *string
		var cEpoch *int64
		var raw []byte
		var checked, recorded time.Time
		err := r.Scan(&v.TaskVersion, &to, &v.Goal, &v.GoalVersion, &v.Assessor, &v.Outcome, &v.Code, &v.Unmet, &v.QuoteID, &v.RootVersion,
			&v.BusinessRevision, &v.BusinessHash, &v.OrderID, &v.OrderRevision, &v.OrderHash, &cState, &cTask, &cEpoch, &v.Unsettled, &raw,
			&checked, &recorded, &kind, &v.By.ID)
		if err != nil {
			return v, err
		}
		v.TaskID, v.To, v.By.Kind, v.CheckedAt, v.RecordedAt = id, task.State(to), kernel.ActorKind(kind), checked.UTC(), recorded.UTC()
		if cState != nil {
			v.Control = &task.RootControl{State: *cState, TaskID: str(cTask), Epoch: *cEpoch}
		}
		v.Assertions, err = decodeAssertions(raw)
		return v, err
	})
}
