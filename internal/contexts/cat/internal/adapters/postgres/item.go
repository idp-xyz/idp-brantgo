package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/item"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ErrUnique is a unique-index violation (23505).
var ErrUnique = errors.New("cat: unique violation")

func unique(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return errors.Join(ErrUnique, err)
	}
	return err
}

func textOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// LoadItem returns the style and its draft and confirmed revisions, or nil.
func LoadItem(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, forUpdate bool) (*item.Item, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	var r item.Item
	var team *string
	err := exec.QueryRow(ctx, `SELECT item_id, document_profile, style_code, root_version, last_revision, current_revision,
		owner_actor_id, owner_team_id FROM brant_cat.item WHERE tenant_id = $1 AND company_id = $2 AND item_id = $3`+lock,
		string(scope.Tenant), string(scope.Company), id).Scan(&r.ID, &r.Profile, &r.StyleCode, &r.Version, &r.Last, &r.Current, &r.OwnerActor, &team)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if team != nil {
		r.OwnerTeam = *team
	}
	revs, err := loadItemRevisions(ctx, exec, scope, id)
	if err != nil {
		return nil, err
	}
	cells, err := loadItemVariants(ctx, exec, scope, id)
	if err != nil {
		return nil, err
	}
	for _, rev := range revs {
		rev.Tech.Variants = cells[rev.Number]
		switch rev.Status {
		case item.Draft:
			r.Draft = rev
		case item.Confirmed:
			r.Confirmed = rev
		}
	}
	return &r, nil
}

func loadItemRevisions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*item.Revision, error) {
	rows, err := exec.Query(ctx, `SELECT revision, status, content_hash, fabric_composition, coalesce(fabric_spec, ''),
		fabric_width_cm, fabric_width_unit, fabric_width_measured, fabric_width_source,
		fabric_gsm, fabric_gsm_unit, fabric_gsm_measured, fabric_gsm_source,
		warp_shrinkage_pct, warp_shrinkage_unit, warp_shrinkage_measured, warp_shrinkage_source,
		weft_shrinkage_pct, weft_shrinkage_unit, weft_shrinkage_measured, weft_shrinkage_source,
		dye_lot_policy, coalesce(size_chart_evidence_id, ''),
		saved_by_kind, saved_by_id, saved_epoch, saved_at,
		confirmed_by_kind, confirmed_by_id, confirm_epoch, confirmed_at, evidence_ids, superseded_at
		FROM brant_cat.item_revision WHERE tenant_id = $1 AND company_id = $2 AND item_id = $3 AND status IN ('draft', 'confirmed')
		ORDER BY revision`, string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*item.Revision, error) {
		var rev item.Revision
		var status, savedKind string
		var confirmedKind, confirmedID *string
		var confirmEpoch *int64
		var confirmedAt, supersededAt *time.Time
		err := row.Scan(&rev.Number, &status, &rev.Hash, &rev.Tech.FabricComposition, &rev.Tech.FabricSpec,
			&rev.Tech.FabricWidth.Value, &rev.Tech.FabricWidth.Unit, &rev.Tech.FabricWidth.Measured, &rev.Tech.FabricWidth.SourceID,
			&rev.Tech.FabricGSM.Value, &rev.Tech.FabricGSM.Unit, &rev.Tech.FabricGSM.Measured, &rev.Tech.FabricGSM.SourceID,
			&rev.Tech.WarpShrinkage.Value, &rev.Tech.WarpShrinkage.Unit, &rev.Tech.WarpShrinkage.Measured, &rev.Tech.WarpShrinkage.SourceID,
			&rev.Tech.WeftShrinkage.Value, &rev.Tech.WeftShrinkage.Unit, &rev.Tech.WeftShrinkage.Measured, &rev.Tech.WeftShrinkage.SourceID,
			&rev.Tech.DyeLotPolicy, &rev.Tech.SizeChartEvidenceID,
			&savedKind, &rev.SavedBy.ID, &rev.SavedEpoch, &rev.SavedAt,
			&confirmedKind, &confirmedID, &confirmEpoch, &confirmedAt, &rev.EvidenceIDs, &supersededAt)
		if err != nil {
			return nil, err
		}
		rev.Status = item.Status(status)
		rev.SavedBy.Kind = kernel.ActorKind(savedKind)
		rev.SavedAt = kernel.Instant(rev.SavedAt)
		if confirmedKind != nil {
			rev.ConfirmedBy = kernel.ActorRef{Kind: kernel.ActorKind(*confirmedKind), ID: *confirmedID}
			rev.ConfirmEpoch = *confirmEpoch
			rev.ConfirmedAt = kernel.Instant(*confirmedAt)
		}
		return &rev, nil
	})
}

