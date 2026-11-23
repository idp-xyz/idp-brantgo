// Package ledger is the stock ledger's quantity and FIFO cost rules (STK-AGG-StockLedger,
// STK-INV-001, STK-INV-002, STK-INV-003; S6-D02). It imports the kernel only. A balance never goes
// negative, a reservation is not counted twice, and a cost allocation rounds once, HALF_UP, in
// integer minor units.
package ledger

import (
	"errors"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Custody of a balance (UL STK-StockMovement, S6-D02).
const (
	CustodyOwnWarehouse            = "own_warehouse"
	CustodyExternalFactoryMaterial = "external_factory_material"
	CustodyExternalFactoryWIP      = "external_factory_wip"
	CustodyInTransit               = "in_transit"
)

// Movement kinds. shipment_issue is written only by StockAvailability v1. The handover kinds stay
// distinct (MFG-INV-002, R42).
const (
	KindReceive          = "receive"
	KindIssue            = "issue"
	KindAdjustIn         = "adjust_in"
	KindAdjustOut        = "adjust_out"
	KindHandoverTransfer = "handover_transfer"
	KindHandoverConsume  = "handover_consume"
	KindHandoverReturn   = "handover_return"
	KindHandoverScrap    = "handover_scrap"
	KindShipmentIssue    = "shipment_issue"
)

// Sources a receipt or an issue may cite. A supplier delivery is never a warehouse receipt
// (STK-INV-002). A shipment line is issued only by StockAvailability v1.
const (
	SourceCommitment       = "pur_commitment_line"
	SourceHoldForReuse     = "pur_hold_for_reuse"
	SourceSupplierDelivery = "supplier_delivery"
	SourceDeliveryLine     = "supplier_delivery_line"
	SourceShipmentLine     = "ful_shipment_line"
	SourceHandover         = "mfg_handover"
)

// Sentinel errors the application maps onto catalog codes.
var (
	ErrShort     = errors.New("stk: available quantity is short")
	ErrLayers    = errors.New("stk: cost layers do not cover the quantity")
	ErrUnit      = errors.New("stk: unit does not match the balance")
	ErrCurrency  = errors.New("stk: currency does not match the balance")
	ErrDirect    = errors.New("stk: a direct-ship delivery is not a warehouse receipt")
	ErrShipment  = errors.New("stk: a shipment line is issued only by StockAvailability")
	ErrHold      = errors.New("stk: no approved hold_for_reuse covers the receipt")
	ErrSource    = errors.New("stk: source kind is not a stock source")
	ErrOrderHold = errors.New("stk: an order reservation is consumed only by StockAvailability")
)

// Custodies are the custody kinds a balance may have.
func Custodies() []string {
	return []string{CustodyOwnWarehouse, CustodyExternalFactoryMaterial, CustodyExternalFactoryWIP, CustodyInTransit}
}

// Kinds are the movement kinds.
func Kinds() []string {
	return []string{KindReceive, KindIssue, KindAdjustIn, KindAdjustOut, KindHandoverTransfer, KindHandoverConsume,
		KindHandoverReturn, KindHandoverScrap, KindShipmentIssue}
}

// Key identifies one balance: item, location, batch and custody inside a tenant and company.
type Key struct {
	SKU      string
	Location string
	Batch    string
	Custody  string
}

// Less reports whether k sorts before o. Several balances are locked in this order.
func (k Key) Less(o Key) bool {
	if k.SKU != o.SKU {
		return k.SKU < o.SKU
	}
	if k.Location != o.Location {
		return k.Location < o.Location
	}
	if k.Batch != o.Batch {
		return k.Batch < o.Batch
	}
	return k.Custody < o.Custody
}

// Same reports whether k and o are the same balance.
func (k Key) Same(o Key) bool {
	return k.SKU == o.SKU && k.Location == o.Location && k.Batch == o.Batch && k.Custody == o.Custody
}

// Balance is the on-hand and reserved quantity of one key. Available is on hand less reserved.
type Balance struct {
	Key
	OnHand       kernel.Decimal
	Reserved     kernel.Decimal
	UnitID       string
	UnitRevision int64
	Currency     string
	MinorUnits   int
	RowVersion   int64
	OwnerActor   string
	OwnerTeam    string
}

// Available is on hand less what active reservations hold.
func (b Balance) Available() kernel.Decimal { return b.OnHand.Sub(b.Reserved) }

// Layer is one FIFO cost layer. Remaining falls as issues allocate it; nothing else about it changes.
type Layer struct {
	ID            string
	MovementID    string
	Key           Key
	Currency      string
	MinorUnits    int
	UnitCostMinor int64
	Basis         kernel.Decimal
	Original      kernel.Decimal
	Remaining     kernel.Decimal
	ReceivedAt    time.Time
}

// Take is the quantity and minor-unit amount one layer contributes to an issue.
type Take struct {
	LayerID     string
	Quantity    kernel.Decimal
	AmountMinor int64
}

// Available reports whether qty can leave the balance without touching reserved stock and without
// going negative (STK-INV-001).
func Available(onHand, reserved, qty kernel.Decimal) bool {
	return qty.Sign() > 0 && onHand.Sub(reserved).Cmp(qty) >= 0 && reserved.Sign() >= 0 && onHand.Cmp(reserved) >= 0
}

// Receive increases on hand. The unit and currency of an existing balance must be the ones received.
func Receive(b *Balance, qty kernel.Decimal, unit string, revision int64, currency string, minor int) error {
	if b.RowVersion > 0 {
		if b.UnitID != unit || b.UnitRevision != revision {
			return ErrUnit
		}
		if b.Currency != currency || b.MinorUnits != minor {
			return ErrCurrency
		}
	}
	b.OnHand = b.OnHand.Add(qty)
	return nil
}

// UnreservedIssue decreases on hand when available covers qty. Reserved is unchanged.
func UnreservedIssue(onHand, reserved, qty kernel.Decimal) (kernel.Decimal, error) {
	if !Available(onHand, reserved, qty) {
		return kernel.Decimal{}, ErrShort
	}
	return onHand.Sub(qty), nil
}

// ReservedIssue decreases on hand and reserved together by qty, so the reservation is not also
// subtracted from available (STK-INV-001). resRemaining is what the reservation still holds.
func ReservedIssue(onHand, reserved, resRemaining, qty kernel.Decimal) (newOn, newReserved, newRemaining kernel.Decimal, err error) {
	if qty.Sign() <= 0 || resRemaining.Cmp(qty) < 0 || reserved.Cmp(qty) < 0 || onHand.Cmp(qty) < 0 {
		return kernel.Decimal{}, kernel.Decimal{}, kernel.Decimal{}, ErrShort
	}
	return onHand.Sub(qty), reserved.Sub(qty), resRemaining.Sub(qty), nil
}

// Place increases reserved when available covers qty.
func Place(onHand, reserved, qty kernel.Decimal) (kernel.Decimal, error) {
	if !Available(onHand, reserved, qty) {
		return kernel.Decimal{}, ErrShort
	}
	return reserved.Add(qty), nil
}

// Amount is the minor-unit cost of qty on layer, rounded once HALF_UP (STK-INV-003).
// It is unit_cost_minor × qty / basis. The currency's minor-unit scale cancels, so the integer
// matches PostgreSQL round(unit_cost_minor * qty / basis, 0) for a non-negative amount.
func Amount(layer Layer, qty kernel.Decimal) (int64, error) {
	if layer.Basis.Sign() <= 0 || qty.Sign() < 0 {
		return 0, ErrLayers
	}
	if qty.Sign() == 0 {
		return 0, nil
	}
	factor, err := qty.Quo(layer.Basis)
	if err != nil {
		return 0, err
	}
	cur, err := kernel.NewCurrency(layer.Currency, layer.MinorUnits)
	if err != nil {
		return 0, err
	}
	money, err := kernel.NewMoney(cur, layer.UnitCostMinor)
	if err != nil {
		return 0, err
	}
	out, err := money.MulHalfUp(factor)
	if err != nil {
		return 0, err
	}
	if !out.Minor().IsInt64() {
		return 0, kernel.ErrAmountOutOfRange
	}
	return out.Minor().Int64(), nil
}

// AllocateFIFO takes need from layers, oldest received_at then layer id first (STK-INV-003).
// It does not change layers. ErrLayers means the remaining quantities do not cover need.
// ErrCurrency means the layers that were used do not share one currency.
func AllocateFIFO(layers []Layer, need kernel.Decimal) ([]Take, error) {
	if need.Sign() <= 0 {
		return nil, ErrLayers
	}
	sorted := append([]Layer(nil), layers...)
	slices.SortFunc(sorted, func(a, b Layer) int {
		if c := a.ReceivedAt.Compare(b.ReceivedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	left := need
	var out []Take
	var currency string
	var minor int
	for _, layer := range sorted {
		if left.Sign() == 0 {
			break
		}
		if layer.Remaining.Sign() <= 0 {
			continue
		}
		if currency == "" {
			currency, minor = layer.Currency, layer.MinorUnits
		} else if layer.Currency != currency || layer.MinorUnits != minor {
			return nil, ErrCurrency
		}
		take := layer.Remaining
		if take.Cmp(left) > 0 {
			take = left
		}
		amount, err := Amount(layer, take)
		if err != nil {
			return nil, err
		}
		out = append(out, Take{LayerID: layer.ID, Quantity: take, AmountMinor: amount})
		left = left.Sub(take)
	}
	if left.Sign() > 0 {
		return nil, ErrLayers
	}
	return out, nil
}

// TotalMinor sums the minor units of takes.
func TotalMinor(takes []Take) int64 {
	var n int64
	for _, t := range takes {
		n += t.AmountMinor
	}
	return n
}

// ReceiptSource reports why kind cannot be a warehouse receipt. A direct-ship commitment line and
// a supplier delivery are refused (STK-INV-002). hold is an approved hold_for_reuse on the line.
// An empty result accepts the source. An empty kind is an unsolicited receipt.
func ReceiptSource(kind string, directShip, hold bool) error {
	switch kind {
	case "", "manual":
		return nil
	case SourceSupplierDelivery, SourceDeliveryLine:
		return ErrDirect
	case SourceShipmentLine:
		return ErrShipment
	case SourceCommitment:
		if directShip {
			return ErrDirect
		}
		return nil
	case SourceHoldForReuse:
		if !hold {
			return ErrHold
		}
		return nil
	default:
		return ErrSource
	}
}

// IssueSource reports whether a person may cite kind on stock.issue. A shipment line is refused
// (STK-INV-002): shipment.confirm is the only issuer of that line.
func IssueSource(kind string) error {
	if kind == SourceShipmentLine {
		return ErrShipment
	}
	return nil
}

// IssueReservation reports whether stock.issue may consume a reservation that names orderID.
// An order reservation belongs to StockAvailability v1 (S6-D02). An empty order id is internal.
func IssueReservation(orderID string) error {
	if orderID != "" {
		return ErrOrderHold
	}
	return nil
}

// HandoverMoves reports the direction of kind: stock leaves the source, and for a transfer or a
// return it arrives at the destination. The four kinds are not interchangeable (MFG-INV-002).
func HandoverMoves(kind string) (out, in bool, err error) {
	switch kind {
	case KindHandoverConsume, KindHandoverScrap:
		return true, false, nil
	case KindHandoverTransfer, KindHandoverReturn:
		return true, true, nil
	default:
		return false, false, ErrSource
	}
}
