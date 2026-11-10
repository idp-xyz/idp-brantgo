// Package application runs CAT's commands and queries on the command ledger. SKU rules live in
// domain/sku; SQL lives in adapters/postgres. Confirmations read their evidence through EVD's api
// before CAT's transaction begins (Context Map R62).
package application

import (
	"context"
	"errors"
	"slices"
	"strconv"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/sku"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Read commands and permissions.
const (
	CommandGetSku   = "sku.get"
	CommandAudit    = "cat.audit"
	PermissionRead  = "cat.read"
	PermissionAudit = "cat.audit"
)

var (
	anyActor     = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}
	humanOrAgent = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}
)

// Rules is CAT's authorization policy (05 stage humanOnlyCommands: sku.confirm, sku.deactivate).
func Rules() []authorization.Rule {
	return []authorization.Rule{
		{Command: api.CommandSaveSku, Permission: api.CommandSaveSku, ActorKinds: humanOrAgent},
		authorization.HumanOnly(api.CommandConfirmSku, api.CommandConfirmSku),
		{Command: api.CommandReviseSpec, Permission: api.CommandReviseSpec, ActorKinds: humanOrAgent},
		authorization.HumanOnly(api.CommandDeactivateSku, api.CommandDeactivateSku),
		{Command: CommandGetSku, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandAudit, Permission: PermissionAudit, ActorKinds: anyActor},
	}
}

// Visible reports whether d's actor sees the SKU a stored result names.
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, _, id string) (bool, error) {
	scope, _ := bento.ScopeFromContext(ctx)
	s, err := postgres.Load(ctx, exec, scope, id, false)
	return err == nil && s != nil && d.Sees(s.OwnerActor, s.OwnerTeam), err
}

// Service runs CAT's use cases.
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

type applyFunc func(h *skuCommand, ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)

// SaveSku saves a whole draft SKU (Python master.upsert_draft kind=sku).
func (s Service) SaveSku(ctx context.Context, cmd api.SaveSku) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: api.CommandSaveSku, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.SkuID},
		struct{ ID, Name, BaseUnit string }{cmd.SkuID, cmd.Name, cmd.BaseUnit}, cmd.Validate(),
		&skuCommand{s: s, id: cmd.SkuID, apply: func(h *skuCommand, ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
			if err := sku.CheckSave(h.sku, h.visible(d), cmd.ExpectedVersion); err != nil {
				return action.Outcome{}, err
			}
			if h.sku == nil {
				h.sku = sku.New(cmd.SkuID, d.Actor.ID, d.Team)
				h.sku.ReplaceDraft(cmd.Name, cmd.BaseUnit)
				return h.outcome(""), postgres.Insert(ctx, exec, h.scope(ctx), h.sku, s.Clock.Now())
			}
			h.sku.ReplaceDraft(cmd.Name, cmd.BaseUnit)
			return h.save(ctx, exec, "")
		}})
}

// ConfirmSku confirms a draft SKU on readable evidence (Python master.confirm).
func (s Service) ConfirmSku(ctx context.Context, cmd api.ConfirmSku) (api.Receipt, error) {
	h := &skuCommand{s: s, id: cmd.SkuID, evidence: cmd.EvidenceIDs}
	h.apply = func(h *skuCommand, ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := sku.Check(h.sku, h.visible(d), cmd.ExpectedVersion, sku.Draft); err != nil {
			return action.Outcome{}, err
		}
		for i, id := range h.evidence {
			if _, ok := h.sources[id]; !ok {
				return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/evidence_ids/"+strconv.Itoa(i), "Source is not accessible")
			}
		}
		if err := h.sku.Confirm(); err != nil {
			return action.Outcome{}, err
		}
		if err := postgres.InsertRevision(ctx, exec, h.scope(ctx), h.sku, cmd.EvidenceIDs, d.Actor, d.Epoch, s.Clock.Now()); err != nil {
			return action.Outcome{}, err
		}
		return h.save(ctx, exec, api.EventSkuConfirmed)
	}
	return s.run(ctx, action.Command{Name: api.CommandConfirmSku, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.SkuID},
		struct {
			ID       string
			Evidence []string
		}{cmd.SkuID, cmd.EvidenceIDs}, cmd.Validate(), &preparedSku{h})
}

// ReviseSpec reopens a confirmed SKU as a draft; confirmed revisions stay.
func (s Service) ReviseSpec(ctx context.Context, cmd api.ReviseSpec) (api.Receipt, error) {
	return s.lifecycle(ctx, api.CommandReviseSpec, cmd.IdempotencyKey, cmd.SkuID, cmd.ExpectedVersion, cmd.Reason, cmd.Validate(),
		api.EventSpecificationRevised, (*sku.Sku).Reopen)
}

