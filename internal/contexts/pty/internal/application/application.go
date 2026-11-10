// Package application runs PTY's commands and queries on the command ledger. Lifecycle and
// confirmation rules live in domain; SQL lives in adapters/postgres. Evidence sources are EVD's: a
// confirmation reads them through EVD's api before PTY's transaction begins (Context Map R61).
package application

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/contact"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/lifecycle"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/party"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Read commands and permissions.
const (
	CommandGetParty   = "party.get"
	CommandGetContact = "contact.get"
	CommandAudit      = "pty.audit"
	PermissionRead    = "pty.read"
	PermissionAudit   = "pty.audit"
)

var (
	anyActor     = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}
	humanOrAgent = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}
)

// Rules is PTY's authorization policy. Confirming and deactivating are human only (05 stage
// humanOnlyCommands); saving and revising drafts are open to AI agents with the grant.
func Rules() []authorization.Rule {
	return []authorization.Rule{
		{Command: api.CommandSaveParty, Permission: api.CommandSaveParty, ActorKinds: humanOrAgent},
		authorization.HumanOnly(api.CommandConfirmParty, api.CommandConfirmParty),
		{Command: api.CommandReviseParty, Permission: api.CommandReviseParty, ActorKinds: humanOrAgent},
		authorization.HumanOnly(api.CommandDeactivateParty, api.CommandDeactivateParty),
		{Command: api.CommandSaveContact, Permission: api.CommandSaveContact, ActorKinds: humanOrAgent},
		authorization.HumanOnly(api.CommandConfirmContact, api.CommandConfirmContact),
		{Command: CommandGetParty, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandGetContact, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandAudit, Permission: PermissionAudit, ActorKinds: anyActor},
	}
}

// Visible reports whether d's actor sees the party or contact a stored result names.
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, id string) (bool, error) {
	scope, _ := bento.ScopeFromContext(ctx)
	if strings.HasPrefix(command, "contact.") {
		c, err := postgres.LoadContact(ctx, exec, scope, id, postgres.NoLock)
		return err == nil && c != nil && d.Sees(c.OwnerActor, c.OwnerTeam), err
	}
	p, err := postgres.LoadParty(ctx, exec, scope, id, postgres.NoLock)
	return err == nil && p != nil && d.Sees(p.OwnerActor, p.OwnerTeam), err
}

// Service runs PTY's use cases.
type Service struct {
	Ledger  *action.Ledger
	Clock   application.Clock
	Sources evdapi.Queries
}

func (s Service) run(ctx context.Context, cmd action.Command, args any, validation error, h action.Handler) (api.Receipt, error) {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, cmd.Name, cmd.ObjectID, rej.Code); err != nil {
				return api.Receipt{}, errors.Join(validation, err)
			}
		}
		return api.Receipt{}, validation
	}
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.Receipt{}, err
	}
	cmd.RequestHash = hash
	res, err := s.Ledger.Execute(ctx, cmd, h)
	return api.Receipt(res), err
}

// SaveParty saves a whole draft party (Python master.upsert_draft kind=customer/supplier).
func (s Service) SaveParty(ctx context.Context, cmd api.SaveParty) (api.Receipt, error) {
	roles := make([]party.Role, len(cmd.Roles))
	for i, r := range cmd.Roles {
		roles[i] = party.Role(r)
	}
	return s.run(ctx, action.Command{Name: api.CommandSaveParty, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.PartyID},
		struct {
			ID, Name string
			Roles    []string
		}{cmd.PartyID, cmd.Name, cmd.Roles}, cmd.Validate(),
		&partyCommand{s: s, id: cmd.PartyID, apply: func(h *partyCommand, ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
			if err := lifecycle.CheckSave(h.record(), h.visible(d), cmd.ExpectedVersion); err != nil {
				return action.Outcome{}, err
			}
			if h.p == nil {
				h.p = &party.Party{Record: lifecycle.New(cmd.PartyID, d.Actor.ID, d.Team)}
				h.p.ReplaceDraft(cmd.Name, roles)
				return h.outcome(nil), postgres.InsertParty(ctx, exec, h.scope(ctx), h.p, s.Clock.Now())
			}
			h.p.ReplaceDraft(cmd.Name, roles)
			return h.save(ctx, exec, nil)
		}})
}

