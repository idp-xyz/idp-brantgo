package application

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
	"go.idp.xyz/idp-brant/internal/reference/api"
	"go.idp.xyz/idp-brant/internal/reference/internal/postgres"
	"go.idp.xyz/idp-brant/internal/reference/internal/programmes"
)

// CommandGetProgrammeList is the audited read of country programme lists (permission ref.read).
const CommandGetProgrammeList = "country_programme_list.get"

// ImportCountryProgrammes imports one version of a country programme list and makes it the list's
// current version. Its programmes never change afterwards.
func (s Service) ImportCountryProgrammes(ctx context.Context, cmd api.ImportCountryProgrammes) (api.Receipt, error) {
	objectID := api.ObjectID(cmd.ListCode, cmd.Version)
	if !kernel.ValidIdentifier(objectID) {
		objectID = ""
	}
	if err := cmd.Validate(); err != nil {
		if rej, ok := kernel.AsRejection(err); ok {
			if auditErr := s.Ledger.AuditRejected(ctx, api.CommandImportCountryProgrammes, objectID, rej.Code); auditErr != nil {
				return api.Receipt{}, errors.Join(err, auditErr)
			}
		}
		return api.Receipt{}, err
	}
	// The request hash covers the canonical content: programmes by ID with sorted countries, so
	// the same list sent in another order is the same request.
	args := cmd
	args.IdempotencyKey = ""
	list := canonicalProgrammes(cmd.Programmes)
	args.Programmes = make([]api.CountryProgramme, len(list))
	for i, p := range list {
		args.Programmes[i] = api.CountryProgramme{ProgrammeID: p.ID, Name: p.Name, Measure: p.Measure, Countries: p.Countries, Scope: p.Scope}
	}
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.Receipt{}, err
	}
	res, err := s.Ledger.Execute(ctx, action.Command{Name: api.CommandImportCountryProgrammes, IdempotencyKey: cmd.IdempotencyKey,
		RequestHash: hash, ObjectID: objectID}, &importProgrammes{s: s, cmd: cmd, programmes: list})
	return api.Receipt(res), err
}

// canonicalProgrammes returns the programmes in canonical form, ordered by programme ID.
func canonicalProgrammes(in []api.CountryProgramme) []programmes.Programme {
	out := make([]programmes.Programme, len(in))
	for i, p := range in {
		out[i] = programmes.NewProgramme(p.ProgrammeID, p.Name, p.Measure, p.Countries, p.Scope)
	}
	slices.SortFunc(out, func(a, b programmes.Programme) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// importProgrammes locks the list row and imports the version.
type importProgrammes struct {
	s          Service
	cmd        api.ImportCountryProgrammes
	programmes []programmes.Programme
	list       *programmes.List
}

func (h *importProgrammes) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.list, err = postgres.LoadProgrammeList(ctx, exec, scope(ctx), h.cmd.ListCode, true)
	return err
}

func (h *importProgrammes) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c := scope(ctx), h.cmd
	published, _ := time.Parse(time.DateOnly, c.PublishedOn)
	now := h.s.Clock.Now()
	v := &programmes.Version{ListCode: c.ListCode, Version: c.Version, SourceName: c.SourceName, SourceURL: c.SourceURL,
		PublishedOn: published, FileSHA256: c.FileSHA256, ContentHash: programmes.ContentHash(h.programmes),
		ProgrammeCount: len(h.programmes), CountryCount: len(programmes.Countries(h.programmes)), ImportedBy: d.Actor, Epoch: d.Epoch,
		ImportedAt: now}
	existing, err := postgres.LoadProgrammeVersion(ctx, exec, sc, c.ListCode, c.Version)
	if err != nil {
		return action.Outcome{}, err
	}
	var current *programmes.Version
	if h.list != nil {
		if current, err = postgres.LoadProgrammeVersion(ctx, exec, sc, c.ListCode, h.list.Current); err != nil {
			return action.Outcome{}, err
		}
	}
	next, err := programmes.Import(h.list, c.Jurisdiction, existing != nil, current, v)
	if err != nil {
		return action.Outcome{}, err
	}
	var from int64
	if h.list != nil {
		from = h.list.RowVersion
	}
	// A new list row refers to its first version through a deferred foreign key; the version
	// refers to the list row.
	if from == 0 {
		err = postgres.SaveProgrammeList(ctx, exec, sc, next, 0, now)
	}
	if err == nil {
		err = postgres.InsertProgrammeVersion(ctx, exec, sc, v, h.programmes)
	}
	if err == nil && from != 0 {
		err = postgres.SaveProgrammeList(ctx, exec, sc, next, from, now)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	fields := map[string]any{"list_code": c.ListCode, "version": c.Version, "jurisdiction": c.Jurisdiction, "import_seq": v.ImportSeq,
		"programme_count": v.ProgrammeCount, "country_count": v.CountryCount, "content_hash": v.ContentHash, "file_sha256": v.FileSHA256}
	if current != nil {
		fields["previous_version"] = current.Version
	}
	return action.Outcome{ObjectID: api.ObjectID(c.ListCode, c.Version), RowVersion: next.RowVersion, Status: api.StatusImported,
		Events: []action.Event{{Type: api.EventCountryProgrammesImported, AggregateType: "CountryProgrammeList", AggregateID: c.ListCode,
			AggregateVersion: next.RowVersion, Fields: fields}}}, nil
}

// ResolveCountryProgrammes answers the embargo lookups of downstream contexts: the versions asked
// for and the programmes of them that name one of the countries, in one read.
func (s Service) ResolveCountryProgrammes(ctx context.Context, q api.ResolveProgrammes) (api.ResolvedProgrammes, error) {
	out := api.ResolvedProgrammes{Versions: map[string]api.ProgrammeListVersion{}, Hits: []api.CountryProgrammeHit{}}
	if err := q.Validate(); err != nil || len(q.Lists) == 0 {
		return out, err
	}
	codes, versions := make([]string, len(q.Lists)), make([]string, len(q.Lists))
	for i, l := range q.Lists {
		codes[i], versions[i] = l.ListCode, l.Version
	}
	countries := slices.Compact(slices.Sorted(slices.Values(q.Countries)))
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		found, err := postgres.ResolveProgrammeVersions(ctx, exec, sc, codes, versions)
		if err != nil {
			return err
		}
		usedCodes, usedVersions := make([]string, len(found)), make([]string, len(found))
		for i, r := range found {
			out.Versions[r.Version.ListCode] = programmeVersionView(r.Jurisdiction, r.Version, r.Current)
			usedCodes[i], usedVersions[i] = r.Version.ListCode, r.Version.Version
		}
		if len(found) == 0 || len(countries) == 0 {
			return nil
		}
		hits, err := postgres.ProgrammesNaming(ctx, exec, sc, usedCodes, usedVersions, countries)
		for _, h := range hits {
			out.Hits = append(out.Hits, api.CountryProgrammeHit{ListCode: h.ListCode, Jurisdiction: out.Versions[h.ListCode].Jurisdiction,
				Version: h.Version, Country: h.Country, Programme: programmeView(h.Programme)})
		}
		return err
	})
	return out, err
}

