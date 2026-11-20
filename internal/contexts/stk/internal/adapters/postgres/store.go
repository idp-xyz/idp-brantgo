// Package postgres stores STK in schema brant_stk, on the executor of STK's own transaction.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stk/internal/domain/ledger"
	"go.idp.xyz/idp-brant/internal/contexts/stk/internal/domain/reservation"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is STK's schema.
const Schema = "brant_stk"

// ErrConflict means a compare-and-set update found another version.
var ErrConflict = errors.New("stk: version changed")

// Lock is how a read locks the row it returns.
type Lock string

const (
	NoLock    Lock = ""
	ForUpdate Lock = " FOR UPDATE"
)

// Text is the exact decimal text of d.
func Text(d kernel.Decimal) string {
	s, ok := d.Exact()
	if !ok {
		return d.String()
	}
	return s
}

func parseQty(s string) (kernel.Decimal, error) {
	d, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	if err != nil {
		return kernel.Decimal{}, fmt.Errorf("stk: stored quantity %q: %w", s, err)
	}
	return d, nil
}

func scope2(s kernel.Scope) (string, string) {
	return string(s.Tenant), string(s.Company)
}

// Setting is the company switch.
type Setting struct {
	Enabled    bool
	RowVersion int64
	ActorKind  string
	ActorID    string
	Epoch      int64
	UpdatedAt  time.Time
	Team       string
}

// LoadSetting returns the company switch, or nil when the company has never been configured.
func LoadSetting(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, lock Lock) (*Setting, error) {
	tenant, company := scope2(scope)
	var s Setting
	err := exec.QueryRow(ctx, `SELECT enabled, row_version, updated_by_kind, updated_by_id, update_epoch, updated_at
		FROM brant_stk.company_setting WHERE tenant_id = $1 AND company_id = $2`+string(lock), tenant, company).
		Scan(&s.Enabled, &s.RowVersion, &s.ActorKind, &s.ActorID, &s.Epoch, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.UpdatedAt = kernel.Instant(s.UpdatedAt)
	return &s, nil
}

// InsertSetting creates the company switch at version 1.
func InsertSetting(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s Setting) error {
	tenant, company := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stk.company_setting (tenant_id, company_id, enabled, row_version,
		updated_by_kind, updated_by_id, update_epoch, updated_at)
		VALUES ($1, $2, $3, 1, $4, $5, $6, $7)`,
		tenant, company, s.Enabled, s.ActorKind, s.ActorID, s.Epoch, s.UpdatedAt)
	return err
}

// SaveSetting updates the switch when expected is the stored version.
func SaveSetting(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s Setting, expected int64) error {
	tenant, company := scope2(scope)
	tag, err := exec.Exec(ctx, `UPDATE brant_stk.company_setting SET enabled = $3, row_version = $4, updated_by_kind = $5,
		updated_by_id = $6, update_epoch = $7, updated_at = $8
		WHERE tenant_id = $1 AND company_id = $2 AND row_version = $9`,
		tenant, company, s.Enabled, s.RowVersion, s.ActorKind, s.ActorID, s.Epoch, s.UpdatedAt, expected)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

const balanceSelect = `SELECT sku_id, location_id, batch_id, custody_kind, trim_scale(on_hand)::text, trim_scale(reserved)::text,
	unit_id, unit_revision, currency, minor_units, row_version, owner_actor_id, owner_team_id`

func scanBalance(row pgx.Row) (*ledger.Balance, error) {
	var b ledger.Balance
	var on, reserved string
	var team *string
	err := row.Scan(&b.SKU, &b.Location, &b.Batch, &b.Custody, &on, &reserved, &b.UnitID, &b.UnitRevision, &b.Currency,
		&b.MinorUnits, &b.RowVersion, &b.OwnerActor, &team)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if b.OnHand, err = parseQty(on); err != nil {
		return nil, err
	}
	if b.Reserved, err = parseQty(reserved); err != nil {
		return nil, err
	}
	if team != nil {
		b.OwnerTeam = *team
	}
	return &b, nil
}

// LoadBalance returns the balance, or nil. lock is applied when the row exists.
func LoadBalance(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, key ledger.Key, lock Lock) (*ledger.Balance, error) {
	tenant, company := scope2(scope)
	return scanBalance(exec.QueryRow(ctx, balanceSelect+` FROM brant_stk.stock_balance
		WHERE tenant_id = $1 AND company_id = $2 AND sku_id = $3 AND location_id = $4 AND batch_id = $5 AND custody_kind = $6`+string(lock),
		tenant, company, key.SKU, key.Location, key.Batch, key.Custody))
}

// InsertBalance creates a balance at version 1.
func InsertBalance(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, b ledger.Balance) error {
	tenant, company := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stk.stock_balance (tenant_id, company_id, sku_id, location_id, batch_id, custody_kind,
		on_hand, reserved, unit_id, unit_revision, currency, minor_units, row_version, owner_actor_id, owner_team_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8::numeric, $9, $10, $11, $12, 1, $13, $14)`,
		tenant, company, b.SKU, b.Location, b.Batch, b.Custody, Text(b.OnHand), Text(b.Reserved), b.UnitID, b.UnitRevision,
		b.Currency, b.MinorUnits, b.OwnerActor, empty(b.OwnerTeam))
	return err
}

