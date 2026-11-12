package application

import (
	"context"
	"fmt"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/allocation"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// What TRD's order changes do to PUR (WBS S5-09b, PUR part; event storming PL-09, ES-09 step 5;
// WF-06 step 4; S5 decisions D-18, D-21; SM-PUR-ALLOCATION-REVIEW; BT10). The Inbox consumer
// pur.allocation_review hands OrderAmended, OrderCancelled and OrderRemainderClosed to
// ReviewOrderChange inside its transaction on PUR's own transaction owner. TRD is read before that
// transaction (PrepareReview: the amended revision's lines through PUR's port Orders, ActiveOrderRef
// v1, R19); inside it PUR locks the order's DemandAllocation roots FOR UPDATE, then the order's
// reviews, judges PUR's own rows and, when the change touches something or supersedes an open
// review, runs allocation_review.open as the consumer's system actor with the event ID as key.
// allocation_review.settle is a person's command: it settles a review by reference to the
// dispositions made with purchase.draft.void, purchase.commitment.change.* and
// purchase.commitment.cancel.

var reviewCmds = []string{api.CommandOpenAllocationReview, api.CommandSettleAllocationReview}

// What an order change did to PUR's reviews, besides the domain's allocation.Effect values.
const (
	// EffectLaterRevision: TRD's order is at a later revision than the amended one; that revision's
	// OrderAmended reviews it.
	EffectLaterRevision = "later_revision"
)

// FailureReviewNotOpenable is the failure code of an order change PUR cannot review for good: TRD
// shows no such order, or another order revision or hash than the event's, or allocation_review.open
// refused it in a way no retry changes.
const FailureReviewNotOpenable = "pur.allocation_review_not_openable"

// ReviewMismatch is an order change PUR cannot review for good: the consumer refuses the event and
// the relay dead-letters it with an alert (ADR-BR-015). Nothing is changed.
type ReviewMismatch struct {
	Code   string
	Detail string
}

func (m *ReviewMismatch) Error() string { return "pur: " + m.Code + ": " + m.Detail }

// OrderChange is one of TRD's order events as the review consumer reads it: the order at revision
// Revision and hash Hash, TRD's row version of the order after the event (Version), the cause and,
// for an end, each line's shipped quantity in its unit (Lines; empty for a draft order's cancel).
type OrderChange struct {
	EventID  string
	Cause    string
	OrderID  string
	Revision int64
	Hash     string
	Version  int64
	Lines    []EndedLine
}

// EndedLine is one line of an ended order: its shipped quantity (kernel decimal text).
type EndedLine struct {
	LineID  string
	Shipped string
}

// ReviewBasis is what reviewing an order change needs from TRD, read before the consumer's
// transaction: for an amendment, TRD's order now (nil when TRD shows none).
type ReviewBasis struct {
	Order *Order
}

// PrepareReview reads, outside any transaction, what order change e needs from TRD: for
// OrderAmended, TRD's order (ActiveOrderRef v1 through PUR's port Orders, R19), whose lines are the
// amended revision's while TRD is still at it. An order's end carries what PUR needs.
func (s Service) PrepareReview(ctx context.Context, e OrderChange) (ReviewBasis, error) {
	if e.Cause != api.CauseOrderAmended {
		return ReviewBasis{}, nil
	}
	orders, err := s.Upstream.Orders.ResolveOrders(ctx, []string{e.OrderID})
	if err != nil {
		return ReviewBasis{}, err
	}
	if o, ok := orders[e.OrderID]; ok {
		return ReviewBasis{Order: &o}, nil
	}
	return ReviewBasis{}, nil
}

// targets are the order's lines after change e as the review reads them, or the effect that leaves
// nothing to review, or a *ReviewMismatch.
func targets(e OrderChange, b ReviewBasis) ([]api.ReviewTarget, string, error) {
	var out []api.ReviewTarget
	if e.Cause != api.CauseOrderAmended {
		for _, l := range e.Lines {
			q, err := kernel.ParseDecimal(l.Shipped, kernel.QuantityFormat)
			if err != nil || q.Sign() < 0 {
				return nil, "", &ReviewMismatch{Code: FailureReviewNotOpenable, Detail: fmt.Sprintf("line %s of order %s shipped %q", l.LineID, e.OrderID, l.Shipped)}
			}
			out = append(out, api.ReviewTarget{OrderLineID: l.LineID, Quantity: text(q)})
		}
		return out, "", nil
	}
	o := b.Order
	switch {
	case o == nil:
		return nil, "", &ReviewMismatch{Code: FailureReviewNotOpenable, Detail: "TRD shows no order " + e.OrderID}
	case o.Binding.OrderRevision > e.Revision:
		return nil, EffectLaterRevision, nil
	case o.Binding.OrderRevision != e.Revision || o.Binding.OrderHash != e.Hash:
		return nil, "", &ReviewMismatch{Code: FailureReviewNotOpenable, Detail: fmt.Sprintf("TRD shows order %s at revision %d, the event revision %d",
			e.OrderID, o.Binding.OrderRevision, e.Revision)}
	}
	for _, l := range o.Lines {
		out = append(out, api.ReviewTarget{OrderLineID: l.LineID, Quantity: l.Quantity, SkuID: l.SkuID, SpecRevision: l.SpecRevision, UnitID: l.Unit.ID})
	}
	return out, "", nil
}

// change is cmd as the domain reads it.
func change(cmd api.OpenAllocationReview) (allocation.Change, error) {
	c := allocation.Change{OrderID: cmd.OrderID, Revision: cmd.OrderRevision, Hash: cmd.OrderHash, Version: cmd.OrderVersion, Cause: cmd.Cause,
		EventID: cmd.EventID, Targets: map[string]allocation.Target{}}
	for _, l := range cmd.Lines {
		q, err := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		if err != nil {
			return c, err
		}
		t := allocation.Target{LineID: l.OrderLineID, Quantity: q}
		if l.SkuID != "" {
			t.Spec = &allocation.Spec{SkuID: l.SkuID, SpecRevision: l.SpecRevision, UnitID: l.UnitID}
		}
		c.Targets[l.OrderLineID] = t
	}
	return c, nil
}

// ReviewOrderChange applies TRD's order change e to PUR's allocation reviews with b, read before the
// transaction (allocation_review.open; WF-06 step 4). It locks the order's DemandAllocation roots FOR
// UPDATE, then its reviews, lists what e touches and opens the review of (order, revision, cause)
// with the event ID as key — superseding the order's open reviews of earlier order versions — unless
// such a review exists already (a redelivery, or one settled since: nothing is reopened), a review of
// a later order version exists (e arrives late), TRD has moved past the amended revision, or nothing
// is touched and no review of the order is open. It returns what it did.
func (s Service) ReviewOrderChange(ctx context.Context, exec bentopg.Executor, e OrderChange, b ReviewBasis) (string, error) {
	lines, effect, err := targets(e, b)
	if err != nil || effect != "" {
		return effect, err
	}
	cmd := api.OpenAllocationReview{IdempotencyKey: e.EventID, OrderID: e.OrderID, OrderRevision: e.Revision, OrderHash: e.Hash,
		OrderVersion: e.Version, Cause: e.Cause, EventID: e.EventID, Lines: lines}
	if err := cmd.Validate(); err != nil {
		return "", &ReviewMismatch{Code: FailureReviewNotOpenable, Detail: err.Error()}
	}
	c, err := change(cmd)
	if err != nil {
		return "", err
	}
	sc := scope(ctx)
	h := &openReview{s: s, cmd: cmd}
	if h.holdings.Allocations, err = postgres.LockOrderAllocations(ctx, exec, sc, e.OrderID); err != nil {
		return "", err
	}
	reviews, err := postgres.ReviewsOfOrder(ctx, exec, sc, e.OrderID, postgres.ForUpdate)
	if err != nil {
		return "", err
	}
	if h.holdings.Drafts, h.holdings.Commitments, err = postgres.OrderHoldings(ctx, exec, sc, e.OrderID); err != nil {
		return "", err
	}
	h.items = allocation.Assess(c, h.holdings)
	var did allocation.Effect
	did, h.superseded = allocation.Plan(c, reviews, h.holdings, h.items)
	if did != allocation.EffectOpened {
		return string(did), nil
	}
	h.change = c
	_, err = s.run(ctx, action.Command{Name: api.CommandOpenAllocationReview, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OrderID}, cmd,
		nil, h)
	if rej, ok := kernel.AsRejection(err); ok && rej.Code != kernel.CodePermissionDenied && rej.Code != kernel.CodeVersionConflict {
		return "", &ReviewMismatch{Code: FailureReviewNotOpenable, Detail: fmt.Sprintf("allocation_review.open of order %s revision %d: %s",
			e.OrderID, e.Revision, rej.Error())}
	}
	if err != nil {
		return "", err
	}
	return string(allocation.EffectOpened), nil
}

// openReview runs allocation_review.open with what ReviewOrderChange decided in the same
// transaction, after it locked the order's allocation roots and reviews: Lock has nothing left to
// lock, and Apply writes the review and the supersessions.
type openReview struct {
	s          Service
	cmd        api.OpenAllocationReview
	change     allocation.Change
	holdings   allocation.Holdings
	items      []allocation.Item
	superseded []*allocation.Review
}

func (h *openReview) Lock(context.Context, bentopg.Executor) error { return nil }

func (h *openReview) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc := scope(ctx)
	now := kernel.Instant(h.s.Clock.Now())
	r := allocation.OpenReview(action.NewID("alr", now), h.change, h.items, allocation.Act{By: d.Actor, Epoch: d.Epoch, At: now})
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

// settleReview runs allocation_review.settle. Before the transaction it reads the review's order and
// asks TRD whether the caller reads it; inside, it locks the order's DemandAllocation roots FOR
// UPDATE, then the review, and judges the dispositions on PUR's rows as they are now.
type settleReview struct {
	s       Service
	cmd     api.SettleAllocationReview
	visible bool
	review  *allocation.Review
}

func (h *settleReview) Prepare(ctx context.Context) error {
	var orderID string
	if err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		r, err := postgres.LoadReview(ctx, exec, scope(ctx), h.cmd.ReviewID, postgres.NoLock)
		if r != nil {
			orderID = r.OrderID
		}
		return err
	}); err != nil || orderID == "" {
		return err
	}
	orders, err := h.s.Upstream.Orders.ResolveOrders(ctx, []string{orderID})
	_, h.visible = orders[orderID]
	return err
}

