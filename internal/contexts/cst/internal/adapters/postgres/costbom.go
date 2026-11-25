package postgres

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/costbom"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// LoadBOM returns the cost BOM id with its revisions, lines, size grades and surcharges, or nil.
func LoadBOM(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*costbom.BOM, error) {
	t, c := string(scope.Tenant), string(scope.Company)
	b := costbom.New(id, "", "")
	var active *int64
	var spec *int64
	err := exec.QueryRow(ctx, `SELECT root_version, last_revision, active_revision, status, document_profile, base_size_code,
		coalesce(item_id, ''), coalesce(sku_id, ''), spec_revision, owner_actor_id, coalesce(owner_team_id, '')
		FROM brant_cst.cost_bom WHERE tenant_id = $1 AND company_id = $2 AND bom_id = $3`+string(lock), t, c, id).
		Scan(&b.RootVersion, &b.LastRevision, &active, &b.Status, &b.Profile, &b.BaseSize, &b.ItemID, &b.SkuID, &spec, &b.OwnerActor, &b.OwnerTeam)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if active != nil {
		b.ActiveRevision = *active
	}
	if spec != nil {
		b.SpecRevision = *spec
	}
	rows, err := exec.Query(ctx, `SELECT revision, status, coalesce(content_sha256, ''), contributors, saved_by_kind, saved_by_id, saved_epoch, saved_at,
		verified_by_kind, verified_by_id, verify_epoch, verified_at, superseded_at, ended_by_kind, ended_by_id, end_epoch, ended_at, end_reason
		FROM brant_cst.cost_bom_revision WHERE tenant_id = $1 AND company_id = $2 AND bom_id = $3 ORDER BY revision`, t, c, id)
	if err != nil {
		return nil, err
	}
	revs, err := pgx.CollectRows(rows, scanBOMRevision)
	if err != nil {
		return nil, err
	}
	b.Revisions = revs
	if err = loadBOMLines(ctx, exec, t, c, id, b); err != nil {
		return nil, err
	}
	return b, nil
}

func scanBOMRevision(row pgx.CollectableRow) (*costbom.Revision, error) {
	var r costbom.Revision
	var status, savedKind string
	var verifiedKind, verifiedID, endedKind, endedID, endReason *string
	var verifyEpoch, endEpoch *int64
	var verifiedAt, supersededAt, endedAt *time.Time
	if err := row.Scan(&r.Number, &status, &r.Hash, &r.Contributors, &savedKind, &r.SavedBy.ID, &r.SavedEpoch, &r.SavedAt,
		&verifiedKind, &verifiedID, &verifyEpoch, &verifiedAt, &supersededAt, &endedKind, &endedID, &endEpoch, &endedAt, &endReason); err != nil {
		return nil, err
	}
	r.Status = costbom.Status(status)
	r.SavedBy.Kind = kernel.ActorKind(savedKind)
	if verifiedID != nil {
		kind := kernel.ActorHuman
		if verifiedKind != nil {
			kind = kernel.ActorKind(*verifiedKind)
		}
		r.VerifiedBy = kernel.ActorRef{Kind: kind, ID: *verifiedID}
	}
	if verifyEpoch != nil {
		r.VerifyEpoch = *verifyEpoch
	}
	if verifiedAt != nil {
		r.VerifiedAt = *verifiedAt
	}
	if supersededAt != nil {
		r.SupersededAt = *supersededAt
	}
	if endedAt != nil && endedID != nil && endEpoch != nil && endReason != nil {
		kind := kernel.ActorHuman
		if endedKind != nil {
			kind = kernel.ActorKind(*endedKind)
		}
		r.End = &costbom.End{By: kernel.ActorRef{Kind: kind, ID: *endedID}, Epoch: *endEpoch, At: *endedAt, Reason: *endReason}
	}
	return &r, nil
}

type lineKey struct {
	rev  int64
	line string
}

