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

// decideException runs credit_exception.decide (SM-STL-CREDIT-DECISION, STL-INV-005): before the
// transaction it reads the credit policy (POL); inside it, it locks the customer's credit root FOR
// UPDATE, checks the decision's version and validity, re-checks the policy with ConfigGuard, reads
// the approver's role templates (D-6) and records the release or rejection. Two approvers deciding
// one decision at the same time commit one after the other under the root lock; the second finds
// it decided (STL-INV-005 concurrency).
type decideException struct {
	s        Service
	cmd      api.DecideCreditException
	policy   polapi.ResolvedConfig
	root     *cc.Root
	decision *cc.Decision
	required int
}

func (h *decideException) Prepare(ctx context.Context) error {
	configs, err := h.s.resolveConfigs(ctx, h.s.Clock.Now(), map[string]polapi.ConfigRef{"/args/credit_policy": polRef(h.cmd.CreditPolicy)})
	if err == nil {
		h.policy = configs["/args/credit_policy"]
	}
	return err
}

func (h *decideException) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.root, h.decision, h.required = nil, nil, 0
	var err error
	if h.root, err = lockSubject(ctx, exec, postgres.SubjectDecision, h.cmd.DecisionID); err != nil || h.root == nil {
		return err
	}
	h.decision, err = postgres.LoadDecision(ctx, exec, scope(ctx), h.cmd.DecisionID)
	return err
}

func (h *decideException) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, now := scope(ctx), h.cmd, kernel.Instant(h.s.Clock.Now())
	x := h.decision
	if x == nil || !d.Sees(x.OwnerActor, x.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/decision_id", "")
	}
	if x.Version != c.ExpectedVersion {
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if _, err := creditPolicy(h.policy, "/args/credit_policy"); err != nil {
		return action.Outcome{}, err
	}
	if err := guardConfig(ctx, exec, polRef(c.CreditPolicy), h.policy.ContentHash, now, "/args/credit_policy"); err != nil {
		return action.Outcome{}, err
	}
	policy, err := policyOf(h.policy, x.Limit.Currency())
	if err != nil {
		return action.Outcome{}, err
	}
	a, err := h.s.approver(ctx, exec, d.Actor)
	if err != nil {
		return action.Outcome{}, err
	}
	from, prior := x.Version, len(x.Approvals)
	act := cc.Act{By: d.Actor, Epoch: d.Epoch, At: now, Reason: c.Reason}
	decided := true
	if c.Decision == api.DecisionReject {
		err = x.Reject(a, act)
	} else {
		decided, h.required, err = x.Release(policy, a, act)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	if err = h.s.bump(ctx, exec, h.root); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.UpdateDecision(ctx, exec, sc, x, from, prior); err != nil {
		return action.Outcome{}, err
	}
	var events []action.Event
	if decided {
		events = append(events, event(api.EventCreditExceptionDecided, h.root, map[string]any{"decision_id": x.ID, "decision_version": x.Version,
			"status": x.Status, "order_id": x.Binding.OrderID, "order_revision": x.Binding.OrderRevision}))
	}
	return action.Outcome{ObjectID: x.ID, RowVersion: x.Version, Status: x.Status, Events: events}, nil
}

// DecideCreditException releases or rejects a credit decision (credit_exception.decide). Human only.
func (s Service) DecideCreditException(ctx context.Context, cmd api.DecideCreditException) (api.DecisionReceipt, error) {
	h := &decideException{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandDecideException, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.DecisionID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.DecisionReceipt{}, err
	}
	out := api.DecisionReceipt{Receipt: res, RequiredApprovals: h.required}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		x, err := postgres.LoadDecision(ctx, exec, scope(ctx), res.ObjectID)
		if err != nil || x == nil {
			return err
		}
		out.PartyID, out.Approvals, out.ExpiresAt = x.PartyID, len(x.Approvals), x.ExpiresAt
		return nil
	})
	return out, err
}