// SaveBalance writes on_hand, reserved and row_version when expected is the stored version.
func SaveBalance(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, b ledger.Balance, expected int64) error {
	tenant, company := scope2(scope)
	tag, err := exec.Exec(ctx, `UPDATE brant_stk.stock_balance SET on_hand = $7::numeric, reserved = $8::numeric, row_version = $9
		WHERE tenant_id = $1 AND company_id = $2 AND sku_id = $3 AND location_id = $4 AND batch_id = $5 AND custody_kind = $6
		AND row_version = $10`,
		tenant, company, b.SKU, b.Location, b.Batch, b.Custody, Text(b.OnHand), Text(b.Reserved), b.RowVersion, expected)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// LoadLayers returns the layers of a balance, FIFO order, optionally locked in that order.
func LoadLayers(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, key ledger.Key, lock Lock) ([]ledger.Layer, error) {
	tenant, company := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT layer_id, movement_id, currency, minor_units, unit_cost_minor, trim_scale(price_basis_qty)::text,
		trim_scale(original_qty)::text, trim_scale(remaining_qty)::text, received_at
		FROM brant_stk.cost_layer
		WHERE tenant_id = $1 AND company_id = $2 AND sku_id = $3 AND location_id = $4 AND batch_id = $5 AND custody_kind = $6
		ORDER BY received_at, layer_id`+string(lock),
		tenant, company, key.SKU, key.Location, key.Batch, key.Custody)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ledger.Layer
	for rows.Next() {
		var l ledger.Layer
		var basis, original, remaining string
		if err = rows.Scan(&l.ID, &l.MovementID, &l.Currency, &l.MinorUnits, &l.UnitCostMinor, &basis, &original, &remaining, &l.ReceivedAt); err != nil {
			return nil, err
		}
		if l.Basis, err = parseQty(basis); err != nil {
			return nil, err
		}
		if l.Original, err = parseQty(original); err != nil {
			return nil, err
		}
		if l.Remaining, err = parseQty(remaining); err != nil {
			return nil, err
		}
		l.Key, l.ReceivedAt = key, kernel.Instant(l.ReceivedAt)
		out = append(out, l)
	}
	return out, rows.Err()
}

// InsertLayer appends a cost layer. The receipt movement already exists.
func InsertLayer(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, l ledger.Layer) error {
	tenant, company := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stk.cost_layer (tenant_id, company_id, layer_id, movement_id, sku_id, location_id,
		batch_id, custody_kind, currency, minor_units, unit_cost_minor, price_basis_qty, original_qty, remaining_qty, received_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::numeric, $13::numeric, $14::numeric, $15)`,
		tenant, company, l.ID, l.MovementID, l.Key.SKU, l.Key.Location, l.Key.Batch, l.Key.Custody, l.Currency, l.MinorUnits,
		l.UnitCostMinor, Text(l.Basis), Text(l.Original), Text(l.Remaining), l.ReceivedAt)
	return err
}

