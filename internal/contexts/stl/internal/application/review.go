package application

import (
	"context"
	"fmt"
	"slices"
	"strings"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	oa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderaccount"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The payment plan review (unit C4b-STL; SM-STL-PAYMENT-PLAN-REVIEW; WF-06; S5 plan D-4, D-21).
// payment_plan_review.open reads TRD's order and its currency's POL revision before the transaction
// (or takes them from the OrderAmended consumer's Prepare step); inside it, it locks the order's
// account FOR UPDATE, then the ledger locks the authority guard, then POL's ConfigGuard locks the
// currency revision FOR SHARE. payment_plan_review.settle locks the account FOR UPDATE and reads
// nothing outside STL. The account root serializes both against application.apply and
// application.unapply (which lock it FOR UPDATE too) and against PaymentConditionGuard v1 (FOR
// SHARE). Neither command publishes an event: the tactical model gives OrderAccount none.

// responsibleRole names the role templates that hold payment_plan_review.settle (D-21: a review has
// a responsible role), comma-separated in code order.
func responsibleRole() string {
	var codes []string
	for _, t := range authorization.Templates() {
		if slices.Contains(t.Permissions, api.PermissionSettleReview) {
			codes = append(codes, t.Code)
		}
	}
	slices.Sort(codes)
	return strings.Join(codes, ",")
}

// stagesOf returns TRD's stages of an order revision as an account holds them, in currency cur.
func stagesOf(stages []OrderStage, cur kernel.Currency) ([]oa.Stage, error) {
	out := make([]oa.Stage, 0, len(stages))
	for _, s := range stages {
		m, err := kernel.ParseMinor(cur, s.AmountMinor)
		if err != nil {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "TRD's payment stage "+s.StageID+" has no amount")
		}
		out = append(out, oa.Stage{StageID: s.StageID, Trigger: s.Trigger, Days: s.Days, Percent: s.Percent, Amount: m, ReleaseControl: s.ReleaseControl})
	}
	return out, nil
}

// openReview runs payment_plan_review.open.
type openReview struct {
	s        Service
	cmd      api.OpenPaymentPlanReview
	order    *Order
	currency polapi.ResolvedConfig
	account  *oa.Account
	same     *oa.Review // the review of the revision asked for, when there is one
	open     *oa.Review // the account's open review
	applied  map[string]kernel.Money
	replay   bool
	review   *oa.Review
}

func (h *openReview) Prepare(ctx context.Context) error {
	orders, err := h.s.Upstream.Orders.ResolveOrders(ctx, []string{h.cmd.OrderID})
	if err != nil {
		return err
	}
	o, ok := orders[h.cmd.OrderID]
	if !ok {
		return nil
	}
	h.order = &o
	configs, err := h.s.resolveConfigs(ctx, h.s.Clock.Now(), map[string]polapi.ConfigRef{"/args/order_id": {Kind: polapi.KindCurrency,
		ConfigID: o.Currency.ID, Revision: o.Currency.Revision}})
	if err == nil {
		h.currency = configs["/args/order_id"]
	}
	return err
}

func (h *openReview) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.account, h.same, h.open, h.applied, h.replay, h.review = nil, nil, nil, nil, false, nil
	sc := scope(ctx)
	var err error
	if h.account, err = postgres.LoadAccount(ctx, exec, sc, h.cmd.OrderID, true); err != nil || h.account == nil {
		return err
	}
	cur := h.account.Currency()
	if h.same, err = postgres.LoadReview(ctx, exec, sc, h.cmd.OrderID, h.cmd.OrderRevision, cur); err != nil {
		return err
	}
	if h.open, err = postgres.LoadOpenReview(ctx, exec, sc, h.cmd.OrderID, cur); err != nil {
		return err
	}
	h.applied, err = postgres.StageApplied(ctx, exec, sc, h.account)
	return err
}

