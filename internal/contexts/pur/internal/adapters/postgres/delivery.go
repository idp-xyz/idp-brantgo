package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/supplierdelivery"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Supplier deliveries (migrations/pur/000003_pur_supplier_deliveries.sql): the root row a command
// locks, its versions and their lines.

const deliveryColumns = `d.delivery_id, d.row_version, d.commitment_id, d.order_id, d.supplier_id, d.latest_version,
	coalesce(d.verified_version, 0), d.owner_actor_id, coalesce(d.owner_team_id, ''), d.created_at`

type fileDoc struct {
	FileID string `json:"file_id"`
	SHA256 string `json:"sha256"`
}

func deliveryEvidenceText(es []supplierdelivery.Evidence) string {
	out := make([]evidenceDoc, len(es))
	for i, e := range es {
		out[i] = evidenceDoc{Locator: e.Locator, SourceID: e.SourceID}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func deliveryEvidenceOf(s string) ([]supplierdelivery.Evidence, error) {
	var docs []evidenceDoc
	if err := json.Unmarshal([]byte(s), &docs); err != nil {
		return nil, fmt.Errorf("pur: stored delivery evidence: %w", err)
	}
	out := make([]supplierdelivery.Evidence, len(docs))
	for i, d := range docs {
		out[i] = supplierdelivery.Evidence{SourceID: d.SourceID, Locator: d.Locator}
	}
	return out, nil
}

func deliveryFilesText(fs []supplierdelivery.File) string {
	out := make([]fileDoc, len(fs))
	for i, f := range fs {
		out[i] = fileDoc{FileID: f.FileID, SHA256: f.SHA256}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func deliveryFilesOf(s string) ([]supplierdelivery.File, error) {
	var docs []fileDoc
	if err := json.Unmarshal([]byte(s), &docs); err != nil {
		return nil, fmt.Errorf("pur: stored delivery files: %w", err)
	}
	out := make([]supplierdelivery.File, len(docs))
	for i, d := range docs {
		out[i] = supplierdelivery.File{FileID: d.FileID, SHA256: d.SHA256}
	}
	return out, nil
}

func scanDelivery(r row) (*supplierdelivery.Delivery, error) {
	d := &supplierdelivery.Delivery{}
	if err := r.Scan(&d.ID, &d.RowVersion, &d.CommitmentID, &d.OrderID, &d.SupplierID, &d.LatestVersion, &d.VerifiedVersion, &d.OwnerActor,
		&d.OwnerTeam, &d.CreatedAt); err != nil {
		return nil, err
	}
	d.Stored, d.CreatedAt = d.RowVersion, kernel.Instant(d.CreatedAt)
	return d, nil
}

// LoadDelivery returns delivery id with its versions and their lines, or nil; lock locks the
// delivery root row.
func LoadDelivery(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*supplierdelivery.Delivery, error) {
	d, err := scanDelivery(exec.QueryRow(ctx, `SELECT `+deliveryColumns+` FROM brant_pur.supplier_delivery d WHERE d.tenant_id = $1
		AND d.company_id = $2 AND d.delivery_id = $3`+string(lock), string(scope.Tenant), string(scope.Company), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return d, deliveryVersions(ctx, exec, scope, []*supplierdelivery.Delivery{d})
}

// DeliveriesOfCommitment returns the deliveries of commitment commitmentID with their versions and
// lines, by delivery ID.
func DeliveriesOfCommitment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, commitmentID string) ([]*supplierdelivery.Delivery, error) {
	rows, err := exec.Query(ctx, `SELECT `+deliveryColumns+` FROM brant_pur.supplier_delivery d WHERE d.tenant_id = $1 AND d.company_id = $2
		AND d.commitment_id = $3 ORDER BY d.delivery_id`, string(scope.Tenant), string(scope.Company), commitmentID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*supplierdelivery.Delivery, error) { return scanDelivery(r) })
	if err != nil {
		return nil, err
	}
	return out, deliveryVersions(ctx, exec, scope, out)
}

type versionKey struct {
	id     string
	number int64
}

// deliveryVersions loads the versions of ds, in order, and their lines, in the order recorded.
func deliveryVersions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ds []*supplierdelivery.Delivery) error {
	if len(ds) == 0 {
		return nil
	}
	byID := map[string]*supplierdelivery.Delivery{}
	ids := make([]string, len(ds))
	for i, d := range ds {
		byID[d.ID], ids[i] = d, d.ID
	}
	versions := map[versionKey]*supplierdelivery.Version{}
	rows, err := exec.Query(ctx, `SELECT delivery_id, version, status, handover_place, handed_over_at, coalesce(supplier_reference, ''),
		coalesce(correction_reason, ''), evidence, files, request_hash, recorded_by_kind, recorded_by_id, record_epoch, recorded_at,
		decided_by_kind, decided_by_id, decide_epoch, decided_at, decision, decision_reason, decision_evidence, superseded_by_version,
		superseded_at FROM brant_pur.supplier_delivery_version WHERE tenant_id = $1 AND company_id = $2 AND delivery_id = ANY($3)
		ORDER BY delivery_id, version`, string(scope.Tenant), string(scope.Company), ids)
	if err != nil {
		return err
	}
	type versionRow struct {
		id string
		v  *supplierdelivery.Version
	}
	loaded, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (versionRow, error) {
		id, v, err := scanDeliveryVersion(r)
		return versionRow{id, v}, err
	})
	if err != nil {
		return err
	}
	for _, x := range loaded {
		if d := byID[x.id]; d != nil {
			d.Versions = append(d.Versions, x.v)
			versions[versionKey{x.id, x.v.Number}] = x.v
		}
	}
	lines, err := exec.Query(ctx, `SELECT delivery_id, version, order_line_id, sku_id, spec_revision, unit_id, unit_revision,
		claimed_quantity::text, verified_quantity::text FROM brant_pur.supplier_delivery_line WHERE tenant_id = $1 AND company_id = $2
		AND delivery_id = ANY($3) ORDER BY delivery_id, version, line_no`, string(scope.Tenant), string(scope.Company), ids)
	if err != nil {
		return err
	}
	defer lines.Close()
	for lines.Next() {
		var k versionKey
		var claimed string
		var verified *string
		var l supplierdelivery.Line
		if err = lines.Scan(&k.id, &k.number, &l.OrderLineID, &l.SkuID, &l.SpecRevision, &l.UnitID, &l.UnitRevision, &claimed, &verified); err != nil {
			return err
		}
		if l.Claimed, err = quantity(claimed); err != nil {
			return err
		}
		if verified != nil {
			if l.Verified, err = quantity(*verified); err != nil {
				return err
			}
			l.Decided = true
		}
		if v := versions[k]; v != nil {
			v.Lines = append(v.Lines, l)
		}
	}
	return lines.Err()
}

func scanDeliveryVersion(r row) (string, *supplierdelivery.Version, error) {
	v := &supplierdelivery.Version{}
	var id, evidence, files, recKind string
	var dKind, dID, decision, dReason, dEvidence *string
	var dEpoch, supersededBy *int64
	var dAt, supersededAt *time.Time
	err := r.Scan(&id, &v.Number, &v.Status, &v.HandoverPlace, &v.HandedOverAt, &v.SupplierReference, &v.CorrectionReason, &evidence,
		&files, &v.RequestHash, &recKind, &v.Recorded.By.ID, &v.Recorded.Epoch, &v.Recorded.At, &dKind, &dID, &dEpoch, &dAt, &decision,
		&dReason, &dEvidence, &supersededBy, &supersededAt)
	if err != nil {
		return "", nil, err
	}
	v.Recorded.By.Kind = kernel.ActorKind(recKind)
	v.HandedOverAt, v.Recorded.At = kernel.Instant(v.HandedOverAt), kernel.Instant(v.Recorded.At)
	if v.Evidence, err = deliveryEvidenceOf(evidence); err != nil {
		return "", nil, err
	}
	if v.Files, err = deliveryFilesOf(files); err != nil {
		return "", nil, err
	}
	if dKind != nil && dID != nil && dEpoch != nil && dAt != nil && decision != nil && dEvidence != nil {
		dec := &supplierdelivery.Decision{Act: supplierdelivery.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*dKind), ID: *dID},
			Epoch: *dEpoch, At: kernel.Instant(*dAt)}, Verify: *decision == supplierdelivery.DecisionVerify}
		if dReason != nil {
			dec.Reason = *dReason
		}
		if dec.Evidence, err = deliveryEvidenceOf(*dEvidence); err != nil {
			return "", nil, err
		}
		v.Decision = dec
	}
	if supersededBy != nil && supersededAt != nil {
		v.SupersededBy, v.SupersededAt = *supersededBy, kernel.Instant(*supersededAt)
	}
	return id, v, nil
}

