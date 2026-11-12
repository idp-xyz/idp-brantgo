package application

import (
	"context"
	"slices"
	"strconv"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/declaration"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/sku"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Audited reads of declaration profiles; they need PermissionRead.
const (
	CommandGetDeclarationProfile   = "declaration_profile.get"
	CommandListDeclarationProfiles = "declaration_profile.list"
)

// SaveDeclarationProfile saves the whole draft revision of a profile (CAT-INV-002: a candidate, by a
// human or an AI agent). The sources its content cites are read through EVD before CAT's
// transaction (Context Map R62); in the transaction the profile's root is locked FOR UPDATE and the
// SKU read, whose confirmed revisions and owner never change.
func (s Service) SaveDeclarationProfile(ctx context.Context, cmd api.SaveDeclarationProfile) (api.Receipt, error) {
	args := cmd
	args.IdempotencyKey, args.ExpectedVersion = "", 0
	key := declaration.Key{SkuID: cmd.SkuID, SpecRevision: cmd.SpecRevision, CodeTableVersion: cmd.CodeTableVersion}
	content := toContent(cmd.Profile())
	var readable map[string]evdapi.SourceRef
	h := &profileCommand{id: cmd.ProfileID, skuID: cmd.SkuID, prepare: func(ctx context.Context) (err error) {
		readable, err = s.readable(ctx, content.SourceIDs())
		return err
	}}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := declaration.CheckSave(h.root, cmd.ExpectedVersion, key); err != nil {
			return action.Outcome{}, err
		}
		if err := cited(cmd.Profile(), readable, "/args"); err != nil {
			return action.Outcome{}, err
		}
		if err := declaration.CheckSku(h.sees(d), h.confirmed(), cmd.SpecRevision); err != nil {
			return action.Outcome{}, within("/args", err)
		}
		sc := scope(ctx)
		created := h.root == nil
		if created {
			taken, err := postgres.ProfileKeyTaken(ctx, exec, sc, key)
			if err != nil {
				return action.Outcome{}, err
			}
			if err = declaration.CheckKey(taken); err != nil {
				return action.Outcome{}, err
			}
			h.root = declaration.New(cmd.ProfileID, key)
		}
		now := s.Clock.Now()
		rev, fresh := h.root.Save(content, d.Actor, d.Epoch, now)
		var err error
		if created {
			err = postgres.InsertProfile(ctx, exec, sc, h.root, now)
		} else {
			err = postgres.UpdateProfile(ctx, exec, sc, h.root, h.from, now)
		}
		if err == nil && fresh {
			err = postgres.InsertProfileRevision(ctx, exec, sc, h.root, rev)
		} else if err == nil {
			err = postgres.UpdateProfileRevision(ctx, exec, sc, h.root, rev)
		}
		// A draft has no consumer: saving publishes nothing (S3-D09).
		return h.outcome(rev), err
	}
	return s.run(ctx, action.Command{Name: api.CommandSaveDeclarationProfile, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ProfileID},
		args, cmd.Validate(), &preparedProfile{h})
}

