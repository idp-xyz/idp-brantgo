// Package application runs INQ's commands and queries on the command ledger. RFQ rules live in
// domain/rfq; SQL lives in adapters/postgres. Parties, contacts, SKUs and sources belong to PTY,
// CAT and EVD: INQ reads them through their api packages before its own transaction begins
// (Context Map R01–R03 carry no guard lock), and the domain judges what they said.
package application

import (
	"context"
	"errors"
	"slices"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/contexts/inq/api"
	"go.idp.xyz/idp-brant/internal/contexts/inq/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/inq/internal/domain/rfq"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Read commands and permissions.
const (
	CommandGetRfq   = "rfq.get"
	CommandAudit    = "inq.audit"
	PermissionRead  = "inq.read"
	PermissionAudit = "inq.audit"
)

var anyActor = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}

// Rules is INQ's authorization policy (05 stage humanOnlyCommands: rfq.confirm_scope, rfq.cancel,
// rfq.close). An AI agent may prepare drafts through the same command path.
func Rules() []authorization.Rule {
	return []authorization.Rule{
		{Command: api.CommandSaveRfq, Permission: api.CommandSaveRfq, ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}},
		authorization.HumanOnly(api.CommandConfirmRfq, api.CommandConfirmRfq),
		authorization.HumanOnly(api.CommandCancelRfq, api.CommandCancelRfq),
		authorization.HumanOnly(api.CommandCloseRfq, api.CommandCloseRfq),
		{Command: CommandGetRfq, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandAudit, Permission: PermissionAudit, ActorKinds: anyActor},
	}
}

// Visible reports whether d's actor sees the RFQ a stored result names.
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, _, id string) (bool, error) {
	scope, _ := bento.ScopeFromContext(ctx)
	r, err := postgres.Load(ctx, exec, scope, id, false)
	return err == nil && r != nil && d.Sees(r.OwnerActor, r.OwnerTeam), err
}

// Upstream are the contexts INQ reads references from.
type Upstream struct {
	Sources evdapi.Queries
	Parties ptyapi.Queries
	Skus    catapi.Queries
}

