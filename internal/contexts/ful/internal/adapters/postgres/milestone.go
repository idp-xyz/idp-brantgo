package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/milestone"
	"go.idp.xyz/idp-brant/internal/kernel"
)

const planColumns = `plan_id, status, row_version, plan_revision, subject_kind, subject_id, request_hash, owner_actor_id, coalesce(owner_team_id, ''),
	created_by_kind, created_by_id, create_epoch, created_at, revised_by_kind, revised_by_id, revise_epoch, revised_at, coalesce(revision_reason, '')`

// LoadPlan returns milestone plan id with the nodes of its current revision and every actual, or
// nil; lock locks the plan row.
func LoadPlan(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*milestone.Plan, error) {
	t, c := scope2(scope)
	var p milestone.Plan
	var cKind, rKind string
	err := exec.QueryRow(ctx, `SELECT `+planColumns+` FROM brant_ful.milestone_plan WHERE tenant_id = $1 AND company_id = $2 AND plan_id = $3`+
		string(lock), t, c, id).Scan(&p.ID, &p.Status, &p.RowVersion, &p.Revision, &p.Subject.Kind, &p.Subject.ID, &p.RequestHash, &p.OwnerActor,
		&p.OwnerTeam, &cKind, &p.Created.By.ID, &p.Created.Epoch, &p.Created.At, &rKind, &p.Revised.By.ID, &p.Revised.Epoch, &p.Revised.At, &p.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Created.By.Kind, p.Revised.By.Kind = kernel.ActorKind(cKind), kernel.ActorKind(rKind)
	p.Created.At, p.Revised.At = kernel.Instant(p.Created.At), kernel.Instant(p.Revised.At)
	rows, err := exec.Query(ctx, `SELECT node_id, node_type, planned_on, coalesce(responsible_kind, ''), coalesce(responsible_id, ''), predecessors
		FROM brant_ful.milestone_node WHERE tenant_id = $1 AND company_id = $2 AND plan_id = $3 AND plan_revision = $4 ORDER BY node_no`,
		t, c, id, p.Revision)
	if err != nil {
		return nil, err
	}
	p.Nodes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (milestone.Node, error) {
		var n milestone.Node
		var kind, who string
		if err := row.Scan(&n.ID, &n.Type, &n.PlannedOn, &kind, &who, &n.Predecessors); err != nil {
			return n, err
		}
		n.PlannedOn = time.Date(n.PlannedOn.Year(), n.PlannedOn.Month(), n.PlannedOn.Day(), 0, 0, 0, 0, time.UTC)
		if kind != "" {
			n.Responsible = &milestone.Responsible{Kind: kind, ID: who}
		}
		n.Predecessors = strs(n.Predecessors)
		return n, nil
	})
	if err != nil {
		return nil, err
	}
	rows, err = exec.Query(ctx, `SELECT seq, node_id, node_type, evidence_level, occurred_at, evidence_ids, coalesce(note, ''), plan_revision,
		recorded_by_kind, recorded_by_id, record_epoch, recorded_at FROM brant_ful.milestone_actual
		WHERE tenant_id = $1 AND company_id = $2 AND plan_id = $3 ORDER BY seq`, t, c, id)
	if err != nil {
		return nil, err
	}
	p.Actuals, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (milestone.Actual, error) {
		var a milestone.Actual
		var kind string
		if err := row.Scan(&a.Seq, &a.NodeID, &a.NodeType, &a.Level, &a.OccurredAt, &a.Evidence, &a.Note, &a.PlanRevision, &kind, &a.Recorded.By.ID,
			&a.Recorded.Epoch, &a.Recorded.At); err != nil {
			return a, err
		}
		a.Recorded.By.Kind, a.Recorded.At, a.OccurredAt, a.Evidence = kernel.ActorKind(kind), kernel.Instant(a.Recorded.At), kernel.Instant(a.OccurredAt),
			strs(a.Evidence)
		return a, nil
	})
	return &p, err
}

