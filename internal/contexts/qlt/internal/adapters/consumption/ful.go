// Package consumption implements QLT's reverse read of FUL (application.ReleaseConsumption; S5R
// unit R5) on the contract FUL publishes: ReleaseConsumption v1 (Context Map R79 FUL→QLT, "OHS（事务内
// 无锁读取）", guardLock false; decision S5R-D18), brant_ful.release_consumption_v1, what FUL's
// confirmed shipments shipped under a quality release. It is a SECURITY DEFINER function run with
// QLT's own executor inside quality_release.revoke's transaction, after the release row is locked
// FOR UPDATE; it takes no lock (the S5-D33 pattern). QLT never calls FUL's transaction owner and needs
// no FUL module to be constructed (FUL is composed after QLT).
package consumption

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/application"
)

var _ application.ReleaseConsumption = FULReleases

// FULReleases reads FUL's ReleaseConsumption v1 of release releaseID with exec: one answer per unit
// of the confirmed shipment lines under it, none when nothing shipped.
func FULReleases(ctx context.Context, exec bentopg.Executor, releaseID string) ([]application.Consumed, error) {
	rows, err := exec.Query(ctx, fulapi.ReleaseConsumptionSQL, fulapi.ReleaseConsumptionArgs(releaseID)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []application.Consumed
	for rows.Next() {
		c, err := fulapi.ScanReleaseConsumption(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, application.Consumed{UnitID: c.UnitID, UnitRevision: c.UnitRevision, Quantity: c.Quantity, Shipments: c.Shipments})
	}
	return out, rows.Err()
}
