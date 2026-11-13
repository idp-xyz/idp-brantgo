package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/exportlot"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ForShare locks the rows a read returns FOR SHARE: export_lot.register holds its shipment so, and
// shipment.confirm and shipment.cancel, which lock it FOR UPDATE, commit before or after it.
const ForShare Lock = " FOR SHARE"

func minor(s string) (*big.Int, error) {
	v, ok := shipment.Minor(s)
	if !ok {
		return nil, fmt.Errorf("ful: stored amount %q", s)
	}
	return v, nil
}

// MinorText is the integer text of an amount in minor units ("0" for none).
func MinorText(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}

// InsertPrices stores the price of every line of shipment s.
func InsertPrices(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *shipment.Shipment, prices map[string]shipment.Price) error {
	t, c := scope2(scope)
	for _, l := range s.Lines {
		p, ok := prices[l.ID]
		if !ok {
			return fmt.Errorf("ful: shipment %s line %s has no price", s.ID, l.ID)
		}
		if _, err := exec.Exec(ctx, `INSERT INTO brant_ful.shipment_line_price (tenant_id, company_id, shipment_id, line_id, currency_id,
			currency_revision, unit_price, net_minor, tax_minor, payable_minor) VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8::numeric, $9::numeric,
			$10::numeric)`, t, c, s.ID, l.ID, p.CurrencyID, p.CurrencyRevision, Text(p.UnitPrice), MinorText(p.Net), MinorText(p.Tax),
			MinorText(p.Payable)); err != nil {
			return err
		}
	}
	return nil
}

// LoadPrices returns the prices of the lines of shipment id, by line ID; a shipment prepared before
// FUL kept prices has none.
func LoadPrices(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (map[string]shipment.Price, error) {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT line_id, currency_id, currency_revision, unit_price::text, net_minor::text, tax_minor::text,
		payable_minor::text FROM brant_ful.shipment_line_price WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3`, t, c, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]shipment.Price{}
	for rows.Next() {
		var line, unit, net, tax, payable string
		var p shipment.Price
		if err = rows.Scan(&line, &p.CurrencyID, &p.CurrencyRevision, &unit, &net, &tax, &payable); err != nil {
			return nil, err
		}
		if p.UnitPrice, err = kernel.ParseDecimal(unit, kernel.PriceFormat); err != nil {
			return nil, fmt.Errorf("ful: stored unit price %q: %w", unit, err)
		}
		if p.Net, err = minor(net); err != nil {
			return nil, err
		}
		if p.Tax, err = minor(tax); err != nil {
			return nil, err
		}
		if p.Payable, err = minor(payable); err != nil {
			return nil, err
		}
		out[line] = p
	}
	return out, rows.Err()
}

const lotColumns = `export_lot_id, status, row_version, shipment_id, shipment_status, shipment_version, order_id, order_revision, order_hash,
	customer_id, currency_id, currency_revision, net_minor::text, tax_minor::text, payable_minor::text, request_hash, owner_actor_id,
	coalesce(owner_team_id, ''), registered_by_kind, registered_by_id, register_epoch, registered_at`

func scanLot(row pgx.Row) (*exportlot.Lot, error) {
	var l exportlot.Lot
	var net, tax, payable, kind string
	if err := row.Scan(&l.ID, &l.Status, &l.RowVersion, &l.ShipmentID, &l.ShipmentStatus, &l.ShipmentVersion, &l.Order.OrderID, &l.Order.OrderRevision,
		&l.Order.OrderHash, &l.CustomerID, &l.CurrencyID, &l.CurrencyRevision, &net, &tax, &payable, &l.RequestHash, &l.OwnerActor, &l.OwnerTeam, &kind,
		&l.Registered.By.ID, &l.Registered.Epoch, &l.Registered.At); err != nil {
		return nil, err
	}
	l.Registered.By.Kind, l.Registered.At = kernel.ActorKind(kind), kernel.Instant(l.Registered.At)
	var err error
	if l.Totals.Net, err = minor(net); err != nil {
		return nil, err
	}
	if l.Totals.Tax, err = minor(tax); err != nil {
		return nil, err
	}
	l.Totals.Payable, err = minor(payable)
	return &l, err
}

// loadLotLines reads the lines of lot l with what their shipment lines and prices say.
func loadLotLines(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, l *exportlot.Lot) error {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT x.line_id, x.shipment_line_id, s.order_line_id, s.sku_id, s.spec_revision, s.unit_id, s.unit_revision,
		x.quantity::text, s.quantity::text, s.order_line_quantity::text, p.unit_price::text, x.net_minor::text, x.tax_minor::text, x.payable_minor::text
		FROM brant_ful.export_lot_line x
		JOIN brant_ful.shipment_line s ON s.tenant_id = x.tenant_id AND s.company_id = x.company_id AND s.shipment_id = x.shipment_id
			AND s.line_id = x.shipment_line_id
		JOIN brant_ful.shipment_line_price p ON p.tenant_id = x.tenant_id AND p.company_id = x.company_id AND p.shipment_id = x.shipment_id
			AND p.line_id = x.shipment_line_id
		WHERE x.tenant_id = $1 AND x.company_id = $2 AND x.export_lot_id = $3 ORDER BY x.line_no`, t, c, l.ID)
	if err != nil {
		return err
	}
	l.Lines, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (exportlot.Line, error) {
		var x exportlot.Line
		var qty, shipped, ordered, unit, net, tax, payable string
		if err := row.Scan(&x.ID, &x.ShipmentLineID, &x.OrderLineID, &x.SkuID, &x.SpecRevision, &x.UnitID, &x.UnitRevision, &qty, &shipped, &ordered,
			&unit, &net, &tax, &payable); err != nil {
			return x, err
		}
		var err error
		if x.Quantity, err = quantity(qty); err != nil {
			return x, err
		}
		if x.ShipmentLineQuantity, err = quantity(shipped); err != nil {
			return x, err
		}
		if x.OrderLineQuantity, err = quantity(ordered); err != nil {
			return x, err
		}
		if x.UnitPrice, err = kernel.ParseDecimal(unit, kernel.PriceFormat); err != nil {
			return x, fmt.Errorf("ful: stored unit price %q: %w", unit, err)
		}
		if x.Amounts.Net, err = minor(net); err != nil {
			return x, err
		}
		if x.Amounts.Tax, err = minor(tax); err != nil {
			return x, err
		}
		x.Amounts.Payable, err = minor(payable)
		return x, err
	})
	return err
}

