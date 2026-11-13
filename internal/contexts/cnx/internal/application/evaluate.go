package application

import (
	"context"
	"slices"
	"strconv"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	tcc "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/tradecompliancecheck"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// trade_compliance.evaluate (human, AI or system; WBS S5-13; CNX-INV-001, D-8).
//
// Before CNX's transaction, Prepare reads the order at its current revision (TRD, R46), the
// countries of its addresses (PTY, R77), the declaration profiles of its lines' specifications
// under the code table version the command states (CAT, R39: the revision verified now; no guard,
// so a revision verified anew before CNX commits leaves the evaluation on the one it read, recorded
// with its revision and content hash), the screening_regulation revision (POL, R50), the current versions of the programme
// lists named and their programmes naming the countries found or entered (reference data) and the
// evidence sources (EVD, R54).
//
// In the transaction (tactical lockOrder, ADR-BR-011): the check of the order revision FOR UPDATE
// (the handler's Lock); the authority guard FOR SHARE (the ledger); then, in this order, the
// refusals: an order the caller does not see, or a check it does not see (NOT_FOUND); the check's
// row version (VERSION_CONFLICT); a sealed order revision that is not intact (REFERENCE_INVALID);
// another current revision of the order (REFERENCE_STALE); an order that ended (ORDER_NOT_ACTIVE); a
// clear conclusion (INVALID_STATE: reopen it first); the regulation (REFERENCE_INVALID, POL's
// CONFIG_NOT_ACTIVE or CONFIG_EXPIRED, re-checked by POL's ConfigGuard v1 FOR SHARE); a foreign
// programme list (INVALID_KNOWN_VALUE: ADR-BR-017 needs the regulation's legal confirmation, which
// POL cannot record for programme lists yet); every list held FOR SHARE through reference data's
// list guard in list-code order and still at the version read (REFERENCE_STALE otherwise); the
// evidence (REFERENCE_INVALID); the entered countries (domain). The evaluation is written with the
// check; no event (an internal step without consumers, S3-D09).

type evaluateFacts struct {
	order      *Order
	countries  map[AddressRef]AddressCountry
	profiles   map[ProfileRef]tcc.Profile
	regulation *polapi.ResolvedConfig
	programmes ProgrammeAnswer
	sources    map[string]evdapi.SourceRef
}

type evaluateCommand struct {
	s     Service
	cmd   api.Evaluate
	check *tcc.Check
	f     evaluateFacts
}

func (h *evaluateCommand) evidenceIDs() []string {
	var ids []string
	if d := h.cmd.Destination; d != nil {
		ids = append(ids, d.EvidenceIDs...)
	}
	for _, a := range h.cmd.AddressCountries {
		ids = append(ids, a.EvidenceIDs...)
	}
	if e := h.cmd.EndUse; e != nil {
		ids = append(ids, e.EvidenceIDs...)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// addresses are the order's addresses with what PTY says of them.
func (f evaluateFacts) addresses() []tcc.Address {
	var out []tcc.Address
	for _, a := range f.order.Addresses {
		addr := tcc.Address{Role: a.Role, PartyID: a.PartyID, PartyRevision: a.PartyRevision, AddressID: a.AddressID}
		if c, ok := f.countries[AddressRef{PartyID: a.PartyID, PartyRevision: a.PartyRevision, AddressID: a.AddressID}]; ok {
			addr.Known, addr.Country = true, c.Country
			if addr.Role == "" {
				addr.Role = c.Role
			}
		}
		out = append(out, addr)
	}
	return out
}

// entered are the countries the command enters.
func (h *evaluateCommand) entered() (*tcc.Entered, []tcc.Entered) {
	var dest *tcc.Entered
	if d := h.cmd.Destination; d != nil {
		dest = &tcc.Entered{Country: d.Country, Reason: d.Reason}
	}
	var out []tcc.Entered
	for _, a := range h.cmd.AddressCountries {
		out = append(out, tcc.Entered{PartyID: a.PartyID, AddressID: a.AddressID, Country: a.Country})
	}
	return dest, out
}

func (h *evaluateCommand) Prepare(ctx context.Context) error {
	h.f = evaluateFacts{}
	u := h.s.Upstream
	orders, err := u.Orders.ResolveOrders(ctx, []string{h.cmd.OrderID})
	if err != nil {
		return err
	}
	o, ok := orders[h.cmd.OrderID]
	if !ok || o.Code != "" {
		if ok {
			h.f.order = &o
		}
		return nil
	}
	h.f.order = &o
	var refs []AddressRef
	for _, a := range o.Addresses {
		refs = append(refs, AddressRef{PartyID: a.PartyID, PartyRevision: a.PartyRevision, AddressID: a.AddressID})
	}
	if h.f.countries, err = u.Countries.ResolveAddressCountries(ctx, refs); err != nil {
		return err
	}
	var keys []ProfileRef
	for _, l := range o.Lines {
		if k := (ProfileRef{SkuID: l.SkuID, SpecRevision: l.SpecRevision, CodeTableVersion: h.cmd.CodeTableVersion}); !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	if h.f.profiles, err = u.Profiles.ResolveProfiles(ctx, keys); err != nil {
		return err
	}
	ref := polapi.ConfigRef{Kind: polapi.KindScreeningRegulation, ConfigID: h.cmd.RegulationID, Revision: h.cmd.RegulationRevision}
	resolved, err := u.Configs.ResolveConfigs(ctx, []polapi.ConfigRef{ref}, h.s.Clock.Now())
	if err != nil {
		return err
	}
	if r, ok := resolved[ref]; ok {
		h.f.regulation = &r
	}
	dest, entered := h.entered()
	if h.f.programmes, err = u.Programmes.ResolveProgrammes(ctx, h.cmd.ProgrammeLists, tcc.Countries(h.f.addresses(), dest, entered)); err != nil {
		return err
	}
	h.f.sources, err = h.s.sources(ctx, h.evidenceIDs())
	return err
}

func (h *evaluateCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.check, err = postgres.LoadCheckOfRevision(ctx, exec, scope(ctx), h.cmd.OrderID, h.cmd.OrderRevision, postgres.ForUpdate)
	return err
}

func evidenceOf(sources map[string]evdapi.SourceRef, ids []string, path string) ([]tcc.Evidence, error) {
	out := []tcc.Evidence{}
	for i, id := range ids {
		s, ok := sources[id]
		if !ok {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, path+"/"+strconv.Itoa(i), "No such source")
		}
		out = append(out, tcc.Evidence{SourceID: s.SourceID, ContentSHA256: s.ContentSHA256})
	}
	return out, nil
}

// listsStillCurrent holds every list in codes FOR SHARE through reference data's list guard, in
// list-code order, and requires the version read before the transaction (absent: none imported) to
// be the current one: an import that committed in between makes the evaluation stale.
func listsStillCurrent(ctx context.Context, exec bentopg.Executor, codes []string, versions map[string]tcc.ListVersion, path string) error {
	stale, err := outdatedLists(ctx, exec, codes, versions)
	if err == nil && len(stale) > 0 {
		err = api.Reject(api.CodeReferenceStale, path, "Programme list "+stale[0]+" has another current version")
	}
	return err
}

// outdatedLists holds the lists of codes FOR SHARE through reference data's list guard and returns,
// in list-code order, those whose current version is not the version in versions; a list without
// a version in versions is outdated when one was imported since.
func outdatedLists(ctx context.Context, exec bentopg.Executor, codes []string, versions map[string]tcc.ListVersion) ([]string, error) {
	sorted := slices.Clone(codes)
	slices.Sort(sorted)
	var out []string
	for _, code := range slices.Compact(sorted) {
		var current *string
		if err := exec.QueryRow(ctx, refapi.CountryProgrammeListGuardSQL, code).Scan(&current); err != nil {
			return nil, err
		}
		v, ok := versions[code]
		if (current == nil) != !ok || (current != nil && *current != v.Version) {
			out = append(out, code)
		}
	}
	return out, nil
}

func (h *evaluateCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, f, cmd := scope(ctx), h.f, h.cmd
	if f.order == nil || (h.check != nil && !d.Sees(h.check.OwnerActor, h.check.OwnerTeam)) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/order_id", "")
	}
	if err := tcc.CheckVersion(h.check, cmd.ExpectedVersion); err != nil {
		return action.Outcome{}, err
	}
	o := f.order
	if o.Code != "" {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "The order's sealed revision is not intact")
	}
	if o.Binding.OrderRevision != cmd.OrderRevision || (h.check != nil && h.check.Binding != o.Binding) {
		return action.Outcome{}, api.Reject(api.CodeReferenceStale, "/args/order_revision", "The order is at another revision")
	}
	if o.Status != OrderDraft && o.Status != OrderActive {
		return action.Outcome{}, api.Reject(api.CodeOrderNotActive, "/args/order_id", "The order is "+o.Status)
	}
	if h.check != nil && h.check.Status == tcc.Clear {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/order_revision", "A clear conclusion is reopened before it is evaluated again")
	}
	const regPath = "/args/regulation_id"
	reg, err := regulationOf(f.regulation, regPath)
	if err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	if !(kernel.ValidityInterval{From: f.regulation.ValidFrom, Until: f.regulation.ValidUntil}).Contains(now) {
		return action.Outcome{}, polapi.Reject(polapi.CodeConfigExpired, regPath)
	}
	if err = regulationGuard(ctx, exec, f.regulation.Ref, f.regulation.ContentHash, now, regPath); err != nil {
		return action.Outcome{}, err
	}
	for i, code := range cmd.ProgrammeLists {
		if v, ok := f.programmes.Versions[code]; ok && v.Jurisdiction == refapi.JurisdictionForeign {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/programme_lists/"+strconv.Itoa(i),
				"A foreign programme list needs the screening_regulation's legal confirmation (ADR-BR-017)")
		}
	}
	if err = listsStillCurrent(ctx, exec, cmd.ProgrammeLists, f.programmes.Versions, "/args/programme_lists"); err != nil {
		return action.Outcome{}, err
	}
	in := tcc.Input{Binding: o.Binding, Regulation: tcc.Regulation{ID: f.regulation.Ref.ConfigID, Revision: f.regulation.Ref.Revision,
		ContentHash: f.regulation.ContentHash, RescreenDays: reg.RescreenIntervalDays, ValidUntil: f.regulation.ValidUntil},
		CodeTableVersion: cmd.CodeTableVersion, Lists: cmd.ProgrammeLists, Versions: f.programmes.Versions, Hits: f.programmes.Hits,
		Addresses: f.addresses(), By: d.Actor, Epoch: d.Epoch, At: now}
	if dest := cmd.Destination; dest != nil {
		ev, err := evidenceOf(f.sources, dest.EvidenceIDs, "/args/destination/evidence_ids")
		if err != nil {
			return action.Outcome{}, err
		}
		in.Destination = &tcc.Entered{Country: dest.Country, Reason: dest.Reason, Evidence: ev}
	}
	for i, a := range cmd.AddressCountries {
		ev, err := evidenceOf(f.sources, a.EvidenceIDs, "/args/address_countries/"+strconv.Itoa(i)+"/evidence_ids")
		if err != nil {
			return action.Outcome{}, err
		}
		in.Countries = append(in.Countries, tcc.Entered{PartyID: a.PartyID, AddressID: a.AddressID, Country: a.Country, Evidence: ev})
	}
	if e := cmd.EndUse; e != nil {
		ev, err := evidenceOf(f.sources, e.EvidenceIDs, "/args/end_use/evidence_ids")
		if err != nil {
			return action.Outcome{}, err
		}
		in.EndUse = &tcc.EndUse{Statement: e.Statement, Evidence: ev}
	}
	for _, l := range o.Lines {
		line := tcc.Line{LineID: l.LineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision}
		if p, ok := f.profiles[ProfileRef{SkuID: l.SkuID, SpecRevision: l.SpecRevision, CodeTableVersion: cmd.CodeTableVersion}]; ok {
			line.Profile = &p
		}
		in.Lines = append(in.Lines, line)
	}

	fresh := h.check == nil
	if fresh {
		previous, err := postgres.PreviousCheck(ctx, exec, sc, o.Binding.OrderID, o.Binding.OrderRevision)
		if err != nil {
			return action.Outcome{}, err
		}
		h.check = tcc.Open(action.NewID("tcc", now), o.Binding, previous, d.Actor, d.Team, d.Epoch, now)
		in.Seq = 1
	} else if in.Seq, err = postgres.NextEvaluationSeq(ctx, exec, sc, h.check.ID); err != nil {
		return action.Outcome{}, err
	}
	in.CheckID, in.EvaluationID = h.check.ID, action.NewID("tce", now)
	ev, err := tcc.Evaluate(in)
	if err != nil {
		return action.Outcome{}, err
	}
	from := h.check.RowVersion
	if err = h.check.Evaluated(ev.EvaluationID, fresh); err != nil {
		return action.Outcome{}, err
	}
	if fresh {
		err = postgres.InsertCheck(ctx, exec, sc, h.check)
	} else {
		err = postgres.SaveCheck(ctx, exec, sc, h.check, from, now)
	}
	if err == nil {
		err = postgres.InsertEvaluation(ctx, exec, sc, ev)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: h.check.ID, RowVersion: h.check.RowVersion, Status: string(h.check.Status)}, nil
}

// Evaluate evaluates an order revision (trade_compliance.evaluate).
func (s Service) Evaluate(ctx context.Context, cmd api.Evaluate) (api.CheckReceipt, error) {
	h := &evaluateCommand{s: s, cmd: cmd}
	return s.run(ctx, action.Command{Name: api.CommandEvaluate, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OrderID}, cmd, cmd.Validate(), h)
}