// DeactivateSku stops a confirmed SKU for new business (Python master.deactivate).
func (s Service) DeactivateSku(ctx context.Context, cmd api.DeactivateSku) (api.Receipt, error) {
	return s.lifecycle(ctx, api.CommandDeactivateSku, cmd.IdempotencyKey, cmd.SkuID, cmd.ExpectedVersion, cmd.Reason, cmd.Validate(),
		api.EventSkuDeactivated, (*sku.Sku).Deactivate)
}

func (s Service) lifecycle(ctx context.Context, command, key, id string, expected int64, reason string, validation error, event string, move func(*sku.Sku)) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: command, IdempotencyKey: key, ObjectID: id, Reason: reason},
		struct{ ID, Reason string }{id, reason}, validation,
		&skuCommand{s: s, id: id, apply: func(h *skuCommand, ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
			if err := sku.Check(h.sku, h.visible(d), expected, sku.Confirmed); err != nil {
				return action.Outcome{}, err
			}
			move(h.sku)
			return h.save(ctx, exec, event)
		}})
}

type skuCommand struct {
	s        Service
	id       string
	evidence []string
	sources  map[string]evdapi.SourceRef
	sku      *sku.Sku
	from     int64
	apply    applyFunc
}

func (h *skuCommand) scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

func (h *skuCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.sku, err = postgres.Load(ctx, exec, h.scope(ctx), h.id, true)
	if h.sku != nil {
		h.from = h.sku.RowVersion
	}
	return err
}

func (h *skuCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(h, ctx, exec, d)
}

func (h *skuCommand) visible(d authorization.Decision) bool {
	return h.sku != nil && d.Sees(h.sku.OwnerActor, h.sku.OwnerTeam)
}

func (h *skuCommand) save(ctx context.Context, exec bentopg.Executor, event string) (action.Outcome, error) {
	if err := postgres.Update(ctx, exec, h.scope(ctx), h.sku, h.from, h.s.Clock.Now()); err != nil {
		return action.Outcome{}, err
	}
	return h.outcome(event), nil
}

func (h *skuCommand) outcome(event string) action.Outcome {
	out := action.Outcome{ObjectID: h.sku.ID, RowVersion: h.sku.RowVersion, Status: string(h.sku.Status)}
	if event != "" {
		out.Events = []action.Event{{Type: event, AggregateType: "Sku", AggregateID: h.sku.ID, AggregateVersion: h.sku.RowVersion,
			Fields: map[string]any{"sku_id": h.sku.ID, "spec_revision": h.sku.Revision, "row_version": h.sku.RowVersion}}}
	}
	return out
}

type preparedSku struct{ *skuCommand }

func (h *preparedSku) Prepare(ctx context.Context) error {
	var err error
	h.sources, err = h.s.Sources.ResolveSources(ctx, h.evidence)
	return err
}

// GetSku returns a SKU and its confirmed specification revisions, audited.
func (s Service) GetSku(ctx context.Context, id string) (api.SkuView, error) {
	var v api.SkuView
	err := s.Ledger.Read(ctx, CommandGetSku, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		scope, _ := bento.ScopeFromContext(ctx)
		k, err := postgres.Load(ctx, exec, scope, id, false)
		if err != nil {
			return err
		}
		if k == nil || !d.Sees(k.OwnerActor, k.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.SkuView{SkuID: k.ID, Status: string(k.Status), RowVersion: k.RowVersion, Name: k.Name, BaseUnit: k.BaseUnit}
		revisions, err := postgres.Revisions(ctx, exec, scope, id)
		for _, r := range revisions {
			v.Revisions = append(v.Revisions, api.SpecRevision{Revision: r.Revision, Name: r.Name, BaseUnit: r.BaseUnit, EvidenceIDs: r.EvidenceIDs, ConfirmedBy: r.By})
		}
		return err
	})
	return v, err
}

// ResolveSkus answers reference lookups of downstream contexts.
func (s Service) ResolveSkus(ctx context.Context, ids []string) (map[string]api.SkuRef, error) {
	out := map[string]api.SkuRef{}
	if len(ids) == 0 {
		return out, nil
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		scope, _ := bento.ScopeFromContext(ctx)
		skus, err := postgres.Skus(ctx, exec, scope, slices.Compact(slices.Sorted(slices.Values(ids))))
		for _, k := range skus {
			if d.Sees(k.OwnerActor, k.OwnerTeam) {
				out[k.ID] = api.SkuRef{SkuID: k.ID, SpecRevision: k.Revision, RowVersion: k.RowVersion, Status: string(k.Status), BaseUnit: k.BaseUnit}
			}
		}
		return err
	})
	return out, err
}

// Audit returns CAT's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, CommandAudit)
}

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}
