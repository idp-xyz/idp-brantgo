package application

import (
	"context"

	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
)

// STL's ports to the sources of settlement statements (S5-04b), read before STL's transaction:
//
//   - Shipments, to FUL (Context Map R30 FUL→STL, ShipmentLineRef v1, guardLock false: 已确认出运明细
//     （销售结算来源）; risk 同一出运明细重复结算): ResolveShipmentSources answers, for keys {ShipmentID,
//     LineID}, the shipment lines the caller sees. A confirmed shipment line never changes (FUL's
//     triggers), so STL needs no lock on it; STL keeps the settlement state itself (STL-INV-001).
//   - Deliveries, to PUR (R31 PUR→STL, PurchaseSettlementSource v1, guardLock false: 采购承诺与收货或直发
//     交付证据（采购结算来源）; risk 无交付证据形成应付): ResolveDeliverySources answers, for refs
//     {DeliveryID, LineID, Version}, the delivery lines the caller sees, with the commitment line's
//     amounts. A version once verified keeps its quantity; a later verified correction makes the
//     settled version REFERENCE_STALE for a new statement (STL settles (delivery, line) once).
//
// Absent keys are lines the caller is not shown. Check is nil for a line that is a settlement source
// and the upstream's refusal at the caller's path otherwise. adapters/sources implements both on
// FUL's and PUR's api; tests implement them to steer the answers.
type (
	Shipments interface {
		ResolveShipmentSources(ctx context.Context, keys []api.SettlementSourceRef) (map[api.SettlementSourceRef]ShipmentSource, error)
	}
	Deliveries interface {
		ResolveDeliverySources(ctx context.Context, refs []api.SettlementSourceRef) (map[api.SettlementSourceRef]DeliverySource, error)
	}
)

// ShipmentSource is a FUL shipment line as a sales statement takes it, and FUL's verdict.
type ShipmentSource struct {
	Line  settlement.ShipmentLine
	Check func(path string) error
}

// DeliverySource is a PUR supplier delivery line as a purchase statement takes it, and PUR's
// verdict.
type DeliverySource struct {
	Line  settlement.DeliveryLine
	Check func(path string) error
}

// The order-level halves of STL's ports to FUL and PUR, for the order result (S5 unit C9, WBS S5-17), read before
// STL's transaction like their siblings:
//
//   - OrderShipments (FUL, R30 ShipmentLineRef v1): ResolveOrderShipments answers, for orders, the lines of every
//     shipment of the order the caller sees, in any state — the batches of the confirmed result, the shipment
//     lines a closed result needs settled, and the shipments a charge may go to.
//   - OrderPurchases (PUR, R31 "采购承诺与收货或直发交付证据"): ResolveOrderPurchases answers the order's purchase
//     commitment lines with what each buys for (the expected purchase cost) and its verified deliveries (which a
//     closed result needs settled); shown is false when PUR shows the caller nothing of the order.
//
// adapters/sources implements both on FUL's and PUR's api beside Shipments and Deliveries. A port value that does
// not implement them (a stand-in of an earlier unit's tests) answers nothing: the order result then says FUL or
// PUR showed nothing, never that there was nothing.
type (
	OrderShipments interface {
		ResolveOrderShipments(ctx context.Context, orderIDs []string) (map[string][]OrderShipmentLine, error)
	}
	OrderPurchases interface {
		ResolveOrderPurchases(ctx context.Context, orderID string) (lines []OrderPurchaseLine, shown bool, err error)
	}
)

// OrderShipmentLine is a line of a FUL shipment of an order: its state (prepared, confirmed, cancelled), the order
// line, unit and quantity.
type OrderShipmentLine struct {
	ShipmentID  string
	LineID      string
	Status      string
	OrderLineID string
	UnitID      string
	Quantity    string
}

// OrderPurchaseLine is a line of a PUR purchase commitment of an order: the commitment's status, supplier, the
// sealed net amount of the line in minor units of Currency, and its verified deliveries {DeliveryID, LineID (the
// order line), Version}.
type OrderPurchaseLine struct {
	CommitmentID string
	OrderLineID  string
	Status       string
	SupplierID   string
	Currency     string
	MinorUnits   int
	NetMinor     string
	Deliveries   []api.SettlementSourceRef
}
