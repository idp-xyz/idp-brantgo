package plugin

import (
	"context"
	"errors"
	"slices"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/plugin/postgres"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Status values of an activation.
const (
	StatusActive   = "active"
	StatusDraining = "draining_readonly"
	StatusDisabled = "disabled"
)

var anyActor = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}

// Rules is the platform's authorization policy. The three lifecycle commands are human-only.
func Rules() []authorization.Rule {
	return []authorization.Rule{
		authorization.HumanOnly(CommandEnable, CommandEnable),
		authorization.HumanOnly(CommandEnterReadOnly, CommandEnterReadOnly),
		authorization.HumanOnly(CommandDisable, CommandDisable),
		{Command: CommandList, Permission: CommandList, ActorKinds: anyActor},
		{Command: CommandAudit, Permission: CommandAudit, ActorKinds: anyActor},
	}
}

// Visible is true for every activation in the company: the row is not owned by one team.
func Visible(context.Context, bentopg.Executor, authorization.Decision, string, string) (bool, error) {
	return true, nil
}

// Service runs plugin.enable, plugin.enter_readonly and plugin.disable on brant_ext.
type Service struct {
	Ledger   *action.Ledger
	Clock    application.Clock
	Registry *Registry
	InFlight Checker
}

func (s Service) scope(ctx context.Context) kernel.Scope {
	sc, _ := bento.ScopeFromContext(ctx)
	return sc
}

func (s Service) run(ctx context.Context, cmd action.Command, req Command, validation error, h action.Handler) (Receipt, error) {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, cmd.Name, cmd.ObjectID, rej.Code); err != nil {
				return Receipt{}, errors.Join(validation, err)
			}
		}
		return Receipt{}, validation
	}
	name, key, args := request(req)
	hash, err := action.RequestHash(args)
	if err != nil {
		return Receipt{}, err
	}
	cmd.Name, cmd.IdempotencyKey, cmd.RequestHash = name, key, hash
	res, err := s.Ledger.Execute(ctx, cmd, h)
	return Receipt(res), err
}

func request(cmd Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case EnablePlugin:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return CommandEnable, key, c
	case EnterReadOnlyPlugin:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return CommandEnterReadOnly, key, c
	case DisablePlugin:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return CommandDisable, key, c
	}
	return "", "", nil
}

type act struct {
	s    Service
	id   string
	reg  Registration
	row  *postgres.Activation
	busy bool
}

func (h *act) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.row, err = postgres.Load(ctx, exec, h.s.scope(ctx), h.id, postgres.ForUpdate)
	return err
}

// Enable activates a registered, compatible plugin.
func (s Service) Enable(ctx context.Context, cmd EnablePlugin) (Receipt, error) {
	h := &act{s: s, id: cmd.PluginID}
	err := cmd.Validate()
	if err == nil {
		reg, ok := s.Registry.Get(cmd.PluginID)
		switch {
		case !ok:
			err = kernel.Reject(kernel.CodeReferenceInvalid, "/args/plugin_id", "No such plugin is registered")
		case !Compatible(CoreVersion, reg.Manifest):
			err = VersionIncompatible()
		default:
			h.reg = reg
		}
	}
	return s.run(ctx, action.Command{ObjectID: cmd.PluginID}, cmd, err, enableHandler{h})
}

type enableHandler struct{ *act }

func (h enableHandler) Lock(ctx context.Context, exec bentopg.Executor) error {
	return h.act.Lock(ctx, exec)
}

