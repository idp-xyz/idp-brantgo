package application

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	tcc "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/tradecompliancecheck"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// GetCheck returns a check with its evaluations, decisions and reopenings, and its state now as
// TradeComplianceGuard v1 decides it for the check's binding (trade_compliance.get, cnx.read).
func (s Service) GetCheck(ctx context.Context, checkID string) (api.CheckView, error) {
	var v api.CheckView
	if !kernel.ValidIdentifier(checkID) {
		return v, kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	}
	err := s.Ledger.Read(ctx, api.CommandGetCheck, checkID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		c, err := postgres.LoadCheck(ctx, exec, sc, checkID, postgres.NoLock)
		if err != nil {
			return err
		}
		if c == nil || !d.Sees(c.OwnerActor, c.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		now := kernel.Instant(s.Clock.Now())
		state, err := postgres.StateOf(ctx, exec, c.Binding, now)
		if err != nil {
			return err
		}
		v = api.CheckView{CheckID: c.ID, OrderID: c.Binding.OrderID, OrderRevision: c.Binding.OrderRevision, OrderHash: c.Binding.OrderHash,
			Status: string(c.Status), State: state.Status, Reason: state.Reason, RowVersion: c.RowVersion, CurrentEvaluationID: c.Evaluation,
			CurrentDecisionID: c.Decision, PreviousCheckID: c.Previous, OpenedBy: c.OpenedBy, OpenedAt: c.OpenedAt,
			Evaluations: []api.EvaluationView{}, Decisions: []api.DecisionView{}, Reopenings: []api.ReopeningView{}, CheckedAt: now}
		evs, err := postgres.Evaluations(ctx, exec, sc, c.ID, "")
		if err != nil {
			return err
		}
		for _, ev := range evs {
			v.Evaluations = append(v.Evaluations, evaluationView(ev))
		}
		ds, err := postgres.Decisions(ctx, exec, sc, c.ID, "")
		if err != nil {
			return err
		}
		for _, dec := range ds {
			v.Decisions = append(v.Decisions, decisionView(dec, evs))
		}
		rs, err := postgres.Reopenings(ctx, exec, sc, c.ID)
		for _, r := range rs {
			rv := api.ReopeningView{ReopeningID: r.ID, Cause: r.Cause, DecisionID: r.DecisionID, UpdatedLists: r.Updated, Reason: r.Reason,
				ReopenedBy: r.By, ReopenedAt: r.At, AuthorityEpoch: r.Epoch}
			if r.Regulation != nil {
				rv.RegulationID, rv.RegulationRevision = r.Regulation.ID, r.Regulation.Revision
			}
			v.Reopenings = append(v.Reopenings, rv)
		}
		return err
	})
	return v, err
}

// OrderChecks returns the checks of an order's revisions the caller sees, newest first, with
// their state now (trade_compliance.order_checks, cnx.read).
func (s Service) OrderChecks(ctx context.Context, orderID string) ([]api.CheckSummary, error) {
	if !kernel.ValidIdentifier(orderID) {
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	}
	out := []api.CheckSummary{}
	err := s.Ledger.Read(ctx, api.CommandOrderChecks, orderID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		cs, err := postgres.ChecksOfOrder(ctx, exec, scope(ctx), orderID)
		if err != nil {
			return err
		}
		now := kernel.Instant(s.Clock.Now())
		for _, c := range cs {
			if !d.Sees(c.OwnerActor, c.OwnerTeam) {
				continue
			}
			state, err := postgres.StateOf(ctx, exec, c.Binding, now)
			if err != nil {
				return err
			}
			out = append(out, api.CheckSummary{CheckID: c.ID, OrderRevision: c.Binding.OrderRevision, OrderHash: c.Binding.OrderHash,
				Status: string(c.Status), State: state.Status, Reason: state.Reason, RowVersion: c.RowVersion, DecisionID: c.Decision,
				LicenseRequired: state.LicenseRequired, ValidUntil: state.ValidUntil, CheckedAt: now})
		}
		return nil
	})
	return out, err
}

