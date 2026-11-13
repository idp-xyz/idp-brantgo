package application

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// read runs an audited read of id; a malformed ID is SCHEMA_INVALID, audited as the read.
func (s Service) read(ctx context.Context, command, id string, fn func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error) error {
	if !kernel.ValidIdentifier(id) {
		err := kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
		if auditErr := s.Ledger.AuditRejected(ctx, command, "", kernel.CodeSchemaInvalid); auditErr != nil {
			return auditErr
		}
		return err
	}
	return s.Ledger.Read(ctx, command, id, fn)
}

func actorOf(a *cc.Act) (kernel.ActorRef, time.Time, string) {
	if a == nil {
		return kernel.ActorRef{}, time.Time{}, ""
	}
	return a.By, a.At, a.Reason
}

func approvalsView(as []cc.Approval) []api.ApprovalView {
	out := []api.ApprovalView{}
	for _, a := range as {
		out = append(out, api.ApprovalView{Approver: a.Approver, Level: a.Level, MaxMinor: a.Max.MinorString(), Epoch: a.Epoch, ApprovedAt: a.At})
	}
	return out
}

func requestView(r *cc.LimitRequest) api.LimitRequestView {
	v := api.LimitRequestView{RequestID: r.ID, Status: r.Status, Version: r.Version, Currency: r.Currency.Code(), AmountMinor: r.Amount.MinorString(),
		ValidFrom: r.ValidFrom, ValidUntil: r.ValidUntil, CreditPolicy: configOf(r.Policy, api.KindCreditPolicy), PolicyHash: r.Policy.Hash,
		Reason: r.Reason, RequestedBy: r.Requested.By, RequestedAt: r.Requested.At, Approvals: approvalsView(r.Approvals),
		LimitVersion: r.LimitVersion}
	for _, s := range []struct {
		name string
		src  *cc.SourceAmount
	}{{cc.SourceInternalAssessment, r.Internal}, {cc.SourceSinosureBuyerLimit, r.Sinosure}} {
		if s.src != nil {
			v.Sources = append(v.Sources, api.LimitSourceView{Source: s.name, AmountMinor: s.src.Amount.MinorString(), EvidenceIDs: s.src.EvidenceIDs})
		}
	}
	v.DecidedBy, v.DecidedAt, v.DecisionReason = actorOf(r.Decided)
	return v
}

func versionView(x *cc.LimitVersion, now time.Time) api.LimitVersionView {
	return api.LimitVersionView{Version: x.Version, RequestID: x.RequestID, Currency: x.Currency.Code(), AmountMinor: x.Amount.MinorString(),
		Source: x.Source, ValidFrom: x.ValidFrom, ValidUntil: x.ValidUntil, InForce: x.InForce(now), PublishedBy: x.Published.By,
		PublishedAt: x.Published.At}
}

func reservationView(r *cc.Reservation, now time.Time) api.ReservationView {
	v := api.ReservationView{ReservationID: r.ID, PartyID: r.PartyID, OrderID: r.Binding.OrderID, OrderRevision: r.Binding.OrderRevision,
		OrderHash: r.Binding.OrderHash, Status: r.Status, State: r.State(now), Version: r.Version, Currency: r.Amount.Currency().Code(),
		PortionMinor: r.Amount.MinorString(), SecuredMinor: r.Secured.MinorString(), LimitCurrency: r.Exposure.Currency().Code(),
		ExposureMinor: r.Exposure.MinorString(), CreditPolicy: configOf(r.Policy, api.KindCreditPolicy), Basis: r.Basis, DecisionID: r.DecisionID,
		ExpiresAt: r.ExpiresAt, ReservedBy: r.Reserved.By, ReservedAt: r.Reserved.At, ActivatedAt: r.ActivatedAt, CheckedAt: now}
	if r.FX != nil {
		fx := configOf(*r.FX, api.KindFXRate)
		v.FXRate = &fx
	}
	v.ConsumedBy, v.ConsumedAt, _ = actorOf(r.Consumed)
	v.ReleasedBy, v.ReleasedAt, v.ReleaseReason = actorOf(r.Released)
	v.ReleaseCause, v.CarriedFrom = r.ReleaseCause, r.CarriedFrom
	return v
}

func decisionView(x *cc.Decision, now time.Time) api.DecisionView {
	v := api.DecisionView{DecisionID: x.ID, PartyID: x.PartyID, OrderID: x.Binding.OrderID, OrderRevision: x.Binding.OrderRevision,
		OrderHash: x.Binding.OrderHash, Status: x.Status, State: x.State(now), Version: x.Version, Reasons: x.Reasons,
		Currency: x.Portion.Currency().Code(), PortionMinor: x.Portion.MinorString(), LimitCurrency: x.Limit.Currency().Code(),
		ConvertedMinor: x.Converted.MinorString(), LimitMinor: x.Limit.MinorString(), LimitVersion: x.LimitVersion,
		ExposureMinor: x.Exposure.MinorString(), ExcessMinor: x.Excess.MinorString(), CoverMinor: x.Cover.MinorString(), OverdueDays: x.OverdueDays,
		CreditPolicy: configOf(x.Policy, api.KindCreditPolicy), ExpiresAt: x.ExpiresAt, OpenedBy: x.Opened.By, OpenedAt: x.Opened.At,
		Approvals: approvalsView(x.Approvals), ReservationID: x.ReservationID, CheckedAt: now}
	v.DecidedBy, v.DecidedAt, v.DecisionReason = actorOf(x.Decided)
	return v
}

