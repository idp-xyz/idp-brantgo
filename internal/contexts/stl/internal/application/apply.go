package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	fa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/application"
	fr "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	oa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderaccount"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// openAccount runs order_account.open (D-4): before the transaction it reads the order (TRD
// OrderCommercialSnapshot v1) and its currency (POL); inside it, it locks the order's account FOR
// UPDATE when it exists (an open account of the same revision answers as it is), re-checks the
// currency with ConfigGuard and writes the account with the revision's payment stages.
type openAccount struct {
	s        Service
	cmd      api.OpenOrderAccount
	order    *Order
	currency polapi.ResolvedConfig
	existing *oa.Account
	replay   bool
}

func (h *openAccount) Prepare(ctx context.Context) error {
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

func (h *openAccount) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.replay = false
	var err error
	h.existing, err = postgres.LoadAccount(ctx, exec, scope(ctx), h.cmd.OrderID, true)
	return err
}

func (h *openAccount) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	c, o, now := h.cmd, h.order, kernel.Instant(h.s.Clock.Now())
	if a := h.existing; a != nil {
		if !d.Sees(a.OwnerActor, a.OwnerTeam) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/order_id", "")
		}
		if a.OrderRevision != c.OrderRevision || a.OrderHash != c.OrderHash {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/order_revision",
				"The order account holds another revision: moving it is the payment plan review")
		}
		h.replay = true
		return action.Outcome{ObjectID: a.OrderID, RowVersion: a.Version, Status: a.Status}, nil
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
	}
	cur, err := currencyOf(h.currency, "/args/order_id")
	if err != nil {
		return action.Outcome{}, err
	}
	if err = guardConfig(ctx, exec, h.currency.Ref, h.currency.ContentHash, now, "/args/order_id"); err != nil {
		return action.Outcome{}, err
	}
	stages := make([]oa.Stage, 0, len(o.Stages))
	for _, s := range o.Stages {
		m, err := kernel.ParseMinor(cur, s.AmountMinor)
		if err != nil {
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "TRD's payment stage "+s.StageID+" has no amount")
		}
		stages = append(stages, oa.Stage{StageID: s.StageID, Trigger: s.Trigger, Days: s.Days, Percent: s.Percent, Amount: m,
			ReleaseControl: s.ReleaseControl})
	}
	a, err := oa.Open(c.OrderID, c.OrderRevision, c.OrderHash, o.CustomerID, o.Currency.Revision, stages, fundAct(d, now, ""), d.Team)
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertAccount(ctx, exec, scope(ctx), a); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: a.OrderID, RowVersion: a.Version, Status: a.Status}, nil
}

// OpenOrderAccount opens an active order's account at its current revision (order_account.open).
func (s Service) OpenOrderAccount(ctx context.Context, cmd api.OpenOrderAccount) (api.OrderAccountReceipt, error) {
	h := &openAccount{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandOpenOrderAccount, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OrderID}, cmd,
		cmd.Validate(), h)
	if err != nil {
		return api.OrderAccountReceipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	out := api.OrderAccountReceipt{Receipt: res}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		a, err := postgres.LoadAccount(ctx, exec, scope(ctx), res.ObjectID, false)
		if err != nil || a == nil {
			return err
		}
		out.OrderRevision, out.OrderHash, out.PartyID, out.Currency, out.Stages = a.OrderRevision, a.OrderHash, a.PartyID, a.Currency().Code(),
			len(a.Stages)
		return nil
	})
	return out, err
}