// PlanOfSubject returns the ID of the plan that follows subject, or "".
func PlanOfSubject(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, subject milestone.Subject) (string, error) {
	t, c := scope2(scope)
	var id string
	err := exec.QueryRow(ctx, `SELECT plan_id FROM brant_ful.milestone_plan WHERE tenant_id = $1 AND company_id = $2 AND subject_kind = $3
		AND subject_id = $4`, t, c, subject.Kind, subject.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func insertNodes(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, p *milestone.Plan) error {
	t, c := scope2(scope)
	for i, n := range p.Nodes {
		var kind, who *string
		if n.Responsible != nil {
			kind, who = &n.Responsible.Kind, &n.Responsible.ID
		}
		if _, err := exec.Exec(ctx, `INSERT INTO brant_ful.milestone_node (tenant_id, company_id, plan_id, plan_revision, node_id, node_no, node_type,
			planned_on, responsible_kind, responsible_id, predecessors) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::date, $9, $10, $11)`,
			t, c, p.ID, p.Revision, n.ID, i+1, n.Type, n.PlannedOn.Format(time.DateOnly), kind, who, strs(n.Predecessors)); err != nil {
			return err
		}
	}
	return nil
}

// InsertPlan stores a new plan with the nodes of its first revision.
func InsertPlan(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, p *milestone.Plan) error {
	t, c := scope2(scope)
	if _, err := exec.Exec(ctx, `INSERT INTO brant_ful.milestone_plan (tenant_id, company_id, plan_id, status, row_version, plan_revision, node_count,
		actual_count, subject_kind, subject_id, request_hash, owner_actor_id, owner_team_id, created_by_kind, created_by_id, create_epoch, created_at,
		revised_by_kind, revised_by_id, revise_epoch, revised_at, revision_reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 0, $8, $9, $10, $11, $12, $13, $14, $15, $16, $13, $14, $15, $16, $17)`,
		t, c, p.ID, p.Status, p.RowVersion, p.Revision, len(p.Nodes), p.Subject.Kind, p.Subject.ID, p.RequestHash, p.OwnerActor, nullable(p.OwnerTeam),
		string(p.Created.By.Kind), p.Created.By.ID, p.Created.Epoch, p.Created.At, nullable(p.Reason)); err != nil {
		return err
	}
	return insertNodes(ctx, exec, scope, p)
}

// RevisePlan writes p's new revision if the stored plan is still at version from.
func RevisePlan(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, p *milestone.Plan, from int64) error {
	t, c := scope2(scope)
	tag, err := exec.Exec(ctx, `UPDATE brant_ful.milestone_plan SET row_version = $4, plan_revision = $5, node_count = $6, revised_by_kind = $7,
		revised_by_id = $8, revise_epoch = $9, revised_at = $10, revision_reason = $11 WHERE tenant_id = $1 AND company_id = $2 AND plan_id = $3
		AND row_version = $12`, t, c, p.ID, p.RowVersion, p.Revision, len(p.Nodes), string(p.Revised.By.Kind), p.Revised.By.ID, p.Revised.Epoch,
		p.Revised.At, nullable(p.Reason), from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	if err != nil {
		return err
	}
	return insertNodes(ctx, exec, scope, p)
}

// InsertActual writes actual a of plan p, which moved from row version from.
func InsertActual(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, p *milestone.Plan, a *milestone.Actual, from int64) error {
	t, c := scope2(scope)
	tag, err := exec.Exec(ctx, `UPDATE brant_ful.milestone_plan SET row_version = $4, actual_count = $5 WHERE tenant_id = $1 AND company_id = $2
		AND plan_id = $3 AND row_version = $6`, t, c, p.ID, p.RowVersion, len(p.Actuals), from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	if err != nil {
		return err
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_ful.milestone_actual (tenant_id, company_id, plan_id, seq, plan_revision, node_id, node_type,
		evidence_level, occurred_at, evidence_ids, note, plan_version, recorded_by_kind, recorded_by_id, record_epoch, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		t, c, p.ID, a.Seq, a.PlanRevision, a.NodeID, a.NodeType, a.Level, a.OccurredAt, strs(a.Evidence), nullable(a.Note), p.RowVersion,
		string(a.Recorded.By.Kind), a.Recorded.By.ID, a.Recorded.Epoch, a.Recorded.At)
	if err != nil {
		return fmt.Errorf("ful: actual %d of plan %s: %w", a.Seq, p.ID, err)
	}
	return nil
}
