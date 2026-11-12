package application

import (
	"context"
	"fmt"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/fulfilment"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// What TRD's order changes do to FUL (WBS S5-09b, FUL part; event storming PL-09; WF-06 step 5; S5
// decisions D-18, D-21; SM-FUL-SHIPMENT-REVIEW; BT10). The Inbox consumer ful.shipment_review hands
// OrderAmended, OrderCancelled and OrderRemainderClosed to ReviewOrderChange inside its transaction on
// FUL's own transaction owner. TRD is read before that transaction (PrepareReview: the amended
// revision's lines through FUL's port Orders, ActiveOrderRef v1, R27); inside it FUL locks the order's
// fulfilment root FOR UPDATE — the root the review belongs to, which every confirmation and release of
// the order also locks after its shipment —, then the order's reviews, reads what it holds of the
// order and, when the change leaves something stale or supersedes an open review, runs
// shipment_review.open as the consumer's system actor with the event ID as key. Lock order: fulfilment
// root, reviews, authority guard (the ledger); no upstream guard. shipment_review.settle is a
// person's command: it settles a review by reference to what was done with shipment.cancel,
// shipment.prepare, packing.plan, packing.record_actual, booking.record, shipment_document.prepare
// and commercial_release.evaluate; it locks the fulfilment root, then the review, then the authority
// guard, and reads the shipments it names without a lock.

var reviewCmds = []string{api.CommandOpenShipmentReview, api.CommandSettleShipmentReview}

// reviewRules: shipment_review.open is the system's (SM-FUL-SHIPMENT-REVIEW: the Inbox consumer
// ful.shipment_review); shipment_review.settle is in humanOnlyCommands (role template merchandiser);
// the reads need ful.read.
func reviewRules() []authorization.Rule {
	return []authorization.Rule{
		{Command: api.CommandOpenShipmentReview, Permission: api.CommandOpenShipmentReview, ActorKinds: []kernel.ActorKind{kernel.ActorSystem}},
		authorization.HumanOnly(api.CommandSettleShipmentReview, api.CommandSettleShipmentReview),
		{Command: api.CommandGetShipmentReview, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandListShipmentReviews, Permission: api.PermissionRead, ActorKinds: anyActor},
	}
}

// reviewRequest is request() for the review commands.
func reviewRequest(cmd api.Command) (name, key string, args any, ok bool) {
	switch c := cmd.(type) {
	case api.OpenShipmentReview:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandOpenShipmentReview, key, c, true
	case api.SettleShipmentReview:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandSettleShipmentReview, key, c, true
	}
	return "", "", nil, false
}

// reviewVisible is Visible for the review commands, whose results name the review: a stored result
// is answered when the review exists (settle re-checks the order's visibility in its Prepare).
func reviewVisible(ctx context.Context, exec bentopg.Executor, command, id string) (ok, handled bool, err error) {
	if !slices.Contains(reviewCmds, command) {
		return false, false, nil
	}
	r, err := postgres.LoadReview(ctx, exec, scope(ctx), id, postgres.NoLock)
	return err == nil && r != nil, true, err
}

// What an order change did to FUL's reviews, besides the domain's fulfilment.Effect values.
const (
	// EffectLaterRevision: TRD's order is at a later revision than the amended one; that revision's
	// OrderAmended reviews it.
	EffectLaterRevision = "later_revision"
)

// FailureReviewNotOpenable is the failure code of an order change FUL cannot review for good: TRD
// shows no such order, or another order revision or hash than the event's, or shipment_review.open
// refused it in a way no retry changes.
const FailureReviewNotOpenable = "ful.shipment_review_not_openable"

// ReviewMismatch is an order change FUL cannot review for good: the consumer refuses the event and the
// relay dead-letters it with an alert (ADR-BR-015). Nothing is changed.
type ReviewMismatch struct {
	Code   string
	Detail string
}

func (m *ReviewMismatch) Error() string { return "ful: " + m.Code + ": " + m.Detail }

// OrderChange is one of TRD's order events as the review consumer reads it: the order at revision
// Revision and hash Hash, TRD's row version of the order after the event (Version) and the cause.
type OrderChange struct {
	EventID  string
	Cause    string
	OrderID  string
	Revision int64
	Hash     string
	Version  int64
}

// ReviewBasis is what reviewing an order change needs from TRD, read before the consumer's
// transaction: for an amendment, TRD's order now (Known false when TRD shows none).
type ReviewBasis struct {
	Known bool
	Order OrderFact
}

// OrderFact is what an amendment's review reads of TRD's order (ActiveOrderRef v1): its revision and
// hash and the lines of that revision.
type OrderFact struct {
	Revision int64
	Hash     string
	Lines    []api.ReviewTarget
}

// PrepareReview reads, outside any transaction, what order change e needs from TRD: for
// OrderAmended, TRD's order (ActiveOrderRef v1 through FUL's port Orders, R27), whose lines are the
// amended revision's while TRD is still at it. An order's end needs nothing from TRD: every prepared
// shipment of an ended order is stale.
func (s Service) PrepareReview(ctx context.Context, e OrderChange) (ReviewBasis, error) {
	if e.Cause != api.CauseOrderAmended {
		return ReviewBasis{}, nil
	}
	orders, err := s.Upstream.Orders.ResolveActiveOrders(ctx, []string{e.OrderID})
	if err != nil {
		return ReviewBasis{}, err
	}
	o, ok := orders[e.OrderID]
	if !ok {
		return ReviewBasis{}, nil
	}
	b := ReviewBasis{Known: true, Order: OrderFact{Revision: o.Binding.OrderRevision, Hash: o.Binding.OrderHash, Lines: []api.ReviewTarget{}}}
	for _, l := range o.Lines {
		b.Order.Lines = append(b.Order.Lines, api.ReviewTarget{OrderLineID: l.LineID, Quantity: l.Quantity, SkuID: l.Sku.SkuID,
			SpecRevision: l.Sku.SpecRevision, UnitID: l.UnitRef.ID})
	}
	return b, nil
}

// targets are the amended revision's lines after change e, or the effect that leaves nothing to
// review, or a *ReviewMismatch. An end states none.
func targets(e OrderChange, b ReviewBasis) ([]api.ReviewTarget, string, error) {
	if e.Cause != api.CauseOrderAmended {
		return nil, "", nil
	}
	switch o := b.Order; {
	case !b.Known:
		return nil, "", &ReviewMismatch{Code: FailureReviewNotOpenable, Detail: "TRD shows no order " + e.OrderID}
	case o.Revision > e.Revision:
		return nil, EffectLaterRevision, nil
	case o.Revision != e.Revision || o.Hash != e.Hash:
		return nil, "", &ReviewMismatch{Code: FailureReviewNotOpenable, Detail: fmt.Sprintf("TRD shows order %s at revision %d, the event revision %d",
			e.OrderID, o.Revision, e.Revision)}
	}
	return b.Order.Lines, "", nil
}

// change is cmd as the domain reads it.
func change(cmd api.OpenShipmentReview) (fulfilment.Change, error) {
	c := fulfilment.Change{OrderID: cmd.OrderID, Revision: cmd.OrderRevision, Hash: cmd.OrderHash, Version: cmd.OrderVersion, Cause: cmd.Cause,
		EventID: cmd.EventID, Targets: map[string]fulfilment.Target{}}
	for _, l := range cmd.Lines {
		q, err := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		if err != nil {
			return c, err
		}
		c.Targets[l.OrderLineID] = fulfilment.Target{LineID: l.OrderLineID, Quantity: q, SkuID: l.SkuID, SpecRevision: l.SpecRevision, UnitID: l.UnitID}
	}
	return c, nil
}

// ReviewOrderChange applies TRD's order change e to FUL's shipment reviews with b, read before the
// transaction (shipment_review.open; WF-06 step 5). It locks the order's fulfilment root FOR UPDATE,
// then its reviews, lists what e leaves stale and opens the review of (order, revision, cause) with
// the event ID as key — superseding the order's open reviews of earlier order versions — unless FUL
// never prepared a shipment of the order, such a review exists already (a redelivery, or one settled
// since: nothing is reopened), a review of a later order version exists (e arrives late), TRD has
// moved past the amended revision, or nothing is stale and no review of the order is open. It
// returns what it did.
func (s Service) ReviewOrderChange(ctx context.Context, exec bentopg.Executor, e OrderChange, b ReviewBasis) (string, error) {
	lines, effect, err := targets(e, b)
	if err != nil || effect != "" {
		return effect, err
	}
	cmd := api.OpenShipmentReview{IdempotencyKey: e.EventID, OrderID: e.OrderID, OrderRevision: e.Revision, OrderHash: e.Hash,
		OrderVersion: e.Version, Cause: e.Cause, EventID: e.EventID, Lines: lines}
	if err := cmd.Validate(); err != nil {
		return "", &ReviewMismatch{Code: FailureReviewNotOpenable, Detail: err.Error()}
	}
	c, err := change(cmd)
	if err != nil {
		return "", err
	}
	sc := scope(ctx)
	rooted, err := postgres.LockFulfilmentIfAny(ctx, exec, sc, e.OrderID)
	if err != nil || !rooted {
		return string(fulfilment.EffectNothingHeld), err
	}
	reviews, err := postgres.ReviewsOfOrder(ctx, exec, sc, e.OrderID, postgres.ForUpdate)
	if err != nil {
		return "", err
	}
	holdings, err := postgres.OrderHoldings(ctx, exec, sc, e.OrderID)
	if err != nil {
		return "", err
	}
	h := &openReview{s: s, change: c}
	h.items = fulfilment.Assess(c, holdings)
	var did fulfilment.Effect
	did, h.superseded = fulfilment.Plan(c, reviews, holdings, h.items)
	if did != fulfilment.EffectOpened {
		return string(did), nil
	}
	_, err = s.run(ctx, action.Command{Name: api.CommandOpenShipmentReview, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OrderID}, cmd, nil, h)
	if rej, ok := kernel.AsRejection(err); ok && rej.Code != kernel.CodePermissionDenied && rej.Code != kernel.CodeVersionConflict {
		return "", &ReviewMismatch{Code: FailureReviewNotOpenable, Detail: fmt.Sprintf("shipment_review.open of order %s revision %d: %s",
			e.OrderID, e.Revision, rej.Error())}
	}
	if err != nil {
		return "", err
	}
	return string(fulfilment.EffectOpened), nil
}

// openReview runs shipment_review.open with what ReviewOrderChange decided in the same transaction,
// after it locked the order's fulfilment root and reviews: Lock has nothing left to lock, and Apply
// writes the review and the supersessions.
type openReview struct {
	s          Service
	change     fulfilment.Change
	items      []fulfilment.Item
	superseded []*fulfilment.Review
}

func (h *openReview) Lock(context.Context, bentopg.Executor) error { return nil }

func (h *openReview) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc := scope(ctx)
	now := kernel.Instant(h.s.Clock.Now())
	r := fulfilment.OpenReview(action.NewID("shr", now), h.change, h.items, fulfilment.Act{By: d.Actor, Epoch: d.Epoch, At: now})
	if err := postgres.InsertReview(ctx, exec, sc, r); err != nil {
		return action.Outcome{}, err
	}
	for _, old := range h.superseded {
		old.Supersede(r.ID, now)
		if err := postgres.SupersedeReview(ctx, exec, sc, old); err != nil {
			return action.Outcome{}, err
		}
	}
	return action.Outcome{ObjectID: r.ID, RowVersion: r.RowVersion, Status: r.Status}, nil
}