func (h enableHandler) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	m := h.reg.Manifest
	profile := string(m.Profile())
	if m.ProvidesCost() {
		other, err := postgres.OtherActiveCostProvider(ctx, exec, h.s.scope(ctx), profile, m.ID)
		if err != nil {
			return action.Outcome{}, err
		}
		if other != "" {
			return action.Outcome{}, CostConflict()
		}
	}
	now := h.s.Clock.Now()
	next := postgres.Activation{
		PluginID: m.ID, DocumentProfile: profile, Status: StatusActive, Version: m.Version,
		ContractVersion: m.ContractVersion, ManifestSHA256: m.SHA256(), CostProvider: m.ProvidesCost(),
		AuthorityEpoch: d.Epoch, ActorKind: string(d.Actor.Kind), ActorID: d.Actor.ID, UpdatedAt: kernel.Instant(now),
	}
	var err error
	if h.row == nil {
		next.RowVersion = 1
		err = postgres.Insert(ctx, exec, h.s.scope(ctx), next, now)
	} else if h.row.Status != StatusDisabled {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/plugin_id", "The plugin is not disabled")
	} else {
		next.RowVersion = h.row.RowVersion + 1
		err = postgres.Update(ctx, exec, h.s.scope(ctx), next, h.row.RowVersion, h.row.Status, now)
	}
	if postgres.CostProviderConflict(err) {
		return action.Outcome{}, CostConflict()
	}
	if err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: m.ID, RowVersion: next.RowVersion, Status: StatusActive}, nil
}

// EnterReadOnly moves active to draining_readonly.
func (s Service) EnterReadOnly(ctx context.Context, cmd EnterReadOnlyPlugin) (Receipt, error) {
	h := &act{s: s, id: cmd.PluginID}
	return s.run(ctx, action.Command{ObjectID: cmd.PluginID}, cmd, cmd.Validate(), readonlyHandler{h})
}

type readonlyHandler struct{ *act }

func (h readonlyHandler) Lock(ctx context.Context, exec bentopg.Executor) error {
	return h.act.Lock(ctx, exec)
}

func (h readonlyHandler) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if h.row == nil || h.row.Status != StatusActive {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/plugin_id", "The plugin is not active")
	}
	now := h.s.Clock.Now()
	next := *h.row
	next.Status = StatusDraining
	next.RowVersion = h.row.RowVersion + 1
	next.AuthorityEpoch = d.Epoch
	next.ActorKind, next.ActorID = string(d.Actor.Kind), d.Actor.ID
	next.UpdatedAt = kernel.Instant(now)
	if err := postgres.Update(ctx, exec, h.s.scope(ctx), next, h.row.RowVersion, h.row.Status, now); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: h.id, RowVersion: next.RowVersion, Status: StatusDraining}, nil
}

// Disable moves draining_readonly to disabled when the profile has nothing open.
func (s Service) Disable(ctx context.Context, cmd DisablePlugin) (Receipt, error) {
	h := &act{s: s, id: cmd.PluginID}
	return s.run(ctx, action.Command{ObjectID: cmd.PluginID}, cmd, cmd.Validate(), disableHandler{h})
}

type disableHandler struct{ *act }

func (h disableHandler) Prepare(ctx context.Context) error {
	profile, check, err := h.prepareRow(ctx)
	if err != nil || !check || h.s.InFlight == nil {
		return err
	}
	// InFlight opens the downstream contexts' own transactions. It must not run on the context
	// that still carries this platform's executor. In draining_readonly the guard refuses every
	// new apparel document, so this set can only shrink before Apply locks the activation.
	h.busy, err = h.s.InFlight.InFlight(ctx, profile)
	if err != nil {
		return err
	}
	if h.busy {
		return InUse()
	}
	return nil
}

func (h disableHandler) Lock(ctx context.Context, exec bentopg.Executor) error {
	return h.act.Lock(ctx, exec)
}

func (h disableHandler) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	switch {
	case h.row != nil && h.row.Status == StatusActive:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/plugin_id",
			"Run plugin.enter_readonly before plugin.disable")
	case h.row == nil || h.row.Status != StatusDraining:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/plugin_id", "The plugin is not draining")
	}
	if h.busy {
		return action.Outcome{}, InUse()
	}
	now := h.s.Clock.Now()
	next := *h.row
	next.Status = StatusDisabled
	next.RowVersion = h.row.RowVersion + 1
	next.AuthorityEpoch = d.Epoch
	next.ActorKind, next.ActorID = string(d.Actor.Kind), d.Actor.ID
	next.UpdatedAt = kernel.Instant(now)
	if err := postgres.Update(ctx, exec, h.s.scope(ctx), next, h.row.RowVersion, h.row.Status, now); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: h.id, RowVersion: next.RowVersion, Status: StatusDisabled}, nil
}

