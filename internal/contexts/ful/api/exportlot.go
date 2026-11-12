package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Export lots (WBS S5-03b; tactical model FUL-AGG-ExportLot "只追加；批次行引用出运行，确认数量与批次不一致时
// 标记复核"; UL FUL-ExportLot; baseline 22.1, 22.2; SAP reference CN-05; CN-AT02). An export lot is
// what of one shipment goes into one export — the business detail customs declarations, invoices and
// tax refund material refer to — with its quantities and amounts at the order's prices. It is never a
// second stock: its lines name shipment lines, and a shipment line belongs to at most one lot.
//
//	export_lot.register  human or AI (merchandiser): registers lot export_lot_id of the lines of one
//	                     prepared or confirmed shipment, each a quantity of a shipment line no larger
//	                     than the line. The lot is never changed. A lot registered before the shipment
//	                     was confirmed is compared with the confirmation when it is read: a shipment
//	                     cancelled, or confirmed with another quantity of a line than the lot states,
//	                     marks the lot review_required, which CNX's reconciliation takes up (C3a).
//
// Request members in snake_case: export_lot.register {export_lot_id, shipment_id, lines [{line_id,
// shipment_line_id, quantity}]}.
//
// The price of every shipment line is the order line's at the order revision its shipment binds,
// recorded when the shipment was prepared (TRD shows an order only at its current revision). The
// amount of a quantity q of an order line ordered Q is the order line's amount × q / Q, rounded
// HALF_UP to minor units once — net and tax each, payable their sum — so q = Q gives the order
// line's amounts exactly.

// Export lot commands, reads and events (Context Map FUL commands and facts).
const (
	CommandRegisterExportLot = "export_lot.register"
	CommandGetExportLot      = "export_lot.get"

	EventExportLotRegistered = "idp-brant.ful.export_lot_registered.v1"

	// LotRegistered is the state of every lot: a lot is only inserted.
	LotRegistered = "registered"

	// How a lot stands against its shipment, judged when it is read.
	LotAwaitingConfirmation = "awaiting_confirmation"
	LotMatched              = "matched"
	LotReviewRequired       = "review_required"

	MaxLotLines     = 200
	MaxLineLookups  = 200
	MaxLotLookups   = 200
	MaxOrderLookups = 50
)

// ExportLotLine puts Quantity (kernel.QuantityFormat, positive, in the shipment line's unit) of
// shipment line ShipmentLineID into the lot as its line LineID.
type ExportLotLine struct {
	LineID         string
	ShipmentLineID string
	Quantity       string
}

// RegisterExportLot registers lot ExportLotID of lines of shipment ShipmentID. Human or AI.
type RegisterExportLot struct {
	IdempotencyKey string
	ExportLotID    string
	ShipmentID     string
	Lines          []ExportLotLine
}

func (RegisterExportLot) fulCommand() {}

// Validate checks the closed contract: identifiers, 1 to MaxLotLines lines with distinct line IDs
// and distinct shipment lines (DUPLICATE_LINE_ID) and positive quantities.
func (c RegisterExportLot) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ExportLotID):
		return schema("/args/export_lot_id")
	case !kernel.ValidIdentifier(c.ShipmentID):
		return schema("/args/shipment_id")
	case len(c.Lines) < 1 || len(c.Lines) > MaxLotLines:
		return schema("/args/lines")
	}
	ids, lines := map[string]bool{}, map[string]bool{}
	for i, l := range c.Lines {
		p := index("/args/lines", i)
		switch {
		case !kernel.ValidIdentifier(l.LineID):
			return schema(p + "/line_id")
		case ids[l.LineID]:
			return kernel.Reject(kernel.CodeDuplicateLineID, p+"/line_id", "")
		case !kernel.ValidIdentifier(l.ShipmentLineID):
			return schema(p + "/shipment_line_id")
		case lines[l.ShipmentLineID]:
			return kernel.Reject(kernel.CodeDuplicateLineID, p+"/shipment_line_id", "")
		}
		ids[l.LineID], lines[l.ShipmentLineID] = true, true
		if q, ok := Quantity(l.Quantity); !ok || q.Sign() <= 0 {
			return schema(p + "/quantity")
		}
	}
	return nil
}

