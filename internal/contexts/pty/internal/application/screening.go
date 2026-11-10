package application

import (
	"context"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/lifecycle"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/screening"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// Read commands of party screening; both need pty.read.
const (
	CommandGetScreening  = "party_screening.get"
	CommandScreeningsDue = "party_screening.due"
)

func scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

// screeningCommand locks a party's screening root FOR UPDATE and applies one screening command;
// the party itself is then held FOR SHARE, after the authority guard (ADR-BR-011).
type screeningCommand struct {
	s       Service
	partyID string
	root    *screening.Root
	prepare func(ctx context.Context) error
	apply   func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *screeningCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.root, err = postgres.LoadScreeningRoot(ctx, exec, scope(ctx), h.partyID, postgres.ForUpdate)
	return err
}

func (h *screeningCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

// party holds the party FOR SHARE and returns it as screened; NOT_FOUND when the actor does not see it.
func (h *screeningCommand) party(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (*screening.Party, error) {
	p, err := postgres.LoadParty(ctx, exec, scope(ctx), h.partyID, postgres.ForShare)
	if err != nil {
		return nil, err
	}
	if p == nil || !d.Sees(p.OwnerActor, p.OwnerTeam) {
		return nil, kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	}
	return &screening.Party{ID: p.ID, Revision: p.Revision, Name: p.Name, Confirmed: p.Status == lifecycle.Confirmed}, nil
}

// save stores the root moved from version from and returns the outcome with event.
func (h *screeningCommand) save(ctx context.Context, exec bentopg.Executor, from int64, event string, fields map[string]any) (action.Outcome, error) {
	if err := postgres.SaveScreeningRoot(ctx, exec, scope(ctx), h.root, from, h.s.Clock.Now()); err != nil {
		return action.Outcome{}, err
	}
	status := string(h.root.Status)
	if h.root.Negative {
		status = api.ScreeningBlocked
	}
	fields["party_id"], fields["row_version"], fields["status"] = h.root.PartyID, h.root.RowVersion, status
	return action.Outcome{ObjectID: h.root.PartyID, RowVersion: h.root.RowVersion, Status: status, Events: []action.Event{{
		Type: event, AggregateType: "PartyScreening", AggregateID: h.root.PartyID, AggregateVersion: h.root.RowVersion, Fields: fields}}}, nil
}

// preparedScreening gives screeningCommand the Preparer step of the ledger.
type preparedScreening struct{ *screeningCommand }

func (p *preparedScreening) Prepare(ctx context.Context) error { return p.prepare(ctx) }

// recordFacts is what a screening reads before PTY's transaction: the party as stored, the
// regulation from POL (R60) and, from reference data, the current versions of the enabled lists and
// the entries the party's name matches in them.
type recordFacts struct {
	party    *screening.Party
	resolved *polapi.ResolvedConfig
	versions map[string]refapi.ListVersion
	hits     []refapi.Hit
}

func (s Service) recordFacts(ctx context.Context, cmd api.RecordScreening) (recordFacts, error) {
	var f recordFacts
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		p, err := postgres.LoadParty(ctx, exec, scope(ctx), cmd.PartyID, postgres.NoLock)
		if err == nil && p != nil && d.Sees(p.OwnerActor, p.OwnerTeam) {
			f.party = &screening.Party{ID: p.ID, Revision: p.Revision, Name: p.Name, Confirmed: p.Status == lifecycle.Confirmed}
		}
		return err
	})
	if err != nil || f.party == nil || !f.party.Confirmed || !refapi.Name(f.party.Name) {
		return f, err
	}
	ref := polapi.ConfigRef{Kind: polapi.KindScreeningRegulation, ConfigID: cmd.RegulationID, Revision: cmd.RegulationRevision}
	resolved, err := s.Configs.ResolveConfigs(ctx, []polapi.ConfigRef{ref}, s.Clock.Now())
	if err != nil {
		return f, err
	}
	r, ok := resolved[ref]
	if !ok || r.Code != "" || r.Data.ScreeningRegulation == nil {
		if ok {
			f.resolved = &r
		}
		return f, nil
	}
	f.resolved = &r
	var codes []string
	for _, l := range r.Data.ScreeningRegulation.Lists {
		codes = append(codes, l.ListCode)
	}
	if f.versions, err = s.Lists.CurrentVersions(ctx, codes); err != nil {
		return f, err
	}
	var refs []refapi.ListVersionRef
	for _, code := range codes {
		if v, ok := f.versions[code]; ok {
			refs = append(refs, refapi.ListVersionRef{ListCode: code, Version: v.Version})
		}
	}
	f.hits, err = s.Lists.Match(ctx, refs, []string{f.party.Name})
	return f, err
}

// regulation judges the regulation revision at instant at: it must exist as a screening_regulation
// of the company and be in force (POL's verdict, CONFIG_NOT_ACTIVE or CONFIG_EXPIRED).
func (f recordFacts) regulation(at time.Time) (screening.Regulation, error) {
	const path = "/args/regulation_id"
	r := f.resolved
	if r == nil || r.Data.ScreeningRegulation == nil {
		return screening.Regulation{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	if err := r.Check(path); err != nil {
		return screening.Regulation{}, err
	}
	if !(kernel.ValidityInterval{From: r.ValidFrom, Until: r.ValidUntil}).Contains(at) {
		return screening.Regulation{}, polapi.Reject(polapi.CodeConfigExpired, path)
	}
	reg := screening.Regulation{ID: r.Ref.ConfigID, Revision: r.Ref.Revision, Hash: r.ContentHash,
		RescreenDays: r.Data.ScreeningRegulation.RescreenIntervalDays, ValidUntil: r.ValidUntil}
	for _, l := range r.Data.ScreeningRegulation.Lists {
		reg.Lists = append(reg.Lists, screening.EnabledList{ListCode: l.ListCode, Jurisdiction: l.Jurisdiction})
	}
	return reg, nil
}

// RecordScreening screens a party's confirmed revision (PTY-INV-002).
func (s Service) RecordScreening(ctx context.Context, cmd api.RecordScreening) (api.Receipt, error) {
	h := &screeningCommand{s: s, partyID: cmd.PartyID}
	var f recordFacts
	h.prepare = func(ctx context.Context) (err error) {
		f, err = s.recordFacts(ctx, cmd)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		party, err := h.party(ctx, exec, d)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = screening.CheckVersion(h.root, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if err = screening.CheckScreenable(party); err != nil {
			return action.Outcome{}, err
		}
		if f.party == nil || *f.party != *party {
			return action.Outcome{}, kernel.RejectAs(api.CodeReferenceStale, kernel.CategoryConflict, "/args/id", "The party changed while it was screened")
		}
		if !refapi.Name(party.Name) {
			// A name without a letter or digit has no key under the normalization and would match
			// nothing; it is never reported clear.
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/id", "The party's name cannot be screened")
		}
		now := s.Clock.Now()
		reg, err := f.regulation(now)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = h.listsStillCurrent(ctx, exec, reg, f.versions); err != nil {
			return action.Outcome{}, err
		}
		in := screening.Input{ID: action.NewID("scr", now), Party: *party, Regulation: reg, Versions: map[string]screening.ListVersion{},
			Normalization: refapi.Normalization, By: d.Actor, Epoch: d.Epoch, At: now}
		for code, v := range f.versions {
			in.Versions[code] = screening.ListVersion{ListCode: code, Jurisdiction: v.Jurisdiction, Version: v.Version, ContentHash: v.ContentHash}
		}
		for _, hit := range f.hits {
			in.Hits = append(in.Hits, screening.Hit{ListCode: hit.ListCode, Version: hit.Version, EntryID: hit.Entry.EntryID,
				EntryHash: hit.Entry.ContentHash, PrimaryName: hit.Entry.PrimaryName, MatchedName: hit.MatchedName, NameKey: hit.NameKey})
		}
		sc := scope(ctx)
		if in.Positives, err = postgres.Positives(ctx, exec, sc, cmd.PartyID); err != nil {
			return action.Outcome{}, err
		}
		if in.Confirmed, err = postgres.ConfirmedMatches(ctx, exec, sc, cmd.PartyID); err != nil {
			return action.Outcome{}, err
		}
		res, err := screening.Screen(in)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = postgres.InsertScreeningResult(ctx, exec, sc, res); err != nil {
			return action.Outcome{}, err
		}
		var from int64
		if h.root == nil {
			h.root = &screening.Root{PartyID: cmd.PartyID}
		} else {
			from = h.root.RowVersion
		}
		h.root.Record(res)
		lists := make([]map[string]any, len(res.Lists))
		for i, l := range res.Lists {
			lists[i] = map[string]any{"list_code": l.ListCode, "version": l.Version}
		}
		return h.save(ctx, exec, from, api.EventPartyScreeningRecorded, map[string]any{"screening_id": res.ID, "party_revision": res.PartyRevision,
			"outcome": string(res.Outcome), "regulation_id": reg.ID, "regulation_revision": reg.Revision, "lists": lists,
			"missing_lists": res.Missing, "hits": len(res.Hits), "valid_until": kernel.FormatTimestamp(res.ValidUntil)})
	}
	return s.screeningRun(ctx, api.CommandRecordScreening, cmd.IdempotencyKey, cmd.PartyID, "",
		struct {
			ID, Regulation string
			Revision       int64
		}{cmd.PartyID, cmd.RegulationID, cmd.RegulationRevision}, cmd.Validate(), &preparedScreening{h})
}

// listsStillCurrent holds every enabled list FOR SHARE through reference data's guard, in list-code
// order, and requires the versions the screening matched against to be the current ones: an import
// that committed after the facts were read makes the screening stale.
func (h *screeningCommand) listsStillCurrent(ctx context.Context, exec bentopg.Executor, reg screening.Regulation, versions map[string]refapi.ListVersion) error {
	codes := make([]string, len(reg.Lists))
	for i, l := range reg.Lists {
		codes[i] = l.ListCode
	}
	slices.Sort(codes)
	for _, code := range codes {
		var current *string
		if err := exec.QueryRow(ctx, refapi.ScreeningListGuardSQL, code).Scan(&current); err != nil {
			return err
		}
		v, ok := versions[code]
		if (current == nil) != !ok || (current != nil && *current != v.Version) {
			return kernel.RejectAs(api.CodeReferenceStale, kernel.CategoryConflict, "/args/regulation_id", "Screening list "+code+" changed while the party was screened")
		}
	}
	return nil
}

// DecideScreening decides the party's current screening, a suspected match (human only).
func (s Service) DecideScreening(ctx context.Context, cmd api.DecideScreening) (api.Receipt, error) {
	h := &screeningCommand{s: s, partyID: cmd.PartyID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		party, err := h.party(ctx, exec, d)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = screening.CheckVersion(h.root, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		sc := scope(ctx)
		res := &screening.Result{}
		if h.root.Current != "" {
			stored, err := postgres.ScreeningResults(ctx, exec, sc, cmd.PartyID, h.root.Current)
			if err != nil {
				return action.Outcome{}, err
			}
			res = stored[0].Result
		}
		now, from := s.Clock.Now(), h.root.RowVersion
		positives, err := h.root.Decide(res, party, cmd.ScreeningID, screening.Decision(cmd.Decision), cmd.RegisterPositive,
			func() string { return action.NewID("pos", now) }, cmd.Reason, d.Actor, d.Epoch, now)
		if err != nil {
			return action.Outcome{}, err
		}
		err = postgres.InsertDecision(ctx, exec, sc, res, postgres.Decision{Decision: screening.Decision(cmd.Decision), Reason: cmd.Reason,
			By: d.Actor, Epoch: d.Epoch, At: now}, positives)
		if err != nil {
			return action.Outcome{}, err
		}
		ids := make([]string, len(positives))
		for i, p := range positives {
			ids[i] = p.ID
		}
		return h.save(ctx, exec, from, api.EventPartyScreeningDecided, map[string]any{"screening_id": res.ID, "decision": cmd.Decision,
			"party_revision": res.PartyRevision, "positive_ids": ids})
	}
	return s.screeningRun(ctx, api.CommandDecideScreening, cmd.IdempotencyKey, cmd.PartyID, cmd.Reason,
		struct {
			ID, Screening, Decision, Reason string
			Register                        bool
		}{cmd.PartyID, cmd.ScreeningID, cmd.Decision, cmd.Reason, cmd.RegisterPositive}, cmd.Validate(), h)
}

// BlockParty puts a party on the negative list for good (human only).
func (s Service) BlockParty(ctx context.Context, cmd api.BlockParty) (api.Receipt, error) {
	h := &screeningCommand{s: s, partyID: cmd.PartyID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if _, err := h.party(ctx, exec, d); err != nil {
			return action.Outcome{}, err
		}
		if err := screening.CheckVersion(h.root, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		var from int64
		if h.root == nil {
			h.root = &screening.Root{PartyID: cmd.PartyID}
		} else {
			from = h.root.RowVersion
		}
		if err := h.root.Block(); err != nil {
			return action.Outcome{}, err
		}
		err := postgres.InsertNegative(ctx, exec, scope(ctx), cmd.PartyID, postgres.Negative{Reason: cmd.Reason, By: d.Actor, Epoch: d.Epoch,
			At: s.Clock.Now()})
		if err != nil {
			return action.Outcome{}, err
		}
		return h.save(ctx, exec, from, api.EventPartyScreeningBlocked, map[string]any{})
	}
	return s.screeningRun(ctx, api.CommandBlockParty, cmd.IdempotencyKey, cmd.PartyID, cmd.Reason,
		struct{ ID, Reason string }{cmd.PartyID, cmd.Reason}, cmd.Validate(), h)
}

func (s Service) screeningRun(ctx context.Context, command, key, partyID, reason string, args any, validation error, h action.Handler) (api.Receipt, error) {
	return s.run(ctx, action.Command{Name: command, IdempotencyKey: key, ObjectID: partyID, Reason: reason}, args, validation, h)
}

// GetScreening returns a party's screening, audited.
func (s Service) GetScreening(ctx context.Context, partyID string) (api.ScreeningView, error) {
	var v api.ScreeningView
	err := s.Ledger.Read(ctx, CommandGetScreening, partyID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		p, err := postgres.LoadParty(ctx, exec, sc, partyID, postgres.NoLock)
		if err != nil {
			return err
		}
		if p == nil || !d.Sees(p.OwnerActor, p.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.ScreeningView{PartyID: partyID, Screenings: []api.ScreeningResultView{}, PositiveEntries: []api.PositiveEntryView{}}
		root, err := postgres.LoadScreeningRoot(ctx, exec, sc, partyID, postgres.NoLock)
		if err != nil {
			return err
		}
		if root != nil {
			v.RowVersion, v.CurrentScreeningID = root.RowVersion, root.Current
		}
		state, err := postgres.ScreeningState(ctx, exec, partyID, s.Clock.Now())
		if err != nil {
			return err
		}
		v.Status, v.Reason = state.Status, state.Reason
		results, err := postgres.ScreeningResults(ctx, exec, sc, partyID, "")
		if err != nil {
			return err
		}
		for _, r := range results {
			v.Screenings = append(v.Screenings, resultView(r))
		}
		positives, err := postgres.Positives(ctx, exec, sc, partyID)
		if err != nil {
			return err
		}
		for _, pe := range positives {
			v.PositiveEntries = append(v.PositiveEntries, api.PositiveEntryView{PositiveID: pe.ID, PartyRevision: pe.PartyRevision,
				ListCode: pe.ListCode, EntryID: pe.EntryID, EntryContentHash: pe.EntryHash, ScreeningID: pe.ScreeningID, Reason: pe.Reason,
				RegisteredBy: pe.By, RegisteredAt: pe.At, AppliesToRevision: p.Status == lifecycle.Confirmed && pe.PartyRevision == p.Revision})
		}
		negative, err := postgres.LoadNegative(ctx, exec, sc, partyID)
		if negative != nil {
			v.Negative = &api.NegativeEntryView{Reason: negative.Reason, BlockedBy: negative.By, BlockedAt: negative.At, AuthorityEpoch: negative.Epoch}
		}
		return err
	})
	return v, err
}

func resultView(r postgres.StoredResult) api.ScreeningResultView {
	v := api.ScreeningResultView{ScreeningID: r.ID, PartyRevision: r.PartyRevision, ScreenedName: r.Name, RegulationID: r.Regulation.ID,
		RegulationRevision: r.Regulation.Revision, RegulationHash: r.Regulation.Hash, Outcome: string(r.Outcome), Status: string(r.Status),
		Lists: []api.ScreeningListUsed{}, MissingLists: r.Missing, Hits: []api.ScreeningHit{}, Normalization: r.Normalization,
		ScreenedBy: r.By, ScreenedAt: r.At, ValidUntil: r.ValidUntil, AuthorityEpoch: r.Epoch}
	if v.MissingLists == nil {
		v.MissingLists = []string{}
	}
	for _, l := range r.Lists {
		v.Lists = append(v.Lists, api.ScreeningListUsed{ListCode: l.ListCode, Jurisdiction: l.Jurisdiction, Version: l.Version, ContentHash: l.ContentHash})
	}
	for _, h := range r.Hits {
		v.Hits = append(v.Hits, api.ScreeningHit{ListCode: h.ListCode, Version: h.Version, EntryID: h.EntryID, EntryContentHash: h.EntryHash,
			PrimaryName: h.PrimaryName, MatchedName: h.MatchedName, NameKey: h.NameKey, Disposition: string(h.Disposition), PositiveID: h.PositiveID})
	}
	if d := r.Decision; d != nil {
		v.Decision = &api.ScreeningDecisionView{Decision: string(d.Decision), Reason: d.Reason, DecidedBy: d.By, DecidedAt: d.At, AuthorityEpoch: d.Epoch}
	}
	return v
}

// dueReasons are the states a new screening changes.
var dueReasons = []string{api.ReasonNotScreened, api.ReasonPartyChanged, api.ReasonExpired, api.ReasonListUpdated}

// ScreeningsDue lists the confirmed parties whose screening is due at instant at (derived, never
// written: a list import or a new party revision makes conclusions due without touching them).
func (s Service) ScreeningsDue(ctx context.Context, at time.Time) ([]api.ScreeningDue, error) {
	if at.IsZero() {
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/at", "")
	}
	at = kernel.Instant(at)
	var rows []postgres.Due
	err := s.Ledger.Read(ctx, CommandScreeningsDue, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		all, err := postgres.ScreeningStates(ctx, exec, scope(ctx), at)
		for _, r := range all {
			if d.Sees(r.OwnerActor, r.OwnerTeam) && r.Reason != api.ReasonNegativeList {
				rows = append(rows, r)
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	// The regulation revision of a standing conclusion must still be in force; POL answers after
	// PTY's read, in a transaction of its own.
	var refs []polapi.ConfigRef
	for _, r := range rows {
		ref := polapi.ConfigRef{Kind: polapi.KindScreeningRegulation, ConfigID: r.RegulationID, Revision: r.RegulationRevision}
		if r.RegulationID != "" && !slices.Contains(dueReasons, r.Reason) && !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	inForce := map[polapi.ConfigRef]bool{}
	for start := 0; start < len(refs); start += polapi.MaxRefs {
		chunk := refs[start:min(start+polapi.MaxRefs, len(refs))]
		resolved, err := s.Configs.ResolveConfigs(ctx, chunk, at)
		if err != nil {
			return nil, err
		}
		for ref, r := range resolved {
			inForce[ref] = r.Code == ""
		}
	}
	out := []api.ScreeningDue{}
	for _, r := range rows {
		due := api.ScreeningDue{PartyID: r.PartyID, PartyRevision: r.Revision, RowVersion: r.RowVersion, Status: r.Status, Reason: r.Reason,
			RegulationID: r.RegulationID, RegulationRevision: r.RegulationRevision}
		ref := polapi.ConfigRef{Kind: polapi.KindScreeningRegulation, ConfigID: r.RegulationID, Revision: r.RegulationRevision}
		switch {
		case slices.Contains(dueReasons, r.Reason):
		case r.RegulationID != "" && !inForce[ref]:
			due.Status, due.Reason = api.ScreeningPending, api.ReasonRegulationChanged
		default:
			continue
		}
		out = append(out, due)
	}
	return out, nil
}
