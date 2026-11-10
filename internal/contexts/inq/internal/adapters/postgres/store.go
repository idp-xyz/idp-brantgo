// Package postgres stores INQ RFQs in schema brant_inq, on the executor of INQ's own transaction.
package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/inq/internal/domain/rfq"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is INQ's schema.
const Schema = "brant_inq"

// ErrConflict means a compare-and-set update found another row version.
var ErrConflict = errors.New("inq: row version changed")

// Load returns the RFQ id with its lines and sources, or nil; forUpdate locks the root row.
func Load(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, forUpdate bool) (*rfq.Rfq, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	t, c := string(scope.Tenant), string(scope.Company)
	var r rfq.Rfq
	var status, confirmedKind, confirmedID string
	var customerVersion, contactVersion, epoch *int64
	var evidence []string
	err := exec.QueryRow(ctx, `SELECT rfq_id, status, row_version, coalesce(customer_party_id, ''), coalesce(contact_id, ''),
		unresolved_fields, owner_actor_id, coalesce(owner_team_id, ''), customer_row_version, contact_row_version, evidence_ids,
		coalesce(confirmed_by_kind, ''), coalesce(confirmed_by_id, ''), authority_epoch
		FROM brant_inq.customer_rfq WHERE tenant_id = $1 AND company_id = $2 AND rfq_id = $3`+lock, t, c, id).
		Scan(&r.ID, &status, &r.RowVersion, &r.CustomerID, &r.ContactID, &r.Unresolved, &r.OwnerActor, &r.OwnerTeam,
			&customerVersion, &contactVersion, &evidence, &confirmedKind, &confirmedID, &epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Status = rfq.Status(status)
	if epoch != nil {
		r.Confirmation = &rfq.Confirmation{CustomerVersion: *customerVersion, ContactVersion: *contactVersion, Evidence: evidence,
			By: kernel.ActorRef{Kind: kernel.ActorKind(confirmedKind), ID: confirmedID}, Epoch: *epoch}
	}
	rows, err := exec.Query(ctx, `SELECT line_id, coalesce(sku_id, ''), coalesce(quantity, ''), coalesce(uom, ''),
		coalesce(requested_item, ''), coalesce(source_id, ''), coalesce(locator, ''), coalesce(sku_spec_revision, 0)
		FROM brant_inq.rfq_line WHERE tenant_id = $1 AND company_id = $2 AND rfq_id = $3 ORDER BY position`, t, c, id)
	if err != nil {
		return nil, err
	}
	r.Lines, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (rfq.Line, error) {
		var l rfq.Line
		err := row.Scan(&l.ID, &l.SkuID, &l.Quantity, &l.UOM, &l.RequestedItem, &l.SourceID, &l.Locator, &l.SkuSpecRevision)
		return l, err
	})
	if err != nil {
		return nil, err
	}
	rows, err = exec.Query(ctx, `SELECT source_id FROM brant_inq.rfq_source WHERE tenant_id = $1 AND company_id = $2 AND rfq_id = $3
		ORDER BY source_id`, t, c, id)
	if err != nil {
		return nil, err
	}
	r.Sources, err = pgx.CollectRows(rows, pgx.RowTo[string])
	return &r, err
}

// Insert stores a new RFQ with its lines and sources.
func Insert(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *rfq.Rfq, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_inq.customer_rfq (tenant_id, company_id, rfq_id, status, row_version, customer_party_id,
		contact_id, unresolved_fields, owner_actor_id, owner_team_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), $8, $9, nullif($10, ''), $11, $11)`,
		string(scope.Tenant), string(scope.Company), r.ID, string(r.Status), r.RowVersion, r.CustomerID, r.ContactID,
		nonNil(r.Unresolved), r.OwnerActor, r.OwnerTeam, at)
	if err != nil {
		return err
	}
	return writeContent(ctx, exec, scope, r, false)
}

// Update writes the RFQ if the stored row is still at version from. With content, it first replaces
// the lines and sources, while the stored RFQ is still a draft (a trigger refuses line changes of
// any other RFQ).
func Update(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *rfq.Rfq, from int64, content bool, at time.Time) error {
	if content {
		if err := writeContent(ctx, exec, scope, r, true); err != nil {
			return err
		}
	}
	var customerVersion, contactVersion, epoch any
	var evidence []string
	var kind, id any
	if c := r.Confirmation; c != nil {
		customerVersion, contactVersion, epoch, evidence, kind, id = c.CustomerVersion, c.ContactVersion, c.Epoch, c.Evidence, string(c.By.Kind), c.By.ID
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_inq.customer_rfq SET status = $4, row_version = $5, customer_party_id = nullif($6, ''),
		contact_id = nullif($7, ''), unresolved_fields = $8, customer_row_version = $9, contact_row_version = $10, evidence_ids = $11,
		confirmed_by_kind = $12, confirmed_by_id = $13, authority_epoch = $14,
		confirmed_at = CASE WHEN $14::bigint IS NULL THEN NULL ELSE coalesce(confirmed_at, $15) END, updated_at = $15
		WHERE tenant_id = $1 AND company_id = $2 AND rfq_id = $3 AND row_version = $16`,
		string(scope.Tenant), string(scope.Company), r.ID, string(r.Status), r.RowVersion, r.CustomerID, r.ContactID, nonNil(r.Unresolved),
		customerVersion, contactVersion, evidence, kind, id, epoch, at, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

func writeContent(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *rfq.Rfq, replace bool) error {
	t, c := string(scope.Tenant), string(scope.Company)
	if replace {
		if _, err := exec.Exec(ctx, `DELETE FROM brant_inq.rfq_line WHERE tenant_id = $1 AND company_id = $2 AND rfq_id = $3`, t, c, r.ID); err != nil {
			return err
		}
		if _, err := exec.Exec(ctx, `DELETE FROM brant_inq.rfq_source WHERE tenant_id = $1 AND company_id = $2 AND rfq_id = $3`, t, c, r.ID); err != nil {
			return err
		}
	}
	for i, l := range r.Lines {
		var revision any
		if l.SkuSpecRevision > 0 {
			revision = l.SkuSpecRevision
		}
		_, err := exec.Exec(ctx, `INSERT INTO brant_inq.rfq_line (tenant_id, company_id, rfq_id, line_id, position, sku_id, quantity, uom,
			requested_item, source_id, locator, sku_spec_revision) VALUES ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), nullif($8, ''),
			nullif($9, ''), nullif($10, ''), nullif($11, ''), $12)`,
			t, c, r.ID, l.ID, i, l.SkuID, l.Quantity, l.UOM, l.RequestedItem, l.SourceID, l.Locator, revision)
		if err != nil {
			return err
		}
	}
	for _, s := range r.Sources {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_inq.rfq_source (tenant_id, company_id, rfq_id, source_id) VALUES ($1, $2, $3, $4)`,
			t, c, r.ID, s); err != nil {
			return err
		}
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
