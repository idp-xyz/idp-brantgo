// Package application runs the commands and queries of screening list reference data on the
// command ledger. List rules live in lists; SQL lives in postgres.
package application

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
	"go.idp.xyz/idp-brant/internal/reference/api"
	"go.idp.xyz/idp-brant/internal/reference/internal/lists"
	"go.idp.xyz/idp-brant/internal/reference/internal/postgres"
)

// Read commands and permissions.
const (
	CommandGetList  = "screening_list.get"
	CommandAudit    = "ref.audit"
	PermissionRead  = "ref.read"
	PermissionAudit = "ref.audit"
)

var anyActor = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}

// Rules is the authorization policy of reference data: only a human imports a list version (the
// compliance lead confirms list sources, S1-09).
func Rules() []authorization.Rule {
	return []authorization.Rule{
		authorization.HumanOnly(api.CommandImportScreeningList, api.CommandImportScreeningList),
		{Command: CommandGetList, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandAudit, Permission: PermissionAudit, ActorKinds: anyActor},
	}
}

// Visible reports whether d's actor sees a stored result. Lists belong to the company, not to an
// owner, so the read permission the ledger checks before a replay is the only restriction.
func Visible(context.Context, bentopg.Executor, authorization.Decision, string, string) (bool, error) {
	return true, nil
}

// Service runs the use cases of reference data.
type Service struct {
	Ledger *action.Ledger
	Clock  application.Clock
}

func scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

// ImportScreeningList imports one version of a screening list and makes it the list's current
// version. Its entries never change afterwards.
func (s Service) ImportScreeningList(ctx context.Context, cmd api.ImportScreeningList) (api.Receipt, error) {
	objectID := api.ObjectID(cmd.ListCode, cmd.Version)
	if !kernel.ValidIdentifier(objectID) {
		objectID = ""
	}
	if err := cmd.Validate(); err != nil {
		if rej, ok := kernel.AsRejection(err); ok {
			if auditErr := s.Ledger.AuditRejected(ctx, api.CommandImportScreeningList, objectID, rej.Code); auditErr != nil {
				return api.Receipt{}, errors.Join(err, auditErr)
			}
		}
		return api.Receipt{}, err
	}
	args := cmd
	args.IdempotencyKey = ""
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.Receipt{}, err
	}
	res, err := s.Ledger.Execute(ctx, action.Command{Name: api.CommandImportScreeningList, IdempotencyKey: cmd.IdempotencyKey,
		RequestHash: hash, ObjectID: objectID}, &importCommand{s: s, cmd: cmd})
	return api.Receipt(res), err
}

// importCommand locks the list row and imports the version.
type importCommand struct {
	s    Service
	cmd  api.ImportScreeningList
	list *lists.List
}

func (h *importCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.list, err = postgres.LoadList(ctx, exec, scope(ctx), h.cmd.ListCode, true)
	return err
}

func (h *importCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c := scope(ctx), h.cmd
	entries := make([]lists.Entry, len(c.Entries))
	for i, e := range c.Entries {
		var err error
		if entries[i], err = lists.NewEntry(e.EntryID, e.PrimaryName, e.Aliases, e.EntityType, e.Country); err != nil {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/entries/"+strconv.Itoa(i), "A name is too long to match")
		}
	}
	published, _ := time.Parse(time.DateOnly, c.PublishedOn)
	now := h.s.Clock.Now()
	v := &lists.Version{ListCode: c.ListCode, Version: c.Version, SourceName: c.SourceName, SourceURL: c.SourceURL, PublishedOn: published,
		FileSHA256: c.FileSHA256, ContentHash: lists.ContentHash(entries), EntryCount: len(entries), ImportedBy: d.Actor, Epoch: d.Epoch,
		ImportedAt: now}
	existing, err := postgres.LoadVersion(ctx, exec, sc, c.ListCode, c.Version)
	if err != nil {
		return action.Outcome{}, err
	}
	var current *lists.Version
	if h.list != nil {
		if current, err = postgres.LoadVersion(ctx, exec, sc, c.ListCode, h.list.Current); err != nil {
			return action.Outcome{}, err
		}
	}
	next, err := lists.Import(h.list, c.Jurisdiction, existing != nil, current, v)
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
		err = postgres.SaveList(ctx, exec, sc, next, 0, now)
	}
	if err == nil {
		err = postgres.InsertVersion(ctx, exec, sc, v, entries)
	}
	if err == nil && from != 0 {
		err = postgres.SaveList(ctx, exec, sc, next, from, now)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	fields := map[string]any{"list_code": c.ListCode, "version": c.Version, "jurisdiction": c.Jurisdiction, "import_seq": v.ImportSeq,
		"entry_count": v.EntryCount, "content_hash": v.ContentHash, "file_sha256": v.FileSHA256}
	if current != nil {
		fields["previous_version"] = current.Version
	}
	return action.Outcome{ObjectID: api.ObjectID(c.ListCode, c.Version), RowVersion: next.RowVersion, Status: api.StatusImported,
		Events: []action.Event{{Type: api.EventScreeningListImported, AggregateType: "ScreeningList", AggregateID: c.ListCode,
			AggregateVersion: next.RowVersion, Fields: fields}}}, nil
}