// Service runs INQ's use cases.
type Service struct {
	Ledger   *action.Ledger
	Clock    application.Clock
	Upstream Upstream
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

// facts asks PTY, CAT and EVD about the references, each in a transaction of its own. A caller
// without the EVD read permission gets PERMISSION_DENIED (Python _source_ref authorizes "read").
func (s Service) facts(ctx context.Context, customer, contact string, skus, sources []string) (rfq.Facts, error) {
	f := rfq.Facts{Parties: map[string]rfq.Party{}, Contacts: map[string]rfq.Contact{}, Skus: map[string]rfq.Sku{}, Sources: map[string]bool{}}
	var parties, contacts []string
	if customer != "" {
		parties = append(parties, customer)
	}
	if contact != "" {
		contacts = append(contacts, contact)
	}
	resolved, err := s.Upstream.Parties.ResolveParties(ctx, ptyapi.Resolve{PartyIDs: parties, ContactIDs: contacts})
	if err != nil {
		return f, err
	}
	for id, p := range resolved.Parties {
		f.Parties[id] = rfq.Party{Customer: slices.Contains(p.Roles, ptyapi.RoleCustomer), Confirmed: p.Status == ptyapi.StatusConfirmed, RowVersion: p.RowVersion}
	}
	for id, c := range resolved.Contacts {
		f.Contacts[id] = rfq.Contact{PartyID: c.PartyID, Confirmed: c.Status == ptyapi.StatusConfirmed, RowVersion: c.RowVersion}
	}
	refs, err := s.Upstream.Skus.ResolveSkus(ctx, skus)
	if err != nil {
		return f, err
	}
	for id, k := range refs {
		f.Skus[id] = rfq.Sku{Confirmed: k.Status == catapi.StatusConfirmed, SpecRevision: k.SpecRevision}
	}
	readable, err := s.Upstream.Sources.ResolveSources(ctx, sources)
	for id := range readable {
		f.Sources[id] = true
	}
	return f, err
}

// SaveRfq saves a whole draft RFQ (Python rfq.save_draft).
func (s Service) SaveRfq(ctx context.Context, cmd api.SaveRfq) (api.Receipt, error) {
	content := rfq.Content{CustomerID: cmd.CustomerID, ContactID: cmd.ContactID, SourceIDs: cmd.SourceIDs, Unresolved: cmd.UnresolvedFields}
	var skus, sources []string
	sources = append(sources, cmd.SourceIDs...)
	for _, l := range cmd.Lines {
		content.Lines = append(content.Lines, rfq.Line{ID: l.LineID, SkuID: l.SkuID, Quantity: l.Quantity, UOM: l.UOM,
			RequestedItem: l.RequestedItem, SourceID: l.SourceID, Locator: l.Locator})
		if l.SkuID != "" {
			skus = append(skus, l.SkuID)
		}
		if l.SourceID != "" {
			sources = append(sources, l.SourceID)
		}
	}
	args := cmd
	args.IdempotencyKey, args.ExpectedVersion = "", 0
	h := &rfqCommand{s: s, id: cmd.RfqID}
	h.prepare = func(ctx context.Context) (err error) {
		h.facts, err = s.facts(ctx, cmd.CustomerID, cmd.ContactID, skus, sources)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := rfq.CheckSave(h.rfq, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if err := rfq.CheckDraft(content, h.facts); err != nil {
			return action.Outcome{}, err
		}
		if h.rfq == nil {
			h.rfq = rfq.New(cmd.RfqID, d.Actor.ID, d.Team)
			h.rfq.ReplaceDraft(content)
			return h.outcome(""), postgres.Insert(ctx, exec, h.scope(ctx), h.rfq, s.Clock.Now())
		}
		h.rfq.ReplaceDraft(content)
		return h.save(ctx, exec, true, "")
	}
	return s.run(ctx, action.Command{Name: api.CommandSaveRfq, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.RfqID}, args, cmd.Validate(), &prepared{h})
}

// ConfirmRfqScope makes a complete draft ready (Python rfq.confirm_scope).
func (s Service) ConfirmRfqScope(ctx context.Context, cmd api.ConfirmRfqScope) (api.Receipt, error) {
	h := &rfqCommand{s: s, id: cmd.RfqID}
	var seen int64 // the row version the references were read for
	h.prepare = func(ctx context.Context) error {
		var stored *rfq.Rfq
		err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
			var err error
			stored, err = postgres.Load(ctx, exec, h.scope(ctx), cmd.RfqID, false)
			if stored != nil && !d.Sees(stored.OwnerActor, stored.OwnerTeam) {
				stored = nil
			}
			return err
		})
		if err != nil {
			return err
		}
		sources := slices.Clone(cmd.EvidenceIDs)
		var customer, contact string
		var skus []string
		if stored != nil {
			seen, customer, contact = stored.RowVersion, stored.CustomerID, stored.ContactID
			sources = append(sources, stored.Sources...)
			for _, l := range stored.Lines {
				if l.SkuID != "" {
					skus = append(skus, l.SkuID)
				}
			}
		}
		h.facts, err = s.facts(ctx, customer, contact, skus, sources)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := rfq.Check(h.rfq, h.visible(d), cmd.ExpectedVersion, rfq.Draft); err != nil {
			return action.Outcome{}, err
		}
		if h.rfq.RowVersion != seen {
			// The draft changed after its references were read; confirming would judge stale facts.
			return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
		}
		if err := h.rfq.Confirm(cmd.EvidenceIDs, h.facts, d.Actor, d.Epoch); err != nil {
			return action.Outcome{}, err
		}
		return h.save(ctx, exec, true, api.EventRfqScopeConfirmed)
	}
	args := cmd
	args.IdempotencyKey, args.ExpectedVersion = "", 0
	return s.run(ctx, action.Command{Name: api.CommandConfirmRfq, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.RfqID}, args, cmd.Validate(), &prepared{h})
}

// CancelRfq cancels a draft or ready RFQ (Python rfq.cancel).
func (s Service) CancelRfq(ctx context.Context, cmd api.CancelRfq) (api.Receipt, error) {
	return s.terminal(ctx, api.CommandCancelRfq, cmd.IdempotencyKey, cmd.RfqID, cmd.ExpectedVersion, cmd.Reason, cmd.Validate(),
		api.EventRfqCancelled, (*rfq.Rfq).Cancel, rfq.Draft, rfq.Ready)
}

// CloseRfq closes a ready RFQ (Python rfq.close).
func (s Service) CloseRfq(ctx context.Context, cmd api.CloseRfq) (api.Receipt, error) {
	return s.terminal(ctx, api.CommandCloseRfq, cmd.IdempotencyKey, cmd.RfqID, cmd.ExpectedVersion, cmd.Reason, cmd.Validate(),
		api.EventRfqClosed, (*rfq.Rfq).Close, rfq.Ready)
}

