// Package application runs TRD's QuoteRoot commands, queries and reconciliation on the command
// ledger. Quote rules live in domain/quote; SQL lives in adapters/postgres.
//
// TRD's upstream contexts for a quote are CST (Context Map R11, CostSnapshotRef v1 with
// AdoptabilityGuard v1), INQ (R12, RfqRef v1 with RfqGuard v1), PTY (R13, PartyRef,
// TradingProfileRef and AddressRoleRef v1), CAT (R14, SkuRef v1), POL (R15, ConfigRef v1 with
// ConfigGuard v1) and EVD (R16, SourceRef v1). A command reads them through their api packages
// before TRD's transaction begins (the ledger's Preparer step), and the domain judges what they
// said. quote.freeze then re-checks, inside TRD's transaction and after the quote root is locked
// FOR UPDATE and the authority guard row FOR SHARE, the upstream facts through their guard functions
// in context code order (ADR-BR-004 option B, ADR-BR-011; freeze.go).
//
// Every write on a root first passes its object control (TRD-INV-015, control.go): visibility, then
// the control fence, then the root version and the command's own rules, in that order (Python A24:
// a caller who does not see the root learns nothing of its control).
package application

import (
	"context"
	"errors"
	"slices"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Read commands and permissions.
const (
	CommandGetQuote          = "quote.get"
	CommandCheckApproval     = "quote.approval.check"
	CommandGetFormalDocument = "quote.document.get"
	CommandGetActiveRevision = "quote.active.get"
	CommandAudit             = "trd.audit"
	PermissionRead           = "trd.read"
	PermissionAudit          = "trd.audit"
)

var (
	anyActor      = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}
	humanOrAgent  = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}
	humanOrSystem = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorSystem}
)

// Rules is TRD's authorization policy for QuoteRoot, customer responses and acceptance. The
// tactical model's humanOnlyCommands name quote.freeze, quote.submit_review, quote.review.decide,
// quote.approval.revoke, quote.activate, quote.replace, quote.withdraw, quote.cancel,
// quote.response.verify, acceptance.invalidate, acceptance.mark_disputed, object_control.stop and
// object_control.release; each needs the permission of its own name. The command catalog gives
// object_control.bind to the system (TSK claims a root for a task) and the role templates give it to
// the sales clerk, so a human or the system binds, never an AI agent (it would fence humans out). An
// AI agent prepares, revises and discards drafts, edits the internal metadata and records customer
// responses (quote.response.record: 人工或 AI; its classification is a candidate a human verifies)
// through the same command path.
func Rules() []authorization.Rule {
	return []authorization.Rule{
		{Command: api.CommandSaveDraft, Permission: api.CommandSaveDraft, ActorKinds: humanOrAgent},
		{Command: api.CommandRevise, Permission: api.CommandRevise, ActorKinds: humanOrAgent},
		{Command: api.CommandDiscardDraft, Permission: api.CommandDiscardDraft, ActorKinds: humanOrAgent},
		authorization.HumanOnly(api.CommandFreeze, api.CommandFreeze),
		{Command: api.CommandUpdateMetadata, Permission: api.CommandUpdateMetadata, ActorKinds: humanOrAgent},
		{Command: api.CommandBindControl, Permission: api.CommandBindControl, ActorKinds: humanOrSystem},
		authorization.HumanOnly(api.CommandStopControl, api.CommandStopControl),
		authorization.HumanOnly(api.CommandReleaseControl, api.CommandReleaseControl),
		authorization.HumanOnly(api.CommandSubmitReview, api.CommandSubmitReview),
		authorization.HumanOnly(api.CommandDecideReview, api.CommandDecideReview),
		authorization.HumanOnly(api.CommandRevokeApproval, api.CommandRevokeApproval),
		authorization.HumanOnly(api.CommandActivate, api.CommandActivate),
		authorization.HumanOnly(api.CommandReplace, api.CommandReplace),
		authorization.HumanOnly(api.CommandWithdraw, api.CommandWithdraw),
		authorization.HumanOnly(api.CommandCancel, api.CommandCancel),
		{Command: CommandGetQuote, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandCheckApproval, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandGetFormalDocument, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandGetActiveRevision, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandAudit, Permission: PermissionAudit, ActorKinds: anyActor},
		{Command: api.CommandRecordResponse, Permission: api.CommandRecordResponse, ActorKinds: humanOrAgent},
		authorization.HumanOnly(api.CommandVerifyResponse, api.CommandVerifyResponse),
		authorization.HumanOnly(api.CommandInvalidateAcceptance, api.CommandInvalidateAcceptance),
		authorization.HumanOnly(api.CommandDisputeAcceptance, api.CommandDisputeAcceptance),
		{Command: CommandGetResponse, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandGetAcceptance, Permission: PermissionRead, ActorKinds: anyActor},
	}
}

