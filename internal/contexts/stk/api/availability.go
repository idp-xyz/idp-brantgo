package api

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// StockAvailability v1 (Context Map R29 STK→FUL, OHS, guardLock true; STK-INV-001, STK-INV-004;
// S6-D02). FUL calls it inside shipment.confirm only, on route stock, with FUL's own executor,
// after FUL has locked its own roots and the authority guard and after STL's guard, and before
// TRD's guard. commercial_release.evaluate, judgeDocuments and supplier_direct do not call it.
//
//	brant_stk.stock_availability_issue_v1(
//	    p_shipment_id text, p_shipment_line_id text, p_sku_id text, p_location_id text,
//	    p_batch_id text, p_custody_kind text, p_reservation_id text, p_quantity text,
//	    p_unit_id text, p_unit_revision bigint, p_actor_kind text, p_actor_id text,
//	    p_epoch bigint, p_at timestamptz)
//	RETURNS TABLE (
//	    status text, movement_id text, total_cost_minor bigint, currency text,
//	    available_qty text, replayed boolean, code text)
//
// status is ok, module_disabled, insufficient, reservation_conflict, unit_mismatch or cost_short.
// code is the catalog code for every status but ok (NULL on ok). available_qty is set only when
// status is insufficient. replayed is true when the shipment line was already issued: the same
// movement and cost are returned and nothing is deducted again. The movement id of an issue is
// m + md5(shipment_id || '|' || shipment_line_id).
//
// Any status other than ok writes nothing. A caller that commits anyway does not change stock.
// p_reservation_id NULL or empty issues unreserved quantity (on_hand - reserved). A reservation id
// consumes that active hold when it matches the balance and its remaining quantity covers the
// issue, decreasing on_hand and reserved by the same quantity. Cost is FIFO, one HALF_UP rounding
// of unit_cost_minor * quantity / price_basis_qty per layer.
//
// The function does not apply the caller's data scope (own, team). It confines rows to
// app.tenant_id and app.company_id. FUL checks visibility before its transaction.
const (
	StockAvailabilityFunction = "brant_stk.stock_availability_issue_v1"
	StockAvailabilitySQL      = `SELECT status, movement_id, total_cost_minor, currency, available_qty, replayed, code
		FROM brant_stk.stock_availability_issue_v1($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`

	// Statuses the function returns.
	AvailabilityOK           = "ok"
	AvailabilityDisabled     = "module_disabled"
	AvailabilityInsufficient = "insufficient"
	AvailabilityConflict     = "reservation_conflict"
	AvailabilityUnitMismatch = "unit_mismatch"
	AvailabilityCostShort    = "cost_short"
)

// StockIssue is one call of StockAvailability v1. ReservationID empty issues unreserved stock.
type StockIssue struct {
	ShipmentID     string
	ShipmentLineID string
	SkuID          string
	LocationID     string
	BatchID        string
	CustodyKind    string
	ReservationID  string
	Quantity       string
	UnitID         string
	UnitRevision   int64
	ActorKind      string
	ActorID        string
	Epoch          int64
	At             time.Time
}

// StockAvailabilityArgs returns the arguments of StockAvailabilitySQL.
func StockAvailabilityArgs(in StockIssue) []any {
	return []any{in.ShipmentID, in.ShipmentLineID, in.SkuID, in.LocationID, in.BatchID, in.CustodyKind,
		in.ReservationID, in.Quantity, in.UnitID, in.UnitRevision, in.ActorKind, in.ActorID, in.Epoch, kernel.Instant(in.At)}
}

// StockAvailabilityVerdict is the function's one row.
type StockAvailabilityVerdict struct {
	Status         string
	MovementID     string
	TotalCostMinor int64
	Currency       string
	AvailableQty   string
	Replayed       bool
	Code           string
	CostKnown      bool // false when total_cost_minor came back NULL
}

// ScanStockAvailability reads the row of StockAvailabilitySQL through scan.
func ScanStockAvailability(scan func(dest ...any) error) (StockAvailabilityVerdict, error) {
	var v StockAvailabilityVerdict
	var movement, currency, available, code *string
	var cost *int64
	if err := scan(&v.Status, &movement, &cost, &currency, &available, &v.Replayed, &code); err != nil {
		return StockAvailabilityVerdict{}, err
	}
	if movement != nil {
		v.MovementID = *movement
	}
	if currency != nil {
		v.Currency = *currency
	}
	if available != nil {
		v.AvailableQty = *available
	}
	if code != nil {
		v.Code = *code
	}
	if cost != nil {
		v.TotalCostMinor, v.CostKnown = *cost, true
	}
	return v, nil
}

// OK reports a successful issue, including an idempotent replay.
func (v StockAvailabilityVerdict) OK() bool { return v.Status == AvailabilityOK }

// AvailabilityQuery asks whether qty of a balance is available, without writing. Quantity empty
// reports the balance only. A disabled company answers Status module_disabled (STK-INV-004).
type AvailabilityQuery struct {
	SkuID       string
	LocationID  string
	BatchID     string
	CustodyKind string
	Quantity    string
}

// Availability is the read-only answer FUL's shipment.prepare and commercial_release.evaluate use.
// They must not treat a disabled company as stock on hand: Status is the signal.
type Availability struct {
	Enabled      bool
	Status       string
	OnHand       string
	Reserved     string
	Available    string
	UnitID       string
	UnitRevision int64
	Currency     string
	MinorUnits   int
}
