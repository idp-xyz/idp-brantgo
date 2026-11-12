package application

import (
	"context"
	"strings"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
)

// The end of an order (SM-TRD-ORDER; TRD-INV-019, TD-AT02, ES-09 step 6): order.cancel of a draft
// (package order) or of an active order with nothing shipped and nothing open downstream,
// order.close_remainder of an active order that shipped something, and order.close of one that
// shipped everything (human only). Each locks the quote root FOR UPDATE, then the order, passes the
// root's object control and the order's row version, and moves the row version once; the conversion
// slot stays taken. The end of an active order then reads, without locks (order_facts.go, D-1),
// what FUL confirmed (ShipmentFacts v1) and, for cancel and close_remainder, what PUR lists against
// the order (DownstreamCommitments v1); order.close reads no downstream commitment (its guard names
// none: downstream_source not_required). The end and its event name the contracts that answered;
// STK's reservations are not_enabled (D-10) and STL releases its credit reservations itself (PL-13).

// CancelOrder cancels a draft or active order (order.cancel).
func (s Service) CancelOrder(ctx context.Context, cmd api.CancelOrder) (api.OrderReceipt, error) {
	return s.endOrder(ctx, api.CommandCancelOrder, cmd.IdempotencyKey, cmd.OrderID, cmd.ExpectedVersion, cmd.Reason, cmd.Evidence, cmd,
		cmd.Validate())
}

// CloseOrderRemainder closes an active order short (order.close_remainder).
func (s Service) CloseOrderRemainder(ctx context.Context, cmd api.CloseOrderRemainder) (api.OrderReceipt, error) {
	return s.endOrder(ctx, api.CommandCloseOrderRemainder, cmd.IdempotencyKey, cmd.OrderID, cmd.ExpectedVersion, cmd.Reason, cmd.Evidence, cmd,
		cmd.Validate())
}

// CloseOrder closes a fulfilled active order (order.close).
func (s Service) CloseOrder(ctx context.Context, cmd api.CloseOrder) (api.OrderReceipt, error) {
	return s.endOrder(ctx, api.CommandCloseOrder, cmd.IdempotencyKey, cmd.OrderID, cmd.ExpectedVersion, cmd.Reason, cmd.Evidence, cmd,
		cmd.Validate())
}

// endEvents are the event of each end.
var endEvents = map[string]string{api.CommandCancelOrder: api.EventOrderCancelled, api.CommandCloseOrderRemainder: api.EventOrderRemainderClosed,
	api.CommandCloseOrder: api.EventOrderClosed}

func (s Service) endOrder(ctx context.Context, name, key, id string, expected int64, reason string, proofs []api.Evidence, req api.Command,
	validation error) (api.OrderReceipt, error) {
	h := &orderCommand{s: s, orderID: id}
	evidenced := evidence(proofs)
	var readable map[string]bool
	h.prepare = func(ctx context.Context) error {
		var err error
		readable, err = s.readEvidence(ctx, evidenceIDs(evidenced))
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, "/args/order_id"); err != nil {
			return action.Outcome{}, err
		}
		if err := h.o.Check(expected); err != nil {
			return action.Outcome{}, err
		}
		act := quote.Act{By: d.Actor, Epoch: d.Epoch, At: s.Clock.Now(), Reason: reason, Evidence: evidenced}
		if name == api.CommandCancelOrder && h.o.Status == order.Draft {
			c, err := h.o.Cancel(act)
			if err != nil {
				return action.Outcome{}, err
			}
			if err = checkCited(evidenced, readable, "/args/evidence"); err != nil {
				return action.Outcome{}, err
			}
			return h.save(ctx, exec, string(c.Status), h.event(api.EventOrderCancelled, map[string]any{"conversion_slot": h.o.Slot}))
		}
		f, err := s.activeFulfilment(ctx, exec, h.o)
		if err != nil {
			return action.Outcome{}, err
		}
		open := orderrev.Downstream{Source: orderrev.NotRequired}
		if name != api.CommandCloseOrder && h.o.Status == order.Active {
			if open, err = s.downstream(ctx, exec, h.o.ID); err != nil {
				return action.Outcome{}, err
			}
		}
		e, err := h.o.EndActive(name, f, open, nil, act)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = checkCited(evidenced, readable, "/args/evidence"); err != nil {
			return action.Outcome{}, err
		}
		lines := make([]map[string]any, 0, len(e.Lines))
		for _, l := range e.Lines {
			lines = append(lines, map[string]any{"line_id": l.LineID, "unit": l.Unit, "ordered": l.Ordered, "shipped": l.Shipped,
				"remainder": l.Remainder})
		}
		var settled []string
		for _, c := range open.Open {
			if c.Disposition != "" {
				settled = append(settled, c.Kind+":"+c.ID+"="+c.Disposition)
			}
		}
		return h.save(ctx, exec, string(e.Status), h.event(endEvents[name], map[string]any{"conversion_slot": h.o.Slot, "ended_from": string(order.Active),
			"lines": lines, "fulfilment_source": e.Fulfilment, "downstream_source": e.Downstream, "end_command": name,
			"downstream_settled": strings.Join(settled, ","), "stock_reservations": api.NotEnabled}))
	}
	res, err := s.run(ctx, action.Command{Name: name, IdempotencyKey: key, ObjectID: id, Reason: reason}, req, validation, h)
	if err != nil {
		return api.OrderReceipt{}, err
	}
	return s.orderReceipt(ctx, res, "")
}
