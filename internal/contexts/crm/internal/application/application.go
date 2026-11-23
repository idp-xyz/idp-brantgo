// Package application runs CRM's commands and queries on the command ledger. Stage rules live in
// domain/opportunity; SQL lives in adapters/postgres. A lead cited by an opportunity is read
// before the opportunity transaction (no guard lock). INQ reads OpportunityRef the same way.
package application

import (
	"context"
	"errors"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/crm/api"
	"go.idp.xyz/idp-brant/internal/contexts/crm/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/crm/internal/domain/lead"
	"go.idp.xyz/idp-brant/internal/contexts/crm/internal/domain/opportunity"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	CommandGetLead        = "lead.get"
	CommandGetOpportunity = "opportunity.get"
	CommandAudit          = "crm.audit"
	PermissionRead        = "crm.read"
	PermissionAudit       = "crm.audit"
)

var humanOrAI = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}
var anyActor = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}

// Rules is CRM's authorization policy. None of the three commands is human-only (S6-R0): an AI
// actor may call them through the gateway. The HTTP transport still refuses actor kind ai before
// the body is read.
func Rules() []authorization.Rule {
	return []authorization.Rule{
		{Command: api.CommandCaptureLead, Permission: api.CommandCaptureLead, ActorKinds: humanOrAI},
		{Command: api.CommandAdvanceOpportunity, Permission: api.CommandAdvanceOpportunity, ActorKinds: humanOrAI},
		{Command: api.CommandRecordFollowUp, Permission: api.CommandRecordFollowUp, ActorKinds: humanOrAI},
		{Command: CommandGetLead, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandGetOpportunity, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandAudit, Permission: PermissionAudit, ActorKinds: anyActor},
	}
}

// Visible reports whether d's actor sees the object a stored result names.
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, id string) (bool, error) {
	scope, _ := bento.ScopeFromContext(ctx)
	if command == api.CommandCaptureLead {
		l, err := postgres.LoadLead(ctx, exec, scope, id, false)
		return err == nil && l != nil && d.Sees(l.OwnerActor, l.OwnerTeam), err
	}
	o, err := postgres.LoadOpportunity(ctx, exec, scope, id, false)
	return err == nil && o != nil && d.Sees(o.OwnerActor, o.OwnerTeam), err
}

// Service runs CRM's use cases.
type Service struct {
	Ledger *action.Ledger
	Clock  application.Clock
}

func (s Service) run(ctx context.Context, cmd action.Command, req api.Command, validation error, h action.Handler) (api.Receipt, error) {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, cmd.Name, cmd.ObjectID, rej.Code); err != nil {
				return api.Receipt{}, errors.Join(validation, err)
			}
		}
		return api.Receipt{}, validation
	}
	_, _, args := request(req)
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.Receipt{}, err
	}
	cmd.RequestHash = hash
	res, err := s.Ledger.Execute(ctx, cmd, h)
	return api.Receipt(res), err
}

// CaptureLead appends a lead.
func (s Service) CaptureLead(ctx context.Context, cmd api.CaptureLead) (api.Receipt, error) {
	h := &leadCommand{s: s, id: cmd.LeadID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if h.lead != nil && !d.Sees(h.lead.OwnerActor, h.lead.OwnerTeam) {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/lead_id", "")
		}
		if h.lead != nil {
			return action.Outcome{}, api.Duplicate("/args/lead_id")
		}
		h.lead = lead.Capture(cmd.LeadID, cmd.SourceChannel, cmd.CompanyName, cmd.ContactName, cmd.Summary, d.Actor.ID, d.Team)
		if err := postgres.InsertLead(ctx, exec, scopeOf(ctx), h.lead, kernel.Instant(s.Clock.Now())); err != nil {
			return action.Outcome{}, stored(err, "/args/lead_id")
		}
		return action.Outcome{ObjectID: h.lead.ID, RowVersion: h.lead.RowVersion, Status: h.lead.Status}, nil
	}
	return s.run(ctx, action.Command{Name: api.CommandCaptureLead, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.LeadID}, cmd, cmd.Validate(), h)
}

