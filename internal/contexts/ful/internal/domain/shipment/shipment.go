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
	// RoleNotify is the order's notify party (S5R-D40: the address role notify the order revision
	// binds), screened like every party of the shipment (R44).
	RoleNotify = "notify"
	// RoleEndUser is the end user the shipment states (S5R-D40: a PTY party at a confirmed revision,
	// no address), screened like every party of the shipment; CNX's licence check takes it before the
	// consignee and the customer (CNX-INV-002).
	RoleEndUser = "end_user"

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
// purchase commitment and supplier the delivery and the release are of; QualityConcession says that
// QLT granted the release on an approved concession (S5R-D18: shown on the shipment and its
// documents).
type Line struct {
	ID                string
	OrderLineID       string
	SkuID             string
	SpecRevision      int64
	UnitID            string
	UnitRevision      int64
	OrderQuantity     kernel.Decimal
	Quantity          kernel.Decimal
	Delivery          DeliveryRef
	DeliveryVersion   int64
	CommitmentID      string
	SupplierID        string
	ReleaseID         string
	QualityConcession bool
	// Stock balance of a stock-route line (R29). Empty on a supplier_direct line.
	LocationID    string
	BatchID       string
	CustodyKind   string
	ReservationID string
}

// Party is a party the confirmation screens (R44): the order's customer (Role RoleCustomer, no
// address), the party of an address role the order binds, its notify party (RoleNotify, S5R-D40) or
// the end user the shipment states (RoleEndUser, no address, S5R-D40).
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
	ID              string
	Status          string
	RowVersion      int64
	Route           string
	Order           OrderBinding
	CustomerID      string
	Lines           []Line
	Parties         []Party
	PackingRecords  int64
	Plan            *Packing // the current planned packing
	Actual          *Packing // the current actual packing
	RequestHash     string
	DocumentProfile string
	OwnerActor      string
	OwnerTeam       string
	Prepared        Act
	Confirmation    *Confirmation
	Cancellation    *Cancellation
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
// it serves new downstream work, otherwise TRD's reason — its customer, and, as OrderRevisionRef v1
// reads that revision (S5R-D35), its lines, the parties of the address roles it binds and its notify
// party (nil: none; S5R-D40).
type OrderFact struct {
	Binding         OrderBinding
	Code            string
	CustomerID      string
	Customer        int64 // the customer's party revision
	DocumentProfile string
	Lines           []OrderLine
	Addresses       []Party
	Notify          *Party
}

// PartyRef names a PTY party at a confirmed revision (PartyRef v1).
type PartyRef struct {
	PartyID  string
	Revision int64
}

// PartyFact is what PTY says of a party now (PartyRef v1, R44): its state and latest confirmed
// revision. A party the actor does not read is absent.
type PartyFact struct {
	PartyID  string
	Status   string
	Revision int64
}

// PartyConfirmed is PTY's state of a confirmed party.
const PartyConfirmed = "confirmed"

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
	Concession  bool // granted on an approved concession (S5R-D18)
}

// DeliveryFact is what PUR says of a supplier delivery line at the version asked (R59): Status,
// Code — "" when that version is the delivery's verified one and serves a shipment —, what it is
// of, the Verified quantity and what its purchase commitment line adds up to (S5R-D05).
type DeliveryFact struct {
	Ref      DeliveryRef
	Version  int64
	Status   string
	Code     string
	Scope    Scope
	Verified kernel.Decimal
	Line     CommitmentLineTotals
}

// CommitmentLine names a purchase commitment line: the commitment and the sales order line it buys.
// Every supplier delivery line of it serves that order line.
type CommitmentLine struct {
	CommitmentID string
	OrderLineID  string
}

// CommitmentLineTotals is what PUR's SupplierDeliveryGuard v2 states of a commitment line (R59,
// S5R-D05): the verified quantity of the versions verified now of all its deliveries, and what its
// approved dispositions took out of the order (returned to the supplier, written off or held for
// reuse).
type CommitmentLineTotals struct {
	Verified kernel.Decimal
	Disposed kernel.Decimal
}

// StockPlace is the balance a stock-route line ships from.
type StockPlace struct {
	LocationID    string
	BatchID       string
	CustodyKind   string
	ReservationID string
}

// StockLineFact is what ResolveAvailability said of that balance before the transaction. Enabled is
// false when the company has not turned STK on. OnHand, Reserved and Available are the balance;
// a named reservation is covered by OnHand even when Available (on hand less reserved) is short.
type StockLineFact struct {
	Enabled      bool
	OnHand       kernel.Decimal
	Reserved     kernel.Decimal
	Available    kernel.Decimal
	UnitID       string
	UnitRevision int64
}

// RequestLine is a line of a preparation: a supplier delivery at Version, or a stock balance.
type RequestLine struct {
	ID          string
	OrderLineID string
	Quantity    kernel.Decimal
	Delivery    DeliveryRef
	Version     int64
	ReleaseID   string
	Stock       *StockPlace
}

