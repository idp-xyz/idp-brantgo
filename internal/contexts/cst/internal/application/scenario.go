package application

import (
	"context"
	"encoding/json"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/scenario"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Cost scenarios (CostScenario, WBS S2-05). cost_scenario.save_draft replaces the draft whole;
// cost_scenario.evaluate reads every input before CST's transaction (the Preparer step: CST's own
// costing context and measurement revisions, INQ's RFQ, CAT's specification standings, SUP's
// supply conditions and POL's configurations, each at the command's instant), computes the
// evaluation in the domain, and inside the transaction, after the scenario root is locked FOR
// UPDATE, re-reads the CST inputs FOR SHARE and re-checks the RFQ through RfqGuard v1, every POL
// input through ConfigGuard v1 and every SUP input through SupplyConditionGuard v1 (ADR-BR-004
// option B, ADR-BR-011; context code order INQ, POL, SUP), comparing the content hashes it read:
// an evaluation is committed only on inputs that are still what it computed on.

// Read commands of scenarios.
const (
	CommandGetScenario   = "cost_scenario.get"
	CommandGetEvaluation = "cost_evaluation.get"
)

// scenarioRules are the authorization rules of scenarios: an AI agent saves and evaluates through
// the same command path as a human (command catalog: 人工或 AI); freezing is human only
// (snapshotRules).
func scenarioRules() []authorization.Rule {
	return append([]authorization.Rule{
		{Command: api.CommandSaveScenario, Permission: api.CommandSaveScenario, ActorKinds: humanOrAgent},
		{Command: api.CommandEvaluateScenario, Permission: api.CommandEvaluateScenario, ActorKinds: humanOrAgent},
		{Command: CommandGetScenario, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandGetEvaluation, Permission: PermissionRead, ActorKinds: anyActor},
	}, snapshotRules()...)
}

func scenarioCommand(command string) bool {
	return command == api.CommandSaveScenario || command == api.CommandEvaluateScenario || snapshotCommand(command)
}

// visibleScenario reports whether d's actor sees the scenario a stored result names: the scenario
// itself, or the scenario of an evaluation.
func visibleScenario(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, id string) (bool, error) {
	if snapshotCommand(command) {
		return visibleSnapshotResult(ctx, exec, d, command, id)
	}
	sc := scope(ctx)
	if command == api.CommandEvaluateScenario {
		e, err := postgres.LoadEvaluation(ctx, exec, sc, id)
		if err != nil || e == nil {
			return false, err
		}
		id = e.ScenarioID
	}
	s, err := postgres.LoadScenario(ctx, exec, sc, id, postgres.NoLock)
	return err == nil && s != nil && d.Sees(s.OwnerActor, s.OwnerTeam), err
}

// scenarioRequest is request for the scenario commands.
func scenarioRequest(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.SaveScenario:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandSaveScenario, key, c
	case api.EvaluateScenario:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandEvaluateScenario, key, c
	}
	return snapshotRequest(cmd)
}

// scenarioHandler locks the scenario root and applies one command.
type scenarioHandler struct {
	s       Service
	id      string
	sc      *scenario.Scenario
	from    int64
	prepare func(ctx context.Context) error
	apply   func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *scenarioHandler) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.sc, h.from = nil, 0
	h.sc, err = postgres.LoadScenario(ctx, exec, scope(ctx), h.id, postgres.ForUpdate)
	if h.sc != nil {
		h.from = h.sc.RootVersion
	}
	return err
}

func (h *scenarioHandler) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

func (h *scenarioHandler) Prepare(ctx context.Context) error { return h.prepare(ctx) }

func (h *scenarioHandler) visible(d authorization.Decision) bool {
	return h.sc != nil && d.Sees(h.sc.OwnerActor, h.sc.OwnerTeam)
}