// AdvanceOpportunity creates an opportunity or moves its stage (SM-CRM-OPPORTUNITY).
func (s Service) AdvanceOpportunity(ctx context.Context, cmd api.AdvanceOpportunity) (api.Receipt, error) {
	h := &opportunityCommand{s: s, id: cmd.OpportunityID}
	if cmd.ExpectedVersion == 0 && cmd.LeadID != "" {
		h.prepare = func(ctx context.Context) error {
			ok, err := s.leadVisible(ctx, cmd.LeadID)
			if err != nil {
				return err
			}
			if !ok {
				return kernel.Reject(kernel.CodeReferenceInvalid, "/args/lead_id", "")
			}
			return nil
		}
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if cmd.ExpectedVersion == 0 {
			if h.opp != nil && !h.visible(d) {
				return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/opportunity_id", "")
			}
			if h.opp != nil {
				return action.Outcome{}, api.Duplicate("/args/opportunity_id")
			}
			h.opp = opportunity.New(cmd.OpportunityID, cmd.Title, cmd.LeadID, cmd.CustomerID, cmd.Currency, cmd.ExpectedAmountMinor, d.Actor.ID, d.Team)
			if err := postgres.InsertOpportunity(ctx, exec, scopeOf(ctx), h.opp, kernel.Instant(s.Clock.Now())); err != nil {
				return action.Outcome{}, stored(err, "/args/opportunity_id")
			}
			return h.outcome(""), nil
		}
		if err := visibleVersion(h.opp, h.visible(d), cmd.ExpectedVersion, "/args/opportunity_id"); err != nil {
			return action.Outcome{}, err
		}
		previous, err := h.opp.Advance(cmd.Stage)
		if err != nil {
			return action.Outcome{}, err
		}
		if err := postgres.UpdateOpportunity(ctx, exec, scopeOf(ctx), h.opp, h.from, kernel.Instant(s.Clock.Now())); err != nil {
			return action.Outcome{}, stored(err, "/args/expected_version")
		}
		return h.outcome(previous), nil
	}
	var handler action.Handler = h
	if h.prepare != nil {
		handler = &preparedOpp{h}
	}
	return s.run(ctx, action.Command{Name: api.CommandAdvanceOpportunity, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OpportunityID}, cmd, cmd.Validate(), handler)
}

// RecordFollowUp appends a follow-up under the opportunity row lock. The stage stays.
func (s Service) RecordFollowUp(ctx context.Context, cmd api.RecordFollowUp) (api.Receipt, error) {
	h := &opportunityCommand{s: s, id: cmd.OpportunityID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := visibleVersion(h.opp, h.visible(d), cmd.ExpectedVersion, "/args/opportunity_id"); err != nil {
			return action.Outcome{}, err
		}
		at := kernel.Instant(s.Clock.Now())
		f := opportunity.FollowUp{ID: cmd.FollowUpID, Summary: cmd.Summary, NextStep: cmd.NextStep, RecordedAt: at}
		if err := h.opp.Record(f); err != nil {
			return action.Outcome{}, err
		}
		if err := postgres.UpdateOpportunity(ctx, exec, scopeOf(ctx), h.opp, h.from, at); err != nil {
			return action.Outcome{}, stored(err, "/args/expected_version")
		}
		if err := postgres.InsertFollowUp(ctx, exec, scopeOf(ctx), h.opp.ID, f); err != nil {
			return action.Outcome{}, stored(err, "/args/follow_up_id")
		}
		return action.Outcome{ObjectID: h.opp.ID, RowVersion: h.opp.RowVersion, Status: h.opp.Stage}, nil
	}
	return s.run(ctx, action.Command{Name: api.CommandRecordFollowUp, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OpportunityID}, cmd, cmd.Validate(), h)
}

func (s Service) leadVisible(ctx context.Context, id string) (bool, error) {
	var ok bool
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		l, err := postgres.LoadLead(ctx, exec, scopeOf(ctx), id, false)
		ok = err == nil && l != nil && d.Sees(l.OwnerActor, l.OwnerTeam)
		return err
	})
	return ok, err
}

// ResolveOpportunity is OpportunityRef v1: a lock-free read in the caller's scope. An id the
// caller cannot see comes back empty, which INQ answers as REFERENCE_INVALID.
func (s Service) ResolveOpportunity(ctx context.Context, opportunityID string) (api.OpportunityRef, error) {
	var ref api.OpportunityRef
	if !kernel.ValidIdentifier(opportunityID) {
		return ref, nil
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		o, err := postgres.LoadOpportunity(ctx, exec, scopeOf(ctx), opportunityID, false)
		if err != nil || o == nil || !d.Sees(o.OwnerActor, o.OwnerTeam) {
			return err
		}
		ref = api.OpportunityRef{OpportunityID: o.ID, CustomerID: o.CustomerID, Stage: o.Stage, Active: o.Active()}
		return nil
	})
	return ref, err
}

func request(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.CaptureLead:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandCaptureLead, key, c
	case api.AdvanceOpportunity:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandAdvanceOpportunity, key, c
	case api.RecordFollowUp:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandRecordFollowUp, key, c
	}
	return "", "", nil
}