// LoadLot returns export lot id with its lines, or nil.
func LoadLot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*exportlot.Lot, error) {
	t, c := scope2(scope)
	l, err := scanLot(exec.QueryRow(ctx, `SELECT `+lotColumns+` FROM brant_ful.export_lot WHERE tenant_id = $1 AND company_id = $2
		AND export_lot_id = $3`, t, c, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return l, loadLotLines(ctx, exec, scope, l)
}

// LotsOfShipment returns the export lots of shipment id in lot ID order, with their lines.
func LotsOfShipment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*exportlot.Lot, error) {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT `+lotColumns+` FROM brant_ful.export_lot WHERE tenant_id = $1 AND company_id = $2 AND shipment_id = $3
		ORDER BY export_lot_id COLLATE "C"`, t, c, id)
	if err != nil {
		return nil, err
	}
	lots, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*exportlot.Lot, error) { return scanLot(row) })
	if err != nil {
		return nil, err
	}
	for _, l := range lots {
		if err = loadLotLines(ctx, exec, scope, l); err != nil {
			return nil, err
		}
	}
	return lots, nil
}

// TakenLines returns the lines of shipment id that an export lot holds: shipment line ID → lot ID.
func TakenLines(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (map[string]string, error) {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT shipment_line_id, export_lot_id FROM brant_ful.export_lot_line WHERE tenant_id = $1 AND company_id = $2
		AND shipment_id = $3`, t, c, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var line, lot string
		if err = rows.Scan(&line, &lot); err != nil {
			return nil, err
		}
		out[line] = lot
	}
	return out, rows.Err()
}

// InsertLot stores a new export lot with its lines.
func InsertLot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, l *exportlot.Lot) error {
	t, c := scope2(scope)
	if _, err := exec.Exec(ctx, `INSERT INTO brant_ful.export_lot (tenant_id, company_id, export_lot_id, status, row_version, shipment_id,
		shipment_status, shipment_version, order_id, order_revision, order_hash, customer_id, currency_id, currency_revision, line_count, net_minor,
		tax_minor, payable_minor, request_hash, owner_actor_id, owner_team_id, registered_by_kind, registered_by_id, register_epoch, registered_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16::numeric, $17::numeric, $18::numeric, $19, $20, $21, $22, $23, $24,
		$25)`, t, c, l.ID, l.Status, l.RowVersion, l.ShipmentID, l.ShipmentStatus, l.ShipmentVersion, l.Order.OrderID, l.Order.OrderRevision,
		l.Order.OrderHash, l.CustomerID, l.CurrencyID, l.CurrencyRevision, len(l.Lines), MinorText(l.Totals.Net), MinorText(l.Totals.Tax),
		MinorText(l.Totals.Payable), l.RequestHash, l.OwnerActor, nullable(l.OwnerTeam), string(l.Registered.By.Kind), l.Registered.By.ID,
		l.Registered.Epoch, l.Registered.At); err != nil {
		return err
	}
	for i, x := range l.Lines {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_ful.export_lot_line (tenant_id, company_id, export_lot_id, line_id, line_no, shipment_id,
			shipment_line_id, quantity, net_minor, tax_minor, payable_minor) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9::numeric, $10::numeric,
			$11::numeric)`, t, c, l.ID, x.ID, i+1, l.ShipmentID, x.ShipmentLineID, Text(x.Quantity), MinorText(x.Amounts.Net), MinorText(x.Amounts.Tax),
			MinorText(x.Amounts.Payable)); err != nil {
			return err
		}
	}
	return nil
}
