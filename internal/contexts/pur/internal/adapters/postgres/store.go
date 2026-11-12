// Package postgres stores PUR's purchase drafts, demand allocations and purchase commitments in
// schema brant_pur, on the executor of PUR's own transaction.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/allocation"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is PUR's schema.
const Schema = "brant_pur"

// ErrConflict means a compare-and-set update found another version.
var ErrConflict = errors.New("pur: version changed")

// Lock is how a read locks the rows it returns.
type Lock string

const (
	NoLock    Lock = ""
	ForUpdate Lock = " FOR UPDATE"
)

// lockOf is lock restricted to table alias, for a query that joins a function.
func lockOf(lock Lock, alias string) string {
	if lock == NoLock {
		return ""
	}
	return string(lock) + " OF " + alias
}

func quantity(s string) (kernel.Decimal, error) {
	// numeric(18,6) prints six fractional digits; the stored value is a QuantityFormat value.
	d, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	if err != nil {
		return kernel.Decimal{}, fmt.Errorf("pur: stored quantity %q: %w", s, err)
	}
	return d, nil
}

func decimalText(d kernel.Decimal) string {
	s, _ := d.Exact()
	return s
}

const draftColumns = `draft_id, status, row_version, commitment_state, order_id, order_revision, order_hash, supplier_id, currency,
	line_count, request, request_hash, document, draft_hash, supply_valid_until, owner_actor_id, coalesce(owner_team_id, ''),
	prepared_by_kind, prepared_by_id, prepare_epoch, prepared_at, voided_by_kind, voided_by_id, void_epoch, voided_at, void_reason`

// LoadDraft returns draft id with its lines, or nil; lock locks the draft row.
func LoadDraft(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*draft.Draft, error) {
	var d draft.Draft
	var request, document, preparedKind string
	var lineCount int
	var vKind, vID, vReason *string
	var vEpoch *int64
	var vAt *time.Time
	err := exec.QueryRow(ctx, `SELECT `+draftColumns+` FROM brant_pur.purchase_draft WHERE tenant_id = $1 AND company_id = $2
		AND draft_id = $3`+string(lock), string(scope.Tenant), string(scope.Company), id).
		Scan(&d.ID, &d.Status, &d.RowVersion, &d.CommitmentState, &d.OrderID, &d.OrderRevision, &d.OrderHash, &d.SupplierID, &d.Currency,
			&lineCount, &request, &d.RequestHash, &document, &d.Hash, &d.SupplyValidUntil, &d.OwnerActor, &d.OwnerTeam,
			&preparedKind, &d.Prepared.By.ID, &d.Prepared.Epoch, &d.Prepared.At, &vKind, &vID, &vEpoch, &vAt, &vReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.Request, d.Document = []byte(request), []byte(document)
	d.Prepared.By.Kind = kernel.ActorKind(preparedKind)
	d.Prepared.At, d.SupplyValidUntil = kernel.Instant(d.Prepared.At), kernel.Instant(d.SupplyValidUntil)
	if vAt != nil && vKind != nil && vID != nil && vEpoch != nil && vReason != nil {
		d.Void = &draft.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*vKind), ID: *vID}, Epoch: *vEpoch, At: kernel.Instant(*vAt), Reason: *vReason}
	}
	rows, err := exec.Query(ctx, `SELECT order_line_id, quantity::text, overbuy::text, source_kind, source_id, source_revision, condition_line_id
		FROM brant_pur.purchase_draft_line WHERE tenant_id = $1 AND company_id = $2 AND draft_id = $3 ORDER BY line_no`,
		string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	d.Lines, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (draft.Line, error) {
		var l draft.Line
		var q, o string
		if err := row.Scan(&l.OrderLineID, &q, &o, &l.Source.Kind, &l.Source.ID, &l.Source.Revision, &l.Source.ConditionLineID); err != nil {
			return l, err
		}
		var err error
		if l.Quantity, err = quantity(q); err != nil {
			return l, err
		}
		l.Overbuy, err = quantity(o)
		return l, err
	})
	if err != nil {
		return nil, err
	}
	if len(d.Lines) != lineCount {
		// A record whose claims are not all there is not intact; Verify refuses it.
		d.Lines = append(d.Lines, draft.Line{})
	}
	return &d, nil
}