func (h *openReview) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, o, a, now := scope(ctx), h.cmd, h.order, h.account, kernel.Instant(h.s.Clock.Now())
	if a != nil && !d.Sees(a.OwnerActor, a.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/order_id", "")
	}
	if r := h.same; r != nil {
		if r.OrderHash != c.OrderHash {
			return action.Outcome{}, api.Reject(api.CodeReferenceStale, "/args/order_hash", "The review of this revision binds another order hash")
		}
		h.replay, h.review = true, r
		return action.Outcome{ObjectID: a.OrderID, RowVersion: a.Version, Status: r.Status}, nil
	}
	switch {
	case o == nil:
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/order_id", "")
	case o.Code != "":
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "TRD's order revision is not intact")
	case o.Binding.OrderRevision != c.OrderRevision:
		return action.Outcome{}, api.Reject(api.CodeReferenceStale, "/args/order_revision", "TRD's order is at another revision")
	case o.Binding.OrderHash != c.OrderHash:
		return action.Outcome{}, api.Reject(api.CodeReferenceStale, "/args/order_hash", "TRD's order revision has another hash")
	case o.Status != OrderActive:
		return action.Outcome{}, api.Reject(api.CodeOrderNotActive, "/args/order_id", "The order is "+o.Status)
	case a != nil && a.OrderRevision >= c.OrderRevision:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/order_revision",
			fmt.Sprintf("The order account holds revision %d", a.OrderRevision))
	}
	cur, err := currencyOf(h.currency, "/args/order_id")
	if err != nil {
		return action.Outcome{}, err
	}
	if err = guardConfig(ctx, exec, h.currency.Ref, h.currency.ContentHash, now, "/args/order_id"); err != nil {
		return action.Outcome{}, err
	}
	stages, err := stagesOf(o.Stages, cur)
	if err != nil {
		return action.Outcome{}, err
	}
	act := fundAct(d, now, "")
	if a == nil {
		if a, err = oa.Open(c.OrderID, c.OrderRevision, c.OrderHash, o.CustomerID, o.Currency.Revision, stages, act, d.Team); err != nil {
			return action.Outcome{}, err
		}
		if err = postgres.InsertAccount(ctx, exec, sc, a); err != nil {
			return action.Outcome{}, err
		}
		h.review = a.Begin(responsibleRole(), act)
	} else {
		if h.review, err = a.Move(c.OrderRevision, c.OrderHash, o.Currency.Revision, stages, h.applied, responsibleRole(), act); err != nil {
			return action.Outcome{}, err
		}
		if old := h.open; old != nil {
			if err = old.Supersede(c.OrderRevision, now); err != nil {
				return action.Outcome{}, err
			}
			if err = postgres.SupersedeReview(ctx, exec, sc, old); err != nil {
				return action.Outcome{}, err
			}
		}
		if err = postgres.MoveAccount(ctx, exec, sc, a, now); err != nil {
			return action.Outcome{}, err
		}
	}
	if err = postgres.InsertReview(ctx, exec, sc, h.review); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: a.OrderID, RowVersion: a.Version, Status: h.review.Status}, nil
}

// OpenPaymentPlanReview moves an active order's account to TRD's current revision and opens its
// payment plan review (payment_plan_review.open).
func (s Service) OpenPaymentPlanReview(ctx context.Context, cmd api.OpenPaymentPlanReview) (api.PaymentPlanReviewReceipt, error) {
	h := &openReview{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandOpenReview, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OrderID}, cmd,
		cmd.Validate(), h)
	if err != nil {
		return api.PaymentPlanReviewReceipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return s.reviewReceipt(ctx, res, cmd.OrderRevision)
}

// settleReview runs payment_plan_review.settle.
type settleReview struct {
	s       Service
	cmd     api.SettlePaymentPlanReview
	account *oa.Account
	review  *oa.Review
	applied map[string]kernel.Money
}

func (h *settleReview) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.account, h.review, h.applied = nil, nil, nil
	sc := scope(ctx)
	var err error
	if h.account, err = postgres.LoadAccount(ctx, exec, sc, h.cmd.OrderID, true); err != nil || h.account == nil {
		return err
	}
	if h.review, err = postgres.LoadReview(ctx, exec, sc, h.cmd.OrderID, h.cmd.OrderRevision, h.account.Currency()); err != nil {
		return err
	}
	h.applied, err = postgres.StageApplied(ctx, exec, sc, h.account)
	return err
}