// VerifyDeclarationProfile makes the draft revision the verified one and supersedes the verified
// revision, for a human other than the draft's last editor (CAT-INV-002, SM-CAT-DECLARATION). The
// evidence, and the sources the draft cites, are read through EVD before CAT's transaction: the
// draft's sources as it stood then, which is what the transaction verifies, since any save in
// between changes the root version the command names.
func (s Service) VerifyDeclarationProfile(ctx context.Context, cmd api.VerifyDeclarationProfile) (api.Receipt, error) {
	args := cmd
	args.IdempotencyKey, args.ExpectedVersion = "", 0
	var readable map[string]evdapi.SourceRef
	h := &profileCommand{id: cmd.ProfileID}
	h.prepare = func(ctx context.Context) error {
		ids := slices.Clone(cmd.EvidenceIDs)
		err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
			r, err := postgres.LoadProfile(ctx, exec, scope(ctx), cmd.ProfileID, false)
			if err == nil && r != nil && r.Draft != nil {
				ids = append(ids, r.Draft.Content.SourceIDs()...)
			}
			return err
		})
		if err == nil {
			readable, err = s.readable(ctx, ids)
		}
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := declaration.Check(h.root, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		draft, err := h.root.CheckVerify(cmd.Revision, d.Actor)
		if err != nil {
			return action.Outcome{}, err
		}
		// The verifier must be able to read every source it cites and every source the revision
		// cites.
		for i, id := range cmd.EvidenceIDs {
			if _, ok := readable[id]; !ok {
				return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/evidence_ids/"+strconv.Itoa(i), "Source is not accessible")
			}
		}
		for _, id := range draft.Content.SourceIDs() {
			if _, ok := readable[id]; !ok {
				return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/revision", "The revision cites source "+id+", which is not accessible")
			}
		}
		if err = declaration.CheckSku(h.sees(d), h.confirmed(), h.root.Key.SpecRevision); err != nil {
			return action.Outcome{}, repath(err, "/args/revision")
		}
		now := s.Clock.Now()
		superseded := h.root.Verify(d.Actor, d.Epoch, cmd.EvidenceIDs, now)
		verified := h.root.Verified
		sc := scope(ctx)
		fields := map[string]any{"profile_id": h.root.ID, "revision": verified.Number, "root_version": h.root.Version,
			"sku_id": h.root.Key.SkuID, "spec_revision": h.root.Key.SpecRevision, "code_table_version": h.root.Key.CodeTableVersion,
			"content_hash": verified.Hash}
		if superseded != nil {
			// The superseded revision is written first: one revision per profile is verified at a time.
			err = postgres.UpdateProfileRevision(ctx, exec, sc, h.root, superseded)
			fields["superseded_revision"] = superseded.Number
		}
		if err == nil {
			err = postgres.UpdateProfileRevision(ctx, exec, sc, h.root, verified)
		}
		if err == nil {
			err = postgres.UpdateProfile(ctx, exec, sc, h.root, h.from, now)
		}
		// The event carries identifiers, versions and the content hash, not the classification:
		// consumers resolve DeclarationProfileRef v1 (contract C4).
		return h.outcome(verified, action.Event{Type: api.EventDeclarationProfileVerified, AggregateType: "DeclarationProfile",
			AggregateID: h.root.ID, AggregateVersion: h.root.Version, Fields: fields}), err
	}
	return s.run(ctx, action.Command{Name: api.CommandVerifyDeclarationProfile, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ProfileID,
		Reason: cmd.Reason}, args, cmd.Validate(), &preparedProfile{h})
}

// readable returns the sources among ids the caller may read; none are asked for when there are
// none, so a draft that cites no source needs no EVD permission.
func (s Service) readable(ctx context.Context, ids []string) (map[string]evdapi.SourceRef, error) {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(ids) == 0 {
		return map[string]evdapi.SourceRef{}, nil
	}
	return s.Sources.ResolveSources(ctx, ids)
}

// cited refuses the first source of p, in request order, the caller may not read.
func cited(p api.DeclarationProfile, readable map[string]evdapi.SourceRef, path string) error {
	for _, part := range []struct {
		path string
		ids  []string
	}{{"/commodity/source_ids/", p.Commodity.SourceIDs}, {"/origin/source_ids/", p.Origin.SourceIDs},
		{"/export_control/source_ids/", p.ExportControl.SourceIDs}} {
		for i, id := range part.ids {
			if _, ok := readable[id]; !ok {
				return kernel.Reject(kernel.CodeReferenceInvalid, path+part.path+strconv.Itoa(i), "Source is not accessible")
			}
		}
	}
	return nil
}

// profileCommand locks a profile's root FOR UPDATE, then reads the SKU it is kept for, and applies
// one command.
type profileCommand struct {
	id      string
	skuID   string // the SKU a save names; a verification uses the profile's
	root    *declaration.Root
	from    int64
	sku     *sku.Sku
	prepare func(ctx context.Context) error
	apply   func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *profileCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.root, h.from, h.sku = nil, 0, nil
	sc := scope(ctx)
	var err error
	if h.root, err = postgres.LoadProfile(ctx, exec, sc, h.id, true); err != nil {
		return err
	}
	skuID := h.skuID
	if h.root != nil {
		h.from = h.root.Version
		if skuID == "" {
			skuID = h.root.Key.SkuID
		}
	}
	if skuID != "" {
		// A confirmed revision and the owner of a SKU never change, so the read needs no lock.
		h.sku, err = postgres.Load(ctx, exec, sc, skuID, false)
	}
	return err
}

