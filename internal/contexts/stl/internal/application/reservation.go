package application

import (
	"context"
	"fmt"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// releaseCredit runs credit.release (SM-STL-CREDIT-RESERVATION). A person or an AI agent releases a
// reserved reservation whose order is not active at its revision (订单未生效即放弃) or that expired;
// before the transaction it asks TRD whether the order is active. The system (the order-end
// consumer, PL-13, unit B6) also releases consumed reservations. Inside the transaction it locks
// the customer's credit root FOR UPDATE and updates the reservation row, which CreditGuard v1
// locks FOR SHARE: a guard holding it makes the release wait until TRD's transaction ends.
type releaseCredit struct {
	s           Service
	cmd         api.ReleaseCredit
	orderActive bool
	root        *cc.Root
	reservation *cc.Reservation
}

func (h *releaseCredit) Prepare(ctx context.Context) error {
	if p, err := identity.FromContext(ctx); err != nil || p.Actor.Kind == kernel.ActorSystem {
		return err
	}
	var r *cc.Reservation
	err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		r, err = postgres.LoadReservation(ctx, exec, scope(ctx), h.cmd.ReservationID)
		return err
	})
	if err != nil || r == nil || r.Status != cc.ReservationReserved {
		return err
	}
	orders, err := h.s.Upstream.Orders.ResolveOrders(ctx, []string{r.Binding.OrderID})
	if err != nil {
		return err
	}
	o, ok := orders[r.Binding.OrderID]
	if !ok {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/reservation_id", "TRD shows no order "+r.Binding.OrderID)
	}
	h.orderActive = o.Status == OrderActive && o.Binding.OrderRevision == r.Binding.OrderRevision
	return nil
}

func (h *releaseCredit) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.root, h.reservation = nil, nil
	var err error
	if h.root, err = lockSubject(ctx, exec, postgres.SubjectReservation, h.cmd.ReservationID); err != nil || h.root == nil {
		return err
	}
	h.reservation, err = postgres.LoadReservation(ctx, exec, scope(ctx), h.cmd.ReservationID)
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
	if d.Actor.Kind != kernel.ActorSystem && h.orderActive && r.State(now) == cc.ReservationReserved {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/reservation_id",
			fmt.Sprintf("Order %s is active at revision %d: its reservation is consumed, and released when the order ends", r.Binding.OrderID,
				r.Binding.OrderRevision))
	}
	from := r.Version
	if err := r.Release(cc.Act{By: d.Actor, Epoch: d.Epoch, At: now, Reason: h.cmd.Reason}); err != nil {
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

// ConsumeCredit marks a reservation consumed by its order's activation (credit.consume). System only.
func (s Service) ConsumeCredit(ctx context.Context, cmd api.ConsumeCredit) (api.ReservationReceipt, error) {
	h := &consumeCredit{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandConsume, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ReservationID}, cmd,
		cmd.Validate(), h)
	if err != nil {
		return api.ReservationReceipt{}, err
	}
	return s.reservationReceipt(ctx, res)
}