// CurrencyRef is the order's currency as TRD binds it: the POL currency configuration (its ID is the
// ISO 4217 code) at a revision. The minor units are the currency revision's (POL).
type CurrencyRef struct {
	ID       string
	Revision int64
}

// LinePrice is an order line's price at the order revision a shipment binds, as TRD sealed it: the
// currency, the unit price as TRD shows it (PriceFormat), the order line's quantity and its net, tax
// and payable amounts in minor units for that quantity.
type LinePrice struct {
	Currency          CurrencyRef
	UnitPrice         string
	OrderLineQuantity string
	NetMinor          string
	TaxMinor          string
	PayableMinor      string
}

// Amounts are net, tax and payable amounts in minor units of the order currency.
type Amounts struct {
	NetMinor     string
	TaxMinor     string
	PayableMinor string
}

// ShipmentLineKey names a line of a FUL shipment.
type ShipmentLineKey struct {
	ShipmentID string
	LineID     string
}

// ShipmentLineRef is ShipmentLineRef v1 (Context Map R30 FUL→STL: 已确认出运明细（销售结算来源）; risk 同一
// 出运明细重复结算; guardLock false): a shipment line — its shipment's state, row version and
// confirmation instant, the order binding and customer, the order line with its SKU specification,
// unit and quantity at the bound revision, the quantity shipped, the order line's price at the bound revision and the amounts of the
// quantity at that price, the supplier delivery line, purchase commitment, supplier and quality
// release it ships under, and the export lot that holds it ("" for none). Price is nil, and the
// amounts empty, for a line of a shipment prepared before FUL recorded prices.
//
// Code is "" when the line is a source of sales settlement: its shipment is confirmed (a confirmed
// shipment never changes, SM-FUL-SHIPMENT) and it has a price. Otherwise INVALID_STATE — the
// shipment is prepared or cancelled — or REFERENCE_INVALID — no price. STL settles a line once
// (STL-INV-001); FUL keeps no settlement state.
type ShipmentLineRef struct {
	Key               ShipmentLineKey
	ShipmentStatus    string
	ShipmentVersion   int64
	ConfirmedAt       time.Time
	Order             OrderBinding
	CustomerID        string
	OrderLineID       string
	SkuID             string
	SpecRevision      int64
	UnitID            string
	UnitRevision      int64
	OrderLineQuantity string
	Quantity          string
	Price             *LinePrice
	Amounts           Amounts
	SupplierDelivery  DeliveryLineRef
	CommitmentID      string
	SupplierID        string
	QualityReleaseID  string
	ExportLotID       string
	Code              string
}

// Settleable reports whether the line is a source of sales settlement.
func (r ShipmentLineRef) Settleable() bool { return r.Code == "" }

// Check returns nil for a line that is a source of sales settlement and its refusal at path of the
// caller's request otherwise: INVALID_STATE or REFERENCE_INVALID.
func (r ShipmentLineRef) Check(path string) error {
	switch r.Code {
	case "":
		return nil
	case kernel.CodeInvalidState:
		return kernel.Reject(kernel.CodeInvalidState, path, "The shipment is "+r.ShipmentStatus+", not confirmed")
	}
	return kernel.Reject(r.Code, path, "The shipment line has no price")
}

// ExportLotRefLine is a line of an export lot: the shipment line it takes and that line's order line,
// SKU specification and unit, the lot's quantity, the shipment line's quantity, the quantity its
// shipment confirmed ("" while the shipment is prepared, "0" when it was cancelled), the unit price
// and the lot quantity's amounts.
type ExportLotRefLine struct {
	LineID               string
	ShipmentLineID       string
	OrderLineID          string
	SkuID                string
	SpecRevision         int64
	UnitID               string
	UnitRevision         int64
	Quantity             string
	ShipmentLineQuantity string
	ConfirmedQuantity    string
	UnitPrice            string
	Amounts              Amounts
}

// ExportLotDifference is a line where the confirmation differs from the lot: the lot's quantity and
// the quantity confirmed ("0" for a cancelled shipment).
type ExportLotDifference struct {
	LineID            string
	ShipmentLineID    string
	LotQuantity       string
	ConfirmedQuantity string
}

