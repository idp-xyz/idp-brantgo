// Package postgres stores FUL's shipments and order fulfilment roots in schema brant_ful, on the
// executor of FUL's own transaction.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is FUL's schema.
const Schema = "brant_ful"

// ErrConflict means a compare-and-set update found another version.
var ErrConflict = errors.New("ful: version changed")

// Lock is how a read locks the rows it returns.
type Lock string

const (
	NoLock    Lock = ""
	ForUpdate Lock = " FOR UPDATE"
)

func quantity(s string) (kernel.Decimal, error) {
	d, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	if err != nil {
		return kernel.Decimal{}, fmt.Errorf("ful: stored quantity %q: %w", s, err)
	}
	return d, nil
}

// Text is the exact decimal text of d.
func Text(d kernel.Decimal) string {
	s, _ := d.Exact()
	return s
}

func scope2(s kernel.Scope) (string, string) { return string(s.Tenant), string(s.Company) }

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func strs(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// OpenFulfilment creates the fulfilment root of order orderID when it is missing, opened by by at
// now, and locks it FOR UPDATE: the lock shipment.prepare takes after its shipment (lock order:
// shipment, then the order's fulfilment root). Two first preparations of an order meet on the
// root's primary key and the second waits for the first.
func OpenFulfilment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string, by kernel.ActorRef, now time.Time) error {
	t, c := scope2(scope)
	if _, err := exec.Exec(ctx, `INSERT INTO brant_ful.order_fulfilment (tenant_id, company_id, order_id, opened_by_kind, opened_by_id, opened_at)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`, t, c, orderID, string(by.Kind), by.ID, kernel.Instant(now)); err != nil {
		return err
	}
	return LockFulfilment(ctx, exec, scope, orderID)
}

// LockFulfilment locks the fulfilment root of order orderID FOR UPDATE: the lock shipment.confirm
// takes after its shipment, so the confirmations of an order commit one at a time (D-3).
func LockFulfilment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string) error {
	t, c := scope2(scope)
	var id string
	err := exec.QueryRow(ctx, `SELECT order_id FROM brant_ful.order_fulfilment WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3
		FOR UPDATE`, t, c, orderID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("ful: order %s has no fulfilment root", orderID)
	}
	return err
}

const shipmentColumns = `shipment_id, status, row_version, route, order_id, order_revision, order_hash, customer_id, packing_records, request_hash,
	owner_actor_id, coalesce(owner_team_id, ''), prepared_by_kind, prepared_by_id, prepare_epoch, prepared_at, confirmed_by_kind, confirmed_by_id,
	confirm_epoch, confirmed_at, confirm_evidence_ids, guard_entries, guard_entries_sha256, unchecked_guards, cancelled_by_kind, cancelled_by_id,
	cancel_epoch, cancelled_at, cancel_reason`

func scanShipment(row pgx.Row) (*shipment.Shipment, error) {
	var s shipment.Shipment
	var preparedKind string
	var cKind, cID, entries, sum, xKind, xID, reason *string
	var cEpoch, xEpoch *int64
	var cAt, xAt *time.Time
	var evidence, unchecked []string
	err := row.Scan(&s.ID, &s.Status, &s.RowVersion, &s.Route, &s.Order.OrderID, &s.Order.OrderRevision, &s.Order.OrderHash, &s.CustomerID,
		&s.PackingRecords, &s.RequestHash, &s.OwnerActor, &s.OwnerTeam, &preparedKind, &s.Prepared.By.ID, &s.Prepared.Epoch, &s.Prepared.At,
		&cKind, &cID, &cEpoch, &cAt, &evidence, &entries, &sum, &unchecked, &xKind, &xID, &xEpoch, &xAt, &reason)
	if err != nil {
		return nil, err
	}
	s.Prepared.By.Kind, s.Prepared.At = kernel.ActorKind(preparedKind), kernel.Instant(s.Prepared.At)
	if cAt != nil && cKind != nil && cID != nil && cEpoch != nil && entries != nil && sum != nil {
		c := &shipment.Confirmation{Confirmed: shipment.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*cKind), ID: *cID}, Epoch: *cEpoch,
			At: kernel.Instant(*cAt)}, Evidence: strs(evidence), EntriesJSON: *entries, EntriesSHA256: *sum, Unchecked: strs(unchecked)}
		if err = json.Unmarshal([]byte(*entries), &c.Entries); err != nil {
			return nil, fmt.Errorf("ful: stored guard entries of %s: %w", s.ID, err)
		}
		s.Confirmation = c
	}
	if xAt != nil && xKind != nil && xID != nil && xEpoch != nil && reason != nil {
		s.Cancellation = &shipment.Cancellation{Reason: *reason, Cancelled: shipment.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*xKind), ID: *xID},
			Epoch: *xEpoch, At: kernel.Instant(*xAt)}}
	}
	return &s, nil
}

