package application

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/item"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Audited read of a style.
const CommandGetItem = "item.get"

// SaveItem saves a style draft. The sources it cites are read before the transaction. Inside it the
// style root is locked FOR UPDATE and each cell's SKU is re-checked with SkuGuard v1, in SKU and
// revision order. The apparel plugin does not have to be active.
func (s Service) SaveItem(ctx context.Context, cmd api.SaveItem) (api.Receipt, error) {
	profile := cmd.DocumentProfile
	if profile == "" {
		profile = api.ProfileGeneric
	}
	tech := techOf(cmd)
	var readable map[string]evdapi.SourceRef
	h := &itemCommand{id: cmd.ItemID, prepare: func(ctx context.Context) (err error) {
		readable, err = s.readable(ctx, tech.SourceIDs())
		return err
	}}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := item.CheckSave(h.item, cmd.ExpectedVersion, profile, cmd.StyleCode); err != nil {
			return action.Outcome{}, err
		}
		if err := techSources(tech, readable); err != nil {
			return action.Outcome{}, err
		}
		if err := requireVariants(ctx, exec, d, tech.Variants); err != nil {
			return action.Outcome{}, err
		}
		sc := scope(ctx)
		created := h.item == nil
		if created {
			taken, err := postgres.ItemStyleTaken(ctx, exec, sc, cmd.StyleCode, cmd.ItemID)
			if err != nil {
				return action.Outcome{}, err
			}
			if err = item.CheckStyle(taken); err != nil {
				return action.Outcome{}, err
			}
			h.item = item.New(cmd.ItemID, profile, cmd.StyleCode, d.Actor.ID, d.Team)
		}
		rev, fresh := h.item.Save(tech, d.Actor, d.Epoch, s.Clock.Now())
		var err error
		if created {
			err = postgres.InsertItem(ctx, exec, sc, h.item, s.Clock.Now())
		}
		if err == nil && fresh {
			err = postgres.InsertItemRevision(ctx, exec, sc, h.item.ID, rev)
		} else if err == nil {
			err = postgres.UpdateItemRevision(ctx, exec, sc, h.item.ID, rev)
		}
		if err == nil && !created {
			err = postgres.UpdateItem(ctx, exec, sc, h.item, h.from, s.Clock.Now())
		}
		return h.outcome(string(rev.Status)), stored(err)
	}
	args := cmd
	args.IdempotencyKey, args.ExpectedVersion = "", 0
	return s.run(ctx, action.Command{Name: api.CommandSaveItem, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ItemID},
		args, cmd.Validate(), &preparedItem{h})
}

// ConfirmItem publishes the draft revision. Human only. SkuGuard runs again, after the style root
// is locked, so a SKU revised since the draft was saved is REFERENCE_STALE.
func (s Service) ConfirmItem(ctx context.Context, cmd api.ConfirmItem) (api.Receipt, error) {
	var readable map[string]evdapi.SourceRef
	h := &itemCommand{id: cmd.ItemID, prepare: func(ctx context.Context) (err error) {
		readable, err = s.readable(ctx, cmd.EvidenceIDs)
		return err
	}}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		rev, err := h.item.CheckConfirm(cmd.ExpectedVersion, cmd.Revision)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = knownIDs(cmd.EvidenceIDs, readable, "/args/evidence_ids/"); err != nil {
			return action.Outcome{}, err
		}
		if err = requireVariants(ctx, exec, d, rev.Tech.Variants); err != nil {
			return action.Outcome{}, err
		}
		superseded := h.item.Confirm(d.Actor, d.Epoch, cmd.EvidenceIDs, s.Clock.Now())
		sc := scope(ctx)
		if superseded != nil {
			err = postgres.SupersedeItemRevision(ctx, exec, sc, h.item.ID, superseded)
		}
		if err == nil {
			err = postgres.ConfirmItemRevision(ctx, exec, sc, h.item.ID, h.item.Confirmed)
		}
		if err == nil {
			err = postgres.UpdateItem(ctx, exec, sc, h.item, h.from, s.Clock.Now())
		}
		if err != nil {
			return action.Outcome{}, stored(err)
		}
		return h.outcome(string(h.item.Confirmed.Status), action.Event{Type: api.EventItemConfirmed, AggregateType: "Item",
			AggregateID: h.item.ID, AggregateVersion: h.item.Version, Fields: map[string]any{
				"item_id": h.item.ID, "revision": h.item.Current, "style_code": h.item.StyleCode,
				"document_profile": h.item.Profile, "root_version": h.item.Version, "content_hash": h.item.Confirmed.Hash}}), nil
	}
	args := cmd
	args.IdempotencyKey, args.ExpectedVersion = "", 0
	return s.run(ctx, action.Command{Name: api.CommandConfirmItem, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ItemID},
		args, cmd.Validate(), &preparedItem{h})
}

