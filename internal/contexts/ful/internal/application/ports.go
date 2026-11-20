package application

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Orders is FUL's port to TRD (R27): ActiveOrderRef v1 before FUL's transaction, OrderGuard v1
// with FUL's executor inside it, and TRD's order state read (brant_trd.order_state_v1, lock-free,
// no data scope) inside it after FUL's own locks — what the documents release of a confirmed
// shipment reads the order's current revision and hash from (S5 C1b). OrderRevisionRef v1 (S5R-D35)
// reads any committed revision by (order, revision) before FUL's transaction: the revision a shipment
// binds, its line prices, terms, Incoterm, addresses with countries and notify party, which a later
// amendment never rewrites.
type Orders interface {
	ResolveActiveOrders(ctx context.Context, orderIDs []string) (map[string]trdapi.ActiveOrderFact, error)
	ResolveOrderRevisions(ctx context.Context, keys []trdapi.OrderRevisionKey) (map[trdapi.OrderRevisionKey]trdapi.OrderRevisionFact, error)
	GuardOrder(ctx context.Context, exec bentopg.Executor, b trdapi.OrderBinding) (trdapi.OrderVerdict, error)
	OrderState(ctx context.Context, exec bentopg.Executor, orderID string) (trdapi.OrderState, error)
}

// boundRevision asks TRD's OrderRevisionRef v1 for the revision binding b names: the fact, or nil when
// TRD shows the caller no such revision, the revision is not intact (Code) or its order hash is not
// b's (a revision never changes: a different hash is not the revision the caller bound).
func (s Service) boundRevision(ctx context.Context, b trdapi.OrderBinding) (*trdapi.OrderRevisionFact, error) {
	key := trdapi.OrderRevisionKey{OrderID: b.OrderID, OrderRevision: b.OrderRevision}
	got, err := s.Upstream.Orders.ResolveOrderRevisions(ctx, []trdapi.OrderRevisionKey{key})
	if err != nil {
		return nil, err
	}
	f, ok := got[key]
	if !ok || f.Code != "" || f.Binding != b {
		return nil, nil
	}
	return &f, nil
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
// have. LineVerifiedQuantity and LineDisposedQuantity are what the commitment line adds up to now:
// its verified deliveries and its approved dispositions (SupplierDeliveryGuard v2, S5R-D05).
type DeliveryLine struct {
	Ref                  api.DeliveryLineRef
	Known                bool
	Status               string
	VerifiedVersion      int64
	CommitmentID         string
	SupplierID           string
	OrderID              string
	OrderLineID          string
	SkuID                string
	SpecRevision         int64
	UnitID               string
	UnitRevision         int64
	VerifiedQuantity     string
	LineVerifiedQuantity string
	LineDisposedQuantity string
	Code                 string
}

// Deliveries is FUL's port to PUR's supplier deliveries (R59): ResolveDeliveryLines before FUL's
// transaction (only lines the caller reads), GuardDeliveryLine — SupplierDeliveryGuard v2,
// brant_pur.guard_supplier_delivery_v2(delivery_id, line_id, version), which locks the line's
// commitment root FOR SHARE — with FUL's executor inside it.
type Deliveries interface {
	ResolveDeliveryLines(ctx context.Context, refs []api.DeliveryLineRef) (map[api.DeliveryLineRef]DeliveryLine, error)
	GuardDeliveryLine(ctx context.Context, exec bentopg.Executor, ref api.DeliveryLineRef) (DeliveryLine, error)
}

// Compliance is FUL's port to CNX (R56): TradeComplianceGuard v1 on an order revision, which locks
// the revision's check FOR SHARE, and ExportLicenseGuard v1 on a shipment, which locks the
// shipment's licence and allocation FOR SHARE, both with FUL's executor until FUL's transaction
// ends.
type Compliance interface {
	GuardTradeCompliance(ctx context.Context, exec bentopg.Executor, b cnxapi.OrderBinding, at time.Time) (cnxapi.TradeComplianceVerdict, error)
	GuardExportLicense(ctx context.Context, exec bentopg.Executor, shipmentID string, at time.Time) (cnxapi.ExportLicenseVerdict, error)
}

// Payments is FUL's port to STL (R45): PaymentConditionGuard v1 for a release scope, which locks the
// order's account FOR SHARE with FUL's executor until FUL's transaction ends.
type Payments interface {
	GuardPaymentCondition(ctx context.Context, exec bentopg.Executor, q stlapi.PaymentQuery) (stlapi.PaymentVerdict, error)
}

func decimal(s string) kernel.Decimal {
	d, _ := api.Quantity(s)
	return d
}

// orderFact is what the domain judges of TRD's answer: ActiveOrderRef v1 says whether the order
// serves new work at which binding; the lines, the customer's address roles and the notify party are
// those of that revision as OrderRevisionRef v1 reads it (rev; S5R-D35, D40) — nil when TRD did not
// show it, which leaves the order not intact for FUL (REFERENCE_INVALID) unless ActiveOrderRef v1
// already refused it.
func orderFact(f trdapi.ActiveOrderFact, rev *trdapi.OrderRevisionFact) *shipment.OrderFact {
	o := &shipment.OrderFact{Binding: shipment.OrderBinding{OrderID: f.Binding.OrderID, OrderRevision: f.Binding.OrderRevision,
		OrderHash: f.Binding.OrderHash}, Code: f.Code, CustomerID: f.CustomerRef.PartyID, Customer: f.CustomerRef.Revision}
	if o.Code != "" {
		return o
	}
	if rev == nil {
		o.Code = kernel.CodeReferenceInvalid
		return o
	}
	for _, l := range rev.Lines {
		q, ok := api.Quantity(l.Quantity)
		if !ok {
			// A line FUL cannot read never ships: the order is not intact for FUL.
			o.Code = kernel.CodeReferenceInvalid
		}
		o.Lines = append(o.Lines, shipment.OrderLine{LineID: l.LineID, SkuID: l.Sku.SkuID, SpecRevision: l.Sku.SpecRevision, UnitID: l.UnitRef.ID,
			UnitRevision: l.UnitRef.Revision, Quantity: q})
	}
	for _, a := range rev.Addresses {
		o.Addresses = append(o.Addresses, shipment.Party{Role: a.Role, PartyID: a.PartyID, PartyRevision: a.PartyRevision, AddressID: a.AddressID})
	}
	if n := rev.NotifyParty; n != nil {
		o.Notify = &shipment.Party{Role: shipment.RoleNotify, PartyID: n.PartyID, PartyRevision: n.PartyRevision, AddressID: n.AddressID}
	}
	return o
}

func releaseScope(s qltapi.ReleaseScope) shipment.Scope {
	return shipment.Scope{CommitmentID: s.Subject.ID, SupplierID: s.SupplierID, OrderID: s.OrderID, OrderLineID: s.OrderLineID, SkuID: s.SkuID,
		SpecRevision: s.SpecRevision, UnitID: s.UnitID, UnitRevision: s.UnitRevision}
}

func releaseFact(r qltapi.ResolvedRelease) shipment.ReleaseFact {
	return shipment.ReleaseFact{ID: r.Ref.ReleaseID, Code: r.Code, State: r.State, SubjectKind: r.Scope.Subject.Kind, Scope: releaseScope(r.Scope),
		Quantity: decimal(r.Quantity), Concession: r.Concession}
}

func releaseVerdict(v qltapi.QualityReleaseVerdict) shipment.ReleaseVerdict {
	return shipment.ReleaseVerdict{Known: v.Known, Status: v.Status, Code: v.Code, SubjectKind: v.Scope.Subject.Kind, Scope: releaseScope(v.Scope),
		Quantity: decimal(v.Quantity), ValidUntil: v.ValidUntil}
}

func deliveryScope(l DeliveryLine) shipment.Scope {
	return shipment.Scope{CommitmentID: l.CommitmentID, SupplierID: l.SupplierID, OrderID: l.OrderID, OrderLineID: l.OrderLineID, SkuID: l.SkuID,
		SpecRevision: l.SpecRevision, UnitID: l.UnitID, UnitRevision: l.UnitRevision}
}

// lineTotals is what PUR stated of the commitment line; a total FUL cannot read is zero, so nothing
// more ships of it.
func lineTotals(l DeliveryLine) shipment.CommitmentLineTotals {
	return shipment.CommitmentLineTotals{Verified: decimal(l.LineVerifiedQuantity), Disposed: decimal(l.LineDisposedQuantity)}
}

func deliveryFact(l DeliveryLine) shipment.DeliveryFact {
	return shipment.DeliveryFact{Ref: shipment.DeliveryRef{DeliveryID: l.Ref.DeliveryID, LineID: l.Ref.LineID}, Version: l.Ref.Version,
		Status: l.Status, Code: l.Code, Scope: deliveryScope(l), Verified: decimal(l.VerifiedQuantity), Line: lineTotals(l)}
}

func deliveryVerdict(l DeliveryLine) shipment.DeliveryVerdict {
	return shipment.DeliveryVerdict{Known: l.Known, Status: l.Status, VerifiedVersion: l.VerifiedVersion, Scope: deliveryScope(l),
		Verified: decimal(l.VerifiedQuantity), Line: lineTotals(l), Code: l.Code}
}

func orderVerdict(v trdapi.OrderVerdict) shipment.OrderVerdict {
	return shipment.OrderVerdict{Known: v.Known, Status: v.Status, OrderRevision: v.OrderRevision, ApprovalRevoked: v.ApprovalRevoked, Code: v.Code}
}

func binding(b shipment.OrderBinding) trdapi.OrderBinding {
	return trdapi.OrderBinding{OrderID: b.OrderID, OrderRevision: b.OrderRevision, OrderHash: b.OrderHash}
}
