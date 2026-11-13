package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	tcc "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/tradecompliancecheck"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// trade_compliance.reopen (human or system; SM-CNX-COMPLIANCE clear → pending; PL-14; S5 D-9,
// D-18).
//
// regulation_revised, programme_updated and expired act on a check: Prepare reads the regulation
// revision named (POL, R50); the transaction locks the check FOR UPDATE, the authority guard FOR
// SHARE, refuses a check the caller does not see (NOT_FOUND), another row version
// (VERSION_CONFLICT), a check that is not clear or a cause that does not hold (INVALID_STATE: the
// conclusion's own regulation revision, every list it used still current — held FOR SHARE through
// reference data's list guard — a conclusion still valid), then, for regulation_revised, a revision
// that is not a screening_regulation in force (REFERENCE_INVALID, POL's verdict, ConfigGuard v1 FOR
// SHARE).
//
// order_amended opens a pending check for a new order revision: Prepare reads the order (TRD,
// R46); the transaction locks the check of the revision FOR UPDATE (none exists), refuses an order
// the caller does not see (NOT_FOUND), an existing check (VERSION_CONFLICT, expected_version 0), a
// revision that is not the order's current one (REFERENCE_STALE) and an order that ended
// (ORDER_NOT_ACTIVE), and links the check of the order's previous revision.
//
// Each writes the reopening with the check, then TradeComplianceReopened.

type reopenCommand struct {
	s          Service
	cmd        api.Reopen
	check      *tcc.Check
	order      *Order
	regulation *polapi.ResolvedConfig
}

func (h *reopenCommand) Prepare(ctx context.Context) error {
	h.order, h.regulation = nil, nil
	u, cmd := h.s.Upstream, h.cmd
	switch cmd.Cause {
	case api.CauseOrderAmended:
		orders, err := u.Orders.ResolveOrders(ctx, []string{cmd.OrderID})
		if err != nil {
			return err
		}
		if o, ok := orders[cmd.OrderID]; ok {
			h.order = &o
		}
	case api.CauseRegulationRevised:
		ref := polapi.ConfigRef{Kind: polapi.KindScreeningRegulation, ConfigID: cmd.RegulationID, Revision: cmd.RegulationRevision}
		resolved, err := u.Configs.ResolveConfigs(ctx, []polapi.ConfigRef{ref}, h.s.Clock.Now())
		if err != nil {
			return err
		}
		if r, ok := resolved[ref]; ok {
			h.regulation = &r
		}
	}
	return nil
}

func (h *reopenCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	if h.cmd.Cause == api.CauseOrderAmended {
		h.check, err = postgres.LoadCheckOfRevision(ctx, exec, scope(ctx), h.cmd.OrderID, h.cmd.OrderRevision, postgres.ForUpdate)
	} else {
		h.check, err = postgres.LoadCheck(ctx, exec, scope(ctx), h.cmd.CheckID, postgres.ForUpdate)
	}
	return err
}

