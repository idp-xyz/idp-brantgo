// Package application runs INQ's commands and queries on the command ledger. RFQ rules live in
// domain/rfq; SQL lives in adapters/postgres. Parties, contacts, SKUs and sources belong to PTY,
// CAT and EVD: INQ reads them through their api packages before its own transaction begins
// (Context Map R01–R03 carry no guard lock), and the domain judges what they said.
package application

import (
	"context"
	"errors"
	"slices"
	"strings"

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
		content.Lines = append(content.Lines, domainLine(l))
		if l.SkuID != "" {
			skus = append(skus, l.SkuID)
		}
		if l.SourceID != "" {
			sources = append(sources, l.SourceID)
		}
	}
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
			h.rfq.ReplaceDraft(content, d.Actor)
			return h.outcome(""), postgres.Insert(ctx, exec, h.scope(ctx), h.rfq, s.Clock.Now())
		}
		corrections := h.rfq.ReplaceDraft(content, d.Actor)
		out, err := h.save(ctx, exec, true, "")
		if err != nil {
			return out, err
		}
		return out, postgres.InsertCorrections(ctx, exec, h.scope(ctx), h.rfq.ID, corrections, s.Clock.Now())
	}
	return s.run(ctx, action.Command{Name: api.CommandSaveRfq, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.RfqID}, cmd, cmd.Validate(), &prepared{h})
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
		for _, v := range cmd.VerifiedLines {
			for _, f := range v.Fields {
				if f.Field == api.FieldSkuID {
					skus = append(skus, f.Value)
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
		var verified []rfq.Verification
		for _, v := range cmd.VerifiedLines {
			lv := rfq.Verification{LineID: v.LineID}
			for _, f := range v.Fields {
				lv.Fields = append(lv.Fields, rfq.VerifiedField(f))
			}
			verified = append(verified, lv)
		}
		corrections, err := h.rfq.Confirm(cmd.EvidenceIDs, verified, h.facts, d.Actor, d.Epoch)
		if err != nil {
			return action.Outcome{}, err
		}
		out, err := h.save(ctx, exec, true, api.EventRfqScopeConfirmed)
		if err != nil {
			return out, err
		}
		return out, postgres.InsertCorrections(ctx, exec, h.scope(ctx), h.rfq.ID, corrections, s.Clock.Now())
	}
	return s.run(ctx, action.Command{Name: api.CommandConfirmRfq, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.RfqID}, cmd, cmd.Validate(), &prepared{h})
}

// CancelRfq cancels a draft or ready RFQ (Python rfq.cancel).
func (s Service) CancelRfq(ctx context.Context, cmd api.CancelRfq) (api.Receipt, error) {
	return s.terminal(ctx, cmd, api.CommandCancelRfq, cmd.IdempotencyKey, cmd.RfqID, cmd.ExpectedVersion, cmd.Reason, cmd.Validate(),
		api.EventRfqCancelled, (*rfq.Rfq).Cancel, rfq.Draft, rfq.Ready)
}

// CloseRfq closes a ready RFQ (Python rfq.close).
func (s Service) CloseRfq(ctx context.Context, cmd api.CloseRfq) (api.Receipt, error) {
	return s.terminal(ctx, cmd, api.CommandCloseRfq, cmd.IdempotencyKey, cmd.RfqID, cmd.ExpectedVersion, cmd.Reason, cmd.Validate(),
		api.EventRfqClosed, (*rfq.Rfq).Close, rfq.Ready)
}

func (s Service) terminal(ctx context.Context, req api.Command, command, key, id string, expected int64, reason string, validation error, event string,
	move func(*rfq.Rfq), allowed ...rfq.Status) (api.Receipt, error) {
	h := &rfqCommand{s: s, id: id}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := rfq.Check(h.rfq, h.visible(d), expected, allowed...); err != nil {
			return action.Outcome{}, err
		}
		move(h.rfq)
		return h.save(ctx, exec, false, event)
	}
	return s.run(ctx, action.Command{Name: command, IdempotencyKey: key, ObjectID: id, Reason: reason}, req, validation, h)
}