func loadBOMLines(ctx context.Context, exec bentopg.Executor, t, c, id string, b *costbom.BOM) error {
	rows, err := exec.Query(ctx, `SELECT revision, line_id, material_kind, material_code, unit, grading_mode, consumption_per_unit,
		wastage_ratio, shrinkage_factor, coalesce(fabric_width_cm, ''), coalesce(fabric_gsm, ''), measured, formula_basis,
		supply_kind, supply_id, supply_revision
		FROM brant_cst.cost_bom_line WHERE tenant_id = $1 AND company_id = $2 AND bom_id = $3 ORDER BY revision, line_id`, t, c, id)
	if err != nil {
		return err
	}
	lines := map[lineKey]*costbom.Line{}
	var order []lineKey
	var rev int64
	var lineID, kind, code, unit, mode, cons, waste, shrink, width, gsm, basis string
	var measured bool
	var supplyKind, supplyID *string
	var supplyRev *int64
	_, err = pgx.ForEachRow(rows, []any{&rev, &lineID, &kind, &code, &unit, &mode, &cons, &waste, &shrink, &width, &gsm, &measured, &basis,
		&supplyKind, &supplyID, &supplyRev}, func() error {
		line := costbom.Line{LineID: lineID, MaterialKind: kind, MaterialCode: code, Unit: unit, GradingMode: mode,
			ConsumptionPerUnit: cons, WastageRatio: waste, ShrinkageFactor: shrink, FabricWidthCM: width, FabricGSM: gsm,
			Measured: measured, FormulaBasis: basis}
		if supplyID != nil {
			ref := costbom.SupplyRef{ID: *supplyID}
			if supplyKind != nil {
				ref.Kind = *supplyKind
			}
			if supplyRev != nil {
				ref.Revision = *supplyRev
			}
			line.Supply = &ref
		}
		k := lineKey{rev, line.LineID}
		lines[k] = &line
		order = append(order, k)
		return nil
	})
	if err != nil {
		return err
	}
	grows, err := exec.Query(ctx, `SELECT revision, line_id, size_code, consumption_per_unit, size_cm_delta_minor, currency, minor_units
		FROM brant_cst.cost_bom_size_grade WHERE tenant_id = $1 AND company_id = $2 AND bom_id = $3 ORDER BY revision, line_id, size_code`, t, c, id)
	if err != nil {
		return err
	}
	var size, gcons string
	var minor *int64
	var currency *string
	var units *int16
	_, err = pgx.ForEachRow(grows, []any{&rev, &lineID, &size, &gcons, &minor, &currency, &units}, func() error {
		g := costbom.Grade{SizeCode: size, ConsumptionPerUnit: gcons}
		if minor != nil {
			g.SizeCMDeltaMinor = strconv.FormatInt(*minor, 10)
			if currency != nil {
				g.Currency = *currency
			}
			if units != nil {
				g.MinorUnits = int(*units)
			}
		}
		if line := lines[lineKey{rev, lineID}]; line != nil {
			line.Grades = append(line.Grades, g)
		}
		return nil
	})
	if err != nil {
		return err
	}
	srows, err := exec.Query(ctx, `SELECT revision, surcharge_id, surcharge_kind, scope, coalesce(color_code, ''), coalesce(material_code, ''),
		coalesce(mcq_threshold, ''), coalesce(mcq_unit, ''), amount_minor, currency, minor_units, allocation, coalesce(fee_category, '')
		FROM brant_cst.cost_bom_surcharge WHERE tenant_id = $1 AND company_id = $2 AND bom_id = $3 ORDER BY revision, surcharge_id`, t, c, id)
	if err != nil {
		return err
	}
	surcharges := map[int64][]costbom.Surcharge{}
	var sid, skind, scope, color, material, threshold, mcqUnit, allocation, fee string
	_, err = pgx.ForEachRow(srows, []any{&rev, &sid, &skind, &scope, &color, &material, &threshold, &mcqUnit, &minor, &currency, &units,
		&allocation, &fee}, func() error {
		s := costbom.Surcharge{SurchargeID: sid, Kind: skind, Scope: scope, ColorCode: color, MaterialCode: material,
			MCQThreshold: threshold, MCQUnit: mcqUnit, Allocation: allocation, FeeCategory: fee}
		if minor != nil {
			s.AmountMinor = strconv.FormatInt(*minor, 10)
			if currency != nil {
				s.Currency = *currency
			}
			if units != nil {
				s.MinorUnits = int(*units)
			}
		}
		surcharges[rev] = append(surcharges[rev], s)
		return nil
	})
	if err != nil {
		return err
	}
	byRev := map[int64]*costbom.Revision{}
	for _, r := range b.Revisions {
		byRev[r.Number] = r
		r.Surcharges = surcharges[r.Number]
	}
	for _, k := range order {
		if r := byRev[k.rev]; r != nil {
			r.Lines = append(r.Lines, *lines[k])
		}
	}
	return nil
}

