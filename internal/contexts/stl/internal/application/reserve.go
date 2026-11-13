package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// reserveCredit runs credit.reserve. Before the transaction it reads the order (TRD), the credit
// policy and the order's currency (POL) and, when the order is in another currency than the policy,
// the credit rate (POL LookupFX). Inside it, it locks the customer's credit root FOR UPDATE
// (creating it), re-checks the policy and the rate with ConfigGuard, and decides on the customer's
// limit versions, reservations, receivables and the order's decisions, all read under that lock.
// The order revision reserved is TRD's current revision or, for an active order, a revision one of
// its amendments proposes (S5 D-9: order.amendment.apply asks CreditGuard v1 for that revision when
// it asks more credit); the stages and credit portion are that revision's.
type reserveCredit struct {
	s         Service
	cmd       api.ReserveCredit
	order     *Order
	policy    polapi.ResolvedConfig
	orderCur  kernel.Currency
	policyCur kernel.Currency
	fxMissing bool
	rate      *polapi.ExchangeRate
	root      *cc.Root
	out       cc.ReserveOutcome
}

func (h *reserveCredit) Prepare(ctx context.Context) error {
	c, now := h.cmd, h.s.Clock.Now()
	orders, err := h.s.Upstream.Orders.ResolveOrders(ctx, []string{c.OrderID})
	if err != nil {
		return err
	}
	o, ok := orders[c.OrderID]
	if !ok {
		return nil
	}
	h.order = &o
	cur := polapi.ConfigRef{Kind: polapi.KindCurrency, ConfigID: o.Currency.ID, Revision: o.Currency.Revision}
	configs, err := h.s.resolveConfigs(ctx, now, map[string]polapi.ConfigRef{"/args/credit_policy": polRef(c.CreditPolicy), "/args/order_id": cur})
	if err != nil {
		return err
	}
	h.policy = configs["/args/credit_policy"]
	p, err := creditPolicy(h.policy, "/args/credit_policy")
	if err != nil {
		return err
	}
	oc := configs["/args/order_id"].Data.Currency
	if oc == nil {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "The order's currency is not a POL currency")
	}
	if h.orderCur, err = kernel.NewCurrency(oc.Code, oc.MinorUnits); err != nil {
		return err
	}
	if p.Currency == oc.Code {
		h.policyCur = h.orderCur
		return nil
	}
	if c.FXRate == nil {
		h.fxMissing = true
		return nil
	}
	ref := polRef(*c.FXRate)
	x, err := h.s.Upstream.Configs.LookupFX(ctx, polapi.FXQuery{FromCurrency: oc.Code, ToCurrency: p.Currency, Purpose: polapi.PurposeCredit, At: now,
		Rate: &ref})
	if err != nil {
		return err
	}
	h.rate, h.policyCur = &x, x.Value.To()
	return nil
}

func (h *reserveCredit) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.root, h.out = nil, cc.ReserveOutcome{}
	if h.order == nil {
		return nil
	}
	var err error
	h.root, err = h.s.lockOrCreate(ctx, exec, h.order.CustomerID)
	return err
}