// Upstream are the contexts TRD reads references from.
type Upstream struct {
	Sources   evdapi.Queries         // SourceRef v1 (R16)
	Parties   ptyapi.Queries         // PartyRef v1 (R13)
	Profiles  ptyapi.ProfileQueries  // TradingProfileRef v1, AddressRoleRef v1 (R13)
	Skus      catapi.Queries         // SkuRef v1 (R14)
	Configs   polapi.Queries         // ConfigRef v1 (R15)
	Numbers   Numbers                // NumberIssue v1 (R15)
	Rfqs      inqapi.RfqLookups      // RfqRef v1 (R12)
	Snapshots cstapi.SnapshotLookups // CostSnapshotRef v1 (R11)
	Contexts  cstapi.Lookups         // the costing context a snapshot froze (R11)
}

// Numbers is POL's NumberIssue v1 (Context Map R15): number.issue, which POL runs as the actor in
// the context, in its own transaction (polapi.Commands satisfies it).
type Numbers interface {
	IssueNumber(ctx context.Context, cmd polapi.IssueNumber) (polapi.Receipt, error)
}

// Service runs TRD's use cases.
type Service struct {
	Ledger   *action.Ledger
	Clock    application.Clock
	Upstream Upstream
	// Calculator is the server calculation of WBS S3-02 (ServerCalculator); without one TRD
	// freezes nothing.
	Calculator Calculator
}

func scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

type writing struct{}

// run executes cmd on the ledger. The context it runs in is marked as a write, so a replay of a
// stored result passes the object control fence (Visible) as the write itself would.
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
	res, err := s.Ledger.Execute(context.WithValue(ctx, writing{}, true), cmd, h)
	return api.Receipt(res), err
}

// request returns the name, the idempotency key and the business arguments of cmd, which its
// request hash covers: everything but the key and the expected root version, control epoch or
// effectivity version, so a replay with another CAS value still matches. Execution and reconciliation hash through it.
func request(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.SaveDraft:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandSaveDraft, key, c
	case api.Revise:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandRevise, key, c
	case api.DiscardDraft:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandDiscardDraft, key, c
	case api.Freeze:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandFreeze, key, c
	case api.UpdateMetadata:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandUpdateMetadata, key, c
	case api.BindControl:
		key, c.IdempotencyKey, c.ExpectedControlEpoch = c.IdempotencyKey, "", 0
		return api.CommandBindControl, key, c
	case api.StopControl:
		key, c.IdempotencyKey, c.ExpectedControlEpoch = c.IdempotencyKey, "", 0
		return api.CommandStopControl, key, c
	case api.ReleaseControl:
		key, c.IdempotencyKey, c.ExpectedControlEpoch = c.IdempotencyKey, "", 0
		return api.CommandReleaseControl, key, c
	case api.SubmitReview:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandSubmitReview, key, c
	case api.DecideReview:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandDecideReview, key, c
	case api.RevokeApproval:
		key, c.IdempotencyKey, c.ExpectedEffectivityVersion = c.IdempotencyKey, "", 0
		return api.CommandRevokeApproval, key, c
	case api.Activate:
		key, c.IdempotencyKey, c.ExpectedVersion, c.ExpectedApprovalEffectivityVersion = c.IdempotencyKey, "", 0, 0
		return api.CommandActivate, key, c
	case api.Replace:
		key, c.IdempotencyKey, c.ExpectedVersion, c.ExpectedApprovalEffectivityVersion = c.IdempotencyKey, "", 0, 0
		return api.CommandReplace, key, c
	case api.Withdraw:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandWithdraw, key, c
	case api.Cancel:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandCancel, key, c
	case api.RecordResponse:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandRecordResponse, key, c
	case api.VerifyResponse:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandVerifyResponse, key, c
	case api.InvalidateAcceptance:
		key, c.IdempotencyKey, c.ExpectedEffectivityVersion = c.IdempotencyKey, "", 0
		return api.CommandInvalidateAcceptance, key, c
	case api.DisputeAcceptance:
		key, c.IdempotencyKey, c.ExpectedEffectivityVersion = c.IdempotencyKey, "", 0
		return api.CommandDisputeAcceptance, key, c
	}
	return "", "", nil
}

var quoteWrites = []string{api.CommandSaveDraft, api.CommandRevise, api.CommandDiscardDraft, api.CommandFreeze, api.CommandUpdateMetadata,
	api.CommandSubmitReview, api.CommandDecideReview, api.CommandRevokeApproval, api.CommandActivate, api.CommandReplace,
	api.CommandWithdraw, api.CommandCancel}