// SaveScenario saves the whole draft of a cost scenario (Python cost_scenario.save_draft). Every
// reference the draft states must be known to its owner and visible (REFERENCE_INVALID); whether it
// is in force is judged when the draft is evaluated.
func (s Service) SaveScenario(ctx context.Context, cmd api.SaveScenario) (api.Receipt, error) {
	data := scenarioData(cmd.Data)
	h := &scenarioHandler{s: s, id: cmd.ScenarioID}
	var known draftFacts
	h.prepare = func(ctx context.Context) (err error) {
		known, err = s.draftFacts(ctx, data)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := scenario.CheckSave(h.sc, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if err := known.check(data); err != nil {
			return action.Outcome{}, err
		}
		created := h.sc == nil
		if created {
			h.sc = scenario.New(cmd.ScenarioID, d.Actor.ID, d.Team)
		}
		now := s.Clock.Now()
		h.sc.Save(data, d.Actor, d.Epoch, now)
		if err := postgres.SaveScenario(ctx, exec, scope(ctx), h.sc, created, h.from, now); err != nil {
			return action.Outcome{}, err
		}
		return action.Outcome{ObjectID: h.sc.ID, RowVersion: h.sc.RootVersion, Status: string(h.sc.Status)}, nil
	}
	return s.run(ctx, action.Command{Name: api.CommandSaveScenario, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ScenarioID},
		cmd, cmd.Validate(), h)
}

// EvaluateScenario evaluates the current draft (Python cost_scenario.evaluate): it appends an
// immutable evaluation and leaves the root version as it is (SM-CST-SCENARIO). The receipt's
// ObjectID is the evaluation ID.
func (s Service) EvaluateScenario(ctx context.Context, cmd api.EvaluateScenario) (api.EvaluationReceipt, error) {
	h := &scenarioHandler{s: s, id: cmd.ScenarioID}
	var in scenario.Inputs
	var seen int64 // the root version the inputs were read for; 0: nothing was read
	h.prepare = func(ctx context.Context) error {
		var stored *scenario.Scenario
		err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
			var err error
			stored, err = postgres.LoadScenario(ctx, exec, scope(ctx), cmd.ScenarioID, postgres.NoLock)
			if stored != nil && !d.Sees(stored.OwnerActor, stored.OwnerTeam) {
				stored = nil
			}
			return err
		})
		if err != nil || stored == nil || stored.Status != scenario.Draft || stored.Data.Complete() != nil {
			return err // Apply refuses what is missing, in the contract's order
		}
		seen = stored.RootVersion
		in, err = s.inputs(ctx, stored.ID, stored.Data, s.Clock.Now())
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := scenario.Check(h.sc, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if err := h.sc.Data.Complete(); err != nil {
			return action.Outcome{}, err
		}
		if h.sc.RootVersion != seen {
			// The draft changed after its inputs were read; evaluating would compute on stale facts.
			return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
		}
		computed, err := scenario.Evaluate(in)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = GuardInputs(ctx, exec, d, computed.Input.Inputs, in.Now); err != nil {
			return action.Outcome{}, err
		}
		now := s.Clock.Now()
		e := &scenario.Evaluation{ID: action.NewID("eval", now), ScenarioID: h.sc.ID, Number: h.sc.Append(),
			ScenarioVersion: h.sc.RootVersion, EvaluatedBy: d.Actor, Epoch: d.Epoch, EvaluatedAt: kernel.Instant(now),
			EngineVersion: scenario.EngineVersion, NormalizationVersion: scenario.NormalizationVersion, Input: computed.InputBytes,
			InputHash: computed.InputHash, Document: computed.ResultBytes, ResultHash: computed.ResultHash, Result: computed.Result,
			Inputs: computed.Input.Inputs}
		if err = postgres.AppendEvaluation(ctx, exec, scope(ctx), e); err != nil {
			return action.Outcome{}, err
		}
		event := action.Event{Type: api.EventCostScenarioEvaluated, AggregateType: "CostScenario", AggregateID: h.sc.ID,
			AggregateVersion: h.sc.RootVersion, Fields: map[string]any{"scenario_id": h.sc.ID, "evaluation_id": e.ID,
				"evaluation_no": e.Number, "scenario_version": e.ScenarioVersion, "engine_version": e.EngineVersion,
				"input_hash": e.InputHash, "result_hash": e.ResultHash, "verification_status": e.Result.VerificationStatus,
				"comparison_status": e.Result.ComparisonStatus}}
		return action.Outcome{ObjectID: e.ID, RowVersion: h.sc.RootVersion, Status: api.StatusEvaluated, Events: []action.Event{event}}, nil
	}
	r, err := s.run(ctx, action.Command{Name: api.CommandEvaluateScenario, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ScenarioID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.EvaluationReceipt{}, err
	}
	return s.evaluationReceipt(ctx, r)
}

// evaluationReceipt completes the receipt of an evaluation, executed or replayed, from the stored
// evaluation it names.
func (s Service) evaluationReceipt(ctx context.Context, r api.Receipt) (api.EvaluationReceipt, error) {
	out := api.EvaluationReceipt{Receipt: r, EvaluationID: r.ObjectID}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		e, err := postgres.LoadEvaluation(ctx, exec, scope(ctx), r.ObjectID)
		if err != nil || e == nil {
			return err
		}
		out.ScenarioID, out.Number, out.ScenarioVersion, out.EngineVersion = e.ScenarioID, e.Number, e.ScenarioVersion, e.EngineVersion
		out.InputHash, out.ResultHash = e.InputHash, e.ResultHash
		out.VerificationStatus, out.ComparisonStatus = e.Result.VerificationStatus, e.Result.ComparisonStatus
		out.Currency, out.TotalMinor = e.Result.Currency, e.Result.TotalMinor
		return nil
	})
	return out, err
}

