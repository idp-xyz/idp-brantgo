// Package application runs CST's commands, queries and lookups on the command ledger. Costing
// context rules live in domain/costing, measurement rules in domain/measurement (on the
// calculation rules of domain/rules); SQL lives in adapters/postgres.
//
// CST's upstream contexts are INQ (Context Map R08, RfqRef v1 with a guard lock), CAT (R51, SkuRef
// v1), POL (R10, ConfigRef v1 with ConfigGuard) and EVD (R64, SourceRef v1: the actor reads every
// source cited evidence names, evidence.go). A command reads them through their api packages
// before CST's transaction begins (the ledger's Preparer step), and the domain judges what they
// said. costing_context.verify then re-checks, inside CST's transaction and after the
// context root is locked FOR UPDATE, the RFQ through RfqGuard v1 and every POL configuration
// through ConfigGuard v1, in context code order (ADR-BR-004 option B, ADR-BR-011): a cancellation,
// closure, supersession or retirement either committed first and the verification is refused, or
// waits until it commits.
package application

import (
	"context"
	"errors"
	"slices"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/costing"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/measurement"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Read commands and permissions.
const (
	CommandGetContext     = "costing_context.get"
	CommandGetMeasurement = "measurement.get"
	CommandAudit          = "cst.audit"
	PermissionRead        = "cst.read"
	PermissionAudit       = "cst.audit"
)

var (
	anyActor     = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}
	humanOrAgent = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}
)

// Rules is CST's authorization policy. The tactical model's humanOnlyCommands name
// costing_context.verify and measurement.record_verified; withdrawing a context, and superseding
// or withdrawing a measurement, are human only as well (Python HUMAN_ACTIONS: withdraw, supersede;
// the sibling supplier_offer.withdraw and logistics_offer.withdraw are human only). An AI agent
// prepares, revises and discards drafts through the same command path.
func Rules() []authorization.Rule {
	return append([]authorization.Rule{
		{Command: api.CommandSaveContext, Permission: api.CommandSaveContext, ActorKinds: humanOrAgent},
		{Command: api.CommandReviseContext, Permission: api.CommandReviseContext, ActorKinds: humanOrAgent},
		{Command: api.CommandDiscardContextDraft, Permission: api.CommandDiscardContextDraft, ActorKinds: humanOrAgent},
		authorization.HumanOnly(api.CommandVerifyContext, api.CommandVerifyContext),
		authorization.HumanOnly(api.CommandWithdrawContext, api.CommandWithdrawContext),
		authorization.HumanOnly(api.CommandRecordMeasurement, api.CommandRecordMeasurement),
		authorization.HumanOnly(api.CommandSupersedeMeasurement, api.CommandSupersedeMeasurement),
		authorization.HumanOnly(api.CommandWithdrawMeasurement, api.CommandWithdrawMeasurement),
		{Command: CommandGetContext, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandGetMeasurement, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandAudit, Permission: PermissionAudit, ActorKinds: anyActor},
	}, scenarioRules()...)
}

// Visible reports whether d's actor sees the costing context or measurement a stored result names.
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, id string) (bool, error) {
	sc := scope(ctx)
	if scenarioCommand(command) {
		return visibleScenario(ctx, exec, d, command, id)
	}
	if measurementCommand(command) {
		s, err := postgres.LoadMeasurement(ctx, exec, sc, id, postgres.NoLock)
		return err == nil && s != nil && d.Sees(s.OwnerActor, s.OwnerTeam), err
	}
	cc, err := postgres.LoadContext(ctx, exec, sc, id, postgres.NoLock)
	return err == nil && cc != nil && d.Sees(cc.OwnerActor, cc.OwnerTeam), err
}

func measurementCommand(command string) bool {
	return slices.Contains([]string{api.CommandRecordMeasurement, api.CommandSupersedeMeasurement, api.CommandWithdrawMeasurement}, command)
}

// Upstream are the contexts CST reads references from.
type Upstream struct {
	Sources evdapi.Queries    // SourceRef v1 (R64)
	Rfqs    inqapi.RfqLookups // RfqRef v1 (R08)
	Skus    catapi.Queries    // SkuRef v1 (R51)
	Configs polapi.Queries    // ConfigRef v1 (R10)
	Supply  supapi.Queries    // SupplyConditionRef v1 (R09)
}

// Service runs CST's use cases.
type Service struct {
	Ledger   *action.Ledger
	Clock    application.Clock
	Upstream Upstream
}

func scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
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

