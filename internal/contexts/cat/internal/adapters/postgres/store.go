// Package postgres stores CAT SKUs and item code mappings in schema brant_cat, on the executor of
// CAT's own transaction.
package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/sku"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is CAT's schema.
const Schema = "brant_cat"

// ErrConflict means a compare-and-set update found another row version.
var ErrConflict = errors.New("cat: row version changed")

const columns = `sku_id, status, row_version, spec_revision, coalesce(name, ''), coalesce(base_uom, ''), owner_actor_id, coalesce(owner_team_id, '')`

func scan(row pgx.Row) (*sku.Sku, error) {
	var s sku.Sku
	var status string
	err := row.Scan(&s.ID, &status, &s.RowVersion, &s.Revision, &s.Name, &s.BaseUnit, &s.OwnerActor, &s.OwnerTeam)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	s.Status = sku.Status(status)
	return &s, err
}

// Load returns the SKU id, or nil; forUpdate locks the root row.
func Load(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, forUpdate bool) (*sku.Sku, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	return scan(exec.QueryRow(ctx, `SELECT `+columns+` FROM brant_cat.sku WHERE tenant_id = $1 AND company_id = $2 AND sku_id = $3`+lock,
		string(scope.Tenant), string(scope.Company), id))
}

// Skus returns the SKUs among ids, without locks.
func Skus(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ids []string) ([]*sku.Sku, error) {
	rows, err := exec.Query(ctx, `SELECT `+columns+` FROM brant_cat.sku WHERE tenant_id = $1 AND company_id = $2 AND sku_id = ANY ($3)`,
		string(scope.Tenant), string(scope.Company), ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*sku.Sku, error) { return scan(row) })
}

// Insert stores a new SKU.
func Insert(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *sku.Sku, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cat.sku (tenant_id, company_id, sku_id, status, row_version, spec_revision, name, base_uom,
		owner_actor_id, owner_team_id, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), nullif($8, ''), $9, nullif($10, ''), $11, $11)`,
		string(scope.Tenant), string(scope.Company), s.ID, string(s.Status), s.RowVersion, s.Revision, s.Name, s.BaseUnit, s.OwnerActor, s.OwnerTeam, at)
	return err
}

// Update writes s if the stored row is still at version from.
func Update(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *sku.Sku, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cat.sku SET status = $4, row_version = $5, spec_revision = $6, name = nullif($7, ''),
		base_uom = nullif($8, ''), updated_at = $9 WHERE tenant_id = $1 AND company_id = $2 AND sku_id = $3 AND row_version = $10`,
		string(scope.Tenant), string(scope.Company), s.ID, string(s.Status), s.RowVersion, s.Revision, s.Name, s.BaseUnit, at, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// InsertRevision keeps the confirmed specification revision of s unchanged for good.
func InsertRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *sku.Sku, evidence []string, by kernel.ActorRef, epoch int64, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cat.sku_revision (tenant_id, company_id, sku_id, spec_revision, row_version, name, base_uom,
		evidence_ids, confirmed_by_kind, confirmed_by_id, authority_epoch, confirmed_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		string(scope.Tenant), string(scope.Company), s.ID, s.Revision, s.RowVersion, s.Name, s.BaseUnit, evidence, string(by.Kind), by.ID, epoch, at)
	return err
}

// Revision is a stored specification revision.
type Revision struct {
	Revision    int64
	Name        string
	BaseUnit    string
	EvidenceIDs []string
	By          kernel.ActorRef
}

// Revisions returns the confirmed specification revisions of a SKU, oldest first.
func Revisions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]Revision, error) {
	rows, err := exec.Query(ctx, `SELECT spec_revision, name, base_uom, evidence_ids, confirmed_by_kind, confirmed_by_id FROM brant_cat.sku_revision
		WHERE tenant_id = $1 AND company_id = $2 AND sku_id = $3 ORDER BY spec_revision`, string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Revision, error) {
		var r Revision
		var kind string
		err := row.Scan(&r.Revision, &r.Name, &r.BaseUnit, &r.EvidenceIDs, &kind, &r.By.ID)
		r.By.Kind = kernel.ActorKind(kind)
		return r, err
	})
}