// request returns the name, the idempotency key and the business arguments of cmd, which its
// request hash covers: everything but the key and the expected version, so a replay with another
// CAS value still matches (Python test_replay_ignores_changed_cas_but_not_changed_business_fields).
// Execution and reconciliation hash through it alike.
func request(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.SaveRfq:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandSaveRfq, key, c
	case api.ConfirmRfqScope:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandConfirmRfq, key, c
	case api.CancelRfq:
		return api.CommandCancelRfq, c.IdempotencyKey, struct{ ID, Reason string }{c.RfqID, c.Reason}
	case api.CloseRfq:
		return api.CommandCloseRfq, c.IdempotencyKey, struct{ ID, Reason string }{c.RfqID, c.Reason}
	}
	return "", "", nil
}

// Reconcile reports what became of cmd, sent earlier with its idempotency key, without executing
// it (api.Reconciler; contract 06, 动作对账).
func (s Service) Reconcile(ctx context.Context, cmd api.Command) (api.Reconciliation, error) {
	r, err := s.RequestOf(cmd)
	if err != nil {
		return api.Reconciliation{}, err
	}
	return s.ReconcileHash(ctx, r.Command, r.IdempotencyKey, r.RequestHash)
}

// RequestOf returns the ledger identity of cmd, hashed as executing it hashes it
// (api.ActionReconciler; ActionReconciliation v1, Context Map R67).
func (s Service) RequestOf(cmd api.Command) (kernel.ActionRequest, error) {
	name, key, args := request(cmd)
	if name == "" {
		return kernel.ActionRequest{}, kernel.Reject(kernel.CodeSchemaInvalid, "/command", "")
	}
	hash, err := action.RequestHash(args)
	if err != nil {
		return kernel.ActionRequest{}, err
	}
	return kernel.ActionRequest{Command: name, IdempotencyKey: key, RequestHash: hash}, nil
}