func (h *settleReview) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, a, r, now := scope(ctx), h.cmd, h.account, h.review, kernel.Instant(h.s.Clock.Now())
	switch {
	case a == nil || !d.Sees(a.OwnerActor, a.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/order_id", "")
	case r == nil:
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/order_revision", "")
	case a.Version != c.ExpectedVersion:
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	ds := make([]oa.Disposition, 0, len(c.Items))
	for _, it := range c.Items {
		ds = append(ds, oa.Disposition{Seq: it.Seq, Disposition: it.Disposition, Note: it.Note})
	}
	if err := r.Settle(a, ds, h.applied, fundAct(d, now, c.Reason)); err != nil {
		return action.Outcome{}, err
	}
	if err := postgres.BumpAccount(ctx, exec, sc, a, now); err != nil {
		return action.Outcome{}, err
	}
	if err := postgres.SettleReview(ctx, exec, sc, r); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: a.OrderID, RowVersion: a.Version, Status: r.Status}, nil
}

// SettlePaymentPlanReview settles the open review of the revision an order account holds
// (payment_plan_review.settle). Human only.
func (s Service) SettlePaymentPlanReview(ctx context.Context, cmd api.SettlePaymentPlanReview) (api.PaymentPlanReviewReceipt, error) {
	h := &settleReview{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandSettleReview, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OrderID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.PaymentPlanReviewReceipt{}, err
	}
	return s.reviewReceipt(ctx, res, cmd.OrderRevision)
}

// reviewReceipt completes res with the stored review of the order's revision.
func (s Service) reviewReceipt(ctx context.Context, res api.Receipt, revision int64) (api.PaymentPlanReviewReceipt, error) {
	out := api.PaymentPlanReviewReceipt{Receipt: res}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		a, err := postgres.LoadAccount(ctx, exec, sc, res.ObjectID, false)
		if err != nil || a == nil {
			return err
		}
		r, err := postgres.LoadReview(ctx, exec, sc, res.ObjectID, revision, a.Currency())
		if err != nil || r == nil {
			return err
		}
		out.OrderRevision, out.OrderHash, out.FromRevision, out.Items = r.OrderRevision, r.OrderHash, r.FromRevision, len(r.Items)
		return nil
	})
	return out, err
}

func reviewView(r *oa.Review) api.PaymentPlanReviewView {
	v := api.PaymentPlanReviewView{OrderRevision: r.OrderRevision, OrderHash: r.OrderHash, FromRevision: r.FromRevision, FromHash: r.FromHash,
		ResponsibleRole: r.ResponsibleRole, Status: r.Status, Version: r.Version, Items: []api.ReviewItemView{}, OpenedBy: r.Opened.By,
		OpenedAt: r.Opened.At, SupersededBy: r.SupersededBy, SupersededAt: r.SupersededAt}
	v.SettledBy, v.SettledAt, v.SettleReason = actOf(r.Settled)
	for _, it := range r.Items {
		v.Items = append(v.Items, api.ReviewItemView{Seq: it.Seq, Kind: it.Kind, StageID: it.StageID, FromMinor: it.From.MinorString(),
			ToMinor: it.To.MinorString(), AppliedMinor: it.Applied.MinorString(), Disposition: it.Disposition, Note: it.Note})
	}
	return v
}

// ListPaymentPlanReviews returns an order account's payment plan reviews, latest revision first
// (payment_plan_review.list).
func (s Service) ListPaymentPlanReviews(ctx context.Context, orderID string) (api.PaymentPlanReviewsView, error) {
	var v api.PaymentPlanReviewsView
	err := s.read(ctx, api.CommandListReviews, orderID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		a, err := postgres.LoadAccount(ctx, exec, sc, orderID, false)
		if err != nil {
			return err
		}
		if a == nil || !d.Sees(a.OwnerActor, a.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		reviews, err := postgres.LoadReviews(ctx, exec, sc, orderID, a.Currency())
		if err != nil {
			return err
		}
		v = api.PaymentPlanReviewsView{OrderID: a.OrderID, OrderRevision: a.OrderRevision, AccountVersion: a.Version, Currency: a.Currency().Code(),
			Reviews: []api.PaymentPlanReviewView{}, CheckedAt: kernel.Instant(s.Clock.Now())}
		for i := range reviews {
			v.Reviews = append(v.Reviews, reviewView(&reviews[i]))
		}
		return nil
	})
	return v, err
}