// request returns the name, the idempotency key and the business arguments of cmd, which its
// request hash covers: everything but the key and the expected version, so a replay with another
// CAS value still matches (Python test_replay_ignores_changed_cas_but_not_changed_business_fields).
// Execution and reconciliation hash through it alike.
func request(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.SaveContext:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandSaveContext, key, c
	case api.ReviseContext:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandReviseContext, key, c
	case api.DiscardContextDraft:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandDiscardContextDraft, key, c
	case api.VerifyContext:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandVerifyContext, key, c
	case api.WithdrawContext:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandWithdrawContext, key, c
	case api.RecordMeasurement:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandRecordMeasurement, key, c
	case api.SupersedeMeasurement:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandSupersedeMeasurement, key, c
	case api.WithdrawMeasurement:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandWithdrawMeasurement, key, c
	}
	return scenarioRequest(cmd)
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

// contextCommand locks the costing context root and applies one command.
type contextCommand struct {
	s       Service
	id      string
	cc      *costing.Context
	from    int64
	prepare func(ctx context.Context) error
	apply   func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *contextCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.cc, h.from = nil, 0
	h.cc, err = postgres.LoadContext(ctx, exec, scope(ctx), h.id, postgres.ForUpdate)
	if h.cc != nil {
		h.from = h.cc.RootVersion
	}
	return err
}

func (h *contextCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

func (h *contextCommand) visible(d authorization.Decision) bool {
	return h.cc != nil && d.Sees(h.cc.OwnerActor, h.cc.OwnerTeam)
}

func (h *contextCommand) save(ctx context.Context, exec bentopg.Executor, created bool, r *costing.Revision, events ...action.Event) (action.Outcome, error) {
	if err := postgres.SaveContext(ctx, exec, scope(ctx), h.cc, created, h.from, h.s.Clock.Now()); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: h.cc.ID, RowVersion: h.cc.RootVersion, Status: string(r.Status), Events: events}, nil
}

// preparedContext gives contextCommand the Preparer step of the ledger.
type preparedContext struct{ *contextCommand }

func (p *preparedContext) Prepare(ctx context.Context) error { return p.prepare(ctx) }