func loadItemVariants(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (map[int64][]item.Variant, error) {
	rows, err := exec.Query(ctx, `SELECT revision, color_code, size_code, sku_id, spec_revision FROM brant_cat.item_sku_variant
		WHERE tenant_id = $1 AND company_id = $2 AND item_id = $3 ORDER BY revision, color_code, size_code`,
		string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	type cell struct {
		rev int64
		v   item.Variant
	}
	got, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (cell, error) {
		var c cell
		err := row.Scan(&c.rev, &c.v.ColorCode, &c.v.SizeCode, &c.v.SkuID, &c.v.SpecRevision)
		return c, err
	})
	if err != nil {
		return nil, err
	}
	out := map[int64][]item.Variant{}
	for _, c := range got {
		out[c.rev] = append(out[c.rev], c.v)
	}
	return out, nil
}

// StyleTaken reports whether another style already uses styleCode.
// ItemStyleTaken reports whether another style already uses styleCode.
func ItemStyleTaken(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, styleCode, exceptID string) (bool, error) {
	var taken bool
	err := exec.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brant_cat.item WHERE tenant_id = $1 AND company_id = $2 AND style_code = $3 AND item_id <> $4)`,
		string(scope.Tenant), string(scope.Company), styleCode, exceptID).Scan(&taken)
	return taken, err
}

// InsertItem stores a new style root.
func InsertItem(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *item.Item, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cat.item (tenant_id, company_id, item_id, document_profile, style_code,
		root_version, last_revision, current_revision, owner_actor_id, owner_team_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11)`,
		string(scope.Tenant), string(scope.Company), r.ID, r.Profile, r.StyleCode, r.Version, r.Last, r.Current,
		r.OwnerActor, textOrNil(r.OwnerTeam), at)
	return unique(err)
}

