package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Carrying a credit reservation over to an amended order revision (S5R-D08; SM-STL-CREDIT-RESERVATION
// "承接", STL-INV-004 "订单修订生效时占用调整为新修订的信用部分"). When TRD's OrderAmended names no reservation
// for the new revision — it asks no more credit than the revision it amends (not_increased) — the
// order's credit follows the new revision anyway: credit.consume with carried_from writes a new
// reservation of the new revision, consumed by the system at the instant the amendment took effect,
// basis carried_over, for the new revision's credit portion under the credit policy and at the credit
// rate of the reservation it carries, which holds at least as much (no limit check); the consumer then
// releases the earlier reservation with the cause superseded. The customer's exposure falls with the
// order (SIM-02: 9,200.00 → 8,280.00).
//
// The new revision's payment stages come from TRD (OrderCommercialSnapshot v1, R32) and the policy's
// documents_secured and the rate's value from POL (R33), all read before STL's transaction; inside it,
// STL decides on its own rows under the customer's credit root lock. A published POL revision never
// changes (its content hash is checked against the one the reservation recorded), so no POL guard is
// needed for revisions the reservation already decided by.

// CreditBasis is what moving an order's credit to an amended revision needs from STL's upstreams, read
// before the transaction: TRD's order now (nil when TRD shows none) and the POL revisions — credit
// policies and credit rates — the order's held reservations were made under.
type CreditBasis struct {
	Order   *Order
	Configs map[polapi.ConfigRef]polapi.ResolvedConfig
}

// creditBasis reads the CreditBasis of order orderID: TRD's order, the order's held reservations (STL,
// without a lock) and the POL revisions they name.
func (s Service) creditBasis(ctx context.Context, orderID string) (CreditBasis, error) {
	var b CreditBasis
	orders, err := s.Upstream.Orders.ResolveOrders(ctx, []string{orderID})
	if err != nil {
		return b, err
	}
	if o, ok := orders[orderID]; ok {
		b.Order = &o
	}
	var held []cc.Reservation
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		held, err = postgres.LoadHeldReservations(ctx, exec, scope(ctx), orderID)
		return err
	})
	if err != nil {
		return b, err
	}
	var refs []polapi.ConfigRef
	for _, r := range held {
		for _, ref := range configsOf(&r) {
			if !slices.Contains(refs, ref) {
				refs = append(refs, ref)
			}
		}
	}
	if len(refs) > 0 {
		b.Configs, err = s.Upstream.Configs.ResolveConfigs(ctx, refs, s.Clock.Now())
	}
	return b, err
}

// configsOf returns the POL revisions reservation r was made under: its credit policy and its rate.
func configsOf(r *cc.Reservation) []polapi.ConfigRef {
	out := []polapi.ConfigRef{{Kind: polapi.KindCreditPolicy, ConfigID: r.Policy.ID, Revision: r.Policy.Revision}}
	if r.FX != nil {
		out = append(out, polapi.ConfigRef{Kind: polapi.KindFXRate, ConfigID: r.FX.ID, Revision: r.FX.Revision})
	}
	return out
}

// errBasisChanged is an error to retry: a reservation the transaction found was made under a POL
// revision the basis read before it does not hold (it appeared after the basis was read).
var errBasisChanged = fmt.Errorf("stl: the order's reservations changed since their POL revisions were read")

// need returns what revision b of the order needs on credit under reservation r's credit policy: the
// credit portion and the documents-secured amount in the order currency (Portion, D-5) and the portion
// in the limit currency at r's credit rate. known is false when TRD does not show the order active and
// intact at b now (the snapshot holds the current revision and those its amendments propose).
func (cb CreditBasis) need(r *cc.Reservation, b cc.Binding) (portion, secured, exposure kernel.Money, known bool, err error) {
	o := cb.Order
	if o == nil || o.Code != "" || o.Status != OrderActive {
		return portion, secured, exposure, false, nil
	}
	stages, _, ok := o.revision(OrderBinding(b))
	if !ok {
		return portion, secured, exposure, false, nil
	}
	cur := r.Amount.Currency()
	policy, ok := cb.Configs[polapi.ConfigRef{Kind: polapi.KindCreditPolicy, ConfigID: r.Policy.ID, Revision: r.Policy.Revision}]
	switch {
	case !ok:
		return portion, secured, exposure, false, errBasisChanged
	case policy.ContentHash != r.Policy.Hash || policy.Data.CreditPolicy == nil:
		return portion, secured, exposure, false, kernel.Reject(kernel.CodeReferenceInvalid, "/args/carried_from",
			fmt.Sprintf("POL's credit policy %s revision %d is not the one reservation %s was made under", r.Policy.ID, r.Policy.Revision, r.ID))
	}
	in := make([]cc.Stage, 0, len(stages))
	for _, s := range stages {
		m, err := kernel.ParseMinor(cur, s.AmountMinor)
		if err != nil {
			return portion, secured, exposure, false, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id",
				"TRD's payment stage "+s.StageID+" has no amount in "+cur.Code())
		}
		in = append(in, cc.Stage{StageID: s.StageID, Trigger: s.Trigger, ReleaseControl: s.ReleaseControl, Amount: m})
	}
	if portion, secured, err = cc.Portion(cur, in, policy.Data.CreditPolicy.DocumentsSecured); err != nil {
		return portion, secured, exposure, false, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", err.Error())
	}
	if r.FX == nil {
		return portion, secured, portion, true, nil
	}
	rate, ok := cb.Configs[polapi.ConfigRef{Kind: polapi.KindFXRate, ConfigID: r.FX.ID, Revision: r.FX.Revision}]
	switch {
	case !ok:
		return portion, secured, exposure, false, errBasisChanged
	case rate.ContentHash != r.FX.Hash || rate.Data.FXRate == nil:
		return portion, secured, exposure, false, kernel.Reject(kernel.CodeReferenceInvalid, "/args/carried_from",
			fmt.Sprintf("POL's fx_rate %s revision %d is not the one reservation %s was made at", r.FX.ID, r.FX.Revision, r.ID))
	}
	value, err := kernel.ParseDecimal(rate.Data.FXRate.Rate, kernel.FactorFormat)
	if err != nil {
		return portion, secured, exposure, false, err
	}
	x, err := kernel.NewExchangeRate(cur, r.Exposure.Currency(), value)
	if err != nil {
		return portion, secured, exposure, false, err
	}
	exposure, err = x.Convert(portion)
	return portion, secured, exposure, err == nil, err
}