// ConfirmParty confirms a draft party on readable evidence (Python master.confirm).
func (s Service) ConfirmParty(ctx context.Context, cmd api.ConfirmParty) (api.Receipt, error) {
	h := &partyCommand{s: s, id: cmd.PartyID, evidence: cmd.EvidenceIDs}
	h.apply = func(h *partyCommand, ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := lifecycle.Check(h.record(), h.visible(d), cmd.ExpectedVersion, lifecycle.Draft); err != nil {
			return action.Outcome{}, err
		}
		if err := h.checkEvidence(); err != nil {
			return action.Outcome{}, err
		}
		if err := h.p.ReadyToConfirm(); err != nil {
			return action.Outcome{}, err
		}
		h.p.Move(lifecycle.Confirmed)
		err := postgres.InsertPartyRevision(ctx, exec, h.scope(ctx), h.p, postgres.Confirmation{
			EvidenceIDs: cmd.EvidenceIDs, By: d.Actor, Epoch: d.Epoch, At: s.Clock.Now()})
		if err != nil {
			return action.Outcome{}, err
		}
		return h.save(ctx, exec, &action.Event{Type: api.EventPartyConfirmed})
	}
	return s.run(ctx, action.Command{Name: api.CommandConfirmParty, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.PartyID},
		struct {
			ID       string
			Evidence []string
		}{cmd.PartyID, cmd.EvidenceIDs}, cmd.Validate(), &preparedParty{h})
}

// ReviseParty reopens a confirmed party as a draft; its confirmed revisions stay (PTY-INV-001).
func (s Service) ReviseParty(ctx context.Context, cmd api.ReviseParty) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: api.CommandReviseParty, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.PartyID, Reason: cmd.Reason},
		struct{ ID, Reason string }{cmd.PartyID, cmd.Reason}, cmd.Validate(),
		&partyCommand{s: s, id: cmd.PartyID, apply: func(h *partyCommand, ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
			if err := lifecycle.Check(h.record(), h.visible(d), cmd.ExpectedVersion, lifecycle.Confirmed); err != nil {
				return action.Outcome{}, err
			}
			h.p.Move(lifecycle.Draft)
			return h.save(ctx, exec, &action.Event{Type: api.EventPartyRevised})
		}})
}

// DeactivateParty stops a confirmed party for new business (Python master.deactivate).
func (s Service) DeactivateParty(ctx context.Context, cmd api.DeactivateParty) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: api.CommandDeactivateParty, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.PartyID, Reason: cmd.Reason},
		struct{ ID, Reason string }{cmd.PartyID, cmd.Reason}, cmd.Validate(),
		&partyCommand{s: s, id: cmd.PartyID, apply: func(h *partyCommand, ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
			if err := lifecycle.Check(h.record(), h.visible(d), cmd.ExpectedVersion, lifecycle.Confirmed); err != nil {
				return action.Outcome{}, err
			}
			h.p.Move(lifecycle.Inactive)
			return h.save(ctx, exec, &action.Event{Type: api.EventPartyDeactivated})
		}})
}

// partyCommand locks the party root and applies one party command.
type partyCommand struct {
	s        Service
	id       string
	evidence []string
	sources  map[string]evdapi.SourceRef
	p        *party.Party
	from     int64
	apply    func(h *partyCommand, ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *partyCommand) scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

func (h *partyCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.p, err = postgres.LoadParty(ctx, exec, h.scope(ctx), h.id, postgres.ForUpdate)
	if h.p != nil {
		h.from = h.p.RowVersion
	}
	return err
}

func (h *partyCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(h, ctx, exec, d)
}

func (h *partyCommand) record() *lifecycle.Record {
	if h.p == nil {
		return nil
	}
	return &h.p.Record
}

func (h *partyCommand) visible(d authorization.Decision) bool {
	return h.p != nil && d.Sees(h.p.OwnerActor, h.p.OwnerTeam)
}

func (h *partyCommand) checkEvidence() error { return checkEvidence(h.evidence, h.sources) }

func (h *partyCommand) save(ctx context.Context, exec bentopg.Executor, event *action.Event) (action.Outcome, error) {
	if err := postgres.UpdateParty(ctx, exec, h.scope(ctx), h.p, h.from, h.s.Clock.Now()); err != nil {
		return action.Outcome{}, err
	}
	return h.outcome(event), nil
}

func (h *partyCommand) outcome(event *action.Event) action.Outcome {
	out := action.Outcome{ObjectID: h.p.ID, RowVersion: h.p.RowVersion, Status: string(h.p.Status)}
	if event != nil {
		event.AggregateType, event.AggregateID, event.AggregateVersion = "Party", h.p.ID, h.p.RowVersion
		event.Fields = map[string]any{"party_id": h.p.ID, "row_version": h.p.RowVersion, "revision": h.p.Revision}
		out.Events = []action.Event{*event}
	}
	return out
}

// preparedParty reads the evidence sources from EVD before PTY's transaction.
type preparedParty struct{ *partyCommand }

func (h *preparedParty) Prepare(ctx context.Context) error {
	var err error
	h.sources, err = h.s.Sources.ResolveSources(ctx, h.evidence)
	return err
}

// checkEvidence refuses evidence the confirming actor may not read (Python _sources: REFERENCE_INVALID
// at the item's path).
func checkEvidence(ids []string, sources map[string]evdapi.SourceRef) error {
	for i, id := range ids {
		if _, ok := sources[id]; !ok {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/args/evidence_ids/"+strconv.Itoa(i), "Source is not accessible")
		}
	}
	return nil
}

// SaveContact saves a whole draft contact of a customer (Python master.upsert_draft kind=contact).
func (s Service) SaveContact(ctx context.Context, cmd api.SaveContact) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: api.CommandSaveContact, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ContactID},
		struct{ ID, Name, Party string }{cmd.ContactID, cmd.Name, cmd.PartyID}, cmd.Validate(),
		&contactCommand{s: s, id: cmd.ContactID, apply: func(h *contactCommand, ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
			if err := lifecycle.CheckSave(h.record(), h.visible(d), cmd.ExpectedVersion); err != nil {
				return action.Outcome{}, err
			}
			if cmd.PartyID != "" {
				if err := h.checkCustomer(ctx, exec, d, cmd.PartyID, false); err != nil {
					return action.Outcome{}, err
				}
			}
			if h.c == nil {
				h.c = &contact.Contact{Record: lifecycle.New(cmd.ContactID, d.Actor.ID, d.Team)}
				h.c.ReplaceDraft(cmd.Name, cmd.PartyID)
				return h.outcome(false), postgres.InsertContact(ctx, exec, h.scope(ctx), h.c, s.Clock.Now())
			}
			h.c.ReplaceDraft(cmd.Name, cmd.PartyID)
			return h.save(ctx, exec, false)
		}})
}