// lockRoots locks the fund record and the order account FOR UPDATE in ID order (tactical lockOrder:
// "STL 为资金记录根与订单账户…按 ID 排序"), so two commands on the same pair never wait on each other in
// opposite orders.
func lockRoots(ctx context.Context, exec bentopg.Executor, fundID, orderID string) (*fr.FundRecord, *oa.Account, error) {
	sc := scope(ctx)
	var f *fr.FundRecord
	var a *oa.Account
	var err error
	lockFund := func() error { f, err = postgres.LoadFund(ctx, exec, sc, fundID, true); return err }
	lockAccount := func() error { a, err = postgres.LoadAccount(ctx, exec, sc, orderID, true); return err }
	first, second := lockFund, lockAccount
	if orderID < fundID {
		first, second = lockAccount, lockFund
	}
	if err = first(); err != nil {
		return nil, nil, err
	}
	if err = second(); err != nil {
		return nil, nil, err
	}
	return f, a, nil
}

// appEvent is an Outbox event of an Application aggregate at its version.
func appEvent(typ string, x *fa.Application) action.Event {
	return action.Event{Type: typ, AggregateType: "Application", AggregateID: x.ID, AggregateVersion: x.Version, Fields: map[string]any{
		"application_id": x.ID, "application_version": x.Version, "fund_record_id": x.FundRecordID, "target_kind": x.TargetKind,
		"order_id": x.OrderID, "order_revision": x.OrderRevision, "stage_id": x.StageID}}
}

// applyFund runs application.apply (STL-INV-002, STL-INV-006; human only): before the transaction it
// reads the order (TRD); inside it, it locks the fund record and the order account FOR UPDATE in ID
// order and derives the fund's available balance and the stage's open amount from the immutable
// applications and draws under those locks (D-3), then writes the application.
type applyFund struct {
	s       Service
	cmd     api.Apply
	order   *Order
	fund    *fr.FundRecord
	account *oa.Account
	taken   bool
}

func (h *applyFund) Prepare(ctx context.Context) error {
	orders, err := h.s.Upstream.Orders.ResolveOrders(ctx, []string{h.cmd.Target.OrderID})
	if err != nil {
		return err
	}
	if o, ok := orders[h.cmd.Target.OrderID]; ok {
		h.order = &o
	}
	return nil
}

func (h *applyFund) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	if h.fund, h.account, err = lockRoots(ctx, exec, h.cmd.FundRecordID, h.cmd.Target.OrderID); err != nil {
		return err
	}
	x, err := postgres.LoadApplication(ctx, exec, scope(ctx), h.cmd.ApplicationID, false)
	h.taken = x != nil
	return err
}

func (h *applyFund) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, f, a, o, now := scope(ctx), h.cmd, h.fund, h.account, h.order, kernel.Instant(h.s.Clock.Now())
	switch {
	case h.taken:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/application_id", "The application ID is taken")
	case f == nil || !d.Sees(f.OwnerActor, f.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/fund_record_id", "")
	case o == nil:
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/target/order_id", "")
	case a == nil:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/target/order_id", "The order has no order account: open it first")
	case o.Status != OrderActive:
		return action.Outcome{}, api.Reject(api.CodeOrderNotActive, "/args/target/order_id", "The order is "+o.Status)
	case o.Binding.OrderRevision != a.OrderRevision:
		return action.Outcome{}, api.Reject(api.CodeReferenceStale, "/args/target/order_id",
			"TRD's order is at another revision than its account: the payment plan review is pending")
	}
	amount, err := amountIn(f.Currency(), c.Amount, "/args/amount")
	if err != nil {
		return action.Outcome{}, err
	}
	entries, err := postgres.FundEntries(ctx, exec, sc, f)
	if err != nil {
		return action.Outcome{}, err
	}
	applied, err := postgres.StageApplied(ctx, exec, sc, a)
	if err != nil {
		return action.Outcome{}, err
	}
	stageApplied, ok := applied[c.Target.StageID]
	if !ok {
		stageApplied, _ = kernel.NewMoney(a.Currency(), 0)
	}
	x, err := fa.Apply(fa.ApplyInput{ID: c.ApplicationID, Fund: f, FundEntries: entries, Account: a, StageID: c.Target.StageID,
		StageApplied: stageApplied, Amount: amount, By: fundAct(d, now, ""), Team: d.Team})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertApplication(ctx, exec, sc, x); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: x.ID, RowVersion: x.Version, Status: x.Status, Events: []action.Event{appEvent(api.EventApplied, x)}}, nil
}