// VerifiedOfCommitment returns, by order line, the quantities verified now for commitment
// commitmentID by the verified versions of its deliveries other than except: what PUR-INV-008
// measures a verification against. Call it with the commitment root locked.
func VerifiedOfCommitment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, commitmentID, except string) (map[string]kernel.Decimal, error) {
	rows, err := exec.Query(ctx, `SELECT l.order_line_id, sum(l.verified_quantity)::text FROM brant_pur.supplier_delivery_line l
		JOIN brant_pur.supplier_delivery_version v ON v.tenant_id = l.tenant_id AND v.company_id = l.company_id AND v.delivery_id = l.delivery_id
			AND v.version = l.version
		WHERE l.tenant_id = $1 AND l.company_id = $2 AND l.commitment_id = $3 AND l.delivery_id <> $4 AND v.status = 'verified'
		GROUP BY l.order_line_id`, string(scope.Tenant), string(scope.Company), commitmentID, except)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]kernel.Decimal{}
	for rows.Next() {
		var line, sum string
		if err = rows.Scan(&line, &sum); err != nil {
			return nil, err
		}
		if out[line], err = quantity(sum); err != nil {
			return nil, err
		}
	}
	return out, rows.Err()
}

// InsertDelivery stores a new delivery with its first version and lines.
func InsertDelivery(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, d *supplierdelivery.Delivery) error {
	v := d.Fresh()
	if v == nil {
		return errors.New("pur: a new delivery has its first version")
	}
	if _, err := exec.Exec(ctx, `INSERT INTO brant_pur.supplier_delivery (tenant_id, company_id, delivery_id, row_version, commitment_id,
		order_id, supplier_id, latest_version, owner_actor_id, owner_team_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, nullif($10, ''), $11, $11)`,
		string(scope.Tenant), string(scope.Company), d.ID, d.RowVersion, d.CommitmentID, d.OrderID, d.SupplierID, d.LatestVersion,
		d.OwnerActor, d.OwnerTeam, d.CreatedAt); err != nil {
		return err
	}
	return insertDeliveryVersion(ctx, exec, scope, d, v)
}

