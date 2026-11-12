package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	tcc "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/tradecompliancecheck"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// trade_compliance.decide (human only, compliance-exclusive; CNX-INV-001, TRD-INV-023, ES-11
// step 5, TD-AT12).
//
// Before CNX's transaction, Prepare reads the evidence sources (EVD, R54). In the transaction: the
// check FOR UPDATE; the authority guard FOR SHARE; then the refusals in this order: a check the
// caller does not see (NOT_FOUND), its row version (VERSION_CONFLICT), the evidence
// (REFERENCE_INVALID), the domain's (PERMISSION_DENIED for a non-human, INVALID_STATE for a check
// that is not pending, REFERENCE_STALE for another evaluation or one past its validity,
// DECLARATION_PROFILE_UNVERIFIED and INVALID_STATE for an incomplete evaluation concluded clear),
// and, for clear, the regulation revision the evaluation followed, still in force at its content
// hash (POL's ConfigGuard v1 FOR SHARE: CONFIG_NOT_ACTIVE, CONFIG_EXPIRED, REFERENCE_STALE), then
// every programme list it used still at the version it used (reference data's list guard FOR
// SHARE, list-code order: REFERENCE_STALE — evaluate again). A blocked conclusion relies on no
// regulation or list being current. The decision is written with the
// check, then TradeComplianceDecided.

type decideCommand struct {
	s       Service
	cmd     api.Decide
	check   *tcc.Check
	sources map[string]evdapi.SourceRef
}

func (h *decideCommand) Prepare(ctx context.Context) (err error) {
	h.sources, err = h.s.sources(ctx, h.cmd.EvidenceIDs)
	return err
}

func (h *decideCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.check, err = postgres.LoadCheck(ctx, exec, scope(ctx), h.cmd.CheckID, postgres.ForUpdate)
	return err
}

func (h *decideCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, cmd := scope(ctx), h.check, h.cmd
	if c == nil || !d.Sees(c.OwnerActor, c.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/check_id", "")
	}
	if err := tcc.CheckVersion(c, cmd.ExpectedVersion); err != nil {
		return action.Outcome{}, err
	}
	evidence, err := evidenceOf(h.sources, cmd.EvidenceIDs, "/args/evidence_ids")
	if err != nil {
		return action.Outcome{}, err
	}
	var ev *tcc.Evaluation
	if c.Evaluation != "" {
		evs, err := postgres.Evaluations(ctx, exec, sc, c.ID, c.Evaluation)
		if err != nil {
			return action.Outcome{}, err
		}
		if len(evs) == 1 {
			ev = evs[0]
		}
	}
	now := kernel.Instant(h.s.Clock.Now())
	in := tcc.DecideInput{ID: action.NewID("tcd", now), EvaluationID: cmd.EvaluationID, Conclusion: tcc.Status(cmd.Conclusion), Reason: cmd.Reason,
		Evidence: evidence, By: d.Actor, Epoch: d.Epoch, At: now}
	if cmd.LicenseRequired != nil {
		in.LicenseRequired = *cmd.LicenseRequired
	}
	dec, err := tcc.Decide(c, ev, in)
	if err != nil {
		return action.Outcome{}, err
	}
	if dec.Conclusion == tcc.Clear {
		ref := polapi.ConfigRef{Kind: polapi.KindScreeningRegulation, ConfigID: dec.Regulation.ID, Revision: dec.Regulation.Revision}
		if err = regulationGuard(ctx, exec, ref, dec.Regulation.ContentHash, now, "/args/evaluation_id"); err != nil {
			return action.Outcome{}, err
		}
		codes, versions := usedLists(ev)
		if err = listsStillCurrent(ctx, exec, codes, versions, "/args/evaluation_id"); err != nil {
			return action.Outcome{}, err
		}
	}
	from := c.RowVersion
	c.Decided(dec)
	if err = postgres.InsertDecision(ctx, exec, sc, dec); err == nil {
		err = postgres.SaveCheck(ctx, exec, sc, c, from, now)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	fields := map[string]any{"check_id": c.ID, "order_id": c.Binding.OrderID, "order_revision": c.Binding.OrderRevision,
		"order_hash": c.Binding.OrderHash, "decision_id": dec.ID, "evaluation_id": dec.EvaluationID, "conclusion": string(dec.Conclusion),
		"license_required": dec.LicenseRequired, "regulation_id": dec.Regulation.ID, "regulation_revision": dec.Regulation.Revision}
	if !dec.ValidUntil.IsZero() {
		fields["valid_until"] = kernel.FormatTimestamp(dec.ValidUntil)
	}
	return action.Outcome{ObjectID: c.ID, RowVersion: c.RowVersion, Status: string(c.Status), Events: []action.Event{{
		Type: api.EventTradeComplianceDecided, AggregateType: "TradeComplianceCheck", AggregateID: c.ID, AggregateVersion: c.RowVersion,
		Fields: fields}}}, nil
}

// Decide concludes a check on its current evaluation (trade_compliance.decide).
func (s Service) Decide(ctx context.Context, cmd api.Decide) (api.CheckReceipt, error) {
	h := &decideCommand{s: s, cmd: cmd}
	return s.run(ctx, action.Command{Name: api.CommandDecide, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.CheckID, Reason: cmd.Reason},
		cmd, cmd.Validate(), h)
}