// LoadShipment returns shipment id with its lines, parties and current packing records, or nil;
// lock locks the shipment row.
func LoadShipment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*shipment.Shipment, error) {
	t, c := scope2(scope)
	s, err := scanShipment(exec.QueryRow(ctx, `SELECT `+shipmentColumns+` FROM brant_ful.shipment WHERE tenant_id = $1 AND company_id = $2
		AND shipment_id = $3`+string(lock), t, c, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = loadLines(ctx, exec, scope, s); err != nil {
		return nil, err
	}
	if err = loadParties(ctx, exec, scope, s); err != nil {
		return nil, err
	}
	if s.Plan, err = loadPacking(ctx, exec, scope, s.ID, shipment.KindPlan); err != nil {
		return nil, err
	}
	s.Actual, err = loadPacking(ctx, exec, scope, s.ID, shipment.KindActual)
	return s, err
}

func loadLines(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *shipment.Shipment) error {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT line_id, order_line_id, sku_id, spec_revision, unit_id, unit_revision, order_line_quantity::text,
		quantity::text, delivery_id, delivery_line_id, delivery_version, commitment_id, supplier_id, quality_release_id
		FROM brant_ful.shipment_line WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3 ORDER BY line_no`, t, c, s.ID)
	if err != nil {
		return err
	}
	s.Lines, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (shipment.Line, error) {
		var l shipment.Line
		var ordered, qty string
		if err := row.Scan(&l.ID, &l.OrderLineID, &l.SkuID, &l.SpecRevision, &l.UnitID, &l.UnitRevision, &ordered, &qty, &l.Delivery.DeliveryID,
			&l.Delivery.LineID, &l.DeliveryVersion, &l.CommitmentID, &l.SupplierID, &l.ReleaseID); err != nil {
			return l, err
		}
		var err error
		if l.OrderQuantity, err = quantity(ordered); err != nil {
			return l, err
		}
		l.Quantity, err = quantity(qty)
		return l, err
	})
	return err
}

func loadParties(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *shipment.Shipment) error {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT role, party_id, party_revision, coalesce(address_id, '') FROM brant_ful.shipment_party
		WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3 ORDER BY party_no`, t, c, s.ID)
	if err != nil {
		return err
	}
	s.Parties, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (shipment.Party, error) {
		var p shipment.Party
		return p, row.Scan(&p.Role, &p.PartyID, &p.PartyRevision, &p.AddressID)
	})
	return err
}

// loadPacking returns the current packing record of kind (the highest number), or nil.
func loadPacking(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id, kind string) (*shipment.Packing, error) {
	t, c := scope2(scope)
	p := &shipment.Packing{Kind: kind}
	var by string
	err := exec.QueryRow(ctx, `SELECT seq, shipment_version, evidence_ids, recorded_by_kind, recorded_by_id, record_epoch, recorded_at
		FROM brant_ful.shipment_packing WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3 AND kind = $4 ORDER BY seq DESC LIMIT 1`,
		t, c, id, kind).Scan(&p.Seq, &p.ShipmentVersion, &p.Evidence, &by, &p.Recorded.By.ID, &p.Recorded.Epoch, &p.Recorded.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Recorded.By.Kind, p.Recorded.At, p.Evidence = kernel.ActorKind(by), kernel.Instant(p.Recorded.At), strs(p.Evidence)
	rows, err := exec.Query(ctx, `SELECT package_no, cartons, coalesce(carton_from, 0), gross_weight_kg::text, net_weight_kg::text, volume_cbm::text
		FROM brant_ful.shipment_package WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3 AND kind = $4 AND seq = $5 ORDER BY package_no`,
		t, c, id, kind, p.Seq)
	if err != nil {
		return nil, err
	}
	p.Packages, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (shipment.Package, error) {
		var k shipment.Package
		var no int
		var gross, net, volume *string
		if err := row.Scan(&no, &k.Cartons, &k.CartonFrom, &gross, &net, &volume); err != nil {
			return k, err
		}
		for _, m := range []struct {
			from *string
			to   **kernel.Decimal
		}{{gross, &k.Gross}, {net, &k.Net}, {volume, &k.Volume}} {
			if m.from != nil {
				d, err := quantity(*m.from)
				if err != nil {
					return k, err
				}
				*m.to = &d
			}
		}
		return k, nil
	})
	if err != nil {
		return nil, err
	}
	rows, err = exec.Query(ctx, `SELECT c.package_no, c.line_id, c.quantity_per_carton::text FROM brant_ful.shipment_package_content c
		JOIN brant_ful.shipment_line l ON l.tenant_id = c.tenant_id AND l.company_id = c.company_id AND l.shipment_id = c.shipment_id AND l.line_id = c.line_id
		WHERE c.tenant_id = $1 AND c.company_id = $2 AND c.shipment_id = $3 AND c.kind = $4 AND c.seq = $5 ORDER BY c.package_no, l.line_no`,
		t, c, id, kind, p.Seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var no int
		var line, per string
		if err = rows.Scan(&no, &line, &per); err != nil {
			return nil, err
		}
		d, err := quantity(per)
		if err != nil {
			return nil, err
		}
		if no < 1 || no > len(p.Packages) {
			return nil, fmt.Errorf("ful: package %d of %s %s %d", no, id, kind, p.Seq)
		}
		p.Packages[no-1].Contents = append(p.Packages[no-1].Contents, shipment.Content{LineID: line, PerCarton: d})
	}
	return p, rows.Err()
}