// settleReview runs shipment_review.settle. Before the transaction it reads the review's order and
// asks TRD whether the caller reads it; inside, it locks the order's fulfilment root FOR UPDATE, then
// the review, and judges the dispositions on FUL's rows as they are now.
type settleReview struct {
	s       Service
	cmd     api.SettleShipmentReview
	visible bool
	review  *fulfilment.Review
}

func (h *settleReview) Prepare(ctx context.Context) error {
	orderID, err := h.s.reviewOrder(ctx, h.cmd.ReviewID)
	if err != nil || orderID == "" {
		return err
	}
	h.visible, err = h.s.orderVisible(ctx, orderID)
	return err
}

func (h *settleReview) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.review = nil
	sc := scope(ctx)
	r, err := postgres.LoadReview(ctx, exec, sc, h.cmd.ReviewID, postgres.NoLock)
	if err != nil || r == nil {
		return err
	}
	if _, err = postgres.LockFulfilmentIfAny(ctx, exec, sc, r.OrderID); err != nil {
		return err
	}
	h.review, err = postgres.LoadReview(ctx, exec, sc, h.cmd.ReviewID, postgres.ForUpdate)
	return err
}

// Apply refuses, in this order: a review of an order the caller does not read (NOT_FOUND), a record
// that is not intact (REFERENCE_INVALID), then what fulfilment.Review.Settle refuses.
func (h *settleReview) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	r := h.review
	if r == nil || !h.visible {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/review_id", "")
	}
	if err := r.Verify(); err != nil {
		return action.Outcome{}, err
	}
	var ids []string
	for _, it := range r.Items {
		ids = appendID(ids, it.Facts.ShipmentID)
	}
	ss := make([]fulfilment.Settlement, len(h.cmd.Items))
	for i, it := range h.cmd.Items {
		ss[i] = fulfilment.Settlement{ItemNo: it.ItemNo, Kind: it.Disposition, ShipmentID: it.ShipmentID, Reason: it.Reason}
		ids = appendID(ids, it.ShipmentID)
	}
	sc := scope(ctx)
	now, err := postgres.ReviewNow(ctx, exec, sc, ids)
	if err != nil {
		return action.Outcome{}, err
	}
	at := kernel.Instant(h.s.Clock.Now())
	if err = r.Settle(h.cmd.ExpectedVersion, ss, now, fulfilment.Act{By: d.Actor, Epoch: d.Epoch, At: at, Reason: h.cmd.Reason}); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SettleReview(ctx, exec, sc, r); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: r.ID, RowVersion: r.RowVersion, Status: r.Status}, nil
}

