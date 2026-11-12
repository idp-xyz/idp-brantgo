package application

import (
	"context"
	"slices"
	"strconv"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// requestLimit runs credit_limit.request. Before the transaction it reads the customer (PTY), the
// evidence (EVD), the credit policy and the currency (POL); inside it, it locks the customer's
// credit root FOR UPDATE (creating it), re-checks both POL revisions with ConfigGuard and writes
// the request.
type requestLimit struct {
	s        Service
	cmd      api.RequestCreditLimit
	party    *ptyapi.PartyRef
	sources  map[string]bool
	policy   polapi.ResolvedConfig
	currency polapi.ResolvedConfig
	root     *cc.Root
	existing *cc.LimitRequest
	replay   bool
}

func (h *requestLimit) Prepare(ctx context.Context) error {
	c := h.cmd
	parties, err := h.s.Upstream.Parties.ResolveParties(ctx, ptyapi.Resolve{PartyIDs: []string{c.PartyID}})
	if err != nil {
		return err
	}
	if p, ok := parties.Parties[c.PartyID]; ok {
		h.party = &p
	}
	var ids []string
	for _, src := range c.Sources {
		ids = append(ids, src.EvidenceIDs...)
	}
	found, err := h.s.Upstream.Sources.ResolveSources(ctx, ids)
	if err != nil {
		return err
	}
	h.sources = map[string]bool{}
	for id := range found {
		h.sources[id] = true
	}
	configs, err := h.s.resolveConfigs(ctx, h.s.Clock.Now(), map[string]polapi.ConfigRef{
		"/args/credit_policy": polRef(c.CreditPolicy), "/args/currency": polRef(c.Currency)})
	if err != nil {
		return err
	}
	h.policy, h.currency = configs["/args/credit_policy"], configs["/args/currency"]
	return nil
}

func (h *requestLimit) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.root, h.existing, h.replay = nil, nil, false
	sc := scope(ctx)
	party, err := postgres.PartyOf(ctx, exec, sc, postgres.SubjectLimitRequest, h.cmd.RequestID)
	if err != nil {
		return err
	}
	if party == "" {
		party = h.cmd.PartyID
	}
	if h.root, err = h.s.lockOrCreate(ctx, exec, party); err != nil {
		return err
	}
	h.existing, err = postgres.LoadRequest(ctx, exec, sc, h.cmd.RequestID)
	return err
}

func (h *requestLimit) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, now := scope(ctx), h.cmd, kernel.Instant(h.s.Clock.Now())
	if r := h.existing; r != nil {
		// The same request by the same requester under another key is answered from the stored one.
		if !d.Sees(r.OwnerActor, r.OwnerTeam) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/request_id", "")
		}
		if r.PartyID != c.PartyID || r.Requested.By != d.Actor || r.Reason != c.Reason || r.Policy.ID != c.CreditPolicy.ConfigID {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/request_id", "The request ID holds another limit request")
		}
		h.replay = true
		return action.Outcome{ObjectID: r.ID, RowVersion: r.Version, Status: r.Status}, nil
	}
	if h.party == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/party_id", "PTY shows no such party")
	}
	if h.party.Status != ptyapi.StatusConfirmed || !slices.Contains(h.party.Roles, ptyapi.RoleCustomer) {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/party_id", "The party is not a confirmed customer")
	}
	for i, src := range c.Sources {
		for j, e := range src.EvidenceIDs {
			if !h.sources[e] {
				return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid,
					"/args/sources/"+strconv.Itoa(i)+"/evidence_ids/"+strconv.Itoa(j), "EVD shows no such source")
			}
		}
	}
	if _, err := creditPolicy(h.policy, "/args/credit_policy"); err != nil {
		return action.Outcome{}, err
	}
	if err := h.currency.Check("/args/currency"); err != nil {
		return action.Outcome{}, err
	}
	cur := h.currency.Data.Currency
	if cur == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/currency", "Not a currency")
	}
	currency, err := kernel.NewCurrency(cur.Code, cur.MinorUnits)
	if err != nil {
		return action.Outcome{}, err
	}
	if err = guardConfig(ctx, exec, polRef(c.CreditPolicy), h.policy.ContentHash, now, "/args/credit_policy"); err != nil {
		return action.Outcome{}, err
	}
	if err = guardConfig(ctx, exec, polRef(c.Currency), h.currency.ContentHash, now, "/args/currency"); err != nil {
		return action.Outcome{}, err
	}
	policy, err := policyOf(h.policy, currency)
	if err != nil {
		return action.Outcome{}, err
	}
	in := cc.LimitInput{ID: c.RequestID, PartyID: c.PartyID, Currency: currency, CurrencyRevision: c.Currency.Revision,
		Sources: map[string]cc.SourceAmount{}, Reason: c.Reason, Team: d.Team,
		By: cc.Act{By: d.Actor, Epoch: d.Epoch, At: now, Reason: c.Reason}}
	in.ValidFrom, _ = kernel.ParseTimestamp(c.ValidFrom)
	in.ValidUntil, _ = kernel.ParseTimestamp(c.ValidUntil)
	for i, src := range c.Sources {
		m, err := kernel.ParseAmount(currency, src.Amount)
		if err != nil {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/sources/"+strconv.Itoa(i)+"/amount",
				"More fractional digits than "+currency.Code()+" has")
		}
		in.Sources[src.Source] = cc.SourceAmount{Amount: m, EvidenceIDs: src.EvidenceIDs}
	}
	r, err := cc.NewLimitRequest(in, policy)
	if err != nil {
		return action.Outcome{}, err
	}
	if err = h.s.bump(ctx, exec, h.root); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertRequest(ctx, exec, sc, r); err != nil {
		return action.Outcome{}, err
	}
	e := event(api.EventCreditLimitRequested, h.root, map[string]any{"request_id": r.ID, "request_version": r.Version})
	return action.Outcome{ObjectID: r.ID, RowVersion: r.Version, Status: r.Status, Events: []action.Event{e}}, nil
}