func (h *profileCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

// sees reports whether d's actor sees the SKU of the command.
func (h *profileCommand) sees(d authorization.Decision) bool {
	return h.sku != nil && d.Sees(h.sku.OwnerActor, h.sku.OwnerTeam)
}

// confirmed is the number of confirmed specification revisions of the command's SKU.
func (h *profileCommand) confirmed() int64 {
	if h.sku == nil {
		return 0
	}
	return h.sku.Revision
}

func (h *profileCommand) outcome(rev *declaration.Revision, events ...action.Event) action.Outcome {
	return action.Outcome{ObjectID: h.root.ID, RowVersion: h.root.Version, Status: string(rev.Status), Events: events}
}

// preparedProfile reads upstream contexts before CAT's transaction (the ledger's Preparer).
type preparedProfile struct{ *profileCommand }

func (p *preparedProfile) Prepare(ctx context.Context) error { return p.prepare(ctx) }

// GetDeclarationProfile returns a profile, its key and all its revisions, audited. Profiles belong to
// the company, like item code mappings.
func (s Service) GetDeclarationProfile(ctx context.Context, id string) (api.DeclarationProfileView, error) {
	var v api.DeclarationProfileView
	err := s.Ledger.Read(ctx, CommandGetDeclarationProfile, id, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		r, err := postgres.LoadProfile(ctx, exec, sc, id, false)
		if err != nil {
			return err
		}
		if r == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		sum := summary(r)
		v = api.DeclarationProfileView{ProfileID: sum.ProfileID, Key: sum.Key, RootVersion: sum.RootVersion, DraftRevision: sum.DraftRevision,
			VerifiedRevision: sum.VerifiedRevision}
		revisions, err := postgres.ProfileRevisions(ctx, exec, sc, id)
		for _, rev := range revisions {
			rv := api.DeclarationProfileRevision{Revision: rev.Number, Status: string(rev.Status), ContentHash: rev.Hash,
				DeclarationProfile: fromContent(rev.Content), SavedBy: rev.SavedBy, SavedAt: rev.SavedAt, SupersededAt: rev.SupersededAt}
			if ver := rev.Verification; ver != nil {
				rv.VerifiedBy, rv.VerifiedAt, rv.EvidenceIDs = ver.By, ver.At, ver.EvidenceIDs
			}
			v.Revisions = append(v.Revisions, rv)
		}
		return err
	})
	return v, err
}

// ListDeclarationProfiles returns the profiles kept for a SKU, audited.
func (s Service) ListDeclarationProfiles(ctx context.Context, skuID string) (api.DeclarationProfileList, error) {
	v := api.DeclarationProfileList{SkuID: skuID, Profiles: []api.DeclarationProfileSummary{}}
	err := s.Ledger.Read(ctx, CommandListDeclarationProfiles, skuID, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		roots, err := postgres.ProfilesOfSku(ctx, exec, scope(ctx), skuID)
		for _, r := range roots {
			v.Profiles = append(v.Profiles, summary(r))
		}
		return err
	})
	return v, err
}

func summary(r *declaration.Root) api.DeclarationProfileSummary {
	s := api.DeclarationProfileSummary{ProfileID: r.ID, RootVersion: r.Version,
		Key: api.DeclarationProfileKey{SkuID: r.Key.SkuID, SpecRevision: r.Key.SpecRevision, CodeTableVersion: r.Key.CodeTableVersion}}
	if r.Draft != nil {
		s.DraftRevision = r.Draft.Number
	}
	if r.Verified != nil {
		s.VerifiedRevision = r.Verified.Number
	}
	return s
}