func evidenceViews(es []tcc.Evidence) []api.EvidenceView {
	out := []api.EvidenceView{}
	for _, e := range es {
		out = append(out, api.EvidenceView{SourceID: e.SourceID, ContentSHA256: e.ContentSHA256})
	}
	return out
}

// timestampOf reads a document's timestamp; "" (open-ended) is the zero instant.
func timestampOf(s string) time.Time {
	t, err := kernel.ParseTimestamp(s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func evaluationView(ev *tcc.Evaluation) api.EvaluationView {
	d := ev.Document
	v := api.EvaluationView{EvaluationID: d.EvaluationID, Seq: d.Seq, OrderRevision: d.Order.OrderRevision, OrderHash: d.Order.OrderHash,
		Regulation: api.RegulationView{ID: d.Regulation.ID, Revision: d.Regulation.Revision, ContentHash: d.Regulation.ContentHash,
			RescreenIntervalDays: d.Regulation.RescreenDays, ValidUntil: timestampOf(d.Regulation.ValidUntil)},
		Destination: api.DestinationView{Country: d.Destination.Country, Source: d.Destination.Source, PartyID: d.Destination.PartyID,
			AddressID: d.Destination.AddressID, Reason: d.Destination.Reason, Evidence: evidenceViews(d.Destination.Evidence)},
		Parties: []api.PartyView{}, Lines: []api.LineView{}, CodeTableVersion: d.CodeTableVersion, ProgrammeLists: d.Lists, Programmes: []api.ProgrammeView{}, Findings: []api.FindingView{},
		Gaps: []api.GapView{}, Outcome: string(d.Outcome), FactsHash: ev.FactsHash,
		EvaluatedBy: kernel.ActorRef{Kind: kernel.ActorKind(d.EvaluatedBy.Kind), ID: d.EvaluatedBy.ID}, EvaluatedAt: timestampOf(d.EvaluatedAt),
		ValidUntil: timestampOf(d.ValidUntil), AuthorityEpoch: d.AuthorityEpoch}
	for _, p := range d.Parties {
		v.Parties = append(v.Parties, api.PartyView{Role: p.Role, PartyID: p.PartyID, PartyRevision: p.PartyRevision, AddressID: p.AddressID,
			Country: p.Country, Source: p.Source, Evidence: evidenceViews(p.Evidence)})
	}
	for _, l := range d.Lines {
		v.Lines = append(v.Lines, api.LineView(l))
	}
	if e := d.EndUse; e != nil {
		v.EndUse = &api.EndUseView{Statement: e.Statement, Evidence: evidenceViews(e.Evidence)}
	}
	for _, p := range d.Programmes {
		v.Programmes = append(v.Programmes, api.ProgrammeView(p))
	}
	for _, f := range d.Findings {
		v.Findings = append(v.Findings, api.FindingView(f))
	}
	for _, g := range d.Gaps {
		v.Gaps = append(v.Gaps, api.GapView(g))
	}
	return v
}

func decisionView(d *tcc.Decision, evs []*tcc.Evaluation) api.DecisionView {
	v := api.DecisionView{DecisionID: d.ID, EvaluationID: d.EvaluationID, Conclusion: string(d.Conclusion), Reason: d.Reason,
		LicenseRequired: d.LicenseRequired, Regulation: api.RegulationView{ID: d.Regulation.ID, Revision: d.Regulation.Revision,
			ContentHash: d.Regulation.ContentHash}, ValidUntil: d.ValidUntil, Evidence: evidenceViews(d.Evidence), DecidedBy: d.By,
		DecidedAt: d.At, AuthorityEpoch: d.Epoch}
	for _, ev := range evs {
		if ev.EvaluationID == d.EvaluationID {
			v.Regulation.RescreenIntervalDays = ev.Regulation.RescreenDays
			v.Regulation.ValidUntil = timestampOf(ev.Regulation.ValidUntil)
		}
	}
	return v
}
