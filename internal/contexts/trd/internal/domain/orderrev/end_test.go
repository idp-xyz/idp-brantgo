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
	e, err := Cancel(Active, r, Fulfilment{Source: "ful.shipment_facts.v1"}, Downstream{Source: "pur.downstream_commitments.v1"}, act("客户撤单"))
	if err != nil || e.Status != Cancelled || e.Command != CommandCancel || e.Fulfilment != "ful.shipment_facts.v1" ||
		e.Downstream != "pur.downstream_commitments.v1" ||
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
	e, err = CloseRemainder(Active, r, sixty, Downstream{Source: "pur.downstream_commitments.v1"}, act("客户同意短交"))
	if err != nil || e.Status != Closed || e.Command != CommandCloseRemainder || e.Fulfilment != "ful.shipment_facts.v1" ||
		e.Downstream != "pur.downstream_commitments.v1" ||
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
	if e, err := Close(Active, r, f, tolerance, act("")); err != nil || e.Command != CommandClose || e.Status != Closed ||
		e.Downstream != NotRequired {
		t.Fatalf("close within tolerance: %v %+v", err, e)
	}
}

// TRD-INV-019 and Context Map R71: what DownstreamCommitments v1 lists stays open against the order
// until it is disposed of, released or used — a prepared draft, a commitment requested, approved,
// issued or confirmed, goods allocated to the order beyond what its line shipped — and is no longer
// open once fulfilled (delivered in full), disposed of, or allocated only to what shipped. Cancel
// sees nothing shipped, so every allocation is open there; closing the remainder after shipments
// keeps the allocation that the shipments used.
func TestOpenDownstreamCommitments(t *testing.T) {
	r := revisionOne(t)
	pur := func(cs ...Commitment) Downstream {
		for i := range cs {
			cs[i].Context = "PUR"
		}
		return Downstream{Source: "pur.downstream_commitments.v1", Open: cs}
	}
	commitment := func(state string) Commitment {
		return Commitment{Kind: KindPurchaseCommitment, ID: "pc.1", State: state}
	}
	allocation := func(line, q string) Commitment {
		return Commitment{Kind: KindDemandAllocation, ID: line, State: "allocated", LineID: line, Quantity: q}
	}
	l4 := shipped("L4", "44")
	for name, c := range map[string]struct {
		end  func() (*Ending, error)
		want string
	}{
		"cancel with a prepared draft": {func() (*Ending, error) {
			return Cancel(Active, r, Fulfilment{}, pur(Commitment{Kind: KindPurchaseDraft, ID: "draft.1", State: "prepared"}), act("撤单"))
		}, "DOWNSTREAM_COMMITMENT_OPEN at /args/order_id"},
		"cancel with a requested commitment": {func() (*Ending, error) {
			return Cancel(Active, r, Fulfilment{}, pur(commitment("requested")), act("撤单"))
		},
			"DOWNSTREAM_COMMITMENT_OPEN at /args/order_id"},
		"cancel with an issued commitment": {func() (*Ending, error) {
			return Cancel(Active, r, Fulfilment{}, pur(commitment("issued")), act("撤单"))
		},
			"DOWNSTREAM_COMMITMENT_OPEN at /args/order_id"},
		"cancel with a confirmed commitment": {func() (*Ending, error) {
			return Cancel(Active, r, Fulfilment{}, pur(commitment("confirmed")), act("撤单"))
		},
			"DOWNSTREAM_COMMITMENT_OPEN at /args/order_id"},
		"cancel with a fulfilled commitment only": {func() (*Ending, error) {
			return Cancel(Active, r, Fulfilment{}, pur(commitment(StateFulfilled)), act("撤单"))
		},
			"OK"},
		"cancel with goods allocated and delivered": {func() (*Ending, error) {
			return Cancel(Active, r, Fulfilment{}, pur(commitment(StateFulfilled), allocation("L4", "48")), act("撤单"))
		}, "DOWNSTREAM_COMMITMENT_OPEN at /args/order_id"},
		"cancel with the allocation disposed of": {func() (*Ending, error) {
			a := allocation("L4", "48")
			a.Disposition = "disposition.1"
			return Cancel(Active, r, Fulfilment{}, pur(commitment(StateFulfilled), a), act("撤单"))
		}, "OK"},
		"close the remainder with the allocation shipped": {func() (*Ending, error) {
			return CloseRemainder(Active, r, l4, pur(commitment(StateFulfilled), allocation("L4", "44")), act("短交"))
		}, "OK"},
		"close the remainder with goods allocated beyond the shipments": {func() (*Ending, error) {
			return CloseRemainder(Active, r, l4, pur(commitment(StateFulfilled), allocation("L4", "48")), act("短交"))
		}, "DOWNSTREAM_COMMITMENT_OPEN at /args/order_id"},
		"close the remainder with an allocation of another line": {func() (*Ending, error) {
			return CloseRemainder(Active, r, l4, pur(allocation("L1", "1")), act("短交"))
		}, "DOWNSTREAM_COMMITMENT_OPEN at /args/order_id"},
		"close the remainder with the commitment still issued": {func() (*Ending, error) {
			return CloseRemainder(Active, r, l4, pur(commitment("issued"), allocation("L4", "44")), act("短交"))
		}, "DOWNSTREAM_COMMITMENT_OPEN at /args/order_id"},
		"an unreadable allocation is open": {func() (*Ending, error) {
			return CloseRemainder(Active, r, l4, pur(allocation("L4", "x")), act("短交"))
		}, "DOWNSTREAM_COMMITMENT_OPEN at /args/order_id"},
		"close ignores what is open downstream": {func() (*Ending, error) {
			return Close(Active, r, shipped("L1", "24", "L2", "36", "L3", "12", "L4", "48"), nil, act(""))
		}, "OK"},
	} {
		if _, err := c.end(); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
	_, err := Cancel(Active, r, Fulfilment{}, pur(commitment(StateFulfilled), allocation("L4", "48")), act("撤单"))
	if rej, ok := kernel.AsRejection(err); !ok || !strings.Contains(rej.Fields[0].Reason, "PUR demand_allocation L4 allocates 48 of line L4, which shipped 0") {
		t.Errorf("the refusal names the allocation: %v", err)
	}
}

// S5R-D05 (TRD side, verified; R71 as R4 publishes it; TRD-INV-019 "为已交付未出运货物保留的需求分配视为未处置，
// 除非 PUR 记录了获批处置"): PUR lists every approved disposition of delivered goods as its own row
// (commitment_disposition, with its disposition) and each allocation net of them. TRD settles a row
// with a disposition and judges the allocation as PUR states it: it never subtracts a disposition a
// second time. 60 PCS of L4 delivered and kept for the order, nothing shipped: with 40 returned to the
// supplier the allocation row says 20 — still open, and named as 20, not as 60 nor as nothing — and
// with the other 20 written off PUR lists no allocation and the order cancels; a requested (not yet
// approved) disposition is not listed at all.
func TestS5RD05ApprovedDispositionsSettleWithoutASecondSubtraction(t *testing.T) {
	r := revisionOne(t)
	pur := func(cs ...Commitment) Downstream {
		for i := range cs {
			cs[i].Context = "PUR"
		}
		return Downstream{Source: "pur.downstream_commitments.v1", Open: cs}
	}
	cancelled := Commitment{Kind: KindPurchaseCommitment, ID: "commitment.1", State: StateFulfilled}
	allocation := func(q string) Commitment {
		return Commitment{Kind: KindDemandAllocation, ID: "L4", State: "allocated", LineID: "L4", Quantity: q}
	}
	disposed := func(id, kind, q string) Commitment {
		return Commitment{Kind: KindCommitmentDisposition, ID: id, State: "approved", LineID: "L4", Quantity: q, Disposition: kind}
	}
	_, err := Cancel(Active, r, Fulfilment{}, pur(cancelled, allocation("60")), act("撤单"))
	if rej, ok := kernel.AsRejection(err); !ok || !strings.Contains(rej.Fields[0].Reason, "allocates 60 of line L4, which shipped 0") {
		t.Fatalf("60 delivered and not disposed of: %v", err)
	}
	_, err = Cancel(Active, r, Fulfilment{}, pur(cancelled, allocation("20"), disposed("disp.return", "return_to_supplier", "40")), act("撤单"))
	if rej, ok := kernel.AsRejection(err); !ok || rej.Code != CodeDownstreamCommitmentOpen ||
		!strings.Contains(rej.Fields[0].Reason, "allocates 20 of line L4, which shipped 0") || strings.Contains(rej.Fields[0].Reason, "disp.return") {
		t.Fatalf("40 of 60 returned: the netted allocation alone is open: %v", err)
	}
	e, err := Cancel(Active, r, Fulfilment{}, pur(cancelled, disposed("disp.return", "return_to_supplier", "40"),
		disposed("disp.writeoff", "write_off", "20")), act("撤单"))
	if err != nil || e.Status != Cancelled || e.Downstream != "pur.downstream_commitments.v1" {
		t.Fatalf("every delivered piece disposed of: %v %+v", err, e)
	}
	// Closing the remainder after 50 of 60 shipped: the 10 held for reuse settle the line.
	_, err = CloseRemainder(Active, r, shipped("L4", "50"), pur(cancelled, allocation("60")), act("短交"))
	if code(err) != "DOWNSTREAM_COMMITMENT_OPEN at /args/order_id" {
		t.Fatalf("10 delivered beyond the shipments: %s", code(err))
	}
	if _, err = CloseRemainder(Active, r, shipped("L4", "50"), pur(cancelled, allocation("50"), disposed("disp.hold", "hold_for_reuse", "10")),
		act("短交")); err != nil {
		t.Fatalf("the 10 held for reuse: %v", err)
	}
}

// ShipmentFacts v1 (R70) as the order's revision reads it: the answers of a line summed in the
// line's unit; another unit, a line the revision does not have, or an unreadable or negative
// quantity cannot be compared with the order (REFERENCE_INVALID).
func TestFulfilmentOfShipmentFacts(t *testing.T) {
	r := revisionOne(t)
	f, err := FulfilmentOf("ful.shipment_facts.v1", r, []Shipped{{LineID: "L4", Unit: "PCS", Quantity: "40", Shipments: 1},
		{LineID: "L4", Unit: "PCS", Quantity: "4", Shipments: 1}, {LineID: "L1", Unit: "PCS", Quantity: "0.5", Shipments: 1}})
	if err != nil || f.Source != "ful.shipment_facts.v1" || len(f.Shipped) != 2 || f.Shipped["L4"].String() != "44" || f.Shipped["L1"].String() != "0.5" {
		t.Fatalf("summed per line: %v %+v", err, f)
	}
	if f, err := FulfilmentOf("ful.shipment_facts.v1", r, nil); err != nil || f.Any() || f.Shipped == nil {
		t.Fatalf("nothing shipped: %v %+v", err, f)
	}
	for name, c := range map[string]struct {
		answer Shipped
		want   string
	}{
		"another unit":   {Shipped{LineID: "L1", Unit: "DOZ", Quantity: "1"}, "REFERENCE_INVALID at /fulfilment/shipped/L1"},
		"another line":   {Shipped{LineID: "L9", Unit: "PCS", Quantity: "1"}, "REFERENCE_INVALID at /fulfilment/shipped/L9"},
		"unreadable":     {Shipped{LineID: "L1", Unit: "PCS", Quantity: "one"}, "REFERENCE_INVALID at /fulfilment/shipped/L1"},
		"negative":       {Shipped{LineID: "L1", Unit: "PCS", Quantity: "-1"}, "REFERENCE_INVALID at /fulfilment/shipped/L1"},
		"empty quantity": {Shipped{LineID: "L1", Unit: "PCS"}, "REFERENCE_INVALID at /fulfilment/shipped/L1"},
	} {
		if _, err := FulfilmentOf("ful.shipment_facts.v1", r, []Shipped{c.answer}); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
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