func appendID(ids []string, id string) []string {
	if id == "" || slices.Contains(ids, id) {
		return ids
	}
	return append(ids, id)
}

// SettleShipmentReview settles a shipment review by reference to what was done (shipment_review.settle;
// SM-FUL-SHIPMENT-REVIEW open → settled). Human only.
func (s Service) SettleShipmentReview(ctx context.Context, cmd api.SettleShipmentReview) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: api.CommandSettleShipmentReview, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ReviewID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), &settleReview{s: s, cmd: cmd})
}

// reviewOrder returns the order of review id ("" when there is none), read in a FUL lookup
// transaction of its own.
func (s Service) reviewOrder(ctx context.Context, id string) (string, error) {
	var orderID string
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		r, err := postgres.LoadReview(ctx, exec, scope(ctx), id, postgres.NoLock)
		if r != nil {
			orderID = r.OrderID
		}
		return err
	})
	return orderID, err
}

// orderVisible asks TRD (ActiveOrderRef v1, R27) whether the caller reads order id.
func (s Service) orderVisible(ctx context.Context, id string) (bool, error) {
	orders, err := s.Upstream.Orders.ResolveActiveOrders(ctx, []string{id})
	_, ok := orders[id]
	return ok, err
}

// GetShipmentReview returns a review of an order the caller reads (TRD's lookup decides that, before
// FUL's read transaction), audited as shipment_review.get (ful.read): NOT_FOUND otherwise;
// REFERENCE_INVALID when its record is not intact.
func (s Service) GetShipmentReview(ctx context.Context, id string) (api.ShipmentReviewView, error) {
	if !kernel.ValidIdentifier(id) {
		return api.ShipmentReviewView{}, invalidID(s, ctx, api.CommandGetShipmentReview)
	}
	orderID, err := s.reviewOrder(ctx, id)
	if err != nil {
		return api.ShipmentReviewView{}, err
	}
	visible := false
	if orderID != "" {
		if visible, err = s.orderVisible(ctx, orderID); err != nil {
			return api.ShipmentReviewView{}, err
		}
	}
	var v api.ShipmentReviewView
	err = s.Ledger.Read(ctx, api.CommandGetShipmentReview, id, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		r, err := postgres.LoadReview(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if r == nil || !visible || r.OrderID != orderID {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		if err = r.Verify(); err != nil {
			return err
		}
		v = reviewView(r)
		return nil
	})
	return v, err
}

// ListShipmentReviews returns the reviews of an order the caller reads, by order version, audited as
// shipment_review.list (ful.read): NOT_FOUND for an order TRD does not show the caller.
func (s Service) ListShipmentReviews(ctx context.Context, orderID string) (api.OrderShipmentReviews, error) {
	if !kernel.ValidIdentifier(orderID) {
		return api.OrderShipmentReviews{}, invalidID(s, ctx, api.CommandListShipmentReviews)
	}
	visible, err := s.orderVisible(ctx, orderID)
	if err != nil {
		return api.OrderShipmentReviews{}, err
	}
	out := api.OrderShipmentReviews{OrderID: orderID, ShipmentReviews: []api.ShipmentReviewView{}}
	err = s.Ledger.Read(ctx, api.CommandListShipmentReviews, orderID, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		if !visible {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		rs, err := postgres.ReviewsOfOrder(ctx, exec, scope(ctx), orderID, postgres.NoLock)
		if err != nil {
			return err
		}
		for _, r := range rs {
			if err = r.Verify(); err != nil {
				return err
			}
			out.ShipmentReviews = append(out.ShipmentReviews, reviewView(r))
		}
		return nil
	})
	return out, err
}

// reviewView is a stored, verified review as the reads show it.
func reviewView(r *fulfilment.Review) api.ShipmentReviewView {
	v := api.ShipmentReviewView{ReviewID: r.ID, OrderID: r.OrderID, OrderRevision: r.Revision, OrderHash: r.Hash, OrderVersion: r.Version,
		Cause: r.Cause, EventID: r.EventID, Status: r.Status, RowVersion: r.RowVersion, ResponsibleRole: r.Role,
		Items: []api.ShipmentReviewItemView{}, OpenedBy: r.Opened.By, OpenedAt: r.Opened.At, OpenEpoch: r.Opened.Epoch,
		SupersededBy: r.SupersededBy, SupersededAt: r.SupersededAt}
	if s := r.Settled; s != nil {
		v.SettledBy, v.SettledAt, v.SettleEpoch, v.SettleReason = s.By, s.At, s.Epoch, s.Reason
	}
	for _, it := range r.Items {
		f := it.Facts
		iv := api.ShipmentReviewItemView{ItemNo: it.No, Kind: f.Kind, ShipmentID: f.ShipmentID, ShipmentStatus: f.ShipmentStatus,
			ShipmentVersion: f.ShipmentVersion, ShipmentRevision: f.ShipmentRevision, Impact: f.Impact, Lines: []api.ReviewLineView{},
			PackingKind: f.PackingKind, PackingSeq: f.PackingSeq, Cartons: f.Cartons, Packages: f.Packages, BookingVersion: f.BookingVersion,
			BookingNumber: f.BookingNumber, Vessel: f.Vessel, Voyage: f.Voyage, ETD: f.ETD, DocumentSetID: f.DocumentSetID,
			DocumentKind: f.DocumentKind, DocumentVersion: f.DocumentVersion, DocumentNumber: f.DocumentNumber, ReleaseID: f.ReleaseID,
			ReleaseScope: f.ReleaseScope, ReleaseStatus: f.ReleaseStatus, ReleaseVersion: f.ReleaseVersion, ReleaseEvaluations: f.ReleaseEvaluations,
			Facts: f.JSON()}
		for _, l := range f.Lines {
			iv.Lines = append(iv.Lines, api.ReviewLineView(l))
		}
		if d := it.Done; d != nil {
			iv.Disposition = &api.ReviewDispositionView{Disposition: d.Kind, ShipmentID: d.ShipmentID, Revision: d.Revision, Ref: d.Ref,
				Status: d.Status, At: d.At, Reason: d.Reason}
		}
		v.Items = append(v.Items, iv)
	}
	return v
}