// ConfirmContact confirms a draft contact of a confirmed customer on readable evidence.
func (s Service) ConfirmContact(ctx context.Context, cmd api.ConfirmContact) (api.Receipt, error) {
	h := &contactCommand{s: s, id: cmd.ContactID, evidence: cmd.EvidenceIDs}
	h.apply = func(h *contactCommand, ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := lifecycle.Check(h.record(), h.visible(d), cmd.ExpectedVersion, lifecycle.Draft); err != nil {
			return action.Outcome{}, err
		}
		if err := checkEvidence(h.evidence, h.sources); err != nil {
			return action.Outcome{}, err
		}
		if err := h.c.ReadyToConfirm(); err != nil {
			return action.Outcome{}, err
		}
		if err := h.checkCustomer(ctx, exec, d, h.c.PartyID, true); err != nil {
			return action.Outcome{}, err
		}
		h.c.Move(lifecycle.Confirmed)
		err := postgres.InsertContactRevision(ctx, exec, h.scope(ctx), h.c, postgres.Confirmation{
			EvidenceIDs: cmd.EvidenceIDs, By: d.Actor, Epoch: d.Epoch, At: s.Clock.Now()})
		if err != nil {
			return action.Outcome{}, err
		}
		return h.save(ctx, exec, true)
	}
	return s.run(ctx, action.Command{Name: api.CommandConfirmContact, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ContactID},
		struct {
			ID       string
			Evidence []string
		}{cmd.ContactID, cmd.EvidenceIDs}, cmd.Validate(), &preparedContact{h})
}