// InsertShipment stores a new prepared shipment with its lines and parties.
func InsertShipment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *shipment.Shipment) error {
	t, c := scope2(scope)
	if _, err := exec.Exec(ctx, `INSERT INTO brant_ful.shipment (tenant_id, company_id, shipment_id, status, row_version, route, order_id,
		order_revision, order_hash, customer_id, line_count, party_count, packing_records, request_hash, owner_actor_id, owner_team_id,
		prepared_by_kind, prepared_by_id, prepare_epoch, prepared_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
		t, c, s.ID, s.Status, s.RowVersion, s.Route, s.Order.OrderID, s.Order.OrderRevision, s.Order.OrderHash, s.CustomerID, len(s.Lines),
		len(s.Parties), s.PackingRecords, s.RequestHash, s.OwnerActor, nullable(s.OwnerTeam), string(s.Prepared.By.Kind), s.Prepared.By.ID,
		s.Prepared.Epoch, s.Prepared.At); err != nil {
		return err
	}
	for i, l := range s.Lines {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_ful.shipment_line (tenant_id, company_id, shipment_id, line_id, line_no, order_line_id,
			sku_id, spec_revision, unit_id, unit_revision, order_line_quantity, quantity, delivery_id, delivery_line_id, delivery_version,
			commitment_id, supplier_id, quality_release_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::numeric, $12::numeric, $13, $14, $15, $16, $17, $18)`,
			t, c, s.ID, l.ID, i+1, l.OrderLineID, l.SkuID, l.SpecRevision, l.UnitID, l.UnitRevision, Text(l.OrderQuantity), Text(l.Quantity),
			l.Delivery.DeliveryID, l.Delivery.LineID, l.DeliveryVersion, l.CommitmentID, l.SupplierID, l.ReleaseID); err != nil {
			return err
		}
	}
	for i, p := range s.Parties {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_ful.shipment_party (tenant_id, company_id, shipment_id, party_no, role, party_id,
			party_revision, address_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			t, c, s.ID, i+1, p.Role, p.PartyID, p.PartyRevision, nullable(p.AddressID)); err != nil {
			return err
		}
	}
	return nil
}

