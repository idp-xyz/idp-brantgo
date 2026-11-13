package application

import (
	"context"
	"strconv"
	"strings"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/amendment"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
)

// What FUL shipped and what PUR committed against an active order (S5 unit C4a; WBS S5-09a;
// TRD-INV-018, TRD-INV-019; Context Map R70 ShipmentFacts v1, R71 DownstreamCommitments v1; WF-06;
// S5 decisions D-1, D-10, D-19). order.cancel, order.close_remainder and order.close of an active
// order, and order.amendment.prepare and apply, read them inside TRD's own transaction with TRD's
// executor, after the quote root and the order are locked FOR UPDATE and the authority guard FOR
// SHARE, without taking a lock: every FUL and PUR command that adds to either answer holds the
// order row FOR SHARE through OrderGuard v1 until it commits, so it committed before TRD's lock and
// is read, or it waits for TRD and then meets the order as TRD left it. A read before TRD's
// transaction would miss a confirmation committing in between (R70's risk).
//
// STK's stock reservations are not enabled (D-10: the stock route is not supported until STK): the
// answers name GuardSTKReservations as NotEnabled. STL's credit reservations are not commitments an
// order end waits for: STL releases them when the order ends (PL-13).

// ShipmentFacts is FUL's ShipmentFacts v1 (R70) as TRD reads it: what the confirmed shipments of
// order orderID shipped of each order line, read with exec without a lock.
type ShipmentFacts func(ctx context.Context, exec bentopg.Executor, orderID string) ([]ShippedLine, error)

// ShippedLine is one answer of ShipmentFacts v1: an order line, the unit and its revision, the
// quantity shipped over every order revision (QuantityFormat) and the number of confirmed shipments.
type ShippedLine struct {
	LineID       string
	UnitID       string
	UnitRevision int64
	Quantity     string
	Shipments    int64
}

// DownstreamCommitments is PUR's DownstreamCommitments v1 (R71) as TRD reads it: the prepared
// drafts, live commitments and demand allocations of order orderID, read with exec without a lock.
type DownstreamCommitments func(ctx context.Context, exec bentopg.Executor, orderID string) ([]DownstreamItem, error)

// DownstreamItem is one answer of DownstreamCommitments v1: kind, object, its draft, the order line
// and quantity of an allocation, the order revision it binds, its state and its approved
// disposition ("" when none).
type DownstreamItem struct {
	Kind          string
	ObjectID      string
	DraftID       string
	OrderLineID   string
	OrderRevision int64
	State         string
	Quantity      string
	Disposition   string
}

// OrderFacts are the reads of FUL's and PUR's facts. The composed process wires both (trd.New
// refuses TRD without them); a Service built without one judges its answer as given and records it
// as not_wired, never as checked.
type OrderFacts struct {
	Shipments   ShipmentFacts
	Commitments DownstreamCommitments
}

// shipped is what FUL confirmed of order id at its current revision r.
func (s Service) shipped(ctx context.Context, exec bentopg.Executor, id string, r *orderrev.Revision) (orderrev.Fulfilment, error) {
	if s.Facts.Shipments == nil {
		return orderrev.Fulfilment{Source: orderrev.NotWired}, nil
	}
	lines, err := s.Facts.Shipments(ctx, exec, id)
	if err != nil {
		return orderrev.Fulfilment{}, err
	}
	return orderrev.FulfilmentOf(api.GuardFULShipments, r, shippedOf(lines))
}

func shippedOf(lines []ShippedLine) []orderrev.Shipped {
	out := make([]orderrev.Shipped, 0, len(lines))
	for _, l := range lines {
		out = append(out, orderrev.Shipped{LineID: l.LineID, Unit: l.UnitID, Quantity: l.Quantity, Shipments: l.Shipments})
	}
	return out
}

// downstream is what PUR lists against order id.
func (s Service) downstream(ctx context.Context, exec bentopg.Executor, id string) (orderrev.Downstream, error) {
	if s.Facts.Commitments == nil {
		return orderrev.Downstream{Source: orderrev.NotWired}, nil
	}
	items, err := s.Facts.Commitments(ctx, exec, id)
	if err != nil {
		return orderrev.Downstream{}, err
	}
	d := orderrev.Downstream{Source: api.GuardDownstream}
	for _, x := range items {
		d.Open = append(d.Open, orderrev.Commitment{Context: "PUR", Kind: x.Kind, ID: x.ObjectID, State: x.State, LineID: x.OrderLineID,
			Quantity: x.Quantity, Disposition: x.Disposition})
	}
	return d, nil
}

// activeFulfilment is what FUL confirmed of order o when it is active at an intact revision; the
// empty answer of the source otherwise (the command refuses the order for its state first).
func (s Service) activeFulfilment(ctx context.Context, exec bentopg.Executor, o *order.Order) (orderrev.Fulfilment, error) {
	if o == nil || o.Status != order.Active {
		return orderrev.Fulfilment{Source: api.GuardFULShipments}, nil
	}
	r, err := o.Revision()
	if err != nil {
		return orderrev.Fulfilment{}, err
	}
	return s.shipped(ctx, exec, o.ID, r)
}

