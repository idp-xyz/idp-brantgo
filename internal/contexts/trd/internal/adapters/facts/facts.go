// Package facts implements the two reverse reads of TRD's active order commands
// (application.OrderFacts; S5 unit C4a), on the contracts their owners publish:
//
//   - FUL's ShipmentFacts v1 (Context Map R70 FUL→TRD, "OHS（事务内无锁读取）", guardLock false):
//     brant_ful.order_shipped_v1, what the confirmed shipments shipped of each order line;
//   - PUR's DownstreamCommitments v1 (R71 PUR→TRD, guardLock false): brant_pur.order_commitments_v1,
//     the prepared drafts, live commitments and demand allocations of the order.
//
// Both are SECURITY DEFINER functions run with TRD's own executor inside TRD's transaction, after
// the order row is locked FOR UPDATE; neither takes a lock (S5 decision D-1). TRD never calls FUL's
// or PUR's transaction owner and needs neither module to be constructed (D-2: FUL and PUR are
// composed after TRD). What the answers mean for an order end or an amendment is TRD's domain's.
package facts

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/application"
)

var (
	_ application.ShipmentFacts         = FULShipments
	_ application.DownstreamCommitments = PURCommitments
)

// FULShipments reads FUL's ShipmentFacts v1 of order orderID with exec: one answer per order line
// and unit that has a confirmed shipment line, none for an order nothing shipped of.
func FULShipments(ctx context.Context, exec bentopg.Executor, orderID string) ([]application.ShippedLine, error) {
	rows, err := exec.Query(ctx, fulapi.OrderShippedSQL, fulapi.OrderShippedArgs(orderID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []application.ShippedLine
	for rows.Next() {
		l, err := fulapi.ScanOrderShipped(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, application.ShippedLine{LineID: l.OrderLineID, UnitID: l.UnitID, UnitRevision: l.UnitRevision, Quantity: l.Quantity,
			Shipments: l.Shipments})
	}
	return out, rows.Err()
}

// PURCommitments reads PUR's DownstreamCommitments v1 of order orderID with exec: one answer per
// open object, none when nothing is open.
func PURCommitments(ctx context.Context, exec bentopg.Executor, orderID string) ([]application.DownstreamItem, error) {
	rows, err := exec.Query(ctx, purapi.DownstreamCommitmentsSQL, purapi.DownstreamCommitmentsArgs(orderID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []application.DownstreamItem
	for rows.Next() {
		c, err := purapi.ScanDownstreamCommitment(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, application.DownstreamItem{Kind: c.Kind, ObjectID: c.ObjectID, DraftID: c.DraftID, OrderLineID: c.OrderLineID,
			OrderRevision: c.OrderRevision, State: c.State, Quantity: c.Quantity, Disposition: c.Disposition})
	}
	return out, rows.Err()
}