// GetScenario returns a scenario the caller sees with its evaluations, audited.
func (s Service) GetScenario(ctx context.Context, id string) (api.ScenarioView, error) {
	var v api.ScenarioView
	err := s.Ledger.Read(ctx, CommandGetScenario, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc, err := postgres.LoadScenario(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if sc == nil || !d.Sees(sc.OwnerActor, sc.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		evaluations, err := postgres.Evaluations(ctx, exec, scope(ctx), id)
		if err != nil {
			return err
		}
		v = scenarioView(sc, evaluations)
		return nil
	})
	return v, err
}

// GetEvaluation returns an evaluation of a scenario the caller sees, audited (Python costing.get of
// kind evaluation). The caller must still read every input the evaluation bound (Python
// _history_access): the CST costing context and measurements through their data scope, and SUP's
// supply conditions, INQ's RFQ, CAT's SKUs and POL's configurations through their lookups, which
// answer only what the caller reads. A scenario the caller does not see is NOT_FOUND, an input it
// no longer reads PERMISSION_DENIED.
func (s Service) GetEvaluation(ctx context.Context, id string) (api.EvaluationView, error) {
	var bound []scenario.BoundInput
	if err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		e, err := postgres.LoadEvaluation(ctx, exec, scope(ctx), id)
		if e != nil {
			bound = e.Inputs
		}
		return err
	}); err != nil {
		return api.EvaluationView{}, err
	}
	readable, err := s.readableUpstream(ctx, bound)
	if err != nil {
		return api.EvaluationView{}, err
	}
	h := history{bound: bound, readable: readable}
	var v api.EvaluationView
	err = s.Ledger.Read(ctx, CommandGetEvaluation, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		e, err := postgres.LoadEvaluation(ctx, exec, scope(ctx), id)
		if err != nil {
			return err
		}
		var sc *scenario.Scenario
		if e != nil {
			if sc, err = postgres.LoadScenario(ctx, exec, scope(ctx), e.ScenarioID, postgres.NoLock); err != nil {
				return err
			}
		}
		if sc == nil || !d.Sees(sc.OwnerActor, sc.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		ok, err := h.allowed(ctx, exec, d)
		if err != nil {
			return err
		}
		if !ok {
			return kernel.Reject(kernel.CodePermissionDenied, "/", "")
		}
		v, err = evaluationView(e)
		return err
	})
	return v, err
}

