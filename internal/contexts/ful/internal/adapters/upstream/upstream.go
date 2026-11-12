// Package upstream implements FUL's ports over the api packages of TRD, QLT and PTY: their lookups
// before FUL's transaction and their guard functions, executed with FUL's own executor inside it.
package upstream

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
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

// PTY is FUL's port to PTY's party screening (R44): PartyScreeningGuard v1.
type PTY struct{}

// GuardScreening runs PartyScreeningGuard v1 at instant at, which locks the party's screening root,
// the party and the lists the conclusion used FOR SHARE until FUL's transaction ends.
func (PTY) GuardScreening(ctx context.Context, exec bentopg.Executor, partyID string, at time.Time) (ptyapi.ScreeningGuardResult, error) {
	var g ptyapi.ScreeningGuardResult
	err := exec.QueryRow(ctx, ptyapi.ScreeningGuardSQL, partyID, at).Scan(g.Targets()...)
	return g, err
}