// Consumed is what FUL's confirmed shipments shipped before, as a transaction holding the order's
// fulfilment root reads it: of each order line over every order revision, of each supplier
// delivery line over every version, of each purchase commitment line over all its deliveries and
// under each quality release (FUL-INV-001, FUL-INV-003).
type Consumed struct {
	OrderLines      map[string]kernel.Decimal
	Deliveries      map[DeliveryRef]kernel.Decimal
	CommitmentLines map[CommitmentLine]kernel.Decimal
	Releases        map[string]kernel.Decimal
}

// Request is a preparation: the shipment's ID, its order and route, its lines, the end user it
// states (nil: none; S5R-D40) and the hash of the request.
type Request struct {
	ID          string
	OrderID     string
	Route       string
	Lines       []RequestLine
	EndUser     *PartyRef
	RequestHash string
}

// Inputs are what a preparation is judged on: the order as TRD shows it (nil: no order the preparer
// reads), the releases and delivery lines the preparer reads, by ID, what PTY says of the end user
// the request states (nil: none the preparer reads), what the order's confirmed shipments consumed,
// and who prepares when.
type Inputs struct {
	Order      *OrderFact
	Releases   map[string]ReleaseFact
	Deliveries map[DeliveryRef]DeliveryFact
	EndUser    *PartyFact
	Consumed   Consumed
	Preparer   kernel.ActorRef
	Epoch      int64
	Team       string
	Now        time.Time
	// StockEnabled is true when STK answered the stock route. False keeps the stock route refused
	// (DEPENDENCY_NOT_SUPPORTED), which is every preparation that does not wire STK.
	StockEnabled bool
	Stock        []StockLineFact
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
// (QUALITY_RELEASE_INSUFFICIENT); then a stated end user the preparer does not read or that is not
// confirmed (REFERENCE_INVALID at /args/end_user_party) or at another revision than PTY's latest
// confirmed one (REFERENCE_STALE at /args/end_user_party/revision; S5R-D40). Nothing is reserved: the
// confirmation decides again under its locks and guards. The shipment binds the customer, the order's
// address roles, its notify party and the stated end user as its parties (R44: each is screened).
func Prepare(req Request, in Inputs) (*Shipment, error) {
	if req.Route == RouteStock {
		if !in.StockEnabled {
			return nil, Reject(CodeDependencyNotSupported, "/args/route", "Only the supplier direct delivery route is delivered; the stock route waits for STK")
		}
		return prepareStock(req, in)
	}
	if req.Route != RouteSupplierDirect {
		return nil, Reject(CodeDependencyNotSupported, "/args/route", "Only the supplier direct delivery route is delivered; the stock route waits for STK")
	}
	s, o, err := openShipment(req, in)
	if err != nil {
		return nil, err
	}
	shipping, fromDelivery, underRelease := map[string]kernel.Decimal{}, map[DeliveryRef]kernel.Decimal{}, map[string]kernel.Decimal{}
	fromCommitment := map[CommitmentLine]kernel.Decimal{}
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
		cl := CommitmentLine{CommitmentID: d.Scope.CommitmentID, OrderLineID: ol.LineID}
		fromCommitment[cl] = fromCommitment[cl].Add(l.Quantity)
		if err := WithinCommitmentLine(cl, in.Consumed.CommitmentLines[cl], fromCommitment[cl], d.Line, linePath(i, "quantity")); err != nil {
			return nil, err
		}
		underRelease[l.ReleaseID] = underRelease[l.ReleaseID].Add(l.Quantity)
		if err := WithinRelease(in.Consumed.Releases[l.ReleaseID], underRelease[l.ReleaseID], r.Quantity, linePath(i, "quantity")); err != nil {
			return nil, err
		}
		s.Lines = append(s.Lines, Line{ID: l.ID, OrderLineID: ol.LineID, SkuID: ol.SkuID, SpecRevision: ol.SpecRevision, UnitID: ol.UnitID,
			UnitRevision: ol.UnitRevision, OrderQuantity: ol.Quantity, Quantity: l.Quantity, Delivery: l.Delivery, DeliveryVersion: l.Version,
			CommitmentID: d.Scope.CommitmentID, SupplierID: d.Scope.SupplierID, ReleaseID: l.ReleaseID, QualityConcession: r.Concession})
	}
	if len(s.Lines) == 0 {
		return nil, Reject(kernel.CodeSchemaInvalid, "/args/lines", "")
	}
	if err := bindParties(s, req, in, o); err != nil {
		return nil, err
	}
	return s, nil
}

// openShipment checks the order and returns the prepared root. The caller fills lines and parties.
func openShipment(req Request, in Inputs) (*Shipment, *OrderFact, error) {
	o := in.Order
	if o == nil {
		return nil, nil, Reject(kernel.CodeReferenceInvalid, "/args/order_id", "No order the actor reads")
	}
	if o.Code != "" {
		return nil, nil, Upstream(o.Code, "/args/order_id", "The order does not serve a shipment: "+o.Code)
	}
	if o.Binding.OrderID != req.OrderID || o.Binding.OrderRevision < 1 || o.CustomerID == "" || o.Customer < 1 {
		return nil, nil, Reject(kernel.CodeReferenceInvalid, "/args/order_id", "The order is not intact")
	}
	now := kernel.Instant(in.Now)
	profile := o.DocumentProfile
	if profile == "" {
		profile = "generic"
	}
	s := &Shipment{ID: req.ID, Status: StatusPrepared, RowVersion: 1, Route: req.Route, Order: o.Binding, CustomerID: o.CustomerID,
		RequestHash: req.RequestHash, DocumentProfile: profile, OwnerActor: in.Preparer.ID, OwnerTeam: in.Team,
		Prepared: Act{By: in.Preparer, Epoch: in.Epoch, At: now}}
	return s, o, nil
}

