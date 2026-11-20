// Package postgres stores QLT's inspections and quality releases in schema brant_qlt, on the
// executor of QLT's own transaction.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/domain/inspection"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/domain/release"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is QLT's schema.
const Schema = "brant_qlt"

// ErrConflict means a compare-and-set update found another version.
var ErrConflict = errors.New("qlt: version changed")

// Lock is how a read locks the rows it returns.
type Lock string

const (
	NoLock    Lock = ""
	ForUpdate Lock = " FOR UPDATE"
)

func quantity(s string) (kernel.Decimal, error) {
	// numeric(18,6) prints six fractional digits; the stored value is a QuantityFormat value.
	d, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	if err != nil {
		return kernel.Decimal{}, fmt.Errorf("qlt: stored quantity %q: %w", s, err)
	}
	return d, nil
}

// Text is the exact decimal text of d.
func Text(d kernel.Decimal) string {
	s, _ := d.Exact()
	return s
}

func scope2(s kernel.Scope) (string, string) { return string(s.Tenant), string(s.Company) }

const inspectionColumns = `inspection_id, status, row_version, subject_kind, subject_id, subject_line_id, subject_seq, subject_revision,
	subject_hash, supplier_id, order_id, order_revision, order_hash, order_line_id, sku_id, spec_revision, unit_id, unit_revision,
	subject_quantity::text, inspected::text, conforming::text, nonconforming::text, sampling_method, sample_size::text,
	coalesce(sampling_plan, ''), standard_source_id, standard_sha256, inspector_kind, coalesce(inspector_actor_id, ''),
	coalesce(agency_party_id, ''), coalesce(agency_service_role_id, ''), coalesce(agency_role_revision, 0), report_file_id, report_sha256,
	report_mime_type, inspected_at, coalesce(supersedes_inspection_id, ''), coalesce(reason, ''), evidence_ids, request_hash, owner_actor_id,
	coalesce(owner_team_id, ''), recorded_by_kind, recorded_by_id, record_epoch, recorded_at, disposed_by_kind, disposed_by_id,
	dispose_epoch, disposed_at, disposition_reason, disposition_evidence_ids, coalesce(superseded_by_inspection_id, ''), superseded_at`

func scanInspection(row pgx.Row) (*inspection.Inspection, error) {
	var i inspection.Inspection
	var subjectQty, inspected, conforming, nonconforming string
	var sampleSize *string
	var recordedKind string
	var dKind, dID, dReason *string
	var dEpoch *int64
	var dAt, sAt *time.Time
	var dEvidence []string
	s := &i.Subject
	err := row.Scan(&i.ID, &i.Status, &i.RowVersion, &s.Ref.Kind, &s.Ref.ID, &s.Ref.LineID, &i.Seq, &s.Revision, &s.Hash, &s.SupplierID,
		&s.OrderID, &s.OrderRevision, &s.OrderHash, &s.OrderLineID, &s.SkuID, &s.SpecRevision, &s.UnitID, &s.UnitRevision, &subjectQty,
		&inspected, &conforming, &nonconforming, &i.Sampling.Method, &sampleSize, &i.Sampling.Plan, &i.Standard.ID, &i.Standard.SHA256,
		&i.Inspector.Kind, &i.Inspector.ActorID, &i.Inspector.PartyID, &i.Inspector.ServiceRoleID, &i.Inspector.RoleRevision, &i.Report.ID,
		&i.Report.SHA256, &i.Report.MimeType, &i.InspectedAt, &i.Supersedes, &i.Reason, &i.Evidence, &i.RequestHash, &i.OwnerActor,
		&i.OwnerTeam, &recordedKind, &i.Recorded.By.ID, &i.Recorded.Epoch, &i.Recorded.At, &dKind, &dID, &dEpoch, &dAt, &dReason, &dEvidence,
		&i.SupersededBy, &sAt)
	if err != nil {
		return nil, err
	}
	for _, q := range []struct {
		text string
		to   *kernel.Decimal
	}{{subjectQty, &s.Quantity}, {inspected, &i.Inspected}, {conforming, &i.Conforming}, {nonconforming, &i.Nonconforming}} {
		if *q.to, err = quantity(q.text); err != nil {
			return nil, err
		}
	}
	if sampleSize != nil {
		size, err := quantity(*sampleSize)
		if err != nil {
			return nil, err
		}
		i.Sampling.SampleSize = &size
	}
	i.Recorded.By.Kind = kernel.ActorKind(recordedKind)
	i.Recorded.At, i.InspectedAt = kernel.Instant(i.Recorded.At), kernel.Instant(i.InspectedAt)
	if i.Evidence == nil {
		i.Evidence = []string{}
	}
	if dAt != nil && dKind != nil && dID != nil && dEpoch != nil && dReason != nil {
		if dEvidence == nil {
			dEvidence = []string{}
		}
		i.Disposition = &inspection.Disposition{Reason: *dReason, Evidence: dEvidence,
			Decided: inspection.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*dKind), ID: *dID}, Epoch: *dEpoch, At: kernel.Instant(*dAt)}}
	}
	if sAt != nil {
		i.SupersededAt = kernel.Instant(*sAt)
	}
	return &i, nil
}