// ReconcileHash reports what became of the command the ledger identity names, without executing
// anything (api.ActionReconciler; ActionReconciliation v1, Context Map R67).
func (s Service) ReconcileHash(ctx context.Context, command, idempotencyKey, requestHash string) (api.Reconciliation, error) {
	r, err := s.Ledger.Reconcile(ctx, command, idempotencyKey, requestHash)
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

// domainLine is the domain's form of an api line.
func domainLine(l api.RfqLine) rfq.Line {
	out := rfq.Line{ID: l.LineID, SkuID: l.SkuID, Quantity: l.Quantity, UOM: l.UOM, RequestedItem: l.RequestedItem, SourceID: l.SourceID,
		Locator: l.Locator}
	for _, x := range l.Locators {
		out.Locators = append(out.Locators, rfq.FieldLocator(x))
	}
	for _, x := range l.Pending {
		out.Pending = append(out.Pending, rfq.PendingField{Field: x.Field, Reason: x.Reason, Candidates: slices.Clone(x.Candidates)})
	}
	return out
}

// GetRfq returns an RFQ the caller sees, audited (Python Core.get kind=rfq), with its lines'
// locators, pending fields and provenance and every correction. The quotes of a line are the
// source's own words: after the read, EVD is asked which of the lines' sources the caller may read
// now, and the quotes of the others are withheld (CT16; a caller without evd.read reads none).
func (s Service) GetRfq(ctx context.Context, id string) (api.RfqView, error) {
	var v api.RfqView
	var quoted []string
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
			SourceIDs: nonNil(r.Sources), UnresolvedFields: nonNil(r.Unresolved), Lines: []api.RfqLineView{}, Corrections: []api.Correction{}}
		for _, l := range r.Lines {
			line := api.RfqLineView{SkuSpecRevision: l.SkuSpecRevision, ProposedBy: l.ProposedBy, RfqLine: api.RfqLine{LineID: l.ID,
				SkuID: l.SkuID, Quantity: l.Quantity, UOM: l.UOM, RequestedItem: l.RequestedItem, SourceID: l.SourceID, Locator: l.Locator}}
			for _, x := range l.Locators {
				line.Locators = append(line.Locators, api.FieldLocator(x))
			}
			for _, x := range l.Pending {
				line.Pending = append(line.Pending, api.PendingField{Field: x.Field, Reason: x.Reason, Candidates: x.Candidates})
			}
			if len(l.Locators) > 0 {
				quoted = append(quoted, l.SourceID)
			}
			v.Lines = append(v.Lines, line)
		}
		if c := r.Confirmation; c != nil {
			v.Confirmation = &api.Confirmation{CustomerRowVersion: c.CustomerVersion, ContactRowVersion: c.ContactVersion,
				EvidenceIDs: c.Evidence, ConfirmedBy: c.By, AuthorityEpoch: c.Epoch}
		}
		corrections, err := postgres.Corrections(ctx, exec, scope, id)
		for _, x := range corrections {
			v.Corrections = append(v.Corrections, api.Correction{Seq: x.Seq, RowVersion: x.RowVersion, Command: x.Command, LineID: x.LineID,
				Field: x.Field, Kind: x.Kind, From: x.From, To: x.To, Locator: x.Locator, Reason: x.Reason, Candidates: x.Candidates,
				ProposedBy: x.ProposedBy, CorrectedBy: x.CorrectedBy, RecordedAt: x.RecordedAt})
		}
		return err
	})
	if err != nil || len(quoted) == 0 {
		return v, err
	}
	readable, err := s.Upstream.Sources.ResolveSources(ctx, quoted)
	if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodePermissionDenied {
		readable, err = nil, nil
	}
	if err != nil {
		return api.RfqView{}, err
	}
	for i, l := range v.Lines {
		if _, ok := readable[l.SourceID]; len(l.Locators) > 0 && !ok {
			v.Lines[i].QuotesWithheld = true
			v.Lines[i].Locators = slices.Clone(l.Locators)
			for j := range v.Lines[i].Locators {
				v.Lines[i].Locators[j].Quote = ""
			}
		}
	}
	return v, nil
}

// ResolveRfqs answers RfqRef v1 lookups of downstream contexts (api.RfqLookups): each RFQ the
// caller sees, at its current row version, with its lines in line-ID order and their hash. It needs
// no permission beyond an active grant and is not audited; the downstream command records its own
// outcome.
func (s Service) ResolveRfqs(ctx context.Context, ids []string) (map[string]api.RfqRef, error) {
	out := map[string]api.RfqRef{}
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(ids) > api.MaxLookup {
		return out, kernel.Reject(kernel.CodeSchemaInvalid, "/ids", "")
	}
	for _, id := range ids {
		if !kernel.ValidIdentifier(id) {
			return out, kernel.Reject(kernel.CodeSchemaInvalid, "/ids", "malformed RFQ ID")
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		scope, _ := bento.ScopeFromContext(ctx)
		for _, id := range ids {
			r, err := postgres.Load(ctx, exec, scope, id, false)
			if err != nil {
				return err
			}
			if r == nil || !d.Sees(r.OwnerActor, r.OwnerTeam) {
				continue
			}
			ref := api.RfqRef{RfqID: r.ID, RowVersion: r.RowVersion, Status: string(r.Status), CustomerID: r.CustomerID, ContactID: r.ContactID,
				Lines: []api.RfqRefLine{}}
			for _, l := range r.Lines {
				ref.Lines = append(ref.Lines, api.RfqRefLine{LineID: l.ID, SkuID: l.SkuID, SkuSpecRevision: l.SkuSpecRevision,
					Quantity: l.Quantity, UOM: l.UOM})
			}
			slices.SortFunc(ref.Lines, func(a, b api.RfqRefLine) int { return strings.Compare(a.LineID, b.LineID) })
			ref.LinesHash = api.RfqLinesHash(ref.Lines)
			out[id] = ref
		}
		return nil
	})
	return out, err
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

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}
