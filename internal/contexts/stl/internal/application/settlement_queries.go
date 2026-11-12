package application

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	fa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/application"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The reads of settlement statements (S5-04b): settlement_statement.get, receivable.get,
// payable.get and order_settlement.get, audited, with stl.read. A statement is owned by its
// confirmer (and team); outside the caller's data scope it is NOT_FOUND.

// overdueDays is how many whole days at is after due, -1 when it is not.
func overdueDays(due, at time.Time) int {
	if !due.Before(at) {
		return -1
	}
	return int(at.Sub(due) / (24 * time.Hour))
}

func zeroIn(c kernel.Currency) kernel.Money {
	z, _ := kernel.NewMoney(c, 0)
	return z
}

func installmentViews(st *settlement.Statement, applied map[int]kernel.Money, now time.Time) []api.InstallmentView {
	out := []api.InstallmentView{}
	for _, i := range st.Installments {
		a, ok := applied[i.Seq]
		if !ok {
			a = zeroIn(st.Currency())
		}
		open := settlement.OpenOf(i, a)
		v := api.InstallmentView{Seq: i.Seq, StageID: i.StageID, Trigger: i.Trigger, Days: i.Days, ReleaseControl: i.ReleaseControl,
			AmountMinor: i.Amount.MinorString(), AppliedMinor: a.MinorString(), OpenMinor: open.MinorString(), DueAt: i.DueAt, OverdueDays: -1}
		if open.Sign() > 0 {
			v.OverdueDays = overdueDays(i.DueAt, now)
		}
		out = append(out, v)
	}
	return out
}

// balance is statement st's receivable or payable now, with the applications d sees.
func balance(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, st *settlement.Statement, d authorization.Decision, now time.Time) (api.BalanceView, error) {
	applied, err := postgres.InstallmentApplied(ctx, exec, sc, st)
	if err != nil {
		return api.BalanceView{}, err
	}
	apps, err := postgres.StatementApplications(ctx, exec, sc, st.ID)
	if err != nil {
		return api.BalanceView{}, err
	}
	open := st.Open(applied)
	paid, _ := st.Amount.Sub(open)
	v := api.BalanceView{StatementID: st.ID, Kind: fa.TargetOf(st.Kind), Status: st.Status, CounterpartyID: st.CounterpartyID, OrderID: st.OrderID,
		OrderRevision: st.OrderRevision, Currency: st.Currency().Code(), AmountMinor: st.Amount.MinorString(), AppliedMinor: paid.MinorString(),
		OpenMinor: open.MinorString(), Installments: installmentViews(st, applied, now), Applications: []api.AllocationView{},
		ConfirmedAt: st.Confirmed.At, CheckedAt: now}
	for _, i := range v.Installments {
		if i.OpenMinor != "0" && (v.EarliestDueAt.IsZero() || i.DueAt.Before(v.EarliestDueAt)) {
			v.EarliestDueAt = i.DueAt
		}
	}
	for i := range apps {
		if x := &apps[i]; d.Sees(x.OwnerActor, x.OwnerTeam) {
			v.Applications = append(v.Applications, allocationView(x))
		}
	}
	return v, nil
}

// visibleStatement loads statement id for a read, NOT_FOUND when d does not see it.
func visibleStatement(ctx context.Context, exec bentopg.Executor, id string, d authorization.Decision) (*settlement.Statement, error) {
	st, err := postgres.LoadStatement(ctx, exec, scope(ctx), id, false)
	if err != nil {
		return nil, err
	}
	if st == nil || !d.Sees(st.OwnerActor, st.OwnerTeam) {
		return nil, kernel.Reject(kernel.CodeNotFound, "/id", "")
	}
	return st, nil
}