// LoadInspection returns inspection id with its disposition, or nil; lock locks the inspection row.
func LoadInspection(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*inspection.Inspection, error) {
	t, c := scope2(scope)
	i, err := scanInspection(exec.QueryRow(ctx, `SELECT `+inspectionColumns+` FROM brant_qlt.inspection WHERE tenant_id = $1 AND company_id = $2
		AND inspection_id = $3`+string(lock), t, c, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = loadItems(ctx, exec, scope, []*inspection.Inspection{i}); err != nil {
		return nil, err
	}
	return i, nil
}

// LockSubject locks every inspection of subject ref FOR UPDATE, in inspection ID order: the lock a
// command that records an inspection of the subject or grants a release on it takes before it
// judges the subject (lock order: QLT release root, then the subject's inspections).
func LockSubject(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ref inspection.SubjectRef) error {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT inspection_id FROM brant_qlt.inspection WHERE tenant_id = $1 AND company_id = $2 AND subject_kind = $3
		AND subject_id = $4 AND subject_line_id = $5 ORDER BY inspection_id COLLATE "C" FOR UPDATE`, t, c, ref.Kind, ref.ID, ref.LineID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

// LoadSubjectInspections returns every inspection of subject ref with its disposition, in inspection
// ID order, as a statement started now sees them.
func LoadSubjectInspections(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ref inspection.SubjectRef) ([]*inspection.Inspection, error) {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT `+inspectionColumns+` FROM brant_qlt.inspection WHERE tenant_id = $1 AND company_id = $2
		AND subject_kind = $3 AND subject_id = $4 AND subject_line_id = $5 ORDER BY inspection_id COLLATE "C"`, t, c, ref.Kind, ref.ID, ref.LineID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*inspection.Inspection, error) { return scanInspection(row) })
	if err != nil {
		return nil, err
	}
	return out, loadItems(ctx, exec, scope, out)
}

