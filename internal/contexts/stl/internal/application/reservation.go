package application

import (
	"context"
	"fmt"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// releaseCredit runs credit.release (SM-STL-CREDIT-RESERVATION). A person or an AI agent releases a
// reserved reservation whose order is not active at its revision (订单未生效即放弃, cause abandoned)
// or that expired (cause expired). The system also releases consumed reservations: the order-end
// consumer (PL-13, unit B6; cause order_ended) and, when a later order revision supersedes them, the
// amendment consumer (cause superseded, S5R-D08). Inside
// the transaction it locks the customer's credit root FOR UPDATE, then the reservation row FOR
// UPDATE — which CreditGuard v1 holds FOR SHARE while order.activate or order.amendment.apply runs,
// so the release waits until TRD's transaction ends — and only then reads TRD's order state
// (brant_trd.order_state_v1, lock-free; A6 gap 1, S5 B4): an activation or amendment that held the
// guard has committed, and the release sees the order it activated.
type releaseCredit struct {
	s           Service
	cmd         api.ReleaseCredit
	root        *cc.Root
	reservation *cc.Reservation
	order       trdapi.OrderState
}

func (h *releaseCredit) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.root, h.reservation, h.order = nil, nil, trdapi.OrderState{}
	var err error
	if h.root, err = lockSubject(ctx, exec, postgres.SubjectReservation, h.cmd.ReservationID); err != nil || h.root == nil {
		return err
	}
	sc := scope(ctx)
	tag, err := exec.Exec(ctx, `SELECT 1 FROM brant_stl.credit_reservation WHERE tenant_id = $1 AND company_id = $2 AND reservation_id = $3
		FOR UPDATE`, string(sc.Tenant), string(sc.Company), h.cmd.ReservationID)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if h.reservation, err = postgres.LoadReservation(ctx, exec, sc, h.cmd.ReservationID); err != nil || h.reservation == nil {
		return err
	}
	id := h.reservation.Binding.OrderID
	h.order, err = trdapi.ScanOrderState(id, exec.QueryRow(ctx, trdapi.OrderStateSQL, trdapi.OrderStateArgs(id)...).Scan)
	return err
}

func (h *releaseCredit) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, now := scope(ctx), kernel.Instant(h.s.Clock.Now())
	r := h.reservation
	if r == nil || !d.Sees(r.OwnerActor, r.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/reservation_id", "")
	}
	if r.Version != h.cmd.ExpectedVersion {
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	// An order active at the reservation's revision, or at a later one that stands on it (an
	// amendment that asked no more credit, D-9), keeps its reservation until the order ends.
	if d.Actor.Kind != kernel.ActorSystem && h.order.ActiveFrom(r.Binding.OrderRevision) && r.State(now) == cc.ReservationReserved {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/reservation_id",
			fmt.Sprintf("Order %s is active at revision %d: its reservation is consumed, and released when the order ends", r.Binding.OrderID,
				h.order.OrderRevision))
	}
	cause := h.cmd.Cause
	switch {
	case cause == "" && d.Actor.Kind == kernel.ActorSystem:
		cause = cc.CauseOrderEnded
	case cause == "" && r.State(now) == cc.StateExpired:
		cause = cc.CauseExpired
	case cause == "":
		cause = cc.CauseAbandoned
	case cause == cc.CauseExpired && r.State(now) != cc.StateExpired:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/cause", "The reservation has not expired")
	}
	from := r.Version
	if err := r.Release(cc.Act{By: d.Actor, Epoch: d.Epoch, At: now, Reason: h.cmd.Reason}, cause); err != nil {
		return action.Outcome{}, err
	}
	if err := h.s.bump(ctx, exec, h.root); err != nil {
		return action.Outcome{}, err
	}
	if err := postgres.UpdateReservation(ctx, exec, sc, r, from); err != nil {
		return action.Outcome{}, err
	}
	e := event(api.EventCreditReservationReleased, h.root, map[string]any{"reservation_id": r.ID, "reservation_version": r.Version,
		"order_id": r.Binding.OrderID, "order_revision": r.Binding.OrderRevision})
	return action.Outcome{ObjectID: r.ID, RowVersion: r.Version, Status: r.Status, Events: []action.Event{e}}, nil
}

// ReleaseCredit releases a reservation (credit.release).
func (s Service) ReleaseCredit(ctx context.Context, cmd api.ReleaseCredit) (api.ReservationReceipt, error) {
	h := &releaseCredit{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandRelease, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ReservationID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.ReservationReceipt{}, err
	}
	return s.reservationReceipt(ctx, res)
}

// consumeCredit runs credit.consume (system; PL-12, consumed by unit B6's OrderActivated consumer):
// it locks the customer's credit root FOR UPDATE and marks the reservation consumed for the order
// binding the activation bound, if the reservation was valid when the order was activated.
type consumeCredit struct {
	s           Service
	cmd         api.ConsumeCredit
	root        *cc.Root
	reservation *cc.Reservation
}

func (h *consumeCredit) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.root, h.reservation = nil, nil
	var err error
	if h.root, err = lockSubject(ctx, exec, postgres.SubjectReservation, h.cmd.ReservationID); err != nil || h.root == nil {
		return err
	}
	h.reservation, err = postgres.LoadReservation(ctx, exec, scope(ctx), h.cmd.ReservationID)
	return err
}

func (h *consumeCredit) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, now := scope(ctx), h.cmd, kernel.Instant(h.s.Clock.Now())
	r := h.reservation
	if r == nil || !d.Sees(r.OwnerActor, r.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/reservation_id", "")
	}
	activated, _ := kernel.ParseTimestamp(c.ActivatedAt)
	from := r.Version
	if err := r.Consume(cc.Binding{OrderID: c.OrderID, OrderRevision: c.OrderRevision, OrderHash: c.OrderHash}, activated,
		cc.Act{By: d.Actor, Epoch: d.Epoch, At: now}); err != nil {
		return action.Outcome{}, err
	}
	if err := h.s.bump(ctx, exec, h.root); err != nil {
		return action.Outcome{}, err
	}
	if err := postgres.UpdateReservation(ctx, exec, sc, r, from); err != nil {
		return action.Outcome{}, err
	}
	e := event(api.EventCreditReservationConsumed, h.root, map[string]any{"reservation_id": r.ID, "reservation_version": r.Version,
		"order_id": r.Binding.OrderID, "order_revision": r.Binding.OrderRevision, "order_hash": r.Binding.OrderHash})
	return action.Outcome{ObjectID: r.ID, RowVersion: r.Version, Status: r.Status, Events: []action.Event{e}}, nil
}

// ConsumeCredit marks a reservation consumed by its order's activation, or carries an earlier
// revision's reservation over to an amended one (carried_from, S5R-D08) (credit.consume). System only.
func (s Service) ConsumeCredit(ctx context.Context, cmd api.ConsumeCredit) (api.ReservationReceipt, error) {
	var h action.Handler = &consumeCredit{s: s, cmd: cmd}
	if cmd.CarriedFrom != "" {
		h = &carryCredit{s: s, cmd: cmd}
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandConsume, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ReservationID}, cmd,
		cmd.Validate(), h)
	if err != nil {
		return api.ReservationReceipt{}, err
	}
	return s.reservationReceipt(ctx, res)
}
