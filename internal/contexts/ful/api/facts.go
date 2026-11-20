package api

import (
	"context"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// ShipmentFacts v1 (Context Map R70 FUL→TRD, "OHS（事务内无锁读取）", guardLock false: 已确认出运量（按订单行
// 跨修订累计）; risk 先读后写使已提交出运被漏看; S5 decision D-1). TRD's order end (order.cancel,
// order.close_remainder, order.close) and order.amendment.apply read what an order shipped inside
// their own transaction, without locks, after they locked the order row FOR UPDATE. That read is
// consistent because shipment.confirm — the only command that adds to it — holds the same order row
// FOR SHARE through OrderGuard v1 until it commits: a confirmation either committed before TRD got
// its lock, and the read sees it, or waits for TRD and then meets the order as TRD left it.
//
// It is a SECURITY DEFINER function in FUL's schema with a fixed search_path
// (migrations/ful/000001_ful_shipments.sql):
//
//	brant_ful.order_shipped_v1(p_order_id text)
//	    RETURNS TABLE (order_line_id text, unit_id text, unit_revision bigint, shipped_quantity text, shipments bigint)
//
// It returns one row per order line and unit that has a confirmed shipment line in the caller's
// scope (app.tenant_id and app.company_id), ordered by order line, and no row for an order nothing
// shipped of: shipped_quantity is the sum of the confirmed shipment lines of the order line over
// every order revision, in the line's unit (QuantityFormat text), shipments the number of confirmed
// shipments it came from. Prepared and cancelled shipments never count. The function takes no lock
// and applies no data scope. A caller:
//
//	rows, err := exec.Query(ctx, fulapi.OrderShippedSQL, fulapi.OrderShippedArgs(orderID)...)
//	for rows.Next() {
//		line, err := fulapi.ScanOrderShipped(rows.Scan)
//		...
//	}
const (
	OrderShippedFunction = "brant_ful.order_shipped_v1"
	OrderShippedSQL      = `SELECT order_line_id, unit_id, unit_revision, shipped_quantity, shipments FROM brant_ful.order_shipped_v1($1)`
)

// OrderShippedArgs returns the arguments of OrderShippedSQL for order orderID.
func OrderShippedArgs(orderID string) []any { return []any{orderID} }

// ShippedLine is one row of ShipmentFacts v1: what the confirmed shipments shipped of an order line,
// in the unit named, and how many confirmed shipments that came from.
type ShippedLine struct {
	OrderLineID  string
	UnitID       string
	UnitRevision int64
	Quantity     string
	Shipments    int64
}

// ScanOrderShipped reads one row of OrderShippedSQL through scan, such as a pgx row's Scan.
func ScanOrderShipped(scan func(dest ...any) error) (ShippedLine, error) {
	var out ShippedLine
	if err := scan(&out.OrderLineID, &out.UnitID, &out.UnitRevision, &out.Quantity, &out.Shipments); err != nil {
		return ShippedLine{}, err
	}
	return out, nil
}

// ShipmentFacts answers ShipmentFacts v1 before the caller's own transaction, in a transaction of
// FUL's: what TRD shows of an order's shipments while it prepares an amendment (WF-06). It needs no
// permission beyond an active grant and applies no data scope: the caller decided before whether
// its actor sees the order. Inside the caller's transaction, after its order lock, use
// OrderShippedSQL instead: only that read is safe against a confirmation in flight.
type ShipmentFacts interface {
	// ResolveOrderShipped answers at most MaxShippedLookups order IDs; an order nothing shipped of
	// has an empty list.
	ResolveOrderShipped(ctx context.Context, orderIDs []string) (map[string][]ShippedLine, error)
}

// ValidateOrderIDs checks a lookup's order IDs.
func ValidateOrderIDs(ids []string) error {
	if len(ids) > MaxShippedLookups {
		return schema("/order_ids")
	}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) {
			return schema(index("/order_ids", i))
		}
	}
	return nil
}

// ReleaseConsumption v1 (Context Map R79 FUL→QLT, "OHS（事务内无锁读取）", guardLock false: 按质量放行
// ID 的已出运量; risk 撤回记录的冻结量与实际出运不符; decision S5R-D18). QLT's quality_release.revoke
// reads what FUL shipped under the release it revokes inside its own transaction, without locks,
// after it locked the release row FOR UPDATE, and records the frozen quantity (QLT-INV-002). That
// read is consistent because shipment.confirm — the only command that adds to it — holds the same
// release row FOR SHARE through QLT's QualityReleaseGuard v1 until it commits: a confirmation either
// committed before QLT got its lock, and the read sees it, or waits for QLT and then meets the release
// revoked.
//
// It is a SECURITY DEFINER function in FUL's schema with a fixed search_path
// (migrations/ful/000008_ful_release_consumption.sql):
//
//	brant_ful.release_consumption_v1(p_release_id text)
//	    RETURNS TABLE (unit_id text, unit_revision bigint, shipped_quantity text, shipments bigint)
//
// It returns one row per unit of the confirmed shipment lines that name the release in the caller's
// scope (app.tenant_id and app.company_id), ordered by unit, and no row when nothing shipped under
// it: shipped_quantity is the sum of those lines (QuantityFormat text), shipments the number of
// confirmed shipments they came from. Prepared and cancelled shipments never count. FUL keeps every
// line of a release in the release's unit, so a caller sees at most one row. The function takes no
// lock and applies no data scope. A caller, after its lock:
//
//	rows, err := exec.Query(ctx, fulapi.ReleaseConsumptionSQL, fulapi.ReleaseConsumptionArgs(releaseID)...)
//	for rows.Next() {
//		c, err := fulapi.ScanReleaseConsumption(rows.Scan)
//		...
//	}
const (
	ReleaseConsumptionFunction = "brant_ful.release_consumption_v1"
	ReleaseConsumptionSQL      = `SELECT unit_id, unit_revision, shipped_quantity, shipments FROM brant_ful.release_consumption_v1($1)`
)

// ReleaseConsumptionArgs returns the arguments of ReleaseConsumptionSQL for quality release
// releaseID.
func ReleaseConsumptionArgs(releaseID string) []any { return []any{releaseID} }

// ReleaseShipped is one row of ReleaseConsumption v1: what the confirmed shipments shipped under a
// quality release, in the unit named, and how many confirmed shipments that came from.
type ReleaseShipped struct {
	UnitID       string
	UnitRevision int64
	Quantity     string
	Shipments    int64
}

// ScanReleaseConsumption reads one row of ReleaseConsumptionSQL through scan, such as a pgx row's
// Scan.
func ScanReleaseConsumption(scan func(dest ...any) error) (ReleaseShipped, error) {
	var out ReleaseShipped
	if err := scan(&out.UnitID, &out.UnitRevision, &out.Quantity, &out.Shipments); err != nil {
		return ReleaseShipped{}, err
	}
	return out, nil
}
