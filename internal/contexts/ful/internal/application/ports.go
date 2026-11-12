package application

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Orders is FUL's port to TRD (R27): ActiveOrderRef v1 before FUL's transaction, OrderGuard v1
// with FUL's executor inside it.
type Orders interface {
	ResolveActiveOrders(ctx context.Context, orderIDs []string) (map[string]trdapi.ActiveOrderFact, error)
	GuardOrder(ctx context.Context, exec bentopg.Executor, b trdapi.OrderBinding) (trdapi.OrderVerdict, error)
}

// Releases is FUL's port to QLT (R26): QualityReleaseRef v1 before FUL's transaction,
// QualityReleaseGuard v1 with FUL's executor inside it.
type Releases interface {
	ResolveReleases(ctx context.Context, releaseIDs []string, at time.Time) (map[qltapi.QualityReleaseRef]qltapi.ResolvedRelease, error)
	GuardRelease(ctx context.Context, exec bentopg.Executor, ref qltapi.QualityReleaseRef, at time.Time) (qltapi.QualityReleaseVerdict, error)
}

// Screening is FUL's port to PTY (R44): PartyScreeningGuard v1 with FUL's executor.
type Screening interface {
	GuardScreening(ctx context.Context, exec bentopg.Executor, partyID string, at time.Time) (ptyapi.ScreeningGuardResult, error)
}

// DeliveryLine is what PUR says of a supplier delivery line at a version (R59): the version's
// status, the version verified now, the purchase commitment line it is delivered against (its
// commitment, supplier, sales order line, SKU specification and unit), the verified quantity and
// Code — "" when the version is the verified one, else PUR's refusal (SUPPLIER_DELIVERY_UNVERIFIED,
// REFERENCE_STALE). Known is false in a guard verdict for a delivery, version or line PUR does not
// have.
type DeliveryLine struct {
	Ref              api.DeliveryLineRef
	Known            bool
	Status           string
	VerifiedVersion  int64
	CommitmentID     string
	SupplierID       string
	OrderID          string
	OrderLineID      string
	SkuID            string
	SpecRevision     int64
	UnitID           string
	UnitRevision     int64
	VerifiedQuantity string
	Code             string
}

// Deliveries is FUL's port to PUR's supplier deliveries (R59): ResolveDeliveryLines before FUL's
// transaction (only lines the caller reads), GuardDeliveryLine — SupplierDeliveryGuard v1,
// brant_pur.guard_supplier_delivery_v1(delivery_id, line_id, version), which locks the delivery
// FOR SHARE — with FUL's executor inside it.
type Deliveries interface {
	ResolveDeliveryLines(ctx context.Context, refs []api.DeliveryLineRef) (map[api.DeliveryLineRef]DeliveryLine, error)
	GuardDeliveryLine(ctx context.Context, exec bentopg.Executor, ref api.DeliveryLineRef) (DeliveryLine, error)
}

// ExtensionCall is what an extension guard is asked about: the shipment, the order it binds and
// the confirmation's instant.
type ExtensionCall struct {
	ShipmentID string
	Order      trdapi.OrderBinding
	At         time.Time
}

// Extension is a guard shipment.confirm calls with FUL's executor in its transaction: it returns
// the value the confirmation records as evidence, or the refusal.
type Extension func(ctx context.Context, exec bentopg.Executor, c ExtensionCall) (string, error)

// Extensions are the guards shipment.confirm names before S5 unit C1 wires them (plan S5-03,
// S5-07; D-11): CNX's TradeComplianceGuard and export licence guard, STL's PaymentConditionGuard
// and FUL's own commercial release. A nil one is recorded as not_wired and listed as unchecked,
// never treated as passed.
type Extensions struct {
	TradeCompliance   Extension
	ExportLicense     Extension
	PaymentCondition  Extension
	CommercialRelease Extension
}

func decimal(s string) kernel.Decimal {
	d, _ := api.Quantity(s)
	return d
}