func (h *settleReview) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.review = nil
	sc := scope(ctx)
	r, err := postgres.LoadReview(ctx, exec, sc, h.cmd.ReviewID, postgres.NoLock)
	if err != nil || r == nil {
		return err
	}
	if _, err = postgres.LockOrderAllocations(ctx, exec, sc, r.OrderID); err != nil {
		return err
	}
	h.review, err = postgres.LoadReview(ctx, exec, sc, h.cmd.ReviewID, postgres.ForUpdate)
	return err
}

// Apply refuses, in this order: a review of an order the caller does not read (NOT_FOUND), a record
// that is not intact (REFERENCE_INVALID), then what allocation.Review.Settle refuses.
func (h *settleReview) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	r := h.review
	if r == nil || !h.visible {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/review_id", "")
	}
	if err := r.Verify(); err != nil {
		return action.Outcome{}, err
	}
	var drafts, commitments, changes []string
	ss := make([]allocation.Settlement, len(h.cmd.Items))
	for i, it := range h.cmd.Items {
		ss[i] = allocation.Settlement{ItemNo: it.ItemNo, Kind: it.Disposition, DraftID: it.DraftID, ChangeID: it.ChangeID, CommitmentID: it.CommitmentID,
			Reason: it.Reason}
		drafts, commitments, changes = appendID(drafts, it.DraftID), appendID(commitments, it.CommitmentID), appendID(changes, it.ChangeID)
	}
	sc := scope(ctx)
	now, err := postgres.ReviewNow(ctx, exec, sc, r.OrderID, drafts, commitments, changes)
	if err != nil {
		return action.Outcome{}, err
	}
	at := kernel.Instant(h.s.Clock.Now())
	if err = r.Settle(h.cmd.ExpectedVersion, ss, now, allocation.Act{By: d.Actor, Epoch: d.Epoch, At: at, Reason: h.cmd.Reason}); err != nil {
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

// SettleAllocationReview settles an allocation review by reference to the dispositions made
// (allocation_review.settle; SM-PUR-ALLOCATION-REVIEW open → settled). Human only.
func (s Service) SettleAllocationReview(ctx context.Context, cmd api.SettleAllocationReview) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: api.CommandSettleAllocationReview, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ReviewID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), &settleReview{s: s, cmd: cmd})
}