func insertDeliveryVersion(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, d *supplierdelivery.Delivery, v *supplierdelivery.Version) error {
	if _, err := exec.Exec(ctx, `INSERT INTO brant_pur.supplier_delivery_version (tenant_id, company_id, delivery_id, version, status,
		commitment_id, handover_place, handed_over_at, supplier_reference, correction_reason, line_count, evidence, files, request_hash,
		recorded_by_kind, recorded_by_id, record_epoch, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		string(scope.Tenant), string(scope.Company), d.ID, v.Number, v.Status, d.CommitmentID, v.HandoverPlace, v.HandedOverAt,
		optText(v.SupplierReference), optText(v.CorrectionReason), len(v.Lines), deliveryEvidenceText(v.Evidence), deliveryFilesText(v.Files),
		v.RequestHash, string(v.Recorded.By.Kind), v.Recorded.By.ID, v.Recorded.Epoch, v.Recorded.At); err != nil {
		return err
	}
	for i, l := range v.Lines {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_pur.supplier_delivery_line (tenant_id, company_id, delivery_id, version, order_line_id,
			line_no, commitment_id, order_id, sku_id, spec_revision, unit_id, unit_revision, claimed_quantity)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::numeric)`,
			string(scope.Tenant), string(scope.Company), d.ID, v.Number, l.OrderLineID, i, d.CommitmentID, d.OrderID, l.SkuID, l.SpecRevision,
			l.UnitID, l.UnitRevision, decimalText(l.Claimed)); err != nil {
			return err
		}
	}
	return nil
}

