package application

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/scenario"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/snapshot"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Cost snapshots and source attestations (WBS S2-05 second half, S2-06).
//
// cost_snapshot.freeze (human) reads every input of the scenario's draft before CST's transaction,
// as cost_scenario.evaluate does, at the freeze's instant. Inside the transaction, after the
// scenario root is locked FOR UPDATE, it admits the named evaluation (an evaluation of this
// scenario, verified, comparable, computed on its current version, with the result hash the caller
// names), recomputes the evaluation on what it read and requires the same normalized input, engine
// and normalization versions and result (CST-INV-001), locks the snapshot it supersedes FOR UPDATE,
// then re-checks every input through GuardInputs (CST's revisions and the sources' standing FOR
// SHARE, then RfqGuard, ConfigGuard and SupplyConditionGuard), and only then freezes the scenario
// and writes its one snapshot (CST-INV-002), the Outbox event in the same transaction.
//
// costing.attest_source (human) records a verdict on a source: the attesting human reads the source
// and every source of its evidence in EVD (R64), none of the evidence is denied, and a source is
// denied once. A denial updates the source's standing row, which every freeze and every
// AdoptabilityGuard call locks FOR SHARE: a denial that committed first refuses the freeze and
// makes the guard answer denied; one that comes second waits for them.

// Read commands of snapshots and attestations.
const (
	CommandGetSnapshot    = "cost_snapshot.get"
	CommandQuerySnapshot  = "cost_snapshot.query"
	CommandGetAttestation = "source_attestation.get"
)

// snapshotRules: freezing and attesting are human only (05 stage humanOnlyCommands); the reads need
// cst.read.
func snapshotRules() []authorization.Rule {
	return []authorization.Rule{
		authorization.HumanOnly(api.CommandFreezeSnapshot, api.CommandFreezeSnapshot),
		authorization.HumanOnly(api.CommandAttestSource, api.CommandAttestSource),
		{Command: CommandGetSnapshot, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandQuerySnapshot, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandGetAttestation, Permission: PermissionRead, ActorKinds: anyActor},
	}
}

func snapshotCommand(command string) bool {
	return command == api.CommandFreezeSnapshot || command == api.CommandAttestSource
}

// visibleSnapshotResult reports whether d's actor sees what a stored freeze or attestation result
// names: the snapshot through its scenario's owner, the attestation through its attester.
func visibleSnapshotResult(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, id string) (bool, error) {
	if command == api.CommandAttestSource {
		a, err := postgres.LoadAttestation(ctx, exec, scope(ctx), id)
		return err == nil && a != nil && d.Sees(a.By.ID, a.OwnerTeam), err
	}
	s, err := postgres.LoadSnapshot(ctx, exec, scope(ctx), id, postgres.NoLock)
	return err == nil && s != nil && d.Sees(s.OwnerActor, s.OwnerTeam), err
}

// snapshotRequest is request for the snapshot and attestation commands.
func snapshotRequest(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.FreezeSnapshot:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandFreezeSnapshot, key, c
	case api.AttestSource:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandAttestSource, key, c
	}
	return "", "", nil
}

