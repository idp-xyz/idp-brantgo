package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/snapshot"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// FreezeScenario moves scenario id from draft at root version from to frozen at from + 1 (the
// trigger on brant_cst.cost_scenario allows exactly that).
func FreezeScenario(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, from int64, at time.Time) error {
	return casRoot(ctx, exec, `UPDATE brant_cst.cost_scenario SET status = 'frozen', root_version = $4 + 1, updated_at = $5
		WHERE tenant_id = $1 AND company_id = $2 AND scenario_id = $3 AND root_version = $4 AND status = 'draft'`,
		string(scope.Tenant), string(scope.Company), id, from, at)
}

// InsertSnapshot inserts snapshot s with its components and allocations, after its scenario was
// frozen in the same transaction.
func InsertSnapshot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *snapshot.Snapshot) error {
	t, c := string(scope.Tenant), string(scope.Company)
	_, err := exec.Exec(ctx, `INSERT INTO brant_cst.cost_snapshot (tenant_id, company_id, snapshot_id, scenario_id, evaluation_id,
		input_scenario_version, frozen_scenario_version, context_id, context_revision, engine_version, normalization_version, input_hash,
		result_hash, selection_reason, currency, total_minor, supersedes_snapshot_id, document, evidence_hash, component_count,
		allocation_count, frozen_by_kind, frozen_by_id, authority_epoch, frozen_at, owner_actor_id, owner_team_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16::numeric, nullif($17, ''), $18, $19, $20, $21, $22,
		$23, $24, $25, $26, nullif($27, ''))`,
		t, c, s.ID, s.ScenarioID, s.EvaluationID, s.InputScenarioVersion, s.FrozenScenarioVersion, s.Context.ContextID, s.Context.Revision,
		s.EngineVersion, s.NormalizationVersion, s.InputHash, s.ResultHash, s.SelectionReason, s.Currency, s.TotalMinor, s.Supersedes,
		string(s.Document), s.EvidenceHash, len(s.Components), len(s.Allocations), string(s.FrozenBy.Kind), s.FrozenBy.ID, s.Epoch,
		s.FrozenAt, s.OwnerActor, s.OwnerTeam)
	if err != nil {
		return err
	}
	for _, p := range s.Components {
		if _, err = exec.Exec(ctx, `INSERT INTO brant_cst.cost_snapshot_component (tenant_id, company_id, snapshot_id, ordinal, component_id,
			kind, obligation_id, currency, amount_minor) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric)`,
			t, c, s.ID, p.Ordinal, p.ComponentID, p.Kind, p.ObligationID, p.Currency, p.AmountMinor); err != nil {
			return err
		}
	}
	for _, a := range s.Allocations {
		if _, err = exec.Exec(ctx, `INSERT INTO brant_cst.cost_snapshot_allocation (tenant_id, company_id, snapshot_id, ordinal, component_id,
			line_id, basis, base_integer, amount_minor) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric)`,
			t, c, s.ID, a.Ordinal, a.ComponentID, a.LineID, a.Basis, a.BaseInteger, a.AmountMinor); err != nil {
			return err
		}
	}
	return nil
}

const snapshotColumns = `snapshot_id, scenario_id, evaluation_id, input_scenario_version, frozen_scenario_version, context_id,
	context_revision, engine_version, normalization_version, input_hash, result_hash, selection_reason, currency, total_minor::text,
	coalesce(supersedes_snapshot_id, ''), document, evidence_hash, frozen_by_kind, frozen_by_id, authority_epoch, frozen_at,
	owner_actor_id, coalesce(owner_team_id, '')`

// LoadSnapshot returns snapshot id, or nil, locking its row as lock says (a freeze that supersedes
// it locks it FOR UPDATE).
func LoadSnapshot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*snapshot.Snapshot, error) {
	return oneSnapshot(exec.Query(ctx, `SELECT `+snapshotColumns+` FROM brant_cst.cost_snapshot
		WHERE tenant_id = $1 AND company_id = $2 AND snapshot_id = $3`+string(lock), string(scope.Tenant), string(scope.Company), id))
}

// SnapshotOfScenario returns the snapshot of scenario id, or nil.
func SnapshotOfScenario(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*snapshot.Snapshot, error) {
	return oneSnapshot(exec.Query(ctx, `SELECT `+snapshotColumns+` FROM brant_cst.cost_snapshot
		WHERE tenant_id = $1 AND company_id = $2 AND scenario_id = $3`, string(scope.Tenant), string(scope.Company), id))
}

func oneSnapshot(rows pgx.Rows, err error) (*snapshot.Snapshot, error) {
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*snapshot.Snapshot, error) {
		var s snapshot.Snapshot
		var kind, document string
		err := row.Scan(&s.ID, &s.ScenarioID, &s.EvaluationID, &s.InputScenarioVersion, &s.FrozenScenarioVersion, &s.Context.ContextID,
			&s.Context.Revision, &s.EngineVersion, &s.NormalizationVersion, &s.InputHash, &s.ResultHash, &s.SelectionReason, &s.Currency,
			&s.TotalMinor, &s.Supersedes, &document, &s.EvidenceHash, &kind, &s.FrozenBy.ID, &s.Epoch, &s.FrozenAt, &s.OwnerActor, &s.OwnerTeam)
		s.FrozenBy.Kind, s.FrozenAt, s.Document = kernel.ActorKind(kind), kernel.Instant(s.FrozenAt), []byte(document)
		if err == nil {
			var d snapshot.Document
			if err = json.Unmarshal(s.Document, &d); err == nil {
				s.Rfq = d.Rfq
			}
		}
		return &s, err
	})
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return all[0], nil
}