// GetItem returns a style and its revisions, audited.
func (s Service) GetItem(ctx context.Context, id string) (api.ItemView, error) {
	var v api.ItemView
	err := s.Ledger.Read(ctx, CommandGetItem, id, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		it, revs, err := postgres.ItemRecord(ctx, exec, scope(ctx), id)
		if err != nil {
			return err
		}
		if it == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.ItemView{ItemID: it.ID, DocumentProfile: it.Profile, StyleCode: it.StyleCode, RootVersion: it.Version, CurrentRevision: it.Current}
		for _, rev := range revs {
			v.Revisions = append(v.Revisions, revisionView(rev))
		}
		return nil
	})
	return v, err
}

type itemCommand struct {
	id      string
	item    *item.Item
	from    int64
	prepare func(context.Context) error
	apply   func(context.Context, bentopg.Executor, authorization.Decision) (action.Outcome, error)
}

func (h *itemCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.item, h.from = nil, 0
	var err error
	h.item, err = postgres.LoadItem(ctx, exec, scope(ctx), h.id, true)
	if h.item != nil {
		h.from = h.item.Version
	}
	return err
}

func (h *itemCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

func (h *itemCommand) outcome(status string, events ...action.Event) action.Outcome {
	return action.Outcome{ObjectID: h.item.ID, RowVersion: h.item.Version, Status: status, Events: events}
}

type preparedItem struct{ *itemCommand }

func (h *preparedItem) Prepare(ctx context.Context) error { return h.prepare(ctx) }

func techOf(cmd api.SaveItem) item.Tech {
	variants := make([]item.Variant, len(cmd.Variants))
	for i, v := range cmd.Variants {
		variants[i] = item.Variant{ColorCode: v.ColorCode, SizeCode: v.SizeCode, SkuID: v.SkuID, SpecRevision: v.SpecRevision}
	}
	return item.Tech{FabricComposition: cmd.FabricComposition, FabricSpec: cmd.FabricSpec, FabricWidth: paramOf(cmd.FabricWidth),
		FabricGSM: paramOf(cmd.FabricGSM), WarpShrinkage: paramOf(cmd.WarpShrinkage), WeftShrinkage: paramOf(cmd.WeftShrinkage),
		DyeLotPolicy: cmd.DyeLotPolicy, SizeChartEvidenceID: cmd.SizeChartEvidenceID, Variants: variants}
}

func paramOf(v api.MeasuredValue) item.Param {
	return item.Param{Value: v.Value, Unit: v.Unit, Measured: v.Measured, SourceID: v.SourceEvidenceID}
}

func revisionView(rev item.Revision) api.ItemRevisionView {
	return api.ItemRevisionView{Revision: rev.Number, Status: string(rev.Status), ContentHash: rev.Hash,
		FabricComposition: rev.Tech.FabricComposition, FabricSpec: rev.Tech.FabricSpec,
		FabricWidth: valueOf(rev.Tech.FabricWidth), FabricGSM: valueOf(rev.Tech.FabricGSM),
		WarpShrinkage: valueOf(rev.Tech.WarpShrinkage), WeftShrinkage: valueOf(rev.Tech.WeftShrinkage),
		DyeLotPolicy: rev.Tech.DyeLotPolicy, SizeChartEvidenceID: rev.Tech.SizeChartEvidenceID,
		Variants: variantsOf(rev.Tech.Variants), EvidenceIDs: rev.EvidenceIDs, SavedBy: rev.SavedBy, ConfirmedBy: rev.ConfirmedBy}
}

func valueOf(p item.Param) api.MeasuredValue {
	return api.MeasuredValue{Value: p.Value, Unit: p.Unit, Measured: p.Measured, SourceEvidenceID: p.SourceID}
}

func variantsOf(in []item.Variant) []api.ItemVariant {
	out := make([]api.ItemVariant, len(in))
	for i, v := range in {
		out[i] = api.ItemVariant{ColorCode: v.ColorCode, SizeCode: v.SizeCode, SkuID: v.SkuID, SpecRevision: v.SpecRevision}
	}
	return out
}

func techSources(t item.Tech, readable map[string]evdapi.SourceRef) error {
	checks := []struct{ id, path string }{
		{t.FabricWidth.SourceID, "/args/fabric_width/source_evidence_id"},
		{t.FabricGSM.SourceID, "/args/fabric_gsm/source_evidence_id"},
		{t.WarpShrinkage.SourceID, "/args/warp_shrinkage/source_evidence_id"},
		{t.WeftShrinkage.SourceID, "/args/weft_shrinkage/source_evidence_id"},
	}
	if t.SizeChartEvidenceID != "" {
		checks = append(checks, struct{ id, path string }{t.SizeChartEvidenceID, "/args/size_chart_evidence_id"})
	}
	for _, c := range checks {
		if _, ok := readable[c.id]; !ok {
			return kernel.Reject(kernel.CodeReferenceInvalid, c.path, "Source is not accessible")
		}
	}
	return nil
}

func knownIDs(ids []string, readable map[string]evdapi.SourceRef, prefix string) error {
	for i, id := range ids {
		if _, ok := readable[id]; !ok {
			return kernel.Reject(kernel.CodeReferenceInvalid, prefix+strconv.Itoa(i), "Source is not accessible")
		}
	}
	return nil
}

func requireVariants(ctx context.Context, exec bentopg.Executor, d authorization.Decision, variants []item.Variant) error {
	ordered := slices.Clone(variants)
	slices.SortFunc(ordered, func(a, b item.Variant) int {
		return cmp.Or(cmp.Compare(a.SkuID, b.SkuID), cmp.Compare(a.SpecRevision, b.SpecRevision))
	})
	seen := map[[2]string]bool{}
	for _, v := range ordered {
		key := [2]string{v.SkuID, strconv.FormatInt(v.SpecRevision, 10)}
		if seen[key] {
			continue
		}
		seen[key] = true
		if err := requireCurrentSKU(ctx, exec, d, v.SkuID, v.SpecRevision, variantPath(variants, v)); err != nil {
			return err
		}
	}
	return nil
}

func variantPath(variants []item.Variant, v item.Variant) string {
	for i, o := range variants {
		if o.ColorCode == v.ColorCode && o.SizeCode == v.SizeCode {
			return "/args/variants/" + strconv.Itoa(i) + "/sku_id"
		}
	}
	return "/args/variants"
}

// requireCurrentSKU locks the SKU with SkuGuard v1 and refuses one the actor cannot see, or one
// whose specification revision is not the current confirmed revision.
func requireCurrentSKU(ctx context.Context, exec bentopg.Executor, d authorization.Decision, skuID string, spec int64, path string) error {
	ref := api.SpecRef{SkuID: skuID, SpecRevision: spec}
	verdict, err := api.ScanSkuGuard(ref, exec.QueryRow(ctx, api.SkuGuardSQL, api.SkuGuardArgs(ref)...).Scan)
	if err != nil {
		return err
	}
	k, err := postgres.Load(ctx, exec, scope(ctx), skuID, false)
	if err != nil {
		return err
	}
	if k == nil || !d.Sees(k.OwnerActor, k.OwnerTeam) {
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	return verdict.Check(path)
}

// stored turns a compare-and-set miss into VERSION_CONFLICT and a style-code or sample-key collision
// into BUSINESS_KEY_CONFLICT. A missing SKU revision is REFERENCE_INVALID.
func stored(err error) error {
	if err == nil || errors.Is(err, postgres.ErrConflict) {
		if err == nil {
			return nil
		}
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			switch pgErr.ConstraintName {
			case "item_style_code":
				return api.Reject(api.CodeBusinessKeyConflict, "/args/style_code")
			case "sample_approval_sku_stage":
				return api.Reject(api.CodeBusinessKeyConflict, "/args/id")
			}
		case "23503":
			return kernel.Reject(kernel.CodeReferenceInvalid, "/args/sku_id", "")
		}
	}
	return err
}