// InsertPacking writes packing record p of shipment s, which moved from row version from.
func InsertPacking(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *shipment.Shipment, p *shipment.Packing, from int64) error {
	t, c := scope2(scope)
	tag, err := exec.Exec(ctx, `UPDATE brant_ful.shipment SET row_version = $4, packing_records = $5
		WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3 AND row_version = $6`, t, c, s.ID, s.RowVersion, s.PackingRecords, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	if err != nil {
		return err
	}
	if _, err = exec.Exec(ctx, `INSERT INTO brant_ful.shipment_packing (tenant_id, company_id, shipment_id, kind, seq, shipment_version,
		package_count, carton_count, evidence_ids, recorded_by_kind, recorded_by_id, record_epoch, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`, t, c, s.ID, p.Kind, p.Seq, p.ShipmentVersion, len(p.Packages),
		p.Cartons(), strs(p.Evidence), string(p.Recorded.By.Kind), p.Recorded.By.ID, p.Recorded.Epoch, p.Recorded.At); err != nil {
		return err
	}
	text := func(d *kernel.Decimal) *string {
		if d == nil {
			return nil
		}
		s := Text(*d)
		return &s
	}
	for i, k := range p.Packages {
		var from *int64
		if k.CartonFrom > 0 {
			from = &k.CartonFrom
		}
		if _, err = exec.Exec(ctx, `INSERT INTO brant_ful.shipment_package (tenant_id, company_id, shipment_id, kind, seq, package_no, cartons,
			carton_from, gross_weight_kg, net_weight_kg, volume_cbm) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10::numeric, $11::numeric)`,
			t, c, s.ID, p.Kind, p.Seq, i+1, k.Cartons, from, text(k.Gross), text(k.Net), text(k.Volume)); err != nil {
			return err
		}
		for _, x := range k.Contents {
			if _, err = exec.Exec(ctx, `INSERT INTO brant_ful.shipment_package_content (tenant_id, company_id, shipment_id, kind, seq, package_no,
				line_id, quantity_per_carton) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric)`,
				t, c, s.ID, p.Kind, p.Seq, i+1, x.LineID, Text(x.PerCarton)); err != nil {
				return err
			}
		}
	}
	return nil
}

// CancelShipment writes s's cancellation if the stored row is still at version from.
func CancelShipment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *shipment.Shipment, from int64) error {
	t, c := scope2(scope)
	x := s.Cancellation
	tag, err := exec.Exec(ctx, `UPDATE brant_ful.shipment SET status = $4, row_version = $5, cancelled_by_kind = $6, cancelled_by_id = $7,
		cancel_epoch = $8, cancelled_at = $9, cancel_reason = $10 WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3 AND row_version = $11`,
		t, c, s.ID, s.Status, s.RowVersion, string(x.Cancelled.By.Kind), x.Cancelled.By.ID, x.Cancelled.Epoch, x.Cancelled.At, x.Reason, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// ConfirmShipment writes s's confirmation if the stored row is still at version from. The table's
// trigger re-checks FUL-INV-001 on the rows as this transaction sees them.
func ConfirmShipment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *shipment.Shipment, from int64) error {
	t, c := scope2(scope)
	x := s.Confirmation
	tag, err := exec.Exec(ctx, `UPDATE brant_ful.shipment SET status = $4, row_version = $5, confirmed_by_kind = $6, confirmed_by_id = $7,
		confirm_epoch = $8, confirmed_at = $9, confirm_evidence_ids = $10, guard_entries = $11, guard_entries_sha256 = $12, unchecked_guards = $13
		WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3 AND row_version = $14`,
		t, c, s.ID, s.Status, s.RowVersion, string(x.Confirmed.By.Kind), x.Confirmed.By.ID, x.Confirmed.Epoch, x.Confirmed.At, strs(x.Evidence),
		x.EntriesJSON, x.EntriesSHA256, strs(x.Unchecked), from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// LoadConsumed returns what FUL's confirmed shipments shipped of order orderID's lines over every
// revision, of the delivery lines deliveries over every version and under releases, as a statement
// started now sees them. Called while the order's fulfilment root is held, it sees every
// confirmation that consumes them (D-3).
func LoadConsumed(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string, deliveries []shipment.DeliveryRef,
	releases []string) (shipment.Consumed, error) {
	t, c := scope2(scope)
	out := shipment.Consumed{OrderLines: map[string]kernel.Decimal{}, Deliveries: map[shipment.DeliveryRef]kernel.Decimal{},
		Releases: map[string]kernel.Decimal{}}
	collect := func(sql string, args []any, put func(key []string, d kernel.Decimal)) error {
		rows, err := exec.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a, b, q string
			if err = rows.Scan(&a, &b, &q); err != nil {
				return err
			}
			d, err := quantity(q)
			if err != nil {
				return err
			}
			put([]string{a, b}, d)
		}
		return rows.Err()
	}
	const confirmed = ` FROM brant_ful.shipment_line l JOIN brant_ful.shipment s ON s.tenant_id = l.tenant_id AND s.company_id = l.company_id
		AND s.shipment_id = l.shipment_id WHERE l.tenant_id = $1 AND l.company_id = $2 AND s.status = 'confirmed' AND `
	if err := collect(`SELECT l.order_line_id, '', trim_scale(sum(l.quantity))::text`+confirmed+`s.order_id = $3 GROUP BY l.order_line_id`,
		[]any{t, c, orderID}, func(k []string, d kernel.Decimal) { out.OrderLines[k[0]] = d }); err != nil {
		return out, err
	}
	if len(deliveries) > 0 {
		ids, lines := make([]string, len(deliveries)), make([]string, len(deliveries))
		for i, d := range deliveries {
			ids[i], lines[i] = d.DeliveryID, d.LineID
		}
		if err := collect(`SELECT l.delivery_id, l.delivery_line_id, trim_scale(sum(l.quantity))::text`+confirmed+
			`(l.delivery_id, l.delivery_line_id) IN (SELECT * FROM unnest($3::text[], $4::text[])) GROUP BY l.delivery_id, l.delivery_line_id`,
			[]any{t, c, ids, lines}, func(k []string, d kernel.Decimal) {
				out.Deliveries[shipment.DeliveryRef{DeliveryID: k[0], LineID: k[1]}] = d
			}); err != nil {
			return out, err
		}
	}
	if len(releases) > 0 {
		if err := collect(`SELECT l.quality_release_id, '', trim_scale(sum(l.quantity))::text`+confirmed+
			`l.quality_release_id = ANY ($3) GROUP BY l.quality_release_id`, []any{t, c, releases},
			func(k []string, d kernel.Decimal) { out.Releases[k[0]] = d }); err != nil {
			return out, err
		}
	}
	return out, nil
}

// OrderShipment is one shipment of an order as the fulfilment read lists it.
type OrderShipment struct {
	ID            string
	Status        string
	OrderRevision int64
	OwnerActor    string
	OwnerTeam     string
	ConfirmedAt   time.Time
}

// OrderLineSums is what an order line's shipments ship in the unit named: Shipped by confirmed
// shipments over every revision, Prepared by prepared ones.
type OrderLineSums struct {
	OrderLineID  string
	UnitID       string
	UnitRevision int64
	Shipped      kernel.Decimal
	Prepared     kernel.Decimal
}

// LoadOrderShipments returns the shipments of order orderID in shipment ID order and the sums of
// their lines per order line and unit, in order line order.
func LoadOrderShipments(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string) ([]OrderShipment, []OrderLineSums, error) {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT shipment_id, status, order_revision, owner_actor_id, coalesce(owner_team_id, ''), confirmed_at
		FROM brant_ful.shipment WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 ORDER BY shipment_id COLLATE "C"`, t, c, orderID)
	if err != nil {
		return nil, nil, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OrderShipment, error) {
		var s OrderShipment
		var at *time.Time
		err := row.Scan(&s.ID, &s.Status, &s.OrderRevision, &s.OwnerActor, &s.OwnerTeam, &at)
		if at != nil {
			s.ConfirmedAt = kernel.Instant(*at)
		}
		return s, err
	})
	if err != nil {
		return nil, nil, err
	}
	rows, err = exec.Query(ctx, `SELECT l.order_line_id, l.unit_id, l.unit_revision,
		trim_scale(coalesce(sum(l.quantity) FILTER (WHERE s.status = 'confirmed'), 0))::text,
		trim_scale(coalesce(sum(l.quantity) FILTER (WHERE s.status = 'prepared'), 0))::text
		FROM brant_ful.shipment_line l JOIN brant_ful.shipment s ON s.tenant_id = l.tenant_id AND s.company_id = l.company_id
		AND s.shipment_id = l.shipment_id WHERE l.tenant_id = $1 AND l.company_id = $2 AND s.order_id = $3
		GROUP BY l.order_line_id, l.unit_id, l.unit_revision ORDER BY l.order_line_id COLLATE "C", l.unit_id COLLATE "C", l.unit_revision`, t, c, orderID)
	if err != nil {
		return nil, nil, err
	}
	sums, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OrderLineSums, error) {
		var s OrderLineSums
		var shipped, prepared string
		if err := row.Scan(&s.OrderLineID, &s.UnitID, &s.UnitRevision, &shipped, &prepared); err != nil {
			return s, err
		}
		var err error
		if s.Shipped, err = quantity(shipped); err != nil {
			return s, err
		}
		s.Prepared, err = quantity(prepared)
		return s, err
	})
	return list, sums, err
}