// SaveDelivery writes what a command changed of a stored delivery d: first the root, if it is still
// at the row version it was loaded at (ErrConflict otherwise); then the version recorded since (a
// correction) with its lines, or the decision — the verified quantities of the lines, the
// supersession of the version verified before and the decided version, in the order the database
// checks them.
func SaveDelivery(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, d *supplierdelivery.Delivery, at time.Time) error {
	t, c := string(scope.Tenant), string(scope.Company)
	var verified *int64
	if d.VerifiedVersion > 0 {
		verified = &d.VerifiedVersion
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_pur.supplier_delivery SET row_version = $4, latest_version = $5, verified_version = $6,
		updated_at = $7 WHERE tenant_id = $1 AND company_id = $2 AND delivery_id = $3 AND row_version = $8`,
		t, c, d.ID, d.RowVersion, d.LatestVersion, verified, at, d.Stored)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	if err != nil {
		return err
	}
	if v := d.Fresh(); v != nil {
		return insertDeliveryVersion(ctx, exec, scope, d, v)
	}
	v := d.Decided()
	if v == nil || v.Decision == nil {
		return nil
	}
	one := func(tag interface{ RowsAffected() int64 }, err error) error {
		if err == nil && tag.RowsAffected() != 1 {
			err = ErrConflict
		}
		return err
	}
	dec, decision := v.Decision, supplierdelivery.DecisionReject
	if dec.Verify {
		decision = supplierdelivery.DecisionVerify
		for _, l := range v.Lines {
			if err = one(exec.Exec(ctx, `UPDATE brant_pur.supplier_delivery_line SET verified_quantity = $6::numeric WHERE tenant_id = $1
				AND company_id = $2 AND delivery_id = $3 AND version = $4 AND order_line_id = $5 AND verified_quantity IS NULL`,
				t, c, d.ID, v.Number, l.OrderLineID, decimalText(l.Verified))); err != nil {
				return err
			}
		}
		if s := d.Superseded(); s != nil {
			if err = one(exec.Exec(ctx, `UPDATE brant_pur.supplier_delivery_version SET status = $5, superseded_by_version = $6, superseded_at = $7
				WHERE tenant_id = $1 AND company_id = $2 AND delivery_id = $3 AND version = $4 AND status = 'verified'`,
				t, c, d.ID, s.Number, s.Status, s.SupersededBy, s.SupersededAt)); err != nil {
				return err
			}
		}
	}
	return one(exec.Exec(ctx, `UPDATE brant_pur.supplier_delivery_version SET status = $5, decided_by_kind = $6, decided_by_id = $7,
		decide_epoch = $8, decided_at = $9, decision = $10, decision_reason = $11, decision_evidence = $12
		WHERE tenant_id = $1 AND company_id = $2 AND delivery_id = $3 AND version = $4 AND status = 'recorded'`,
		t, c, d.ID, v.Number, v.Status, string(dec.By.Kind), dec.By.ID, dec.Epoch, dec.At, decision, optText(dec.Reason),
		deliveryEvidenceText(dec.Evidence)))
}