// Apply allocates part of a receipt to an order stage (application.apply). Human only.
func (s Service) Apply(ctx context.Context, cmd api.Apply) (api.ApplicationReceipt, error) {
	h := &applyFund{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandApply, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ApplicationID}, cmd,
		cmd.Validate(), h)
	if err != nil {
		return api.ApplicationReceipt{}, err
	}
	return s.applicationReceipt(ctx, res)
}

// unapplyFund runs application.unapply (SM-STL-APPLICATION; human only): inside the transaction it
// locks the application's fund record and order account FOR UPDATE in ID order, then the
// application, checks its version and records the unapply. PaymentConditionGuard v1 holds the order
// account FOR SHARE: an unapply waits for a FUL transaction that judged by the application.
type unapplyFund struct {
	s           Service
	cmd         api.Unapply
	application *fa.Application
}

func (h *unapplyFund) Lock(ctx context.Context, exec bentopg.Executor) error {
	sc := scope(ctx)
	x, err := postgres.LoadApplication(ctx, exec, sc, h.cmd.ApplicationID, false)
	if err != nil || x == nil {
		h.application = nil
		return err
	}
	if _, _, err = lockRoots(ctx, exec, x.FundRecordID, x.OrderID); err != nil {
		return err
	}
	h.application, err = postgres.LoadApplication(ctx, exec, sc, h.cmd.ApplicationID, true)
	return err
}

func (h *unapplyFund) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	x, now := h.application, kernel.Instant(h.s.Clock.Now())
	if x == nil || !d.Sees(x.OwnerActor, x.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/application_id", "")
	}
	if x.Version != h.cmd.ExpectedVersion {
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if err := x.Unapply(fundAct(d, now, h.cmd.Reason)); err != nil {
		return action.Outcome{}, err
	}
	if err := postgres.SaveUnapply(ctx, exec, scope(ctx), x); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: x.ID, RowVersion: x.Version, Status: x.Status, Events: []action.Event{appEvent(api.EventUnapplied, x)}}, nil
}

// Unapply undoes an application (application.unapply). Human only.
func (s Service) Unapply(ctx context.Context, cmd api.Unapply) (api.ApplicationReceipt, error) {
	h := &unapplyFund{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandUnapply, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ApplicationID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.ApplicationReceipt{}, err
	}
	return s.applicationReceipt(ctx, res)
}

// applicationReceipt completes res with the stored application, the fund's available balance and
// the stage's open amount now.
func (s Service) applicationReceipt(ctx context.Context, res api.Receipt) (api.ApplicationReceipt, error) {
	out := api.ApplicationReceipt{Receipt: res}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		x, err := postgres.LoadApplication(ctx, exec, sc, res.ObjectID, false)
		if err != nil || x == nil {
			return err
		}
		out.FundRecordID, out.OrderID, out.OrderRevision, out.StageID = x.FundRecordID, x.OrderID, x.OrderRevision, x.StageID
		out.Currency, out.AmountMinor = x.Amount.Currency().Code(), x.Amount.MinorString()
		f, err := postgres.LoadFund(ctx, exec, sc, x.FundRecordID, false)
		if err != nil || f == nil {
			return err
		}
		e, err := postgres.FundEntries(ctx, exec, sc, f)
		if err != nil {
			return err
		}
		out.FundAvailableMinor = f.Available(e).MinorString()
		a, err := postgres.LoadAccount(ctx, exec, sc, x.OrderID, false)
		if err != nil || a == nil {
			return err
		}
		applied, err := postgres.StageApplied(ctx, exec, sc, a)
		if err != nil {
			return err
		}
		if st := a.Stage(x.StageID); st != nil && a.OrderRevision == x.OrderRevision {
			out.StageOpenMinor = oa.OpenOf(*st, applied[x.StageID]).MinorString()
		}
		return nil
	})
	return out, err
}
