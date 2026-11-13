package application

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ReleaseConsumption is FUL's ReleaseConsumption v1 as QLT reads it (Context Map R79 FUL→QLT,
// "OHS（事务内无锁读取）", guardLock false; decision S5R-D18; QLT-INV-002): what FUL's confirmed
// shipments shipped under quality release releaseID, one answer per unit, none when nothing shipped.
// quality_release.revoke calls it with QLT's own executor inside its own transaction, after it
// locked the release row FOR UPDATE; it takes no lock. It is consistent because FUL's
// shipment.confirm, the only command that adds to it, holds the release row FOR SHARE through
// QualityReleaseGuard v1 until it commits. QLT never calls FUL's transaction owner and needs no FUL
// module to be constructed (FUL is composed after QLT).
type ReleaseConsumption func(ctx context.Context, exec bentopg.Executor, releaseID string) ([]Consumed, error)

// Consumed is one answer of ReleaseConsumption v1: what the confirmed shipments shipped under a
// release in a unit (kernel.QuantityFormat), and how many confirmed shipments that came from.
type Consumed struct {
	UnitID       string
	UnitRevision int64
	Quantity     string
	Shipments    int64
}

// Agencies is QLT's port to PTY for external inspection agencies (Context Map R73 PTY→QLT, "PartyRef
// v1、ServiceRoleRef v1（inspection_agency：只需已核实，不要求航线；S5R-D36）", guardLock false). QLT asks
// it before its own transaction for the agency service role an inspection cites, at the instant of
// the inspection; nothing PTY holds is locked while QLT decides. The port says whether the role is
// an inspection agency's and whether it serves then (verified, the latest revision, its party
// confirmed in the revision it was verified against, the instant within its validity): QLT checks no
// route for an agency (S5R-D36).
//
// The adapter over PTY's current ServiceRoleRef v1 lookup is qlt.PTYAgencies; when PTY publishes the
// S5R-D36 service role kinds (unit R2), only that adapter follows. ResolveAgencies answers at most
// api.MaxLookup references; roles PTY does not show the caller (another company, outside its data
// scope, no such party, role or revision) are absent.
type Agencies interface {
	ResolveAgencies(ctx context.Context, refs []api.AgencyRef, at time.Time) (map[api.AgencyRef]Agency, error)
}

// Agency is what PTY says of an agency's service role at an instant: Kind is PTY's service role
// kind, InspectionAgency whether that kind is an inspection agency's, Code "" when the role serves
// at that instant and PTY's code otherwise (REFERENCE_STALE), with Stale saying why.
type Agency struct {
	Ref              api.AgencyRef
	Kind             string
	InspectionAgency bool
	Code             string
	Stale            string
}

// Check returns nil when the role serves and PTY's refusal at path otherwise.
func (a Agency) Check(path string) error {
	if a.Code == "" {
		return nil
	}
	reason := "Reference is " + a.Stale
	if c, ok := api.Category(a.Code); ok {
		return kernel.RejectAs(a.Code, c, path, reason)
	}
	return kernel.Reject(a.Code, path, reason)
}
