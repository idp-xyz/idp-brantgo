// Package api is FUL's published contract (fulfilment and shipment). S5 unit B1a (WBS S5-03a) builds
// its first aggregates: the Shipment with its lines, packing plan and actual packing, and the
// OrderFulfilment root of an order (tactical model FUL-AGG-Shipment, FUL-AGG-OrderFulfilment,
// SM-FUL-SHIPMENT; FUL-INV-001, FUL-INV-003; UL FUL-ShipmentConfirmation, FUL-ActualPackage; S5
// decisions D-1, D-3, D-10, D-11, D-19). Unit B1b (S5-03b) adds milestone plans (milestone.go),
// export lots with ShipmentLineRef v1 and ExportLotRef v1 (exportlot.go); the commercial release,
// bookings and shipment documents follow (C1, C2). Python V0.50 has no fulfilment context: every rule
// comes from the design, and the tests cite its IDs.
//
//	shipment.prepare       human or AI (merchandiser): prepares a shipment of one active sales order
//	                       at the order revision and hash TRD shows now (ActiveOrderRef v1, R27). Each
//	                       line ships a quantity of an order line, in the order line's unit, from one
//	                       line of a PUR supplier delivery (R59) under one QLT quality release (R26);
//	                       the delivery line and the release must be of that order line, its SKU
//	                       specification and its supplier. Route supplier_direct only: the stock route
//	                       waits for STK (D-10, DEPENDENCY_NOT_SUPPORTED). A shipment line never
//	                       exceeds its order line; nothing is reserved by preparing.
//	packing.plan           human or AI: the planned packing of a prepared shipment — groups of equal
//	                       cartons with their contents per shipment line.
//	packing.record_actual  human or AI: the actual packing — carton numbers, contents per carton (the
//	                       SKU and size ratio), gross and net weight and volume per carton. Planned and
//	                       actual packing account for exactly the quantity of every shipment line
//	                       (BT11 "箱至订单数量一致"); a new record replaces the one before, which is kept.
//	shipment.cancel        human or AI: cancels a prepared shipment, with a reason.
//	shipment.confirm       human only (merchandiser): confirms the actual shipment. In FUL's
//	                       transaction, with the shipment and the order's fulfilment root locked, every
//	                       guard is re-checked in context code order — PTY PartyScreeningGuard on the
//	                       order's parties (R44), PUR SupplierDeliveryGuard (R59), QLT
//	                       QualityReleaseGuard (R26), TRD OrderGuard (R27) — and the quantities are
//	                       judged on what FUL's confirmed shipments consumed before: of each supplier
//	                       delivery line (FUL-INV-003, SUPPLIER_DELIVERY_INSUFFICIENT), of each
//	                       quality release (QUALITY_RELEASE_INSUFFICIENT) and of each order line
//	                       (FUL-INV-001, SHIPMENT_QUANTITY_EXCEEDED, zero tolerance, D-19). Since S5
//	                       unit C1 it also runs CNX's trade compliance and export licence guards and
//	                       STL's payment condition guard, needs a granted shipment release
//	                       (COMMERCIAL_RELEASE_BLOCKED) and consumes it (release.go); unchecked_guards
//	                       stays empty.
//
// TRD reads what an order shipped through ShipmentFacts v1 (facts.go, R70).
//
// Request members in snake_case: shipment.prepare {shipment_id, order_id, route, lines [{line_id,
// order_line_id, quantity, supplier_delivery {delivery_id, line_id, version}, quality_release_id}]};
// packing.plan and packing.record_actual {shipment_id, expected_version, packages [{cartons,
// carton_from, contents [{line_id, quantity_per_carton}], gross_weight_kg, net_weight_kg,
// volume_cbm}], evidence_ids}; shipment.cancel {shipment_id, expected_version, reason};
// shipment.confirm {shipment_id, expected_version, evidence_ids}.
package api

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Source is FUL's event source.
const Source = "idp-brant.ful"