// loadItems fills in the disposition items of the disposed inspections among is.
func loadItems(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, is []*inspection.Inspection) error {
	byID := map[string]*inspection.Inspection{}
	var ids []string
	for _, i := range is {
		if i.Disposition != nil {
			byID[i.ID] = i
			ids = append(ids, i.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT inspection_id, action, quantity::text FROM brant_qlt.disposition_item WHERE tenant_id = $1
		AND company_id = $2 AND inspection_id = ANY ($3) ORDER BY inspection_id, action`, t, c, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, action, q string
		if err = rows.Scan(&id, &action, &q); err != nil {
			return err
		}
		d, err := quantity(q)
		if err != nil {
			return err
		}
		byID[id].Disposition.Items = append(byID[id].Disposition.Items, inspection.Item{Action: action, Quantity: d})
	}
	return rows.Err()
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// InsertInspection stores a new inspection.
func InsertInspection(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, i *inspection.Inspection) error {
	t, c := scope2(scope)
	s := i.Subject
	var sampleSize *string
	if i.Sampling.SampleSize != nil {
		v := Text(*i.Sampling.SampleSize)
		sampleSize = &v
	}
	var role *int64
	if i.Inspector.RoleRevision > 0 {
		role = &i.Inspector.RoleRevision
	}
	evidence := i.Evidence
	if evidence == nil {
		evidence = []string{}
	}
	_, err := exec.Exec(ctx, `INSERT INTO brant_qlt.inspection (tenant_id, company_id, inspection_id, status, row_version, subject_kind,
		subject_id, subject_line_id, subject_seq, subject_revision, subject_hash, supplier_id, order_id, order_revision, order_hash,
		order_line_id, sku_id, spec_revision, unit_id, unit_revision, subject_quantity, inspected, conforming, nonconforming,
		sampling_method, sample_size, sampling_plan, standard_source_id, standard_sha256, inspector_kind, inspector_actor_id,
		agency_party_id, agency_service_role_id, agency_role_revision, report_file_id, report_sha256, report_mime_type, inspected_at,
		supersedes_inspection_id, reason, evidence_ids, request_hash, owner_actor_id, owner_team_id, recorded_by_kind, recorded_by_id,
		record_epoch, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21::numeric, $22::numeric,
		$23::numeric, $24::numeric, $25, $26::numeric, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40, $41, $42, $43,
		$44, $45, $46, $47, $48)`,
		t, c, i.ID, i.Status, i.RowVersion, s.Ref.Kind, s.Ref.ID, s.Ref.LineID, i.Seq, s.Revision, s.Hash, s.SupplierID, s.OrderID,
		s.OrderRevision, s.OrderHash, s.OrderLineID, s.SkuID, s.SpecRevision, s.UnitID, s.UnitRevision, Text(s.Quantity), Text(i.Inspected),
		Text(i.Conforming), Text(i.Nonconforming), i.Sampling.Method, sampleSize, nullable(i.Sampling.Plan), i.Standard.ID, i.Standard.SHA256,
		i.Inspector.Kind, nullable(i.Inspector.ActorID), nullable(i.Inspector.PartyID), nullable(i.Inspector.ServiceRoleID), role, i.Report.ID,
		i.Report.SHA256, i.Report.MimeType, i.InspectedAt, nullable(i.Supersedes), nullable(i.Reason), evidence, i.RequestHash, i.OwnerActor,
		nullable(i.OwnerTeam), string(i.Recorded.By.Kind), i.Recorded.By.ID, i.Recorded.Epoch, i.Recorded.At)
	return err
}

// SupersedeInspection writes i's supersession if the stored row is still at version from.
func SupersedeInspection(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, i *inspection.Inspection, from int64) error {
	t, c := scope2(scope)
	tag, err := exec.Exec(ctx, `UPDATE brant_qlt.inspection SET status = $4, row_version = $5, superseded_by_inspection_id = $6, superseded_at = $7
		WHERE tenant_id = $1 AND company_id = $2 AND inspection_id = $3 AND row_version = $8`,
		t, c, i.ID, i.Status, i.RowVersion, i.SupersededBy, i.SupersededAt, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// DisposeInspection writes i's disposition and its items if the stored row is still at version from.
func DisposeInspection(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, i *inspection.Inspection, from int64) error {
	t, c := scope2(scope)
	d := i.Disposition
	evidence := d.Evidence
	if evidence == nil {
		evidence = []string{}
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_qlt.inspection SET status = $4, row_version = $5, disposed_by_kind = $6, disposed_by_id = $7,
		dispose_epoch = $8, disposed_at = $9, disposition_reason = $10, disposition_evidence_ids = $11
		WHERE tenant_id = $1 AND company_id = $2 AND inspection_id = $3 AND row_version = $12`,
		t, c, i.ID, i.Status, i.RowVersion, string(d.Decided.By.Kind), d.Decided.By.ID, d.Decided.Epoch, d.Decided.At, d.Reason, evidence, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	if err != nil {
		return err
	}
	for _, it := range d.Items {
		if _, err = exec.Exec(ctx, `INSERT INTO brant_qlt.disposition_item (tenant_id, company_id, inspection_id, action, quantity)
			VALUES ($1, $2, $3, $4, $5::numeric)`, t, c, i.ID, it.Action, Text(it.Quantity)); err != nil {
			return err
		}
	}
	return nil
}

const releaseColumns = `release_id, status, row_version, subject_kind, subject_id, subject_line_id, subject_revision, subject_hash, supplier_id,
	order_id, order_revision, order_hash, order_line_id, sku_id, spec_revision, unit_id, unit_revision, subject_quantity::text, quantity::text,
	conforming::text, released_before::text, inspection_ids, valid_from, valid_until, evidence_ids, request_hash, owner_actor_id,
	coalesce(owner_team_id, ''), granted_by_kind, granted_by_id, grant_epoch, granted_at, revoked_by_kind, revoked_by_id, revoke_epoch,
	revoked_at, revoke_reason, revoke_evidence_ids, conceded::text, concession_quantity::text, shipped_quantity::text`

func scanRelease(row pgx.Row) (*release.Release, error) {
	var r release.Release
	var subjectQty, qty, conforming, before, grantedKind, conceded, concession string
	var rKind, rID, rReason, shipped *string
	var rEpoch *int64
	var rAt *time.Time
	var rEvidence []string
	s := &r.Subject
	err := row.Scan(&r.ID, &r.Status, &r.RowVersion, &s.Ref.Kind, &s.Ref.ID, &s.Ref.LineID, &s.Revision, &s.Hash, &s.SupplierID, &s.OrderID,
		&s.OrderRevision, &s.OrderHash, &s.OrderLineID, &s.SkuID, &s.SpecRevision, &s.UnitID, &s.UnitRevision, &subjectQty, &qty, &conforming,
		&before, &r.InspectionIDs, &r.ValidFrom, &r.ValidUntil, &r.Evidence, &r.RequestHash, &r.OwnerActor, &r.OwnerTeam, &grantedKind,
		&r.Granted.By.ID, &r.Granted.Epoch, &r.Granted.At, &rKind, &rID, &rEpoch, &rAt, &rReason, &rEvidence, &conceded, &concession, &shipped)
	if err != nil {
		return nil, err
	}
	for _, q := range []struct {
		text string
		to   *kernel.Decimal
	}{{subjectQty, &s.Quantity}, {qty, &r.Quantity}, {conforming, &r.Conforming}, {before, &r.ReleasedBefore}, {conceded, &r.Conceded},
		{concession, &r.ConcessionQuantity}} {
		if *q.to, err = quantity(q.text); err != nil {
			return nil, err
		}
	}
	r.Granted.By.Kind = kernel.ActorKind(grantedKind)
	r.Granted.At, r.ValidFrom, r.ValidUntil = kernel.Instant(r.Granted.At), kernel.Instant(r.ValidFrom), kernel.Instant(r.ValidUntil)
	if r.Evidence == nil {
		r.Evidence = []string{}
	}
	if rAt != nil && rKind != nil && rID != nil && rEpoch != nil && rReason != nil {
		if rEvidence == nil {
			rEvidence = []string{}
		}
		r.Revocation = &release.Revocation{Reason: *rReason, Evidence: rEvidence,
			Revoked: inspection.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*rKind), ID: *rID}, Epoch: *rEpoch, At: kernel.Instant(*rAt)}}
		if shipped != nil {
			d, err := quantity(*shipped)
			if err != nil {
				return nil, err
			}
			r.Revocation.Shipped = &d
		}
	}
	return &r, nil
}