// FreezeSnapshot freezes an evaluation of a draft scenario (Python cost_snapshot.freeze). The
// receipt's ObjectID is the snapshot ID and RowVersion the scenario's frozen root version.
func (s Service) FreezeSnapshot(ctx context.Context, cmd api.FreezeSnapshot) (api.SnapshotReceipt, error) {
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
		if err := scenarioArg(scenario.Check(h.sc, h.visible(d), cmd.ExpectedVersion)); err != nil {
			return action.Outcome{}, err
		}
		sc := scope(ctx)
		e, err := postgres.LoadEvaluation(ctx, exec, sc, cmd.EvaluationID)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = snapshot.CheckEvaluation(h.sc, e, cmd.ResultHash); err != nil {
			return action.Outcome{}, err
		}
		if h.sc.RootVersion != seen {
			// The draft changed after its inputs were read; freezing would judge stale facts.
			return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
		}
		computed, err := scenario.Evaluate(in)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = snapshot.CheckCurrent(e, computed); err != nil {
			return action.Outcome{}, err
		}
		var supersedes string
		if cmd.SupersedesSnapshotID != "" {
			old, err := postgres.LoadSnapshot(ctx, exec, sc, cmd.SupersedesSnapshotID, postgres.ForUpdate)
			if err != nil {
				return action.Outcome{}, err
			}
			var successor string
			if old != nil {
				if successor, err = postgres.Successor(ctx, exec, sc, old.ID); err != nil {
					return action.Outcome{}, err
				}
			}
			if err = snapshot.CheckSupersede(old, old != nil && d.Sees(old.OwnerActor, old.OwnerTeam), successor, e.Result.ContextRef); err != nil {
				return action.Outcome{}, err
			}
			supersedes = old.ID
		}
		now := s.Clock.Now()
		if err = GuardInputs(ctx, exec, d, computed.Input.Inputs, now); err != nil {
			return action.Outcome{}, err
		}
		if err = postgres.FreezeScenario(ctx, exec, sc, h.sc.ID, h.sc.RootVersion, now); err != nil {
			return action.Outcome{}, err
		}
		h.sc.RootVersion, h.sc.Status = h.sc.RootVersion+1, scenario.Frozen
		snap := snapshot.Freeze(action.NewID("snap", now), h.sc, e, cmd.SelectionReason, supersedes, d.Actor, d.Epoch, now)
		if err = postgres.InsertSnapshot(ctx, exec, sc, snap); err != nil {
			return action.Outcome{}, err
		}
		events := []action.Event{{Type: api.EventCostSnapshotFrozen, AggregateType: "CostScenario", AggregateID: h.sc.ID,
			AggregateVersion: h.sc.RootVersion, Fields: map[string]any{"scenario_id": h.sc.ID, "snapshot_id": snap.ID,
				"evaluation_id": e.ID, "input_scenario_version": snap.InputScenarioVersion, "frozen_scenario_version": snap.FrozenScenarioVersion,
				"context_id": snap.Context.ContextID, "context_revision": snap.Context.Revision, "rfq_id": snap.Rfq.RfqID,
				"input_hash": snap.InputHash, "result_hash": snap.ResultHash, "evidence_hash": snap.EvidenceHash,
				"currency": snap.Currency, "total_minor": snap.TotalMinor, "supersedes_snapshot_id": snap.Supersedes}}}
		if supersedes != "" {
			events = append(events, action.Event{Type: api.EventCostSnapshotSuperseded, AggregateType: "CostSnapshot", AggregateID: supersedes,
				AggregateVersion: 1, Fields: map[string]any{"snapshot_id": supersedes, "superseded_by_snapshot_id": snap.ID,
					"context_id": snap.Context.ContextID}})
		}
		return action.Outcome{ObjectID: snap.ID, RowVersion: h.sc.RootVersion, Status: api.ScenarioFrozen, Events: events}, nil
	}
	r, err := s.run(ctx, action.Command{Name: api.CommandFreezeSnapshot, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ScenarioID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.SnapshotReceipt{}, err
	}
	return s.snapshotReceipt(ctx, r)
}

// scenarioArg moves a refusal of the scenario's identity to the freeze's argument scenario_id.
func scenarioArg(err error) error {
	if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) > 0 && rej.Fields[0].Path == "/args/id" {
		out := *rej
		out.Fields = slices.Clone(rej.Fields)
		out.Fields[0].Path = "/args/scenario_id"
		return &out
	}
	return err
}

