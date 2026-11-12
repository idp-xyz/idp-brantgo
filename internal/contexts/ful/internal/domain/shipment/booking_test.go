package shipment

import (
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// booking.record in the domain (UL FUL-BookingRecord; tactical model BookingRecord, an entity of
// the Shipment): versions from 1, a forwarder's or carrier's current service role, never on a
// cancelled shipment.
func TestRecordBooking(t *testing.T) {
	ref := ServiceRoleRef{PartyID: "forwarder.1", ServiceRoleID: "sea", Revision: 1}
	roles := map[ServiceRoleRef]ServiceRoleFact{ref: {Ref: ref, Role: ServiceForwarder}}
	found := map[string]bool{"evidence": true}
	at := time.Date(2026, 11, 12, 8, 0, 0, 1500, time.UTC)
	act := Act{By: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"}, Epoch: 2, At: at}
	req := BookingRequest{ServiceRole: ref, Number: "SHA1", Vessel: "V", Voyage: "1E", ETD: "2026-12-20", CutOff: at, ContainerType: "40HQ",
		ContainerCount: 1, PortOfLoading: "CNSHA", PortOfDischarge: "DEHAM", Evidence: []string{"evidence"}}
	s := &Shipment{ID: "shipment.1", Status: StatusPrepared}
	outcome := func(err error) string {
		if r, ok := kernel.AsRejection(err); ok {
			return r.Code + " " + r.Fields[0].Path
		}
		return "OK"
	}
	b, err := s.RecordBooking(nil, req, roles, found, act)
	if err != nil || b.Version != 1 || b.ServiceKind != ServiceForwarder || !b.Recorded.At.Equal(kernel.Instant(at)) || b.Recorded.By.ID != "agent.1" {
		t.Fatalf("%+v %v", b, err)
	}
	cases := []struct {
		edit func(*Shipment, *BookingRequest, map[ServiceRoleRef]ServiceRoleFact)
		want string
	}{
		{func(s *Shipment, _ *BookingRequest, _ map[ServiceRoleRef]ServiceRoleFact) { s.Status = StatusCancelled }, "INVALID_STATE /args/shipment_id"},
		{func(_ *Shipment, r *BookingRequest, _ map[ServiceRoleRef]ServiceRoleFact) { r.Expected = 0 }, "VERSION_CONFLICT /args/expected_version"},
		{func(_ *Shipment, r *BookingRequest, _ map[ServiceRoleRef]ServiceRoleFact) {
			r.Evidence = []string{"hidden"}
		},
			"REFERENCE_INVALID /args/evidence_ids/0"},
		{func(_ *Shipment, r *BookingRequest, _ map[ServiceRoleRef]ServiceRoleFact) { r.ServiceRole.Revision = 2 }, "REFERENCE_INVALID /args/service_role"},
		{func(_ *Shipment, _ *BookingRequest, m map[ServiceRoleRef]ServiceRoleFact) {
			m[ref] = ServiceRoleFact{Ref: ref, Role: ServiceCarrier, Code: CodeReferenceStale}
		}, "REFERENCE_STALE /args/service_role"},
		{func(_ *Shipment, _ *BookingRequest, m map[ServiceRoleRef]ServiceRoleFact) {
			m[ref] = ServiceRoleFact{Ref: ref, Role: "insurer"}
		},
			"INVALID_KNOWN_VALUE /args/service_role"},
		{func(_ *Shipment, _ *BookingRequest, m map[ServiceRoleRef]ServiceRoleFact) {
			m[ref] = ServiceRoleFact{Ref: ref, Role: ServiceCarrier}
		}, "OK"},
	}
	for i, c := range cases {
		sh, r, m := &Shipment{ID: "shipment.1", Status: StatusConfirmed}, req, map[ServiceRoleRef]ServiceRoleFact{ref: roles[ref]}
		r.Expected, r.Reason = 1, "correction"
		c.edit(sh, &r, m)
		got, err := sh.RecordBooking(b, r, m, found, act)
		if outcome(err) != c.want {
			t.Errorf("%d: %s, want %s", i, outcome(err), c.want)
		}
		if err == nil && (got.Version != 2 || got.Reason != "correction" || !strings.HasPrefix(got.Number, "SHA")) {
			t.Errorf("%d: %+v", i, got)
		}
	}
}