// readableOwn reports whether d's actor sees every CST revision among bound: the costing contexts
// and measurements through their roots' owners.
func readableOwn(ctx context.Context, exec bentopg.Executor, d authorization.Decision, bound []scenario.BoundInput) (bool, error) {
	for _, b := range bound {
		if b.Context != scenario.FromCST {
			continue
		}
		var owner, team string
		switch b.Kind {
		case scenario.KindCostingContext:
			cc, err := postgres.LoadContext(ctx, exec, scope(ctx), b.ID, postgres.NoLock)
			if err != nil || cc == nil {
				return false, err
			}
			owner, team = cc.OwnerActor, cc.OwnerTeam
		case scenario.KindMeasurement:
			m, err := postgres.LoadMeasurement(ctx, exec, scope(ctx), b.ID, postgres.NoLock)
			if err != nil || m == nil {
				return false, err
			}
			owner, team = m.OwnerActor, m.OwnerTeam
		}
		if !d.Sees(owner, team) {
			return false, nil
		}
	}
	return true, nil
}

// LatestEvaluation returns the latest evaluation of a scenario the caller sees with every input it
// bound (api.EvaluationLookups), for the freeze.
func (s Service) LatestEvaluation(ctx context.Context, scenarioID string) (api.BoundEvaluation, error) {
	var out api.BoundEvaluation
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		var err error
		out, err = LatestEvaluation(ctx, exec, d, scenarioID, postgres.NoLock)
		return err
	})
	return out, err
}

// LatestEvaluation reads the latest evaluation of scenarioID with exec inside a CST transaction:
// NOT_FOUND when d's actor does not see the scenario or it has no evaluation. A freeze passes
// postgres.ForUpdate to lock the scenario root first (lock order: own aggregate root FOR UPDATE),
// then re-reads the CST inputs FOR SHARE and guards the upstream ones (GuardInputs).
func LatestEvaluation(ctx context.Context, exec bentopg.Executor, d authorization.Decision, scenarioID string, lock postgres.Lock) (api.BoundEvaluation, error) {
	sc, err := postgres.LoadScenario(ctx, exec, scope(ctx), scenarioID, lock)
	if err != nil {
		return api.BoundEvaluation{}, err
	}
	if sc == nil || !d.Sees(sc.OwnerActor, sc.OwnerTeam) {
		return api.BoundEvaluation{}, kernel.Reject(kernel.CodeNotFound, "/args/scenario_id", "")
	}
	e, err := postgres.LatestEvaluation(ctx, exec, scope(ctx), scenarioID)
	if err != nil {
		return api.BoundEvaluation{}, err
	}
	if e == nil {
		return api.BoundEvaluation{}, kernel.Reject(kernel.CodeNotFound, "/args/scenario_id", "The scenario has no evaluation")
	}
	return boundEvaluation(sc, e), nil
}

func boundEvaluation(sc *scenario.Scenario, e *scenario.Evaluation) api.BoundEvaluation {
	r := e.Result
	return api.BoundEvaluation{EvaluationID: e.ID, ScenarioID: e.ScenarioID, Number: e.Number, ScenarioVersion: e.ScenarioVersion,
		ScenarioStatus: string(sc.Status), ScenarioRootVersion: sc.RootVersion, EvaluatedAt: e.EvaluatedAt, EngineVersion: e.EngineVersion,
		NormalizationVersion: e.NormalizationVersion, InputHash: e.InputHash, ResultHash: e.ResultHash,
		VerificationStatus: r.VerificationStatus, ComparisonStatus: r.ComparisonStatus, Currency: r.Currency, TotalMinor: r.TotalMinor,
		Context: api.ContextRef{ContextID: r.ContextRef.ContextID, Revision: r.ContextRef.Revision},
		Rfq:     api.RfqBinding{RfqID: r.Rfq.RfqID, RowVersion: r.Rfq.RowVersion, LinesHash: r.Rfq.LinesHash}, Inputs: boundInputs(e.Inputs)}
}