// ExportLotRef is ExportLotRef v1 (Context Map R36 FUL→CNX: 出口批次与出运单证明细; risk 本地出库被当作海关出口
// 确认; guardLock false), and the read's view of a lot: the lot, its shipment with its state now and
// the state (prepared or confirmed) and row version it had when the lot was registered, the order
// binding, customer and currency, the lines with their amounts and the lot's totals, who registered
// it when, and how the lot stands against its shipment now — awaiting_confirmation while the shipment is prepared,
// matched once it is confirmed with every lot line's quantity, review_required (with the
// differences) when it was cancelled or confirmed with another quantity. A lot is never changed;
// only its standing moves, once, when its shipment ends. An export lot is not a customs export: the
// declaration and its result are CNX's.
type ExportLotRef struct {
	ExportLotID                   string
	Status                        string
	RowVersion                    int64
	ShipmentID                    string
	ShipmentStatus                string
	ShipmentVersion               int64
	ConfirmedAt                   time.Time
	ShipmentStatusAtRegistration  string
	ShipmentVersionAtRegistration int64
	Order                         OrderBinding
	CustomerID                    string
	Currency                      CurrencyRef
	Lines                         []ExportLotRefLine
	Totals                        Amounts
	Reconciliation                string
	Differences                   []ExportLotDifference
	RegisteredBy                  kernel.ActorRef
	RegisteredAt                  time.Time
	AuthorityEpoch                int64
}

// ExportLotView is the read's view of a lot.
type ExportLotView = ExportLotRef

// ExportLotCommands is FUL's command entry point for export lots. Errors are *kernel.Rejection for
// refusals.
type ExportLotCommands interface {
	RegisterExportLot(ctx context.Context, cmd RegisterExportLot) (Receipt, error)
}

// ExportLotQueries is the audited read of export lots.
type ExportLotQueries interface {
	// GetExportLot returns a lot the caller sees, audited as export_lot.get (ful.read): NOT_FOUND
	// otherwise.
	GetExportLot(ctx context.Context, lotID string) (ExportLotView, error)
}

// ShipmentLineLookups answer ShipmentLineRef v1 for STL (R30). Like the other lookups they need no
// permission beyond an active grant; shipments outside the caller's data scope are absent, as are
// unknown shipments and lines. Ask before opening your own transaction: a confirmed shipment line
// never changes, so no guard is published.
type ShipmentLineLookups interface {
	// ResolveShipmentLines answers at most MaxLineLookups keys.
	ResolveShipmentLines(ctx context.Context, keys []ShipmentLineKey) (map[ShipmentLineKey]ShipmentLineRef, error)
	// ResolveOrderShipmentLines answers, for at most MaxOrderLookups orders, the lines of every
	// shipment of the order the caller sees — prepared, confirmed and cancelled — by shipment and
	// line number; an order without such a shipment has an empty list.
	ResolveOrderShipmentLines(ctx context.Context, orderIDs []string) (map[string][]ShipmentLineRef, error)
}

// ExportLotLookups answer ExportLotRef v1 for CNX (R36), under the same rules as
// ShipmentLineLookups: lots outside the caller's data scope are absent.
type ExportLotLookups interface {
	// ResolveExportLots answers at most MaxLotLookups lot IDs.
	ResolveExportLots(ctx context.Context, lotIDs []string) (map[string]ExportLotRef, error)
	// ResolveShipmentExportLots answers, for at most MaxLotLookups shipments, the lots of the
	// shipment the caller sees, by lot ID; a shipment without one has an empty list.
	ResolveShipmentExportLots(ctx context.Context, shipmentIDs []string) (map[string][]ExportLotRef, error)
}

// ValidateShipmentLineKeys checks a lookup's keys.
func ValidateShipmentLineKeys(keys []ShipmentLineKey) error {
	if len(keys) > MaxLineLookups {
		return schema("/keys")
	}
	for i, k := range keys {
		p := index("/keys", i)
		switch {
		case !kernel.ValidIdentifier(k.ShipmentID):
			return schema(p + "/shipment_id")
		case !kernel.ValidIdentifier(k.LineID):
			return schema(p + "/line_id")
		}
	}
	return nil
}

// ValidateIDs checks the IDs of a lookup of at most limit objects.
func ValidateIDs(ids []string, limit int) error {
	if len(ids) > limit {
		return schema("/ids")
	}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) {
			return schema(index("/ids", i))
		}
	}
	return nil
}