// RequestCreditLimit asks for a credit limit (credit_limit.request).
func (s Service) RequestCreditLimit(ctx context.Context, cmd api.RequestCreditLimit) (api.LimitReceipt, error) {
	h := &requestLimit{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandRequestLimit, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.RequestID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.LimitReceipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return s.limitReceipt(ctx, res, 0)
}

// publishLimit runs credit_limit.publish: before the transaction it reads the credit policy (POL);
// inside it, it locks the customer's credit root, checks the request's version, re-checks the policy
// with ConfigGuard, reads the approver's role templates (D-6) and records the approval, publishing
// the limit version once the approvals the amount needs are in.
type publishLimit struct {
	s        Service
	cmd      api.PublishCreditLimit
	policy   polapi.ResolvedConfig
	root     *cc.Root
	request  *cc.LimitRequest
	required int
}

func (h *publishLimit) Prepare(ctx context.Context) error {
	configs, err := h.s.resolveConfigs(ctx, h.s.Clock.Now(), map[string]polapi.ConfigRef{"/args/credit_policy": polRef(h.cmd.CreditPolicy)})
	if err == nil {
		h.policy = configs["/args/credit_policy"]
	}
	return err
}

func (h *publishLimit) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.root, h.request, h.required = nil, nil, 0
	var err error
	if h.root, err = lockSubject(ctx, exec, postgres.SubjectLimitRequest, h.cmd.RequestID); err != nil || h.root == nil {
		return err
	}
	h.request, err = postgres.LoadRequest(ctx, exec, scope(ctx), h.cmd.RequestID)
	return err
}

func (h *publishLimit) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, now := scope(ctx), h.cmd, kernel.Instant(h.s.Clock.Now())
	r := h.request
	if r == nil || !d.Sees(r.OwnerActor, r.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/request_id", "")
	}
	if r.Version != c.ExpectedVersion {
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if _, err := creditPolicy(h.policy, "/args/credit_policy"); err != nil {
		return action.Outcome{}, err
	}
	if err := guardConfig(ctx, exec, polRef(c.CreditPolicy), h.policy.ContentHash, now, "/args/credit_policy"); err != nil {
		return action.Outcome{}, err
	}
	policy, err := policyOf(h.policy, r.Currency)
	if err != nil {
		return action.Outcome{}, err
	}
	a, err := h.s.approver(ctx, exec, d.Actor)
	if err != nil {
		return action.Outcome{}, err
	}
	from, prior := r.Version, len(r.Approvals)
	act := cc.Act{By: d.Actor, Epoch: d.Epoch, At: now, Reason: c.Reason}
	var version *cc.LimitVersion
	if c.Decision == api.DecisionReject {
		err = r.Reject(a, act)
	} else {
		version, h.required, err = r.Publish(policy, a, act, h.root.LimitVersion+1)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	if version != nil {
		h.root.LimitVersion = version.Version
	}
	if err = h.s.bump(ctx, exec, h.root); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.UpdateRequest(ctx, exec, sc, r, from, prior); err != nil {
		return action.Outcome{}, err
	}
	var events []action.Event
	if version != nil {
		if err = postgres.InsertVersion(ctx, exec, sc, version); err != nil {
			return action.Outcome{}, err
		}
		events = append(events, event(api.EventCreditLimitPublished, h.root, map[string]any{"request_id": r.ID, "limit_version": version.Version}))
	}
	return action.Outcome{ObjectID: r.ID, RowVersion: r.Version, Status: r.Status, Events: events}, nil
}

// PublishCreditLimit publishes or rejects a limit request (credit_limit.publish). Human only.
func (s Service) PublishCreditLimit(ctx context.Context, cmd api.PublishCreditLimit) (api.LimitReceipt, error) {
	h := &publishLimit{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandPublishLimit, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.RequestID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.LimitReceipt{}, err
	}
	return s.limitReceipt(ctx, res, h.required)
}

// limitReceipt completes res with what the stored request says.
func (s Service) limitReceipt(ctx context.Context, res api.Receipt, required int) (api.LimitReceipt, error) {
	out := api.LimitReceipt{Receipt: res, RequiredApprovals: required}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		r, err := postgres.LoadRequest(ctx, exec, scope(ctx), res.ObjectID)
		if err != nil || r == nil {
			return err
		}
		out.PartyID, out.Currency, out.AmountMinor, out.Approvals, out.LimitVersion = r.PartyID, r.Currency.Code(), r.Amount.MinorString(),
			len(r.Approvals), r.LimitVersion
		return nil
	})
	return out, err
}
