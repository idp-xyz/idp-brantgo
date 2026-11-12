package orderrev

import (
	"maps"
	"slices"
	"strings"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Status is the state of a sales order (SM-TRD-ORDER; package order holds the aggregate).
type Status string

const (
	Draft             Status = "draft"
	Active            Status = "active"
	Cancelled         Status = "cancelled"
	ClosedUnfulfilled Status = "closed_unfulfilled"
	Closed            Status = "closed"
)

// Commands of SM-TRD-ORDER.
const (
	CommandActivate         = "order.activate"
	CommandCancel           = "order.cancel"
	CommandCloseUnfulfilled = "order.close_unfulfilled"
	CommandApplyAmendment   = "order.amendment.apply"
	CommandCloseRemainder   = "order.close_remainder"
	CommandClose            = "order.close"
)

// Transition returns the state SM-TRD-ORDER moves an order in state from to by command:
//
//	draft  → active              order.activate
//	draft  → cancelled           order.cancel
//	draft  → closed_unfulfilled  order.close_unfulfilled
//	active → active              order.amendment.apply (a new order revision)
//	active → cancelled           order.cancel
//	active → closed              order.close_remainder, order.close
//
// Every other pair is refused: ORDER_NOT_DRAFT for activate and close_unfulfilled, ORDER_NOT_ACTIVE
// for the amendment, close_remainder and close, INVALID_STATE for cancel of an ended order and for a
// command the machine does not know. The guards of each transition are the commands' own (package
// order for draft, end.go and package amendment for active).
func Transition(from Status, command string) (Status, error) {
	at := "/args/order_id"
	switch command {
	case CommandActivate, CommandCloseUnfulfilled:
		if from != Draft {
			return "", fail(CodeOrderNotDraft, at, "The order is "+string(from))
		}
		if command == CommandActivate {
			return Active, nil
		}
		return ClosedUnfulfilled, nil
	case CommandCancel:
		if from != Draft && from != Active {
			return "", kernel.Reject(kernel.CodeInvalidState, at, "The order is "+string(from))
		}
		return Cancelled, nil
	case CommandApplyAmendment, CommandCloseRemainder, CommandClose:
		if from != Active {
			return "", fail(CodeOrderNotActive, at, "The order is "+string(from))
		}
		if command == CommandApplyAmendment {
			return Active, nil
		}
		return Closed, nil
	}
	return "", kernel.Reject(kernel.CodeInvalidState, "/command", "SM-TRD-ORDER has no command "+command)
}

// NotWired is the Source of an answer a caller could not ask for because the owner's context is
// not built yet (FUL's shipments, PUR's commitments and STL's reservations in S3). The rules judge
// such an answer as given — there is nothing to find where nothing can exist yet — and the caller's
// commit evidence names it, so it is never mistaken for a checked one.
const NotWired = "not_wired"

// Fulfilment is what FUL confirmed of an order (FUL ShipmentConfirmation, ActiveOrderRef v1 R27):
// the shipped quantity of each order line in the line's unit, cumulated over the order's
// revisions, and the contract that answered (Source; NotWired until FUL is built). A line that is
// absent shipped nothing.
type Fulfilment struct {
	Source  string
	Shipped map[string]kernel.Decimal
}

// check refuses shipments of a line r never had or a negative quantity (REFERENCE_INVALID at
// /fulfilment/shipped/<line>).
func (f Fulfilment) check(r *Revision) error {
	for _, id := range slices.Sorted(maps.Keys(f.Shipped)) {
		if r.Line(id) == nil || f.Shipped[id].Sign() < 0 {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/fulfilment/shipped/"+id, "Shipments are of the order's lines and not negative")
		}
	}
	return nil
}

// Any reports whether any line shipped anything.
func (f Fulfilment) Any() bool {
	for _, q := range f.Shipped {
		if q.Sign() > 0 {
			return true
		}
	}
	return false
}

// Commitment is a downstream obligation made against the order that its end would leave without a
// cause: a purchase commitment or demand allocation (PUR), a stock reservation (STK) or a credit
// reservation (STL), and the approved disposition that settles it ("" when none: cancel the
// commitment, release the reservation, or have its disposition approved first).
type Commitment struct {
	Context     string
	Kind        string
	ID          string
	Disposition string
}

// Downstream is what the downstream contexts say is still open against the order, and the
// contract that answered (Source; NotWired until they are built).
type Downstream struct {
	Source string
	Open   []Commitment
}

// unsettled refuses the first open commitment without an approved disposition
// (DOWNSTREAM_COMMITMENT_OPEN).
func (d Downstream) unsettled() error {
	for _, c := range d.Open {
		if strings.TrimSpace(c.Disposition) == "" {
			return fail(CodeDownstreamCommitmentOpen, "/args/order_id", c.Context+" "+c.Kind+" "+c.ID+
				" is open against the order without an approved disposition")
		}
	}
	return nil
}

// Tolerance is the approved under- and over-delivery of one order line, as ratios of its ordered
// quantity (0–1; UL-INV-21: none unless approved). A line without one ships exactly its quantity.
type Tolerance struct {
	Under kernel.Decimal
	Over  kernel.Decimal
}

// Ending is how an active order ended: the command, the state it reached, each line's ordered,
// shipped and unshipped quantity in its unit (the remainder a short closing gives up), who ended it
// and why, and where the shipments and commitments it judged came from. The conversion slot and
// every revision stay (TRD-INV-019: 三者均保留转换名额与历史).
type Ending struct {
	Command    string
	Status     Status
	Lines      []LineEnd
	Act        quote.Act
	Fulfilment string
	Downstream string
}

// LineEnd is one line at the end of an order.
type LineEnd struct {
	LineID    string
	Unit      string
	Ordered   string
	Shipped   string
	Remainder string
}

// Cancel decides order.cancel of an active order at its current revision r (SM-TRD-ORDER active →
// cancelled; TRD-INV-019, TD-AT02), by act (a human with a reason): nothing shipped
// (ORDER_HAS_SHIPMENTS: close the remainder instead) and every downstream commitment released or
// settled by an approved disposition (DOWNSTREAM_COMMITMENT_OPEN).
func Cancel(from Status, r *Revision, f Fulfilment, d Downstream, act quote.Act) (*Ending, error) {
	if from != Active {
		return nil, fail(CodeOrderNotActive, "/args/order_id", "The order is "+string(from)+"; a draft is cancelled as a draft")
	}
	if err := ended(r, f, act, true); err != nil {
		return nil, err
	}
	if f.Any() {
		return nil, fail(CodeOrderHasShipments, "/args/order_id", "The order has confirmed shipments; close the remainder instead")
	}
	if err := d.unsettled(); err != nil {
		return nil, err
	}
	return ending(CommandCancel, Cancelled, r, f, d, act), nil
}

// CloseRemainder decides order.close_remainder of an active order at its current revision r
// (SM-TRD-ORDER active → closed; TRD-INV-019, TD-AT02, ES-09 step 6), by act (a human with the
// reason the rest is not shipped): something shipped (INVALID_STATE otherwise: cancel the order),
// something is left (INVALID_STATE otherwise: close it), and every downstream commitment of the
// remainder released or settled (DOWNSTREAM_COMMITMENT_OPEN). The remainder of each line is given up.
func CloseRemainder(from Status, r *Revision, f Fulfilment, d Downstream, act quote.Act) (*Ending, error) {
	if from != Active {
		return nil, fail(CodeOrderNotActive, "/args/order_id", "The order is "+string(from))
	}
	if err := ended(r, f, act, true); err != nil {
		return nil, err
	}
	if !f.Any() {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/order_id", "Nothing is shipped; cancel the order instead")
	}
	e := ending(CommandCloseRemainder, Closed, r, f, d, act)
	if !slices.ContainsFunc(e.Lines, func(l LineEnd) bool { return l.Remainder != "0" }) {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/order_id", "Everything is shipped; close the order instead")
	}
	if err := d.unsettled(); err != nil {
		return nil, err
	}
	return e, nil
}

// Close decides order.close of an active order at its current revision r (SM-TRD-ORDER active →
// closed; TRD-INV-019): every line shipped its quantity, within the line's approved tolerance
// (ORDER_NOT_FULFILLED below it, INVALID_STATE above it: amend the order to what was shipped). One
// line's excess never makes up another's shortfall. act needs no reason.
func Close(from Status, r *Revision, f Fulfilment, tolerances map[string]Tolerance, act quote.Act) (*Ending, error) {
	if from != Active {
		return nil, fail(CodeOrderNotActive, "/args/order_id", "The order is "+string(from))
	}
	if err := ended(r, f, act, false); err != nil {
		return nil, err
	}
	one := kernel.DecimalFromInt(1)
	for _, l := range r.Lines {
		ordered, err := kernel.ParseDecimal(l.Quantity, wide)
		if err != nil {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/revision/lines", "Line "+l.LineID+" has no readable quantity")
		}
		t := tolerances[l.LineID]
		if t.Under.Sign() < 0 || t.Under.Cmp(one) > 0 || t.Over.Sign() < 0 || t.Over.Cmp(one) > 0 {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/tolerances/"+l.LineID, "A tolerance is a ratio from 0 to 1")
		}
		shipped := f.Shipped[l.LineID]
		switch {
		case shipped.Cmp(ordered.Mul(one.Sub(t.Under))) < 0:
			return nil, fail(CodeOrderNotFulfilled, "/args/order_id", "Line "+l.LineID+" shipped "+shipped.String()+" of "+l.Quantity+" "+l.Unit.ID)
		case shipped.Cmp(ordered.Mul(one.Add(t.Over))) > 0:
			return nil, kernel.Reject(kernel.CodeInvalidState, "/args/order_id", "Line "+l.LineID+" shipped "+shipped.String()+
				" beyond its "+l.Quantity+" "+l.Unit.ID+" and approved tolerance; amend the order first")
		}
	}
	return ending(CommandClose, Closed, r, f, Downstream{Source: NotWired}, act), nil
}

// ended checks what every end needs: shipments of the order's lines, and a human act at an instant
// with a reason when one is needed (SCHEMA_INVALID at /args/reason).
func ended(r *Revision, f Fulfilment, act quote.Act, reason bool) error {
	if err := f.check(r); err != nil {
		return err
	}
	if reason && strings.TrimSpace(act.Reason) == "" {
		return kernel.Reject(kernel.CodeSchemaInvalid, "/args/reason", "An order is ended for a reason")
	}
	if act.By.Kind != kernel.ActorHuman || act.At.IsZero() {
		return kernel.Reject(kernel.CodePermissionDenied, "/", "A human ends an order")
	}
	return nil
}

func ending(command string, to Status, r *Revision, f Fulfilment, d Downstream, act quote.Act) *Ending {
	act.At, act.Evidence = kernel.Instant(act.At), slices.Clone(act.Evidence)
	e := &Ending{Command: command, Status: to, Act: act, Fulfilment: source(f.Source), Downstream: source(d.Source)}
	for _, l := range r.Lines {
		ordered, _ := kernel.ParseDecimal(l.Quantity, wide)
		shipped := f.Shipped[l.LineID]
		rest := ordered.Sub(shipped)
		if rest.Sign() < 0 {
			rest = kernel.Decimal{}
		}
		e.Lines = append(e.Lines, LineEnd{LineID: l.LineID, Unit: l.Unit.ID, Ordered: l.Quantity, Shipped: shipped.String(), Remainder: rest.String()})
	}
	return e
}

func source(s string) string {
	if strings.TrimSpace(s) == "" {
		return NotWired
	}
	return s
}
