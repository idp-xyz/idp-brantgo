package orderrev

import (
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	endedAt = time.Date(2027, 2, 20, 9, 0, 0, 123456789, time.UTC)
	clerk   = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "clerk"}
)

func act(reason string) quote.Act {
	return quote.Act{By: clerk, Epoch: 3, At: endedAt, Reason: reason, Evidence: proof}
}

func lineEnds(e *Ending) []string {
	var out []string
	for _, l := range e.Lines {
		out = append(out, strings.Join([]string{l.LineID, l.Ordered, l.Shipped, l.Remainder, l.Unit}, " "))
	}
	return out
}

// SM-TRD-ORDER: every command from every state.
func TestOrderStateMachine(t *testing.T) {
	commands := []string{CommandActivate, CommandCancel, CommandCloseUnfulfilled, CommandApplyAmendment, CommandCloseRemainder, CommandClose}
	want := map[Status][]string{
		Draft:             {"active", "cancelled", "closed_unfulfilled", "ORDER_NOT_ACTIVE", "ORDER_NOT_ACTIVE", "ORDER_NOT_ACTIVE"},
		Active:            {"ORDER_NOT_DRAFT", "cancelled", "ORDER_NOT_DRAFT", "active", "closed", "closed"},
		Cancelled:         {"ORDER_NOT_DRAFT", "INVALID_STATE", "ORDER_NOT_DRAFT", "ORDER_NOT_ACTIVE", "ORDER_NOT_ACTIVE", "ORDER_NOT_ACTIVE"},
		ClosedUnfulfilled: {"ORDER_NOT_DRAFT", "INVALID_STATE", "ORDER_NOT_DRAFT", "ORDER_NOT_ACTIVE", "ORDER_NOT_ACTIVE", "ORDER_NOT_ACTIVE"},
		Closed:            {"ORDER_NOT_DRAFT", "INVALID_STATE", "ORDER_NOT_DRAFT", "ORDER_NOT_ACTIVE", "ORDER_NOT_ACTIVE", "ORDER_NOT_ACTIVE"},
	}
	for from, outcomes := range want {
		for i, c := range commands {
			to, err := Transition(from, c)
			got := string(to)
			if err != nil {
				got = strings.Fields(code(err))[0]
			}
			if got != outcomes[i] {
				t.Errorf("%s from %s: %s, want %s", c, from, got, outcomes[i])
			}
		}
	}
	if _, err := Transition(Active, "order.reopen"); code(err) != "INVALID_STATE at /command" {
		t.Errorf("unknown command: %s", code(err))
	}
}

// TD-AT02: an active order with nothing shipped is cancelled; one that shipped 60 pieces is not
// (ORDER_HAS_SHIPMENTS) but closes its remainder, keeping its history (the lines' ordered, shipped
// and given-up quantities).
func TestTDAT02CancelOrCloseTheRemainder(t *testing.T) {
	r := revisionOne(t)
	e, err := Cancel(Active, r, Fulfilment{Source: NotWired}, Downstream{Source: NotWired}, act("客户撤单"))
	if err != nil || e.Status != Cancelled || e.Command != CommandCancel || e.Fulfilment != NotWired || e.Downstream != NotWired ||
		!e.Act.At.Equal(kernel.Instant(endedAt)) || e.Act.At.Nanosecond() != 123456000 {
		t.Fatalf("cancel unshipped: %v %+v", err, e)
	}
	if !slices.Equal(lineEnds(e), []string{"L1 24 0 24 PCS", "L2 36 0 36 PCS", "L3 12 0 12 PCS", "L4 48 0 48 PCS"}) {
		t.Fatalf("cancel lines: %q", lineEnds(e))
	}
	sixty := shipped("L1", "24", "L2", "36")
	if _, err := Cancel(Active, r, sixty, Downstream{}, act("客户撤单")); code(err) != "ORDER_HAS_SHIPMENTS at /args/order_id" {
		t.Fatalf("cancel after 60 shipped: %s", code(err))
	}
	e, err = CloseRemainder(Active, r, sixty, Downstream{Source: "pur.purchase_commitments.v1"}, act("客户同意短交"))
	if err != nil || e.Status != Closed || e.Command != CommandCloseRemainder || e.Fulfilment != "ful.shipment_confirmation.v1" ||
		e.Downstream != "pur.purchase_commitments.v1" ||
		!slices.Equal(lineEnds(e), []string{"L1 24 24 0 PCS", "L2 36 36 0 PCS", "L3 12 0 12 PCS", "L4 48 0 48 PCS"}) {
		t.Fatalf("close the remainder after 60 shipped: %v %+v", err, e)
	}
}

// ES-09 step 6: 116 of 120 pieces shipped (blue M 44 of 48); the 4 left are closed short with the
// customer's consent.
func TestES09ShortClosingTheLastFourPieces(t *testing.T) {
	r := revisionOne(t)
	f := shipped("L1", "24", "L2", "36", "L3", "12", "L4", "44")
	e, err := CloseRemainder(Active, r, f, Downstream{}, act("客户同意短交"))
	if err != nil || !slices.Equal(lineEnds(e), []string{"L1 24 24 0 PCS", "L2 36 36 0 PCS", "L3 12 12 0 PCS", "L4 48 44 4 PCS"}) {
		t.Fatalf("short closing: %v %q", err, lineEnds(e))
	}
	// Not yet closable as fulfilled; within an approved 10 % under-delivery on L4 it is.
	if _, err := Close(Active, r, f, nil, act("")); code(err) != "ORDER_NOT_FULFILLED at /args/order_id" {
		t.Fatalf("close at 116 of 120: %s", code(err))
	}
	tolerance := map[string]Tolerance{"L4": {Under: kernel.MustDecimal("0.1")}}
	if e, err := Close(Active, r, f, tolerance, act("")); err != nil || e.Command != CommandClose || e.Status != Closed {
		t.Fatalf("close within tolerance: %v %+v", err, e)
	}
}