// SaveLayerRemaining sets the remaining quantity of a layer.
func SaveLayerRemaining(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, remaining kernel.Decimal) error {
	tenant, company := scope2(scope)
	tag, err := exec.Exec(ctx, `UPDATE brant_stk.cost_layer SET remaining_qty = $4::numeric
		WHERE tenant_id = $1 AND company_id = $2 AND layer_id = $3`, tenant, company, id, Text(remaining))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// Movement is one append-only stock movement.
type Movement struct {
	ID         string
	Kind       string
	Key        ledger.Key
	Quantity   kernel.Decimal
	UnitID     string
	UnitRev    int64
	SourceKind string
	SourceID   string
	SourceLine string
	Reason     string
	At         time.Time
	ActorKind  string
	ActorID    string
	Epoch      int64
	OwnerActor string
	OwnerTeam  string
}

// InsertMovement appends a movement.
func InsertMovement(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, m Movement) error {
	tenant, company := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stk.stock_movement (tenant_id, company_id, movement_id, kind, sku_id, location_id,
		batch_id, custody_kind, quantity, unit_id, unit_revision, source_kind, source_id, source_line_id, reason, at,
		actor_kind, actor_id, authority_epoch, owner_actor_id, owner_team_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)`,
		tenant, company, m.ID, m.Kind, m.Key.SKU, m.Key.Location, m.Key.Batch, m.Key.Custody, Text(m.Quantity), m.UnitID, m.UnitRev,
		empty(m.SourceKind), empty(m.SourceID), empty(m.SourceLine), empty(m.Reason), m.At, m.ActorKind, m.ActorID, m.Epoch,
		m.OwnerActor, empty(m.OwnerTeam))
	return err
}

// LoadMovement returns the movement, or nil.
func LoadMovement(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*Movement, error) {
	tenant, company := scope2(scope)
	var m Movement
	var qty string
	var sourceKind, sourceID, sourceLine, reason, team *string
	err := exec.QueryRow(ctx, `SELECT movement_id, kind, sku_id, location_id, batch_id, custody_kind, trim_scale(quantity)::text,
		unit_id, unit_revision, source_kind, source_id, source_line_id, reason, at, actor_kind, actor_id, authority_epoch,
		owner_actor_id, owner_team_id
		FROM brant_stk.stock_movement WHERE tenant_id = $1 AND company_id = $2 AND movement_id = $3`, tenant, company, id).
		Scan(&m.ID, &m.Kind, &m.Key.SKU, &m.Key.Location, &m.Key.Batch, &m.Key.Custody, &qty, &m.UnitID, &m.UnitRev,
			&sourceKind, &sourceID, &sourceLine, &reason, &m.At, &m.ActorKind, &m.ActorID, &m.Epoch, &m.OwnerActor, &team)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if m.Quantity, err = parseQty(qty); err != nil {
		return nil, err
	}
	m.SourceKind, m.SourceID, m.SourceLine, m.Reason = str(sourceKind), str(sourceID), str(sourceLine), str(reason)
	if team != nil {
		m.OwnerTeam = *team
	}
	m.At = kernel.Instant(m.At)
	return &m, nil
}

// ListMovements returns the movements of a balance, oldest first.
func ListMovements(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, key ledger.Key) ([]Movement, error) {
	tenant, company := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT movement_id, kind, trim_scale(quantity)::text, unit_id, unit_revision, source_kind, source_id,
		source_line_id, reason, at FROM brant_stk.stock_movement
		WHERE tenant_id = $1 AND company_id = $2 AND sku_id = $3 AND location_id = $4 AND batch_id = $5 AND custody_kind = $6
		ORDER BY at, movement_id`, tenant, company, key.SKU, key.Location, key.Batch, key.Custody)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Movement
	for rows.Next() {
		var m Movement
		var qty string
		var sourceKind, sourceID, sourceLine, reason *string
		if err = rows.Scan(&m.ID, &m.Kind, &qty, &m.UnitID, &m.UnitRev, &sourceKind, &sourceID, &sourceLine, &reason, &m.At); err != nil {
			return nil, err
		}
		if m.Quantity, err = parseQty(qty); err != nil {
			return nil, err
		}
		m.Key = key
		m.SourceKind, m.SourceID, m.SourceLine, m.Reason = str(sourceKind), str(sourceID), str(sourceLine), str(reason)
		m.At = kernel.Instant(m.At)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Allocation is one FIFO slice of an issue.
type Allocation struct {
	ID          string
	MovementID  string
	LayerID     string
	Quantity    kernel.Decimal
	AmountMinor int64
	Currency    string
}

// InsertAllocation appends a cost allocation.
func InsertAllocation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, a Allocation) error {
	tenant, company := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stk.cost_allocation (tenant_id, company_id, allocation_id, issue_movement_id, layer_id,
		quantity, amount_minor, currency) VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8)`,
		tenant, company, a.ID, a.MovementID, a.LayerID, Text(a.Quantity), a.AmountMinor, a.Currency)
	return err
}

func scanReservation(row pgx.Row) (*reservation.Reservation, error) {
	var r reservation.Reservation
	var qty, remaining string
	var orderID, orderLine, team *string
	err := row.Scan(&r.ID, &r.SKU, &r.Location, &r.Batch, &r.Custody, &orderID, &orderLine, &qty, &remaining, &r.Status,
		&r.UnitID, &r.UnitRevision, &r.RowVersion, &r.OwnerActor, &team)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.Quantity, err = parseQty(qty); err != nil {
		return nil, err
	}
	if r.Remaining, err = parseQty(remaining); err != nil {
		return nil, err
	}
	r.OrderID, r.OrderLineID = str(orderID), str(orderLine)
	if team != nil {
		r.OwnerTeam = *team
	}
	return &r, nil
}

// LoadReservation returns the reservation, or nil.
func LoadReservation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*reservation.Reservation, error) {
	tenant, company := scope2(scope)
	return scanReservation(exec.QueryRow(ctx, `SELECT reservation_id, sku_id, location_id, batch_id, custody_kind, order_id, order_line_id,
		trim_scale(quantity)::text, trim_scale(remaining_qty)::text, status, unit_id, unit_revision, row_version, owner_actor_id, owner_team_id
		FROM brant_stk.stock_reservation WHERE tenant_id = $1 AND company_id = $2 AND reservation_id = $3`+string(lock),
		tenant, company, id))
}

// InsertReservation creates an active hold at version 1. Remaining equals Quantity.
func InsertReservation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r reservation.Reservation) error {
	tenant, company := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stk.stock_reservation (tenant_id, company_id, reservation_id, sku_id, location_id, batch_id,
		custody_kind, order_id, order_line_id, quantity, remaining_qty, status, unit_id, unit_revision, row_version, owner_actor_id, owner_team_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::numeric, $11::numeric, $12, $13, $14, 1, $15, $16)`,
		tenant, company, r.ID, r.SKU, r.Location, r.Batch, r.Custody, empty(r.OrderID), empty(r.OrderLineID), Text(r.Quantity),
		Text(r.Remaining), r.Status, r.UnitID, r.UnitRevision, r.OwnerActor, empty(r.OwnerTeam))
	return err
}