// snapshotReceipt completes the receipt of a freeze, executed or replayed, from the stored snapshot.
func (s Service) snapshotReceipt(ctx context.Context, r api.Receipt) (api.SnapshotReceipt, error) {
	out := api.SnapshotReceipt{Receipt: r, SnapshotID: r.ObjectID}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		snap, err := postgres.LoadSnapshot(ctx, exec, scope(ctx), r.ObjectID, postgres.NoLock)
		if err != nil || snap == nil {
			return err
		}
		out.ScenarioID, out.EvaluationID = snap.ScenarioID, snap.EvaluationID
		out.InputScenarioVersion, out.FrozenScenarioVersion = snap.InputScenarioVersion, snap.FrozenScenarioVersion
		out.InputHash, out.ResultHash, out.EvidenceHash = snap.InputHash, snap.ResultHash, snap.EvidenceHash
		out.Currency, out.TotalMinor, out.SupersedesSnapshotID = snap.Currency, snap.TotalMinor, snap.Supersedes
		return nil
	})
	return out, err
}

// attestationHandler locks the standing rows of the source a costing.attest_source names and of
// its evidence, and applies the attestation.
type attestationHandler struct {
	s         Service
	cmd       api.AttestSource
	existing  *snapshot.Attestation
	standings map[string]snapshot.Standing
	readable  map[string]bool // the sources the caller reads
}

// Prepare asks EVD which of the target and evidence sources the caller reads.
func (h *attestationHandler) Prepare(ctx context.Context) (err error) {
	h.readable, err = h.s.sources(ctx, append([]string{h.cmd.SourceID}, cited(h.cmd.Evidence)...))
	return err
}

// Lock locks, in source ID order, the target's standing row FOR UPDATE (the row the attestation
// changes) and the evidence's FOR SHARE, creating undisputed rows for sources CST holds none for.
func (h *attestationHandler) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.existing, h.standings = nil, map[string]snapshot.Standing{}
	sc := scope(ctx)
	ids := append([]string{h.cmd.SourceID}, cited(h.cmd.Evidence)...)
	if err := postgres.EnsureSources(ctx, exec, sc, ids, h.s.Clock.Now()); err != nil {
		return err
	}
	for _, id := range slices.Compact(slices.Sorted(slices.Values(ids))) {
		lock := postgres.ForShare
		if id == h.cmd.SourceID {
			lock = postgres.ForUpdate
		}
		found, err := postgres.SourceStandings(ctx, exec, sc, []string{id}, lock)
		if err != nil {
			return err
		}
		h.standings[id] = found[id]
	}
	var err error
	h.existing, err = postgres.LoadAttestation(ctx, exec, sc, h.cmd.AttestationID)
	return err
}

// Apply decides in Python's order: the attestation ID unused (VERSION_CONFLICT), the target readable
// (REFERENCE_INVALID, alike for a source that does not exist), its standing (a denied source is
// final: INVALID_STATE), then the evidence readable (REFERENCE_INVALID) and not denied
// (REFERENCE_STALE).
func (h *attestationHandler) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	cmd := h.cmd
	if h.existing != nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/id", "")
	}
	if !h.readable[cmd.SourceID] {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/source_id", "Source is not accessible")
	}
	standing := h.standings[cmd.SourceID]
	var ev []snapshot.Evidence
	for _, e := range cmd.Evidence {
		ev = append(ev, snapshot.Evidence{SourceID: e.SourceID, Locator: e.Locator})
	}
	now := h.s.Clock.Now()
	a, err := snapshot.Attest(nil, &standing, cmd.AttestationID, cmd.Verdict, cmd.Reason, ev, d.Actor, d.Team, d.Epoch, now)
	if err != nil {
		return action.Outcome{}, err
	}
	if err = readable(cmd.Evidence, h.readable, "/args/evidence"); err != nil {
		return action.Outcome{}, err
	}
	for i, e := range cmd.Evidence {
		if h.standings[e.SourceID].Denied() {
			return action.Outcome{}, api.Reject(api.CodeReferenceStale, index("/args/evidence", i)+"/source_id")
		}
	}
	sc := scope(ctx)
	if err = postgres.RecordAttestation(ctx, exec, sc, a, standing); err != nil {
		return action.Outcome{}, err
	}
	var events []action.Event
	if a.Verdict == snapshot.Denied {
		affected, err := postgres.SnapshotsRestingOn(ctx, exec, sc, cmd.SourceID)
		if err != nil {
			return action.Outcome{}, err
		}
		events = append(events, action.Event{Type: api.EventCostSourceDenied, AggregateType: "CostSnapshot", AggregateID: cmd.SourceID,
			AggregateVersion: standing.RootVersion, Fields: map[string]any{"attestation_id": a.ID, "source_id": cmd.SourceID,
				"snapshot_ids": affected}})
	}
	return action.Outcome{ObjectID: a.ID, RowVersion: 1, Status: a.Verdict, Events: events}, nil
}