// Contract names FUL's command contract, the envelope's contract member on the command routes
// POST /api/brant/v1/ful/commands/<command>.
const Contract = "brant.fulfilment/0.1.0"

// Commands, reads, permissions and events (Context Map FUL, tactical model FUL aggregates).
const (
	CommandPrepareShipment = "shipment.prepare"
	CommandPlanPacking     = "packing.plan"
	CommandRecordPacking   = "packing.record_actual"
	CommandCancelShipment  = "shipment.cancel"
	CommandConfirmShipment = "shipment.confirm"

	// Reads: a shipment, an order's fulfilment, the audit trail.
	CommandGetShipment   = "shipment.get"
	CommandGetFulfilment = "order_fulfilment.get"
	CommandAudit         = "ful.audit"
	PermissionRead       = "ful.read"
	PermissionAudit      = "ful.audit"

	EventShipmentConfirmed = "idp-brant.ful.shipment_confirmed.v1"

	// Routes of a shipment (ShipmentRoute). Only RouteSupplierDirect is delivered in S5 (D-10).
	RouteSupplierDirect = "supplier_direct"
	RouteStock          = "stock"

	// States of a shipment (SM-FUL-SHIPMENT).
	StatusPrepared  = "prepared"
	StatusConfirmed = "confirmed"
	StatusCancelled = "cancelled"

	// Kinds of a packing record.
	PackingPlan   = "plan"
	PackingActual = "actual"

	// The guards shipment.confirm, commercial_release.evaluate and document_release.record name in
	// their evidence. NotWired is the value confirmations before S5 unit C1 recorded for the guards
	// that were extension points then.
	GuardCommercialRelease = "ful.commercial_release.v1"
	GuardTradeCompliance   = "cnx.trade_compliance_guard.v1"
	GuardExportLicense     = "cnx.export_license_guard.v1"
	GuardPaymentCondition  = "stl.payment_condition_guard.v1"
	GuardPartyScreening    = "pty.party_screening_guard.v1"
	GuardSupplierDelivery  = "pur.supplier_delivery_guard.v1"
	GuardQualityRelease    = "qlt.quality_release_guard.v1"
	GuardOrder             = "trd.order_guard.v1"
	NotWired               = "not_wired"

	// GuardConfirmation names, in the evidence of the documents release of a confirmed shipment, the
	// shipment confirmation whose verdicts it reused (S5 C1b): "<release>/v<version>/consumed/<guard
	// evidence SHA-256>/r<order revision the goods left under>".
	GuardConfirmation = "ful.shipment_confirmation"
	// GuardTradeComplianceCurrent names, in the evidence of the documents release of a confirmed
	// shipment whose order moved to a later revision, CNX's answer for that current revision (S5
	// C1c): "<status>/<reason>/<check>/<decision>/<license_required>/r<revision>";
	// GuardTradeCompliance names the same for the revision the goods left under.
	GuardTradeComplianceCurrent = "cnx.trade_compliance_guard.v1/current"

	MaxLines          = 200
	MaxPackages       = 500
	MaxContents       = 50
	MaxCartons        = 999999
	MaxCartonNumber   = 999999999
	MaxReason         = 1000
	MaxEvidence       = 20
	MaxShippedLookups = 200
	maxVersion        = 2147483647
)

// DeliveryLineRef names a line of a PUR supplier delivery at a version of the delivery, as PUR's
// SupplierDeliveryGuard v1 takes it (R59): LineID is the order line the delivery line's commitment
// line buys, Version the delivery's verified version the shipment ships from.
type DeliveryLineRef struct {
	DeliveryID string
	LineID     string
	Version    int64
}

// ShipmentLine ships Quantity (kernel.QuantityFormat, positive, in the order line's unit) of order
// line OrderLineID from SupplierDelivery under quality release QualityReleaseID.
type ShipmentLine struct {
	LineID           string
	OrderLineID      string
	Quantity         string
	SupplierDelivery DeliveryLineRef
	QualityReleaseID string
}