// SaveReservation writes the remaining quantity, status and version when expected is current.
func SaveReservation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r reservation.Reservation, expected int64) error {
	tenant, company := scope2(scope)
	tag, err := exec.Exec(ctx, `UPDATE brant_stk.stock_reservation SET remaining_qty = $4::numeric, status = $5, row_version = $6
		WHERE tenant_id = $1 AND company_id = $2 AND reservation_id = $3 AND row_version = $7`,
		tenant, company, r.ID, Text(r.Remaining), r.Status, r.RowVersion, expected)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// Lineage is one batch link.
type Lineage struct {
	ID            string
	MovementID    string
	InputBatchID  string
	OutputKind    string
	OutputBatchID string
	OutputID      string
	OutputLineID  string
	Quantity      kernel.Decimal
	UnitID        string
	UnitRevision  int64
	At            time.Time
}

// InsertLineage appends a batch link.
func InsertLineage(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, l Lineage) error {
	tenant, company := scope2(scope)
	_, err := exec.Exec(ctx, `INSERT INTO brant_stk.batch_lineage (tenant_id, company_id, lineage_id, movement_id, input_batch_id,
		output_kind, output_batch_id, output_id, output_line_id, quantity, unit_id, unit_revision, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::numeric, $11, $12, $13)`,
		tenant, company, l.ID, l.MovementID, l.InputBatchID, l.OutputKind, empty(l.OutputBatchID), empty(l.OutputID),
		empty(l.OutputLineID), Text(l.Quantity), l.UnitID, l.UnitRevision, l.At)
	return err
}

// ShipmentLineCost is the stored FIFO cost of one shipment line.
type ShipmentLineCost struct {
	ShipmentID     string
	ShipmentLineID string
	MovementID     string
	Currency       string
	Total          int64
	Layers         []Allocation
}

// ShipmentCosts returns the shipment issues of shipmentIDs. Lines with no issue are absent.
func ShipmentCosts(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, shipmentIDs []string) ([]ShipmentLineCost, error) {
	if len(shipmentIDs) == 0 {
		return nil, nil
	}
	tenant, company := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT m.source_id, m.source_line_id, m.movement_id, a.allocation_id, a.layer_id,
		trim_scale(a.quantity)::text, a.amount_minor, a.currency
		FROM brant_stk.stock_movement m
		JOIN brant_stk.cost_allocation a ON a.tenant_id = m.tenant_id AND a.company_id = m.company_id AND a.issue_movement_id = m.movement_id
		WHERE m.tenant_id = $1 AND m.company_id = $2 AND m.source_kind = 'ful_shipment_line' AND m.source_id = ANY($3)
		ORDER BY m.source_id, m.source_line_id, a.allocation_id`, tenant, company, shipmentIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShipmentLineCost
	for rows.Next() {
		var ship, line, movement, allocID, layerID, qty, currency string
		var amount int64
		if err = rows.Scan(&ship, &line, &movement, &allocID, &layerID, &qty, &amount, &currency); err != nil {
			return nil, err
		}
		q, err := parseQty(qty)
		if err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].MovementID != movement {
			out = append(out, ShipmentLineCost{ShipmentID: ship, ShipmentLineID: line, MovementID: movement, Currency: currency})
		}
		cur := &out[len(out)-1]
		cur.Layers = append(cur.Layers, Allocation{ID: allocID, MovementID: movement, LayerID: layerID, Quantity: q, AmountMinor: amount, Currency: currency})
		cur.Total += amount
	}
	return out, rows.Err()
}

func empty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