// GetAllocationReview returns a review of an order the caller reads (TRD's lookup decides that,
// before PUR's read transaction), audited as allocation_review.get (pur.read): NOT_FOUND otherwise;
// REFERENCE_INVALID when its record is not intact.
func (s Service) GetAllocationReview(ctx context.Context, id string) (api.AllocationReviewView, error) {
	if !kernel.ValidIdentifier(id) {
		err := kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
		if auditErr := s.Ledger.AuditRejected(ctx, api.CommandGetAllocationReview, "", kernel.CodeSchemaInvalid); auditErr != nil {
			return api.AllocationReviewView{}, auditErr
		}
		return api.AllocationReviewView{}, err
	}
	var orderID string
	if err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		r, err := postgres.LoadReview(ctx, exec, scope(ctx), id, postgres.NoLock)
		if r != nil {
			orderID = r.OrderID
		}
		return err
	}); err != nil {
		return api.AllocationReviewView{}, err
	}
	visible := false
	if orderID != "" {
		orders, err := s.Upstream.Orders.ResolveOrders(ctx, []string{orderID})
		if err != nil {
			return api.AllocationReviewView{}, err
		}
		_, visible = orders[orderID]
	}
	var v api.AllocationReviewView
	err := s.Ledger.Read(ctx, api.CommandGetAllocationReview, id, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
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

// GetAllocationReviews returns the reviews of an order the caller reads, by order version, audited as
// allocation_review.list (pur.read).
func (s Service) GetAllocationReviews(ctx context.Context, orderID string) ([]api.AllocationReviewView, error) {
	if !kernel.ValidIdentifier(orderID) {
		err := kernel.Reject(kernel.CodeSchemaInvalid, "/order_id", "")
		if auditErr := s.Ledger.AuditRejected(ctx, api.CommandGetAllocationReviews, "", kernel.CodeSchemaInvalid); auditErr != nil {
			return nil, auditErr
		}
		return nil, err
	}
	orders, err := s.Upstream.Orders.ResolveOrders(ctx, []string{orderID})
	if err != nil {
		return nil, err
	}
	_, visible := orders[orderID]
	var out []api.AllocationReviewView
	err = s.Ledger.Read(ctx, api.CommandGetAllocationReviews, orderID, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		if !visible {
			return kernel.Reject(kernel.CodeNotFound, "/order_id", "")
		}
		rs, err := postgres.ReviewsOfOrder(ctx, exec, scope(ctx), orderID, postgres.NoLock)
		if err != nil {
			return err
		}
		out = []api.AllocationReviewView{}
		for _, r := range rs {
			if err = r.Verify(); err != nil {
				return err
			}
			out = append(out, reviewView(r))
		}
		return nil
	})
	return out, err
}

