// Package shipment holds the rules of FUL's Shipment aggregate (tactical model FUL-AGG-Shipment,
// SM-FUL-SHIPMENT; UL FUL-ShipmentConfirmation, FUL-ActualPackage; WBS S5-03a): what a shipment
// binds when it is prepared, its packing records, its cancellation and its confirmation. The
// quantities an order's confirmed shipments consumed are judged in package fulfilment. The package
// knows no database: the application gives it what TRD, QLT and PUR said and what the guards
// answered.
package shipment

import (
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// States of a shipment (SM-FUL-SHIPMENT), its routes and the role of the order's customer among
// its parties.
const (
	StatusPrepared  = "prepared"
	StatusConfirmed = "confirmed"
	StatusCancelled = "cancelled"

	RouteSupplierDirect = "supplier_direct"
	RouteStock          = "stock"

	RoleCustomer = "customer"

	// SubjectCommitmentLine is the kind of a quality release's subject that is a line of a PUR
	// purchase commitment (QLT SubjectRef).
	SubjectCommitmentLine = "purchase_commitment_line"
)

// Act is who did something, under which authority epoch, when.
type Act struct {
	By    kernel.ActorRef
	Epoch int64
	At    time.Time
}

// OrderBinding is the order a shipment binds: at an order revision and its order hash.
type OrderBinding struct {
	OrderID       string
	OrderRevision int64
	OrderHash     string
}

// DeliveryRef names a line of a PUR supplier delivery.
type DeliveryRef struct {
	DeliveryID string
	LineID     string
}

// Compare orders delivery line references by delivery and line.
func (r DeliveryRef) Compare(o DeliveryRef) int {
	if r.DeliveryID != o.DeliveryID {
		if r.DeliveryID < o.DeliveryID {
			return -1
		}
		return 1
	}
	switch {
	case r.LineID < o.LineID:
		return -1
	case r.LineID > o.LineID:
		return 1
	}
	return 0
}

// Line ships Quantity of order line OrderLineID, in the order line's unit, from supplier delivery
// line Delivery at DeliveryVersion under quality release ReleaseID. OrderQuantity is the order
// line's quantity at the order revision the shipment binds; CommitmentID and SupplierID are the
// purchase commitment and supplier the delivery and the release are of.
type Line struct {
	ID              string
	OrderLineID     string
	SkuID           string
	SpecRevision    int64
	UnitID          string
	UnitRevision    int64
	OrderQuantity   kernel.Decimal
	Quantity        kernel.Decimal
	Delivery        DeliveryRef
	DeliveryVersion int64
	CommitmentID    string
	SupplierID      string
	ReleaseID       string
}

// Party is a party the confirmation screens (R44): the order's customer (Role RoleCustomer, no
// address) or the party of an address role the order binds.
type Party struct {
	Role          string
	PartyID       string
	PartyRevision int64
	AddressID     string
}

// Cancellation is the cancellation of a shipment.
type Cancellation struct {
	Reason    string
	Cancelled Act
}

// Shipment is the aggregate root. RowVersion counts its commands: 1 when prepared, one more for
// every packing record and for its confirmation or cancellation.
type Shipment struct {
	ID             string
	Status         string
	RowVersion     int64
	Route          string
	Order          OrderBinding
	CustomerID     string
	Lines          []Line
	Parties        []Party
	PackingRecords int64
	Plan           *Packing // the current planned packing
	Actual         *Packing // the current actual packing
	RequestHash    string
	OwnerActor     string
	OwnerTeam      string
	Prepared       Act
	Confirmation   *Confirmation
	Cancellation   *Cancellation
}

// OrderLine is a line of the order's current revision as TRD shows it.
type OrderLine struct {
	LineID       string
	SkuID        string
	SpecRevision int64
	UnitID       string
	UnitRevision int64
	Quantity     kernel.Decimal
}

// OrderFact is what TRD says of the order now (ActiveOrderRef v1, R27): its binding, Code — "" when
// it serves new downstream work, otherwise TRD's reason — its customer, its lines and the parties
// of the address roles it binds.
type OrderFact struct {
	Binding    OrderBinding
	Code       string
	CustomerID string
	Customer   int64 // the customer's party revision
	Lines      []OrderLine
	Addresses  []Party
}

// Scope is what a quality release or a supplier delivery line is of: the purchase commitment and
// its supplier, the sales order line it serves, the SKU specification and the unit of its quantity.
type Scope struct {
	CommitmentID string
	SupplierID   string
	OrderID      string
	OrderLineID  string
	SkuID        string
	SpecRevision int64
	UnitID       string
	UnitRevision int64
}

// ReleaseFact is what QLT says of a quality release (QualityReleaseRef v1, R26): Code "" when it
// serves a shipment at the instant asked, State its state then, SubjectKind the kind of its subject
// (Scope.CommitmentID is the subject's ID for SubjectCommitmentLine) and Quantity what it released.
type ReleaseFact struct {
	ID          string
	Code        string
	State       string
	SubjectKind string
	Scope       Scope
	Quantity    kernel.Decimal
}

// DeliveryFact is what PUR says of a supplier delivery line at the version asked (R59): Status,
// Code — "" when that version is the delivery's verified one and serves a shipment —, what it is
// of and the Verified quantity.
type DeliveryFact struct {
	Ref      DeliveryRef
	Version  int64
	Status   string
	Code     string
	Scope    Scope
	Verified kernel.Decimal
}

// RequestLine is a line of a preparation: Delivery at Version.
type RequestLine struct {
	ID          string
	OrderLineID string
	Quantity    kernel.Decimal
	Delivery    DeliveryRef
	Version     int64
	ReleaseID   string
}

// Consumed is what FUL's confirmed shipments shipped before, as a transaction holding the order's
// fulfilment root reads it: of each order line over every order revision, of each supplier
// delivery line over every version, and under each quality release (FUL-INV-001, FUL-INV-003).
type Consumed struct {
	OrderLines map[string]kernel.Decimal
	Deliveries map[DeliveryRef]kernel.Decimal
	Releases   map[string]kernel.Decimal
}

// Request is a preparation: the shipment's ID, its order and route, its lines and the hash of the
// request.
type Request struct {
	ID          string
	OrderID     string
	Route       string
	Lines       []RequestLine
	RequestHash string
}

// Inputs are what a preparation is judged on: the order as TRD shows it (nil: no order the preparer
// reads), the releases and delivery lines the preparer reads, by ID, what the order's confirmed
// shipments consumed, and who prepares when.
type Inputs struct {
	Order      *OrderFact
	Releases   map[string]ReleaseFact
	Deliveries map[DeliveryRef]DeliveryFact
	Consumed   Consumed
	Preparer   kernel.ActorRef
	Epoch      int64
	Team       string
	Now        time.Time
}

func linePath(i int, member string) string {
	return "/args/lines/" + strconv.Itoa(i) + "/" + member
}

// Prepare decides a preparation. It refuses, in this order: the stock route
// (DEPENDENCY_NOT_SUPPORTED, D-10); an order the preparer does not read (REFERENCE_INVALID) or one
// TRD says does not serve new work (TRD's code); then line by line an order line the order's
// current revision does not have, a release or a delivery line the preparer does not read
// (REFERENCE_INVALID), a release that does not serve a shipment now (QUALITY_RELEASE_REQUIRED), a
// delivery line PUR refuses (PUR's code, such as SUPPLIER_DELIVERY_UNVERIFIED), a release or
// delivery line of another order, order line, specification, supplier or purchase commitment
// (REFERENCE_INVALID) or in another unit than the order line's (DEPENDENCY_NOT_SUPPORTED), one
// delivery line named at two versions (REFERENCE_INVALID), and lines that together with what the
// order's confirmed shipments consumed ship more of an order line than it orders
// (SHIPMENT_QUANTITY_EXCEEDED, FUL-INV-001), more of a delivery line than PUR verified
// (SUPPLIER_DELIVERY_INSUFFICIENT, FUL-INV-003) or more under a release than it released
// (QUALITY_RELEASE_INSUFFICIENT). Nothing is reserved: the confirmation decides again under its
// locks and guards.
func Prepare(req Request, in Inputs) (*Shipment, error) {
	if req.Route != RouteSupplierDirect {
		return nil, Reject(CodeDependencyNotSupported, "/args/route", "Only the supplier direct delivery route is delivered; the stock route waits for STK")
	}
	o := in.Order
	if o == nil {
		return nil, Reject(kernel.CodeReferenceInvalid, "/args/order_id", "No order the actor reads")
	}
	if o.Code != "" {
		return nil, Upstream(o.Code, "/args/order_id", "The order does not serve a shipment: "+o.Code)
	}
	if o.Binding.OrderID != req.OrderID || o.Binding.OrderRevision < 1 || o.CustomerID == "" || o.Customer < 1 {
		return nil, Reject(kernel.CodeReferenceInvalid, "/args/order_id", "The order is not intact")
	}
	now := kernel.Instant(in.Now)
	s := &Shipment{ID: req.ID, Status: StatusPrepared, RowVersion: 1, Route: req.Route, Order: o.Binding, CustomerID: o.CustomerID,
		RequestHash: req.RequestHash, OwnerActor: in.Preparer.ID, OwnerTeam: in.Team,
		Prepared: Act{By: in.Preparer, Epoch: in.Epoch, At: now}}
	shipping, fromDelivery, underRelease := map[string]kernel.Decimal{}, map[DeliveryRef]kernel.Decimal{}, map[string]kernel.Decimal{}
	versions := map[DeliveryRef]int64{}
	for i, l := range req.Lines {
		at := slices.IndexFunc(o.Lines, func(ol OrderLine) bool { return ol.LineID == l.OrderLineID })
		if at < 0 {
			return nil, Reject(kernel.CodeReferenceInvalid, linePath(i, "order_line_id"), "The order's current revision has no such line")
		}
		ol := o.Lines[at]
		want := Scope{OrderID: req.OrderID, OrderLineID: ol.LineID, SkuID: ol.SkuID, SpecRevision: ol.SpecRevision, UnitID: ol.UnitID,
			UnitRevision: ol.UnitRevision}
		r, ok := in.Releases[l.ReleaseID]
		if !ok {
			return nil, Reject(kernel.CodeReferenceInvalid, linePath(i, "quality_release_id"), "No quality release the actor reads")
		}
		if r.Code != "" {
			return nil, Reject(CodeQualityReleaseRequired, linePath(i, "quality_release_id"), "The quality release is "+r.State)
		}
		if err := want.covers(r.Scope, linePath(i, "quality_release_id"), "quality release"); err != nil {
			return nil, err
		}
		if v, seen := versions[l.Delivery]; seen && v != l.Version {
			return nil, Reject(kernel.CodeReferenceInvalid, linePath(i, "supplier_delivery"), "The shipment names this supplier delivery line at two versions")
		}
		versions[l.Delivery] = l.Version
		d, ok := in.Deliveries[l.Delivery]
		if !ok || d.Version != l.Version {
			return nil, Reject(kernel.CodeReferenceInvalid, linePath(i, "supplier_delivery"), "No supplier delivery line the actor reads")
		}
		if d.Code != "" {
			return nil, Upstream(d.Code, linePath(i, "supplier_delivery"), "The supplier delivery is "+d.Status)
		}
		if err := want.covers(d.Scope, linePath(i, "supplier_delivery"), "supplier delivery"); err != nil {
			return nil, err
		}
		if d.Version < 1 || d.Scope.SupplierID == "" || d.Scope.CommitmentID == "" || d.Scope.SupplierID != r.Scope.SupplierID ||
			(r.SubjectKind == SubjectCommitmentLine && r.Scope.CommitmentID != d.Scope.CommitmentID) {
			return nil, Reject(kernel.CodeReferenceInvalid, linePath(i, "supplier_delivery"),
				"The supplier delivery and the quality release are of different suppliers or purchase commitments")
		}
		shipping[ol.LineID] = shipping[ol.LineID].Add(l.Quantity)
		if err := WithinOrderLine(ol.LineID, in.Consumed.OrderLines[ol.LineID], shipping[ol.LineID], ol.Quantity, linePath(i, "quantity")); err != nil {
			return nil, err
		}
		fromDelivery[l.Delivery] = fromDelivery[l.Delivery].Add(l.Quantity)
		if err := WithinDelivery(in.Consumed.Deliveries[l.Delivery], fromDelivery[l.Delivery], d.Verified, linePath(i, "quantity")); err != nil {
			return nil, err
		}
		underRelease[l.ReleaseID] = underRelease[l.ReleaseID].Add(l.Quantity)
		if err := WithinRelease(in.Consumed.Releases[l.ReleaseID], underRelease[l.ReleaseID], r.Quantity, linePath(i, "quantity")); err != nil {
			return nil, err
		}
		s.Lines = append(s.Lines, Line{ID: l.ID, OrderLineID: ol.LineID, SkuID: ol.SkuID, SpecRevision: ol.SpecRevision, UnitID: ol.UnitID,
			UnitRevision: ol.UnitRevision, OrderQuantity: ol.Quantity, Quantity: l.Quantity, Delivery: l.Delivery, DeliveryVersion: l.Version,
			CommitmentID: d.Scope.CommitmentID, SupplierID: d.Scope.SupplierID, ReleaseID: l.ReleaseID})
	}
	if len(s.Lines) == 0 {
		return nil, Reject(kernel.CodeSchemaInvalid, "/args/lines", "")
	}
	s.Parties = append([]Party{{Role: RoleCustomer, PartyID: o.CustomerID, PartyRevision: o.Customer}}, o.Addresses...)
	return s, nil
}

// covers refuses a release or delivery line whose scope got is not of the order line want
// describes: REFERENCE_INVALID for another order, order line or specification,
// DEPENDENCY_NOT_SUPPORTED for another unit (no unit conversion is delivered).
func (want Scope) covers(got Scope, path, what string) error {
	if got.OrderID != want.OrderID || got.OrderLineID != want.OrderLineID || got.SkuID != want.SkuID || got.SpecRevision != want.SpecRevision {
		return Reject(kernel.CodeReferenceInvalid, path, "The "+what+" is of another order line or specification")
	}
	if got.UnitID != want.UnitID || got.UnitRevision != want.UnitRevision {
		return Reject(CodeDependencyNotSupported, path, "The "+what+" is in another unit than the order line; unit conversion is not delivered")
	}
	return nil
}

// Answer decides what a preparation gets whose shipment ID is taken: nil when the stored shipment
// is the same request by the same preparer (the command is answered from it), and
// BUSINESS_KEY_CONFLICT otherwise.
func (s *Shipment) Answer(requestHash string, by kernel.ActorRef) error {
	if s.RequestHash != requestHash || s.Prepared.By != by {
		return Reject(CodeBusinessKeyConflict, "/args/shipment_id", "The shipment ID holds another request")
	}
	return nil
}

// open refuses a shipment that is not prepared (INVALID_STATE) and one at another row version than
// expected (VERSION_CONFLICT).
func (s *Shipment) open(expected int64) error {
	if s.Status != StatusPrepared {
		return Reject(kernel.CodeInvalidState, "/args/shipment_id", "The shipment is "+s.Status)
	}
	if s.RowVersion != expected {
		return Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// Cancel cancels a prepared shipment for reason (SM-FUL-SHIPMENT prepared → cancelled).
func (s *Shipment) Cancel(expected int64, reason string, act Act) error {
	if err := s.open(expected); err != nil {
		return err
	}
	act.At = kernel.Instant(act.At)
	s.Status, s.RowVersion, s.Cancellation = StatusCancelled, s.RowVersion+1, &Cancellation{Reason: reason, Cancelled: act}
	return nil
}

// Line returns the shipment's line id and its index, or false.
func (s *Shipment) Line(id string) (Line, int, bool) {
	for i, l := range s.Lines {
		if l.ID == id {
			return l, i, true
		}
	}
	return Line{}, -1, false
}

// Readable refuses the first of ids that found, the sources the actor reads, does not hold
// (REFERENCE_INVALID at path/<index>).
func Readable(ids []string, found map[string]bool, path string) error {
	for i, id := range ids {
		if !found[id] {
			return Reject(kernel.CodeReferenceInvalid, path+"/"+strconv.Itoa(i), "No source the actor reads")
		}
	}
	return nil
}