// AttestSource records a human's verdict on a source (Python source_denial.record and
// attest_costing_source).
func (s Service) AttestSource(ctx context.Context, cmd api.AttestSource) (api.Receipt, error) {
	h := &attestationHandler{s: s, cmd: cmd}
	return s.run(ctx, action.Command{Name: api.CommandAttestSource, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.AttestationID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
}

// history is what a history read of an evaluation's inputs asked the upstream contexts before its
// transaction.
type history struct {
	bound    []scenario.BoundInput
	readable map[scenario.BoundInput]bool
}

// allowed reports whether d's actor still reads every input of the history (Python
// _history_access): CST's revisions through their roots' owners, the rest as the upstream lookups
// answered.
func (h history) allowed(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (bool, error) {
	ok, err := readableOwn(ctx, exec, d, h.bound)
	if err != nil || !ok {
		return false, err
	}
	for _, b := range h.bound {
		if b.Context != scenario.FromCST && !h.readable[b] {
			return false, nil
		}
	}
	return true, nil
}

// snapshotHistory reads, before the audited read, the inputs of snapshot id's evaluation and which
// of them the caller reads upstream.
func (s Service) snapshotHistory(ctx context.Context, id string) (history, error) {
	var h history
	if err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		snap, err := postgres.LoadSnapshot(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil || snap == nil {
			return err
		}
		e, err := postgres.LoadEvaluation(ctx, exec, scope(ctx), snap.EvaluationID)
		if e != nil {
			h.bound = e.Inputs
		}
		return err
	}); err != nil {
		return h, err
	}
	var err error
	h.readable, err = s.readableUpstream(ctx, h.bound)
	return h, err
}

// loadSnapshotView reads snapshot id as d's actor sees it inside an audited read: NOT_FOUND for a
// snapshot of a scenario the actor does not see (or no snapshot of that ID), PERMISSION_DENIED when
// it no longer reads an input or source of the frozen evaluation, EVIDENCE_HASH_MISMATCH for stored
// evidence that does not hash to its hashes.
func loadSnapshotView(ctx context.Context, exec bentopg.Executor, d authorization.Decision, id string, h history) (api.SnapshotView, error) {
	sc := scope(ctx)
	snap, err := postgres.LoadSnapshot(ctx, exec, sc, id, postgres.NoLock)
	if err != nil {
		return api.SnapshotView{}, err
	}
	if snap == nil || !d.Sees(snap.OwnerActor, snap.OwnerTeam) {
		return api.SnapshotView{}, kernel.Reject(kernel.CodeNotFound, "/id", "")
	}
	ok, err := h.allowed(ctx, exec, d)
	if err != nil {
		return api.SnapshotView{}, err
	}
	if !ok {
		return api.SnapshotView{}, kernel.Reject(kernel.CodePermissionDenied, "/", "")
	}
	e, err := postgres.LoadEvaluation(ctx, exec, sc, snap.EvaluationID)
	if err != nil {
		return api.SnapshotView{}, err
	}
	if err = snap.Verify(e); err != nil {
		return api.SnapshotView{}, err
	}
	successor, err := postgres.Successor(ctx, exec, sc, snap.ID)
	if err != nil {
		return api.SnapshotView{}, err
	}
	var sources []string
	for _, b := range e.Inputs {
		if b.Context == scenario.FromEVD {
			sources = append(sources, b.ID)
		}
	}
	standings, err := postgres.SourceStandings(ctx, exec, sc, sources, postgres.NoLock)
	if err != nil {
		return api.SnapshotView{}, err
	}
	return snapshotView(snap, e, successor, sources, standings)
}

func snapshotView(snap *snapshot.Snapshot, e *scenario.Evaluation, successor string, sources []string, standings map[string]snapshot.Standing) (api.SnapshotView, error) {
	v := api.SnapshotView{SnapshotID: snap.ID, ScenarioID: snap.ScenarioID, EvaluationID: snap.EvaluationID,
		InputScenarioVersion: snap.InputScenarioVersion, FrozenScenarioVersion: snap.FrozenScenarioVersion,
		Context: api.ContextRef{ContextID: snap.Context.ContextID, Revision: snap.Context.Revision},
		Rfq:     api.RfqBinding{RfqID: snap.Rfq.RfqID, RowVersion: snap.Rfq.RowVersion, LinesHash: snap.Rfq.LinesHash}, EngineVersion: snap.EngineVersion,
		NormalizationVersion: snap.NormalizationVersion, InputHash: snap.InputHash, ResultHash: snap.ResultHash, EvidenceHash: snap.EvidenceHash,
		SelectionReason: snap.SelectionReason, Currency: snap.Currency, TotalMinor: snap.TotalMinor, SupersedesSnapshotID: snap.Supersedes,
		SupersededBySnapshotID: successor, FrozenBy: snap.FrozenBy, AuthorityEpoch: snap.Epoch, FrozenAt: snap.FrozenAt,
		Document: string(snap.Document), Input: string(e.Input), Inputs: boundInputs(e.Inputs), Sources: []api.SourceStanding{}}
	for _, id := range sources {
		st := api.SourceStanding{SourceID: id, Status: api.SourceUndisputed}
		if s, ok := standings[id]; ok && s.Denied() {
			st.Status, st.DenialID, st.DeniedAt = api.SourceDenied, s.DenialID, s.DeniedAt
		}
		v.Sources = append(v.Sources, st)
	}
	return v, json.Unmarshal(e.Document, &v.Result)
}

// GetSnapshot returns a snapshot with its provenance, audited (Python costing.get of kind
// snapshot).
func (s Service) GetSnapshot(ctx context.Context, id string) (api.SnapshotView, error) {
	h, err := s.snapshotHistory(ctx, id)
	if err != nil {
		return api.SnapshotView{}, err
	}
	var v api.SnapshotView
	err = s.Ledger.Read(ctx, CommandGetSnapshot, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		var err error
		v, err = loadSnapshotView(ctx, exec, d, id, h)
		return err
	})
	return v, err
}

// QuerySnapshot returns a snapshot and whether it is currently eligible for a new quote (Python
// snapshot_query), as AdoptabilityGuard v1 decides it now inside the audited read: the verdict is
// what a downstream transaction starting now would see.
func (s Service) QuerySnapshot(ctx context.Context, id string) (api.SnapshotQuery, error) {
	h, err := s.snapshotHistory(ctx, id)
	if err != nil {
		return api.SnapshotQuery{}, err
	}
	var q api.SnapshotQuery
	err = s.Ledger.Read(ctx, CommandQuerySnapshot, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		v, err := loadSnapshotView(ctx, exec, d, id, h)
		if err != nil {
			return err
		}
		at := kernel.Instant(s.Clock.Now())
		a, err := adoptability(ctx, exec, api.CostSnapshotRef{SnapshotID: v.SnapshotID, ResultHash: v.ResultHash}, at)
		q = api.SnapshotQuery{Snapshot: v, Adoptability: a, CurrentlyEligibleForQuote: a.Adoptable(), EligibilityCheckedAt: at}
		return err
	})
	return q, err
}

