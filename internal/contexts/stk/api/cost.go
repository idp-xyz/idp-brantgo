package api

// CostLayerRef v1 (Context Map R41 STK→STL, guardLock false; STK-INV-003). STL reads it when an
// order result values a warehouse shipment. Direct-ship shipments stay valued from purchase
// statements. STL does not keep a second stock balance. The read is in process, before or during
// STL's own transaction, and it does not lock STK rows.
//
// ResolveShipmentStockCost returns one ShipmentCost per shipment line that StockAvailability v1
// (or, never, a person) issued for the shipment ids. Lines with no issue are absent. Amounts are
// the stored cost_allocation minor units, already rounded once HALF_UP. A disabled company still
// returns issues that were posted while it was enabled; it does not invent any.

// ShipmentCost is the FIFO cost of one shipment line.
type ShipmentCost struct {
	ShipmentID     string
	ShipmentLineID string
	MovementID     string
	Currency       string
	TotalCostMinor int64
	Allocations    []CostAllocation
}

// CostAllocation is one layer's contribution to a shipment line.
type CostAllocation struct {
	LayerID     string
	Quantity    string
	AmountMinor int64
	Currency    string
}
