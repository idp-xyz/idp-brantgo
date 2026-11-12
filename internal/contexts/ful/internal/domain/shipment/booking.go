package shipment

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The service roles a booking is confirmed by (PTY service role kinds forwarder and carrier).
const (
	ServiceForwarder = "forwarder"
	ServiceCarrier   = "carrier"
)

// ServiceRoleRef names one verified revision of a PTY party's service role (ServiceRoleRef v1).
type ServiceRoleRef struct {
	PartyID       string
	ServiceRoleID string
	Revision      int64
}

// ServiceRoleFact is what PTY says of a service role revision now (ResolvedServiceRole): its kind
// and Code — "" when it serves new business now, else PTY's REFERENCE_STALE. A role the actor does
// not read is absent.
type ServiceRoleFact struct {
	Ref  ServiceRoleRef
	Role string
	Code string
}

// Booking is one version of a shipment's booking (tactical model BookingRecord, an entity of the
// Shipment; UL FUL-BookingRecord: 订舱委托、承运人确认、船期与截关等出运前的业务事实 — not the actual
// shipment or departure). Versions are numbered from 1 and only appended; a correction states its
// reason.
type Booking struct {
	Version         int64
	ServiceRole     ServiceRoleRef
	ServiceKind     string
	Number          string
	Vessel          string
	Voyage          string
	ETD             string // YYYY-MM-DD
	CutOff          time.Time
	ContainerType   string
	ContainerCount  int64
	PortOfLoading   string
	PortOfDischarge string
	Evidence        []string
	Reason          string
	Recorded        Act
}

// BookingRequest is a booking.record as the contract checked it.
type BookingRequest struct {
	Expected        int64
	ServiceRole     ServiceRoleRef
	Number          string
	Vessel          string
	Voyage          string
	ETD             string
	CutOff          time.Time
	ContainerType   string
	ContainerCount  int64
	PortOfLoading   string
	PortOfDischarge string
	Evidence        []string
	Reason          string
}

// RecordBooking decides booking.record on shipment s, whose current booking is current (nil: none),
// given what PTY said of the service roles the actor reads and the sources the actor reads (found).
// It refuses, in this order: a cancelled shipment (INVALID_STATE; a prepared or confirmed one takes
// its booking and corrections), another booking version than expected (VERSION_CONFLICT), a source
// of evidence the actor does not read (REFERENCE_INVALID), a service role the actor does not read
// (REFERENCE_INVALID), one PTY no longer serves now (REFERENCE_STALE), one that is neither a
// forwarder's nor a carrier's (INVALID_KNOWN_VALUE). The booking is the next version.
func (s *Shipment) RecordBooking(current *Booking, req BookingRequest, roles map[ServiceRoleRef]ServiceRoleFact, found map[string]bool,
	act Act) (*Booking, error) {
	if s.Status == StatusCancelled {
		return nil, Reject(kernel.CodeInvalidState, "/args/shipment_id", "The shipment is cancelled")
	}
	var at int64
	if current != nil {
		at = current.Version
	}
	if req.Expected != at {
		return nil, Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if err := Readable(req.Evidence, found, "/args/evidence_ids"); err != nil {
		return nil, err
	}
	role, ok := roles[req.ServiceRole]
	switch {
	case !ok:
		return nil, Reject(kernel.CodeReferenceInvalid, "/args/service_role", "No service role the actor reads")
	case role.Code != "":
		return nil, Upstream(role.Code, "/args/service_role", "The service role does not serve a booking now")
	case role.Role != ServiceForwarder && role.Role != ServiceCarrier:
		return nil, Reject(kernel.CodeInvalidKnownValue, "/args/service_role", "A booking is confirmed by a forwarder or a carrier, not a "+
			role.Role)
	}
	act.At = kernel.Instant(act.At)
	return &Booking{Version: at + 1, ServiceRole: req.ServiceRole, ServiceKind: role.Role, Number: req.Number, Vessel: req.Vessel,
		Voyage: req.Voyage, ETD: req.ETD, CutOff: kernel.Instant(req.CutOff), ContainerType: req.ContainerType, ContainerCount: req.ContainerCount,
		PortOfLoading: req.PortOfLoading, PortOfDischarge: req.PortOfDischarge, Evidence: slices.Clone(req.Evidence), Reason: req.Reason,
		Recorded: act}, nil
}