// CarriedID is the ID of the reservation the consumer carries over for OrderAmended eventID: the same
// for every delivery of the event.
func CarriedID(eventID string) string {
	sum := sha256.Sum256([]byte("carried\x00" + eventID))
	return "carried." + hex.EncodeToString(sum[:16])
}

// carryCredit runs credit.consume with carried_from (S5R-D08): before the transaction it reads the
// CreditBasis of the order (unless the consumer read it already); inside it, it locks the customer's
// credit root FOR UPDATE, reads the reservation it carries and the order's held reservations under
// that lock, and writes the carried-over reservation consumed.
type carryCredit struct {
	s     Service
	cmd   api.ConsumeCredit
	basis CreditBasis
	root  *cc.Root
	from  *cc.Reservation
	taken *cc.Reservation
	held  []cc.Reservation
}

func (h *carryCredit) Prepare(ctx context.Context) error {
	var err error
	h.basis, err = h.s.creditBasis(ctx, h.cmd.OrderID)
	return err
}

func (h *carryCredit) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.root, h.from, h.taken, h.held = nil, nil, nil, nil
	var err error
	if h.root, err = lockSubject(ctx, exec, postgres.SubjectReservation, h.cmd.CarriedFrom); err != nil || h.root == nil {
		return err
	}
	sc := scope(ctx)
	if h.from, err = postgres.LoadReservation(ctx, exec, sc, h.cmd.CarriedFrom); err != nil {
		return err
	}
	if h.taken, err = postgres.LoadReservation(ctx, exec, sc, h.cmd.ReservationID); err != nil {
		return err
	}
	h.held, err = postgres.LoadHeldReservations(ctx, exec, sc, h.cmd.OrderID)
	return err
}

func (h *carryCredit) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	c, f, now := h.cmd, h.from, kernel.Instant(h.s.Clock.Now())
	b := cc.Binding{OrderID: c.OrderID, OrderRevision: c.OrderRevision, OrderHash: c.OrderHash}
	switch {
	case f == nil || !d.Sees(f.OwnerActor, f.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/carried_from", "")
	case h.taken != nil:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/reservation_id", "The reservation ID is taken")
	}
	for _, x := range h.held {
		if x.Binding.OrderRevision == c.OrderRevision {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/order_revision",
				fmt.Sprintf("Order revision %d holds reservation %s", c.OrderRevision, x.ID))
		}
	}
	switch o := h.basis.Order; {
	case o == nil:
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/order_id", "")
	case o.Code != "":
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "TRD's order revision is not intact")
	case o.Status != OrderActive:
		return action.Outcome{}, api.Reject(api.CodeOrderNotActive, "/args/order_id", "The order is "+o.Status)
	}
	portion, secured, exposure, known, err := h.basis.need(f, b)
	if err != nil {
		return action.Outcome{}, err
	}
	if !known {
		return action.Outcome{}, api.Reject(api.CodeReferenceStale, "/args/order_revision", "TRD's order is not at this revision and hash")
	}
	applied, _ := kernel.ParseTimestamp(c.ActivatedAt)
	r, err := cc.CarryOver(cc.Carry{ID: c.ReservationID, From: f, Binding: b, Portion: portion, Secured: secured, Exposure: exposure,
		AppliedAt: applied, By: cc.Act{By: d.Actor, Epoch: d.Epoch, At: now}})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = h.s.bump(ctx, exec, h.root); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertReservation(ctx, exec, scope(ctx), r); err != nil {
		return action.Outcome{}, err
	}
	e := event(api.EventCreditReservationConsumed, h.root, map[string]any{"reservation_id": r.ID, "reservation_version": r.Version,
		"order_id": r.Binding.OrderID, "order_revision": r.Binding.OrderRevision, "order_hash": r.Binding.OrderHash})
	return action.Outcome{ObjectID: r.ID, RowVersion: r.Version, Status: r.Status, Events: []action.Event{e}}, nil
}