func (s Service) terminal(ctx context.Context, command, key, id string, expected int64, reason string, validation error, event string,
	move func(*rfq.Rfq), allowed ...rfq.Status) (api.Receipt, error) {
	h := &rfqCommand{s: s, id: id}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := rfq.Check(h.rfq, h.visible(d), expected, allowed...); err != nil {
			return action.Outcome{}, err
		}
		move(h.rfq)
		return h.save(ctx, exec, false, event)
	}
	return s.run(ctx, action.Command{Name: command, IdempotencyKey: key, ObjectID: id, Reason: reason},
		struct{ ID, Reason string }{id, reason}, validation, h)
}

// rfqCommand locks the RFQ root and applies one command.
type rfqCommand struct {
	s       Service
	id      string
	facts   rfq.Facts
	rfq     *rfq.Rfq
	from    int64
	prepare func(ctx context.Context) error
	apply   func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *rfqCommand) scope(ctx context.Context) kernel.Scope {
	if s, ok := bento.ScopeFromContext(ctx); ok {
		return s
	}
	return kernel.Scope{}
}

func (h *rfqCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.rfq, err = postgres.Load(ctx, exec, h.scope(ctx), h.id, true)
	if h.rfq != nil {
		h.from = h.rfq.RowVersion
	}
	return err
}

func (h *rfqCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

func (h *rfqCommand) visible(d authorization.Decision) bool {
	return h.rfq != nil && d.Sees(h.rfq.OwnerActor, h.rfq.OwnerTeam)
}

func (h *rfqCommand) save(ctx context.Context, exec bentopg.Executor, content bool, event string) (action.Outcome, error) {
	if err := postgres.Update(ctx, exec, h.scope(ctx), h.rfq, h.from, content, h.s.Clock.Now()); err != nil {
		return action.Outcome{}, err
	}
	return h.outcome(event), nil
}

func (h *rfqCommand) outcome(event string) action.Outcome {
	r := h.rfq
	out := action.Outcome{ObjectID: r.ID, RowVersion: r.RowVersion, Status: string(r.Status)}
	if event == "" {
		return out
	}
	fields := map[string]any{"rfq_id": r.ID, "row_version": r.RowVersion}
	if event == api.EventRfqScopeConfirmed {
		lines := make([]map[string]any, len(r.Lines))
		for i, l := range r.Lines {
			lines[i] = map[string]any{"line_id": l.ID, "sku_id": l.SkuID, "sku_spec_revision": l.SkuSpecRevision}
		}
		fields["customer_party_id"], fields["customer_row_version"] = r.CustomerID, r.Confirmation.CustomerVersion
		fields["contact_id"], fields["contact_row_version"] = r.ContactID, r.Confirmation.ContactVersion
		fields["lines"], fields["source_ids"] = lines, r.Sources
	}
	out.Events = []action.Event{{Type: event, AggregateType: "CustomerRfq", AggregateID: r.ID, AggregateVersion: r.RowVersion, Fields: fields}}
	return out
}

// prepared gives rfqCommand the Preparer step of the ledger.
type prepared struct{ *rfqCommand }

func (p *prepared) Prepare(ctx context.Context) error { return p.prepare(ctx) }

// GetRfq returns an RFQ the caller sees, audited (Python Core.get kind=rfq).
func (s Service) GetRfq(ctx context.Context, id string) (api.RfqView, error) {
	var v api.RfqView
	err := s.Ledger.Read(ctx, CommandGetRfq, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		scope, _ := bento.ScopeFromContext(ctx)
		r, err := postgres.Load(ctx, exec, scope, id, false)
		if err != nil {
			return err
		}
		if r == nil || !d.Sees(r.OwnerActor, r.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.RfqView{RfqID: r.ID, Status: string(r.Status), RowVersion: r.RowVersion, CustomerID: r.CustomerID, ContactID: r.ContactID,
			SourceIDs: nonNil(r.Sources), UnresolvedFields: nonNil(r.Unresolved), Lines: []api.RfqLineView{}}
		for _, l := range r.Lines {
			v.Lines = append(v.Lines, api.RfqLineView{SkuSpecRevision: l.SkuSpecRevision, RfqLine: api.RfqLine{LineID: l.ID, SkuID: l.SkuID,
				Quantity: l.Quantity, UOM: l.UOM, RequestedItem: l.RequestedItem, SourceID: l.SourceID, Locator: l.Locator}})
		}
		if c := r.Confirmation; c != nil {
			v.Confirmation = &api.Confirmation{CustomerRowVersion: c.CustomerVersion, ContactRowVersion: c.ContactVersion,
				EvidenceIDs: c.Evidence, ConfirmedBy: c.By, AuthorityEpoch: c.Epoch}
		}
		return nil
	})
	return v, err
}

// Audit returns INQ's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, CommandAudit)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