func (h *reserveCredit) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, now := scope(ctx), h.cmd, kernel.Instant(h.s.Clock.Now())
	o := h.order
	if o == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/order_id", "")
	}
	b := cc.Binding{OrderID: c.OrderID, OrderRevision: c.OrderRevision, OrderHash: c.OrderHash}
	orderStages, portion, known := o.revision(OrderBinding(b))
	switch {
	case o.Code != "":
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "TRD's order revision is not intact")
	case !known && !o.numbers(c.OrderRevision):
		return action.Outcome{}, api.Reject(api.CodeReferenceStale, "/args/order_revision",
			"TRD's order is at another revision, and no amendment of it proposes this one")
	case !known:
		return action.Outcome{}, api.Reject(api.CodeReferenceStale, "/args/order_hash", "TRD's order revision has another hash")
	case o.Status != OrderDraft && o.Status != OrderActive:
		return action.Outcome{}, api.Reject(api.CodeOrderNotActive, "/args/order_id", "The order is "+o.Status)
	case h.fxMissing:
		return action.Outcome{}, api.Reject(api.CodeFXScopeMismatch, "/args/fx_rate", "The order is in another currency than the credit policy: name an fx_rate of purpose credit")
	}
	if err := guardConfig(ctx, exec, polRef(c.CreditPolicy), h.policy.ContentHash, now, "/args/credit_policy"); err != nil {
		return action.Outcome{}, err
	}
	var fx *cc.ConfigUse
	var convert func(kernel.Money) (kernel.Money, error)
	if h.rate != nil {
		if err := guardConfig(ctx, exec, h.rate.Rate, h.rate.ContentHash, now, "/args/fx_rate"); err != nil {
			return action.Outcome{}, err
		}
		fx = &cc.ConfigUse{ID: h.rate.Rate.ConfigID, Revision: h.rate.Rate.Revision, Hash: h.rate.ContentHash}
		convert = func(m kernel.Money) (kernel.Money, error) {
			v, err := h.rate.Value.Convert(m)
			if err != nil {
				return kernel.Money{}, api.Reject(api.CodeFXScopeMismatch, "/args/fx_rate", err.Error())
			}
			return v, nil
		}
	}
	policy, err := policyOf(h.policy, h.policyCur)
	if err != nil {
		return action.Outcome{}, err
	}
	stages := make([]cc.Stage, 0, len(orderStages))
	for _, s := range orderStages {
		m, err := kernel.ParseMinor(h.orderCur, s.AmountMinor)
		if err != nil {
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "TRD's payment stage "+s.StageID+" has no amount")
		}
		stages = append(stages, cc.Stage{StageID: s.StageID, Trigger: s.Trigger, ReleaseControl: s.ReleaseControl, Amount: m})
	}
	versions, err := postgres.LoadVersions(ctx, exec, sc, h.root.PartyID)
	if err != nil {
		return action.Outcome{}, err
	}
	reservations, err := postgres.LoadReservations(ctx, exec, sc, h.root.PartyID)
	if err != nil {
		return action.Outcome{}, err
	}
	decisions, err := postgres.LoadOrderDecisions(ctx, exec, sc, c.OrderID, c.OrderRevision)
	if err != nil {
		return action.Outcome{}, err
	}
	receivables, err := receivablesOf(ctx, exec, sc, h.root.PartyID, policy.Currency, reservations)
	if err != nil {
		return action.Outcome{}, err
	}
	h.out, err = cc.Reserve(cc.ReserveInput{ID: c.ReservationID, PartyID: h.root.PartyID, Binding: b, OrderCurrency: h.orderCur, Stages: stages,
		Convert: convert, FX: fx, Policy: policy, Versions: versions, Reservations: reservations, Decisions: decisions, Receivables: receivables,
		By: cc.Act{By: d.Actor, Epoch: d.Epoch, At: now}, Team: d.Team, NewDecisionID: action.NewID("cdec", now)})
	if err != nil {
		return action.Outcome{}, err
	}
	out := h.out
	if out.Existing == nil && policy.DocumentsSecured && portion != "" && portion != out.Portion.MinorString() {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id",
			"TRD's credit portion "+portion+" is not its stages' "+out.Portion.MinorString())
	}
	switch {
	case out.Existing != nil:
		r := out.Existing
		if !d.Sees(r.OwnerActor, r.OwnerTeam) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/reservation_id", "")
		}
		return action.Outcome{ObjectID: r.ID, RowVersion: r.Version, Status: r.Status}, nil
	case out.Decision != nil && !out.NewDecision:
		x := out.Decision
		return action.Outcome{ObjectID: x.ID, RowVersion: x.Version, Status: api.StatusDecisionOpened}, nil
	}
	if err = h.s.bump(ctx, exec, h.root); err != nil {
		return action.Outcome{}, err
	}
	var events []action.Event
	if r := out.Expired; r != nil {
		from := r.Version
		if err = r.Release(cc.Act{By: d.Actor, Epoch: d.Epoch, At: now, Reason: cc.ReleaseExpired}); err != nil {
			return action.Outcome{}, err
		}
		if err = postgres.UpdateReservation(ctx, exec, sc, r, from); err != nil {
			return action.Outcome{}, err
		}
		events = append(events, event(api.EventCreditReservationReleased, h.root, map[string]any{"reservation_id": r.ID,
			"reservation_version": r.Version, "order_id": r.Binding.OrderID, "order_revision": r.Binding.OrderRevision}))
	}
	if x := out.Decision; x != nil {
		if err = postgres.InsertDecision(ctx, exec, sc, x); err != nil {
			return action.Outcome{}, err
		}
		events = append(events, event(api.EventCreditDecisionOpened, h.root, map[string]any{"decision_id": x.ID, "decision_version": x.Version,
			"order_id": x.Binding.OrderID, "order_revision": x.Binding.OrderRevision, "order_hash": x.Binding.OrderHash, "reasons": x.Reasons}))
		return action.Outcome{ObjectID: x.ID, RowVersion: x.Version, Status: api.StatusDecisionOpened, Events: events}, nil
	}
	r := out.Reservation
	if err = postgres.InsertReservation(ctx, exec, sc, r); err != nil {
		return action.Outcome{}, err
	}
	fields := map[string]any{"reservation_id": r.ID, "reservation_version": r.Version, "order_id": r.Binding.OrderID,
		"order_revision": r.Binding.OrderRevision, "order_hash": r.Binding.OrderHash, "basis": r.Basis}
	if r.DecisionID != "" {
		fields["decision_id"] = r.DecisionID
	}
	events = append(events, event(api.EventCreditReserved, h.root, fields))
	return action.Outcome{ObjectID: r.ID, RowVersion: r.Version, Status: r.Status, Events: events}, nil
}