func boundInputs(bs []scenario.BoundInput) []api.BoundInput {
	out := make([]api.BoundInput, len(bs))
	for i, b := range bs {
		out[i] = api.BoundInput(b)
	}
	return out
}

func scenarioData(d api.ScenarioData) scenario.Data {
	out := scenario.Data{SelectionReason: d.SelectionReason}
	if r := d.ContextRef; r != nil {
		out.Context = &scenario.ContextRef{ContextID: r.ContextID, Revision: r.Revision}
	}
	for _, a := range d.Allocations {
		out.Allocations = append(out.Allocations, scenario.Allocation{RfqLineID: a.RfqLineID, PurchaseQuantity: a.PurchaseQuantity,
			PurchaseUnit: scenario.Ref{Kind: a.PurchaseUnitRef.Kind, ID: a.PurchaseUnitRef.ID, Revision: a.PurchaseUnitRef.Revision},
			Source:       scenario.LineRef{Kind: a.Source.Kind, ID: a.Source.ID, Revision: a.Source.Revision, ConditionLineID: a.Source.ConditionLineID}})
	}
	for _, r := range d.LogisticsOfferRefs {
		out.LogisticsOffers = append(out.LogisticsOffers, scenario.Ref{Kind: r.Kind, ID: r.ID, Revision: r.Revision})
	}
	for _, r := range d.FXRefs {
		out.FX = append(out.FX, scenario.Ref{Kind: r.Kind, ID: r.ID, Revision: r.Revision})
	}
	return out
}

// scenarioDataView is the stored draft as the contract shows it; lists are never nil.
func scenarioDataView(d scenario.Data) api.ScenarioData {
	out := api.ScenarioData{SelectionReason: d.SelectionReason, Allocations: []api.ScenarioAllocation{},
		LogisticsOfferRefs: []api.SupplyRef{}, FXRefs: []api.ConfigRef{}}
	if r := d.Context; r != nil {
		out.ContextRef = &api.ContextRef{ContextID: r.ContextID, Revision: r.Revision}
	}
	for _, a := range d.Allocations {
		out.Allocations = append(out.Allocations, api.ScenarioAllocation{RfqLineID: a.RfqLineID, PurchaseQuantity: a.PurchaseQuantity,
			PurchaseUnitRef: api.ConfigRef{Kind: a.PurchaseUnit.Kind, ID: a.PurchaseUnit.ID, Revision: a.PurchaseUnit.Revision},
			Source:          api.SupplyLineRef{Kind: a.Source.Kind, ID: a.Source.ID, Revision: a.Source.Revision, ConditionLineID: a.Source.ConditionLineID}})
	}
	for _, r := range d.LogisticsOffers {
		out.LogisticsOfferRefs = append(out.LogisticsOfferRefs, api.SupplyRef{Kind: r.Kind, ID: r.ID, Revision: r.Revision})
	}
	for _, r := range d.FX {
		out.FXRefs = append(out.FXRefs, api.ConfigRef{Kind: r.Kind, ID: r.ID, Revision: r.Revision})
	}
	return out
}

func scenarioView(sc *scenario.Scenario, evaluations []*scenario.Evaluation) api.ScenarioView {
	v := api.ScenarioView{ScenarioID: sc.ID, RootVersion: sc.RootVersion, Status: string(sc.Status), ContentHash: sc.Hash,
		Data: scenarioDataView(sc.Data), Contributors: slices.Clone(sc.Contributors), SavedBy: sc.SavedBy, SavedAt: sc.SavedAt,
		LastEvaluation: sc.LastEvaluation, Evaluations: []api.EvaluationSummary{}}
	for _, e := range evaluations {
		v.Evaluations = append(v.Evaluations, api.EvaluationSummary{EvaluationID: e.ID, Number: e.Number, ScenarioVersion: e.ScenarioVersion,
			EvaluatedBy: e.EvaluatedBy, EvaluatedAt: e.EvaluatedAt, InputHash: e.InputHash, ResultHash: e.ResultHash,
			VerificationStatus: e.Result.VerificationStatus, ComparisonStatus: e.Result.ComparisonStatus, TotalMinor: e.Result.TotalMinor})
	}
	return v
}

