// Package postgres stores plugin activations in schema brant_ext.
package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is the platform schema.
const Schema = "brant_ext"

// Lock modes.
const (
	NoLock    = ""
	ForUpdate = " FOR UPDATE"
	ForShare  = " FOR SHARE"
)

// ErrConflict is a compare-and-set miss.
var ErrConflict = errors.New("plugin: activation changed")

// Activation is one company's plugin row.
type Activation struct {
	PluginID        string
	DocumentProfile string
	Status          string
	Version         string
	ContractVersion string
	ManifestSHA256  string
	CostProvider    bool
	RowVersion      int64
	AuthorityEpoch  int64
	ActorKind       string
	ActorID         string
	UpdatedAt       time.Time
}

// Load returns the activation of pluginID, or nil. lock is empty, ForUpdate or ForShare.
func Load(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, pluginID, lock string) (*Activation, error) {
	row := exec.QueryRow(ctx, `SELECT plugin_id, document_profile, status, version, contract_version, manifest_sha256,
		cost_provider, row_version, authority_epoch, activated_by_kind, activated_by_id, updated_at
		FROM brant_ext.plugin_activation WHERE tenant_id = $1 AND company_id = $2 AND plugin_id = $3`+lock,
		string(scope.Tenant), string(scope.Company), pluginID)
	return scanOne(row)
}

// List returns every activation in the scope, by plugin id.
func List(ctx context.Context, exec bentopg.Executor, scope kernel.Scope) ([]Activation, error) {
	rows, err := exec.Query(ctx, `SELECT plugin_id, document_profile, status, version, contract_version, manifest_sha256,
		cost_provider, row_version, authority_epoch, activated_by_kind, activated_by_id, updated_at
		FROM brant_ext.plugin_activation WHERE tenant_id = $1 AND company_id = $2 ORDER BY plugin_id`,
		string(scope.Tenant), string(scope.Company))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Activation, error) {
		a, err := scanOne(row)
		if err != nil || a == nil {
			return Activation{}, err
		}
		return *a, nil
	})
}

func scanOne(row interface{ Scan(dest ...any) error }) (*Activation, error) {
	var a Activation
	err := row.Scan(&a.PluginID, &a.DocumentProfile, &a.Status, &a.Version, &a.ContractVersion, &a.ManifestSHA256,
		&a.CostProvider, &a.RowVersion, &a.AuthorityEpoch, &a.ActorKind, &a.ActorID, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.UpdatedAt = kernel.Instant(a.UpdatedAt)
	return &a, nil
}

// OtherActiveCostProvider returns the id of another active or draining cost provider for profile, or "".
func OtherActiveCostProvider(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, profile, except string) (string, error) {
	var id string
	err := exec.QueryRow(ctx, `SELECT plugin_id FROM brant_ext.plugin_activation
		WHERE tenant_id = $1 AND company_id = $2 AND document_profile = $3 AND cost_provider
		  AND status IN ('active', 'draining_readonly') AND plugin_id <> $4
		LIMIT 1 FOR SHARE`, string(scope.Tenant), string(scope.Company), profile, except).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// Insert creates an active row at version 1 and its first history line.
func Insert(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, a Activation, at time.Time) error {
	t, c := string(scope.Tenant), string(scope.Company)
	if _, err := exec.Exec(ctx, `INSERT INTO brant_ext.plugin_activation (tenant_id, company_id, plugin_id, document_profile,
		status, version, contract_version, manifest_sha256, cost_provider, row_version, authority_epoch,
		activated_by_kind, activated_by_id, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		t, c, a.PluginID, a.DocumentProfile, a.Status, a.Version, a.ContractVersion, a.ManifestSHA256, a.CostProvider,
		a.RowVersion, a.AuthorityEpoch, a.ActorKind, a.ActorID, at); err != nil {
		return err
	}
	return insertHistory(ctx, exec, t, c, a, "", at)
}

// Update moves the row from version to a.RowVersion and appends history. fromStatus is the previous status.
func Update(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, a Activation, fromVersion int64, fromStatus string, at time.Time) error {
	t, c := string(scope.Tenant), string(scope.Company)
	tag, err := exec.Exec(ctx, `UPDATE brant_ext.plugin_activation SET document_profile = $4, status = $5, version = $6,
		contract_version = $7, manifest_sha256 = $8, cost_provider = $9, row_version = $10, authority_epoch = $11,
		activated_by_kind = $12, activated_by_id = $13, updated_at = $14
		WHERE tenant_id = $1 AND company_id = $2 AND plugin_id = $3 AND row_version = $15`,
		t, c, a.PluginID, a.DocumentProfile, a.Status, a.Version, a.ContractVersion, a.ManifestSHA256, a.CostProvider,
		a.RowVersion, a.AuthorityEpoch, a.ActorKind, a.ActorID, at, fromVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return insertHistory(ctx, exec, t, c, a, fromStatus, at)
}

func insertHistory(ctx context.Context, exec bentopg.Executor, tenant, company string, a Activation, from string, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_ext.plugin_activation_history (tenant_id, company_id, plugin_id, seq,
		from_status, to_status, version, manifest_sha256, authority_epoch, actor_kind, actor_id, recorded_at)
		VALUES ($1, $2, $3, $4, nullif($5, ''), $6, $7, $8, $9, $10, $11, $12)`,
		tenant, company, a.PluginID, a.RowVersion, from, a.Status, a.Version, a.ManifestSHA256, a.AuthorityEpoch,
		a.ActorKind, a.ActorID, at)
	return err
}

// CostProviderConflict reports a unique violation of the one-active-cost-provider index.
func CostProviderConflict(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "plugin_activation_one_active_cost"
}