// adoptability runs AdoptabilityGuard v1 for ref at at with exec.
func adoptability(ctx context.Context, exec bentopg.Executor, ref api.CostSnapshotRef, at time.Time) (api.Adoptability, error) {
	v, err := api.ScanAdoptabilityGuard(ref, exec.QueryRow(ctx, api.AdoptabilityGuardSQL, api.AdoptabilityGuardArgs(ref, at)...).Scan)
	if err != nil {
		return api.Adoptability{}, err
	}
	if !v.Known {
		return api.Adoptability{}, errors.New("cst: the adoptability guard does not know a snapshot CST just read")
	}
	return api.Adoptability{Status: v.Status, Code: v.Code, Input: v.Input, CheckedAt: kernel.Instant(at)}, nil
}

// GetAttestation returns an attestation the caller sees, audited.
func (s Service) GetAttestation(ctx context.Context, id string) (api.AttestationView, error) {
	var v api.AttestationView
	err := s.Ledger.Read(ctx, CommandGetAttestation, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		a, err := postgres.LoadAttestation(ctx, exec, scope(ctx), id)
		if err != nil {
			return err
		}
		if a == nil || !d.Sees(a.By.ID, a.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.AttestationView{AttestationID: a.ID, SourceID: a.SourceID, Verdict: a.Verdict, Reason: a.Reason, AttestedBy: a.By,
			AuthorityEpoch: a.Epoch, AttestedAt: a.At, Evidence: []api.Evidence{}}
		for _, e := range a.Evidence {
			v.Evidence = append(v.Evidence, api.Evidence{SourceID: e.SourceID, Locator: e.Locator})
		}
		return nil
	})
	return v, err
}

