package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/scenario"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// LoadScenario returns the cost scenario id, or nil, locking the root as lock says.
func LoadScenario(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*scenario.Scenario, error) {
	s := &scenario.Scenario{ID: id}
	var status, kind string
	var data []byte
	err := exec.QueryRow(ctx, `SELECT root_version, status, data, content_hash, contributors, saved_by_kind, saved_by_id, saved_epoch,
		saved_at, last_evaluation, owner_actor_id, coalesce(owner_team_id, '')
		FROM brant_cst.cost_scenario WHERE tenant_id = $1 AND company_id = $2 AND scenario_id = $3`+string(lock),
		string(scope.Tenant), string(scope.Company), id).
		Scan(&s.RootVersion, &status, &data, &s.Hash, &s.Contributors, &kind, &s.SavedBy.ID, &s.SavedEpoch, &s.SavedAt,
			&s.LastEvaluation, &s.OwnerActor, &s.OwnerTeam)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.Status, s.SavedBy.Kind, s.SavedAt = scenario.Status(status), kernel.ActorKind(kind), kernel.Instant(s.SavedAt)
	return s, json.Unmarshal(data, &s.Data)
}

// SaveScenario writes a saved draft: the root inserted when created, otherwise updated if it is
// still at root version from.
func SaveScenario(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *scenario.Scenario, created bool, from int64, at time.Time) error {
	data, err := json.Marshal(s.Data)
	if err != nil {
		return err
	}
	t, c := string(scope.Tenant), string(scope.Company)
	if created {
		_, err = exec.Exec(ctx, `INSERT INTO brant_cst.cost_scenario (tenant_id, company_id, scenario_id, root_version, status, data,
			content_hash, contributors, saved_by_kind, saved_by_id, saved_epoch, saved_at, last_evaluation, owner_actor_id, owner_team_id,
			created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, nullif($15, ''), $16, $16)`,
			t, c, s.ID, s.RootVersion, string(s.Status), data, s.Hash, s.Contributors, string(s.SavedBy.Kind), s.SavedBy.ID, s.SavedEpoch,
			s.SavedAt, s.LastEvaluation, s.OwnerActor, s.OwnerTeam, at)
		return err
	}
	return casRoot(ctx, exec, `UPDATE brant_cst.cost_scenario SET root_version = $4, data = $5, content_hash = $6, contributors = $7,
		saved_by_kind = $8, saved_by_id = $9, saved_epoch = $10, saved_at = $11, updated_at = $12
		WHERE tenant_id = $1 AND company_id = $2 AND scenario_id = $3 AND root_version = $13 AND status = 'draft'`,
		t, c, s.ID, s.RootVersion, data, s.Hash, s.Contributors, string(s.SavedBy.Kind), s.SavedBy.ID, s.SavedEpoch, s.SavedAt, at, from)
}

// AppendEvaluation counts evaluation e on its scenario's root, which must still be the draft e
// evaluated, and inserts e. The trigger on brant_cst.cost_evaluation checks the same.
func AppendEvaluation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, e *scenario.Evaluation) error {
	t, c := string(scope.Tenant), string(scope.Company)
	err := casRoot(ctx, exec, `UPDATE brant_cst.cost_scenario SET last_evaluation = $4, updated_at = $5
		WHERE tenant_id = $1 AND company_id = $2 AND scenario_id = $3 AND root_version = $6 AND status = 'draft'
		AND last_evaluation = $4 - 1`, t, c, e.ScenarioID, e.Number, e.EvaluatedAt, e.ScenarioVersion)
	if err != nil {
		return err
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_cst.cost_evaluation (tenant_id, company_id, evaluation_id, scenario_id, evaluation_no,
		scenario_version, evaluated_by_kind, evaluated_by_id, authority_epoch, evaluated_at, engine_version, normalization_version,
		normalized_input, input_hash, result, result_hash, verification_status, comparison_status, currency, total_minor)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20::numeric)`,
		t, c, e.ID, e.ScenarioID, e.Number, e.ScenarioVersion, string(e.EvaluatedBy.Kind), e.EvaluatedBy.ID, e.Epoch, e.EvaluatedAt,
		e.EngineVersion, e.NormalizationVersion, string(e.Input), e.InputHash, string(e.Document), e.ResultHash,
		e.Result.VerificationStatus, e.Result.ComparisonStatus, e.Result.Currency, e.Result.TotalMinor)
	return err
}

const evaluationColumns = `evaluation_id, scenario_id, evaluation_no, scenario_version, evaluated_by_kind, evaluated_by_id,
	authority_epoch, evaluated_at, engine_version, normalization_version, normalized_input, input_hash, result, result_hash`

// LoadEvaluation returns evaluation id, or nil.
func LoadEvaluation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*scenario.Evaluation, error) {
	rows, err := exec.Query(ctx, `SELECT `+evaluationColumns+` FROM brant_cst.cost_evaluation
		WHERE tenant_id = $1 AND company_id = $2 AND evaluation_id = $3`, string(scope.Tenant), string(scope.Company), id)
	return oneEvaluation(rows, err)
}

// LatestEvaluation returns the evaluation of scenarioID with the highest number, or nil.
func LatestEvaluation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, scenarioID string) (*scenario.Evaluation, error) {
	rows, err := exec.Query(ctx, `SELECT `+evaluationColumns+` FROM brant_cst.cost_evaluation
		WHERE tenant_id = $1 AND company_id = $2 AND scenario_id = $3 ORDER BY evaluation_no DESC LIMIT 1`,
		string(scope.Tenant), string(scope.Company), scenarioID)
	return oneEvaluation(rows, err)
}

// Evaluations returns the evaluations of scenarioID, oldest first.
func Evaluations(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, scenarioID string) ([]*scenario.Evaluation, error) {
	rows, err := exec.Query(ctx, `SELECT `+evaluationColumns+` FROM brant_cst.cost_evaluation
		WHERE tenant_id = $1 AND company_id = $2 AND scenario_id = $3 ORDER BY evaluation_no`,
		string(scope.Tenant), string(scope.Company), scenarioID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanEvaluation)
}

func oneEvaluation(rows pgx.Rows, err error) (*scenario.Evaluation, error) {
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, scanEvaluation)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return all[0], nil
}

func scanEvaluation(row pgx.CollectableRow) (*scenario.Evaluation, error) {
	var e scenario.Evaluation
	var kind, input, document string
	err := row.Scan(&e.ID, &e.ScenarioID, &e.Number, &e.ScenarioVersion, &kind, &e.EvaluatedBy.ID, &e.Epoch, &e.EvaluatedAt,
		&e.EngineVersion, &e.NormalizationVersion, &input, &e.InputHash, &document, &e.ResultHash)
	if err != nil {
		return nil, err
	}
	e.EvaluatedBy.Kind, e.EvaluatedAt = kernel.ActorKind(kind), kernel.Instant(e.EvaluatedAt)
	e.Input, e.Document = []byte(input), []byte(document)
	var ni scenario.NormalizedInput
	if err = json.Unmarshal(e.Input, &ni); err != nil {
		return nil, err
	}
	e.Inputs = ni.Inputs
	return &e, json.Unmarshal(e.Document, &e.Result)
}