// orderFact is what the domain judges of TRD's answer.
func orderFact(f trdapi.ActiveOrderFact) *shipment.OrderFact {
	o := &shipment.OrderFact{Binding: shipment.OrderBinding{OrderID: f.Binding.OrderID, OrderRevision: f.Binding.OrderRevision,
		OrderHash: f.Binding.OrderHash}, Code: f.Code, CustomerID: f.CustomerRef.PartyID, Customer: f.CustomerRef.Revision}
	for _, l := range f.Lines {
		q, ok := api.Quantity(l.Quantity)
		if !ok {
			// A line FUL cannot read never ships: the order is not intact for FUL.
			o.Code = kernel.CodeReferenceInvalid
		}
		o.Lines = append(o.Lines, shipment.OrderLine{LineID: l.LineID, SkuID: l.Sku.SkuID, SpecRevision: l.Sku.SpecRevision, UnitID: l.UnitRef.ID,
			UnitRevision: l.UnitRef.Revision, Quantity: q})
	}
	for _, a := range f.Terms.Addresses {
		o.Addresses = append(o.Addresses, shipment.Party{Role: a.Role, PartyID: a.PartyID, PartyRevision: a.PartyRevision, AddressID: a.AddressID})
	}
	return o
}

func releaseScope(s qltapi.ReleaseScope) shipment.Scope {
	return shipment.Scope{CommitmentID: s.Subject.ID, SupplierID: s.SupplierID, OrderID: s.OrderID, OrderLineID: s.OrderLineID, SkuID: s.SkuID,
		SpecRevision: s.SpecRevision, UnitID: s.UnitID, UnitRevision: s.UnitRevision}
}

func releaseFact(r qltapi.ResolvedRelease) shipment.ReleaseFact {
	return shipment.ReleaseFact{ID: r.Ref.ReleaseID, Code: r.Code, State: r.State, SubjectKind: r.Scope.Subject.Kind, Scope: releaseScope(r.Scope),
		Quantity: decimal(r.Quantity)}
}

func releaseVerdict(v qltapi.QualityReleaseVerdict) shipment.ReleaseVerdict {
	return shipment.ReleaseVerdict{Known: v.Known, Status: v.Status, Code: v.Code, SubjectKind: v.Scope.Subject.Kind, Scope: releaseScope(v.Scope),
		Quantity: decimal(v.Quantity), ValidUntil: v.ValidUntil}
}

func deliveryScope(l DeliveryLine) shipment.Scope {
	return shipment.Scope{CommitmentID: l.CommitmentID, SupplierID: l.SupplierID, OrderID: l.OrderID, OrderLineID: l.OrderLineID, SkuID: l.SkuID,
		SpecRevision: l.SpecRevision, UnitID: l.UnitID, UnitRevision: l.UnitRevision}
}

func deliveryFact(l DeliveryLine) shipment.DeliveryFact {
	return shipment.DeliveryFact{Ref: shipment.DeliveryRef{DeliveryID: l.Ref.DeliveryID, LineID: l.Ref.LineID}, Version: l.Ref.Version,
		Status: l.Status, Code: l.Code, Scope: deliveryScope(l), Verified: decimal(l.VerifiedQuantity)}
}

func deliveryVerdict(l DeliveryLine) shipment.DeliveryVerdict {
	return shipment.DeliveryVerdict{Known: l.Known, Status: l.Status, VerifiedVersion: l.VerifiedVersion, Scope: deliveryScope(l),
		Verified: decimal(l.VerifiedQuantity), Code: l.Code}
}

func orderVerdict(v trdapi.OrderVerdict) shipment.OrderVerdict {
	return shipment.OrderVerdict{Known: v.Known, Status: v.Status, OrderRevision: v.OrderRevision, Code: v.Code}
}

func binding(b shipment.OrderBinding) trdapi.OrderBinding {
	return trdapi.OrderBinding{OrderID: b.OrderID, OrderRevision: b.OrderRevision, OrderHash: b.OrderHash}
}
