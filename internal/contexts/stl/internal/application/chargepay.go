package application

import (
	"context"
	"sort"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	fa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/application"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/charge"
	fr "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Charge payments and the settlement rate of a receipt in the order's currency (S5R unit R8; simulated sign-off E0 of
// 2026-11-13: S5R-D19; STL-INV-002, STL-INV-010, STL-INV-011).
//
// application.apply to a charge {kind: "charge", charge_id} locks the fund record and the charge FOR UPDATE in ID
// order (tactical lockOrder: STL's roots by ID), then the order results of the charge's orders that exist FOR SHARE in
// ID order — order_result.close holds its result FOR UPDATE, so a payment and a close of one order commit one after the
// other and the snapshot's net cash is what stood —, then the ledger locks the authority guard, then POL's ConfigGuard
// v1 a settlement rate it names (R33). application.unapply of a charge payment takes the same locks.

// settlementLookup reads, before the transaction, the settlement rate ref an application names into order o's analysis
// currency (S5R-D19): POL's answer, or the refusal Apply returns at /args/settlement_fx_rate — FX_SCOPE_MISMATCH when
// the order is analysed in its own currency.
func (s Service) settlementLookup(ctx context.Context, ref *api.ConfigRef, o *Order) (settlementRate, error) {
	if ref == nil || o == nil {
		return settlementRate{}, nil
	}
	path := "/args/settlement_fx_rate"
	to := analysisCode(o)
	if to == "" {
		return settlementRate{refusal: api.Reject(api.CodeFXScopeMismatch, path, "Order "+o.Binding.OrderID+" is analysed in its own currency: "+
			"no settlement rate applies"), path: path}, nil
	}
	return s.lookupRateAt(ctx, ref, orderCurrencyOf(*o), to, path)
}

// lockChargeRoots locks fund record fundID and charge chargeID FOR UPDATE in ID order, then the order results of the
// charge's orders that exist FOR SHARE in ID order.
func lockChargeRoots(ctx context.Context, exec bentopg.Executor, fundID, chargeID string) (*fr.FundRecord, *charge.Charge, error) {
	sc := scope(ctx)
	var f *fr.FundRecord
	var c *charge.Charge
	var err error
	lockFund := func() error { f, err = postgres.LoadFund(ctx, exec, sc, fundID, true); return err }
	lockCharge := func() error { c, err = postgres.LoadCharge(ctx, exec, sc, chargeID, true); return err }
	first, second := lockFund, lockCharge
	if chargeID < fundID {
		first, second = lockCharge, lockFund
	}
	if err = first(); err != nil {
		return nil, nil, err
	}
	if err = second(); err != nil {
		return nil, nil, err
	}
	if c == nil {
		return f, nil, nil
	}
	orders := c.Orders()
	sort.Strings(orders)
	for _, id := range orders {
		if _, err = postgres.LoadResult(ctx, exec, sc, id, postgres.ForShare); err != nil {
			return nil, nil, err
		}
	}
	return f, c, nil
}

// chargePaid is what stands applied to charge c (in its currency).
func chargePaid(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, c *charge.Charge) (kernel.Money, error) {
	paid, _ := kernel.NewMoney(c.Currency(), 0)
	apps, err := postgres.ChargeApplications(ctx, exec, sc, []string{c.ID})
	if err != nil {
		return paid, err
	}
	for _, x := range apps[c.ID] {
		if x.Stands() {
			if paid, err = paid.Add(x.Amount); err != nil {
				return paid, err
			}
		}
	}
	return paid, nil
}

// applyCharge runs application.apply to a charge (S5R-D19; human only, treasury): before the transaction it reads a
// settlement rate it names (POL); inside it, it locks the fund record and the charge (lockChargeRoots), derives what
// the fund has available and what is open on the charge from the immutable applications and draws, and writes the
// application.
type applyCharge struct {
	s      Service
	cmd    api.Apply
	rate   settlementRate
	fund   *fr.FundRecord
	charge *charge.Charge
	taken  bool
}

func (h *applyCharge) Prepare(ctx context.Context) error {
	if h.cmd.FXRate == nil {
		return nil
	}
	var from, to string
	err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		f, err := postgres.LoadFund(ctx, exec, sc, h.cmd.FundRecordID, false)
		if err != nil || f == nil {
			return err
		}
		c, err := postgres.LoadCharge(ctx, exec, sc, h.cmd.Target.ChargeID, false)
		if err != nil || c == nil {
			return err
		}
		from, to = f.Currency().Code(), c.Currency().Code()
		return nil
	})
	if err != nil {
		return err
	}
	h.rate, err = h.s.lookupRate(ctx, h.cmd.FXRate, from, to)
	return err
}

func (h *applyCharge) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.fund, h.charge, h.taken = nil, nil, false
	var err error
	if h.fund, h.charge, err = lockChargeRoots(ctx, exec, h.cmd.FundRecordID, h.cmd.Target.ChargeID); err != nil {
		return err
	}
	x, err := postgres.LoadApplication(ctx, exec, scope(ctx), h.cmd.ApplicationID, false)
	h.taken = x != nil
	return err
}

func (h *applyCharge) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, f, ch, now := scope(ctx), h.cmd, h.fund, h.charge, kernel.Instant(h.s.Clock.Now())
	switch {
	case h.taken:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/application_id", "The application ID is taken")
	case f == nil || !d.Sees(f.OwnerActor, f.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/fund_record_id", "")
	case ch == nil || !d.Sees(ch.OwnerActor, ch.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/target/charge_id", "")
	}
	amount, err := amountIn(f.Currency(), c.Amount, "/args/amount")
	if err != nil {
		return action.Outcome{}, err
	}
	entries, err := postgres.FundEntries(ctx, exec, sc, f)
	if err != nil {
		return action.Outcome{}, err
	}
	paid, err := chargePaid(ctx, exec, sc, ch)
	if err != nil {
		return action.Outcome{}, err
	}
	fx, err := h.rate.input(ctx, exec, now, nil)
	if err != nil {
		return action.Outcome{}, err
	}
	x, err := fa.ApplyToCharge(fa.ChargeInput{ID: c.ApplicationID, Fund: f, FundEntries: entries, Charge: ch, Applied: paid, Amount: amount, FX: fx,
		By: fundAct(d, now, ""), Team: d.Team})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertApplication(ctx, exec, sc, x); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: x.ID, RowVersion: x.Version, Status: x.Status, Events: []action.Event{appEvent(api.EventApplied, x)}}, nil
}
