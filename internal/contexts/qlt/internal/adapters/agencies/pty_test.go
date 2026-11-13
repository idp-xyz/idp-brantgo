package agencies

import (
	"context"
	"errors"
	"testing"
	"time"

	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

type roles struct {
	at   time.Time
	have map[ptyapi.ServiceRoleRef]ptyapi.ResolvedServiceRole
	err  error
}

func (r *roles) ResolveServiceRoles(_ context.Context, refs []ptyapi.ServiceRoleRef, at time.Time) (map[ptyapi.ServiceRoleRef]ptyapi.ResolvedServiceRole, error) {
	r.at = at
	out := map[ptyapi.ServiceRoleRef]ptyapi.ResolvedServiceRole{}
	for _, ref := range refs {
		if v, ok := r.have[ref]; ok {
			out[ref] = v
		}
	}
	return out, r.err
}

// S5R-D36 behind QLT's port: a service role of kind inspection (PTY today) or inspection_agency
// (S5R-D36) is an inspection agency's, whatever its route; another kind is not; PTY's verdict passes
// through; roles PTY does not show are absent.
func TestInspectionAgenciesS5RD36(t *testing.T) {
	at := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	ref := func(id string) ptyapi.ServiceRoleRef {
		return ptyapi.ServiceRoleRef{PartyID: "agency", ServiceRoleID: id, Revision: 1}
	}
	r := &roles{have: map[ptyapi.ServiceRoleRef]ptyapi.ResolvedServiceRole{
		ref("today"):     {Ref: ref("today"), Role: ptyapi.ServiceInspection, Origin: "CNSHA", Destination: "CNSHA"},
		ref("d36"):       {Ref: ref("d36"), Role: KindInspectionAgency},
		ref("forwarder"): {Ref: ref("forwarder"), Role: ptyapi.ServiceForwarder, Origin: "CNSHA", Destination: "USLAX"},
		ref("stale"):     {Ref: ref("stale"), Role: KindInspectionAgency, Code: api.CodeReferenceStale, Stale: ptyapi.StaleSuperseded},
	}}
	asked := []api.AgencyRef{{PartyID: "agency", ServiceRoleID: "today", Revision: 1}, {PartyID: "agency", ServiceRoleID: "d36", Revision: 1},
		{PartyID: "agency", ServiceRoleID: "forwarder", Revision: 1}, {PartyID: "agency", ServiceRoleID: "stale", Revision: 1},
		{PartyID: "agency", ServiceRoleID: "unknown", Revision: 1}}
	got, err := PTY{Roles: r}.ResolveAgencies(context.Background(), asked, at)
	if err != nil || len(got) != 4 || !r.at.Equal(at) {
		t.Fatalf("%v %v", got, err)
	}
	for _, c := range []struct {
		id         string
		inspection bool
		check      string
	}{{"today", true, ""}, {"d36", true, ""}, {"forwarder", false, ""}, {"stale", true, api.CodeReferenceStale}} {
		a := got[api.AgencyRef{PartyID: "agency", ServiceRoleID: c.id, Revision: 1}]
		err := a.Check("/args/inspector/agency")
		code := ""
		if rej, ok := kernel.AsRejection(err); ok && rej.Fields[0].Path == "/args/inspector/agency" && rej.Category == kernel.CategoryConflict {
			code = rej.Code
		}
		if a.InspectionAgency != c.inspection || code != c.check || a.Ref.ServiceRoleID != c.id {
			t.Errorf("%s: %+v %v", c.id, a, err)
		}
	}
	if got, err = (PTY{Roles: r}).ResolveAgencies(context.Background(), nil, at); err != nil || len(got) != 0 {
		t.Fatal("nothing asked")
	}
	boom := errors.New("pty")
	if _, err = (PTY{Roles: &roles{err: boom}}).ResolveAgencies(context.Background(), asked[:1], at); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}
