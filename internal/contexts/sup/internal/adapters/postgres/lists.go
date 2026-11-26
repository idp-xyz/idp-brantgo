package postgres

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/foundation/listquery"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The customer RFQ and the supplier a supplier RFQ or an offer revision names, which are members
// of its stored content. The indexes of 000003 are on exactly these expressions.
const (
	requestRfq      = `(data #>> '{rfq,rfq_id}')`
	requestSupplier = `(data #>> '{supplier,party_id}')`
	offerRfq        = `(v.data #>> '{rfq,rfq_id}')`
	offerSupplier   = `(v.data #>> '{supplier,party_id}')`
)

// SupplierRfqListRow is one supplier RFQ of a list.
type SupplierRfqListRow struct {
	ID, Status, RfqID, SupplierID, OwnerActor string
	RowVersion                                int64
	Lines                                     int
	UpdatedAt                                 time.Time
}

// ListSupplierRfqs returns the supplier RFQs of the scope the caller sees, in (updated_at,
// supplier_rfq_id) descending order (index supplier_rfq_listing), without locks.
func ListSupplierRfqs(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, rfq, supplier, status string, s listquery.Sight) ([]SupplierRfqListRow, error) {
	var w listquery.Where
	w.Scope("", scope)
	if rfq != "" {
		w.Add(requestRfq + " = " + w.Arg(rfq))
	}
	if supplier != "" {
		w.Add(requestSupplier + " = " + w.Arg(supplier))
	}
	if status != "" {
		w.Add("status = " + w.Arg(status))
	}
	w.Sees(s, "owner_actor_id", "owner_team_id")
	w.After(s, "updated_at", "supplier_rfq_id")
	rows, err := exec.Query(ctx, `SELECT supplier_rfq_id, status, row_version, coalesce(`+requestRfq+`, ''), coalesce(`+requestSupplier+`, ''),
		CASE WHEN jsonb_typeof(data -> 'rfq_line_ids') = 'array' THEN jsonb_array_length(data -> 'rfq_line_ids') ELSE 0 END, owner_actor_id, updated_at
		FROM brant_sup.supplier_rfq WHERE `+w.SQL()+` ORDER BY updated_at DESC, supplier_rfq_id DESC LIMIT `+w.Arg(s.Limit), w.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SupplierRfqListRow
	for rows.Next() {
		var r SupplierRfqListRow
		if err := rows.Scan(&r.ID, &r.Status, &r.RowVersion, &r.RfqID, &r.SupplierID, &r.Lines, &r.OwnerActor, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OfferListRow is one supplier offer of a list: the root and its latest revision.
type OfferListRow struct {
	ID, Status, RfqID, SupplierID, SupplierRfqID, OwnerActor string
	RootVersion, LastRevision, ActiveRevision                int64
	Lines                                                    int
	ValidFrom, ValidUntil                                    *time.Time
	UpdatedAt                                                time.Time
}

// ListOffers returns the supplier offers of the scope the caller sees, in (updated_at,
// condition_id) descending order (index condition_root_listing), without locks. rfq, supplier
// and status narrow by the latest revision when not empty.
func ListOffers(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, rfq, supplier, status string, s listquery.Sight) ([]OfferListRow, error) {
	var w listquery.Where
	w.Scope("r", scope)
	w.Add("r.kind = " + w.Arg(api.KindSupplierOffer))
	if rfq != "" {
		w.Add(offerRfq + " = " + w.Arg(rfq))
	}
	if supplier != "" {
		w.Add(offerSupplier + " = " + w.Arg(supplier))
	}
	if status != "" {
		w.Add("v.status = " + w.Arg(status))
	}
	w.Sees(s, "r.owner_actor_id", "r.owner_team_id")
	w.After(s, "r.updated_at", "r.condition_id")
	rows, err := exec.Query(ctx, `SELECT r.condition_id, r.root_version, r.last_revision, v.status,
		coalesce((SELECT a.revision FROM brant_sup.condition_revision a WHERE a.tenant_id = r.tenant_id AND a.company_id = r.company_id
			AND a.kind = r.kind AND a.condition_id = r.condition_id AND a.status = 'active'), 0),
		coalesce(`+offerRfq+`, ''), coalesce(`+offerSupplier+`, ''), coalesce(v.data #>> '{origin,supplier_rfq,supplier_rfq_id}', ''),
		v.valid_from, v.valid_until,
		CASE WHEN jsonb_typeof(v.data -> 'lines') = 'array' THEN jsonb_array_length(v.data -> 'lines') ELSE 0 END, r.owner_actor_id, r.updated_at
		FROM brant_sup.condition_root r
		JOIN brant_sup.condition_revision v ON v.tenant_id = r.tenant_id AND v.company_id = r.company_id AND v.kind = r.kind
			AND v.condition_id = r.condition_id AND v.revision = r.last_revision
		WHERE `+w.SQL()+` ORDER BY r.updated_at DESC, r.condition_id DESC LIMIT `+w.Arg(s.Limit), w.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OfferListRow
	for rows.Next() {
		var r OfferListRow
		if err := rows.Scan(&r.ID, &r.RootVersion, &r.LastRevision, &r.Status, &r.ActiveRevision, &r.RfqID, &r.SupplierID, &r.SupplierRfqID,
			&r.ValidFrom, &r.ValidUntil, &r.Lines, &r.OwnerActor, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
