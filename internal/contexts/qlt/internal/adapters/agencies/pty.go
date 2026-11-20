// Package agencies implements QLT's port to PTY for external inspection agencies (Context Map R73
// PTY→QLT, ServiceRoleRef v1; decision S5R-D36) over PTY's published lookup ResolveServiceRoles.
//
// S5R-D36 names the service role kind inspection_agency, verified and needing no route; PTY's
// ServiceRoleRef v1 with those kinds is unit R2's. Until it is in this tree, PTY's service role kind
// of an inspection agency is ptyapi.ServiceInspection ("inspection"). The adapter accepts both, so it
// serves before and after R2's change; QLT's rules see only whether the role is an inspection
// agency's and whether it serves (QLT checks no route).
package agencies

import (
	"context"
	"time"

	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/application"
)

// KindInspectionAgency is S5R-D36's service role kind of an inspection agency.
const KindInspectionAgency = "inspection_agency"

// ServiceRoles is PTY's ServiceRoleRef v1 lookup.
type ServiceRoles interface {
	ResolveServiceRoles(ctx context.Context, refs []ptyapi.ServiceRoleRef, at time.Time) (map[ptyapi.ServiceRoleRef]ptyapi.ResolvedServiceRole, error)
}

// PTY answers QLT's agencies from PTY's ServiceRoleRef v1.
type PTY struct{ Roles ServiceRoles }

var _ application.Agencies = PTY{}

// InspectionAgency reports whether kind is the service role kind of an inspection agency.
func InspectionAgency(kind string) bool {
	return kind == KindInspectionAgency || kind == ptyapi.ServiceInspection
}

// ResolveAgencies asks PTY for the service roles refs name at instant at; roles PTY does not show the
// caller are absent.
func (p PTY) ResolveAgencies(ctx context.Context, refs []api.AgencyRef, at time.Time) (map[api.AgencyRef]application.Agency, error) {
	out := map[api.AgencyRef]application.Agency{}
	if len(refs) == 0 {
		return out, nil
	}
	asked := make([]ptyapi.ServiceRoleRef, len(refs))
	for i, r := range refs {
		asked[i] = ptyapi.ServiceRoleRef{PartyID: r.PartyID, ServiceRoleID: r.ServiceRoleID, Revision: r.Revision}
	}
	roles, err := p.Roles.ResolveServiceRoles(ctx, asked, at)
	if err != nil {
		return nil, err
	}
	for ref, r := range roles {
		a := api.AgencyRef{PartyID: ref.PartyID, ServiceRoleID: ref.ServiceRoleID, Revision: ref.Revision}
		out[a] = application.Agency{Ref: a, Kind: r.Role, InspectionAgency: InspectionAgency(r.Role), Code: r.Code, Stale: r.Stale}
	}
	return out, nil
}
