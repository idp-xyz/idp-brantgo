// Package upstream implements FUL's ports over the api packages of TRD, QLT, PUR and PTY: their
// lookups before FUL's transaction and their guard functions, executed with FUL's own executor
// inside it.
package upstream

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/application"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
)

// TRD is FUL's port to TRD's sales orders (R27): ActiveOrderRef v1 and OrderGuard v1.
type TRD struct {
	Orders trdapi.ActiveOrderLookups
}

func (t TRD) ResolveActiveOrders(ctx context.Context, ids []string) (map[string]trdapi.ActiveOrderFact, error) {
	return t.Orders.ResolveActiveOrders(ctx, ids)
}

// GuardOrder runs OrderGuard v1, which locks the order row FOR SHARE until FUL's transaction ends.
func (TRD) GuardOrder(ctx context.Context, exec bentopg.Executor, b trdapi.OrderBinding) (trdapi.OrderVerdict, error) {
	return trdapi.ScanOrderGuard(b, exec.QueryRow(ctx, trdapi.OrderGuardSQL, trdapi.OrderGuardArgs(b)...).Scan)
}

// QLT is FUL's port to QLT's quality releases (R26): QualityReleaseRef v1 and QualityReleaseGuard v1.
type QLT struct {
	Releases qltapi.ReleaseLookups
}

func (q QLT) ResolveReleases(ctx context.Context, ids []string, at time.Time) (map[qltapi.QualityReleaseRef]qltapi.ResolvedRelease, error) {
	return q.Releases.ResolveReleases(ctx, ids, at)
}

// GuardRelease runs QualityReleaseGuard v1, which locks the release row FOR SHARE until FUL's
// transaction ends.
func (QLT) GuardRelease(ctx context.Context, exec bentopg.Executor, ref qltapi.QualityReleaseRef, at time.Time) (qltapi.QualityReleaseVerdict, error) {
	return qltapi.ScanQualityReleaseGuard(ref, exec.QueryRow(ctx, qltapi.QualityReleaseGuardSQL, qltapi.QualityReleaseGuardArgs(ref, at)...).Scan)
}

// PUR is FUL's port to PUR's supplier deliveries (R59): DeliveryLookups v1 before FUL's transaction
// and SupplierDeliveryGuard v1 inside it.
type PUR struct {
	Deliveries purapi.DeliveryLookups
}

func purRef(r fulapi.DeliveryLineRef) purapi.DeliveryLineRef {
	return purapi.DeliveryLineRef{DeliveryID: r.DeliveryID, LineID: r.LineID, Version: r.Version}
}

// ResolveDeliveryLines answers the delivery lines among refs the caller reads, with the commitment
// line each is delivered against; PUR's Code is passed on as PUR decides it.
func (p PUR) ResolveDeliveryLines(ctx context.Context, refs []fulapi.DeliveryLineRef) (map[fulapi.DeliveryLineRef]application.DeliveryLine, error) {
	out := map[fulapi.DeliveryLineRef]application.DeliveryLine{}
	if len(refs) == 0 {
		return out, nil
	}
	asked := make([]purapi.DeliveryLineRef, len(refs))
	for i, r := range refs {
		asked[i] = purRef(r)
	}
	got, err := p.Deliveries.ResolveDeliveryLines(ctx, asked)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		l, ok := got[purRef(r)]
		if !ok {
			continue
		}
		c := l.Line
		out[r] = application.DeliveryLine{Ref: r, Known: true, Status: l.Status, VerifiedVersion: l.VerifiedVersion, CommitmentID: c.Ref.CommitmentID,
			SupplierID: c.SupplierID, OrderID: c.OrderID, OrderLineID: c.Ref.OrderLineID, SkuID: c.SkuID, SpecRevision: c.SpecRevision,
			UnitID: c.UnitID, UnitRevision: c.UnitRevision, VerifiedQuantity: l.VerifiedQuantity, Code: l.Code}
	}
	return out, nil
}

// GuardDeliveryLine runs SupplierDeliveryGuard v1, which locks the delivery root FOR SHARE until
// FUL's transaction ends.
func (PUR) GuardDeliveryLine(ctx context.Context, exec bentopg.Executor, ref fulapi.DeliveryLineRef) (application.DeliveryLine, error) {
	r := purRef(ref)
	v, err := purapi.ScanSupplierDeliveryGuard(r, exec.QueryRow(ctx, purapi.SupplierDeliveryGuardSQL, purapi.SupplierDeliveryGuardArgs(r)...).Scan)
	if err != nil {
		return application.DeliveryLine{}, err
	}
	return application.DeliveryLine{Ref: ref, Known: v.Known, Status: v.Status, VerifiedVersion: v.VerifiedVersion, CommitmentID: v.CommitmentID,
		SupplierID: v.SupplierID, OrderID: v.OrderID, OrderLineID: v.OrderLineID, SkuID: v.SkuID, SpecRevision: v.SpecRevision, UnitID: v.UnitID,
		UnitRevision: v.UnitRevision, VerifiedQuantity: v.VerifiedQuantity, Code: v.Code}, nil
}

// PTY is FUL's port to PTY's party screening (R44): PartyScreeningGuard v1.
type PTY struct{}

// GuardScreening runs PartyScreeningGuard v1 at instant at, which locks the party's screening root,
// the party and the lists the conclusion used FOR SHARE until FUL's transaction ends.
func (PTY) GuardScreening(ctx context.Context, exec bentopg.Executor, partyID string, at time.Time) (ptyapi.ScreeningGuardResult, error) {
	var g ptyapi.ScreeningGuardResult
	err := exec.QueryRow(ctx, ptyapi.ScreeningGuardSQL, partyID, at).Scan(g.Targets()...)
	return g, err
}