// PrepareShipment prepares shipment ShipmentID of order OrderID by Route. Human or AI.
type PrepareShipment struct {
	IdempotencyKey string
	ShipmentID     string
	OrderID        string
	Route          string
	Lines          []ShipmentLine
}

// PackageContent is what each carton of a package holds of shipment line LineID.
type PackageContent struct {
	LineID            string
	QuantityPerCarton string
}

// Package is a group of Cartons equal cartons. CartonFrom is the number of its first carton (the
// cartons are numbered CartonFrom to CartonFrom+Cartons−1); GrossWeightKg, NetWeightKg and VolumeCbm
// are per carton (kernel.QuantityFormat). A plan may leave the numbers, weights and volume out; an
// actual packing states them all.
type Package struct {
	Cartons       int64
	CartonFrom    int64
	Contents      []PackageContent
	GrossWeightKg string
	NetWeightKg   string
	VolumeCbm     string
}

// PlanPacking records the planned packing of shipment ShipmentID at ExpectedVersion. Human or AI.
type PlanPacking struct {
	IdempotencyKey  string
	ShipmentID      string
	ExpectedVersion int64
	Packages        []Package
	EvidenceIDs     []string
}

// RecordPacking records the actual packing of shipment ShipmentID at ExpectedVersion; EvidenceIDs
// are EVD sources such as the factory's packing list (R74). Human or AI.
type RecordPacking struct {
	IdempotencyKey  string
	ShipmentID      string
	ExpectedVersion int64
	Packages        []Package
	EvidenceIDs     []string
}

// CancelShipment cancels prepared shipment ShipmentID at ExpectedVersion for Reason. Human or AI.
type CancelShipment struct {
	IdempotencyKey  string
	ShipmentID      string
	ExpectedVersion int64
	Reason          string
}

// ConfirmShipment confirms prepared shipment ShipmentID at ExpectedVersion; EvidenceIDs are EVD
// sources of the shipment (R74). Human only.
type ConfirmShipment struct {
	IdempotencyKey  string
	ShipmentID      string
	ExpectedVersion int64
	EvidenceIDs     []string
}

// Command is one of FUL's commands.
type Command interface{ fulCommand() }

func (PrepareShipment) fulCommand() {}
func (PlanPacking) fulCommand()     {}
func (RecordPacking) fulCommand()   {}
func (CancelShipment) fulCommand()  {}
func (ConfirmShipment) fulCommand() {}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

func invalid(path, reason string) error {
	return kernel.Reject(kernel.CodeInvalidKnownValue, path, reason)
}

// Reason reports whether s is a reason: 1 to MaxReason characters of valid UTF-8, not only white
// space, without control characters.
func Reason(s string) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= MaxReason && utf8.ValidString(s) && strings.TrimSpace(s) != "" && strings.IndexFunc(s, unicode.IsControl) < 0
}

// Quantity parses s as a quantity in kernel.QuantityFormat.
func Quantity(s string) (kernel.Decimal, bool) {
	q, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	return q, err == nil
}

func index(path string, i int) string { return path + "/" + strconv.Itoa(i) }

func evidence(ids []string, path string) error {
	if len(ids) > MaxEvidence {
		return schema(path)
	}
	seen := map[string]bool{}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) || seen[id] {
			return schema(index(path, i))
		}
		seen[id] = true
	}
	return nil
}

func head(key, shipmentID string) error {
	switch {
	case !kernel.ValidIdentifier(key):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(shipmentID):
		return schema("/args/shipment_id")
	}
	return nil
}

func version(v int64) error {
	if v < 1 || v > maxVersion {
		return schema("/args/expected_version")
	}
	return nil
}