// GetProgrammeList returns a programme list and its versions, audited.
func (s Service) GetProgrammeList(ctx context.Context, code string) (api.ProgrammeListView, error) {
	var v api.ProgrammeListView
	err := s.Ledger.Read(ctx, CommandGetProgrammeList, code, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		if !api.ValidListCode(code) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		l, err := postgres.LoadProgrammeList(ctx, exec, scope(ctx), code, false)
		if err != nil {
			return err
		}
		if l == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.ProgrammeListView{ListCode: l.Code, Jurisdiction: l.Jurisdiction, CurrentVersion: l.Current, RowVersion: l.RowVersion}
		versions, err := postgres.ProgrammeVersions(ctx, exec, scope(ctx), code)
		for _, ver := range versions {
			v.Versions = append(v.Versions, programmeVersionView(l.Jurisdiction, ver, ver.Version == l.Current))
		}
		return err
	})
	return v, err
}

// GetProgrammeListVersion returns one version and its programmes, audited.
func (s Service) GetProgrammeListVersion(ctx context.Context, code, version string) (api.ProgrammeListVersionView, error) {
	var v api.ProgrammeListVersionView
	objectID := api.ObjectID(code, version)
	err := s.Ledger.Read(ctx, CommandGetProgrammeList, objectID, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		if !api.ValidListCode(code) || !api.ValidVersion(version) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		sc := scope(ctx)
		l, err := postgres.LoadProgrammeList(ctx, exec, sc, code, false)
		if err != nil {
			return err
		}
		var ver *programmes.Version
		if l != nil {
			if ver, err = postgres.LoadProgrammeVersion(ctx, exec, sc, code, version); err != nil {
				return err
			}
		}
		if ver == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v.ProgrammeListVersion = programmeVersionView(l.Jurisdiction, ver, ver.Version == l.Current)
		list, err := postgres.ProgrammesOf(ctx, exec, sc, code, version)
		v.Programmes = []api.Programme{}
		for _, p := range list {
			v.Programmes = append(v.Programmes, programmeView(p))
		}
		return err
	})
	return v, err
}

func programmeVersionView(jurisdiction string, v *programmes.Version, current bool) api.ProgrammeListVersion {
	return api.ProgrammeListVersion{ListCode: v.ListCode, Jurisdiction: jurisdiction, Version: v.Version, SourceName: v.SourceName,
		SourceURL: v.SourceURL, PublishedOn: v.PublishedOn.Format(time.DateOnly), FileSHA256: v.FileSHA256, ContentHash: v.ContentHash,
		ProgrammeCount: v.ProgrammeCount, CountryCount: v.CountryCount, ImportSeq: v.ImportSeq, ImportedAt: v.ImportedAt,
		ImportedBy: v.ImportedBy, Current: current}
}

func programmeView(p programmes.Programme) api.Programme {
	countries := p.Countries
	if countries == nil {
		countries = []string{}
	}
	return api.Programme{ProgrammeID: p.ID, Name: p.Name, Measure: p.Measure, Countries: countries, Scope: p.Scope, ContentHash: p.Hash}
}