// GetCustomerCredit returns a customer's credit (customer_credit.get).
func (s Service) GetCustomerCredit(ctx context.Context, partyID string) (api.CustomerCreditView, error) {
	var v api.CustomerCreditView
	err := s.read(ctx, api.CommandGetCredit, partyID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc, now := scope(ctx), kernel.Instant(s.Clock.Now())
		root, err := postgres.LoadRoot(ctx, exec, sc, partyID, false)
		if err != nil {
			return err
		}
		if root == nil || root.RowVersion == 0 {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.CustomerCreditView{PartyID: root.PartyID, RowVersion: root.RowVersion, Versions: []api.LimitVersionView{},
			Requests: []api.LimitRequestView{}, Reservations: []api.ReservationView{}, Decisions: []api.DecisionView{}, CheckedAt: now}
		versions, err := postgres.LoadVersions(ctx, exec, sc, partyID)
		if err != nil {
			return err
		}
		for i := range versions {
			v.Versions = append(v.Versions, versionView(&versions[i], now))
		}
		if in := cc.LimitInForce(versions, now); in != nil {
			x := versionView(in, now)
			v.LimitInForce = &x
		}
		requests, err := postgres.LoadRequests(ctx, exec, sc, partyID)
		if err != nil {
			return err
		}
		for i := range requests {
			if r := &requests[i]; d.Sees(r.OwnerActor, r.OwnerTeam) {
				v.Requests = append(v.Requests, requestView(r))
			}
		}
		reservations, err := postgres.LoadReservations(ctx, exec, sc, partyID)
		if err != nil {
			return err
		}
		var held []cc.Held
		for i := range reservations {
			r := &reservations[i]
			if r.Live(now) && r.Basis != cc.BasisDocumentsSecured {
				held = append(held, cc.HeldOf(r))
			}
			if d.Sees(r.OwnerActor, r.OwnerTeam) {
				v.Reservations = append(v.Reservations, reservationView(r, now))
			}
		}
		// The exposure is in the live reservations' limit currency, else the limit in force's, else that
		// of the receivables (S5-04b: open receivables count, D-4).
		var c kernel.Currency
		if len(held) > 0 {
			c = held[0].Exposure.Currency()
		} else if in := cc.LimitInForce(versions, now); in != nil {
			c = in.Currency
		}
		owed, err := postgres.OwedInstallments(ctx, exec, sc, partyID)
		if err != nil {
			return err
		}
		if c.IsZero() && len(owed) > 0 {
			c = owed[0].Amount.Currency()
		}
		if !c.IsZero() && (len(held) > 0 || len(owed) > 0) {
			// Receivables in another currency convert at a credit rate only (S5R-D22), which a read
			// names none of: the exposure is left out and the currencies are named.
			if missing := cc.Unconverted(c, owed, nil); len(missing) > 0 {
				v.ExposureCurrency, v.UnconvertedCurrencies = c.Code(), missing
				return s.listDecisions(ctx, exec, d, partyID, now, &v)
			}
			receivables, err := cc.ReceivablesOf(c, owed, nil)
			if err != nil {
				return err
			}
			limit, _ := kernel.NewMoney(c, 0)
			exposure, err := cc.Assess(cc.Assessment{Limit: limit, Held: held, Receivables: receivables, Portion: limit, Now: now})
			if err != nil {
				return err
			}
			v.ExposureCurrency, v.ExposureMinor = c.Code(), exposure.Exposure.MinorString()
		}
		return s.listDecisions(ctx, exec, d, partyID, now, &v)
	})
	return v, err
}

// listDecisions adds the customer's decisions the caller sees to v.
func (s Service) listDecisions(ctx context.Context, exec bentopg.Executor, d authorization.Decision, partyID string, now time.Time,
	v *api.CustomerCreditView) error {
	decisions, err := postgres.LoadDecisions(ctx, exec, scope(ctx), partyID)
	if err != nil {
		return err
	}
	for i := range decisions {
		if x := &decisions[i]; d.Sees(x.OwnerActor, x.OwnerTeam) {
			v.Decisions = append(v.Decisions, decisionView(x, now))
		}
	}
	return nil
}

// GetReservation returns a reservation (credit_reservation.get).
func (s Service) GetReservation(ctx context.Context, id string) (api.ReservationView, error) {
	var v api.ReservationView
	err := s.read(ctx, api.CommandGetReservation, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		r, err := postgres.LoadReservation(ctx, exec, scope(ctx), id)
		if err != nil {
			return err
		}
		if r == nil || !d.Sees(r.OwnerActor, r.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = reservationView(r, kernel.Instant(s.Clock.Now()))
		return nil
	})
	return v, err
}

// GetDecision returns a credit decision (credit_decision.get).
func (s Service) GetDecision(ctx context.Context, id string) (api.DecisionView, error) {
	var v api.DecisionView
	err := s.read(ctx, api.CommandGetDecision, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		x, err := postgres.LoadDecision(ctx, exec, scope(ctx), id)
		if err != nil {
			return err
		}
		if x == nil || !d.Sees(x.OwnerActor, x.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = decisionView(x, kernel.Instant(s.Clock.Now()))
		return nil
	})
	return v, err
}