// Validate checks the closed contract: identifiers, a known route, 1 to MaxLines lines with
// distinct line IDs (DUPLICATE_LINE_ID), positive quantities and complete references.
func (c PrepareShipment) Validate() error {
	if err := head(c.IdempotencyKey, c.ShipmentID); err != nil {
		return err
	}
	switch {
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case c.Route != RouteSupplierDirect && c.Route != RouteStock:
		return schema("/args/route")
	case len(c.Lines) < 1 || len(c.Lines) > MaxLines:
		return schema("/args/lines")
	}
	seen := map[string]bool{}
	for i, l := range c.Lines {
		p := index("/args/lines", i)
		if !kernel.ValidIdentifier(l.LineID) {
			return schema(p + "/line_id")
		}
		if seen[l.LineID] {
			return kernel.Reject(kernel.CodeDuplicateLineID, p+"/line_id", "")
		}
		seen[l.LineID] = true
		switch q, ok := Quantity(l.Quantity); {
		case !kernel.ValidIdentifier(l.OrderLineID):
			return schema(p + "/order_line_id")
		case !ok || q.Sign() <= 0:
			return schema(p + "/quantity")
		case !kernel.ValidIdentifier(l.SupplierDelivery.DeliveryID):
			return schema(p + "/supplier_delivery/delivery_id")
		case !kernel.ValidIdentifier(l.SupplierDelivery.LineID):
			return schema(p + "/supplier_delivery/line_id")
		case l.SupplierDelivery.Version < 1 || l.SupplierDelivery.Version > maxVersion:
			return schema(p + "/supplier_delivery/version")
		case !kernel.ValidIdentifier(l.QualityReleaseID):
			return schema(p + "/quality_release_id")
		}
	}
	return nil
}

// measure parses an optional per-carton measure; required says whether it must be stated.
func measure(s, path string, required bool) (kernel.Decimal, error) {
	if s == "" {
		if required {
			return kernel.Decimal{}, schema(path)
		}
		return kernel.Decimal{}, nil
	}
	q, ok := Quantity(s)
	if !ok || q.Sign() <= 0 {
		return kernel.Decimal{}, schema(path)
	}
	return q, nil
}

// packages checks the packages of a packing record: 1 to MaxPackages packages of 1 to MaxCartons
// cartons, each with 1 to MaxContents contents of distinct lines and positive quantities; carton
// numbers that do not overlap; a net weight not above the gross weight. An actual packing states
// carton numbers, weights and volume of every package.
func packages(ps []Package, actual bool) error {
	if len(ps) < 1 || len(ps) > MaxPackages {
		return schema("/args/packages")
	}
	type span struct{ from, to int64 }
	var numbered []span
	for i, p := range ps {
		at := index("/args/packages", i)
		switch {
		case p.Cartons < 1 || p.Cartons > MaxCartons:
			return schema(at + "/cartons")
		case p.CartonFrom < 0 || p.CartonFrom > MaxCartonNumber || (actual && p.CartonFrom == 0):
			return schema(at + "/carton_from")
		case len(p.Contents) < 1 || len(p.Contents) > MaxContents:
			return schema(at + "/contents")
		}
		seen := map[string]bool{}
		for j, c := range p.Contents {
			cp := index(at+"/contents", j)
			if !kernel.ValidIdentifier(c.LineID) {
				return schema(cp + "/line_id")
			}
			if seen[c.LineID] {
				return kernel.Reject(kernel.CodeDuplicateLineID, cp+"/line_id", "")
			}
			seen[c.LineID] = true
			if q, ok := Quantity(c.QuantityPerCarton); !ok || q.Sign() <= 0 {
				return schema(cp + "/quantity_per_carton")
			}
		}
		gross, err := measure(p.GrossWeightKg, at+"/gross_weight_kg", actual)
		if err != nil {
			return err
		}
		net, err := measure(p.NetWeightKg, at+"/net_weight_kg", actual)
		if err != nil {
			return err
		}
		if _, err = measure(p.VolumeCbm, at+"/volume_cbm", actual); err != nil {
			return err
		}
		if p.GrossWeightKg != "" && p.NetWeightKg != "" && net.Cmp(gross) > 0 {
			return invalid(at+"/net_weight_kg", "The net weight is above the gross weight")
		}
		if p.CartonFrom > 0 {
			s := span{p.CartonFrom, p.CartonFrom + p.Cartons - 1}
			if s.to > MaxCartonNumber {
				return schema(at + "/carton_from")
			}
			for _, o := range numbered {
				if s.from <= o.to && o.from <= s.to {
					return invalid(at+"/carton_from", "Carton numbers overlap those of another package")
				}
			}
			numbered = append(numbered, s)
		}
	}
	return nil
}