type contactCommand struct {
	s        Service
	id       string
	evidence []string
	sources  map[string]evdapi.SourceRef
	c        *contact.Contact
	from     int64
	apply    func(h *contactCommand, ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *contactCommand) scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

func (h *contactCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.c, err = postgres.LoadContact(ctx, exec, h.scope(ctx), h.id, postgres.ForUpdate)
	if h.c != nil {
		h.from = h.c.RowVersion
	}
	return err
}

func (h *contactCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(h, ctx, exec, d)
}

func (h *contactCommand) record() *lifecycle.Record {
	if h.c == nil {
		return nil
	}
	return &h.c.Record
}

func (h *contactCommand) visible(d authorization.Decision) bool {
	return h.c != nil && d.Sees(h.c.OwnerActor, h.c.OwnerTeam)
}

// checkCustomer requires partyID to be a customer the actor sees and, for a confirmation, a
// confirmed one. The party row is held FOR SHARE after the contact root, so a deactivation waits
// for the confirmation (Python _master_ref).
func (h *contactCommand) checkCustomer(ctx context.Context, exec bentopg.Executor, d authorization.Decision, partyID string, confirmed bool) error {
	p, err := postgres.LoadParty(ctx, exec, h.scope(ctx), partyID, postgres.ForShare)
	if err != nil {
		return err
	}
	if p == nil || !d.Sees(p.OwnerActor, p.OwnerTeam) || !p.Has(party.Customer) || (confirmed && p.Status != lifecycle.Confirmed) {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/party_id", "")
	}
	return nil
}

func (h *contactCommand) save(ctx context.Context, exec bentopg.Executor, confirmed bool) (action.Outcome, error) {
	if err := postgres.UpdateContact(ctx, exec, h.scope(ctx), h.c, h.from, h.s.Clock.Now()); err != nil {
		return action.Outcome{}, err
	}
	return h.outcome(confirmed), nil
}

func (h *contactCommand) outcome(confirmed bool) action.Outcome {
	out := action.Outcome{ObjectID: h.c.ID, RowVersion: h.c.RowVersion, Status: string(h.c.Status)}
	if confirmed {
		out.Events = []action.Event{{Type: api.EventContactConfirmed, AggregateType: "Contact", AggregateID: h.c.ID, AggregateVersion: h.c.RowVersion,
			Fields: map[string]any{"contact_id": h.c.ID, "party_id": h.c.PartyID, "row_version": h.c.RowVersion}}}
	}
	return out
}

type preparedContact struct{ *contactCommand }

func (h *preparedContact) Prepare(ctx context.Context) error {
	var err error
	h.sources, err = h.s.Sources.ResolveSources(ctx, h.evidence)
	return err
}

// GetParty returns a party and its confirmed revisions, audited.
func (s Service) GetParty(ctx context.Context, id string) (api.PartyView, error) {
	var v api.PartyView
	err := s.Ledger.Read(ctx, CommandGetParty, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		scope, _ := bento.ScopeFromContext(ctx)
		p, err := postgres.LoadParty(ctx, exec, scope, id, postgres.NoLock)
		if err != nil {
			return err
		}
		if p == nil || !d.Sees(p.OwnerActor, p.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.PartyView{PartyID: p.ID, Status: string(p.Status), RowVersion: p.RowVersion, Name: p.Name, Roles: roleNames(p)}
		revisions, err := postgres.PartyRevisions(ctx, exec, scope, id)
		for _, r := range revisions {
			v.Revisions = append(v.Revisions, api.PartyRevision{Revision: r.Revision, Name: r.Name, Roles: r.Roles,
				EvidenceIDs: r.Confirmation.EvidenceIDs, ConfirmedBy: r.Confirmation.By})
		}
		return err
	})
	return v, err
}

// GetContact returns a contact, audited.
func (s Service) GetContact(ctx context.Context, id string) (api.ContactView, error) {
	var v api.ContactView
	err := s.Ledger.Read(ctx, CommandGetContact, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		scope, _ := bento.ScopeFromContext(ctx)
		c, err := postgres.LoadContact(ctx, exec, scope, id, postgres.NoLock)
		if err != nil {
			return err
		}
		if c == nil || !d.Sees(c.OwnerActor, c.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.ContactView{ContactID: c.ID, PartyID: c.PartyID, Status: string(c.Status), RowVersion: c.RowVersion, Name: c.Name}
		v.EvidenceIDs, err = postgres.ContactEvidence(ctx, exec, scope, id)
		return err
	})
	return v, err
}

// ResolveParties answers reference lookups of downstream contexts.
func (s Service) ResolveParties(ctx context.Context, q api.Resolve) (api.Resolved, error) {
	out := api.Resolved{Parties: map[string]api.PartyRef{}, Contacts: map[string]api.ContactRef{}}
	if len(q.PartyIDs) == 0 && len(q.ContactIDs) == 0 {
		return out, nil
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		scope, _ := bento.ScopeFromContext(ctx)
		parties, err := postgres.Parties(ctx, exec, scope, slices.Compact(slices.Sorted(slices.Values(q.PartyIDs))))
		if err != nil {
			return err
		}
		for _, p := range parties {
			if d.Sees(p.OwnerActor, p.OwnerTeam) {
				out.Parties[p.ID] = api.PartyRef{PartyID: p.ID, RowVersion: p.RowVersion, Status: string(p.Status), Roles: roleNames(p)}
			}
		}
		contacts, err := postgres.Contacts(ctx, exec, scope, slices.Compact(slices.Sorted(slices.Values(q.ContactIDs))))
		for _, c := range contacts {
			if d.Sees(c.OwnerActor, c.OwnerTeam) {
				out.Contacts[c.ID] = api.ContactRef{ContactID: c.ID, PartyID: c.PartyID, RowVersion: c.RowVersion, Status: string(c.Status)}
			}
		}
		return err
	})
	return out, err
}

// Audit returns PTY's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, CommandAudit)
}

func roleNames(p *party.Party) []string {
	out := make([]string, len(p.Roles))
	for i, r := range p.Roles {
		out[i] = string(r)
	}
	return out
}

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}