// prepareRow reads the activation in its own transaction and returns the profile to check.
// The in-flight read happens after this returns, outside any transaction.
func (h disableHandler) prepareRow(ctx context.Context) (profile DocumentProfile, check bool, err error) {
	err = h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		row, err := postgres.Load(ctx, exec, h.s.scope(ctx), h.id, postgres.NoLock)
		if err != nil || row == nil {
			return err
		}
		if row.Status == StatusDraining {
			profile = DocumentProfile(row.DocumentProfile)
			check = true
		}
		return nil
	})
	return profile, check, err
}

// ListPlugins returns every registered plugin with the company's activation. A plugin that was
// enabled and later dropped from the process is still listed from the ledger.
func (s Service) ListPlugins(ctx context.Context) (PluginList, error) {
	var list PluginList
	err := s.Ledger.Read(ctx, CommandList, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		rows, err := postgres.List(ctx, exec, s.scope(ctx))
		if err != nil {
			return err
		}
		byID := map[string]postgres.Activation{}
		for _, row := range rows {
			byID[row.PluginID] = row
		}
		seen := map[string]bool{}
		for _, reg := range s.Registry.All() {
			seen[reg.Manifest.ID] = true
			st := PluginState{PluginID: reg.Manifest.ID, Version: reg.Manifest.Version, ContractVersion: reg.Manifest.ContractVersion,
				DocumentProfile: string(reg.Manifest.Profile()), Status: StatusDisabled, ManifestSHA256: reg.Manifest.SHA256(),
				CostProvider: reg.Manifest.ProvidesCost()}
			if row, ok := byID[reg.Manifest.ID]; ok {
				st.Status, st.Version, st.ContractVersion = row.Status, row.Version, row.ContractVersion
				st.DocumentProfile, st.ManifestSHA256, st.CostProvider, st.RowVersion = row.DocumentProfile, row.ManifestSHA256, row.CostProvider, row.RowVersion
			}
			list.Plugins = append(list.Plugins, st)
		}
		for _, row := range rows {
			if seen[row.PluginID] {
				continue
			}
			list.Plugins = append(list.Plugins, PluginState{PluginID: row.PluginID, Version: row.Version, ContractVersion: row.ContractVersion,
				DocumentProfile: row.DocumentProfile, Status: row.Status, ManifestSHA256: row.ManifestSHA256,
				CostProvider: row.CostProvider, RowVersion: row.RowVersion})
		}
		slices.SortFunc(list.Plugins, func(a, b PluginState) int {
			if a.PluginID < b.PluginID {
				return -1
			}
			if a.PluginID > b.PluginID {
				return 1
			}
			return 0
		})
		if list.Plugins == nil {
			list.Plugins = []PluginState{}
		}
		return nil
	})
	return list, err
}

// Audit returns the platform audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, CommandAudit)
}

// AuditRejected records a refusal the transport decided before the command ran.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}

// Reconcile reports what became of cmd.
func (s Service) Reconcile(ctx context.Context, cmd Command) (Reconciliation, error) {
	name, key, args := request(cmd)
	if name == "" {
		return Reconciliation{}, kernel.Reject(kernel.CodeSchemaInvalid, "/command", "")
	}
	hash, err := action.RequestHash(args)
	if err != nil {
		return Reconciliation{}, err
	}
	r, err := s.Ledger.Reconcile(ctx, name, key, hash)
	if err != nil {
		return Reconciliation{}, err
	}
	out := Reconciliation{State: r.State, ActionID: r.ActionID, ObjectID: r.ObjectID}
	if r.Result != nil {
		receipt := Receipt(*r.Result)
		out.Receipt = &receipt
	}
	return out, nil
}
