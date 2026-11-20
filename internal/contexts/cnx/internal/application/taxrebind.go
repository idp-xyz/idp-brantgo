package application

import (
	"context"
	"strconv"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	tr "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/taxrefund"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Rebinding a case's export tax policy (S5R-D23; tactical model SM-CNX-TAXCASE expected —rebind_policy→ expected,
// CNX-INV-004; humanOnlyCommands and the tax_specialist template name tax_refund_case.rebind_policy; R0 chose the
// name). A case binds the revision POL's ConfigAt v1 answers for its export date when it opens; a revision published
// later whose validity also contains the export date then answers instead, which the case read shows as
// policy_superseded while the case is expected. The tax specialist rebinds the case to it with a reason. Planned and
// after, the binding no longer changes.

func rebindRules() []authorization.Rule {
	return []authorization.Rule{authorization.HumanOnly(api.CommandRebindPolicy, api.CommandRebindPolicy)}
}

func rebindRequest(cmd api.Command) (name, key string, args any) {
	if c, ok := cmd.(api.RebindPolicy); ok {
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandRebindPolicy, key, c
	}
	return "", "", nil
}

// tax_refund_case.rebind_policy (human only; tax specialist). Before CNX's transaction: the case (its policy and export
// date, read without a lock) and what ConfigAt v1 answers for them (POL, policyAt). In the transaction, after the case
// FOR UPDATE, the refusals in this order: NOT_FOUND, VERSION_CONFLICT, a case that is not expected (INVALID_STATE
// /args/case_id), POL's answer (REFERENCE_INVALID, CONFIG_NOT_ACTIVE at /args/revision), an answer that is the revision
// bound (INVALID_STATE: nothing superseded it), a revision other than the answer (REFERENCE_STALE /args/revision), a
// revision published since that answers now (REFERENCE_STALE, ConfigAt's SQL function in the transaction), a taxable
// revision or one that refunds nothing while export_refund evidence counts (INVALID_STATE /args/revision). The case
// moves by one version to the new binding and the rebinding is recorded. Nothing is published: the Context Map names no
// fact for it, and no consumer reads case bindings.
type rebindPolicy struct {
	s         Service
	cmd       api.RebindPolicy
	read      *tr.Case
	policy    *polapi.RevisionAt
	policyErr error
	taxCase   *tr.Case
}

func (h *rebindPolicy) Prepare(ctx context.Context) error {
	h.read, h.policy, h.policyErr = nil, nil, nil
	if err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		h.read, err = postgres.LoadCase(ctx, exec, scope(ctx), h.cmd.CaseID, postgres.NoLock)
		return err
	}); err != nil || h.read == nil {
		return err
	}
	var err error
	h.policy, err = h.s.policyAt(ctx, h.read.Policy.PolicyID, h.read.ExportedAt)
	if rej, ok := kernel.AsRejection(err); ok {
		h.policyErr, err = kernel.RejectAs(rej.Code, rej.Category, "/args/revision", "No revision of export tax policy "+h.read.Policy.PolicyID+
			" applies at the export date"), nil
	}
	return err
}

func (h *rebindPolicy) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.taxCase, err = postgres.LoadCase(ctx, exec, scope(ctx), h.cmd.CaseID, postgres.ForUpdate)
	return err
}

func (h *rebindPolicy) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, cmd, c := scope(ctx), h.cmd, h.taxCase
	const path = "/args/revision"
	switch {
	case c == nil:
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/case_id", "")
	case c.RowVersion != cmd.ExpectedVersion:
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	case c.Status != tr.Expected:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/case_id", "The tax refund case is "+c.Status+
			": only an expected case rebinds its policy")
	case h.read == nil || h.read.Policy.PolicyID != c.Policy.PolicyID || !h.read.ExportedAt.Equal(c.ExportedAt):
		return action.Outcome{}, api.Reject(api.CodeReferenceStale, "/args/case_id", "The case changed: send again")
	case h.policyErr != nil:
		return action.Outcome{}, h.policyErr
	case h.policy.Ref.Revision == c.Policy.Revision:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, path, "The case binds revision "+strconv.FormatInt(c.Policy.Revision, 10)+
			", which ConfigAt v1 answers for its export date: no later revision supersedes it")
	case h.policy.Ref.Revision != cmd.Revision:
		return action.Outcome{}, api.Reject(api.CodeReferenceStale, path, "Revision "+strconv.FormatInt(h.policy.Ref.Revision, 10)+
			" applies at the export date")
	}
	if err := policyStillAt(ctx, exec, h.policy, c.ExportedAt, path); err != nil {
		return action.Outcome{}, err
	}
	allocations, err := postgres.CaseAllocations(ctx, exec, sc, c)
	if err != nil {
		return action.Outcome{}, err
	}
	b := bindingOf(h.policy)
	if err = tr.Rebind(c, b, allocations); err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	from, before := c.RowVersion, c.Policy
	c.RowVersion++
	c.Policy = b
	if err = postgres.RebindCase(ctx, exec, sc, c, from, now); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertRebinding(ctx, exec, sc, &tr.Rebinding{ID: action.NewID("trb", now), CaseID: c.ID, CaseVersion: c.RowVersion,
		PolicyID: c.Policy.PolicyID, From: before, To: b, Reason: cmd.Reason, By: d.Actor, Epoch: d.Epoch, At: now}); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: c.ID, RowVersion: c.RowVersion, Status: c.Status}, nil
}

// RebindPolicy rebinds an expected case to the revision ConfigAt v1 answers now (tax_refund_case.rebind_policy).
func (s Service) RebindPolicy(ctx context.Context, cmd api.RebindPolicy) (api.TaxCaseReceipt, error) {
	h := &rebindPolicy{s: s, cmd: cmd}
	return s.runTaxCase(ctx, action.Command{Name: api.CommandRebindPolicy, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.CaseID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h, nil, nil)
}

// policyAtView is what ConfigAt v1 answers now, inside CNX's read, for an expected case's policy at its export date,
// and whether it supersedes the binding (S5R-D23); nil for a case past expected.
func policyAtView(ctx context.Context, exec bentopg.Executor, c *tr.Case) (*api.PolicyAtView, bool, error) {
	if c.Status != tr.Expected {
		return nil, false, nil
	}
	v, err := configAt(ctx, exec, c.Policy.PolicyID, c.ExportedAt)
	if err != nil {
		return nil, false, err
	}
	out := &api.PolicyAtView{Revision: v.Ref.Revision, ContentHash: v.ContentHash, Code: v.Code}
	if !v.Known && v.Code == "" {
		out.Code = kernel.CodeReferenceInvalid
	}
	return out, v.Applies() && v.Ref.Revision != c.Policy.Revision, nil
}

// rebindingViews are a case's rebindings as the read shows them.
func rebindingViews(rs []*tr.Rebinding) []api.RebindingView {
	out := []api.RebindingView{}
	for _, r := range rs {
		out = append(out, api.RebindingView{RebindingID: r.ID, CaseVersion: r.CaseVersion, FromRevision: r.From.Revision, FromHash: r.From.ContentHash,
			FromMethod: r.From.RefundMethod, ToRevision: r.To.Revision, ToHash: r.To.ContentHash, ToMethod: r.To.RefundMethod, Reason: r.Reason,
			ReboundBy: r.By, AuthorityEpoch: r.Epoch, ReboundAt: r.At})
	}
	return out
}
