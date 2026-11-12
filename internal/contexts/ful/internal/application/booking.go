package application

import (
	"context"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// bookingCommand runs booking.record (tactical model FUL-AGG-Shipment entity BookingRecord; UL
// FUL-BookingRecord; SAP reference O2C-11). Before the transaction it asks EVD which cited sources
// the actor reads and PTY what the service role is now (ServiceRoleRef v1, R44). Inside it, it locks
// the shipment FOR UPDATE — the booking is an entity of the Shipment aggregate — and reads its
// current booking; the shipment's row version does not move.
type bookingCommand struct {
	s       Service
	cmd     api.RecordBooking
	req     shipment.BookingRequest
	sources map[string]bool
	roles   map[shipment.ServiceRoleRef]shipment.ServiceRoleFact
	ship    *shipment.Shipment
	current *shipment.Booking
}

func (h *bookingCommand) Prepare(ctx context.Context) error {
	var err error
	if h.sources, err = h.s.sources(ctx, h.cmd.EvidenceIDs); err != nil {
		return err
	}
	ref := ptyapi.ServiceRoleRef{PartyID: h.cmd.ServiceRole.PartyID, ServiceRoleID: h.cmd.ServiceRole.ServiceRoleID, Revision: h.cmd.ServiceRole.Revision}
	got, err := h.s.Upstream.Documents.Parties.ResolveServiceRoles(ctx, []ptyapi.ServiceRoleRef{ref}, h.s.Clock.Now())
	if err != nil {
		return err
	}
	h.roles = map[shipment.ServiceRoleRef]shipment.ServiceRoleFact{}
	if r, ok := got[ref]; ok {
		key := shipment.ServiceRoleRef(ref)
		h.roles[key] = shipment.ServiceRoleFact{Ref: key, Role: r.Role, Code: r.Code}
	}
	return nil
}

func (h *bookingCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.ship, h.current = nil, nil
	var err error
	if h.ship, err = postgres.LoadShipment(ctx, exec, scope(ctx), h.cmd.ShipmentID, postgres.ForUpdate); err != nil || h.ship == nil {
		return err
	}
	h.current, err = postgres.CurrentBooking(ctx, exec, scope(ctx), h.ship.ID)
	return err
}

func (h *bookingCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	s := h.ship
	if s == nil || !d.Sees(s.OwnerActor, s.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/shipment_id", "")
	}
	b, err := s.RecordBooking(h.current, h.req, h.roles, h.sources, shipment.Act{By: d.Actor, Epoch: d.Epoch, At: h.s.Clock.Now()})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertBooking(ctx, exec, scope(ctx), s.ID, b); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: s.ID, RowVersion: b.Version, Status: api.StatusBooked}, nil
}

// RecordBooking records a booking version of a prepared or confirmed shipment (booking.record).
func (s Service) RecordBooking(ctx context.Context, cmd api.RecordBooking) (api.Receipt, error) {
	h := &bookingCommand{s: s, cmd: cmd}
	err := cmd.Validate()
	if err == nil {
		cut, _ := kernel.ParseTimestamp(cmd.CutOff)
		h.req = shipment.BookingRequest{Expected: cmd.ExpectedVersion, ServiceRole: shipment.ServiceRoleRef(cmd.ServiceRole), Number: cmd.BookingNumber,
			Vessel: cmd.Vessel, Voyage: cmd.Voyage, ETD: cmd.ETD, CutOff: cut, ContainerType: cmd.ContainerType, ContainerCount: cmd.ContainerCount,
			PortOfLoading: cmd.PortOfLoading, PortOfDischarge: cmd.PortOfDischarge, Evidence: slices.Clone(cmd.EvidenceIDs), Reason: cmd.Reason}
	}
	return s.run(ctx, action.Command{Name: api.CommandRecordBooking, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ShipmentID,
		Reason: cmd.Reason}, cmd, err, h)
}

func bookingView(b *shipment.Booking) api.BookingView {
	return api.BookingView{Version: b.Version, ServiceRole: api.ServiceRoleRef(b.ServiceRole), ServiceKind: b.ServiceKind, BookingNumber: b.Number,
		Vessel: b.Vessel, Voyage: b.Voyage, ETD: b.ETD, CutOff: b.CutOff, ContainerType: b.ContainerType, ContainerCount: b.ContainerCount,
		PortOfLoading: b.PortOfLoading, PortOfDischarge: b.PortOfDischarge, EvidenceIDs: slices.Clone(b.Evidence), Reason: b.Reason,
		RecordedBy: b.Recorded.By, RecordedAt: b.Recorded.At, AuthorityEpoch: b.Recorded.Epoch}
}

// GetBookings returns the booking of a shipment the caller sees, audited as booking.get (ful.read):
// NOT_FOUND otherwise.
func (s Service) GetBookings(ctx context.Context, id string) (api.ShipmentBookingsView, error) {
	if !kernel.ValidIdentifier(id) {
		return api.ShipmentBookingsView{}, invalidID(s, ctx, api.CommandGetBookings)
	}
	var v api.ShipmentBookingsView
	err := s.Ledger.Read(ctx, api.CommandGetBookings, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sh, err := postgres.LoadShipment(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if sh == nil || !d.Sees(sh.OwnerActor, sh.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		all, err := postgres.LoadBookings(ctx, exec, scope(ctx), id)
		if err != nil {
			return err
		}
		v = api.ShipmentBookingsView{ShipmentID: sh.ID, ShipmentStatus: sh.Status, Versions: []api.BookingView{}}
		for _, b := range all {
			v.Versions = append(v.Versions, bookingView(b))
		}
		if n := len(v.Versions); n > 0 {
			c := v.Versions[n-1]
			v.Current = &c
		}
		return nil
	})
	return v, err
}