// evaluationView is an evaluation as its reader sees it. The result document's JSON names are the
// contract's, so the typed result is read from the stored document itself.
func evaluationView(e *scenario.Evaluation) (api.EvaluationView, error) {
	v := api.EvaluationView{EvaluationID: e.ID, ScenarioID: e.ScenarioID, Number: e.Number, ScenarioVersion: e.ScenarioVersion,
		EvaluatedBy: e.EvaluatedBy, AuthorityEpoch: e.Epoch, EvaluatedAt: e.EvaluatedAt, EngineVersion: e.EngineVersion,
		NormalizationVersion: e.NormalizationVersion, Input: string(e.Input), InputHash: e.InputHash, Document: string(e.Document),
		ResultHash: e.ResultHash, Inputs: boundInputs(e.Inputs)}
	return v, json.Unmarshal(e.Document, &v.Result)
}

// draftFacts are what the owners of a draft's references said about them before the transaction.
type draftFacts struct {
	context bool // the costing context revision exists and is visible
	supply  map[scenario.Ref]bool
	configs map[scenario.Ref]bool
}

// draftFacts asks CST, SUP and POL whether the references of d exist and are visible (Python
// _access_document of a draft, historical: exact revisions readable, effect not judged).
func (s Service) draftFacts(ctx context.Context, d scenario.Data) (draftFacts, error) {
	f := draftFacts{supply: map[scenario.Ref]bool{}, configs: map[scenario.Ref]bool{}}
	if r := d.Context; r != nil {
		err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, dec authorization.Decision) error {
			cc, err := postgres.LoadContext(ctx, exec, scope(ctx), r.ContextID, postgres.NoLock)
			f.context = cc != nil && dec.Sees(cc.OwnerActor, cc.OwnerTeam) && cc.Revision(r.Revision) != nil
			return err
		})
		if err != nil {
			return f, err
		}
	}
	var supply []scenario.Ref
	for _, a := range d.Allocations {
		supply = append(supply, a.Source.Supply())
	}
	supply = append(supply, d.LogisticsOffers...)
	found, err := s.resolveSupply(ctx, supply, s.Clock.Now())
	if err != nil {
		return f, err
	}
	for r := range found {
		f.supply[r] = true
	}
	var configs []scenario.Ref
	for _, a := range d.Allocations {
		configs = append(configs, a.PurchaseUnit)
	}
	configs = append(configs, d.FX...)
	resolved, err := s.resolveConfigs(ctx, configs, s.Clock.Now())
	if err != nil {
		return f, err
	}
	for r := range resolved {
		f.configs[r] = true
	}
	return f, nil
}

// check refuses the first reference of d its owner does not know or the caller does not see.
func (f draftFacts) check(d scenario.Data) error {
	const p = "/args/data"
	if d.Context != nil && !f.context {
		return kernel.Reject(kernel.CodeReferenceInvalid, p+"/context_ref", "")
	}
	for i, a := range d.Allocations {
		ap := index(p+"/allocations", i)
		if !f.supply[a.Source.Supply()] {
			return kernel.Reject(kernel.CodeReferenceInvalid, ap+"/source", "No verified supply condition revision of this kind is visible")
		}
		if !f.configs[a.PurchaseUnit] {
			return kernel.Reject(kernel.CodeReferenceInvalid, ap+"/purchase_unit_ref", "")
		}
	}
	for i, r := range d.LogisticsOffers {
		if !f.supply[r] {
			return kernel.Reject(kernel.CodeReferenceInvalid, index(p+"/logistics_offer_refs", i), "")
		}
	}
	for i, r := range d.FX {
		if !f.configs[r] {
			return kernel.Reject(kernel.CodeReferenceInvalid, index(p+"/fx_refs", i), "")
		}
	}
	return nil
}
