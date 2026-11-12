// Package exportlot holds the rules of FUL's ExportLot aggregate (tactical model FUL-AGG-ExportLot
// "只追加；批次行引用出运行，确认数量与批次不一致时标记复核"; UL FUL-ExportLot "不复制第二份库存"; baseline 22.1,
// 22.2; CN-AT02; WBS S5-03b): what of one shipment goes into one export, with quantities and amounts
// at the order's prices. A lot is only inserted; how it stands against its shipment's confirmation is
// judged when it is read. The package knows no database.
package exportlot

import (
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The state of a lot and how it stands against its shipment (as in package api).
const (
	StatusRegistered = "registered"

	AwaitingConfirmation = "awaiting_confirmation"
	Matched              = "matched"
	ReviewRequired       = "review_required"
)

// Act is who did something, under which authority epoch, when.
type Act = shipment.Act

// Line is a line of a lot: Quantity of shipment line ShipmentLineID, which ships ShipmentLineQuantity
// of order line OrderLineID (ordered OrderLineQuantity at the bound revision) in the order line's
// unit; its unit price and the amounts of Quantity at the order line's price.
type Line struct {
	ID                   string
	ShipmentLineID       string
	OrderLineID          string
	SkuID                string
	SpecRevision         int64
	UnitID               string
	UnitRevision         int64
	Quantity             kernel.Decimal
	ShipmentLineQuantity kernel.Decimal
	OrderLineQuantity    kernel.Decimal
	UnitPrice            kernel.Decimal
	Amounts              shipment.Amounts
}

// Lot is the aggregate root: registered once at row version 1 and never changed. ShipmentStatus and
// ShipmentVersion are the shipment's when the lot was registered.
type Lot struct {
	ID               string
	Status           string
	RowVersion       int64
	ShipmentID       string
	ShipmentStatus   string
	ShipmentVersion  int64
	Order            shipment.OrderBinding
	CustomerID       string
	CurrencyID       string
	CurrencyRevision int64
	Lines            []Line
	Totals           shipment.Amounts
	OwnerActor       string
	OwnerTeam        string
	RequestHash      string
	Registered       Act
}

// RequestLine puts Quantity of shipment line ShipmentLineID into the lot as line ID.
type RequestLine struct {
	ID             string
	ShipmentLineID string
	Quantity       kernel.Decimal
}

// Request is a registration: the lot's ID, the shipment, the lines and the request's hash.
type Request struct {
	ID          string
	ShipmentID  string
	Lines       []RequestLine
	RequestHash string
}

// Inputs are what a registration is judged on: the shipment as the registrant sees it (nil: none),
// the prices of its lines by line ID, the lots that already hold its lines (shipment line ID → lot
// ID), who registers when and the registrant's team.
type Inputs struct {
	Shipment *shipment.Shipment
	Prices   map[string]shipment.Price
	Taken    map[string]string
	Act      Act
	Team     string
}

func linePath(i int, member string) string {
	return "/args/lines/" + strconv.Itoa(i) + "/" + member
}

// Register decides a lot. It refuses, in this order: a shipment the registrant does not see
// (REFERENCE_INVALID); a cancelled shipment (INVALID_STATE: nothing of it is exported); then line by
// line a shipment line the shipment does not have (REFERENCE_INVALID), one another lot holds
// (BUSINESS_KEY_CONFLICT: a shipment line goes into one export lot, never a second stock), one without
// a price (REFERENCE_INVALID: prepared before FUL kept prices) and a quantity above the shipment
// line's (INVALID_KNOWN_VALUE).
func Register(req Request, in Inputs) (*Lot, error) {
	s := in.Shipment
	if s == nil {
		return nil, shipment.Reject(kernel.CodeReferenceInvalid, "/args/shipment_id", "No shipment the actor reads")
	}
	if s.Status == shipment.StatusCancelled {
		return nil, shipment.Reject(kernel.CodeInvalidState, "/args/shipment_id", "The shipment is cancelled")
	}
	act := in.Act
	act.At = kernel.Instant(act.At)
	lot := &Lot{ID: req.ID, Status: StatusRegistered, RowVersion: 1, ShipmentID: s.ID, ShipmentStatus: s.Status, ShipmentVersion: s.RowVersion,
		Order: s.Order, CustomerID: s.CustomerID, OwnerActor: act.By.ID, OwnerTeam: in.Team, RequestHash: req.RequestHash, Registered: act}
	for i, r := range req.Lines {
		sl, _, ok := s.Line(r.ShipmentLineID)
		if !ok {
			return nil, shipment.Reject(kernel.CodeReferenceInvalid, linePath(i, "shipment_line_id"), "The shipment has no such line")
		}
		if other, taken := in.Taken[sl.ID]; taken {
			return nil, shipment.Reject(shipment.CodeBusinessKeyConflict, linePath(i, "shipment_line_id"), "Export lot "+other+" holds this shipment line")
		}
		p, ok := in.Prices[sl.ID]
		if !ok {
			return nil, shipment.Reject(kernel.CodeReferenceInvalid, linePath(i, "shipment_line_id"), "The shipment line has no price")
		}
		if r.Quantity.Cmp(sl.Quantity) > 0 {
			return nil, shipment.Reject(kernel.CodeInvalidKnownValue, linePath(i, "quantity"), "The lot takes more than the shipment line ships")
		}
		lot.CurrencyID, lot.CurrencyRevision = p.CurrencyID, p.CurrencyRevision
		amounts := p.Of(r.Quantity, sl.OrderQuantity)
		lot.Lines = append(lot.Lines, Line{ID: r.ID, ShipmentLineID: sl.ID, OrderLineID: sl.OrderLineID, SkuID: sl.SkuID, SpecRevision: sl.SpecRevision,
			UnitID: sl.UnitID, UnitRevision: sl.UnitRevision, Quantity: r.Quantity, ShipmentLineQuantity: sl.Quantity, OrderLineQuantity: sl.OrderQuantity,
			UnitPrice: p.UnitPrice, Amounts: amounts})
		lot.Totals = lot.Totals.Add(amounts)
	}
	if len(lot.Lines) == 0 {
		return nil, shipment.Reject(kernel.CodeSchemaInvalid, "/args/lines", "")
	}
	return lot, nil
}

// Answer decides what a registration gets whose lot ID is taken: nil when the stored lot is the same
// request by the same registrant (the command is answered from it), and BUSINESS_KEY_CONFLICT
// otherwise.
func (l *Lot) Answer(requestHash string, by kernel.ActorRef) error {
	if l.RequestHash != requestHash || l.Registered.By != by {
		return shipment.Reject(shipment.CodeBusinessKeyConflict, "/args/export_lot_id", "The export lot ID holds another lot")
	}
	return nil
}

// Difference is a lot line whose quantity the confirmation does not confirm: the lot's quantity and
// the confirmed one (zero for a cancelled shipment).
type Difference struct {
	LineID         string
	ShipmentLineID string
	Lot            kernel.Decimal
	Confirmed      kernel.Decimal
}

// Standing is how a lot stands against its shipment now: State, the differences, and the confirmed
// quantity of each lot line by line ID (absent while the shipment is prepared).
type Standing struct {
	State       string
	Differences []Difference
	Confirmed   map[string]kernel.Decimal
	ConfirmedAt time.Time
}

// Against judges the lot against its shipment as it is now (s, never nil: a lot's shipment is never
// deleted). While the shipment is prepared the lot awaits its confirmation. A confirmed shipment
// confirms each shipment line's quantity, a cancelled one nothing; a lot line whose quantity differs
// from what was confirmed marks the lot review_required (CNX reconciles it), otherwise it is matched.
func (l *Lot) Against(s *shipment.Shipment) Standing {
	if s.Status == shipment.StatusPrepared {
		return Standing{State: AwaitingConfirmation}
	}
	out := Standing{State: Matched, Confirmed: map[string]kernel.Decimal{}}
	if s.Confirmation != nil {
		out.ConfirmedAt = s.Confirmation.Confirmed.At
	}
	for _, x := range l.Lines {
		var confirmed kernel.Decimal
		if sl, _, ok := s.Line(x.ShipmentLineID); ok && s.Status == shipment.StatusConfirmed {
			confirmed = sl.Quantity
		}
		out.Confirmed[x.ID] = confirmed
		if !confirmed.Equal(x.Quantity) {
			out.State = ReviewRequired
			out.Differences = append(out.Differences, Difference{LineID: x.ID, ShipmentLineID: x.ShipmentLineID, Lot: x.Quantity, Confirmed: confirmed})
		}
	}
	return out
}