// Validate checks the closed contract of a planned packing.
func (c PlanPacking) Validate() error {
	if err := head(c.IdempotencyKey, c.ShipmentID); err != nil {
		return err
	}
	if err := version(c.ExpectedVersion); err != nil {
		return err
	}
	if err := packages(c.Packages, false); err != nil {
		return err
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids")
}

// Validate checks the closed contract of an actual packing: every package states its carton
// numbers, gross and net weight and volume.
func (c RecordPacking) Validate() error {
	if err := head(c.IdempotencyKey, c.ShipmentID); err != nil {
		return err
	}
	if err := version(c.ExpectedVersion); err != nil {
		return err
	}
	if err := packages(c.Packages, true); err != nil {
		return err
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids")
}

// Validate checks the closed contract.
func (c CancelShipment) Validate() error {
	if err := head(c.IdempotencyKey, c.ShipmentID); err != nil {
		return err
	}
	if err := version(c.ExpectedVersion); err != nil {
		return err
	}
	if !Reason(c.Reason) {
		return schema("/args/reason")
	}
	return nil
}

// Validate checks the closed contract.
func (c ConfirmShipment) Validate() error {
	if err := head(c.IdempotencyKey, c.ShipmentID); err != nil {
		return err
	}
	if err := version(c.ExpectedVersion); err != nil {
		return err
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids")
}

// Receipt is the closed success answer of a command.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// OrderBinding is an order as a shipment binds it: at an order revision and its order hash (TRD
// OrderBinding).
type OrderBinding struct {
	OrderID       string
	OrderRevision int64
	OrderHash     string
}

// ShipmentReceipt answers every shipment command: the receipt (ObjectID the shipment, Status its
// state) and the order the shipment binds. UncheckedGuards names, for a confirmed shipment, the
// extension points its confirmation recorded as not wired. A shipment.prepare answered from the
// stored shipment (the same request by the same preparer under another key) is Replayed.
type ShipmentReceipt struct {
	Receipt
	Order           OrderBinding
	UncheckedGuards []string
}

// ShipmentLineView is a shipment line as the reads show it: the order line with its SKU
// specification, unit and quantity at the bound order revision, the quantity shipped, the supplier
// delivery line at the version bound with the purchase commitment and supplier it is of, and the
// quality release.
type ShipmentLineView struct {
	LineID            string
	OrderLineID       string
	SkuID             string
	SpecRevision      int64
	UnitID            string
	UnitRevision      int64
	OrderLineQuantity string
	Quantity          string
	SupplierDelivery  DeliveryLineRef
	CommitmentID      string
	SupplierID        string
	QualityReleaseID  string
}

// ShipmentParty is a party the confirmation screens (R44): the order's customer (Role customer, no
// address) or the party of an address role the order binds (buyer, payer, consignee, …).
type ShipmentParty struct {
	Role          string
	PartyID       string
	PartyRevision int64
	AddressID     string
}

// PackingView is a packing record: its kind and version, the packages, the totals derived from them
// and who recorded it.
type PackingView struct {
	Kind           string
	Version        int64
	Packages       []Package
	CartonCount    int64
	GrossWeightKg  string
	NetWeightKg    string
	VolumeCbm      string
	EvidenceIDs    []string
	RecordedBy     kernel.ActorRef
	RecordedAt     time.Time
	AuthorityEpoch int64
}

// GuardEvidence is one named value of a confirmation's evidence.
type GuardEvidence struct {
	Name  string
	Value string
}

// ConfirmationView is the confirmation of a shipment: who confirmed it when, the evidence it
// cited, what every guard answered in the confirming transaction, the SHA-256 of those entries, and
// the extension points recorded as not wired.
type ConfirmationView struct {
	ConfirmedBy     kernel.ActorRef
	ConfirmedAt     time.Time
	AuthorityEpoch  int64
	EvidenceIDs     []string
	Guards          []GuardEvidence
	GuardsSHA256    string
	UncheckedGuards []string
}

// CancellationView is the cancellation of a shipment.
type CancellationView struct {
	Reason         string
	CancelledBy    kernel.ActorRef
	CancelledAt    time.Time
	AuthorityEpoch int64
}

// ShipmentView is a shipment as the reads show it.
type ShipmentView struct {
	ShipmentID     string
	Status         string
	RowVersion     int64
	Route          string
	Order          OrderBinding
	CustomerID     string
	Lines          []ShipmentLineView
	Parties        []ShipmentParty
	PackingPlan    *PackingView
	ActualPacking  *PackingView
	PreparedBy     kernel.ActorRef
	PreparedAt     time.Time
	AuthorityEpoch int64
	Confirmation   *ConfirmationView
	Cancellation   *CancellationView
}

// FulfilmentLine is an order line's fulfilment now: its quantity at the order's current revision
// (empty when that revision no longer has the line), what the confirmed shipments shipped of it
// over every revision, what remains, and what prepared shipments plan to ship.
type FulfilmentLine struct {
	OrderLineID   string
	UnitID        string
	UnitRevision  int64
	OrderQuantity string
	Shipped       string
	Remaining     string
	Prepared      string
}

// FulfilmentShipment is one shipment of an order. ShipmentID is empty for one the reader does not
// see.
type FulfilmentShipment struct {
	ShipmentID    string
	Status        string
	OrderRevision int64
	ConfirmedAt   time.Time
}

// FulfilmentView is the fulfilment of an order (OrderFulfilment): the order as TRD shows it now,
// its lines with the shipped quantities derived from the confirmed shipment lines, and its
// shipments.
type FulfilmentView struct {
	Order       OrderBinding
	OrderStatus string
	Lines       []FulfilmentLine
	Shipments   []FulfilmentShipment
	CheckedAt   time.Time
}

// Commands is FUL's command entry point. Errors are *kernel.Rejection for refusals. An error that
// is application.ErrCommitUncertain from IDP-Bento-Go means the outcome is unknown: the caller sends
// the same command with the same idempotency key again, or reconciles it.
type Commands interface {
	PrepareShipment(ctx context.Context, cmd PrepareShipment) (ShipmentReceipt, error)
	PlanPacking(ctx context.Context, cmd PlanPacking) (ShipmentReceipt, error)
	RecordPacking(ctx context.Context, cmd RecordPacking) (ShipmentReceipt, error)
	CancelShipment(ctx context.Context, cmd CancelShipment) (ShipmentReceipt, error)
	ConfirmShipment(ctx context.Context, cmd ConfirmShipment) (ShipmentReceipt, error)
}

// Queries is FUL's audited read side.
type Queries interface {
	// GetShipment returns a shipment the caller sees, audited as shipment.get (ful.read): NOT_FOUND
	// otherwise.
	GetShipment(ctx context.Context, shipmentID string) (ShipmentView, error)
	// GetFulfilment returns the fulfilment of an order TRD shows the caller, audited as
	// order_fulfilment.get (ful.read): NOT_FOUND otherwise. Shipments outside the caller's data
	// scope count without their IDs.
	GetFulfilment(ctx context.Context, orderID string) (FulfilmentView, error)
	// Audit returns FUL's audit trail (ful.audit).
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
}

// Auditor records a request an inbound adapter refused before it became a command.
type Auditor interface {
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// Reconciliation is what became of a command whose answer the caller lost.
type Reconciliation struct {
	State    kernel.ReconcileState
	ActionID string
	ObjectID string
	Receipt  *Receipt
}

// Reconciler recovers from an unknown outcome without executing anything (contract 06, 动作对账).
// Reconciling needs ful.read, not the command's own permission.
type Reconciler interface {
	Reconcile(ctx context.Context, cmd Command) (Reconciliation, error)
}