// CurrentVersions answers lookups of downstream contexts.
func (s Service) CurrentVersions(ctx context.Context, codes []string) (map[string]api.ListVersion, error) {
	out := map[string]api.ListVersion{}
	if len(codes) == 0 {
		return out, nil
	}
	for i, c := range codes {
		if !api.ValidListCode(c) {
			return out, kernel.Reject(kernel.CodeSchemaInvalid, "/list_codes/"+strconv.Itoa(i), "")
		}
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		found, err := postgres.CurrentVersions(ctx, exec, scope(ctx), slices.Compact(slices.Sorted(slices.Values(codes))))
		for _, c := range found {
			out[c.Version.ListCode] = versionView(c.Jurisdiction, c.Version, true)
		}
		return err
	})
	return out, err
}

// Match returns the entries of the versions that one of names matches under api.Normalization.
func (s Service) Match(ctx context.Context, versions []api.ListVersionRef, names []string) ([]api.Hit, error) {
	if len(versions) > api.MaxMatchVersions || len(names) > api.MaxMatchNames {
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/", "")
	}
	codes, numbers := make([]string, len(versions)), make([]string, len(versions))
	for i, v := range versions {
		if !api.ValidListCode(v.ListCode) || !api.ValidVersion(v.Version) {
			return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/versions/"+strconv.Itoa(i), "")
		}
		codes[i], numbers[i] = v.ListCode, v.Version
	}
	byKey := map[string][]string{} // key → the queried names that have it
	var keys []string
	for i, n := range names {
		if !api.Name(n) {
			return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/names/"+strconv.Itoa(i), "")
		}
		k := lists.NameKey(n)
		if _, seen := byKey[k]; !seen {
			keys = append(keys, k)
		}
		byKey[k] = append(byKey[k], n)
	}
	var out []api.Hit
	if len(versions) == 0 || len(keys) == 0 {
		return out, nil
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		found, err := postgres.MatchKeys(ctx, exec, scope(ctx), codes, numbers, keys)
		for _, m := range found {
			for _, q := range byKey[m.Key] {
				out = append(out, api.Hit{ListCode: m.ListCode, Version: m.Version, Entry: entryView(m.Entry), QueryName: q,
					MatchedName: m.Name, NameKey: m.Key})
			}
		}
		return err
	})
	return out, err
}

// GetList returns a list and its versions, audited.
func (s Service) GetList(ctx context.Context, code string) (api.ListView, error) {
	var v api.ListView
	err := s.Ledger.Read(ctx, CommandGetList, code, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		if !api.ValidListCode(code) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		l, err := postgres.LoadList(ctx, exec, scope(ctx), code, false)
		if err != nil {
			return err
		}
		if l == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.ListView{ListCode: l.Code, Jurisdiction: l.Jurisdiction, CurrentVersion: l.Current, RowVersion: l.RowVersion}
		versions, err := postgres.Versions(ctx, exec, scope(ctx), code)
		for _, ver := range versions {
			v.Versions = append(v.Versions, versionView(l.Jurisdiction, ver, ver.Version == l.Current))
		}
		return err
	})
	return v, err
}

// GetListVersion returns one version and its entries, audited.
func (s Service) GetListVersion(ctx context.Context, code, version string) (api.ListVersionView, error) {
	var v api.ListVersionView
	objectID := api.ObjectID(code, version)
	err := s.Ledger.Read(ctx, CommandGetList, objectID, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		if !api.ValidListCode(code) || !api.ValidVersion(version) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		sc := scope(ctx)
		l, err := postgres.LoadList(ctx, exec, sc, code, false)
		if err != nil {
			return err
		}
		var ver *lists.Version
		if l != nil {
			if ver, err = postgres.LoadVersion(ctx, exec, sc, code, version); err != nil {
				return err
			}
		}
		if ver == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v.ListVersion = versionView(l.Jurisdiction, ver, ver.Version == l.Current)
		entries, err := postgres.Entries(ctx, exec, sc, code, version)
		for _, e := range entries {
			v.Entries = append(v.Entries, entryView(e))
		}
		return err
	})
	return v, err
}

// Audit returns the audit trail of reference data.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, CommandAudit)
}

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}

func versionView(jurisdiction string, v *lists.Version, current bool) api.ListVersion {
	return api.ListVersion{ListCode: v.ListCode, Jurisdiction: jurisdiction, Version: v.Version, SourceName: v.SourceName,
		SourceURL: v.SourceURL, PublishedOn: v.PublishedOn.Format(time.DateOnly), FileSHA256: v.FileSHA256, ContentHash: v.ContentHash,
		EntryCount: v.EntryCount, ImportSeq: v.ImportSeq, ImportedAt: v.ImportedAt, ImportedBy: v.ImportedBy, Current: current}
}

func entryView(e lists.Entry) api.Entry {
	aliases := e.Aliases
	if aliases == nil {
		aliases = []string{}
	}
	return api.Entry{EntryID: e.ID, PrimaryName: e.PrimaryName, Aliases: aliases, EntityType: e.EntityType, Country: e.Country,
		ContentHash: e.Hash}
}