// SaveContext saves the whole working revision of a costing context (Python
// costing_context.save_draft). The references the draft states must exist and be readable, the
// sources its assumptions cite included; their effect is judged at verification.
func (s Service) SaveContext(ctx context.Context, cmd api.SaveContext) (api.Receipt, error) {
	data := contextData(cmd.Data)
	h := &contextCommand{s: s, id: cmd.ContextID}
	var facts costing.Facts
	h.prepare = func(ctx context.Context) (err error) {
		facts, err = s.facts(ctx, data, false)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := costing.CheckSave(h.cc, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if err := costing.CheckDraft(data, facts); err != nil {
			return action.Outcome{}, err
		}
		created := h.cc == nil
		if created {
			h.cc = costing.New(cmd.ContextID, d.Actor.ID, d.Team)
		}
		r := h.cc.Save(data, d.Actor, d.Epoch, s.Clock.Now())
		return h.save(ctx, exec, created, r)
	}
	return s.run(ctx, action.Command{Name: api.CommandSaveContext, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ContextID},
		cmd, cmd.Validate(), &preparedContext{h})
}

// ReviseContext copies a sealed revision into a new working revision (Python costing_context.revise).
func (s Service) ReviseContext(ctx context.Context, cmd api.ReviseContext) (api.Receipt, error) {
	h := &contextCommand{s: s, id: cmd.ContextID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := costing.Check(h.cc, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		r, err := h.cc.Revise(cmd.BaseRevision, d.Actor, d.Epoch, s.Clock.Now())
		if err != nil {
			return action.Outcome{}, err
		}
		return h.save(ctx, exec, false, r)
	}
	return s.run(ctx, action.Command{Name: api.CommandReviseContext, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ContextID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
}

// DiscardContextDraft ends the working revision (Python costing_context.discard_draft).
func (s Service) DiscardContextDraft(ctx context.Context, cmd api.DiscardContextDraft) (api.Receipt, error) {
	h := &contextCommand{s: s, id: cmd.ContextID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := costing.Check(h.cc, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		r, err := h.cc.Discard(cmd.WorkingRevision, d.Actor, d.Epoch, s.Clock.Now(), cmd.Reason)
		if err != nil {
			return action.Outcome{}, err
		}
		return h.save(ctx, exec, false, r)
	}
	return s.run(ctx, action.Command{Name: api.CommandDiscardContextDraft, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ContextID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
}

// VerifyContext seals the working revision (Python costing_context.verify, T02 §13.1, §13.2): EVD,
// INQ, CAT and POL are read before the transaction and judged; inside it, after the root is locked,
// the RFQ binding is re-checked through RfqGuard v1 and each configuration through ConfigGuard v1.
// The verifier reads the sources of the draft's assumptions and of its own evidence.
func (s Service) VerifyContext(ctx context.Context, cmd api.VerifyContext) (api.Receipt, error) {
	h := &contextCommand{s: s, id: cmd.ContextID}
	var facts costing.Facts
	var seen int64 // the root version the upstream facts were read for
	h.prepare = func(ctx context.Context) error {
		var stored *costing.Context
		err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
			var err error
			stored, err = postgres.LoadContext(ctx, exec, scope(ctx), cmd.ContextID, postgres.NoLock)
			if stored != nil && !d.Sees(stored.OwnerActor, stored.OwnerTeam) {
				stored = nil
			}
			return err
		})
		if err != nil {
			return err
		}
		// Apply refuses a context that is missing or has no working revision, in the contract's
		// order; the evidence the command cites is asked about all the same.
		var data costing.Data
		if stored != nil && stored.Working() != nil {
			seen, data = stored.RootVersion, stored.Working().Data
		}
		facts, err = s.facts(ctx, data, true, cmd.Evidence...)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := costing.Check(h.cc, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if err := h.cc.CheckVerify(cmd.WorkingRevision, cmd.SupersedesRevision, d.Actor); err != nil {
			return action.Outcome{}, err
		}
		if h.cc.RootVersion != seen {
			// The context changed after its references were read; verifying would judge stale facts.
			return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
		}
		now := s.Clock.Now()
		r, err := h.cc.Verify(cmd.WorkingRevision, cmd.SupersedesRevision, d.Actor, d.Epoch, now, evidence(cmd.Evidence), facts)
		if err != nil {
			return action.Outcome{}, err
		}
		// The command's own evidence is judged last, before the guards lock upstream rows (Python
		// verify; SUP's seal).
		if err = readable(cmd.Evidence, facts.Sources, "/args/evidence"); err != nil {
			return action.Outcome{}, err
		}
		if err = guard(ctx, exec, r.Data, r.Seal.Rfq, now); err != nil {
			return action.Outcome{}, err
		}
		event := action.Event{Type: api.EventCostingContextVerified, AggregateType: "CostingContext", AggregateID: h.cc.ID,
			AggregateVersion: h.cc.RootVersion, Fields: map[string]any{"context_id": h.cc.ID, "revision": r.Number,
				"supersedes_revision": cmd.SupersedesRevision, "rfq_id": r.Seal.Rfq.RfqID, "rfq_row_version": r.Seal.Rfq.RowVersion,
				"context_kind": r.Data.Kind}}
		return h.save(ctx, exec, false, r, event)
	}
	return s.run(ctx, action.Command{Name: api.CommandVerifyContext, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ContextID},
		cmd, cmd.Validate(), &preparedContext{h})
}

// WithdrawContext withdraws a sealed revision (Python costing_context.withdraw); the withdrawer
// reads the sources of its evidence.
func (s Service) WithdrawContext(ctx context.Context, cmd api.WithdrawContext) (api.Receipt, error) {
	h := &contextCommand{s: s, id: cmd.ContextID}
	var sources map[string]bool
	h.prepare = func(ctx context.Context) (err error) {
		sources, err = s.sources(ctx, cited(cmd.Evidence))
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := costing.Check(h.cc, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		r, err := h.cc.Withdraw(cmd.TargetRevision, d.Actor, d.Epoch, s.Clock.Now(), cmd.Reason, evidence(cmd.Evidence))
		if err != nil {
			return action.Outcome{}, err
		}
		if err = readable(cmd.Evidence, sources, "/args/evidence"); err != nil {
			return action.Outcome{}, err
		}
		return h.save(ctx, exec, false, r)
	}
	return s.run(ctx, action.Command{Name: api.CommandWithdrawContext, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ContextID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), &preparedContext{h})
}

// measurementHandler locks the measurement root and applies one command. Each measurement command
// cites evidence, which prepare asks EVD about before the transaction.
type measurementHandler struct {
	s       Service
	id      string
	m       *measurement.Snapshot
	from    int64
	cited   []api.Evidence
	sources map[string]bool // the cited sources the caller reads
	apply   func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *measurementHandler) Prepare(ctx context.Context) (err error) {
	h.sources, err = h.s.sources(ctx, cited(h.cited))
	return err
}

func (h *measurementHandler) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.m, h.from = nil, 0
	h.m, err = postgres.LoadMeasurement(ctx, exec, scope(ctx), h.id, postgres.ForUpdate)
	if h.m != nil {
		h.from = h.m.RootVersion
	}
	return err
}

func (h *measurementHandler) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

func (h *measurementHandler) save(ctx context.Context, exec bentopg.Executor, created bool, r *measurement.Revision, events ...action.Event) (action.Outcome, error) {
	if err := postgres.SaveMeasurement(ctx, exec, scope(ctx), h.m, created, h.from, h.s.Clock.Now()); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: h.m.ID, RowVersion: h.m.RootVersion, Status: string(r.Status), Events: events}, nil
}

func (h *measurementHandler) recorded(r *measurement.Revision, supersedes int64) action.Event {
	return action.Event{Type: api.EventMeasurementRecorded, AggregateType: "MeasurementSnapshot", AggregateID: h.m.ID,
		AggregateVersion: h.m.RootVersion, Fields: map[string]any{"measurement_id": h.m.ID, "revision": r.Number,
			"supersedes_revision": supersedes}}
}

// RecordMeasurement records a new measurement snapshot (Python measurement.record_verified). The
// recorder reads the sources of the data's evidence, judged before the measurement rules (Python
// _access_document before _document_ready).
func (s Service) RecordMeasurement(ctx context.Context, cmd api.RecordMeasurement) (api.Receipt, error) {
	h := &measurementHandler{s: s, id: cmd.MeasurementID, cited: cmd.Data.Evidence}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := measurement.CheckRecord(h.m); err != nil {
			return action.Outcome{}, err
		}
		if err := readable(cmd.Data.Evidence, h.sources, "/args/data/evidence"); err != nil {
			return action.Outcome{}, err
		}
		var err error
		if h.m, err = measurement.Record(cmd.MeasurementID, measurementData(cmd.Data), d.Actor, d.Team, d.Epoch, s.Clock.Now()); err != nil {
			return action.Outcome{}, err
		}
		r := h.m.Active()
		return h.save(ctx, exec, true, r, h.recorded(r, 0))
	}
	return s.run(ctx, action.Command{Name: api.CommandRecordMeasurement, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.MeasurementID},
		cmd, cmd.Validate(), h)
}

// SupersedeMeasurement records the next revision of a measurement (Python measurement.supersede),
// with the evidence judged as in RecordMeasurement, after the revision it supersedes.
func (s Service) SupersedeMeasurement(ctx context.Context, cmd api.SupersedeMeasurement) (api.Receipt, error) {
	h := &measurementHandler{s: s, id: cmd.MeasurementID, cited: cmd.Data.Evidence}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := measurement.Check(h.m, h.m != nil && d.Sees(h.m.OwnerActor, h.m.OwnerTeam), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if err := h.m.CheckSupersede(cmd.ActiveRevision); err != nil {
			return action.Outcome{}, err
		}
		if err := readable(cmd.Data.Evidence, h.sources, "/args/data/evidence"); err != nil {
			return action.Outcome{}, err
		}
		r, err := h.m.Supersede(cmd.ActiveRevision, measurementData(cmd.Data), d.Actor, d.Epoch, s.Clock.Now(), cmd.Reason)
		if err != nil {
			return action.Outcome{}, err
		}
		return h.save(ctx, exec, false, r, h.recorded(r, cmd.ActiveRevision))
	}
	return s.run(ctx, action.Command{Name: api.CommandSupersedeMeasurement, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.MeasurementID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
}

// WithdrawMeasurement withdraws a measurement revision (Python measurement.withdraw); the
// withdrawer reads the sources of its evidence.
func (s Service) WithdrawMeasurement(ctx context.Context, cmd api.WithdrawMeasurement) (api.Receipt, error) {
	h := &measurementHandler{s: s, id: cmd.MeasurementID, cited: cmd.Evidence}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := measurement.Check(h.m, h.m != nil && d.Sees(h.m.OwnerActor, h.m.OwnerTeam), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		r, err := h.m.Withdraw(cmd.TargetRevision, d.Actor, d.Epoch, s.Clock.Now(), cmd.Reason, measurementEvidence(cmd.Evidence))
		if err != nil {
			return action.Outcome{}, err
		}
		if err = readable(cmd.Evidence, h.sources, "/args/evidence"); err != nil {
			return action.Outcome{}, err
		}
		return h.save(ctx, exec, false, r)
	}
	return s.run(ctx, action.Command{Name: api.CommandWithdrawMeasurement, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.MeasurementID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
}

// GetContext returns a costing context the caller sees, audited.
func (s Service) GetContext(ctx context.Context, id string) (api.ContextView, error) {
	var v api.ContextView
	err := s.Ledger.Read(ctx, CommandGetContext, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		cc, err := postgres.LoadContext(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if cc == nil || !d.Sees(cc.OwnerActor, cc.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = contextView(cc)
		return nil
	})
	return v, err
}

// GetMeasurement returns a measurement snapshot the caller sees, audited.
func (s Service) GetMeasurement(ctx context.Context, id string) (api.MeasurementView, error) {
	var v api.MeasurementView
	err := s.Ledger.Read(ctx, CommandGetMeasurement, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		m, err := postgres.LoadMeasurement(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if m == nil || !d.Sees(m.OwnerActor, m.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = measurementView(m)
		return nil
	})
	return v, err
}

// Audit returns CST's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, CommandAudit)
}

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}