// GetStatement returns a settlement statement (settlement_statement.get).
func (s Service) GetStatement(ctx context.Context, id string) (api.StatementView, error) {
	var v api.StatementView
	err := s.read(ctx, api.CommandGetStatement, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc, now := scope(ctx), kernel.Instant(s.Clock.Now())
		st, err := visibleStatement(ctx, exec, id, d)
		if err != nil {
			return err
		}
		b, err := balance(ctx, exec, sc, st, d, now)
		if err != nil {
			return err
		}
		v = api.StatementView{StatementID: st.ID, Kind: st.Kind, Status: st.Status, Version: st.Version, CounterpartyID: st.CounterpartyID,
			OrderID: st.OrderID, OrderRevision: st.OrderRevision, OrderHash: st.OrderHash, ShippedAt: st.ShippedAt, Currency: b.Currency,
			NetMinor: st.Net.MinorString(), TaxMinor: st.Tax.MinorString(), AmountMinor: b.AmountMinor, AppliedMinor: b.AppliedMinor,
			OpenMinor: b.OpenMinor, EvidenceIDs: st.EvidenceIDs, Lines: []api.StatementLineView{}, Installments: b.Installments,
			Applications: b.Applications, ConfirmedBy: st.Confirmed.By, ConfirmedAt: st.Confirmed.At, CheckedAt: now}
		for _, l := range st.Lines {
			v.Lines = append(v.Lines, api.StatementLineView{Seq: l.Seq, SourceKind: l.Source.Kind, SourceID: l.Source.ID, SourceLineID: l.Source.LineID,
				SourceVersion: l.SourceVersion, OrderID: l.OrderID, OrderRevision: l.OrderRevision, OrderHash: l.OrderHash, OrderLineID: l.OrderLineID,
				CommitmentID: l.CommitmentID, SupplierID: l.SupplierID, SkuID: l.SkuID, SpecRevision: l.SpecRevision, UnitID: l.UnitID,
				UnitRevision: l.UnitRevision, Quantity: l.Quantity.String(), BasisQuantity: l.BasisQuantity.String(), UnitPrice: l.UnitPrice,
				BasisNetMinor: l.BasisNet.MinorString(), BasisTaxMinor: l.BasisTax.MinorString(), NetMinor: l.Net.MinorString(),
				TaxMinor: l.Tax.MinorString(), AmountMinor: l.Amount.MinorString(), OccurredAt: l.OccurredAt, Status: l.Status})
		}
		return nil
	})
	return v, err
}

func (s Service) getBalance(ctx context.Context, command, kind, id string) (api.BalanceView, error) {
	var v api.BalanceView
	err := s.read(ctx, command, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		st, err := visibleStatement(ctx, exec, id, d)
		if err != nil {
			return err
		}
		if st.Kind != kind {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v, err = balance(ctx, exec, scope(ctx), st, d, kernel.Instant(s.Clock.Now()))
		return err
	})
	return v, err
}

// GetReceivable returns the business receivable of a sales statement (receivable.get).
func (s Service) GetReceivable(ctx context.Context, id string) (api.BalanceView, error) {
	return s.getBalance(ctx, api.CommandGetReceivable, settlement.KindSales, id)
}

// GetPayable returns the business payable of a purchase statement (payable.get).
func (s Service) GetPayable(ctx context.Context, id string) (api.BalanceView, error) {
	return s.getBalance(ctx, api.CommandGetPayable, settlement.KindPurchase, id)
}