// TRD-INV-019's refusals and the conditions every end shares.
func TestEndingRefusals(t *testing.T) {
	r := revisionOne(t)
	all := shipped("L1", "24", "L2", "36", "L3", "12", "L4", "48")
	open := Downstream{Source: "pur.purchase_commitments.v1", Open: []Commitment{{Context: "PUR", Kind: "purchase_commitment", ID: "pc.1"}}}
	settled := Downstream{Source: "pur.purchase_commitments.v1", Open: []Commitment{{Context: "PUR", Kind: "purchase_commitment", ID: "pc.1",
		Disposition: "disposition.1"}}}
	ai := act("客户撤单")
	ai.By = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent"}
	for name, c := range map[string]struct {
		end  func() (*Ending, error)
		want string
	}{
		"cancel with an open commitment": {func() (*Ending, error) { return Cancel(Active, r, Fulfilment{}, open, act("撤单")) },
			"DOWNSTREAM_COMMITMENT_OPEN at /args/order_id"},
		"cancel with a settled commitment": {func() (*Ending, error) { return Cancel(Active, r, Fulfilment{}, settled, act("撤单")) }, "OK"},
		"cancel without a reason":          {func() (*Ending, error) { return Cancel(Active, r, Fulfilment{}, Downstream{}, act(" ")) }, "SCHEMA_INVALID at /args/reason"},
		"cancel by an AI agent":            {func() (*Ending, error) { return Cancel(Active, r, Fulfilment{}, Downstream{}, ai) }, "PERMISSION_DENIED at /"},
		"cancel of a draft here":           {func() (*Ending, error) { return Cancel(Draft, r, Fulfilment{}, Downstream{}, act("撤单")) }, "ORDER_NOT_ACTIVE at /args/order_id"},
		"close the remainder of nothing shipped": {func() (*Ending, error) { return CloseRemainder(Active, r, Fulfilment{}, Downstream{}, act("短交")) },
			"INVALID_STATE at /args/order_id"},
		"close the remainder of everything shipped": {func() (*Ending, error) { return CloseRemainder(Active, r, all, Downstream{}, act("短交")) },
			"INVALID_STATE at /args/order_id"},
		"close the remainder with an open commitment": {func() (*Ending, error) {
			return CloseRemainder(Active, r, shipped("L1", "1"), open, act("短交"))
		}, "DOWNSTREAM_COMMITMENT_OPEN at /args/order_id"},
		"close the remainder without a reason": {func() (*Ending, error) { return CloseRemainder(Active, r, shipped("L1", "1"), Downstream{}, act("")) },
			"SCHEMA_INVALID at /args/reason"},
		"close everything shipped": {func() (*Ending, error) { return Close(Active, r, all, nil, act("")) }, "OK"},
		"close with nothing shipped": {func() (*Ending, error) { return Close(Active, r, Fulfilment{}, nil, act("")) },
			"ORDER_NOT_FULFILLED at /args/order_id"},
		"close beyond the order quantity": {func() (*Ending, error) {
			return Close(Active, r, shipped("L1", "25", "L2", "36", "L3", "12", "L4", "48"), nil, act(""))
		}, "INVALID_STATE at /args/order_id"},
		"close within an approved over-delivery": {func() (*Ending, error) {
			return Close(Active, r, shipped("L1", "25", "L2", "36", "L3", "12", "L4", "48"), map[string]Tolerance{"L1": {Over: kernel.MustDecimal("0.05")}},
				act(""))
		}, "OK"},
		"one line's excess never covers another's shortfall": {func() (*Ending, error) {
			return Close(Active, r, shipped("L1", "25", "L2", "35", "L3", "12", "L4", "48"), map[string]Tolerance{"L1": {Over: kernel.MustDecimal("0.05")}},
				act(""))
		}, "ORDER_NOT_FULFILLED at /args/order_id"},
		"a tolerance beyond 1": {func() (*Ending, error) {
			return Close(Active, r, all, map[string]Tolerance{"L1": {Under: kernel.MustDecimal("2")}}, act(""))
		},
			"REFERENCE_INVALID at /tolerances/L1"},
		"close of a closed order":          {func() (*Ending, error) { return Close(Closed, r, all, nil, act("")) }, "ORDER_NOT_ACTIVE at /args/order_id"},
		"shipments of a line it never had": {func() (*Ending, error) { return Close(Active, r, shipped("L9", "1"), nil, act("")) }, "REFERENCE_INVALID at /fulfilment/shipped/L9"},
		"negative shipments":               {func() (*Ending, error) { return Cancel(Active, r, shipped("L1", "-1"), Downstream{}, act("撤单")) }, "REFERENCE_INVALID at /fulfilment/shipped/L1"},
		"close the remainder of a draft": {func() (*Ending, error) {
			return CloseRemainder(Draft, r, shipped("L1", "1"), Downstream{}, act("短交"))
		}, "ORDER_NOT_ACTIVE at /args/order_id"},
		"close the remainder by an AI agent": {func() (*Ending, error) { return CloseRemainder(Active, r, shipped("L1", "1"), Downstream{}, ai) }, "PERMISSION_DENIED at /"},
	} {
		if _, err := c.end(); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
}
