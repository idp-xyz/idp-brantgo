package api

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Bookings (WBS S5-08; tactical model FUL-AGG-Shipment entity BookingRecord; UL FUL-BookingRecord
// "订舱委托、承运人确认、船期与截关等出运前的业务事实", not the actual shipment or departure; Context Map
// R44 PTY->FUL, R74 EVD->FUL; SAP reference O2C-11; baseline INT-03 service roles). A booking is
// recorded on a shipment, in versions: a correction is a new version with its reason, and every
// version is kept.
//
//	booking.record   human or AI (merchandiser): records the booking of a prepared or confirmed
//	                 shipment with the forwarder or carrier that confirmed it — a PTY party's verified
//	                 service role of kind forwarder or carrier (ServiceRoleRef v1, current now) —
//	                 the booking number, vessel and voyage, the estimated departure (ETD), the cargo
//	                 cut-off, the container type and count, the ports of loading and discharge
//	                 (UN/LOCODE) and the confirmation's evidence (EVD sources, at least one).
//	                 expected_version is the booking version the shipment has (0: none yet); a
//	                 correction (expected_version >= 1) states its reason. The shipment's own row
//	                 version does not move: a booking changes nothing a confirmation is judged on.
//
// Request members in snake_case: booking.record {shipment_id, expected_version, service_role
// {party_id, service_role_id, revision}, booking_number, vessel, voyage, etd (YYYY-MM-DD), cut_off
// (UTC timestamp), container_type, container_count, port_of_loading, port_of_discharge,
// evidence_ids, reason}.
const (
	CommandRecordBooking = "booking.record"
	CommandGetBookings   = "booking.get"

	// StatusBooked is the receipt status of booking.record.
	StatusBooked = "booked"

	// The service roles a booking is confirmed by (PTY service role kinds).
	ServiceForwarder = "forwarder"
	ServiceCarrier   = "carrier"

	MaxBookingNumber  = 64
	MaxVessel         = 100
	MaxVoyage         = 32
	MaxContainerCount = 999
)

// ServiceRoleRef is PTY's ServiceRoleRef v1: one verified revision of a service role of a party.
type ServiceRoleRef struct {
	PartyID       string
	ServiceRoleID string
	Revision      int64
}

// RecordBooking records booking version ExpectedVersion+1 of shipment ShipmentID. Human or AI.
type RecordBooking struct {
	IdempotencyKey  string
	ShipmentID      string
	ExpectedVersion int64
	ServiceRole     ServiceRoleRef
	BookingNumber   string
	Vessel          string
	Voyage          string
	ETD             string // YYYY-MM-DD
	CutOff          string // UTC timestamp
	ContainerType   string
	ContainerCount  int64
	PortOfLoading   string
	PortOfDischarge string
	EvidenceIDs     []string
	Reason          string
}

func (RecordBooking) fulCommand() {}

// Line reports whether s is one line of 1 to max characters of valid UTF-8, not only white space,
// without control characters.
func Line(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s) && strings.TrimSpace(s) != "" && strings.IndexFunc(s, unicode.IsControl) < 0
}

// Block reports whether s is 1 to max characters of valid UTF-8 text, not only white space, in lines
// separated by "\n" without other control characters.
func Block(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s) && strings.TrimSpace(s) != "" &&
		strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' }) < 0
}

// Location reports whether s is a UN/LOCODE: a two-letter country and a three-character place
// ("CNSHA", PTY's location form).
func Location(s string) bool {
	if len(s) != 5 {
		return false
	}
	for i := 0; i < 5; i++ {
		c := s[i]
		upper := c >= 'A' && c <= 'Z'
		if !upper && (i < 2 || c < '2' || c > '9') {
			return false
		}
	}
	return true
}

// ContainerType reports whether s is a container type code: 2 to 10 upper-case letters and digits
// ("40HQ", "22G1").
func ContainerType(s string) bool {
	if len(s) < 2 || len(s) > 10 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// Validate checks the closed contract: a service role reference, the booking's facts in their
// forms, the cut-off not after the ETD's day (UTC), 1 to MaxEvidence sources of confirmation, and a
// reason for a correction.
func (c RecordBooking) Validate() error {
	if err := head(c.IdempotencyKey, c.ShipmentID); err != nil {
		return err
	}
	switch {
	case c.ExpectedVersion < 0 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case !kernel.ValidIdentifier(c.ServiceRole.PartyID):
		return schema("/args/service_role/party_id")
	case !kernel.ValidIdentifier(c.ServiceRole.ServiceRoleID):
		return schema("/args/service_role/service_role_id")
	case c.ServiceRole.Revision < 1 || c.ServiceRole.Revision > maxVersion:
		return schema("/args/service_role/revision")
	case !Line(c.BookingNumber, MaxBookingNumber):
		return schema("/args/booking_number")
	case !Line(c.Vessel, MaxVessel):
		return schema("/args/vessel")
	case !Line(c.Voyage, MaxVoyage):
		return schema("/args/voyage")
	case !ContainerType(c.ContainerType):
		return schema("/args/container_type")
	case c.ContainerCount < 1 || c.ContainerCount > MaxContainerCount:
		return schema("/args/container_count")
	case !Location(c.PortOfLoading):
		return schema("/args/port_of_loading")
	case !Location(c.PortOfDischarge):
		return schema("/args/port_of_discharge")
	}
	etd, ok := PlannedOn(c.ETD)
	if !ok {
		return schema("/args/etd")
	}
	cut, err := kernel.ParseTimestamp(c.CutOff)
	if err != nil {
		return schema("/args/cut_off")
	}
	if !cut.Before(etd.Add(24 * time.Hour)) {
		return invalid("/args/cut_off", "The cut-off is after the estimated departure")
	}
	if len(c.EvidenceIDs) == 0 {
		return kernel.Reject(kernel.CodeSchemaInvalid, "/args/evidence_ids", "A booking cites the evidence of its confirmation")
	}
	if err := evidence(c.EvidenceIDs, "/args/evidence_ids"); err != nil {
		return err
	}
	switch {
	case c.ExpectedVersion > 0 && c.Reason == "":
		return kernel.Reject(kernel.CodeSchemaInvalid, "/args/reason", "A correction states its reason")
	case c.Reason != "" && !Reason(c.Reason):
		return schema("/args/reason")
	}
	return nil
}

// BookingView is one version of a shipment's booking.
type BookingView struct {
	Version         int64
	ServiceRole     ServiceRoleRef
	ServiceKind     string
	BookingNumber   string
	Vessel          string
	Voyage          string
	ETD             string
	CutOff          time.Time
	ContainerType   string
	ContainerCount  int64
	PortOfLoading   string
	PortOfDischarge string
	EvidenceIDs     []string
	Reason          string
	RecordedBy      kernel.ActorRef
	RecordedAt      time.Time
	AuthorityEpoch  int64
}

// ShipmentBookingsView is a shipment's booking: the current version (nil: none) and every version,
// oldest first.
type ShipmentBookingsView struct {
	ShipmentID     string
	ShipmentStatus string
	Current        *BookingView
	Versions       []BookingView
}

// BookingCommands records bookings. Errors are *kernel.Rejection for refusals.
type BookingCommands interface {
	RecordBooking(ctx context.Context, cmd RecordBooking) (Receipt, error)
}

// BookingQueries reads bookings.
type BookingQueries interface {
	// GetBookings returns the booking of a shipment the caller sees, audited as booking.get
	// (ful.read): NOT_FOUND otherwise.
	GetBookings(ctx context.Context, shipmentID string) (ShipmentBookingsView, error)
}