// Visible reports whether d's actor sees the quote root a stored result names: the quote itself,
// or the quote a stored response (quote.response.record) or acceptance (quote.response.verify,
// acceptance.invalidate, acceptance.mark_disputed) belongs to. On the write path (a replay) of a
// quote write or one of those it also passes the root's object control as it is now (Python
// test_committed_public_replay_and_local_replay_cannot_bypass_new_binding): a stored success is
// never handed to a caller the binding now fences out. Reconciliation is not fenced.
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, id string) (bool, error) {
	quoteID := id
	switch command {
	case api.CommandRecordResponse:
		r, err := postgres.LoadResponse(ctx, exec, scope(ctx), id)
		if err != nil || r == nil {
			return false, err
		}
		quoteID = r.QuoteID
	case api.CommandVerifyResponse, api.CommandInvalidateAcceptance, api.CommandDisputeAcceptance:
		a, err := postgres.LoadAcceptance(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil || a == nil {
			return false, err
		}
		quoteID = a.QuoteID
	}
	q, err := postgres.LoadRoot(ctx, exec, scope(ctx), quoteID)
	if err != nil || q == nil || !d.Sees(q.OwnerActor, q.OwnerTeam) {
		return false, err
	}
	if ctx.Value(writing{}) != nil && (slices.Contains(quoteWrites, command) || slices.Contains(acceptanceWrites, command)) {
		if err = q.Control.Admit(claim(ctx), d.Epoch); err != nil {
			return false, err
		}
	}
	return true, nil
}

// claim returns the control claim the trusted adapter put into ctx, or nil.
func claim(ctx context.Context) *quote.Claim {
	c, ok := api.ControlFromContext(ctx)
	if !ok {
		return nil
	}
	return &quote.Claim{TaskID: c.TaskID, Epoch: c.ControlEpoch}
}

// Reconcile reports what became of cmd, sent earlier with its idempotency key, without executing
// it (api.Reconciler; contract 06, 动作对账).
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

// quoteCommand locks the quote root and applies one command.
type quoteCommand struct {
	s         Service
	id        string
	q         *quote.Quote
	from      int64 // root version as locked
	fromEpoch int64 // control epoch as locked
	prepare   func(ctx context.Context) error
	apply     func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *quoteCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.q, h.from, h.fromEpoch = nil, 0, 0
	h.q, err = postgres.LoadQuote(ctx, exec, scope(ctx), h.id, postgres.ForUpdate)
	if h.q != nil {
		h.from, h.fromEpoch = h.q.RootVersion, h.q.Control.Epoch
	}
	return err
}

func (h *quoteCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

func (h *quoteCommand) visible(d authorization.Decision) bool {
	return h.q != nil && d.Sees(h.q.OwnerActor, h.q.OwnerTeam)
}

// admit runs the checks every quote write starts with: the root exists and the caller sees it
// (NOT_FOUND; a new root only where create allows it), then the object control fence.
func (h *quoteCommand) admit(ctx context.Context, d authorization.Decision, create bool) error {
	if h.q == nil && create {
		return quote.Control{State: quote.Unbound}.Admit(claim(ctx), d.Epoch)
	}
	if !h.visible(d) {
		return kernel.Reject(kernel.CodeNotFound, "/args/quote_id", "")
	}
	return h.q.Control.Admit(claim(ctx), d.Epoch)
}

func (h *quoteCommand) save(ctx context.Context, exec bentopg.Executor, created bool, status string, events ...action.Event) (action.Outcome, error) {
	if err := postgres.SaveQuote(ctx, exec, scope(ctx), h.q, created, h.from, h.fromEpoch, h.s.Clock.Now()); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: h.q.ID, RowVersion: h.q.RootVersion, Status: status, Events: events}, nil
}

// preparedQuote gives quoteCommand the Preparer step of the ledger.
type preparedQuote struct{ *quoteCommand }

func (p *preparedQuote) Prepare(ctx context.Context) error { return p.prepare(ctx) }

// sources asks EVD which of ids the caller reads, with their content hashes.
func (s Service) sources(ctx context.Context, ids []string) (map[string]string, error) {
	readable := map[string]string{}
	if len(ids) == 0 {
		return readable, nil
	}
	refs, err := s.Upstream.Sources.ResolveSources(ctx, ids)
	for id, r := range refs {
		readable[id] = r.ContentSHA256
	}
	return readable, err
}

func readableSet(sources map[string]string) map[string]bool {
	out := map[string]bool{}
	for id := range sources {
		out[id] = true
	}
	return out
}

// SaveDraft saves the whole working revision of a quote (Python quote.save_draft). The sources the
// draft cites must be ones the caller reads; the references' effect is judged at the freeze.
func (s Service) SaveDraft(ctx context.Context, cmd api.SaveDraft) (api.Receipt, error) {
	data := quoteData(cmd.Data)
	h := &quoteCommand{s: s, id: cmd.QuoteID}
	var readable map[string]bool
	h.prepare = func(ctx context.Context) error {
		found, err := s.sources(ctx, data.Sources())
		readable = readableSet(found)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, cmd.ExpectedVersion == 0); err != nil {
			return action.Outcome{}, err
		}
		if err := quote.CheckSave(h.q, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if err := quote.CheckDraft(data, readable); err != nil {
			return action.Outcome{}, err
		}
		created := h.q == nil
		if created {
			h.q = quote.New(cmd.QuoteID, d.Actor.ID, d.Team)
		}
		h.q.Save(data, d.Actor, d.Epoch, s.Clock.Now())
		return h.save(ctx, exec, created, api.StatusWorking)
	}
	return s.run(ctx, action.Command{Name: api.CommandSaveDraft, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.QuoteID},
		cmd, cmd.Validate(), &preparedQuote{h})
}

// Revise copies a frozen revision into a new working revision (Python quote.revise).
func (s Service) Revise(ctx context.Context, cmd api.Revise) (api.Receipt, error) {
	h := &quoteCommand{s: s, id: cmd.QuoteID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, false); err != nil {
			return action.Outcome{}, err
		}
		if err := quote.Check(h.q, true, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if _, err := h.q.Revise(cmd.BaseRevision, d.Actor, d.Epoch, s.Clock.Now()); err != nil {
			return action.Outcome{}, err
		}
		return h.save(ctx, exec, false, api.StatusWorking)
	}
	return s.run(ctx, action.Command{Name: api.CommandRevise, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.QuoteID},
		cmd, cmd.Validate(), h)
}

// DiscardDraft ends the working revision (Python quote.discard_draft); the discarder reads the
// sources of its evidence.
func (s Service) DiscardDraft(ctx context.Context, cmd api.DiscardDraft) (api.Receipt, error) {
	h := &quoteCommand{s: s, id: cmd.QuoteID}
	proofs := evidence(cmd.Evidence)
	var readable map[string]bool
	h.prepare = func(ctx context.Context) error {
		ids := make([]string, len(proofs))
		for i, e := range proofs {
			ids[i] = e.SourceID
		}
		found, err := s.sources(ctx, ids)
		readable = readableSet(found)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, false); err != nil {
			return action.Outcome{}, err
		}
		if err := quote.Check(h.q, true, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if _, err := h.q.Discard(cmd.WorkingRevision, d.Actor, d.Epoch, s.Clock.Now(), cmd.Reason, proofs); err != nil {
			return action.Outcome{}, err
		}
		if err := quote.CheckDraft(quote.Data{Evidence: proofs}, readable); err != nil {
			return action.Outcome{}, moved(err, "/args/data/evidence", "/args/evidence")
		}
		return h.save(ctx, exec, false, api.StatusDiscarded)
	}
	return s.run(ctx, action.Command{Name: api.CommandDiscardDraft, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.QuoteID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), &preparedQuote{h})
}

// UpdateMetadata replaces the root's internal labels and note (BC-AT09): the root version moves,
// no revision and no business hash changes (TRD-INV-002).
func (s Service) UpdateMetadata(ctx context.Context, cmd api.UpdateMetadata) (api.Receipt, error) {
	h := &quoteCommand{s: s, id: cmd.QuoteID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, false); err != nil {
			return action.Outcome{}, err
		}
		if err := quote.Check(h.q, true, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		h.q.UpdateMetadata(cmd.Labels, cmd.InternalNote)
		return h.save(ctx, exec, false, api.StatusUpdated)
	}
	return s.run(ctx, action.Command{Name: api.CommandUpdateMetadata, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.QuoteID},
		cmd, cmd.Validate(), h)
}

// GetQuote returns a quote the caller sees, audited; a frozen revision whose commercial document
// does not hash to its business hash is refused (BUSINESS_HASH_MISMATCH).
func (s Service) GetQuote(ctx context.Context, id string) (api.QuoteView, error) {
	var v api.QuoteView
	err := s.Ledger.Read(ctx, CommandGetQuote, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		q, err := postgres.LoadQuote(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if q == nil || !d.Sees(q.OwnerActor, q.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v, err = quoteView(q)
		return err
	})
	return v, err
}

// Audit returns TRD's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, CommandAudit)
}

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}

// moved returns err with its first field path from replaced by to.
func moved(err error, from, to string) error {
	rej, ok := kernel.AsRejection(err)
	if !ok || len(rej.Fields) == 0 {
		return err
	}
	out := *rej
	out.Fields = slices.Clone(rej.Fields)
	if p := out.Fields[0].Path; len(p) >= len(from) && p[:len(from)] == from {
		out.Fields[0].Path = to + p[len(from):]
	}
	return &out
}
