package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/fulfilment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Shipment reviews after an order change (S5-09b; brant_ful.shipment_review and
// brant_ful.shipment_review_item, migration 000005).

// LockFulfilmentIfAny locks the fulfilment root of order orderID FOR UPDATE, as LockFulfilment does,
// and reports whether the order has one: a review of the order is an entity of that root (tactical
// model FUL-AGG-OrderFulfilment), so reviews lock it first, as every confirmation and release of the
// order does after its shipment.
func LockFulfilmentIfAny(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string) (bool, error) {
	t, c := scope2(scope)
	var id string
	err := exec.QueryRow(ctx, `SELECT order_id FROM brant_ful.order_fulfilment WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3
		FOR UPDATE`, t, c, orderID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// OrderHoldings returns what FUL holds of order orderID: its shipments that are not cancelled, by
// shipment ID, each with its lines, current packing record of each kind, current booking, document set
// with its draft versions and commercial releases, and what its confirmed shipments shipped of each
// order line and unit over every revision. It locks nothing: the caller holds the order's fulfilment
// root, so no shipment of the order is confirmed meanwhile.
func OrderHoldings(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string) (fulfilment.Holdings, error) {
	t, c := scope2(scope)
	h := fulfilment.Holdings{Rooted: true, Shipped: map[fulfilment.LineUnit]kernel.Decimal{}}
	rows, err := exec.Query(ctx, `SELECT shipment_id, status, row_version, order_revision FROM brant_ful.shipment
		WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 AND status <> 'cancelled' ORDER BY shipment_id COLLATE "C"`, t, c, orderID)
	if err != nil {
		return h, err
	}
	h.Shipments, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (fulfilment.HeldShipment, error) {
		var s fulfilment.HeldShipment
		return s, r.Scan(&s.ID, &s.Status, &s.RowVersion, &s.Revision)
	})
	if err != nil || len(h.Shipments) == 0 {
		return h, err
	}
	ids := make([]string, len(h.Shipments))
	at := map[string]*fulfilment.HeldShipment{}
	for i := range h.Shipments {
		ids[i], at[h.Shipments[i].ID] = h.Shipments[i].ID, &h.Shipments[i]
	}
	each := func(sql string, scan func(r pgx.Rows) error) error {
		rows, err := exec.Query(ctx, sql, t, c, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err = each(`SELECT shipment_id, line_id, order_line_id, sku_id, spec_revision, unit_id, quantity::text FROM brant_ful.shipment_line
		WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = ANY ($3) ORDER BY shipment_id, line_no`, func(r pgx.Rows) error {
		var id, q string
		var l fulfilment.HeldLine
		if err := r.Scan(&id, &l.LineID, &l.OrderLineID, &l.SkuID, &l.SpecRevision, &l.UnitID, &q); err != nil {
			return err
		}
		var err error
		if l.Quantity, err = quantity(q); err != nil {
			return err
		}
		at[id].Lines = append(at[id].Lines, l)
		return nil
	}); err != nil {
		return h, err
	}
	if err = each(`SELECT DISTINCT ON (shipment_id, kind) shipment_id, kind, seq, carton_count, package_count FROM brant_ful.shipment_packing
		WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = ANY ($3) ORDER BY shipment_id, kind, seq DESC`, func(r pgx.Rows) error {
		var id string
		var p fulfilment.HeldPacking
		if err := r.Scan(&id, &p.Kind, &p.Seq, &p.Cartons, &p.Packages); err != nil {
			return err
		}
		at[id].Packing = append(at[id].Packing, p)
		return nil
	}); err != nil {
		return h, err
	}
	if err = each(`SELECT DISTINCT ON (shipment_id) shipment_id, version, booking_number, vessel, voyage, etd::text FROM brant_ful.shipment_booking
		WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = ANY ($3) ORDER BY shipment_id, version DESC`, func(r pgx.Rows) error {
		var id string
		var b fulfilment.HeldBooking
		if err := r.Scan(&id, &b.Version, &b.Number, &b.Vessel, &b.Voyage, &b.ETD); err != nil {
			return err
		}
		at[id].Booking = &b
		return nil
	}); err != nil {
		return h, err
	}
	if err = each(`SELECT s.shipment_id, s.document_set_id, coalesce(d.document_kind, ''), coalesce(d.version, 0), coalesce(d.number, '')
		FROM brant_ful.shipment_document_set s LEFT JOIN brant_ful.shipment_document d ON d.tenant_id = s.tenant_id
			AND d.company_id = s.company_id AND d.document_set_id = s.document_set_id AND d.status = 'draft'
		WHERE s.tenant_id = $1 AND s.company_id = $2 AND s.shipment_id = ANY ($3) ORDER BY s.shipment_id, d.document_kind, d.version`,
		func(r pgx.Rows) error {
			var id, set string
			var d fulfilment.HeldDocument
			if err := r.Scan(&id, &set, &d.Kind, &d.Version, &d.Number); err != nil {
				return err
			}
			at[id].DocumentSetID = set
			if d.Version > 0 {
				at[id].Drafts = append(at[id].Drafts, d)
			}
			return nil
		}); err != nil {
		return h, err
	}
	// The revision a release's payment condition was last judged on: the order revision of the latest
	// evaluation that judged the documents release of a confirmed shipment (S5 C1b: its bindings name
	// the confirmation and the order's current binding) or the exception granted on it, else the
	// revision the release binds. Revisions only grow, so the greatest is the latest.
	if err = each(`SELECT r.shipment_id, r.release_id, r.scope, r.status, r.row_version, r.evaluations,
			greatest(r.order_revision,
				(SELECT max((e.bindings->'order'->>'order_revision')::bigint) FROM brant_ful.commercial_release_evaluation e
					WHERE e.tenant_id = r.tenant_id AND e.company_id = r.company_id AND e.release_id = r.release_id AND e.bindings ? 'confirmation'),
				(SELECT max(x.order_revision) FROM brant_ful.commercial_release_exception x
					WHERE x.tenant_id = r.tenant_id AND x.company_id = r.company_id AND x.release_id = r.release_id))
		FROM brant_ful.commercial_release r
		WHERE r.tenant_id = $1 AND r.company_id = $2 AND r.shipment_id = ANY ($3) ORDER BY r.shipment_id, r.scope DESC`, func(r pgx.Rows) error {
		var id string
		var x fulfilment.HeldRelease
		if err := r.Scan(&id, &x.ID, &x.Scope, &x.Status, &x.RowVersion, &x.Evaluations, &x.Judged); err != nil {
			return err
		}
		at[id].Releases = append(at[id].Releases, x)
		return nil
	}); err != nil {
		return h, err
	}
	rows, err = exec.Query(ctx, `SELECT l.order_line_id, l.unit_id, trim_scale(sum(l.quantity))::text FROM brant_ful.shipment_line l
		JOIN brant_ful.shipment s ON s.tenant_id = l.tenant_id AND s.company_id = l.company_id AND s.shipment_id = l.shipment_id
		WHERE l.tenant_id = $1 AND l.company_id = $2 AND s.order_id = $3 AND s.status = 'confirmed' GROUP BY l.order_line_id, l.unit_id`,
		t, c, orderID)
	if err != nil {
		return h, err
	}
	defer rows.Close()
	for rows.Next() {
		var k fulfilment.LineUnit
		var q string
		if err = rows.Scan(&k.OrderLineID, &k.UnitID, &q); err != nil {
			return h, err
		}
		if h.Shipped[k], err = quantity(q); err != nil {
			return h, err
		}
	}
	return h, rows.Err()
}

// ReviewNow returns what FUL shows now of shipments ids: order, state, bound revision and
// cancellation, the latest packing record of each kind, the latest booking version, the latest
// version of each document kind of its set, and its release of each scope with its evaluations and
// when it was last evaluated. Shipments it does not find are absent. It locks nothing.
func ReviewNow(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ids []string) (fulfilment.Now, error) {
	t, c := scope2(scope)
	now := fulfilment.Now{Shipments: map[string]fulfilment.ShipmentNow{}}
	if len(ids) == 0 {
		return now, nil
	}
	rows, err := exec.Query(ctx, `SELECT shipment_id, order_id, status, order_revision, cancelled_at FROM brant_ful.shipment
		WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = ANY ($3)`, t, c, ids)
	if err != nil {
		return now, err
	}
	for rows.Next() {
		var id string
		var s fulfilment.ShipmentNow
		var at *time.Time
		if err = rows.Scan(&id, &s.OrderID, &s.Status, &s.Revision, &at); err != nil {
			rows.Close()
			return now, err
		}
		if at != nil {
			s.CancelledAt = kernel.Instant(*at)
		}
		s.Packing, s.Documents, s.Releases = map[string]int64{}, map[string]int64{}, map[string]fulfilment.ReleaseNow{}
		now.Shipments[id] = s
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return now, err
	}
	each := func(sql string, scan func(r pgx.Rows) error) error {
		rows, err := exec.Query(ctx, sql, t, c, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err = each(`SELECT shipment_id, kind, max(seq) FROM brant_ful.shipment_packing WHERE tenant_id = $1 AND company_id = $2
		AND shipment_id = ANY ($3) GROUP BY shipment_id, kind`, func(r pgx.Rows) error {
		var id, kind string
		var seq int64
		if err := r.Scan(&id, &kind, &seq); err != nil {
			return err
		}
		now.Shipments[id].Packing[kind] = seq
		return nil
	}); err != nil {
		return now, err
	}
	if err = each(`SELECT shipment_id, max(version) FROM brant_ful.shipment_booking WHERE tenant_id = $1 AND company_id = $2
		AND shipment_id = ANY ($3) GROUP BY shipment_id`, func(r pgx.Rows) error {
		var id string
		var v int64
		if err := r.Scan(&id, &v); err != nil {
			return err
		}
		s := now.Shipments[id]
		s.Booking = v
		now.Shipments[id] = s
		return nil
	}); err != nil {
		return now, err
	}
	if err = each(`SELECT s.shipment_id, d.document_kind, max(d.version) FROM brant_ful.shipment_document d
		JOIN brant_ful.shipment_document_set s ON s.tenant_id = d.tenant_id AND s.company_id = d.company_id AND s.document_set_id = d.document_set_id
		WHERE d.tenant_id = $1 AND d.company_id = $2 AND s.shipment_id = ANY ($3) GROUP BY s.shipment_id, d.document_kind`, func(r pgx.Rows) error {
		var id, kind string
		var v int64
		if err := r.Scan(&id, &kind, &v); err != nil {
			return err
		}
		now.Shipments[id].Documents[kind] = v
		return nil
	}); err != nil {
		return now, err
	}
	err = each(`SELECT r.shipment_id, r.scope, r.release_id, r.status, r.evaluations, (SELECT max(e.evaluated_at)
			FROM brant_ful.commercial_release_evaluation e WHERE e.tenant_id = r.tenant_id AND e.company_id = r.company_id
			AND e.release_id = r.release_id), r.consumed_at
		FROM brant_ful.commercial_release r WHERE r.tenant_id = $1 AND r.company_id = $2 AND r.shipment_id = ANY ($3)`, func(r pgx.Rows) error {
		var id, scope string
		var x fulfilment.ReleaseNow
		var at, consumed *time.Time
		if err := r.Scan(&id, &scope, &x.ID, &x.Status, &x.Evaluations, &at, &consumed); err != nil {
			return err
		}
		if at != nil {
			x.EvaluatedAt = kernel.Instant(*at)
		}
		if consumed != nil {
			x.ConsumedAt = kernel.Instant(*consumed)
		}
		now.Shipments[id].Releases[scope] = x
		return nil
	})
	return now, err
}

const reviewColumns = `review_id, order_id, order_revision, order_hash, order_version, cause, event_id, status, row_version, responsible_role,
	item_count, opened_by_kind, opened_by_id, open_epoch, opened_at, settled_by_kind, settled_by_id, settle_epoch, settled_at, settle_reason,
	coalesce(superseded_by, ''), superseded_at`

func scanReview(row pgx.Row) (*fulfilment.Review, int, error) {
	v := &fulfilment.Review{}
	var count int
	var openKind string
	var sKind, sID, sReason *string
	var sEpoch *int64
	var sAt, supAt *time.Time
	if err := row.Scan(&v.ID, &v.OrderID, &v.Revision, &v.Hash, &v.Version, &v.Cause, &v.EventID, &v.Status, &v.RowVersion, &v.Role, &count,
		&openKind, &v.Opened.By.ID, &v.Opened.Epoch, &v.Opened.At, &sKind, &sID, &sEpoch, &sAt, &sReason, &v.SupersededBy, &supAt); err != nil {
		return nil, 0, err
	}
	v.Opened.By.Kind, v.Opened.At = kernel.ActorKind(openKind), kernel.Instant(v.Opened.At)
	if sKind != nil && sID != nil && sEpoch != nil && sAt != nil {
		a := &fulfilment.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*sKind), ID: *sID}, Epoch: *sEpoch, At: kernel.Instant(*sAt)}
		if sReason != nil {
			a.Reason = *sReason
		}
		v.Settled = a
	}
	if supAt != nil {
		v.SupersededAt = kernel.Instant(*supAt)
	}
	return v, count, nil
}

// loadItems reads the items of review r in item order and checks that they are the count the review
// row states.
func loadItems(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *fulfilment.Review, count int) error {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT item_no, facts, disposition, disposition_shipment_id, disposition_revision, disposition_ref,
		disposition_status, disposition_at, disposition_reason FROM brant_ful.shipment_review_item
		WHERE tenant_id = $1 AND company_id = $2 AND review_id = $3 ORDER BY item_no`, t, c, r.ID)
	if err != nil {
		return err
	}
	r.Items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (fulfilment.Item, error) {
		var it fulfilment.Item
		var facts string
		var kind, ship, ref, status, reason *string
		var revision *int64
		var at *time.Time
		if err := row.Scan(&it.No, &facts, &kind, &ship, &revision, &ref, &status, &at, &reason); err != nil {
			return it, err
		}
		var err error
		if it.Facts, err = fulfilment.ParseFacts(facts); err != nil {
			return it, fmt.Errorf("ful: stored facts of review %s item %d: %w", r.ID, it.No, err)
		}
		if kind != nil {
			d := &fulfilment.Disposition{Kind: *kind}
			for _, f := range []struct {
				from *string
				to   *string
			}{{ship, &d.ShipmentID}, {ref, &d.Ref}, {status, &d.Status}, {reason, &d.Reason}} {
				if f.from != nil {
					*f.to = *f.from
				}
			}
			if revision != nil {
				d.Revision = *revision
			}
			if at != nil {
				d.At = kernel.Instant(*at)
			}
			it.Done = d
		}
		return it, nil
	})
	if err == nil && len(r.Items) != count {
		err = fmt.Errorf("ful: review %s states %d items, %d are stored", r.ID, count, len(r.Items))
	}
	return err
}

// LoadReview returns review id with its items, or nil; lock locks the review row.
func LoadReview(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*fulfilment.Review, error) {
	t, c := scope2(scope)
	r, count, err := scanReview(exec.QueryRow(ctx, `SELECT `+reviewColumns+` FROM brant_ful.shipment_review WHERE tenant_id = $1 AND company_id = $2
		AND review_id = $3`+string(lock), t, c, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, loadItems(ctx, exec, scope, r, count)
}

// ReviewsOfOrder returns the reviews of order orderID by order version, with their items; lock locks
// the review rows.
func ReviewsOfOrder(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string, lock Lock) ([]*fulfilment.Review, error) {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT `+reviewColumns+` FROM brant_ful.shipment_review WHERE tenant_id = $1 AND company_id = $2
		AND order_id = $3 ORDER BY order_version, review_id`+string(lock), t, c, orderID)
	if err != nil {
		return nil, err
	}
	type counted struct {
		r *fulfilment.Review
		n int
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (counted, error) {
		r, n, err := scanReview(row)
		return counted{r, n}, err
	})
	if err != nil {
		return nil, err
	}
	out := make([]*fulfilment.Review, 0, len(list))
	for _, x := range list {
		if err = loadItems(ctx, exec, scope, x.r, x.n); err != nil {
			return nil, err
		}
		out = append(out, x.r)
	}
	return out, nil
}

// InsertReview stores open review r with its items.
func InsertReview(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *fulfilment.Review) error {
	t, c := scope2(scope)
	if _, err := exec.Exec(ctx, `INSERT INTO brant_ful.shipment_review (tenant_id, company_id, review_id, order_id, order_revision, order_hash,
		order_version, cause, event_id, status, row_version, responsible_role, item_count, opened_by_kind, opened_by_id, open_epoch, opened_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		t, c, r.ID, r.OrderID, r.Revision, r.Hash, r.Version, r.Cause, r.EventID, r.Status, r.RowVersion, r.Role, len(r.Items),
		string(r.Opened.By.Kind), r.Opened.By.ID, r.Opened.Epoch, r.Opened.At); err != nil {
		return err
	}
	for _, it := range r.Items {
		f := it.Facts
		var evaluations *int64
		if f.Kind == fulfilment.ItemRelease {
			evaluations = &f.ReleaseEvaluations
		}
		doc := f.JSON()
		if _, err := exec.Exec(ctx, `INSERT INTO brant_ful.shipment_review_item (tenant_id, company_id, review_id, item_no, kind, shipment_id,
			shipment_status, packing_kind, document_kind, release_id, release_scope, release_evaluations, facts, facts_sha256)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, encode(sha256(convert_to($13, 'UTF8')), 'hex'))`,
			t, c, r.ID, it.No, f.Kind, f.ShipmentID, f.ShipmentStatus, nullable(f.PackingKind), nullable(f.DocumentKind), nullable(f.ReleaseID),
			nullable(f.ReleaseScope), evaluations, doc); err != nil {
			return err
		}
	}
	return nil
}

// SupersedeReview writes that open review r was superseded.
func SupersedeReview(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *fulfilment.Review) error {
	t, c := scope2(scope)
	tag, err := exec.Exec(ctx, `UPDATE brant_ful.shipment_review SET status = $4, row_version = $5, superseded_by = $6, superseded_at = $7
		WHERE tenant_id = $1 AND company_id = $2 AND review_id = $3 AND status = 'open'`, t, c, r.ID, r.Status, r.RowVersion, r.SupersededBy,
		r.SupersededAt)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// SettleReview writes review r's dispositions, then its settlement.
func SettleReview(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *fulfilment.Review) error {
	t, c := scope2(scope)
	for _, it := range r.Items {
		d := it.Done
		var revision *int64
		var at *time.Time
		if d.Revision > 0 {
			revision = &d.Revision
		}
		if !d.At.IsZero() {
			at = &d.At
		}
		if _, err := exec.Exec(ctx, `UPDATE brant_ful.shipment_review_item SET disposition = $5, disposition_shipment_id = $6,
			disposition_revision = $7, disposition_ref = $8, disposition_status = $9, disposition_at = $10, disposition_reason = $11
			WHERE tenant_id = $1 AND company_id = $2 AND review_id = $3 AND item_no = $4`, t, c, r.ID, it.No, d.Kind, nullable(d.ShipmentID),
			revision, nullable(d.Ref), nullable(d.Status), at, d.Reason); err != nil {
			return err
		}
	}
	s := r.Settled
	tag, err := exec.Exec(ctx, `UPDATE brant_ful.shipment_review SET status = $4, row_version = $5, settled_by_kind = $6, settled_by_id = $7,
		settle_epoch = $8, settled_at = $9, settle_reason = $10 WHERE tenant_id = $1 AND company_id = $2 AND review_id = $3 AND status = 'open'`,
		t, c, r.ID, r.Status, r.RowVersion, string(s.By.Kind), s.By.ID, s.Epoch, s.At, nullable(s.Reason))
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}