// reviewView is a stored, verified review as the reads show it.
func reviewView(r *allocation.Review) api.AllocationReviewView {
	v := api.AllocationReviewView{ReviewID: r.ID, OrderID: r.OrderID, OrderRevision: r.Revision, OrderHash: r.Hash, OrderVersion: r.Version,
		Cause: r.Cause, EventID: r.EventID, Status: r.Status, RowVersion: r.RowVersion, ResponsibleRole: r.Role, Items: []api.ReviewItemView{},
		OpenedBy: r.Opened.By, OpenedAt: r.Opened.At, OpenEpoch: r.Opened.Epoch, SupersededBy: r.SupersededBy, SupersededAt: r.SupersededAt}
	if s := r.Settled; s != nil {
		v.SettledBy, v.SettledAt, v.SettleEpoch, v.SettleReason = s.By, s.At, s.Epoch, s.Reason
	}
	for _, it := range r.Items {
		f := it.Facts
		iv := api.ReviewItemView{ItemNo: it.No, Kind: f.Kind, OrderLineID: f.OrderLineID, DraftID: f.DraftID, DraftHash: f.DraftHash,
			CommitmentID: f.CommitmentID, CommitmentRevision: f.CommitmentRevision, Status: f.Status, SupplierID: f.SupplierID, PONumber: f.PONumber,
			Fulfilled: f.Fulfilled, Lines: []api.ReviewLineView{}, Holders: []api.ReviewHolderView{}, Facts: f.FactsJSON()}
		for _, l := range f.Lines {
			iv.Lines = append(iv.Lines, api.ReviewLineView(l))
		}
		for _, hd := range f.Holders {
			iv.Holders = append(iv.Holders, api.ReviewHolderView(hd))
		}
		if d := it.Done; d != nil {
			iv.Disposition = &api.ReviewDispositionView{Disposition: d.Kind, DraftID: d.DraftID, ChangeID: d.ChangeID, CommitmentID: d.CommitmentID,
				Revision: d.Revision, At: d.At, Reason: d.Reason}
		}
		v.Items = append(v.Items, iv)
	}
	return v
}