// Reconcile reports what became of cmd without executing it.
func (s Service) Reconcile(ctx context.Context, cmd api.Command) (api.Reconciliation, error) {
	name, key, args := request(cmd)
	if name == "" {
		return api.Reconciliation{}, kernel.Reject(kernel.CodeSchemaInvalid, "/command", "")
	}
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.Reconciliation{}, err
	}
	r, err := s.Ledger.Reconcile(ctx, name, key, hash)
	if err != nil {
		return api.Reconciliation{}, err
	}
	out := api.Reconciliation{State: r.State, ActionID: r.ActionID, ObjectID: r.ObjectID}
	if r.Result != nil {
		receipt := api.Receipt(*r.Result)
		out.Receipt = &receipt
	}
	return out, nil
}

// GetLead returns a lead the caller sees, audited.
func (s Service) GetLead(ctx context.Context, id string) (api.LeadView, error) {
	var v api.LeadView
	err := s.Ledger.Read(ctx, CommandGetLead, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		l, err := postgres.LoadLead(ctx, exec, scopeOf(ctx), id, false)
		if err != nil {
			return err
		}
		if l == nil || !d.Sees(l.OwnerActor, l.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.LeadView{LeadID: l.ID, SourceChannel: l.SourceChannel, CompanyName: l.CompanyName, ContactName: l.ContactName,
			Summary: l.Summary, Status: l.Status, RowVersion: l.RowVersion}
		return nil
	})
	return v, err
}

// GetOpportunity returns an opportunity the caller sees, with its follow-ups, audited.
func (s Service) GetOpportunity(ctx context.Context, id string) (api.OpportunityView, error) {
	var v api.OpportunityView
	err := s.Ledger.Read(ctx, CommandGetOpportunity, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		o, err := postgres.LoadOpportunity(ctx, exec, scopeOf(ctx), id, false)
		if err != nil {
			return err
		}
		if o == nil || !d.Sees(o.OwnerActor, o.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = viewOf(o)
		return nil
	})
	return v, err
}

// Audit returns CRM's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, CommandAudit)
}

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}

func viewOf(o *opportunity.Opportunity) api.OpportunityView {
	v := api.OpportunityView{OpportunityID: o.ID, LeadID: o.LeadID, CustomerID: o.CustomerID, Title: o.Title, Stage: o.Stage,
		ExpectedAmountMinor: o.AmountMinor, Currency: o.Currency, RowVersion: o.RowVersion, FollowUps: []api.FollowUpView{}}
	for _, f := range o.FollowUps {
		v.FollowUps = append(v.FollowUps, api.FollowUpView{FollowUpID: f.ID, Summary: f.Summary, NextStep: f.NextStep, RecordedAt: f.RecordedAt})
	}
	return v
}

func visibleVersion(o *opportunity.Opportunity, visible bool, expected int64, path string) error {
	switch {
	case o == nil || !visible:
		return kernel.Reject(kernel.CodeNotFound, path, "")
	case o.RowVersion != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

func stored(err error, path string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, postgres.ErrDuplicate):
		return api.Duplicate(path)
	case errors.Is(err, postgres.ErrConflict):
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return err
}

func scopeOf(ctx context.Context) kernel.Scope {
	if s, ok := bento.ScopeFromContext(ctx); ok {
		return s
	}
	return kernel.Scope{}
}

type leadCommand struct {
	s     Service
	id    string
	lead  *lead.Lead
	apply func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *leadCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.lead, err = postgres.LoadLead(ctx, exec, scopeOf(ctx), h.id, true)
	return err
}

func (h *leadCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

type opportunityCommand struct {
	s       Service
	id      string
	opp     *opportunity.Opportunity
	from    int64
	prepare func(ctx context.Context) error
	apply   func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *opportunityCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.opp, err = postgres.LoadOpportunity(ctx, exec, scopeOf(ctx), h.id, true)
	if h.opp != nil {
		h.from = h.opp.RowVersion
	}
	return err
}

func (h *opportunityCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

func (h *opportunityCommand) visible(d authorization.Decision) bool {
	return h.opp != nil && d.Sees(h.opp.OwnerActor, h.opp.OwnerTeam)
}

func (h *opportunityCommand) outcome(previous string) action.Outcome {
	o := h.opp
	return action.Outcome{ObjectID: o.ID, RowVersion: o.RowVersion, Status: o.Stage, Events: []action.Event{{
		Type: api.EventOpportunityAdvanced, AggregateType: "Opportunity", AggregateID: o.ID, AggregateVersion: o.RowVersion,
		Fields: map[string]any{"opportunity_id": o.ID, "stage": o.Stage, "previous_stage": previous},
	}}}
}

type preparedOpp struct{ *opportunityCommand }

func (p *preparedOpp) Prepare(ctx context.Context) error { return p.prepare(ctx) }
