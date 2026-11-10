package application

import (
	"context"
	"slices"
	"strconv"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/mapping"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/sku"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CommandGetItemCodeMapping is the audited read of a mapping; it needs PermissionRead.
const CommandGetItemCodeMapping = "item_code_mapping.get"

func scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

// within returns the rejection in err with its path placed under prefix; other errors pass
// unchanged.
func within(prefix string, err error) error {
	if rej, ok := kernel.AsRejection(err); ok {
		return kernel.RejectAs(rej.Code, rej.Category, prefix+rej.Fields[0].Path, rej.Fields[0].Reason)
	}
	return err
}

// repath returns the rejection in err at path instead of its own, keeping its reason.
func repath(err error, path string) error {
	if rej, ok := kernel.AsRejection(err); ok {
		return kernel.RejectAs(rej.Code, rej.Category, path, rej.Fields[0].Reason)
	}
	return err
}

// SaveItemCodeMapping saves the whole draft revision of a mapping (Python foundation.save, kind
// code_mapping). The SKU is read in CAT's transaction and held FOR SHARE after the mapping's root.
func (s Service) SaveItemCodeMapping(ctx context.Context, cmd api.SaveItemCodeMapping) (api.Receipt, error) {
	args := cmd
	args.IdempotencyKey, args.ExpectedVersion = "", 0
	key := mapping.Key{PartyID: cmd.PartyID, PartyRole: mapping.Role(cmd.PartyRole), ExternalCode: cmd.ExternalCode}
	h := &mappingCommand{id: cmd.MappingID, skuID: cmd.SkuID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := mapping.CheckSave(h.root, cmd.ExpectedVersion, key); err != nil {
			return action.Outcome{}, err
		}
		if err := sku.CheckSpec(h.sku, h.sees(d), cmd.SpecRevision); err != nil {
			return action.Outcome{}, within("/args", err)
		}
		now := s.Clock.Now()
		created := h.root == nil
		if created {
			h.root = mapping.New(cmd.MappingID, key)
		}
		rev, fresh := h.root.Save(mapping.Target{SkuID: cmd.SkuID, SpecRevision: cmd.SpecRevision}, d.Actor, d.Epoch, now)
		sc := scope(ctx)
		var err error
		if created {
			err = postgres.InsertMapping(ctx, exec, sc, h.root, now)
		} else {
			err = postgres.UpdateMapping(ctx, exec, sc, h.root, h.from, now)
		}
		if err == nil && fresh {
			err = postgres.InsertMappingRevision(ctx, exec, sc, h.root, rev)
		} else if err == nil {
			err = postgres.UpdateMappingRevision(ctx, exec, sc, h.root, rev)
		}
		return h.outcome(rev), err
	}
	return s.run(ctx, action.Command{Name: api.CommandSaveItemCodeMapping, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.MappingID},
		args, cmd.Validate(), h)
}

// PublishItemCodeMapping makes the draft revision active and supersedes the active one (Python
// foundation.publish, kind code_mapping). The evidence is read through EVD before CAT's
// transaction (Context Map R62).
func (s Service) PublishItemCodeMapping(ctx context.Context, cmd api.PublishItemCodeMapping) (api.Receipt, error) {
	args := cmd
	args.IdempotencyKey, args.ExpectedVersion = "", 0
	var evidence map[string]evdapi.SourceRef
	h := &mappingCommand{id: cmd.MappingID, prepare: func(ctx context.Context) (err error) {
		evidence, err = s.Sources.ResolveSources(ctx, cmd.EvidenceIDs)
		return err
	}}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := mapping.Check(h.root, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if err := h.root.CheckPublish(cmd.Revision); err != nil {
			return action.Outcome{}, err
		}
		// The publisher must be able to read every source it cites (Python publish: _sources after
		// the state checks, before the references).
		for i, id := range cmd.EvidenceIDs {
			if _, ok := evidence[id]; !ok {
				return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/evidence_ids/"+strconv.Itoa(i), "Source is not accessible")
			}
		}
		if err := sku.CheckSpec(h.sku, h.sees(d), h.root.Draft.Target.SpecRevision); err != nil {
			return action.Outcome{}, repath(err, "/args/revision")
		}
		sc := scope(ctx)
		taken, err := postgres.KeyTaken(ctx, exec, sc, h.root.Key, h.root.ID)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = mapping.CheckKey(taken); err != nil {
			return action.Outcome{}, err
		}
		now := s.Clock.Now()
		superseded := h.root.Publish(d.Actor, d.Epoch, cmd.EvidenceIDs, now)
		active := h.root.Active
		fields := map[string]any{"mapping_id": h.root.ID, "revision": active.Number, "root_version": h.root.Version,
			"party_id": h.root.Key.PartyID, "party_role": string(h.root.Key.PartyRole), "sku_id": active.Target.SkuID,
			"spec_revision": active.Target.SpecRevision}
		if superseded != nil {
			// The superseded revision is written first: one revision per key is active at a time.
			err = postgres.UpdateMappingRevision(ctx, exec, sc, h.root, superseded)
			fields["superseded_revision"] = superseded.Number
		}
		if err == nil {
			err = postgres.UpdateMappingRevision(ctx, exec, sc, h.root, active)
		}
		if err == nil {
			err = postgres.UpdateMapping(ctx, exec, sc, h.root, h.from, now)
		}
		// The event carries identifiers and versions, not the external code (contract C4).
		return h.outcome(active, action.Event{Type: api.EventItemCodeMappingPublished, AggregateType: "ItemCodeMapping",
			AggregateID: h.root.ID, AggregateVersion: h.root.Version, Fields: fields}), err
	}
	return s.run(ctx, action.Command{Name: api.CommandPublishItemCodeMapping, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.MappingID,
		Reason: cmd.Reason}, args, cmd.Validate(), &preparedMapping{h})
}

// mappingCommand locks a mapping's root FOR UPDATE, then the SKU its command references FOR SHARE,
// and applies one command.
type mappingCommand struct {
	id      string
	skuID   string // the SKU a save names; a publication references its draft's
	root    *mapping.Root
	from    int64
	sku     *sku.Sku
	prepare func(ctx context.Context) error
	apply   func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *mappingCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.root, h.from, h.sku = nil, 0, nil
	sc := scope(ctx)
	var err error
	if h.root, err = postgres.LoadMapping(ctx, exec, sc, h.id, true); err != nil {
		return err
	}
	skuID := h.skuID
	if h.root != nil {
		h.from = h.root.Version
		if skuID == "" && h.root.Draft != nil {
			skuID = h.root.Draft.Target.SkuID
		}
	}
	if skuID != "" {
		h.sku, err = postgres.LoadShared(ctx, exec, sc, skuID)
	}
	return err
}

func (h *mappingCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

// sees reports whether d's actor sees the SKU the command references.
func (h *mappingCommand) sees(d authorization.Decision) bool {
	return h.sku != nil && d.Sees(h.sku.OwnerActor, h.sku.OwnerTeam)
}

func (h *mappingCommand) outcome(rev *mapping.Revision, events ...action.Event) action.Outcome {
	return action.Outcome{ObjectID: h.root.ID, RowVersion: h.root.Version, Status: string(rev.Status), Events: events}
}

// preparedMapping reads upstream contexts before CAT's transaction (the ledger's Preparer).
type preparedMapping struct{ *mappingCommand }

func (p *preparedMapping) Prepare(ctx context.Context) error { return p.prepare(ctx) }

// GetItemCodeMapping returns a mapping, its key and all its revisions, audited.
func (s Service) GetItemCodeMapping(ctx context.Context, id string) (api.ItemCodeMappingView, error) {
	var v api.ItemCodeMappingView
	err := s.Ledger.Read(ctx, CommandGetItemCodeMapping, id, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		r, err := postgres.LoadMapping(ctx, exec, sc, id, false)
		if err != nil {
			return err
		}
		if r == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.ItemCodeMappingView{MappingID: r.ID, RootVersion: r.Version,
			Code: api.ItemCode{PartyID: r.Key.PartyID, PartyRole: string(r.Key.PartyRole), ExternalCode: r.Key.ExternalCode}}
		if r.Draft != nil {
			v.DraftRevision = r.Draft.Number
		}
		if r.Active != nil {
			v.ActiveRevision = r.Active.Number
		}
		revisions, err := postgres.MappingRevisions(ctx, exec, sc, id)
		for _, rev := range revisions {
			rv := api.ItemCodeMappingRevision{Revision: rev.Number, Status: string(rev.Status), SavedBy: rev.SavedBy, SavedAt: rev.SavedAt,
				Sku: api.SpecRef{SkuID: rev.Target.SkuID, SpecRevision: rev.Target.SpecRevision}, SupersededAt: rev.SupersededAt}
			if p := rev.Publication; p != nil {
				rv.PublishedBy, rv.PublishedAt, rv.EvidenceIDs = p.By, p.At, p.EvidenceIDs
			}
			v.Revisions = append(v.Revisions, rv)
		}
		return err
	})
	return v, err
}

// ResolveItemCodes answers ItemCodeMappingRef v1 lookups of downstream contexts by item code.
func (s Service) ResolveItemCodes(ctx context.Context, codes []api.ItemCode) (map[api.ItemCode]api.ResolvedItemCode, error) {
	if err := api.ValidateItemCodes(codes); err != nil {
		return nil, err
	}
	out := make(map[api.ItemCode]api.ResolvedItemCode, len(codes))
	if len(codes) == 0 {
		return out, nil
	}
	unique := make(map[mapping.Key]bool, len(codes))
	keys := make([]mapping.Key, 0, len(codes))
	for _, c := range codes {
		if k := keyOf(c); !unique[k] {
			unique[k] = true
			keys = append(keys, k)
		}
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		found, err := postgres.PublishedFor(ctx, exec, scope(ctx), keys)
		if err != nil {
			return err
		}
		for _, c := range codes {
			r := api.ResolvedItemCode{Code: c, Status: api.ItemCodeUnmapped}
			if p, ok := found[keyOf(c)]; ok {
				r.Status, r.Mapping = api.ItemCodeMapped, api.ItemCodeMappingRef{MappingID: p.MappingID, Revision: p.Revision}
				r.Sku = api.SpecRef{SkuID: p.Target.SkuID, SpecRevision: p.Target.SpecRevision}
			}
			out[c] = r
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func keyOf(c api.ItemCode) mapping.Key {
	return mapping.Key{PartyID: c.PartyID, PartyRole: mapping.Role(c.PartyRole), ExternalCode: c.ExternalCode}
}

// ResolveSpecs tells downstream contexts whether specification revisions are current for new use
// (CAT-INV-001); SKUs outside the caller's data scope are REFERENCE_INVALID.
func (s Service) ResolveSpecs(ctx context.Context, refs []api.SpecRef) (map[api.SpecRef]api.SpecStanding, error) {
	if err := api.ValidateSpecRefs(refs); err != nil {
		return nil, err
	}
	out := make(map[api.SpecRef]api.SpecStanding, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	ids := make([]string, len(refs))
	for i, r := range refs {
		ids[i] = r.SkuID
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		skus, err := postgres.Skus(ctx, exec, scope(ctx), slices.Compact(slices.Sorted(slices.Values(ids))))
		if err != nil {
			return err
		}
		byID := make(map[string]*sku.Sku, len(skus))
		for _, k := range skus {
			if d.Sees(k.OwnerActor, k.OwnerTeam) {
				byID[k.ID] = k
			}
		}
		for _, r := range refs {
			k := byID[r.SkuID]
			standing := api.SpecStanding{Ref: r}
			if k != nil {
				standing.Status = string(k.Status)
				if k.Status == sku.Confirmed {
					standing.CurrentRevision = k.Revision
				}
			}
			err := sku.CheckSpec(k, k != nil, r.SpecRevision)
			rej, refused := kernel.AsRejection(err)
			switch {
			case err == nil:
				standing.Current = true
			case refused:
				standing.Code = rej.Code
			default:
				return err
			}
			out[r] = standing
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