// InsertDraft stores a new draft and its lines.
func InsertDraft(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, d *draft.Draft) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_pur.purchase_draft (tenant_id, company_id, draft_id, status, row_version, commitment_state,
		order_id, order_revision, order_hash, supplier_id, currency, line_count, request, request_hash, document, draft_hash,
		supply_valid_until, owner_actor_id, owner_team_id, prepared_by_kind, prepared_by_id, prepare_epoch, prepared_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, nullif($19, ''), $20, $21, $22, $23)`,
		string(scope.Tenant), string(scope.Company), d.ID, d.Status, d.RowVersion, d.CommitmentState, d.OrderID, d.OrderRevision, d.OrderHash,
		d.SupplierID, d.Currency, len(d.Lines), string(d.Request), d.RequestHash, string(d.Document), d.Hash, d.SupplyValidUntil,
		d.OwnerActor, d.OwnerTeam, string(d.Prepared.By.Kind), d.Prepared.By.ID, d.Prepared.Epoch, d.Prepared.At)
	if err != nil {
		return err
	}
	for i, l := range d.Lines {
		if _, err = exec.Exec(ctx, `INSERT INTO brant_pur.purchase_draft_line (tenant_id, company_id, draft_id, order_line_id, line_no, order_id,
			quantity, overbuy, source_kind, source_id, source_revision, condition_line_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8::numeric, $9, $10, $11, $12)`,
			string(scope.Tenant), string(scope.Company), d.ID, l.OrderLineID, i, d.OrderID, decimalText(l.Quantity), decimalText(l.Overbuy),
			l.Source.Kind, l.Source.ID, l.Source.Revision, l.Source.ConditionLineID); err != nil {
			return err
		}
	}
	return nil
}

// VoidDraft writes d's void if the stored row is still at version from.
func VoidDraft(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, d *draft.Draft, from int64) error {
	v := d.Void
	tag, err := exec.Exec(ctx, `UPDATE brant_pur.purchase_draft SET status = $4, row_version = $5, voided_by_kind = $6, voided_by_id = $7,
		void_epoch = $8, voided_at = $9, void_reason = $10 WHERE tenant_id = $1 AND company_id = $2 AND draft_id = $3 AND row_version = $11`,
		string(scope.Tenant), string(scope.Company), d.ID, d.Status, d.RowVersion, string(v.By.Kind), v.By.ID, v.Epoch, v.At, v.Reason, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// DraftOwner is who owns a draft, for visibility.
type DraftOwner struct {
	Actor string
	Team  string
}

// DraftOwners returns the owners of the drafts of ids that exist.
func DraftOwners(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ids []string) (map[string]DraftOwner, error) {
	out := map[string]DraftOwner{}
	rows, err := exec.Query(ctx, `SELECT draft_id, owner_actor_id, coalesce(owner_team_id, '') FROM brant_pur.purchase_draft
		WHERE tenant_id = $1 AND company_id = $2 AND draft_id = ANY($3)`, string(scope.Tenant), string(scope.Company), ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var o DraftOwner
		if err = rows.Scan(&id, &o.Actor, &o.Team); err != nil {
			return nil, err
		}
		out[id] = o
	}
	return out, rows.Err()
}

// LoadAllocations returns the roots of keys that exist, with their entries; lock locks the roots in
// (order_id, order_line_id) order.
func LoadAllocations(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, keys []allocation.Key, lock Lock) (map[allocation.Key]*allocation.Allocation, error) {
	out := map[allocation.Key]*allocation.Allocation{}
	if len(keys) == 0 {
		return out, nil
	}
	orders, lines := make([]string, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		orders[i], lines[i] = k.OrderID, k.LineID
	}
	rows, err := exec.Query(ctx, `SELECT a.order_id, a.order_line_id, a.order_revision, a.unit_id, a.unit_revision, a.demand::text,
		a.allocated::text, a.row_version FROM brant_pur.demand_allocation a
		JOIN unnest($3::text[], $4::text[]) AS k(order_id, order_line_id) ON k.order_id = a.order_id AND k.order_line_id = a.order_line_id
		WHERE a.tenant_id = $1 AND a.company_id = $2 ORDER BY a.order_id, a.order_line_id`+lockOf(lock, "a"),
		string(scope.Tenant), string(scope.Company), orders, lines)
	if err != nil {
		return nil, err
	}
	roots, err := collectRoots(rows)
	if err != nil {
		return nil, err
	}
	for _, a := range roots {
		out[a.Key] = a
	}
	return out, entries(ctx, exec, scope, roots)
}

// AllocationsOfOrder returns the roots of every line of order orderID, with their entries, by line.
func AllocationsOfOrder(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string) ([]*allocation.Allocation, error) {
	rows, err := exec.Query(ctx, `SELECT order_id, order_line_id, order_revision, unit_id, unit_revision, demand::text, allocated::text,
		row_version FROM brant_pur.demand_allocation WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 ORDER BY order_line_id`,
		string(scope.Tenant), string(scope.Company), orderID)
	if err != nil {
		return nil, err
	}
	roots, err := collectRoots(rows)
	if err != nil {
		return nil, err
	}
	return roots, entries(ctx, exec, scope, roots)
}

func collectRoots(rows pgx.Rows) ([]*allocation.Allocation, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*allocation.Allocation, error) {
		a := &allocation.Allocation{}
		var demand, allocated string
		if err := row.Scan(&a.Key.OrderID, &a.Key.LineID, &a.OrderRevision, &a.Unit.ID, &a.Unit.Revision, &demand, &allocated, &a.RowVersion); err != nil {
			return nil, err
		}
		var err error
		if a.Demand, err = quantity(demand); err != nil {
			return nil, err
		}
		if a.Allocated, err = quantity(allocated); err != nil {
			return nil, err
		}
		a.Stored = a.RowVersion
		return a, nil
	})
}

// entries loads the entries of roots, in the order they were taken.
func entries(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, roots []*allocation.Allocation) error {
	if len(roots) == 0 {
		return nil
	}
	byKey := map[allocation.Key]*allocation.Allocation{}
	orders, lines := make([]string, len(roots)), make([]string, len(roots))
	for i, a := range roots {
		byKey[a.Key] = a
		orders[i], lines[i] = a.Key.OrderID, a.Key.LineID
	}
	rows, err := exec.Query(ctx, `SELECT e.order_id, e.order_line_id, e.draft_id, e.quantity::text, e.taken_version,
		coalesce(e.released_version, 0), e.released_at FROM brant_pur.allocation_entry e
		JOIN unnest($3::text[], $4::text[]) AS k(order_id, order_line_id) ON k.order_id = e.order_id AND k.order_line_id = e.order_line_id
		WHERE e.tenant_id = $1 AND e.company_id = $2 ORDER BY e.order_id, e.order_line_id, e.taken_version`,
		string(scope.Tenant), string(scope.Company), orders, lines)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k allocation.Key
		var e allocation.Entry
		var q string
		var at *time.Time
		if err = rows.Scan(&k.OrderID, &k.LineID, &e.DraftID, &q, &e.Taken, &e.Released, &at); err != nil {
			return err
		}
		if e.Quantity, err = quantity(q); err != nil {
			return err
		}
		if at != nil {
			e.At = kernel.Instant(*at)
		}
		if a := byKey[k]; a != nil {
			a.Entries = append(a.Entries, e)
		}
	}
	return rows.Err()
}

// SaveAllocations writes the roots a command changed: a new root is inserted, a stored one updated
// with a compare-and-set on the row version it was loaded at (ErrConflict otherwise); then the
// entries taken are inserted and the entries released updated. Call it after the draft's lines are
// stored.
func SaveAllocations(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, roots []*allocation.Allocation, at time.Time) error {
	for _, a := range roots {
		if a.New() {
			if _, err := exec.Exec(ctx, `INSERT INTO brant_pur.demand_allocation (tenant_id, company_id, order_id, order_line_id, order_revision,
				unit_id, unit_revision, demand, allocated, row_version, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9::numeric, $10, $11, $11)`,
				string(scope.Tenant), string(scope.Company), a.Key.OrderID, a.Key.LineID, a.OrderRevision, a.Unit.ID, a.Unit.Revision,
				decimalText(a.Demand), decimalText(a.Allocated), a.RowVersion, at); err != nil {
				return err
			}
		} else {
			// The conditional update of the balance (PUR-INV-002): only the version the root was locked at,
			// and only within the demand.
			tag, err := exec.Exec(ctx, `UPDATE brant_pur.demand_allocation SET order_revision = $5, demand = $6::numeric, allocated = $7::numeric,
				row_version = $8, updated_at = $9 WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 AND order_line_id = $4
				AND row_version = $10 AND $7::numeric <= $6::numeric`,
				string(scope.Tenant), string(scope.Company), a.Key.OrderID, a.Key.LineID, a.OrderRevision, decimalText(a.Demand),
				decimalText(a.Allocated), a.RowVersion, at, a.Stored)
			if err == nil && tag.RowsAffected() != 1 {
				err = ErrConflict
			}
			if err != nil {
				return err
			}
		}
		for _, e := range a.Fresh() {
			if _, err := exec.Exec(ctx, `INSERT INTO brant_pur.allocation_entry (tenant_id, company_id, order_id, order_line_id, draft_id,
				quantity, taken_version) VALUES ($1, $2, $3, $4, $5, $6::numeric, $7)`,
				string(scope.Tenant), string(scope.Company), a.Key.OrderID, a.Key.LineID, e.DraftID, decimalText(e.Quantity), e.Taken); err != nil {
				return err
			}
		}
		for _, e := range a.Released() {
			tag, err := exec.Exec(ctx, `UPDATE brant_pur.allocation_entry SET released_version = $6, released_at = $7
				WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 AND order_line_id = $4 AND draft_id = $5 AND released_version IS NULL`,
				string(scope.Tenant), string(scope.Company), a.Key.OrderID, a.Key.LineID, e.DraftID, e.Released, e.At)
			if err == nil && tag.RowsAffected() != 1 {
				err = ErrConflict
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}
