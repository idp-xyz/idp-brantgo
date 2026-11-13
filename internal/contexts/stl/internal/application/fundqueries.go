package application

import (
	"context"
	"strconv"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	fa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/application"
	fr "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	oa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderaccount"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// visibleFund reports, for the commands of funds, applications and order accounts, whether d's
// actor sees the object a stored result names; handled is false for other commands.
func visibleFund(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, id string) (visible, handled bool, err error) {
	sc := scope(ctx)
	switch command {
	case api.CommandRecordFund, api.CommandResolveDifference, api.CommandVerifyPayer:
		f, err := postgres.LoadFund(ctx, exec, sc, id, false)
		return err == nil && f != nil && d.Sees(f.OwnerActor, f.OwnerTeam), true, err
	case api.CommandOpenOrderAccount, api.CommandOpenReview, api.CommandSettleReview:
		a, err := postgres.LoadAccount(ctx, exec, sc, id, false)
		return err == nil && a != nil && d.Sees(a.OwnerActor, a.OwnerTeam), true, err
	case api.CommandApply, api.CommandUnapply:
		x, err := postgres.LoadApplication(ctx, exec, sc, id, false)
		return err == nil && x != nil && d.Sees(x.OwnerActor, x.OwnerTeam), true, err
	}
	return false, false, nil
}

func allocationView(x *fa.Application) api.AllocationView {
	return api.AllocationView{ApplicationID: x.ID, FundRecordID: x.FundRecordID, CreditNoteID: x.CreditNoteID, TargetKind: x.TargetKind,
		OrderID: x.OrderID, OrderRevision: x.OrderRevision, StageID: x.StageID, StatementID: x.StatementID, InstallmentSeq: x.InstallmentSeq,
		MovedFrom: x.MovedFrom, AmountMinor: x.Amount.MinorString(), Status: x.Status, Conversion: conversionView(x.Conversion),
		PayerBasis: payerBasis(x), PayerProfileRevision: x.PayerProfileRevision}
}

// payerBasis is x's payer basis as the reads show it (S5R-D21): customer when none is recorded.
func payerBasis(x *fa.Application) string {
	if x.PayerBasis == "" {
		return api.PayerCustomer
	}
	return x.PayerBasis
}

// fundRef is fund record f as FundRecordRef v1 states it, with the allocations d sees.
func fundRef(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, f *fr.FundRecord, d authorization.Decision) (api.FundRecordRef, error) {
	draws, err := postgres.Draws(ctx, exec, sc, f)
	if err != nil {
		return api.FundRecordRef{}, err
	}
	e, err := postgres.FundEntries(ctx, exec, sc, f)
	if err != nil {
		return api.FundRecordRef{}, err
	}
	apps, err := postgres.Applications(ctx, exec, sc, f.ID, "")
	if err != nil {
		return api.FundRecordRef{}, err
	}
	verified, err := postgres.PayerVerifications(ctx, exec, sc, f.ID)
	if err != nil {
		return api.FundRecordRef{}, err
	}
	r := api.FundRecordRef{FundRecordID: f.ID, Kind: f.Kind, Direction: f.Direction, Status: f.Status, Version: f.Version,
		CounterpartyID: f.CounterpartyID, Currency: f.Currency().Code(), CurrencyRevision: f.CurrencyRevision,
		BankAccount:   api.ConfigRef{Kind: api.KindBankAccount, ConfigID: f.BankAccountID, Revision: f.BankAccountRevision},
		BankReference: f.BankReference, ValueDate: f.ValueDate, EvidenceIDs: f.EvidenceIDs, ReceivedMinor: f.Received.MinorString(),
		RemittedMinor: f.Remitted.MinorString(), BankChargesMinor: f.BankCharges.MinorString(), DifferenceMinor: f.Difference().MinorString(),
		ResolvedMinor: f.Resolved().MinorString(), UnexplainedMinor: f.Unexplained().MinorString(), AppliedMinor: e.Applied.MinorString(),
		DrawnMinor: e.Drawn.MinorString(), SoldMinor: e.Sold.MinorString(), AvailableMinor: f.Available(e).MinorString(),
		CashMinor: f.Cash(e).MinorString(), SourceFundRecordID: f.SourceID, Resolutions: []api.ResolutionView{}, Draws: []api.DrawView{},
		Allocations: []api.AllocationView{}, RecordedBy: f.Recorded.By, RecordedAt: f.Recorded.At, PayerVerifications: []api.PayerVerificationView{}}
	for _, v := range verified {
		r.PayerVerifications = append(r.PayerVerifications, api.PayerVerificationView{Seq: v.Seq, PayerID: v.PayerID, CustomerID: v.CustomerID,
			EvidenceIDs: v.EvidenceIDs, Reason: v.By.Reason, VerifiedBy: v.By.By, VerifiedAt: v.By.At})
	}
	if x := f.FX; x != nil {
		r.FX = &api.FXView{SoldCurrency: x.Sold.Currency().Code(), SoldMinor: x.Sold.MinorString(), BankRate: x.Rate.String(),
			FeeMinor: x.Fee.MinorString()}
	}
	for _, s := range f.Resolutions {
		r.Resolutions = append(r.Resolutions, api.ResolutionView{Seq: s.Seq, Resolution: s.Resolution, AmountMinor: s.Amount.MinorString(),
			EvidenceIDs: s.EvidenceIDs, Reason: s.By.Reason, ResolvedBy: s.By.By, ResolvedAt: s.By.At})
	}
	for _, w := range draws {
		r.Draws = append(r.Draws, api.DrawView{FundRecordID: w.FundRecordID, Kind: w.Kind, AmountMinor: w.Amount.MinorString()})
	}
	for i := range apps {
		if x := &apps[i]; d.Sees(x.OwnerActor, x.OwnerTeam) {
			r.Allocations = append(r.Allocations, allocationView(x))
		}
	}
	return r, nil
}

// GetFundRecord returns a fund record (fund_record.get).
func (s Service) GetFundRecord(ctx context.Context, id string) (api.FundRecordView, error) {
	var v api.FundRecordView
	err := s.read(ctx, api.CommandGetFundRecord, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		f, err := postgres.LoadFund(ctx, exec, sc, id, false)
		if err != nil {
			return err
		}
		if f == nil || !d.Sees(f.OwnerActor, f.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		r, err := fundRef(ctx, exec, sc, f, d)
		v = api.FundRecordView{FundRecordRef: r, CheckedAt: kernel.Instant(s.Clock.Now())}
		return err
	})
	return v, err
}

// ResolveFundRecords publishes FundRecordRef v1 (R38) for CNX.
func (s Service) ResolveFundRecords(ctx context.Context, ids []string) (map[string]api.FundRecordRef, error) {
	out := map[string]api.FundRecordRef{}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) {
			return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/fund_record_ids/"+strconv.Itoa(i), "")
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		for _, id := range ids {
			f, err := postgres.LoadFund(ctx, exec, sc, id, false)
			if err != nil {
				return err
			}
			if f == nil || !d.Sees(f.OwnerActor, f.OwnerTeam) {
				continue
			}
			if out[id], err = fundRef(ctx, exec, sc, f, d); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func conditionView(v oa.Verdict) api.ConditionView {
	c := api.ConditionView{Scope: v.Scope, Status: v.Status, Reason: v.Reason}
	if !v.Required.Currency().IsZero() {
		c.RequiredMinor, c.AppliedMinor = v.Required.MinorString(), v.Applied.MinorString()
	}
	if v.Stage != nil {
		c.StageID, c.ShortfallMinor = v.Stage.StageID, v.Shortfall.MinorString()
	}
	return c
}

// GetOrderAccount returns an order account with its payment condition per scope now
// (order_account.get).
func (s Service) GetOrderAccount(ctx context.Context, orderID string) (api.OrderAccountView, error) {
	var v api.OrderAccountView
	err := s.read(ctx, api.CommandGetOrderAccount, orderID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc, now := scope(ctx), kernel.Instant(s.Clock.Now())
		a, err := postgres.LoadAccount(ctx, exec, sc, orderID, false)
		if err != nil {
			return err
		}
		if a == nil || !d.Sees(a.OwnerActor, a.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		applied, err := postgres.StageApplied(ctx, exec, sc, a)
		if err != nil {
			return err
		}
		apps, err := postgres.Applications(ctx, exec, sc, "", orderID)
		if err != nil {
			return err
		}
		v = api.OrderAccountView{OrderID: a.OrderID, OrderRevision: a.OrderRevision, OrderHash: a.OrderHash, PartyID: a.PartyID,
			Currency: a.Currency().Code(), CurrencyRevision: a.CurrencyRevision, Status: a.Status, Version: a.Version, Stages: []api.StageView{},
			Applications: []api.AllocationView{}, OpenedBy: a.Opened.By, OpenedAt: a.Opened.At, CheckedAt: now}
		for _, st := range a.Stages {
			v.Stages = append(v.Stages, api.StageView{StageID: st.StageID, Seq: st.Seq, Trigger: st.Trigger, Days: st.Days, Percent: st.Percent,
				AmountMinor: st.Amount.MinorString(), ReleaseControl: st.ReleaseControl, AppliedMinor: applied[st.StageID].MinorString(),
				OpenMinor: oa.OpenOf(st, applied[st.StageID]).MinorString()})
		}
		for i := range apps {
			if x := &apps[i]; d.Sees(x.OwnerActor, x.OwnerTeam) {
				v.Applications = append(v.Applications, allocationView(x))
			}
		}
		for _, release := range []string{oa.ScopeShipment, oa.ScopeDocuments} {
			v.Conditions = append(v.Conditions, conditionView(oa.Condition(a, a.OrderRevision, release, applied, now)))
		}
		return nil
	})
	return v, err
}

func actOf(a *fr.Act) (kernel.ActorRef, time.Time, string) {
	if a == nil {
		return kernel.ActorRef{}, time.Time{}, ""
	}
	return a.By, a.At, a.Reason
}

// GetApplication returns an application (application.get).
func (s Service) GetApplication(ctx context.Context, id string) (api.ApplicationView, error) {
	var v api.ApplicationView
	err := s.read(ctx, api.CommandGetApplication, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		x, err := postgres.LoadApplication(ctx, exec, scope(ctx), id, false)
		if err != nil {
			return err
		}
		if x == nil || !d.Sees(x.OwnerActor, x.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.ApplicationView{ApplicationID: x.ID, FundRecordID: x.FundRecordID, CreditNoteID: x.CreditNoteID, TargetKind: x.TargetKind,
			OrderID: x.OrderID, OrderRevision: x.OrderRevision, StageID: x.StageID, StatementID: x.StatementID, InstallmentSeq: x.InstallmentSeq,
			MovedFrom: x.MovedFrom, Currency: x.Amount.Currency().Code(), AmountMinor: x.Amount.MinorString(), Conversion: conversionView(x.Conversion),
			Status: x.Status, Version: x.Version, AppliedBy: x.Applied.By, AppliedAt: x.Applied.At, PayerBasis: payerBasis(x),
			PayerProfileRevision: x.PayerProfileRevision, CheckedAt: kernel.Instant(s.Clock.Now())}
		v.UnappliedBy, v.UnappliedAt, v.UnapplyReason = actOf(x.Unapplied)
		return nil
	})
	return v, err
}