func minorArg(s string) any {
	if s == "" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	return n
}

// SaveBOM writes what a command changed. created inserts the root; otherwise the root must still
// be at from. A new verified revision is inserted as a draft, its lines are written, then it is
// promoted, because sealed lines cannot be inserted.
func SaveBOM(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, b *costbom.BOM, created bool, from int64, at time.Time) error {
	t, c := string(scope.Tenant), string(scope.Company)
	var err error
	if created {
		_, err = exec.Exec(ctx, `INSERT INTO brant_cst.cost_bom (tenant_id, company_id, bom_id, root_version, last_revision, active_revision,
			status, document_profile, base_size_code, item_id, sku_id, spec_revision, owner_actor_id, owner_team_id, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, nullif($10, ''), nullif($11, ''), $12, $13, nullif($14, ''), $15, $15)`,
			t, c, b.ID, b.RootVersion, b.LastRevision, activeRevision(b), string(b.Status), b.Profile, b.BaseSize, b.ItemID, b.SkuID,
			specRevision(b.SpecRevision), b.OwnerActor, b.OwnerTeam, at)
	} else {
		err = casRoot(ctx, exec, `UPDATE brant_cst.cost_bom SET root_version = $4, last_revision = $5, active_revision = $6, status = $7,
			document_profile = $8, base_size_code = $9, item_id = nullif($10, ''), sku_id = nullif($11, ''), spec_revision = $12, updated_at = $13
			WHERE tenant_id = $1 AND company_id = $2 AND bom_id = $3 AND root_version = $14`,
			t, c, b.ID, b.RootVersion, b.LastRevision, activeRevision(b), string(b.Status), b.Profile, b.BaseSize, b.ItemID, b.SkuID,
			specRevision(b.SpecRevision), at, from)
	}
	if err != nil {
		return err
	}
	for _, r := range b.Revisions {
		if r.Changed && !r.Fresh && (r.Status == costbom.StatusSuperseded || r.Status == costbom.StatusWithdrawn) {
			if err = updateBOMRevision(ctx, exec, t, c, b.ID, r); err != nil {
				return err
			}
		}
	}
	for _, r := range b.Revisions {
		if !r.Fresh {
			continue
		}
		if err = insertBOMRevision(ctx, exec, t, c, b.ID, r); err != nil {
			return err
		}
		if err = insertBOMChildren(ctx, exec, t, c, b.ID, r); err != nil {
			return err
		}
	}
	for _, r := range b.Revisions {
		if r.Fresh || !r.Changed || r.Status != costbom.StatusDraft || !r.LinesReplaced {
			continue
		}
		if err = updateBOMRevision(ctx, exec, t, c, b.ID, r); err != nil {
			return err
		}
		if err = deleteBOMChildren(ctx, exec, t, c, b.ID, r.Number); err != nil {
			return err
		}
		if err = insertBOMChildren(ctx, exec, t, c, b.ID, r); err != nil {
			return err
		}
	}
	for _, r := range b.Revisions {
		if r.Status == costbom.StatusVerified && (r.Fresh || r.Changed) {
			if err = updateBOMRevision(ctx, exec, t, c, b.ID, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func activeRevision(b *costbom.BOM) any {
	if b.ActiveRevision == 0 {
		return nil
	}
	return b.ActiveRevision
}

func specRevision(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func insertBOMRevision(ctx context.Context, exec bentopg.Executor, t, c, id string, r *costbom.Revision) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cst.cost_bom_revision (tenant_id, company_id, bom_id, revision, status, content_sha256,
		contributors, saved_by_kind, saved_by_id, saved_epoch, saved_at)
		VALUES ($1, $2, $3, $4, 'draft', NULL, $5, $6, $7, $8, $9)`,
		t, c, id, r.Number, r.Contributors, string(r.SavedBy.Kind), r.SavedBy.ID, r.SavedEpoch, r.SavedAt)
	return err
}

func updateBOMRevision(ctx context.Context, exec bentopg.Executor, t, c, id string, r *costbom.Revision) error {
	var hash, verifiedKind, verifiedID, endedKind, endedID, endReason any
	var verifyEpoch, endEpoch, verifiedAt, supersededAt, endedAt any
	if r.Hash != "" {
		hash = r.Hash
	}
	if r.VerifiedBy.ID != "" {
		verifiedKind, verifiedID, verifyEpoch, verifiedAt = string(r.VerifiedBy.Kind), r.VerifiedBy.ID, r.VerifyEpoch, r.VerifiedAt
	}
	if !r.SupersededAt.IsZero() {
		supersededAt = r.SupersededAt
	}
	if r.End != nil {
		endedKind, endedID, endEpoch, endedAt, endReason = string(r.End.By.Kind), r.End.By.ID, r.End.Epoch, r.End.At, r.End.Reason
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_cst.cost_bom_revision SET status = $5, content_sha256 = $6, contributors = $7,
		saved_by_kind = $8, saved_by_id = $9, saved_epoch = $10, saved_at = $11,
		verified_by_kind = $12, verified_by_id = $13, verify_epoch = $14, verified_at = $15, superseded_at = $16,
		ended_by_kind = $17, ended_by_id = $18, end_epoch = $19, ended_at = $20, end_reason = $21
		WHERE tenant_id = $1 AND company_id = $2 AND bom_id = $3 AND revision = $4`,
		t, c, id, r.Number, string(r.Status), hash, r.Contributors, string(r.SavedBy.Kind), r.SavedBy.ID, r.SavedEpoch, r.SavedAt,
		verifiedKind, verifiedID, verifyEpoch, verifiedAt, supersededAt, endedKind, endedID, endEpoch, endedAt, endReason)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

func deleteBOMChildren(ctx context.Context, exec bentopg.Executor, t, c, id string, rev int64) error {
	for _, table := range []string{"brant_cst.cost_bom_size_grade", "brant_cst.cost_bom_surcharge", "brant_cst.cost_bom_line"} {
		if _, err := exec.Exec(ctx, `DELETE FROM `+table+` WHERE tenant_id = $1 AND company_id = $2 AND bom_id = $3 AND revision = $4`, t, c, id, rev); err != nil {
			return err
		}
	}
	return nil
}

func insertBOMChildren(ctx context.Context, exec bentopg.Executor, t, c, id string, r *costbom.Revision) error {
	for _, l := range r.Lines {
		var kind, sid any
		var srev any
		if l.Supply != nil {
			kind, sid, srev = l.Supply.Kind, l.Supply.ID, l.Supply.Revision
		}
		if _, err := exec.Exec(ctx, `INSERT INTO brant_cst.cost_bom_line (tenant_id, company_id, bom_id, revision, line_id, material_kind,
			material_code, unit, grading_mode, consumption_per_unit, wastage_ratio, shrinkage_factor, fabric_width_cm, fabric_gsm,
			measured, formula_basis, supply_kind, supply_id, supply_revision)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12, nullif($13,''), nullif($14,''), $15,$16,$17,$18,$19)`,
			t, c, id, r.Number, l.LineID, l.MaterialKind, l.MaterialCode, l.Unit, l.GradingMode, l.ConsumptionPerUnit, l.WastageRatio,
			l.ShrinkageFactor, l.FabricWidthCM, l.FabricGSM, l.Measured, l.FormulaBasis, kind, sid, srev); err != nil {
			return err
		}
		for _, g := range l.Grades {
			var minor, currency, units any
			if g.SizeCMDeltaMinor != "" {
				minor, currency, units = minorArg(g.SizeCMDeltaMinor), g.Currency, g.MinorUnits
			}
			if _, err := exec.Exec(ctx, `INSERT INTO brant_cst.cost_bom_size_grade (tenant_id, company_id, bom_id, revision, line_id, size_code,
				consumption_per_unit, size_cm_delta_minor, currency, minor_units)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
				t, c, id, r.Number, l.LineID, g.SizeCode, g.ConsumptionPerUnit, minor, currency, units); err != nil {
				return err
			}
		}
	}
	for _, s := range r.Surcharges {
		var minor, currency, units any
		if s.AmountMinor != "" {
			minor, currency, units = minorArg(s.AmountMinor), s.Currency, s.MinorUnits
		}
		if _, err := exec.Exec(ctx, `INSERT INTO brant_cst.cost_bom_surcharge (tenant_id, company_id, bom_id, revision, surcharge_id,
			surcharge_kind, scope, color_code, material_code, mcq_threshold, mcq_unit, amount_minor, currency, minor_units, allocation, fee_category)
			VALUES ($1,$2,$3,$4,$5,$6,$7, nullif($8,''), nullif($9,''), nullif($10,''), nullif($11,''), $12,$13,$14,$15, nullif($16,''))`,
			t, c, id, r.Number, s.SurchargeID, s.Kind, s.Scope, s.ColorCode, s.MaterialCode, s.MCQThreshold, s.MCQUnit, minor, currency, units,
			s.Allocation, s.FeeCategory); err != nil {
			return err
		}
	}
	return nil
}

// LoadSubstitution returns the substitution, or nil when it is not in the caller's company.
func LoadSubstitution(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*SubstitutionRow, error) {
	t, c := string(scope.Tenant), string(scope.Company)
	var row SubstitutionRow
	err := exec.QueryRow(ctx, `SELECT substitution_id, bom_id, original_line_id, original_material_code, substitute_material_code,
		applicable_quantity, target_spec_revision, cost_delta_minor, currency, minor_units, lead_days_delta, sample_impact, reason,
		status, requested_by_kind, requested_by_id
		FROM brant_cst.cost_bom_substitution WHERE tenant_id = $1 AND company_id = $2 AND substitution_id = $3`, t, c, id).
		Scan(&row.ID, &row.BomID, &row.LineID, &row.FromCode, &row.ToCode, &row.Quantity, &row.SpecRevision, &row.CostDeltaMinor,
			&row.Currency, &row.MinorUnits, &row.LeadDays, &row.SampleImpact, &row.Reason, &row.Status, &row.RequestedByKind, &row.RequestedByID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &row, err
}

// SubstitutionRow is a stored substitution request.
type SubstitutionRow struct {
	ID, BomID, LineID, FromCode, ToCode, Quantity, Currency, Reason, Status, RequestedByKind, RequestedByID string
	SpecRevision, CostDeltaMinor                                                                            int64
	MinorUnits                                                                                              int
	LeadDays                                                                                                int
	SampleImpact                                                                                            bool
}

// InsertSubstitution inserts a requested row.
func InsertSubstitution(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, row SubstitutionRow, by kernel.ActorRef, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cst.cost_bom_substitution (tenant_id, company_id, substitution_id, bom_id, original_line_id,
		original_material_code, substitute_material_code, applicable_quantity, target_spec_revision, cost_delta_minor, currency, minor_units,
		lead_days_delta, sample_impact, reason, status, requested_by_kind, requested_by_id, requested_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'requested',$16,$17,$18)`,
		string(scope.Tenant), string(scope.Company), row.ID, row.BomID, row.LineID, row.FromCode, row.ToCode, row.Quantity, row.SpecRevision,
		minorArg(strconv.FormatInt(row.CostDeltaMinor, 10)), row.Currency, row.MinorUnits, row.LeadDays, row.SampleImpact, row.Reason, string(by.Kind), by.ID, at)
	return err
}

// DecideSubstitution records the decision. The trigger refuses a second decision and a requester.
func DecideSubstitution(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, status string, by kernel.ActorRef, epoch, decidedRevision int64, at time.Time) error {
	var rev any
	if decidedRevision > 0 {
		rev = decidedRevision
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_cst.cost_bom_substitution SET status = $4, decided_by_kind = 'human', decided_by_id = $5,
		decided_at = $6, authority_epoch = $7, decided_revision = $8
		WHERE tenant_id = $1 AND company_id = $2 AND substitution_id = $3 AND status = 'requested'`,
		string(scope.Tenant), string(scope.Company), id, status, by.ID, at, epoch, rev)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}