// Successor returns the snapshot that supersedes snapshot id, or "".
func Successor(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (string, error) {
	var out string
	err := exec.QueryRow(ctx, `SELECT snapshot_id FROM brant_cst.cost_snapshot WHERE tenant_id = $1 AND company_id = $2
		AND supersedes_snapshot_id = $3`, string(scope.Tenant), string(scope.Company), id).Scan(&out)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return out, err
}

// SnapshotsRestingOn returns the snapshots whose frozen evaluation rests on source id, sorted.
func SnapshotsRestingOn(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]string, error) {
	probe, err := json.Marshal([]map[string]string{{"context": "evd", "kind": "source", "id": id}})
	if err != nil {
		return nil, err
	}
	rows, err := exec.Query(ctx, `SELECT s.snapshot_id FROM brant_cst.cost_snapshot s JOIN brant_cst.cost_evaluation e
		ON e.tenant_id = s.tenant_id AND e.company_id = s.company_id AND e.evaluation_id = s.evaluation_id
		WHERE s.tenant_id = $1 AND s.company_id = $2 AND (e.normalized_input::jsonb -> 'inputs') @> $3::jsonb ORDER BY s.snapshot_id`,
		string(scope.Tenant), string(scope.Company), string(probe))
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	return nonNil(out), err
}

// EnsureSources inserts an undisputed standing row for each of ids CST holds none for, in ID
// order. A row another transaction is inserting makes this one wait until it commits.
func EnsureSources(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ids []string, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := exec.Exec(ctx, `INSERT INTO brant_cst.source_standing (tenant_id, company_id, source_id, root_version, status, created_at,
		updated_at) SELECT $1, $2, id, 1, 'undisputed', $4, $4 FROM unnest($3::text[]) AS id ORDER BY id ON CONFLICT DO NOTHING`,
		string(scope.Tenant), string(scope.Company), sorted(ids), at)
	return err
}

// SourceStandings returns CST's standing of the sources among ids it holds a row for, locking the
// rows in ID order as lock says.
func SourceStandings(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ids []string, lock Lock) (map[string]snapshot.Standing, error) {
	out := map[string]snapshot.Standing{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := exec.Query(ctx, `SELECT source_id, root_version, coalesce(denial_id, ''), denied_at FROM brant_cst.source_standing
		WHERE tenant_id = $1 AND company_id = $2 AND source_id = ANY($3) ORDER BY source_id`+string(lock),
		string(scope.Tenant), string(scope.Company), sorted(ids))
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (snapshot.Standing, error) {
		var s snapshot.Standing
		var at *time.Time
		err := row.Scan(&s.SourceID, &s.RootVersion, &s.DenialID, &at)
		if at != nil {
			s.DeniedAt = kernel.Instant(*at)
		}
		return s, err
	})
	for _, s := range all {
		out[s.SourceID] = s
	}
	return out, err
}

func sorted(ids []string) []string { return slices.Compact(slices.Sorted(slices.Values(ids))) }

// LoadAttestation returns attestation id, or nil.
func LoadAttestation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*snapshot.Attestation, error) {
	var a snapshot.Attestation
	var kind string
	var evidence []byte
	err := exec.QueryRow(ctx, `SELECT attestation_id, source_id, verdict, reason, evidence, attested_by_kind, attested_by_id, authority_epoch,
		attested_at, coalesce(owner_team_id, '') FROM brant_cst.source_attestation WHERE tenant_id = $1 AND company_id = $2
		AND attestation_id = $3`, string(scope.Tenant), string(scope.Company), id).
		Scan(&a.ID, &a.SourceID, &a.Verdict, &a.Reason, &evidence, &kind, &a.By.ID, &a.Epoch, &a.At, &a.OwnerTeam)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.By.Kind, a.At = kernel.ActorKind(kind), kernel.Instant(a.At)
	return &a, json.Unmarshal(evidence, &a.Evidence)
}

// RecordAttestation inserts attestation a and, for a denial, moves the standing of its source to
// denied with the standing's next root version.
func RecordAttestation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, a *snapshot.Attestation, s snapshot.Standing) error {
	t, c := string(scope.Tenant), string(scope.Company)
	evidence, err := json.Marshal(a.Evidence)
	if err != nil {
		return err
	}
	if _, err = exec.Exec(ctx, `INSERT INTO brant_cst.source_attestation (tenant_id, company_id, attestation_id, source_id, verdict, reason,
		evidence, attested_by_kind, attested_by_id, authority_epoch, attested_at, owner_team_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, nullif($12, ''))`,
		t, c, a.ID, a.SourceID, a.Verdict, a.Reason, evidence, string(a.By.Kind), a.By.ID, a.Epoch, a.At, a.OwnerTeam); err != nil {
		return err
	}
	if a.Verdict != snapshot.Denied {
		return nil
	}
	return casRoot(ctx, exec, `UPDATE brant_cst.source_standing SET status = 'denied', root_version = $4, denial_id = $5, denied_at = $6,
		updated_at = $6 WHERE tenant_id = $1 AND company_id = $2 AND source_id = $3 AND root_version = $4 - 1 AND status = 'undisputed'`,
		t, c, s.SourceID, s.RootVersion, s.DenialID, s.DeniedAt)
}