// ReserveCredit reserves credit for an order revision, or opens a credit decision (credit.reserve;
// STL-INV-004, WF-08).
func (s Service) ReserveCredit(ctx context.Context, cmd api.ReserveCredit) (api.ReservationReceipt, error) {
	h := &reserveCredit{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandReserve, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ReservationID}, cmd,
		cmd.Validate(), h)
	if err != nil {
		return api.ReservationReceipt{}, err
	}
	res.Replayed = res.Replayed || h.out.Existing != nil
	out, err := s.reservationReceipt(ctx, res)
	if err != nil {
		return out, err
	}
	if v := h.out.Verdict; !h.out.Limit.Currency().IsZero() && out.LimitMinor == "" {
		out.LimitCurrency, out.LimitMinor = h.out.Limit.Currency().Code(), h.out.Limit.MinorString()
		if !v.Exposure.Currency().IsZero() {
			out.ExposureMinor, out.ExcessMinor = v.Exposure.MinorString(), v.Excess.MinorString()
		}
	}
	return out, nil
}

// reservationReceipt completes res with what the stored reservation, or the stored decision when
// credit.reserve opened one, says.
func (s Service) reservationReceipt(ctx context.Context, res api.Receipt) (api.ReservationReceipt, error) {
	out := api.ReservationReceipt{Receipt: res}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		if res.Status == api.StatusDecisionOpened {
			x, err := postgres.LoadDecision(ctx, exec, sc, res.ObjectID)
			if err != nil || x == nil {
				return err
			}
			out.PartyID, out.DecisionID, out.Reasons, out.ExpiresAt = x.PartyID, x.ID, x.Reasons, x.ExpiresAt
			out.OrderID, out.OrderRevision, out.OrderHash = x.Binding.OrderID, x.Binding.OrderRevision, x.Binding.OrderHash
			out.Currency, out.PortionMinor = x.Portion.Currency().Code(), x.Portion.MinorString()
			out.LimitCurrency, out.LimitMinor, out.ExposureMinor, out.ExcessMinor = x.Limit.Currency().Code(), x.Limit.MinorString(),
				x.Exposure.MinorString(), x.Excess.MinorString()
			return nil
		}
		r, err := postgres.LoadReservation(ctx, exec, sc, res.ObjectID)
		if err != nil || r == nil {
			return err
		}
		out.PartyID, out.ReservationID, out.DecisionID, out.Basis, out.ExpiresAt = r.PartyID, r.ID, r.DecisionID, r.Basis, r.ExpiresAt
		out.OrderID, out.OrderRevision, out.OrderHash = r.Binding.OrderID, r.Binding.OrderRevision, r.Binding.OrderHash
		out.Currency, out.PortionMinor, out.SecuredMinor = r.Amount.Currency().Code(), r.Amount.MinorString(), r.Secured.MinorString()
		return nil
	})
	return out, err
}
