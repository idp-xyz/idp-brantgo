package party

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/lifecycle"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5R-D36 (UL PTY-ServicePartyRole): a forwarder or a carrier serves a route; an insurer, an
// inspection agency and a customs broker are verified without one; the S1 kinds broker and
// inspection keep their route.
func TestS5RServiceRolesWithoutARoute(t *testing.T) {
	supplier := &Party{Record: lifecycle.Record{ID: "service", Status: lifecycle.Confirmed, RowVersion: 4, Revision: 2}, Roles: []Role{Supplier}}
	role := func(kind ServiceKind, origin, destination string) ServiceRole {
		return ServiceRole{ID: "r", Kind: kind, ServiceCode: "s", Origin: origin, Destination: destination,
			Validity: kernel.ValidityInterval{From: at.Add(-time.Hour), Until: at.Add(24 * time.Hour)}, EvidenceIDs: []string{"evidence"}}
	}
	for _, c := range []struct {
		role ServiceRole
		want string
	}{
		{role(ServiceInspectionAgency, "", ""), "OK"},
		{role(ServiceCustomsBroker, "", ""), "OK"},
		{role(ServiceInsurer, "", ""), "OK"},
		{role(ServiceInspectionAgency, "CNNGB", "CNNGB"), "OK"},
		{role(ServiceForwarder, "CNSHA", "DEHAM"), "OK"},
		{role(ServiceForwarder, "", "DEHAM"), "SCHEMA_INVALID /args/origin"},
		{role(ServiceCarrier, "CNSHA", ""), "SCHEMA_INVALID /args/origin"},
		{role(ServiceInspection, "", ""), "SCHEMA_INVALID /args/origin"},
		{role(ServiceBroker, "CNSHA", "CNSHA"), "OK"},
	} {
		if _, err := supplier.VerifyServiceRole(c.role, 0, at); code(err) != c.want {
			t.Errorf("%s %q-%q: %s, want %s", c.role.Kind, c.role.Origin, c.role.Destination, code(err), c.want)
		}
	}
	if AddressNotify != "notify" || !ServiceForwarder.NeedsRoute() || ServiceCustomsBroker.NeedsRoute() {
		t.Fatal("S5R-D36, D40 constants")
	}
}