func (h *reopenCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, cmd := scope(ctx), h.cmd
	now := kernel.Instant(h.s.Clock.Now())
	r := tcc.Reopening{ID: action.NewID("tcr", now), Reason: cmd.Reason, By: d.Actor, Epoch: d.Epoch, At: now}
	var fields map[string]any
	if cmd.Cause == api.CauseOrderAmended {
		o := h.order
		if o == nil || (h.check != nil && !d.Sees(h.check.OwnerActor, h.check.OwnerTeam)) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/order_id", "")
		}
		if err := tcc.CheckVersion(h.check, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if o.Binding.OrderRevision != cmd.OrderRevision {
			return action.Outcome{}, api.Reject(api.CodeReferenceStale, "/args/order_revision", "The order is at another revision")
		}
		if o.Status != OrderDraft && o.Status != OrderActive {
			return action.Outcome{}, api.Reject(api.CodeOrderNotActive, "/args/order_id", "The order is "+o.Status)
		}
		previous, err := postgres.PreviousCheck(ctx, exec, sc, o.Binding.OrderID, o.Binding.OrderRevision)
		if err != nil {
			return action.Outcome{}, err
		}
		h.check = tcc.Open(action.NewID("tcc", now), o.Binding, previous, d.Actor, d.Team, d.Epoch, now)
		r.CheckID, r.Cause = h.check.ID, tcc.CauseOrderAmended
		if err = postgres.InsertCheck(ctx, exec, sc, h.check); err == nil {
			err = postgres.InsertReopening(ctx, exec, sc, &r)
		}
		if err != nil {
			return action.Outcome{}, err
		}
		fields = map[string]any{"previous_check_id": previous}
	} else {
		c := h.check
		if c == nil || !d.Sees(c.OwnerActor, c.OwnerTeam) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/check_id", "")
		}
		if err := tcc.CheckVersion(c, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		var decision *tcc.Decision
		if c.Decision != "" {
			ds, err := postgres.Decisions(ctx, exec, sc, c.ID, c.Decision)
			if err != nil {
				return action.Outcome{}, err
			}
			if len(ds) == 1 {
				decision = ds[0]
			}
		}
		var reg *tcc.RegulationRef
		if cmd.Cause == api.CauseRegulationRevised {
			reg = &tcc.RegulationRef{ID: cmd.RegulationID, Revision: cmd.RegulationRevision}
		}
		var updated []string
		if cmd.Cause == api.CauseProgrammeUpdated && c.Status == tcc.Clear && c.Evaluation != "" {
			evs, err := postgres.Evaluations(ctx, exec, sc, c.ID, c.Evaluation)
			if err != nil {
				return action.Outcome{}, err
			}
			if len(evs) == 1 {
				codes, versions := usedLists(evs[0])
				if updated, err = outdatedLists(ctx, exec, codes, versions); err != nil {
					return action.Outcome{}, err
				}
			}
		}
		from := c.RowVersion
		reopened, err := tcc.Reopen(c, decision, cmd.Cause, reg, updated, r)
		if err != nil {
			return action.Outcome{}, err
		}
		if reg != nil {
			const path = "/args/regulation_id"
			if _, err = regulationOf(h.regulation, path); err != nil {
				return action.Outcome{}, err
			}
			reg.ContentHash = h.regulation.ContentHash
			if err = regulationGuard(ctx, exec, h.regulation.Ref, h.regulation.ContentHash, now, path); err != nil {
				return action.Outcome{}, err
			}
		}
		if err = postgres.InsertReopening(ctx, exec, sc, reopened); err == nil {
			err = postgres.SaveCheck(ctx, exec, sc, c, from, now)
		}
		if err != nil {
			return action.Outcome{}, err
		}
		r = *reopened
		fields = map[string]any{"decision_id": r.DecisionID}
		if reg != nil {
			fields["regulation_id"], fields["regulation_revision"] = reg.ID, reg.Revision
		}
		if r.Updated != nil {
			fields["updated_lists"] = r.Updated
		}
	}
	c := h.check
	fields["check_id"], fields["order_id"], fields["order_revision"], fields["order_hash"], fields["cause"] =
		c.ID, c.Binding.OrderID, c.Binding.OrderRevision, c.Binding.OrderHash, r.Cause
	return action.Outcome{ObjectID: c.ID, RowVersion: c.RowVersion, Status: string(c.Status), Events: []action.Event{{
		Type: api.EventTradeComplianceReopened, AggregateType: "TradeComplianceCheck", AggregateID: c.ID, AggregateVersion: c.RowVersion,
		Fields: fields}}}, nil
}

// Reopen reopens a clear conclusion or opens a pending check for a new order revision
// (trade_compliance.reopen).
func (s Service) Reopen(ctx context.Context, cmd api.Reopen) (api.CheckReceipt, error) {
	h := &reopenCommand{s: s, cmd: cmd}
	object := cmd.CheckID
	if object == "" {
		object = cmd.OrderID
	}
	return s.run(ctx, action.Command{Name: api.CommandReopen, IdempotencyKey: cmd.IdempotencyKey, ObjectID: object, Reason: cmd.Reason},
		cmd, cmd.Validate(), h)
}