// ResolveSnapshots answers CostSnapshotRef v1 lookups (api.SnapshotLookups) at instant at: the
// snapshots the caller sees whose result hash is the reference's, with their adoptability then.
func (s Service) ResolveSnapshots(ctx context.Context, refs []api.CostSnapshotRef, at time.Time) (map[api.CostSnapshotRef]api.ResolvedSnapshot, error) {
	out := map[api.CostSnapshotRef]api.ResolvedSnapshot{}
	if len(refs) > api.MaxSnapshotLookup {
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/refs", "")
	}
	for i, r := range refs {
		if err := r.Validate(index("/refs", i)); err != nil {
			return nil, err
		}
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		for _, r := range refs {
			snap, err := postgres.LoadSnapshot(ctx, exec, sc, r.SnapshotID, postgres.NoLock)
			if err != nil {
				return err
			}
			if snap == nil || !d.Sees(snap.OwnerActor, snap.OwnerTeam) || (r.ResultHash != "" && r.ResultHash != snap.ResultHash) {
				continue
			}
			e, err := postgres.LoadEvaluation(ctx, exec, sc, snap.EvaluationID)
			if err != nil {
				return err
			}
			var result api.EvaluationResult
			if err = json.Unmarshal(e.Document, &result); err != nil {
				return err
			}
			a, err := adoptability(ctx, exec, r, at)
			if err != nil {
				return err
			}
			out[r] = api.ResolvedSnapshot{Ref: r, ScenarioID: snap.ScenarioID, Context: api.ContextRef{ContextID: snap.Context.ContextID,
				Revision: snap.Context.Revision}, Rfq: api.RfqBinding{RfqID: snap.Rfq.RfqID, RowVersion: snap.Rfq.RowVersion,
				LinesHash: snap.Rfq.LinesHash}, EvidenceHash: snap.EvidenceHash, Currency: snap.Currency, TotalMinor: snap.TotalMinor,
				Lines: result.Lines, Components: result.Components, Adoptability: a}
		}
		return nil
	})
	return out, err
}