// LoadRelease returns release id, or nil; lock locks the release row.
func LoadRelease(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*release.Release, error) {
	t, c := scope2(scope)
	r, err := scanRelease(exec.QueryRow(ctx, `SELECT `+releaseColumns+` FROM brant_qlt.quality_release WHERE tenant_id = $1 AND company_id = $2
		AND release_id = $3`+string(lock), t, c, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// LoadReleases returns the releases among ids that exist, keyed by ID.
func LoadReleases(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ids []string) (map[string]*release.Release, error) {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT `+releaseColumns+` FROM brant_qlt.quality_release WHERE tenant_id = $1 AND company_id = $2
		AND release_id = ANY ($3)`, t, c, ids)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*release.Release, error) { return scanRelease(row) })
	out := make(map[string]*release.Release, len(list))
	for _, r := range list {
		out[r.ID] = r
	}
	return out, err
}

// LoadSubjectReleases returns every release of subject ref, in release ID order.
func LoadSubjectReleases(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ref inspection.SubjectRef) ([]*release.Release, error) {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT `+releaseColumns+` FROM brant_qlt.quality_release WHERE tenant_id = $1 AND company_id = $2
		AND subject_kind = $3 AND subject_id = $4 AND subject_line_id = $5 ORDER BY release_id COLLATE "C"`, t, c, ref.Kind, ref.ID, ref.LineID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*release.Release, error) { return scanRelease(row) })
}

// InsertRelease stores a new release.
func InsertRelease(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *release.Release) error {
	t, c := scope2(scope)
	s := r.Subject
	evidence := r.Evidence
	if evidence == nil {
		evidence = []string{}
	}
	_, err := exec.Exec(ctx, `INSERT INTO brant_qlt.quality_release (tenant_id, company_id, release_id, status, row_version, subject_kind,
		subject_id, subject_line_id, subject_revision, subject_hash, supplier_id, order_id, order_revision, order_hash, order_line_id, sku_id,
		spec_revision, unit_id, unit_revision, subject_quantity, quantity, conforming, released_before, inspection_ids, valid_from,
		valid_until, evidence_ids, request_hash, owner_actor_id, owner_team_id, granted_by_kind, granted_by_id, grant_epoch, granted_at, conceded,
		concession_quantity)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20::numeric, $21::numeric,
		$22::numeric, $23::numeric, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35::numeric, $36::numeric)`,
		t, c, r.ID, r.Status, r.RowVersion, s.Ref.Kind, s.Ref.ID, s.Ref.LineID, s.Revision, s.Hash, s.SupplierID, s.OrderID, s.OrderRevision,
		s.OrderHash, s.OrderLineID, s.SkuID, s.SpecRevision, s.UnitID, s.UnitRevision, Text(s.Quantity), Text(r.Quantity), Text(r.Conforming),
		Text(r.ReleasedBefore), r.InspectionIDs, r.ValidFrom, r.ValidUntil, evidence, r.RequestHash, r.OwnerActor, nullable(r.OwnerTeam),
		string(r.Granted.By.Kind), r.Granted.By.ID, r.Granted.Epoch, r.Granted.At, Text(r.Conceded), Text(r.ConcessionQuantity))
	return err
}

// RevokeRelease writes r's revocation, with the shipped and frozen quantities it recorded, if the
// stored row is still at version from.
func RevokeRelease(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *release.Release, from int64) error {
	t, c := scope2(scope)
	v := r.Revocation
	evidence := v.Evidence
	if evidence == nil {
		evidence = []string{}
	}
	if v.Shipped == nil {
		return errors.New("qlt: a revocation records what FUL shipped")
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_qlt.quality_release SET status = $4, row_version = $5, revoked_by_kind = $6, revoked_by_id = $7,
		revoke_epoch = $8, revoked_at = $9, revoke_reason = $10, revoke_evidence_ids = $11, shipped_quantity = $13::numeric,
		frozen_quantity = $14::numeric
		WHERE tenant_id = $1 AND company_id = $2 AND release_id = $3 AND row_version = $12`,
		t, c, r.ID, r.Status, r.RowVersion, string(v.Revoked.By.Kind), v.Revoked.By.ID, v.Revoked.Epoch, v.Revoked.At, v.Reason, evidence, from,
		Text(*v.Shipped), Text(r.Frozen()))
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}