// ResolveDeclarationProfiles answers DeclarationProfileRef v1 lookups of downstream contexts (R39):
// every reference asked is answered, as declaration.Resolve decides it.
func (s Service) ResolveDeclarationProfiles(ctx context.Context, refs []api.DeclarationProfileRef) (map[api.DeclarationProfileRef]api.ResolvedDeclarationProfile, error) {
	if err := api.ValidateDeclarationProfileRefs(refs); err != nil {
		return nil, err
	}
	out := make(map[api.DeclarationProfileRef]api.ResolvedDeclarationProfile, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	seen := make(map[declaration.Key]bool, len(refs))
	keys := make([]declaration.Key, 0, len(refs))
	for _, r := range refs {
		if k := keyOfRef(r); !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		roots, err := postgres.ProfilesFor(ctx, exec, sc, keys)
		if err != nil {
			return err
		}
		var asked []postgres.RevisionRef
		for _, r := range refs {
			if root := roots[keyOfRef(r)]; root != nil && r.Revision > 0 {
				asked = append(asked, postgres.RevisionRef{ProfileID: root.ID, Revision: r.Revision})
			}
		}
		revisions, err := postgres.ProfileRevisionsAt(ctx, exec, sc, asked)
		if err != nil {
			return err
		}
		for _, r := range refs {
			root := roots[keyOfRef(r)]
			var rev *declaration.Revision
			if root != nil {
				rev = revisions[postgres.RevisionRef{ProfileID: root.ID, Revision: r.Revision}]
			}
			out[r] = resolved(r, root, declaration.Resolve(root, r.Revision, rev))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func keyOfRef(r api.DeclarationProfileRef) declaration.Key {
	return declaration.Key{SkuID: r.SkuID, SpecRevision: r.SpecRevision, CodeTableVersion: r.CodeTableVersion}
}

// resolved is the answer to ref: the profile now, the revision the answer names and, when it was
// verified, its content.
func resolved(ref api.DeclarationProfileRef, root *declaration.Root, a declaration.Answer) api.ResolvedDeclarationProfile {
	out := api.ResolvedDeclarationProfile{Ref: ref, Code: a.Code, Verified: a.Code == "" && a.Revision != nil}
	if root != nil {
		s := summary(root)
		out.ProfileID, out.CurrentRevision, out.DraftRevision = s.ProfileID, s.VerifiedRevision, s.DraftRevision
	}
	if rev := a.Revision; rev != nil {
		out.Revision, out.Status = rev.Number, string(rev.Status)
		if ver := rev.Verification; ver != nil {
			// A draft is a candidate: only what a professional verified is answered.
			out.ContentHash, out.Profile = rev.Hash, fromContent(rev.Content)
			out.VerifiedBy, out.VerifiedAt, out.EvidenceIDs = ver.By, ver.At, slices.Clone(ver.EvidenceIDs)
		}
	}
	return out
}

func toContent(p api.DeclarationProfile) declaration.Content {
	c := declaration.Content{
		Commodity:       declaration.Commodity{Code: p.Commodity.Code, Basis: p.Commodity.Basis, SourceIDs: p.Commodity.SourceIDs},
		DeclarationName: p.DeclarationName,
		Origin:          declaration.Origin{Country: p.Origin.Country, Criterion: p.Origin.Criterion, Basis: p.Origin.Basis, SourceIDs: p.Origin.SourceIDs},
		ExportControl: declaration.ExportControl{Status: p.ExportControl.Status, ListReference: p.ExportControl.ListReference,
			Basis: p.ExportControl.Basis, SourceIDs: p.ExportControl.SourceIDs},
	}
	for _, e := range p.Elements {
		c.Elements = append(c.Elements, declaration.Element{Name: e.Name, Value: e.Value})
	}
	for _, u := range p.LegalUnits {
		c.LegalUnits = append(c.LegalUnits, declaration.LegalUnit{Unit: u.Unit, PerBaseUnit: u.PerBaseUnit, Basis: u.Basis})
	}
	return declaration.Normalize(c)
}

func fromContent(c declaration.Content) api.DeclarationProfile {
	p := api.DeclarationProfile{
		Commodity:       api.CommodityClassification{Code: c.Commodity.Code, Basis: c.Commodity.Basis, SourceIDs: slices.Clone(c.Commodity.SourceIDs)},
		DeclarationName: c.DeclarationName,
		Origin: api.OriginBasis{Country: c.Origin.Country, Criterion: c.Origin.Criterion, Basis: c.Origin.Basis,
			SourceIDs: slices.Clone(c.Origin.SourceIDs)},
		ExportControl: api.ExportControlClassification{Status: c.ExportControl.Status, ListReference: c.ExportControl.ListReference,
			Basis: c.ExportControl.Basis, SourceIDs: slices.Clone(c.ExportControl.SourceIDs)},
	}
	for _, e := range c.Elements {
		p.Elements = append(p.Elements, api.DeclarationElement{Name: e.Name, Value: e.Value})
	}
	for _, u := range c.LegalUnits {
		p.LegalUnits = append(p.LegalUnits, api.LegalUnitConversion{Unit: u.Unit, PerBaseUnit: u.PerBaseUnit, Basis: u.Basis})
	}
	return p
}