// impactFacts is what order.amendment.prepare reads of an active order before its transaction to
// pre-fill the impact assessment (WF-06 step 1): FUL's shipments, PUR's open objects and STL's
// CreditGuard answer for the current revision, each with the contract that answered.
type impactFacts struct {
	read        bool
	revision    int64
	shipped     []ShippedLine
	shipments   string
	commitments []DownstreamItem
	downstream  string
	credit      []quote.Entry
	terms       quote.Ref
}

// readImpactFacts reads, in a TRD transaction of its own before prepare's, what the downstream
// contexts say of active order id at now. An unknown order, or one that is not active, reads
// nothing: the command refuses it.
func (s Service) readImpactFacts(ctx context.Context, id string, now time.Time) (impactFacts, error) {
	var f impactFacts
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		o, err := postgres.LoadOrder(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil || o == nil || o.Status != order.Active {
			return err
		}
		r, err := o.Revision()
		if err != nil {
			return err
		}
		f.read, f.revision, f.terms = true, r.Number, r.Terms.PaymentTerms
		f.shipments, f.downstream = orderrev.NotWired, orderrev.NotWired
		if s.Facts.Shipments != nil {
			if f.shipped, err = s.Facts.Shipments(ctx, exec, id); err != nil {
				return err
			}
			f.shipments = api.GuardFULShipments
		}
		if s.Facts.Commitments != nil {
			if f.commitments, err = s.Facts.Commitments(ctx, exec, id); err != nil {
				return err
			}
			f.downstream = api.GuardDownstream
		}
		need, err := orderrev.NeedOf(r.Amounts.Payments)
		if err != nil {
			return err
		}
		out, err := s.credit(ctx, exec, OrderRevisionRef{OrderID: id, OrderRevision: r.Number, OrderHash: r.Hash}, need, nil, r.Currency.ID, now,
			"/args/order_id")
		f.credit = out.entries
		return err
	})
	return f, err
}

// notes are the impact notes the facts give for the areas the downstream contexts speak for
// (purchasing: PUR; quality: PUR's commitments and FUL's shipments; packing and booking: FUL;
// payment plan: STL), for a proposal with payment terms terms. Materials and operations have no
// owner that publishes facts yet: a person assesses them.
func (f impactFacts) notes(terms quote.Ref) map[string]string {
	if !f.read {
		return nil
	}
	var shipped, shipments []string
	for _, l := range f.shipped {
		shipped = append(shipped, l.LineID+" "+l.Quantity+" "+l.UnitID)
		shipments = append(shipments, l.LineID+" in "+strconv.FormatInt(l.Shipments, 10))
	}
	fulNote := f.shipments + ": nothing shipped"
	if len(shipped) > 0 {
		fulNote = f.shipments + ": shipped " + strings.Join(shipped, ", ") + " (kept: a line never holds less than it shipped)"
	}
	var open, disposed, inspected []string
	for _, x := range f.commitments {
		switch x.Kind {
		case orderrev.KindCommitmentDisposition:
			// An approved disposition of delivered goods (S5R-D05): settled, listed apart; PUR's
			// allocation row of the line is already net of it.
			disposed = append(disposed, x.ObjectID+" "+x.Disposition+" "+x.Quantity+" of line "+x.OrderLineID)
			continue
		case orderrev.KindDemandAllocation:
			open = append(open, x.Kind+" "+x.OrderLineID+" "+x.Quantity)
		default:
			open = append(open, x.Kind+" "+x.ObjectID+" "+x.State+" at order revision "+strconv.FormatInt(x.OrderRevision, 10))
		}
		if x.Kind == orderrev.KindPurchaseCommitment && x.State != "requested" && x.State != "approved" {
			inspected = append(inspected, x.ObjectID)
		}
		if x.Disposition != "" {
			open[len(open)-1] += " (disposition " + x.Disposition + ")"
		}
	}
	purNote := f.downstream + ": nothing open against the order"
	if len(open) > 0 {
		purNote = f.downstream + ": " + strings.Join(open, "; ")
	}
	if len(disposed) > 0 {
		purNote += "; disposed of with approval: " + strings.Join(disposed, "; ")
	}
	quality := "issued commitments: none"
	if len(inspected) > 0 {
		quality = "issued commitments inspected on their lines: " + strings.Join(inspected, ", ")
	}
	booking := f.shipments + ": no confirmed shipment"
	if len(shipped) > 0 {
		booking = f.shipments + ": confirmed shipments per line: " + strings.Join(shipments, ", ") +
			"; prepared shipments and bookings are FUL's review"
	}
	var credit []string
	for _, e := range f.credit {
		if e.Value != "" {
			credit = append(credit, e.Name+"="+e.Value)
		}
	}
	plan := "payment terms unchanged"
	if terms != f.terms {
		plan = "payment terms change from " + f.terms.ID + "@" + strconv.FormatInt(f.terms.Revision, 10) + " to " + terms.ID + "@" +
			strconv.FormatInt(terms.Revision, 10)
	}
	return map[string]string{
		amendment.AreaPurchasing:  purNote,
		amendment.AreaQuality:     f.downstream + ": " + quality + "; " + fulNote,
		amendment.AreaPacking:     fulNote,
		amendment.AreaBooking:     booking,
		amendment.AreaPaymentPlan: plan + "; revision " + strconv.FormatInt(f.revision, 10) + ": " + strings.Join(credit, ", "),
	}
}