// GetOrderSettlement returns an order's settlement position (order_settlement.get): the account's
// stages with what stands prepaid, settled and paid on each; the order's receivables; the purchase
// statement lines of the order and their payables. NOT_FOUND when the caller sees neither the order
// account nor a statement of the order.
func (s Service) GetOrderSettlement(ctx context.Context, orderID string) (api.OrderSettlementView, error) {
	var v api.OrderSettlementView
	err := s.read(ctx, api.CommandGetOrderSettlement, orderID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc, now := scope(ctx), kernel.Instant(s.Clock.Now())
		v = api.OrderSettlementView{OrderID: orderID, Stages: []api.StagePositionView{}, Receivables: []api.BalanceView{},
			PurchaseLines: []api.PurchasePositionView{}, Payables: []api.BalanceView{}, CheckedAt: now}
		found := false
		a, err := postgres.LoadAccount(ctx, exec, sc, orderID, false)
		if err != nil {
			return err
		}
		sales, err := postgres.Statements(ctx, exec, sc, settlement.KindSales, orderID)
		if err != nil {
			return err
		}
		var prepaid, settledBy, paidBy map[string]kernel.Money
		if a != nil && d.Sees(a.OwnerActor, a.OwnerTeam) {
			found = true
			c := a.Currency()
			v.OrderRevision, v.Currency = a.OrderRevision, c.Code()
			if prepaid, err = postgres.StagePrepaid(ctx, exec, sc, orderID, a.OrderRevision, c); err != nil {
				return err
			}
			if settledBy, err = postgres.SettledByStage(ctx, exec, sc, orderID, c); err != nil {
				return err
			}
			if paidBy, err = postgres.StageApplied(ctx, exec, sc, a); err != nil {
				return err
			}
			total := zeroIn(c)
			for _, m := range prepaid {
				total, _ = total.Add(m)
			}
			v.PrepaidMinor = total.MinorString()
			for _, st := range a.Stages {
				get := func(m map[string]kernel.Money) kernel.Money {
					if x, ok := m[st.StageID]; ok {
						return x
					}
					return zeroIn(c)
				}
				unsettled, _ := st.Amount.Sub(get(settledBy))
				v.Stages = append(v.Stages, api.StagePositionView{StageID: st.StageID, Seq: st.Seq, Trigger: st.Trigger, ReleaseControl: st.ReleaseControl,
					AmountMinor: st.Amount.MinorString(), PrepaidMinor: get(prepaid).MinorString(), SettledMinor: get(settledBy).MinorString(),
					PaidMinor: get(paidBy).MinorString(), UnsettledMinor: unsettled.MinorString()})
			}
		}
		var settledSum, appliedSum, openSum, overdueSum kernel.Money
		for _, st := range sales {
			if !d.Sees(st.OwnerActor, st.OwnerTeam) {
				continue
			}
			found = true
			b, err := balance(ctx, exec, sc, st, d, now)
			if err != nil {
				return err
			}
			v.Receivables = append(v.Receivables, b)
			if v.Currency == "" {
				v.Currency = st.Currency().Code()
			}
			if settledSum.Currency().IsZero() {
				settledSum, appliedSum, openSum, overdueSum = zeroIn(st.Currency()), zeroIn(st.Currency()), zeroIn(st.Currency()), zeroIn(st.Currency())
			}
			applied, err := postgres.InstallmentApplied(ctx, exec, sc, st)
			if err != nil {
				return err
			}
			open := st.Open(applied)
			paid, _ := st.Amount.Sub(open)
			settledSum, _ = settledSum.Add(st.Amount)
			appliedSum, _ = appliedSum.Add(paid)
			openSum, _ = openSum.Add(open)
			for _, i := range st.Installments {
				if o := settlement.OpenOf(i, applied[i.Seq]); o.Sign() > 0 && i.DueAt.Before(now) {
					overdueSum, _ = overdueSum.Add(o)
				}
			}
		}
		if !settledSum.Currency().IsZero() {
			v.SettledMinor, v.AppliedMinor, v.OpenMinor, v.OverdueMinor = settledSum.MinorString(), appliedSum.MinorString(), openSum.MinorString(),
				overdueSum.MinorString()
		} else if v.Currency != "" {
			v.SettledMinor, v.AppliedMinor, v.OpenMinor, v.OverdueMinor = "0", "0", "0", "0"
		}
		purchases, err := postgres.Statements(ctx, exec, sc, settlement.KindPurchase, orderID)
		if err != nil {
			return err
		}
		for _, st := range purchases {
			if !d.Sees(st.OwnerActor, st.OwnerTeam) {
				continue
			}
			found = true
			for _, l := range st.Lines {
				if l.OrderID != orderID {
					continue
				}
				v.PurchaseLines = append(v.PurchaseLines, api.PurchasePositionView{StatementID: st.ID, Seq: l.Seq, SupplierID: st.CounterpartyID,
					Currency: st.Currency().Code(), CommitmentID: l.CommitmentID, SourceID: l.Source.ID, OrderLineID: l.OrderLineID,
					Quantity: l.Quantity.String(), AmountMinor: l.Amount.MinorString()})
			}
			b, err := balance(ctx, exec, sc, st, d, now)
			if err != nil {
				return err
			}
			v.Payables = append(v.Payables, b)
		}
		if !found {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		return nil
	})
	return v, err
}
