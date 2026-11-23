// Package api is STK's published contract (inventory and external-factory stock, WBS S6-01;
// STK-AGG-StockLedger, STK-AGG-Reservation, STK-AGG-BatchLineage; STK-INV-001..004; S6-D02).
// Python has no stock context. The company switch defaults to off.
//
//	stk.module.configure   human only, administrator: enables or disables the company.
//	                       expected_version 0 inserts the setting.
//	stock.receive          human only, purchaser: appends a receipt movement and a FIFO cost layer.
//	stock.issue            human only, purchaser: a non-shipment issue. source_kind
//	                       ful_shipment_line is refused, and so is an order reservation.
//	stock.adjust           human only, administrator: adjust_in opens a cost layer, adjust_out
//	                       allocates FIFO. A reason is required. Stock never goes negative.
//	reservation.place      human or AI, merchandiser: holds available quantity.
//	reservation.release    human or AI, merchandiser: gives an active hold back.
//
// RecordHandover is in process for MFG (R42), not an HTTP command. HTTP writing routes refuse an
// AI actor before the body is read; an AI reservation goes through the gateway.
//
// Request members in snake_case. Scope, actor and tenant come from the principal.
package api

import (
	"context"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Source is STK's event source.
const Source = "idp-brant.stk"

// Contract is the envelope contract on POST /api/brant/v1/stk/commands/<command>.
const Contract = "brant.stock/0.1.0"

// Commands, reads and events.
const (
	CommandConfigure = "stk.module.configure"
	CommandReceive   = "stock.receive"
	CommandIssue     = "stock.issue"
	CommandAdjust    = "stock.adjust"
	CommandPlace     = "reservation.place"
	CommandRelease   = "reservation.release"
	CommandHandover  = "material.handover" // in process only (R42); not an HTTP route

	CommandGetSetting     = "stk.setting.get"
	CommandGetBalance     = "stock.balance.get"
	CommandGetReservation = "reservation.get"
	CommandListMovements  = "stock.movement.list"
	CommandAudit          = "stk.audit"
	PermissionRead        = "stk.read"
	PermissionAudit       = "stk.audit"

	EventStockReceived       = "idp-brant.stk.stock_received.v1"
	EventStockIssued         = "idp-brant.stk.stock_issued.v1"
	EventStockAdjusted       = "idp-brant.stk.stock_adjusted.v1"
	EventReservationPlaced   = "idp-brant.stk.reservation_placed.v1"
	EventReservationReleased = "idp-brant.stk.reservation_released.v1"

	CustodyOwnWarehouse            = "own_warehouse"
	CustodyExternalFactoryMaterial = "external_factory_material"
	CustodyExternalFactoryWIP      = "external_factory_wip"
	CustodyInTransit               = "in_transit"

	AdjustIn  = "adjust_in"
	AdjustOut = "adjust_out"

	HandoverTransfer = "handover_transfer"
	HandoverConsume  = "handover_consume"
	HandoverReturn   = "handover_return"
	HandoverScrap    = "handover_scrap"

	SourceCommitment   = "pur_commitment_line"
	SourceHoldForReuse = "pur_hold_for_reuse"
	SourceDelivery     = "supplier_delivery"
	SourceDeliveryLine = "supplier_delivery_line"
	SourceShipmentLine = "ful_shipment_line"

	StatusEnabled  = "enabled"
	StatusDisabled = "disabled"
	StatusReceived = "received"
	StatusIssued   = "issued"
	StatusAdjusted = "adjusted"
	StatusActive   = "active"
	StatusReleased = "released"

	MaxReason = 1000
)

// Receipt is the closed success of a command.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// Reconciliation is what became of a command whose caller lost the answer.
type Reconciliation struct {
	State    kernel.ReconcileState
	ActionID string
	ObjectID string
	Receipt  *Receipt
}

// ConfigureModule sets the company switch. ExpectedVersion 0 creates it. Human only.
type ConfigureModule struct {
	IdempotencyKey  string
	Enabled         bool
	ExpectedVersion int64
}

// LineageLink is one input batch written with a receipt or an issue. Exactly one of OutputBatchID
// or the shipment pair is set.
type LineageLink struct {
	InputBatchID   string
	OutputBatchID  string
	ShipmentID     string
	ShipmentLineID string
	Quantity       string
}

// ReceiveStock receives Quantity into a balance and opens a FIFO cost layer at UnitCostMinor per
// PriceBasisQty. SourceKind empty is an unsolicited receipt. Human only.
type ReceiveStock struct {
	IdempotencyKey string
	MovementID     string
	SkuID          string
	LocationID     string
	BatchID        string
	CustodyKind    string
	Quantity       string
	UnitID         string
	UnitRevision   int64
	Currency       string
	MinorUnits     int
	UnitCostMinor  string
	PriceBasisQty  string
	SourceKind     string
	SourceID       string
	SourceLineID   string
	Reason         string
	ReceivedAt     string
	Lineage        []LineageLink
}

// IssueStock issues Quantity that is not a shipment. ReservationID, when set, must be an active
// internal hold on the same balance. Human only.
type IssueStock struct {
	IdempotencyKey string
	MovementID     string
	SkuID          string
	LocationID     string
	BatchID        string
	CustodyKind    string
	Quantity       string
	UnitID         string
	UnitRevision   int64
	ReservationID  string
	SourceKind     string
	SourceID       string
	SourceLineID   string
	Reason         string
	IssuedAt       string
}

// AdjustStock adjusts one balance up (AdjustIn, with a cost layer) or down (AdjustOut, FIFO).
// Reason is required. Human only.
type AdjustStock struct {
	IdempotencyKey string
	MovementID     string
	Direction      string
	SkuID          string
	LocationID     string
	BatchID        string
	CustodyKind    string
	Quantity       string
	UnitID         string
	UnitRevision   int64
	Currency       string
	MinorUnits     int
	UnitCostMinor  string
	PriceBasisQty  string
	Reason         string
	AdjustedAt     string
}

// PlaceReservation holds Quantity of a balance for an order line, or, with OrderID empty, as an
// internal hold. Human or AI.
type PlaceReservation struct {
	IdempotencyKey string
	ReservationID  string
	SkuID          string
	LocationID     string
	BatchID        string
	CustodyKind    string
	Quantity       string
	UnitID         string
	UnitRevision   int64
	OrderID        string
	OrderLineID    string
}

// ReleaseReservation gives an active hold back. Human or AI.
type ReleaseReservation struct {
	IdempotencyKey  string
	ReservationID   string
	ExpectedVersion int64
}

// RecordHandover moves, consumes, returns or scraps material (R42). It is not an HTTP command.
// CounterMovementID is the inbound movement of a transfer or a return.
type RecordHandover struct {
	IdempotencyKey    string
	MovementID        string
	CounterMovementID string
	Kind              string
	SkuID             string
	LocationID        string
	BatchID           string
	CustodyKind       string
	ToLocationID      string
	ToBatchID         string
	ToCustodyKind     string
	Quantity          string
	UnitID            string
	UnitRevision      int64
	Reason            string
}

// Command is one of STK's commands.
type Command interface{ stkCommand() }

func (ConfigureModule) stkCommand()    {}
func (ReceiveStock) stkCommand()       {}
func (IssueStock) stkCommand()         {}
func (AdjustStock) stkCommand()        {}
func (PlaceReservation) stkCommand()   {}
func (ReleaseReservation) stkCommand() {}
func (RecordHandover) stkCommand()     {}

// Commands is STK's command entry point. RecordHandover is in process only.
type Commands interface {
	ConfigureModule(ctx context.Context, cmd ConfigureModule) (Receipt, error)
	ReceiveStock(ctx context.Context, cmd ReceiveStock) (Receipt, error)
	IssueStock(ctx context.Context, cmd IssueStock) (Receipt, error)
	AdjustStock(ctx context.Context, cmd AdjustStock) (Receipt, error)
	PlaceReservation(ctx context.Context, cmd PlaceReservation) (Receipt, error)
	ReleaseReservation(ctx context.Context, cmd ReleaseReservation) (Receipt, error)
	RecordHandover(ctx context.Context, cmd RecordHandover) (Receipt, error)
}

// SettingView is the company switch.
type SettingView struct {
	Enabled    bool
	RowVersion int64
	UpdatedAt  string
}

// BalanceKey names one balance.
type BalanceKey struct {
	SkuID       string
	LocationID  string
	BatchID     string
	CustodyKind string
}

// BalanceView is one balance. Available is on hand less reserved.
type BalanceView struct {
	SkuID        string
	LocationID   string
	BatchID      string
	CustodyKind  string
	OnHand       string
	Reserved     string
	Available    string
	UnitID       string
	UnitRevision int64
	Currency     string
	MinorUnits   int
	RowVersion   int64
}

// ReservationView is one hold.
type ReservationView struct {
	ReservationID string
	SkuID         string
	LocationID    string
	BatchID       string
	CustodyKind   string
	OrderID       string
	OrderLineID   string
	Quantity      string
	Remaining     string
	Status        string
	UnitID        string
	UnitRevision  int64
	RowVersion    int64
}

// MovementView is one append-only movement.
type MovementView struct {
	MovementID   string
	Kind         string
	SkuID        string
	LocationID   string
	BatchID      string
	CustodyKind  string
	Quantity     string
	UnitID       string
	UnitRevision int64
	SourceKind   string
	SourceID     string
	SourceLineID string
	Reason       string
	At           string
}

// Queries are the in-process reads. They are not HTTP routes.
type Queries interface {
	GetSetting(ctx context.Context) (SettingView, error)
	GetBalance(ctx context.Context, key BalanceKey) (BalanceView, error)
	GetReservation(ctx context.Context, id string) (ReservationView, error)
	ListMovements(ctx context.Context, key BalanceKey) ([]MovementView, error)
	ResolveAvailability(ctx context.Context, q AvailabilityQuery) (Availability, error)
	ResolveShipmentStockCost(ctx context.Context, shipmentIDs []string) ([]ShipmentCost, error)
}

// Auditor is the audit trail.
type Auditor interface {
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// Reconciler answers what became of a command without executing it.
type Reconciler interface {
	Reconcile(ctx context.Context, cmd Command) (Reconciliation, error)
}