// prepareStock binds a stock-route shipment (R29). It does not read a supplier delivery or a quality
// release. A named reservation is allowed when on-hand covers the quantity; available quantity can
// be short because the hold is inside reserved. Confirm's SQL is the reservation authority.
func prepareStock(req Request, in Inputs) (*Shipment, error) {
	s, o, err := openShipment(req, in)
	if err != nil {
		return nil, err
	}
	if len(in.Stock) != len(req.Lines) {
		return nil, Reject(kernel.CodeReferenceInvalid, "/args/lines", "STK did not answer every line")
	}
	shipping := map[string]kernel.Decimal{}
	for i, l := range req.Lines {
		if l.Stock == nil {
			return nil, Reject(kernel.CodeSchemaInvalid, linePath(i, "stock"), "")
		}
		at := slices.IndexFunc(o.Lines, func(ol OrderLine) bool { return ol.LineID == l.OrderLineID })
		if at < 0 {
			return nil, Reject(kernel.CodeReferenceInvalid, linePath(i, "order_line_id"), "The order's current revision has no such line")
		}
		ol := o.Lines[at]
		fact := in.Stock[i]
		if !fact.Enabled {
			return nil, Reject(CodeDependencyNotSupported, "/args/route", "STK is not enabled for the company")
		}
		if fact.UnitID != "" && (fact.UnitID != ol.UnitID || fact.UnitRevision != ol.UnitRevision) {
			return nil, Reject(kernel.CodeInvalidKnownValue, linePath(i, "stock"), "The stock balance is in another unit than the order line")
		}
		cover := fact.Available
		if l.Stock.ReservationID != "" {
			cover = fact.OnHand
		}
		if cover.Cmp(l.Quantity) < 0 {
			return nil, Reject(CodeStockInsufficient, linePath(i, "quantity"), "The stock on hand does not cover the shipment line")
		}
		shipping[ol.LineID] = shipping[ol.LineID].Add(l.Quantity)
		if err := WithinOrderLine(ol.LineID, in.Consumed.OrderLines[ol.LineID], shipping[ol.LineID], ol.Quantity, linePath(i, "quantity")); err != nil {
			return nil, err
		}
		s.Lines = append(s.Lines, Line{ID: l.ID, OrderLineID: ol.LineID, SkuID: ol.SkuID, SpecRevision: ol.SpecRevision, UnitID: ol.UnitID,
			UnitRevision: ol.UnitRevision, OrderQuantity: ol.Quantity, Quantity: l.Quantity, LocationID: l.Stock.LocationID,
			BatchID: l.Stock.BatchID, CustodyKind: l.Stock.CustodyKind, ReservationID: l.Stock.ReservationID})
	}
	if len(s.Lines) == 0 {
		return nil, Reject(kernel.CodeSchemaInvalid, "/args/lines", "")
	}
	if err := bindParties(s, req, in, o); err != nil {
		return nil, err
	}
	return s, nil
}

// bindParties adds the customer, the order's address roles, its notify party and the stated end user.
func bindParties(s *Shipment, req Request, in Inputs, o *OrderFact) error {
	s.Parties = append([]Party{{Role: RoleCustomer, PartyID: o.CustomerID, PartyRevision: o.Customer}}, o.Addresses...)
	if o.Notify != nil {
		n := *o.Notify
		n.Role = RoleNotify
		s.Parties = append(s.Parties, n)
	}
	if u := req.EndUser; u != nil {
		f := in.EndUser
		switch {
		case f == nil || f.PartyID != u.PartyID:
			return Reject(kernel.CodeReferenceInvalid, "/args/end_user_party", "No party the actor reads")
		case f.Status != PartyConfirmed:
			return Reject(kernel.CodeReferenceInvalid, "/args/end_user_party", "The party is "+f.Status+", not confirmed")
		case f.Revision != u.Revision:
			return Reject(CodeReferenceStale, "/args/end_user_party/revision", "The party's confirmed revision is "+
				strconv.FormatInt(f.Revision, 10))
		}
		s.Parties = append(s.Parties, Party{Role: RoleEndUser, PartyID: u.PartyID, PartyRevision: u.Revision})
	}
	return nil
}

// EndUser returns the end user the shipment states (S5R-D40), or nil.
func (s *Shipment) EndUser() *Party {
	for i := range s.Parties {
		if s.Parties[i].Role == RoleEndUser {
			return &s.Parties[i]
		}
	}
	return nil
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