// UpdateItem writes the root when it is still at from.
func UpdateItem(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *item.Item, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cat.item SET root_version = $4, last_revision = $5, current_revision = $6, updated_at = $7
		WHERE tenant_id = $1 AND company_id = $2 AND item_id = $3 AND root_version = $8`,
		string(scope.Tenant), string(scope.Company), r.ID, r.Version, r.Last, r.Current, at, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// InsertItemRevision stores a new draft revision.
func InsertItemRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, rev *item.Revision) error {
	c := rev.Tech
	_, err := exec.Exec(ctx, `INSERT INTO brant_cat.item_revision (tenant_id, company_id, item_id, revision, status, content_hash,
		fabric_composition, fabric_spec,
		fabric_width_cm, fabric_width_unit, fabric_width_measured, fabric_width_source,
		fabric_gsm, fabric_gsm_unit, fabric_gsm_measured, fabric_gsm_source,
		warp_shrinkage_pct, warp_shrinkage_unit, warp_shrinkage_measured, warp_shrinkage_source,
		weft_shrinkage_pct, weft_shrinkage_unit, weft_shrinkage_measured, weft_shrinkage_source,
		dye_lot_policy, size_chart_evidence_id, saved_by_kind, saved_by_id, saved_epoch, saved_at)
		VALUES ($1,$2,$3,$4,'draft',$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29)`,
		string(scope.Tenant), string(scope.Company), id, rev.Number, rev.Hash, c.FabricComposition, textOrNil(c.FabricSpec),
		c.FabricWidth.Value, c.FabricWidth.Unit, c.FabricWidth.Measured, c.FabricWidth.SourceID,
		c.FabricGSM.Value, c.FabricGSM.Unit, c.FabricGSM.Measured, c.FabricGSM.SourceID,
		c.WarpShrinkage.Value, c.WarpShrinkage.Unit, c.WarpShrinkage.Measured, c.WarpShrinkage.SourceID,
		c.WeftShrinkage.Value, c.WeftShrinkage.Unit, c.WeftShrinkage.Measured, c.WeftShrinkage.SourceID,
		c.DyeLotPolicy, textOrNil(c.SizeChartEvidenceID), string(rev.SavedBy.Kind), rev.SavedBy.ID, rev.SavedEpoch, rev.SavedAt)
	if err != nil {
		return err
	}
	return ReplaceItemVariants(ctx, exec, scope, id, rev)
}

// UpdateItemRevision replaces a draft revision's body.
func UpdateItemRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, rev *item.Revision) error {
	c := rev.Tech
	tag, err := exec.Exec(ctx, `UPDATE brant_cat.item_revision SET content_hash = $5, fabric_composition = $6, fabric_spec = $7,
		fabric_width_cm = $8, fabric_width_unit = $9, fabric_width_measured = $10, fabric_width_source = $11,
		fabric_gsm = $12, fabric_gsm_unit = $13, fabric_gsm_measured = $14, fabric_gsm_source = $15,
		warp_shrinkage_pct = $16, warp_shrinkage_unit = $17, warp_shrinkage_measured = $18, warp_shrinkage_source = $19,
		weft_shrinkage_pct = $20, weft_shrinkage_unit = $21, weft_shrinkage_measured = $22, weft_shrinkage_source = $23,
		dye_lot_policy = $24, size_chart_evidence_id = $25, saved_by_kind = $26, saved_by_id = $27, saved_epoch = $28, saved_at = $29
		WHERE tenant_id = $1 AND company_id = $2 AND item_id = $3 AND revision = $4 AND status = 'draft'`,
		string(scope.Tenant), string(scope.Company), id, rev.Number, rev.Hash, c.FabricComposition, textOrNil(c.FabricSpec),
		c.FabricWidth.Value, c.FabricWidth.Unit, c.FabricWidth.Measured, c.FabricWidth.SourceID,
		c.FabricGSM.Value, c.FabricGSM.Unit, c.FabricGSM.Measured, c.FabricGSM.SourceID,
		c.WarpShrinkage.Value, c.WarpShrinkage.Unit, c.WarpShrinkage.Measured, c.WarpShrinkage.SourceID,
		c.WeftShrinkage.Value, c.WeftShrinkage.Unit, c.WeftShrinkage.Measured, c.WeftShrinkage.SourceID,
		c.DyeLotPolicy, textOrNil(c.SizeChartEvidenceID), string(rev.SavedBy.Kind), rev.SavedBy.ID, rev.SavedEpoch, rev.SavedAt)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	if err != nil {
		return err
	}
	return ReplaceItemVariants(ctx, exec, scope, id, rev)
}

// ConfirmItemRevision publishes a draft revision.
func ConfirmItemRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, rev *item.Revision) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cat.item_revision SET status = 'confirmed', confirmed_by_kind = 'human', confirmed_by_id = $5,
		confirm_epoch = $6, confirmed_at = $7, evidence_ids = $8
		WHERE tenant_id = $1 AND company_id = $2 AND item_id = $3 AND revision = $4 AND status = 'draft'`,
		string(scope.Tenant), string(scope.Company), id, rev.Number, rev.ConfirmedBy.ID, rev.ConfirmEpoch, rev.ConfirmedAt, rev.EvidenceIDs)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// SupersedeItemRevision closes the published revision.
func SupersedeItemRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, rev *item.Revision) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cat.item_revision SET status = 'superseded', superseded_at = $5
		WHERE tenant_id = $1 AND company_id = $2 AND item_id = $3 AND revision = $4 AND status = 'confirmed'`,
		string(scope.Tenant), string(scope.Company), id, rev.Number, rev.SupersededAt)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// ItemRecord returns the style and the revisions a read shows.
func ItemRecord(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*item.Item, []item.Revision, error) {
	it, err := LoadItem(ctx, exec, scope, id, false)
	if err != nil || it == nil {
		return it, nil, err
	}
	var revs []item.Revision
	if it.Draft != nil {
		revs = append(revs, *it.Draft)
	}
	if it.Confirmed != nil {
		revs = append(revs, *it.Confirmed)
	}
	return it, revs, nil
}

// ReplaceItemVariants rewrites the draft matrix.
func ReplaceItemVariants(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, rev *item.Revision) error {
	if _, err := exec.Exec(ctx, `DELETE FROM brant_cat.item_sku_variant WHERE tenant_id = $1 AND company_id = $2 AND item_id = $3 AND revision = $4`,
		string(scope.Tenant), string(scope.Company), id, rev.Number); err != nil {
		return err
	}
	for _, v := range rev.Tech.Variants {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_cat.item_sku_variant (tenant_id, company_id, item_id, revision, color_code, size_code, sku_id, spec_revision)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			string(scope.Tenant), string(scope.Company), id, rev.Number, v.ColorCode, v.SizeCode, v.SkuID, v.SpecRevision); err != nil {
			return err
		}
	}
	return nil
}
